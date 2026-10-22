package gitops_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/gitops"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// objectFormats are the object formats every git operation is tested with.
var objectFormats = []string{"sha1", "sha256"}

// fixture is a fresh repository together with its local helper objects.
type fixture struct {
	ctx  context.Context
	cfg  config.Cfg
	repo *localrepo.Repo
	path string
	hash git.ObjectHash
}

func newFixture(t *testing.T, format string) fixture {
	t.Helper()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	protoRepo, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		ObjectFormat:           format,
		SkipCreationViaService: true,
	})

	hash, err := git.ObjectHashByFormat(format)
	require.NoError(t, err)

	return fixture{
		ctx:  ctx,
		cfg:  cfg,
		repo: localrepo.NewTestRepo(t, cfg, protoRepo),
		path: path,
		hash: hash,
	}
}

// commit writes a commit with one file of the given content and the given
// parents. The commit is not referenced. gittest.WriteCommit is not used
// because it hashes with the default object format only.
func (f fixture) commit(t *testing.T, parents []git.ObjectID, content string) git.ObjectID {
	t.Helper()

	blob := gittest.WriteBlob(t, f.cfg, f.path, []byte(content))

	mktree := gittest.NewCommand(t, f.cfg, "-C", f.path, "mktree")
	mktree.Stdin = strings.NewReader(fmt.Sprintf("100644 blob %s\tfile\n", blob))
	tree, err := mktree.Output()
	require.NoError(t, err)

	args := []string{"-C", f.path, "commit-tree", strings.TrimSpace(string(tree)), "-m", "commit " + content}
	for _, parent := range parents {
		args = append(args, "-p", parent.String())
	}

	out, err := gittest.NewCommand(t, f.cfg, args...).Output()
	require.NoError(t, err)

	return git.ObjectID(strings.TrimSpace(string(out)))
}

func (f fixture) git(t *testing.T, args ...string) string {
	t.Helper()

	return strings.TrimSpace(string(gittest.Exec(t, f.cfg, append([]string{"-C", f.path}, args...)...)))
}

func readPack(t *testing.T, p gitops.Pack) []byte {
	t.Helper()

	r, err := p.Open()
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()

	data, err := io.ReadAll(r)
	require.NoError(t, err)

	sum := sha256.Sum256(data)
	require.Equal(t, sum, p.SHA256, "pack hash must match its bytes")
	require.Equal(t, int64(len(data)), p.Size, "pack size must match its bytes")

	return data
}

func countEntries(t *testing.T, dir string) int {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	return len(entries)
}

func TestBuildPackOnlyNewObjects(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)
			c1 := f.commit(t, nil, "old")
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)
			c2 := f.commit(t, []git.ObjectID{c1}, "new")

			tmpDir := t.TempDir()
			pack, err := gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{c2}, tmpDir)
			require.NoError(t, err)
			require.False(t, pack.Empty())
			require.Equal(t, 3, pack.Objects, "commit, tree and blob of c2 only")
			require.Positive(t, pack.Walk, "the walk of the objects is timed")
			require.Equal(t, 1, countEntries(t, tmpDir))
			require.True(t, strings.HasPrefix(filepath.Base(pack.Path), "graft-pack-"))
			readPack(t, pack)
			require.NoError(t, pack.Remove())
			require.Equal(t, 0, countEntries(t, tmpDir))
		})
	}
}

func TestBuildPackIndexesIntoEmptyRepository(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			source := newFixture(t, format)
			c1 := source.commit(t, nil, "old")
			gittest.WriteRef(t, source.cfg, source.path, "refs/heads/main", c1)
			c2 := source.commit(t, []git.ObjectID{c1}, "new")

			tmpDir := t.TempDir()
			full, err := gitops.BuildFullPack(source.ctx, source.repo, tmpDir)
			require.NoError(t, err)
			fullData := readPack(t, full)
			require.Equal(t, 3, full.Objects)

			incremental, err := gitops.BuildPack(source.ctx, source.repo, []git.ObjectID{c2}, tmpDir)
			require.NoError(t, err)
			incrementalData := readPack(t, incremental)

			target := newFixture(t, format)
			require.NoError(t, gitops.IndexPack(target.ctx, target.repo, bytes.NewReader(fullData)))
			require.NoError(t, gitops.IndexPack(target.ctx, target.repo, bytes.NewReader(incrementalData)))

			require.NoError(t, gitops.ApplyRefs(target.ctx, target.repo, map[string]corestore.Target{
				"refs/heads/main": corestore.Target(c2.String()),
			}))

			// Both packs together make the history of c2 complete.
			gittest.Exec(t, target.cfg, "-C", target.path, "fsck", "--connectivity-only")
			require.Equal(t, c2.String(), target.git(t, "rev-parse", "refs/heads/main"))
		})
	}
}

