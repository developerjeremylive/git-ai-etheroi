package localrefs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// durabilityFixture is a reftable repository with a few references, written
// one transaction each so that its stack has several tables.
type durabilityFixture struct {
	ctx     context.Context
	cfg     config.Cfg
	locator storage.Locator
	repo    RepoRef
	path    string
	commit  git.ObjectID
	synced  *syncRecorder
}

// syncRecorder records the paths made durable, and fails them while failWith
// is set.
type syncRecorder struct {
	mu       sync.Mutex
	paths    []string
	failWith error
	// failOnly, when set, limits the failures to the paths that contain it.
	failOnly string
}

func (s *syncRecorder) sync(f *os.File) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failWith != nil && strings.Contains(f.Name(), s.failOnly) {
		return s.failWith
	}
	s.paths = append(s.paths, f.Name())
	return f.Sync()
}

func (s *syncRecorder) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

func (s *syncRecorder) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith = err
}

func setupDurability(t *testing.T) durabilityFixture {
	t.Helper()
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	pb, path := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		ReferenceBackend:       git.ReferenceBackendReftables,
	})
	commit := gittest.WriteCommit(t, cfg, path)
	for _, name := range []string{"refs/heads/main", "refs/heads/a", "refs/heads/b"} {
		gittest.WriteRef(t, cfg, path, git.ReferenceName(name), commit)
	}
	return durabilityFixture{
		ctx:     ctx,
		cfg:     cfg,
		locator: config.NewLocator(cfg),
		repo:    RepoRef{Storage: pb.GetStorageName(), RelativePath: pb.GetRelativePath()},
		path:    path,
		commit:  commit,
		synced:  &syncRecorder{},
	}
}

// unowned returns a durability of the fixture's storage that does not own it.
// The recovery grace is 0 unless opts set it.
func (f durabilityFixture) unowned(t *testing.T, bootID string, idle time.Duration, opts ...DurabilityOption) *Durability {
	t.Helper()
	d, err := NewDurability(f.locator, idle, append([]DurabilityOption{WithBootID(bootID), WithSync(f.synced.sync), WithRecoveryGrace(0)}, opts...)...)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, d.Close()) })
	return d
}

// durability returns a durability that owns the fixture's storage and defers,
// as the process that runs the durable steps (the recovery, when a test runs
// it, comes after: no writer of these tests runs before it). Close it before
// another one owns.
func (f durabilityFixture) durability(t *testing.T, bootID string, idle time.Duration, opts ...DurabilityOption) *Durability {
	t.Helper()
	d := f.unowned(t, bootID, idle, opts...)
	f.own(t, d)
	d.Activate()
	return d
}

// own takes the owner locks for d. A git process forked by another test at the
// moment the previous owner closed its lock holds a copy of the lock until it
// execs (close-on-exec), so the lock can be busy for that short while.
func (f durabilityFixture) own(t *testing.T, d *Durability) {
	t.Helper()
	require.Eventually(t, func() bool {
		owned, err := d.TryOwn(f.ctx, []string{f.repo.Storage})
		require.NoError(t, err)
		return owned
	}, 10*time.Second, time.Millisecond)
}

func (f durabilityFixture) markerPath(t *testing.T, d *Durability) string {
	t.Helper()
	p, err := d.markerPath(f.ctx, f.repo, f.path)
	require.NoError(t, err)
	return p
}

func (f durabilityFixture) tables(t *testing.T) []string {
	t.Helper()
	names, err := readTablesList(filepath.Join(f.path, "reftable", "tables.list"))
	require.NoError(t, err)
	require.NotEmpty(t, names)
	return names
}

