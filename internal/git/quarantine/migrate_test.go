package quarantine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

// The tests of this file replace beforeLink, syncDirFile and syncFile, so they are not parallel.

// writeQuarantine writes a quarantine at dir with one object in each of the fan-out directories
// fanouts and a pack with its index, and returns the relative paths of its files.
func writeQuarantine(t *testing.T, dir string, fanouts []string) []string {
	t.Helper()

	var files []string
	for _, fanout := range fanouts {
		files = append(files, filepath.Join(fanout, "object"+fanout))
	}
	files = append(files, filepath.Join("pack", "pack-1.pack"), filepath.Join("pack", "pack-1.idx"))

	for _, file := range files {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(file)), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(file), 0o444))
	}
	return files
}

// replaceHooks replaces the test hooks of migrate for the test and counts the fsyncs.
func replaceHooks(t *testing.T, before func(targetPath string)) (dirSyncs, fileSyncs *atomic.Int32) {
	t.Helper()

	dirSyncs, fileSyncs = &atomic.Int32{}, &atomic.Int32{}
	originalBefore, originalSyncDir, originalSyncFile := beforeLink, syncDirFile, syncFile
	t.Cleanup(func() { beforeLink, syncDirFile, syncFile = originalBefore, originalSyncDir, originalSyncFile })

	if before != nil {
		beforeLink = before
	}
	syncDirFile = func(f *os.File) error {
		dirSyncs.Add(1)
		return originalSyncDir(f)
	}
	syncFile = func(path string) error {
		fileSyncs.Add(1)
		return originalSyncFile(path)
	}
	return dirSyncs, fileSyncs
}

// TestMigrateRecreatesTargetDirectoriesThatVanish removes each empty target fan-out directory
// right before the first link into it, as housekeeping (git prune-packed) may. Before, the link
// and its rename fallback then failed with ENOENT. The migration creates the directory again and
// links every object.
func TestMigrateRecreatesTargetDirectoriesThatVanish(t *testing.T) {
	ctx := testhelper.Context(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "quarantine"), filepath.Join(root, "objects")
	fanouts := []string{"0a", "1b", "2c"}
	files := writeQuarantine(t, source, fanouts)

	// The target fan-out directories exist and are empty, as housekeeping leaves them.
	require.NoError(t, os.MkdirAll(filepath.Join(target, "pack"), 0o755))
	for _, fanout := range fanouts {
		require.NoError(t, os.Mkdir(filepath.Join(target, fanout), 0o755))
	}

	var removed sync.Map
	replaceHooks(t, func(targetPath string) {
		dir := filepath.Dir(targetPath)
		if filepath.Base(dir) == "pack" {
			return
		}
		if _, done := removed.LoadOrStore(dir, true); !done {
			require.NoError(t, os.Remove(dir))
		}
	})

	require.NoError(t, migrate(ctx, source, target, false))

	for _, file := range files {
		require.FileExists(t, filepath.Join(target, file))
	}
	for _, fanout := range fanouts {
		_, ok := removed.Load(filepath.Join(target, fanout))
		require.True(t, ok, "the directory %s was removed during the migration", fanout)
	}
	require.NoDirExists(t, source)
}

// TestMigrateRecreatesTargetDirectoriesThatVanishAgain removes each target fan-out directory before
// the first three links into it, so it vanishes again after the migration created it again, as
// git prune-packed may each time between the mkdir and the link. git's own writer retries only
// once after its mkdir (create_tmpfile), which a second removal defeats; the migration keeps
// creating the directory until the link holds.
func TestMigrateRecreatesTargetDirectoriesThatVanishAgain(t *testing.T) {
	ctx := testhelper.Context(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "quarantine"), filepath.Join(root, "objects")
	fanouts := []string{"0a", "1b"}
	files := writeQuarantine(t, source, fanouts)
	require.NoError(t, os.MkdirAll(filepath.Join(target, "pack"), 0o755))

	const removals = 3
	var mu sync.Mutex
	removedTimes := map[string]int{}
	replaceHooks(t, func(targetPath string) {
		dir := filepath.Dir(targetPath)
		if filepath.Base(dir) == "pack" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if removedTimes[dir] < removals {
			removedTimes[dir]++
			require.NoError(t, os.Remove(dir))
		}
	})

	require.NoError(t, migrate(ctx, source, target, false))

	for _, file := range files {
		require.FileExists(t, filepath.Join(target, file))
	}
	for _, fanout := range fanouts {
		require.Equal(t, removals, removedTimes[filepath.Join(target, fanout)], fanout)
	}
	require.NoDirExists(t, source)
}

