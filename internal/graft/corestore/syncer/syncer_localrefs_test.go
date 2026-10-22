package syncer_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/graft/corestore/localrefs"
	"github.com/feder-cr/git-ai/internal/graft/corestore/syncer"
)

// markers returns the dirty markers of the node's storage.
func (e *env) markers(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(e.storagePath, localrefs.MarkerDirName))
	if os.IsNotExist(err) {
		return nil
	}
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// untrusted returns graft_storage_ref_untrusted_total{reason} of the node.
func (e *env) untrusted(t *testing.T, reason string) float64 {
	t.Helper()

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(e.core.RefDurability))
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "graft_storage_ref_untrusted_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "reason" && label.GetValue() == reason {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// tearNewestTable truncates the newest table of the local repository's stack:
// what a kernel restart can leave of a table written without fsync.
func (e *env) tearNewestTable(t *testing.T) {
	t.Helper()

	list, err := os.ReadFile(filepath.Join(e.localPath(), "reftable", "tables.list"))
	require.NoError(t, err)
	names := strings.Fields(string(list))
	require.NotEmpty(t, names)
	require.NoError(t, os.Truncate(filepath.Join(e.localPath(), "reftable", names[len(names)-1]), 0))
}

// caughtUpDirty returns a node whose local repository was hydrated, then caught
// up with one changeset: its references were written without fsync, so the
// repository is dirty.
func caughtUpDirty(t *testing.T, opts ...localrefs.DurabilityOption) (*env, corestore.Repository, git.ObjectID) {
	t.Helper()

	e := newEnv(t, envOptions{})
	coretest.EnableLocalRefs(t, e.core, e.cfg, opts...)
	src := e.newSource(t)
	c1 := e.writeCommit(t, src)
	repo := e.register(t, e.key(localRelPath), src, c1)

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Empty(t, e.markers(t), "a hydration writes its references durably")

	c2 := e.advanceMain(t, repo.ID, src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))
	require.Len(t, e.markers(t), 1, "the catch-up marks the repository before writing without fsync")

	repo, err := e.core.Meta.GetRepository(e.ctx, repo.Key)
	require.NoError(t, err)
	return e, repo, c2
}

func TestSyncerCatchUpMarksTheRepositoryDirty(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t)
	e.verifySynced(t, repo)
	require.Equal(t, []string{e.localPath()}, e.core.RefDurability.Dirty())

	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.Empty(t, e.markers(t))
	require.Empty(t, e.core.RefDurability.Dirty())
	e.verifySynced(t, repo)
}

// TestSyncerDefersTheWatermarkOfADirtyRepository checks that the syncer writes
// the watermark without fsync while the repository is dirty, and with fsync once
// the durable step removed the marker.
func TestSyncerDefersTheWatermarkOfADirtyRepository(t *testing.T) {
	t.Parallel()

	e := newEnv(t, envOptions{})
	coretest.EnableLocalRefs(t, e.core, e.cfg)
	var points []string
	e.sy.SetTestHook(func(point string) {
		if strings.HasPrefix(point, "watermark") {
			points = append(points, point)
		}
	})
	src := e.newSource(t)
	c1 := e.writeCommit(t, src)
	repo := e.register(t, e.key(localRelPath), src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))

	c2 := e.advanceMain(t, repo.ID, src, c1)
	e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 2)
	require.Equal(t, []string{"watermark"}, points)

	// The catch-up writes references without fsync, so the repository is dirty.
	points = nil
	c3 := e.advanceMain(t, repo.ID, src, c2)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, []string{"watermark deferred"}, points)
	c4 := e.advanceMain(t, repo.ID, src, c3)
	points = nil
	e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 4)
	require.Equal(t, []string{"watermark deferred"}, points)
	w, ok, err := e.sy.Applied(e.ctx, e.pbRepo(localRelPath))
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, uint64(4), w.Seq)

	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.Empty(t, e.markers(t))
	points = nil
	e.advanceMain(t, repo.ID, src, c4)
	e.sy.NoteCommitted(e.ctx, e.localPath(), repo.ID, 5)
	require.Equal(t, []string{"watermark"}, points)
}