func TestBuildPackEmpty(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)
			tmpDir := t.TempDir()

			// A repository without references has nothing to pack.
			full, err := gitops.BuildFullPack(f.ctx, f.repo, tmpDir)
			require.NoError(t, err)
			require.True(t, full.Empty())
			require.NoError(t, full.Remove())

			c1 := f.commit(t, nil, "old")
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)

			// Every object of the tip is already reachable from a reference.
			pack, err := gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{c1}, tmpDir)
			require.NoError(t, err)
			require.True(t, pack.Empty())
			require.Equal(t, 0, pack.Objects)
			require.Positive(t, pack.Walk, "an empty walk is timed too")

			pack, err = gitops.BuildPack(f.ctx, f.repo, nil, tmpDir)
			require.NoError(t, err)
			require.True(t, pack.Empty())
			require.Zero(t, pack.Walk, "without a tip there is no walk")

			pack, err = gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{f.hash.ZeroOID}, tmpDir)
			require.NoError(t, err)
			require.True(t, pack.Empty())

			require.Equal(t, 0, countEntries(t, tmpDir), "an empty result writes no file")
		})
	}
}

func TestBuildPackRejectsInvalidTip(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	tmpDir := t.TempDir()

	_, err := gitops.BuildPack(f.ctx, f.repo, []git.ObjectID{"not-an-object-id"}, tmpDir)
	require.Error(t, err)
	require.Equal(t, 0, countEntries(t, tmpDir))
}

func TestPackOpenAndRemove(t *testing.T) {
	t.Parallel()

	require.True(t, gitops.Pack{}.Empty())
	require.NoError(t, gitops.Pack{}.Remove())

	_, err := gitops.Pack{}.Open()
	require.Error(t, err)

	f := newFixture(t, "sha1")
	c1 := f.commit(t, nil, "content")
	gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)

	pack, err := gitops.BuildFullPack(f.ctx, f.repo, t.TempDir())
	require.NoError(t, err)
	require.False(t, pack.Empty())
	readPack(t, pack)

	require.NoError(t, pack.Remove())
	_, err = os.Stat(pack.Path)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, pack.Remove(), "removing a missing pack is not an error")
}

func TestReadRefs(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			f := newFixture(t, format)

			// An unborn HEAD is a symbolic reference like any other.
			refs, err := gitops.ReadRefs(f.ctx, f.repo)
			require.NoError(t, err)
			require.Equal(t, map[string]corestore.Target{
				"HEAD": corestore.SymbolicTarget("refs/heads/main"),
			}, refs)

			c1 := f.commit(t, nil, "one")
			c2 := f.commit(t, []git.ObjectID{c1}, "two")
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)
			gittest.WriteRef(t, f.cfg, f.path, "refs/heads/other", c2)
			f.git(t, "tag", "v1", c1.String())
			f.git(t, "symbolic-ref", "refs/heads/alias", "refs/heads/main")

			refs, err = gitops.ReadRefs(f.ctx, f.repo)
			require.NoError(t, err)
			require.Equal(t, map[string]corestore.Target{
				"HEAD":             corestore.SymbolicTarget("refs/heads/main"),
				"refs/heads/main":  corestore.Target(c1.String()),
				"refs/heads/other": corestore.Target(c2.String()),
				"refs/heads/alias": corestore.SymbolicTarget("refs/heads/main"),
				"refs/tags/v1":     corestore.Target(c1.String()),
			}, refs)

			// A detached HEAD holds the object ID.
			f.git(t, "update-ref", "--no-deref", "HEAD", c2.String())
			refs, err = gitops.ReadRefs(f.ctx, f.repo)
			require.NoError(t, err)
			require.Equal(t, corestore.Target(c2.String()), refs["HEAD"])
		})
	}
}