// TestMigrateSyncsTheDirectoryThatHoldsTheLink removes each target fan-out directory right before
// the first link into it and creates it again, as housekeeping and then another writer may. The
// link then succeeds into the new directory. The second pass must fsync that directory, not the
// removed one: every descriptor it fsyncs is of the directory now at its path.
func TestMigrateSyncsTheDirectoryThatHoldsTheLink(t *testing.T) {
	ctx := testhelper.Context(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "quarantine"), filepath.Join(root, "objects")
	fanouts := []string{"0a", "1b", "2c"}
	files := writeQuarantine(t, source, fanouts)
	require.NoError(t, os.MkdirAll(target, 0o755))

	var recreated sync.Map
	replaceHooks(t, func(targetPath string) {
		dir := filepath.Dir(targetPath)
		if filepath.Base(dir) == "pack" {
			return
		}
		if _, done := recreated.LoadOrStore(dir, true); !done {
			require.NoError(t, os.Remove(dir))
			require.NoError(t, os.Mkdir(dir, 0o755))
		}
	})

	var synced []string
	originalSyncDir := syncDirFile
	syncDirFile = func(f *os.File) error {
		descriptor, err := f.Stat()
		require.NoError(t, err)
		current, err := os.Stat(f.Name())
		require.NoError(t, err)
		require.True(t, os.SameFile(descriptor, current), "the second pass fsyncs a removed directory: %s", f.Name())
		synced = append(synced, f.Name())
		return originalSyncDir(f)
	}

	require.NoError(t, migrate(ctx, source, target, false))

	for _, file := range files {
		require.FileExists(t, filepath.Join(target, file))
	}
	for _, fanout := range fanouts {
		require.Contains(t, synced, filepath.Join(target, fanout))
	}
}

// TestMigrateSyncsEachDirectoryOnce counts the fsyncs: one per target directory that received a
// link, one for the object directory, and none per file, whose writers made them durable.
func TestMigrateSyncsEachDirectoryOnce(t *testing.T) {
	ctx := testhelper.Context(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "quarantine"), filepath.Join(root, "objects")
	fanouts := []string{"0a", "1b", "2c", "3d"}
	writeQuarantine(t, source, fanouts)
	require.NoError(t, os.MkdirAll(target, 0o755))

	dirSyncs, fileSyncs := replaceHooks(t, nil)
	require.NoError(t, migrate(ctx, source, target, false))

	require.EqualValues(t, len(fanouts)+1+1, dirSyncs.Load(), "the fan-out directories, pack and the object directory")
	require.Zero(t, fileSyncs.Load())
}

// TestMigrateSyncsTheFilesOfAnUntrustedWriter fsyncs every file of a quarantine that a writer
// Gitaly does not control wrote into, and still each directory once.
func TestMigrateSyncsTheFilesOfAnUntrustedWriter(t *testing.T) {
	ctx := testhelper.Context(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "quarantine"), filepath.Join(root, "objects")
	fanouts := []string{"0a", "1b"}
	files := writeQuarantine(t, source, fanouts)
	require.NoError(t, os.MkdirAll(target, 0o755))

	dirSyncs, fileSyncs := replaceHooks(t, nil)
	require.NoError(t, migrate(ctx, source, target, true))

	require.EqualValues(t, len(files), fileSyncs.Load())
	require.EqualValues(t, len(fanouts)+1+1, dirSyncs.Load())
}

// fakeTransaction makes storage.NeedsSync false, as a transaction of the transaction manager does.
type fakeTransaction struct {
	storage.Transaction
}

// TestMigrateDoesNotSyncUnderATransaction leaves durability to the transaction manager.
func TestMigrateDoesNotSyncUnderATransaction(t *testing.T) {
	ctx := storage.ContextWithTransaction(testhelper.Context(t), fakeTransaction{})
	require.False(t, storage.NeedsSync(ctx))

	root := t.TempDir()
	source, target := filepath.Join(root, "quarantine"), filepath.Join(root, "objects")
	files := writeQuarantine(t, source, []string{"0a"})
	require.NoError(t, os.MkdirAll(target, 0o755))

	dirSyncs, fileSyncs := replaceHooks(t, nil)
	require.NoError(t, migrate(ctx, source, target, true))

	require.Zero(t, dirSyncs.Load())
	require.Zero(t, fileSyncs.Load())
	for _, file := range files {
		require.FileExists(t, filepath.Join(target, file))
	}
}