// TestSyncerMovesAsideATornWatermarkAfterAKernelRestart checks that a dirty
// repository whose watermark, written without fsync, is empty or torn after a
// kernel restart is moved aside and hydrated again like any dirty repository.
func TestSyncerMovesAsideATornWatermarkAfterAKernelRestart(t *testing.T) {
	t.Parallel()

	for name, torn := range map[string][]byte{"empty": nil, "garbage": []byte(`{"repo_id":`)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e, repo, _ := caughtUpDirty(t, localrefs.WithBootID("boot-a"))
			require.NoError(t, os.WriteFile(filepath.Join(e.localPath(), "graft", "storage", "applied"), torn, 0o644))

			coretest.EnableLocalRefs(t, e.core, e.cfg, localrefs.WithBootID("boot-b"))
			e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
			require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

			require.NoDirExists(t, e.localPath())
			require.Empty(t, e.markers(t))
			require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
			e.verifySynced(t, repo)
		})
	}
}

// TestSyncerMovesAsideARepositoryDirtyAtAKernelRestart is the recovery of the
// LOCAL REFERENCE WRITES RULE: a repository with references written without
// fsync when the kernel restarted is moved aside before anything reads it, and
// hydrated again. Without the recovery the torn stack reads as an empty
// repository, without an error.
func TestSyncerMovesAsideARepositoryDirtyAtAKernelRestart(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t, localrefs.WithBootID("boot-a"))
	e.tearNewestTable(t)

	// A read before the recovery returns the wrong state, and nothing reports
	// an error: git lists no references.
	torn := gittest.Exec(t, e.cfg, "-C", e.localPath(), "for-each-ref", "--format=%(refname)")
	require.Empty(t, strings.TrimSpace(string(torn)))

	// The node restarts after a kernel restart.
	coretest.EnableLocalRefs(t, e.core, e.cfg, localrefs.WithBootID("boot-b"))
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

	require.NoDirExists(t, e.localPath())
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(1), e.untrusted(t, localrefs.ReasonReboot))

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(1), syncCount(t, e.sy, "hydrated"), "the restarted node hydrates the repository moved aside")
	e.verifySynced(t, repo)
}

// TestSyncerKeepsARepositoryDirtyAtAProcessRestart verifies that a restart of
// the process alone keeps the repository: the kernel kept every write.
func TestSyncerKeepsARepositoryDirtyAtAProcessRestart(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t, localrefs.WithBootID("boot-a"))

	coretest.EnableLocalRefs(t, e.core, e.cfg, localrefs.WithBootID("boot-a"))
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

	require.DirExists(t, e.localPath())
	require.Equal(t, []string{e.localPath()}, e.core.RefDurability.Dirty())
	e.verifySynced(t, repo)

	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.Empty(t, e.markers(t))
}

// TestSyncerRecoveryKeepsWhatIsNotACache verifies that the recovery never moves
// a local repository that a hydration cannot rebuild: one the database does
// not hold. Its marker is dropped.
func TestSyncerRecoveryKeepsWhatIsNotACache(t *testing.T) {
	t.Parallel()

	e := newEnv(t, envOptions{})
	coretest.EnableLocalRefs(t, e.core, e.cfg, localrefs.WithBootID("boot-a"))
	pb, path := gittest.CreateRepository(t, e.ctx, e.cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		ReferenceBackend:       git.ReferenceBackendReftables,
	})
	require.NoError(t, e.core.RefDurability.MarkDirty(e.ctx, localrefs.RepoRef{Storage: pb.GetStorageName(), RelativePath: pb.GetRelativePath()}, path))

	coretest.EnableLocalRefs(t, e.core, e.cfg, localrefs.WithBootID("boot-b"))
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

	require.DirExists(t, path)
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(0), e.untrusted(t, localrefs.ReasonReboot))
}

// TestSyncerDurableStepMovesAsideOnAFailedWrite verifies the runtime half of
// the rule: when the kernel reports that a write of the references failed, the
// repository is moved aside at once, without waiting for a restart, and the
// next request hydrates it.
func TestSyncerDurableStepMovesAsideOnAFailedWrite(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	e, repo, _ := caughtUpDirty(t, localrefs.WithSync(func(f *os.File) error {
		if failing.Load() {
			return errors.New("input/output error")
		}
		return f.Sync()
	}))

	failing.Store(true)
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	failing.Store(false)

	require.NoDirExists(t, e.localPath())
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(1), e.untrusted(t, localrefs.ReasonSyncFailed))

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(2), syncCount(t, e.sy, "hydrated"), "the first hydration and the one after the move")
	e.verifySynced(t, repo)
}