func TestReadRefValues(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	c1 := f.commit(t, nil, "one")
	gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)

	values, err := gitops.ReadRefValues(f.ctx, f.repo, nil)
	require.NoError(t, err)
	require.Empty(t, values)

	values, err = gitops.ReadRefValues(f.ctx, f.repo, []string{"HEAD", "refs/heads/main", "refs/heads/missing"})
	require.NoError(t, err)
	require.Equal(t, map[string]corestore.Target{
		"HEAD":            corestore.SymbolicTarget("refs/heads/main"),
		"refs/heads/main": corestore.Target(c1.String()),
	}, values)

	f.git(t, "update-ref", "--no-deref", "HEAD", c1.String())
	values, err = gitops.ReadRefValues(f.ctx, f.repo, []string{"HEAD"})
	require.NoError(t, err)
	require.Equal(t, map[string]corestore.Target{"HEAD": corestore.Target(c1.String())}, values)
}

func TestApplyRefs(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			t.Run("sets and deletes", func(t *testing.T) {
				f := newFixture(t, format)
				c1 := f.commit(t, nil, "one")
				c2 := f.commit(t, []git.ObjectID{c1}, "two")
				gittest.WriteRef(t, f.cfg, f.path, "refs/heads/a", c1)
				gittest.WriteRef(t, f.cfg, f.path, "refs/heads/b", c1)

				require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
					"refs/heads/a": corestore.Target(c2.String()),
					"refs/heads/b": "",
					"refs/heads/c": corestore.Target(c1.String()),
				}))

				refs, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/a", "refs/heads/b", "refs/heads/c"})
				require.NoError(t, err)
				require.Equal(t, map[string]corestore.Target{
					"refs/heads/a": corestore.Target(c2.String()),
					"refs/heads/c": corestore.Target(c1.String()),
				}, refs)
			})

			t.Run("symbolic HEAD", func(t *testing.T) {
				f := newFixture(t, format)
				c1 := f.commit(t, nil, "one")
				gittest.WriteRef(t, f.cfg, f.path, "refs/heads/feature", c1)

				require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
					"HEAD": corestore.SymbolicTarget("refs/heads/feature"),
				}))
				require.Equal(t, "refs/heads/feature", f.git(t, "symbolic-ref", "HEAD"))
			})

			t.Run("detached HEAD", func(t *testing.T) {
				f := newFixture(t, format)
				c1 := f.commit(t, nil, "one")
				gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)

				require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
					"HEAD": corestore.Target(c1.String()),
				}))

				refs, err := gitops.ReadRefs(f.ctx, f.repo)
				require.NoError(t, err)
				require.Equal(t, corestore.Target(c1.String()), refs["HEAD"])
			})

			t.Run("file directory conflict", func(t *testing.T) {
				f := newFixture(t, format)
				c1 := f.commit(t, nil, "one")
				gittest.WriteRef(t, f.cfg, f.path, "refs/heads/d", c1)

				require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
					"refs/heads/d":   "",
					"refs/heads/d/e": corestore.Target(c1.String()),
				}))

				refs, err := gitops.ReadRefValues(f.ctx, f.repo, []string{"refs/heads/d", "refs/heads/d/e"})
				require.NoError(t, err)
				require.Equal(t, map[string]corestore.Target{
					"refs/heads/d/e": corestore.Target(c1.String()),
				}, refs)
			})

			t.Run("invalid input changes nothing", func(t *testing.T) {
				f := newFixture(t, format)
				c1 := f.commit(t, nil, "one")
				gittest.WriteRef(t, f.cfg, f.path, "refs/heads/main", c1)

				valid := corestore.Target(c1.String())
				for _, tc := range []struct {
					desc string
					refs map[string]corestore.Target
				}{
					{desc: "dot dot name", refs: map[string]corestore.Target{"refs/../x": valid}},
					{desc: "HEAD with a path", refs: map[string]corestore.Target{"HEAD:x": valid}},
					{desc: "name with a space", refs: map[string]corestore.Target{"refs/heads/with space": valid}},
					{desc: "name with a newline", refs: map[string]corestore.Target{"refs/heads/nl\n": valid}},
					{desc: "not fully qualified", refs: map[string]corestore.Target{"main": valid}},
					{desc: "non hex object ID", refs: map[string]corestore.Target{"refs/heads/x": "zz"}},
					{desc: "uppercase object ID", refs: map[string]corestore.Target{"refs/heads/x": corestore.Target(strings.ToUpper(c1.String()))}},
					{desc: "short object ID", refs: map[string]corestore.Target{"refs/heads/x": "abc"}},
					{desc: "invalid symbolic target", refs: map[string]corestore.Target{"refs/heads/x": "ref:refs/../x"}},
				} {
					t.Run(tc.desc, func(t *testing.T) {
						refs := map[string]corestore.Target{"refs/heads/valid": valid}
						for name, target := range tc.refs {
							refs[name] = target
						}

						require.Error(t, gitops.ApplyRefs(f.ctx, f.repo, refs))

						current, err := gitops.ReadRefs(f.ctx, f.repo)
						require.NoError(t, err)
						require.Equal(t, map[string]corestore.Target{
							"HEAD":            corestore.SymbolicTarget("refs/heads/main"),
							"refs/heads/main": valid,
						}, current)
					})
				}
			})

			t.Run("empty map", func(t *testing.T) {
				f := newFixture(t, format)
				require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, nil))
			})
		})
	}
}