// TestMigrateKeepsThePackOrder links the data of a pack before its index.
func TestMigrateKeepsThePackOrder(t *testing.T) {
	ctx := testhelper.Context(t)
	root := t.TempDir()
	source, target := filepath.Join(root, "quarantine"), filepath.Join(root, "objects")
	writeQuarantine(t, source, nil)
	require.NoError(t, os.MkdirAll(target, 0o755))

	var linked []string
	var mu sync.Mutex
	replaceHooks(t, func(targetPath string) {
		mu.Lock()
		defer mu.Unlock()
		linked = append(linked, filepath.Base(targetPath))
	})

	require.NoError(t, migrate(ctx, source, target, false))
	require.Equal(t, []string{"pack-1.pack", "pack-1.idx"}, linked)
}

// TestMarkUntrustedWriter marks the quarantine whose object directory a hook was given, and only
// while the quarantine exists.
func TestMarkUntrustedWriter(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	locator := config.NewLocator(cfg)

	marked, cleanupMarked, err := New(ctx, repo, testhelper.NewLogger(t), locator)
	require.NoError(t, err)
	defer cleanupMarked()
	other, cleanupOther, err := New(ctx, repo, testhelper.NewLogger(t), locator)
	require.NoError(t, err)
	defer cleanupOther()

	MarkUntrustedWriter(marked.dir.Path() + "/")
	require.True(t, marked.untrustedWriter.Load())
	require.False(t, other.untrustedWriter.Load())

	// A path that is no quarantine marks nothing.
	MarkUntrustedWriter(t.TempDir())
	require.False(t, other.untrustedWriter.Load())

	path := other.dir.Path()
	cleanupOther()
	_, registered := quarantines.Load(filepath.Clean(path))
	require.False(t, registered, "a quarantine leaves the registry when it is cleaned up")
}

// TestMigrateWhileGitPrunesPacked runs migrations into a repository while git prune-packed, which
// housekeeping runs after a repack, removes its empty fan-out directories in a loop.
func TestMigrateWhileGitPrunesPacked(t *testing.T) {
	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	_, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{SkipCreationViaService: true})
	objects := filepath.Join(repoPath, "objects")

	// Every fan-out directory exists and is empty, the state that prune-packed cleans up.
	for i := 0; i < 256; i++ {
		require.NoError(t, os.MkdirAll(filepath.Join(objects, fmt.Sprintf("%02x", i)), 0o755))
	}

	stop := make(chan struct{})
	var pruned sync.WaitGroup
	pruned.Add(1)
	runs := 0
	go func() {
		defer pruned.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			var stderr bytes.Buffer
			cmd := gittest.NewCommand(t, cfg, "-C", repoPath, "prune-packed", "--quiet")
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Errorf("prune-packed: %v: %s", err, stderr.String())
				return
			}
			runs++
			// Recreate empty directories, so that the race stays possible.
			for i := 0; i < 256; i += 7 {
				_ = os.Mkdir(filepath.Join(objects, fmt.Sprintf("%02x", i)), 0o755)
			}
		}
	}()

	for i := 0; i < 200; i++ {
		source := filepath.Join(t.TempDir(), "quarantine")
		fanouts := []string{fmt.Sprintf("%02x", (i*7)%256), fmt.Sprintf("%02x", (i*13+1)%256)}
		var files []string
		for _, fanout := range fanouts {
			file := filepath.Join(fanout, fmt.Sprintf("object-%03d", i))
			require.NoError(t, os.MkdirAll(filepath.Join(source, fanout), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(source, file), []byte(file), 0o444))
			files = append(files, file)
		}

		require.NoError(t, migrate(context.Background(), source, objects, false), "migration %d", i)
		for _, file := range files {
			require.FileExists(t, filepath.Join(objects, file))
		}
	}

	close(stop)
	pruned.Wait()
	require.Positive(t, runs)

	var names []string
	entries, err := os.ReadDir(objects)
	require.NoError(t, err)
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	t.Logf("%d prune-packed runs during 200 migrations", runs)
}