// TestSyncerUntrustedRepositoryIsNeverSettled is a failed fsync whose move
// aside fails too (the temporary directory cannot be written for a moment).
// The kernel reports the failed writeback once, so the next step's fsyncs
// succeed: the repository must still be moved aside, never settled.
func TestSyncerUntrustedRepositoryIsNeverSettled(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	e, repo, _ := caughtUpDirty(t, localrefs.WithSync(func(f *os.File) error {
		if failing.Load() {
			return errors.New("input/output error")
		}
		return f.Sync()
	}))
	tmpDir, err := e.locator.TempDir(e.storageName)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmpDir, 0o700))

	failing.Store(true)
	require.NoError(t, os.Chmod(tmpDir, 0o500))
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	failing.Store(false)
	require.NoError(t, os.Chmod(tmpDir, 0o700))

	require.DirExists(t, e.localPath(), "the move aside failed")
	require.Len(t, e.markers(t), 1, "the marker stays")

	e.sy.DurableStep(e.ctx, e.localPath(), true)
	require.NoDirExists(t, e.localPath(), "moved aside, although its fsyncs succeed now")
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(1), e.untrusted(t, localrefs.ReasonSyncFailed))

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	e.verifySynced(t, repo)
}

// failReferences returns a sync function that fails the fsyncs of reference
// files while failing is set: the writes of the references were lost, the rest
// of the disk is fine.
func failReferences(failing *atomic.Bool) func(f *os.File) error {
	return func(f *os.File) error {
		if failing.Load() && strings.Contains(f.Name(), "/reftable") {
			return errors.New("input/output error")
		}
		return f.Sync()
	}
}

// TestSyncerUntrustedRepositoryAcrossAProcessRestart is a failed fsync whose
// move aside fails, followed by a restart of the process in the same boot:
// the restarted process's fsyncs succeed, and the repository is moved aside
// all the same, never settled.
func TestSyncerUntrustedRepositoryAcrossAProcessRestart(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	e, repo, _ := caughtUpDirty(t, localrefs.WithSync(failReferences(&failing)))
	tmpDir, err := e.locator.TempDir(e.storageName)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmpDir, 0o700))

	failing.Store(true)
	require.NoError(t, os.Chmod(tmpDir, 0o500))
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.NoError(t, os.Chmod(tmpDir, 0o700))
	failing.Store(false)
	require.DirExists(t, e.localPath(), "the move aside failed")

	// The process restarts in the same boot.
	coretest.EnableLocalRefs(t, e.core, e.cfg)
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	require.NoError(t, e.sy.RecoverLocalReferences(e.ctx, []string{e.storageName}))

	require.NoDirExists(t, e.localPath(), "an untrusted repository is moved aside at the restart")
	require.Empty(t, e.markers(t))
	require.Equal(t, float64(1), e.untrusted(t, localrefs.ReasonSyncFailed))
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	e.verifySynced(t, repo)
}

// TestSyncerUntrustedRepositoryWithShadowHistoryIsKept verifies that an
// untrusted repository whose local copy holds history written in shadow mode
// and not imported yet (the guard of a sync's hydration) is moved aside but
// not deleted: it is never served, and the history is not lost.
func TestSyncerUntrustedRepositoryWithShadowHistoryIsKept(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	e, _, c2 := caughtUpDirty(t, localrefs.WithSync(failReferences(&failing)))
	_, err := changelog.Append(e.localPath(), changelog.Meta{Operation: "push", Actor: "alice"}, []changelog.Update{
		{Reference: "refs/heads/shadow", OldOID: gittest.DefaultObjectHash.ZeroOID.String(), NewOID: c2.String()},
	})
	require.NoError(t, err)

	failing.Store(true)
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	failing.Store(false)

	require.NoDirExists(t, e.localPath(), "never served")
	kept, err := filepath.Glob(filepath.Join(e.storagePath, syncer.KeptDirName, "*", "repo.git"))
	require.NoError(t, err)
	require.Len(t, kept, 1, "the copy with the shadow history is kept")
	last, err := changelog.LastSequence(kept[0])
	require.NoError(t, err)
	require.Positive(t, last)
	require.Empty(t, e.markers(t))
}