func TestApplyRefsLockContention(t *testing.T) {
	t.Parallel()

	f := newFixture(t, "sha1")
	testhelper.SkipWithReftable(t, "the reference lock is a file in the files backend")

	c1 := f.commit(t, nil, "one")
	lock := filepath.Join(f.path, "refs", "heads", "locked.lock")
	require.NoError(t, os.WriteFile(lock, nil, 0o600))

	err := gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
		"refs/heads/locked": corestore.Target(c1.String()),
	})
	require.Error(t, err)
	require.True(t, gitops.IsLockError(err), "error: %v", err)

	require.NoError(t, os.Remove(lock))
	require.NoError(t, gitops.ApplyRefs(f.ctx, f.repo, map[string]corestore.Target{
		"refs/heads/locked": corestore.Target(c1.String()),
	}))
	require.Equal(t, c1.String(), f.git(t, "rev-parse", "refs/heads/locked"))
}

func TestIsLockError(t *testing.T) {
	t.Parallel()

	require.True(t, gitops.IsLockError(updateref.AlreadyLockedError{ReferenceName: "refs/heads/x"}))
	require.True(t, gitops.IsLockError(updateref.AlreadyLockedError{}))
	require.True(t, gitops.IsLockError(updateref.ErrPackedRefsLocked))
	require.False(t, gitops.IsLockError(errors.New("boom")))
	require.False(t, gitops.IsLockError(nil))
}

func TestInitBare(t *testing.T) {
	t.Parallel()

	for _, format := range objectFormats {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			factory := gittest.NewCommandFactory(t, cfg)

			path := filepath.Join(t.TempDir(), "repo.git")
			require.NoError(t, gitops.InitBare(ctx, factory, path, format))

			out := gittest.Exec(t, cfg, "-C", path, "rev-parse", "--show-ref-format")
			require.Equal(t, "reftable", strings.TrimSpace(string(out)))

			out = gittest.Exec(t, cfg, "-C", path, "rev-parse", "--show-object-format")
			require.Equal(t, format, strings.TrimSpace(string(out)))

			require.Error(t, gitops.InitBare(ctx, factory, path, format), "an existing path is not reused")
		})
	}

	t.Run("unknown object format", func(t *testing.T) {
		t.Parallel()

		ctx := testhelper.Context(t)
		cfg := testcfg.Build(t)
		path := filepath.Join(t.TempDir(), "repo.git")
		require.Error(t, gitops.InitBare(ctx, gittest.NewCommandFactory(t, cfg), path, "md5"))

		_, err := os.Lstat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestWatermarkRoundTrip(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()

	w, ok, err := gitops.ReadWatermark(repoPath)
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, corestore.Watermark{}, w)

	require.Equal(t, filepath.Join(repoPath, "graft", "storage", "applied"), gitops.WatermarkPath(repoPath))

	require.NoError(t, gitops.WriteWatermark(repoPath, corestore.Watermark{RepoID: 7, Seq: 42}))
	w, ok, err = gitops.ReadWatermark(repoPath)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, corestore.Watermark{RepoID: 7, Seq: 42}, w)

	require.NoError(t, gitops.WriteWatermark(repoPath, corestore.Watermark{RepoID: 7, Seq: 43}))
	w, ok, err = gitops.ReadWatermark(repoPath)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint64(43), w.Seq)

	// The temporary files of the atomic write do not remain.
	entries, err := os.ReadDir(filepath.Dir(gitops.WatermarkPath(repoPath)))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "applied", entries[0].Name())
}