func TestMarkDirtyWritesOneDurableMarker(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)
	before := len(f.synced.recorded())

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))

	content, err := os.ReadFile(f.markerPath(t, d))
	require.NoError(t, err)
	require.JSONEq(t, `{"storage":"`+f.repo.Storage+`","relative_path":"`+f.repo.RelativePath+`","boot_id":"boot-a"}`, string(content))
	require.Equal(t, 1.0, testutil.ToFloat64(d.marked))
	require.Equal(t, []string{f.path}, d.Dirty())

	// The marker file and its directory were made durable before MarkDirty
	// returned, once.
	recorded := f.synced.recorded()[before:]
	require.Len(t, recorded, 2)
	require.Equal(t, filepath.Dir(f.markerPath(t, d)), recorded[1])
	require.Equal(t, filepath.Join(f.cfg.Storages[0].Path, MarkerDirName), filepath.Dir(f.markerPath(t, d)))
}

func TestOwnershipCreatesTheMarkerDirectoryDurably(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	f.durability(t, "boot-a", 0)

	dir := filepath.Join(f.cfg.Storages[0].Path, MarkerDirName)
	require.DirExists(t, dir)
	require.Contains(t, f.synced.recorded(), filepath.Dir(dir), "the new directory's entry is durable")
}

// TestOneOwnerPerStorage is the overlap of a graceful upgrade: two processes
// on one storage. Only the owner defers, settles and recovers; the other one
// writes with fsync until the owner is gone.
func TestOneOwnerPerStorage(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	storages := []string{f.repo.Storage}

	old := f.durability(t, "boot-a", 0)
	require.True(t, old.Deferring())
	require.NoError(t, old.MarkDirty(f.ctx, f.repo, f.path))
	_ = old.Begin(f.path)
	// The old process's durable step holds git's lock right now.
	liveLock, ok, err := lockStack(f.path)
	require.NoError(t, err)
	require.True(t, ok)

	next := f.unowned(t, "boot-a", 0)
	owned, err := next.TryOwn(f.ctx, storages)
	require.NoError(t, err)
	require.False(t, owned, "the old process owns the storage")
	require.False(t, next.Deferring())
	require.Equal(t, 0.0, testutil.ToFloat64(next.owning))

	// The next process neither recovers, nor removes the old process's lock,
	// nor settles: the old process's writer is live, and only the old process
	// counts it.
	found, err := next.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.Empty(t, found)
	require.FileExists(t, filepath.Join(f.path, "reftable", "tables.list.lock"), "the live owner's lock is kept")
	require.NoError(t, next.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, old))
	require.NoError(t, liveLock.release())

	// The old process exits; the next one owns, recovers the marker before it
	// defers, and settles it.
	require.NoError(t, old.Close())
	f.own(t, next)
	require.False(t, next.Deferring(), "no write is deferred before the recovery")
	found, err = next.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.Equal(t, []Recovered{{Repo: f.repo, Path: f.path}}, found)
	next.Activate()
	require.True(t, next.Deferring())
	require.NoError(t, next.Settle(f.path, false))
	require.NoFileExists(t, f.markerPath(t, next))
}

// TestRecoveredMarkerWaitsForTheGrace verifies that a marker found by the
// recovery stays for the recovery grace: a git process of the previous owner
// that is not reaped yet may still be committing.
func TestRecoveredMarkerWaitsForTheGrace(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)

	old := f.durability(t, "boot-a", 0)
	require.NoError(t, old.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, old.Close())

	next := f.durability(t, "boot-a", 0, WithRecoveryGrace(time.Hour))
	_, err := next.Recover(f.ctx, []string{f.repo.Storage})
	require.NoError(t, err)
	require.NoError(t, next.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, next), "not even the last step removes it within the grace")
}

func TestSettleMakesTheStackDurableAndRemovesTheMarker(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, d.Settle(f.path, false))

	require.NoFileExists(t, f.markerPath(t, d))
	require.Empty(t, d.Dirty())
	require.Equal(t, 1.0, testutil.ToFloat64(d.cleared))
	require.Equal(t, 0.0, testutil.ToFloat64(d.dirty))

	recorded := f.synced.recorded()
	for _, table := range f.tables(t) {
		require.Contains(t, recorded, filepath.Join(f.path, "reftable", table))
	}
	require.Contains(t, recorded, filepath.Join(f.path, "reftable", "tables.list"))
	require.Contains(t, recorded, filepath.Join(f.path, "reftable"))
	// The lock taken for the removal is released, and its temporary file gone.
	require.NoFileExists(t, filepath.Join(f.path, "reftable", "tables.list.lock"))
	leftovers, err := filepath.Glob(filepath.Join(f.path, stepTempPrefix+"*"))
	require.NoError(t, err)
	require.Empty(t, leftovers)
}