// TestSyncerNeverServesAnUntrustedRepository verifies that a repository whose
// references may have lost writes is not served while it is still in place
// (its move aside failed): every request is Unavailable until the next durable
// step moves it aside, and the request after that hydrates it.
func TestSyncerNeverServesAnUntrustedRepository(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	e, repo, _ := caughtUpDirty(t, localrefs.WithSync(failReferences(&failing)))
	tmpDir, err := e.locator.TempDir(e.storageName)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmpDir, 0o700))

	failing.Store(true)
	require.NoError(t, os.Chmod(tmpDir, 0o500))
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.NoError(t, os.Chmod(tmpDir, 0o700))
	failing.Store(false)
	require.DirExists(t, e.localPath(), "the move aside failed")

	err = e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath))
	require.Equal(t, codes.Unavailable, status.Code(err), "%v", err)

	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.NoDirExists(t, e.localPath())
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	e.verifySynced(t, repo)
}

// TestSyncerRecoveryKeepsACopyWhoseHistoryCannotBeRead verifies that a local
// changelog that cannot be read does not stop the recovery, so the node: the
// untrusted copy is moved aside and kept for an operator, as one with history
// not imported yet, and the kept-copies metrics report it.
func TestSyncerRecoveryKeepsACopyWhoseHistoryCannotBeRead(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t, localrefs.WithBootID("boot-a"))
	// A directory where the changelog file is: reading it fails.
	require.NoError(t, os.MkdirAll(changelog.Path(e.localPath()), 0o700))

	coretest.EnableLocalRefs(t, e.core, e.cfg, localrefs.WithBootID("boot-b"))
	e.sy = syncer.New(e.core, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)
	stop, err := e.sy.StartLocalReferences(e.ctx, []string{e.storageName})
	require.NoError(t, err)
	defer stop()

	require.NoDirExists(t, e.localPath(), "never served")
	kept, err := filepath.Glob(filepath.Join(e.storagePath, syncer.KeptDirName, "*", "repo.git"))
	require.NoError(t, err)
	require.Len(t, kept, 1)
	require.Empty(t, e.markers(t))

	reg := prometheus.NewPedanticRegistry()
	require.NoError(t, reg.Register(e.sy))
	families, err := reg.Gather()
	require.NoError(t, err)
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			values[family.GetName()] = metric.GetGauge().GetValue()
		}
	}
	require.Equal(t, float64(1), values["graft_storage_kept_untrusted_repositories"])
	require.Positive(t, values["graft_storage_kept_untrusted_bytes"])

	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	e.verifySynced(t, repo)
}

// TestSyncerUntrustedRepositoryThatIsGoneLosesItsMarker verifies that an
// untrusted repository that was moved away meanwhile (a stale cache) does not
// keep its marker forever.
func TestSyncerUntrustedRepositoryThatIsGoneLosesItsMarker(t *testing.T) {
	t.Parallel()

	var failing atomic.Bool
	e, _, _ := caughtUpDirty(t, localrefs.WithSync(func(f *os.File) error {
		if failing.Load() {
			return errors.New("input/output error")
		}
		return f.Sync()
	}))

	tmpDir, err := e.locator.TempDir(e.storageName)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tmpDir, 0o700))
	failing.Store(true)
	require.NoError(t, os.Chmod(tmpDir, 0o500))
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.NoError(t, os.Chmod(tmpDir, 0o700))
	failing.Store(false)
	require.Len(t, e.markers(t), 1)

	require.NoError(t, os.Rename(e.localPath(), filepath.Join(tmpDir, "moved.git")))
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.Empty(t, e.markers(t))
	require.Empty(t, e.core.RefDurability.Dirty())
}

