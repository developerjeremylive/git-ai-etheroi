package objectpool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/storagemgr/partition/fsrecorder"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// recordingWAL records the operations of a transaction's FS.
type recordingWAL struct {
	directories []string
	links       []string
}

func (w *recordingWAL) CreateDirectory(relativePath string)          { w.directories = append(w.directories, relativePath) }
func (w *recordingWAL) RemoveDirectoryEntry(string)                  {}
func (w *recordingWAL) CreateFile(string, string) error              { return nil }
func (w *recordingWAL) CreateLink(sourcePath, destinationPath string) { w.links = append(w.links, destinationPath) }

// TestDisconnect_fanOutDirectoryRemoved removes each loose object directory of the repository right
// before the first links into it, three times, as housekeeping may (git prune-packed removes empty
// ones): each time after Disconnect created it, or created it again. Disconnect creates it again
// until the link holds, and records each directory and each link once through the transaction's
// FS, however many rounds a link took.
func TestDisconnect_fanOutDirectoryRemoved(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	logger := testhelper.SharedLogger(t)

	pool, repo := setupObjectPoolWithCfg(t, ctx, cfg)
	require.NoError(t, pool.Link(ctx, repo))
	poolPath := gittest.RepositoryPath(t, ctx, pool)
	repoPath := gittest.RepositoryPath(t, ctx, repo)

	// A packed commit and a loose one on top: both kinds of object file are linked.
	packed := gittest.WriteCommit(t, cfg, poolPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "packed", Mode: "100644", Content: "packed\n"},
	))
	gittest.Exec(t, cfg, "-C", poolPath, "update-ref", "refs/heads/main", packed.String())
	gittest.Exec(t, cfg, "-C", poolPath, "repack", "-a", "-d")
	loose := gittest.WriteCommit(t, cfg, poolPath, gittest.WithParents(packed), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "loose", Mode: "100644", Content: "loose\n"},
	))
	gittest.WriteRef(t, cfg, repoPath, "refs/heads/main", loose)
	looseDirs, err := filepath.Glob(filepath.Join(poolPath, "objects", "[0-9a-f][0-9a-f]"))
	require.NoError(t, err)
	require.NotEmpty(t, looseDirs)

	const removals = 3
	removed := map[string]int{}
	original := beforeLink
	beforeLink = func(targetRelativePath string) {
		dir := filepath.Join(cfg.Storages[0].Path, filepath.Dir(targetRelativePath))
		if filepath.Base(dir) == "pack" || removed[dir] == removals {
			return
		}
		removed[dir]++
		require.NoError(t, os.Remove(dir))
	}
	t.Cleanup(func() { beforeLink = original })

	wal := &recordingWAL{}
	require.NoError(t, Disconnect(ctx, fsrecorder.NewFS(cfg.Storages[0].Path, wal), repo, logger, transaction.NewManager(cfg, logger, nil)))

	require.Len(t, removed, len(looseDirs))
	for dir, times := range removed {
		require.Equal(t, removals, times, dir)
	}
	require.ElementsMatch(t, uniqueStrings(wal.directories), wal.directories, "a directory is recorded once")
	require.ElementsMatch(t, uniqueStrings(wal.links), wal.links, "a link is recorded once")
	for _, dir := range looseDirs {
		require.Contains(t, wal.directories, filepath.Join(repo.GetRelativePath(), "objects", filepath.Base(dir)))
	}

	require.NoFileExists(t, filepath.Join(repoPath, "objects", "info", "alternates"))
	gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "-e", loose.String())
	gittest.Exec(t, cfg, "-C", repoPath, "cat-file", "-e", packed.String())
	gittest.Exec(t, cfg, "-C", repoPath, "fsck", "--strict")
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var unique []string
	for _, value := range values {
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			unique = append(unique, value)
		}
	}
	return unique
}