func TestSettleKeepsTheMarkerWhileWritesMayGoOn(t *testing.T) {
	t.Parallel()

	t.Run("not idle", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", time.Hour)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.NoError(t, d.Settle(f.path, false))
		require.FileExists(t, f.markerPath(t, d))
		// The stack was made durable all the same.
		require.Contains(t, f.synced.recorded(), filepath.Join(f.path, "reftable", f.tables(t)[0]))

		// The last step of a stopping process does not wait for idleness.
		require.NoError(t, d.Settle(f.path, true))
		require.NoFileExists(t, f.markerPath(t, d))
	})

	t.Run("a live git process", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		end := d.Begin(f.path)
		require.NoError(t, d.Settle(f.path, true))
		require.FileExists(t, f.markerPath(t, d), "not even the last step removes it")

		end()
		require.NoError(t, d.Settle(f.path, false))
		require.NoFileExists(t, f.markerPath(t, d))
		require.Equal(t, 0.0, testutil.ToFloat64(d.inflight))
	})

	t.Run("two live git processes", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0)

		// Whatever hooks they ran or skipped, each counts until its own end, and
		// an end called twice counts once.
		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		first := d.Begin(f.path)
		second := d.Begin(f.path)
		first()
		first()
		require.NoError(t, d.Settle(f.path, false))
		require.FileExists(t, f.markerPath(t, d))
		second()
		require.NoError(t, d.Settle(f.path, false))
		require.NoFileExists(t, f.markerPath(t, d))
		require.Equal(t, 0.0, testutil.ToFloat64(d.inflight))
	})

	t.Run("git holds the reference lock", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		lock := filepath.Join(f.path, "reftable", "tables.list.lock")
		require.NoError(t, os.WriteFile(lock, nil, 0o666))

		require.NoError(t, d.Settle(f.path, false))
		require.FileExists(t, f.markerPath(t, d))
		require.FileExists(t, lock, "the durable step removed a lock it did not take")
	})
}

// TestStackLockReleaseRemovesOnlyItsOwnLock verifies that the durable step
// never removes a reference lock that is not the one it created: one that
// someone else removed and git took again stays.
func TestStackLockReleaseRemovesOnlyItsOwnLock(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	lockPath := filepath.Join(f.path, "reftable", "tables.list.lock")

	lock, ok, err := lockStack(f.path)
	require.NoError(t, err)
	require.True(t, ok)
	_, ok, err = lockStack(f.path)
	require.NoError(t, err)
	require.False(t, ok, "the lock is exclusive")

	// The same inode with another content: what a reused inode looks like to
	// the file check. The nonce tells it is not the step's lock.
	require.NoError(t, os.WriteFile(lockPath, []byte(stackLockTag+"0123456789abcdef\n"), 0o666))
	current, err := os.Lstat(lockPath)
	require.NoError(t, err)
	require.True(t, os.SameFile(current, lock.info), "the file check alone would take it for the step's lock")
	require.NoError(t, lock.release())
	require.FileExists(t, lockPath, "a lock with another nonce is not removed, even in the same inode")

	// Housekeeping removed the lock as stale: the release has nothing to do.
	require.NoError(t, os.Remove(lockPath))
	require.NoError(t, lock.release())
	require.NoFileExists(t, lockPath)

	lock, ok, err = lockStack(f.path)
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, lock.release())
	require.NoFileExists(t, lockPath)
}