// TestWriteWatermarkWithoutSync checks that the watermark written without fsync
// reads back, leaves no temporary file, and is written as WriteWatermark does
// when its directory is missing.
func TestWriteWatermarkWithoutSync(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	// Without its directory the watermark is written as WriteWatermark does.
	require.NoError(t, gitops.WriteWatermarkWithoutSync(repoPath, corestore.Watermark{RepoID: 7, Seq: 1}))
	require.FileExists(t, gitops.WatermarkPath(repoPath))

	require.NoError(t, gitops.WriteWatermarkWithoutSync(repoPath, corestore.Watermark{RepoID: 7, Seq: 2}))
	w, ok, err := gitops.ReadWatermark(repoPath)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, corestore.Watermark{RepoID: 7, Seq: 2}, w)

	entries, err := os.ReadDir(filepath.Dir(gitops.WatermarkPath(repoPath)))
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestWatermarkCorrupt(t *testing.T) {
	t.Parallel()

	repoPath := t.TempDir()
	require.NoError(t, gitops.WriteWatermark(repoPath, corestore.Watermark{RepoID: 1, Seq: 1}))
	require.NoError(t, os.WriteFile(gitops.WatermarkPath(repoPath), []byte("{not json"), 0o600))

	_, _, err := gitops.ReadWatermark(repoPath)
	require.Error(t, err)
}

func TestValidateName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		valid bool
	}{
		{name: "HEAD", valid: true},
		{name: "refs/heads/main", valid: true},
		{name: "refs/tags/v1.0", valid: true},
		{name: "main", valid: false},
		{name: "", valid: false},
		{name: "refs/", valid: false},
		{name: "refs/../x", valid: false},
		{name: "refs/heads/a b", valid: false},
		{name: "refs/heads/a..b", valid: false},
		{name: "refs/heads/a\nb", valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := gitops.ValidateName(tc.name)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidateTarget(t *testing.T) {
	t.Parallel()

	sha1 := git.ObjectHashSHA1
	sha256Hash := git.ObjectHashSHA256
	sha1OID := strings.Repeat("a", 40)
	sha256OID := strings.Repeat("b", 64)

	for _, tc := range []struct {
		desc   string
		target corestore.Target
		hash   git.ObjectHash
		valid  bool
	}{
		{desc: "absent", target: "", hash: sha1, valid: true},
		{desc: "sha1 object", target: corestore.Target(sha1OID), hash: sha1, valid: true},
		{desc: "sha256 object", target: corestore.Target(sha256OID), hash: sha256Hash, valid: true},
		{desc: "sha256 object in a sha1 repository", target: corestore.Target(sha256OID), hash: sha1, valid: false},
		{desc: "sha1 object in a sha256 repository", target: corestore.Target(sha1OID), hash: sha256Hash, valid: false},
		{desc: "uppercase", target: corestore.Target(strings.ToUpper(sha1OID)), hash: sha1, valid: false},
		{desc: "symbolic", target: corestore.SymbolicTarget("refs/heads/main"), hash: sha1, valid: true},
		{desc: "symbolic to HEAD", target: corestore.SymbolicTarget("HEAD"), hash: sha1, valid: false},
		{desc: "symbolic to an invalid name", target: corestore.SymbolicTarget("main"), hash: sha1, valid: false},
		{desc: "symbolic without name", target: corestore.Target("ref:"), hash: sha1, valid: false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			err := gitops.ValidateTarget(tc.target, tc.hash)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