// TestSyncerCatchUpOfANonOwnerWritesWithFsync verifies that a process that
// does not own the deferred durability catches up with fsync: it writes no
// marker.
func TestSyncerCatchUpOfANonOwnerWritesWithFsync(t *testing.T) {
	t.Parallel()

	e := newEnv(t, envOptions{})
	durability, err := localrefs.NewDurability(e.locator, 0, localrefs.WithBootID(coretest.TestBootID))
	require.NoError(t, err)
	e.core.RefDurability = durability
	require.False(t, durability.Deferring())

	src := e.newSource(t)
	c1 := e.writeCommit(t, src)
	repo := e.register(t, e.key(localRelPath), src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	e.advanceMain(t, repo.ID, src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Equal(t, float64(1), syncCount(t, e.sy, "caught_up"))

	require.Empty(t, e.markers(t))
	require.Empty(t, durability.Dirty())
}

// TestSyncerDurableStepRetriesATransientError verifies that a durable step that
// fails without a failed fsync (the stack cannot be read for a moment) keeps
// the repository and its marker and is retried.
func TestSyncerDurableStepRetriesATransientError(t *testing.T) {
	t.Parallel()

	e, repo, _ := caughtUpDirty(t)
	list := filepath.Join(e.localPath(), "reftable", "tables.list")
	require.NoError(t, os.Rename(list, list+".away"))
	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.NoError(t, os.Rename(list+".away", list))

	require.DirExists(t, e.localPath())
	require.Len(t, e.markers(t), 1)
	require.Equal(t, float64(0), e.untrusted(t, localrefs.ReasonSyncFailed))

	e.sy.DurableStep(e.ctx, e.localPath(), false)
	require.Empty(t, e.markers(t))
	e.verifySynced(t, repo)
}

// TestSyncerSecondProcessWaitsForTheOwner is the overlap of a graceful
// upgrade: while the old process owns the deferred durability, the new one
// writes with fsync and touches no marker; once the old one is gone, the new
// one owns, recovers and settles.
func TestSyncerSecondProcessWaitsForTheOwner(t *testing.T) {
	t.Parallel()

	e, _, _ := caughtUpDirty(t)
	storages := []string{e.storageName}

	next := *e.core
	durability, err := localrefs.NewDurability(e.locator, 0, localrefs.WithBootID(coretest.TestBootID), localrefs.WithRecoveryGrace(0))
	require.NoError(t, err)
	next.RefDurability = durability
	t.Cleanup(func() { _ = durability.Close() })
	nextSyncer := syncer.New(&next, e.logger, e.locator, e.repoFactory, e.gitCmdFactory)

	stop, err := nextSyncer.StartLocalReferences(e.ctx, storages)
	require.NoError(t, err)
	defer stop()
	require.False(t, durability.Deferring(), "the old process owns the storage")
	require.Len(t, e.markers(t), 1)

	// The old process exits.
	require.NoError(t, e.core.RefDurability.Close())

	// The next process tries every durable step interval.
	require.Eventually(t, func() bool { return durability.Deferring() && len(e.markers(t)) == 0 },
		30*time.Second, 10*time.Millisecond, "the next process owns and settles the marker")
}

// TestSyncerStopSettlesEveryRepository verifies that a clean stop removes the
// marker of a repository whose writes ended, without waiting for it to be idle.
func TestSyncerStopSettlesEveryRepository(t *testing.T) {
	t.Parallel()

	e := newEnv(t, envOptions{})
	e.core.RefAdmission = localrefs.NewAdmission(10*time.Second, 10*time.Second)
	durability, err := localrefs.NewDurability(e.locator, time.Hour, localrefs.WithBootID(coretest.TestBootID), localrefs.WithRecoveryGrace(0))
	require.NoError(t, err)
	t.Cleanup(func() { _ = durability.Close() })
	e.core.RefDurability = durability

	stop, err := e.sy.StartLocalReferences(e.ctx, []string{e.storageName})
	require.NoError(t, err)
	require.True(t, durability.Deferring())

	src := e.newSource(t)
	c1 := e.writeCommit(t, src)
	repo := e.register(t, e.key(localRelPath), src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	e.advanceMain(t, repo.ID, src, c1)
	require.NoError(t, e.sy.EnsureFresh(e.ctx, e.pbRepo(localRelPath)))
	require.Len(t, e.markers(t), 1, "the repository is not idle for an hour")

	stop()
	require.Empty(t, e.markers(t))
}