func TestSettleReportsAFailedWriteAsUntrusted(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	writeback := errors.New("input/output error")
	f.synced.fail(writeback)

	err := d.Settle(f.path, false)
	require.ErrorIs(t, err, ErrUntrusted)
	require.ErrorIs(t, err, writeback)
	var untrusted *UntrustedError
	require.ErrorAs(t, err, &untrusted)
	require.Equal(t, f.repo, untrusted.Repo)
	require.Equal(t, f.path, untrusted.Path)

	// The repository stays dirty until the caller moves it aside.
	require.FileExists(t, f.markerPath(t, d))
	require.Equal(t, []string{f.path}, d.Dirty())

	// The kernel reports a failed writeback once: the next fsync succeeds, and
	// the repository stays untrusted all the same.
	f.synced.fail(nil)
	err = d.Settle(f.path, true)
	require.ErrorIs(t, err, ErrUntrusted, "a later successful fsync does not make the repository clean")
	require.ErrorIs(t, err, writeback)
	require.FileExists(t, f.markerPath(t, d))

	// Only moving it aside (Forget) ends it.
	require.NoError(t, d.Forget(f.ctx, f.repo, f.path))
	require.NoFileExists(t, f.markerPath(t, d))
	require.Empty(t, d.Dirty())
	require.Equal(t, 0.0, testutil.ToFloat64(d.dirty))
}

// testWatermarkPath is gitops.WatermarkPath, the watermark of the syncer.
func testWatermarkPath(repoPath string) string {
	return filepath.Join(repoPath, "graft", "storage", "applied")
}

// TestDurabilityDefersTheWatermarkOfADirtyRepository checks that the watermark
// is written without fsync only by the owner configured with WithWatermark, and
// only for a trusted dirty repository with a reftable stack, whose durable step
// makes it durable.
func TestDurabilityDefersTheWatermarkOfADirtyRepository(t *testing.T) {
	t.Parallel()

	t.Run("without the option", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.False(t, d.DefersWatermark(f.path))
	})

	t.Run("while the repository is dirty", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0, WithWatermark(testWatermarkPath))

		require.False(t, d.DefersWatermark(f.path), "a clean repository")
		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.True(t, d.DefersWatermark(f.path))
		require.NoError(t, d.Settle(f.path, false))
		require.False(t, d.DefersWatermark(f.path), "the marker is gone")
	})

	t.Run("an untrusted repository", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0, WithWatermark(testWatermarkPath))

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		f.synced.fail(errors.New("input/output error"))
		require.ErrorIs(t, d.Settle(f.path, false), ErrUntrusted)
		require.False(t, d.DefersWatermark(f.path))
	})

	t.Run("a repository without a stack", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.durability(t, "boot-a", 0, WithWatermark(testWatermarkPath))

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.NoError(t, os.Rename(filepath.Join(f.path, "reftable"), filepath.Join(t.TempDir(), "reftable")))
		require.False(t, d.DefersWatermark(f.path))
	})

	t.Run("a process that does not own the storage", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		d := f.unowned(t, "boot-a", 0, WithWatermark(testWatermarkPath))

		require.False(t, d.DefersWatermark(f.path))
	})

	t.Run("a nil durability", func(t *testing.T) {
		t.Parallel()
		var d *Durability
		require.False(t, d.DefersWatermark("/any"))
	})
}

// TestDurableStepMakesTheWatermarkDurableBeforeTheMarkerGoes checks that a
// durable step and Forget fsync the watermark that the syncer wrote without
// fsync, and its directory, while the marker is still there, and outside git's
// reference lock, which the transactions of the repository wait for. A
// repository without a stack gets the same.
func TestDurableStepMakesTheWatermarkDurableBeforeTheMarkerGoes(t *testing.T) {
	t.Parallel()

	// watermarkSync is what the fsync of the watermark found.
	type watermarkSync struct {
		synced bool
		// marker and gitLock tell whether the dirty marker and git's reference
		// lock existed then.
		marker, gitLock bool
	}
	setup := func(t *testing.T) (durabilityFixture, *Durability, string, *watermarkSync) {
		f := setupDurability(t)
		watermark := testWatermarkPath(f.path)
		require.NoError(t, os.MkdirAll(filepath.Dir(watermark), 0o755))
		require.NoError(t, os.WriteFile(watermark, []byte(`{"repo_id":1,"seq":2}`), 0o644))

		var found watermarkSync
		var d *Durability
		d = f.durability(t, "boot-a", 0, WithWatermark(testWatermarkPath), WithSync(func(file *os.File) error {
			if file.Name() == watermark {
				found.synced = true
				_, err := os.Stat(f.markerPath(t, d))
				found.marker = err == nil
				_, err = os.Stat(filepath.Join(f.path, "reftable", "tables.list.lock"))
				found.gitLock = err == nil
			}
			return f.synced.sync(file)
		}))
		return f, d, watermark, &found
	}

	t.Run("settle", func(t *testing.T) {
		t.Parallel()
		f, d, watermark, found := setup(t)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.NoError(t, d.Settle(f.path, false))

		recorded := f.synced.recorded()
		require.Contains(t, recorded, watermark)
		require.Contains(t, recorded, filepath.Dir(watermark))
		require.Equal(t, watermarkSync{synced: true, marker: true}, *found,
			"the watermark is durable before the marker goes, and its fsync does not hold git's lock")
		require.NoFileExists(t, f.markerPath(t, d))
	})

	t.Run("forget", func(t *testing.T) {
		t.Parallel()
		f, d, watermark, found := setup(t)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.NoError(t, d.Forget(f.ctx, f.repo, f.path))

		require.Contains(t, f.synced.recorded(), watermark)
		require.Equal(t, watermarkSync{synced: true, marker: true}, *found)
		require.NoFileExists(t, f.markerPath(t, d))
	})

	t.Run("a step that is not due", func(t *testing.T) {
		t.Parallel()
		f := setupDurability(t)
		watermark := testWatermarkPath(f.path)
		require.NoError(t, os.MkdirAll(filepath.Dir(watermark), 0o755))
		require.NoError(t, os.WriteFile(watermark, []byte(`{"repo_id":1,"seq":2}`), 0o644))
		d := f.durability(t, "boot-a", time.Hour, WithWatermark(testWatermarkPath))

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.NoError(t, d.Settle(f.path, false))
		require.NotContains(t, f.synced.recorded(), watermark, "the marker stays, so the watermark need not be durable yet")
		require.FileExists(t, f.markerPath(t, d))
	})

	for _, forget := range []bool{false, true} {
		desc := "a repository without a stack, settled"
		if forget {
			desc = "a repository without a stack, forgotten"
		}
		t.Run(desc, func(t *testing.T) {
			t.Parallel()
			f, d, watermark, found := setup(t)

			// The syncer wrote the watermark without fsync while the repository had a
			// stack, which has gone since.
			require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
			require.True(t, d.DefersWatermark(f.path))
			require.NoError(t, os.Rename(filepath.Join(f.path, "reftable"), filepath.Join(t.TempDir(), "reftable")))

			if forget {
				require.NoError(t, d.Forget(f.ctx, f.repo, f.path))
			} else {
				require.NoError(t, d.Settle(f.path, false))
			}

			require.Contains(t, f.synced.recorded(), watermark)
			require.Contains(t, f.synced.recorded(), filepath.Dir(watermark))
			require.Equal(t, watermarkSync{synced: true, marker: true}, *found)
			require.NoFileExists(t, f.markerPath(t, d))
		})
	}

	t.Run("a failed fsync of the watermark", func(t *testing.T) {
		t.Parallel()
		f, d, _, _ := setup(t)

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		f.synced.mu.Lock()
		f.synced.failOnly = filepath.Join("graft", "storage")
		f.synced.mu.Unlock()
		f.synced.fail(errors.New("input/output error"))

		require.ErrorIs(t, d.Settle(f.path, false), ErrUntrusted)
		require.FileExists(t, f.markerPath(t, d))
	})

	t.Run("no watermark", func(t *testing.T) {
		t.Parallel()
		f, d, watermark, _ := setup(t)
		require.NoError(t, os.RemoveAll(filepath.Dir(watermark)))

		require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
		require.NoError(t, d.Settle(f.path, false))
		require.NoFileExists(t, f.markerPath(t, d))
	})
}

// TestUntrustedSurvivesAProcessRestart verifies that the untrusted state is in
// the marker: a restart of the process in the same boot, whose fsyncs then
// succeed, finds the repository untrusted and never settles it.
func TestUntrustedSurvivesAProcessRestart(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)

	before := f.durability(t, "boot-a", 0)
	require.NoError(t, before.MarkDirty(f.ctx, f.repo, f.path))
	// The writes of the references were lost; the marker's directory is fine.
	f.synced.mu.Lock()
	f.synced.failOnly = "/reftable"
	f.synced.mu.Unlock()
	f.synced.fail(errors.New("input/output error"))
	require.ErrorIs(t, before.Settle(f.path, false), ErrUntrusted)
	f.synced.fail(nil)
	content, err := os.ReadFile(f.markerPath(t, before))
	require.NoError(t, err)
	require.Contains(t, string(content), `"untrusted":"fsync: input/output error"`)
	require.NoError(t, before.Close())

	after := f.durability(t, "boot-a", 0)
	found, err := after.Recover(f.ctx, []string{f.repo.Storage})
	require.NoError(t, err)
	require.Equal(t, []Recovered{{Repo: f.repo, Path: f.path, Untrusted: true}}, found)
	err = after.Settle(f.path, true)
	require.ErrorIs(t, err, ErrUntrusted)
	require.FileExists(t, f.markerPath(t, after))
}

// TestForgetKeepsARepositoryWithALiveGitProcess verifies that moving a
// repository aside while a git process that writes without fsync is still
// running keeps the repository dirty: that process names the repository by its
// path, so its later writes land in what is there now.
func TestForgetKeepsARepositoryWithALiveGitProcess(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	end := d.Begin(f.path)
	f.synced.fail(errors.New("input/output error"))
	require.ErrorIs(t, d.Settle(f.path, false), ErrUntrusted)
	f.synced.fail(nil)

	require.NoError(t, d.Forget(f.ctx, f.repo, f.path))
	require.FileExists(t, f.markerPath(t, d))
	content, err := os.ReadFile(f.markerPath(t, d))
	require.NoError(t, err)
	require.NotContains(t, string(content), "untrusted", "what is at the path now is not the untrusted repository")
	require.Equal(t, []string{f.path}, d.Dirty())
	require.NoError(t, d.Settle(f.path, true))
	require.FileExists(t, f.markerPath(t, d), "the git process is still running")

	end()
	require.NoError(t, d.Settle(f.path, false))
	require.NoFileExists(t, f.markerPath(t, d))
}

// TestRecoveredMarkerWaitsForGit verifies that a marker found at the
// recovery stays while git's lock or a table being written is in the
// repository: a git process of the previous owner may still be writing.
func TestRecoveredMarkerWaitsForGit(t *testing.T) {
	t.Parallel()

	for _, leftover := range []string{"tables.list.lock", "0x000000000009-0x000000000009-abcdef01.ref.lock", "0x000000000009-0x000000000009-abcdef01.ref.temp.XyZ123"} {
		t.Run(leftover, func(t *testing.T) {
			t.Parallel()
			f := setupDurability(t)

			old := f.durability(t, "boot-a", 0)
			require.NoError(t, old.MarkDirty(f.ctx, f.repo, f.path))
			require.NoError(t, old.Close())

			file := filepath.Join(f.path, "reftable", leftover)
			require.NoError(t, os.WriteFile(file, []byte("0x000000000001-0x000000000003-abcdef01.ref\n"), 0o666))

			next := f.durability(t, "boot-a", 0)
			_, err := next.Recover(f.ctx, []string{f.repo.Storage})
			require.NoError(t, err)
			require.NoError(t, next.Settle(f.path, true))
			require.FileExists(t, f.markerPath(t, next), "git may still be writing")

			require.NoError(t, os.Remove(file))
			require.NoError(t, next.Settle(f.path, true))
			require.NoFileExists(t, f.markerPath(t, next))
		})
	}
}

// TestSettleDropsTheStateOfACleanRepository verifies that the states do not
// grow with every repository ever written, and that a writer after the drop
// marks the repository again.
func TestSettleDropsTheStateOfACleanRepository(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, d.Settle(f.path, false))
	d.mu.Lock()
	require.Empty(t, d.repos)
	d.mu.Unlock()

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.FileExists(t, f.markerPath(t, d))
	require.Equal(t, []string{f.path}, d.Dirty())
}

// TestSettleTransientErrorIsNotUntrusted verifies that a failure to read the
// stack (not a failed fsync) is retried: the repository is not distrusted.
func TestSettleTransientErrorIsNotUntrusted(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))

	list := filepath.Join(f.path, "reftable", "tables.list")
	require.NoError(t, os.Rename(list, list+".away"))
	err := d.Settle(f.path, false)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrUntrusted)
	require.FileExists(t, f.markerPath(t, d))

	require.NoError(t, os.Rename(list+".away", list))
	require.NoError(t, d.Settle(f.path, false))
	require.NoFileExists(t, f.markerPath(t, d))
}

// TestSettleForgetsTablesTheStackNoLongerLists verifies that the set of tables
// made durable does not grow with tables compacted away.
func TestSettleForgetsTablesTheStackNoLongerLists(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", time.Hour)
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))

	d.mu.Lock()
	d.repos[f.path].synced["0x000000000001-0x000000000001-compacted.ref"] = struct{}{}
	d.mu.Unlock()

	require.NoError(t, d.Settle(f.path, false))
	d.mu.Lock()
	defer d.mu.Unlock()
	require.Len(t, d.repos[f.path].synced, len(f.tables(t)))
	require.NotContains(t, d.repos[f.path].synced, "0x000000000001-0x000000000001-compacted.ref")
}

func TestSettleDropsTheMarkerOfARepositoryThatIsGone(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	d := f.durability(t, "boot-a", 0)

	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, os.Rename(f.path, f.path+".moved"))
	t.Cleanup(func() { require.NoError(t, os.Rename(f.path+".moved", f.path)) })

	require.NoError(t, d.Settle(f.path, false))
	require.NoFileExists(t, f.markerPath(t, d))
	require.Empty(t, d.Dirty())
}

func TestRecoverTellsAKernelRestartFromAProcessRestart(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	storages := []string{f.repo.Storage}

	before := f.durability(t, "boot-a", time.Hour)
	require.NoError(t, before.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, before.Close())

	// The same boot: the process restarted, the kernel kept every write.
	sameBoot := f.durability(t, "boot-a", 0)
	found, err := sameBoot.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.Equal(t, []Recovered{{Repo: f.repo, Path: f.path, Reboot: false}}, found)
	require.Equal(t, []string{f.path}, sameBoot.Dirty())
	require.NoError(t, sameBoot.Settle(f.path, false))
	require.NoFileExists(t, f.markerPath(t, sameBoot))

	// Another boot: the writes may not have reached the disk. The repository is
	// untrusted until the caller moves it aside (Forget): never served, its
	// marker never removed by a durable step.
	require.NoError(t, sameBoot.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, sameBoot.Close())
	otherBoot := f.durability(t, "boot-b", 0)
	found, err = otherBoot.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.Equal(t, []Recovered{{Repo: f.repo, Path: f.path, Reboot: true}}, found)
	require.Equal(t, []string{f.path}, otherBoot.Dirty())
	require.True(t, otherBoot.Untrusted(f.path))
	require.ErrorIs(t, otherBoot.Settle(f.path, true), ErrUntrusted)
	require.FileExists(t, f.markerPath(t, otherBoot))
}

// TestRecoverRemovesWhatADurableStepThatDiedLeft verifies that the reference
// lock and the temporary files of a durable step of a process that died are
// removed when the next process owns the storage, and that a lock of git is
// not.
func TestRecoverRemovesWhatADurableStepThatDiedLeft(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	storages := []string{f.repo.Storage}
	lock := filepath.Join(f.path, "reftable", "tables.list.lock")

	dead := f.durability(t, "boot-a", 0)
	require.NoError(t, dead.MarkDirty(f.ctx, f.repo, f.path))
	_, ok, err := lockStack(f.path)
	require.NoError(t, err)
	require.True(t, ok)
	leftover := filepath.Join(f.path, stepTempPrefix+"123")
	require.NoError(t, os.WriteFile(leftover, []byte(stackLockTag+"0123456789abcdef\n"), 0o600))
	require.NoError(t, dead.Close())

	next := f.durability(t, "boot-a", 0)
	_, err = next.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.NoFileExists(t, lock)
	require.NoFileExists(t, leftover)
	require.NoError(t, next.Close())

	// A lock of git is kept.
	require.NoError(t, os.WriteFile(lock, []byte("0x000000000001-0x000000000003-abcdef01.ref\n"), 0o666))
	_, err = f.durability(t, "boot-a", 0).Recover(f.ctx, storages)
	require.NoError(t, err)
	require.FileExists(t, lock)
}

func TestRecoverRefusesACorruptMarker(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)
	storages := []string{f.repo.Storage}
	d := f.durability(t, "boot-a", 0)
	require.NoError(t, d.MarkDirty(f.ctx, f.repo, f.path))
	require.NoError(t, d.Close())

	marker := f.markerPath(t, d)
	incomplete := filepath.Join(filepath.Dir(marker), markerTempPrefix+"123.tmp")
	require.NoError(t, os.WriteFile(incomplete, []byte("{"), 0o600))

	next := f.durability(t, "boot-b", 0)
	found, err := next.Recover(f.ctx, storages)
	require.NoError(t, err)
	require.Len(t, found, 1)
	require.NoFileExists(t, incomplete, "an incomplete marker write is removed")
	require.FileExists(t, filepath.Join(filepath.Dir(marker), ownerLockName), "the owner lock is not a marker")
	require.NoError(t, next.Close())

	require.NoError(t, os.WriteFile(marker, []byte(`{"storage":"`), 0o600))
	_, err = f.durability(t, "boot-b", 0).Recover(f.ctx, storages)
	require.ErrorContains(t, err, "corrupt dirty marker")
}

func TestNilDurabilityIsOff(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	var d *Durability
	require.False(t, d.Enabled())
	require.False(t, d.Deferring())
	owned, err := d.TryOwn(ctx, []string{"default"})
	require.NoError(t, err)
	require.False(t, owned)
	require.NoError(t, d.MarkDirty(ctx, RepoRef{}, "path"))
	require.NoError(t, d.Settle("path", true))
	d.Begin("path")()
	d.Activate()
	found, err := d.Recover(ctx, []string{"default"})
	require.NoError(t, err)
	require.Empty(t, found)
	require.NoError(t, d.Close())
}

// TestTornReftableStackReadsAsAnEmptyRepository is the experiment behind the
// LOCAL REFERENCE WRITES RULE (doc/graft/PLAN7.md): a reftable stack that a
// crash tore (a table whose data never reached the disk) is read by git as a
// repository without references, without an error. Only writes fail. So a torn
// stack must never be served: nothing in git would tell.
func TestTornReftableStackReadsAsAnEmptyRepository(t *testing.T) {
	t.Parallel()
	f := setupDurability(t)

	refs := gittest.Exec(t, f.cfg, "-C", f.path, "for-each-ref", "--format=%(refname)")
	require.Equal(t, "refs/heads/a\nrefs/heads/b\nrefs/heads/main", strings.TrimSpace(string(refs)))

	tables := f.tables(t)
	newest := filepath.Join(f.path, "reftable", tables[len(tables)-1])
	require.NoError(t, os.Truncate(newest, 0))

	refs = gittest.Exec(t, f.cfg, "-C", f.path, "for-each-ref", "--format=%(refname)")
	require.Empty(t, strings.TrimSpace(string(refs)), "git reports the references of a torn stack")

	cmd := gittest.NewCommand(t, f.cfg, "-C", f.path, "update-ref", "refs/heads/new", f.commit.String())
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "corrupt reftable file")
}
