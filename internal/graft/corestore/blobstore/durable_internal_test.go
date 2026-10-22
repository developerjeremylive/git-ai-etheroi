package blobstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// openFileStoreForTest opens a file bucket and installs syncHook for the test.
func openFileStoreForTest(t *testing.T, dir string, hook func(path string, isDir bool) error) *Store {
	t.Helper()
	s, err := Open(testhelper.Context(t), "file://"+dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	syncHook = hook
	t.Cleanup(func() { syncHook = nil })
	return s
}

func TestPutSyncsFileBeforeCommitAndSyncsDirectories(t *testing.T) {
	dir := t.TempDir()
	content := []byte("pack content, synced before the commit")
	key := corestore.NewPackKey(1, sha256.Sum256(content))
	final := filepath.Join(dir, filepath.FromSlash(key))

	var fileSyncs int
	var dirSyncs []string
	s := openFileStoreForTest(t, dir, func(path string, isDir bool) error {
		if isDir {
			dirSyncs = append(dirSyncs, path)
			return nil
		}
		fileSyncs++
		// The complete content is in the temporary file, and the key is not
		// committed yet.
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(data, content) {
			return errors.New("the synced content is incomplete")
		}
		if _, err := os.Stat(final); !os.IsNotExist(err) {
			return errors.New("the key was committed before its content was synced")
		}
		return nil
	})

	require.NoError(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))

	require.Equal(t, 1, fileSyncs)
	// The directories from the key's parent up to the bucket root are synced,
	// the parent first and the root last.
	require.NotEmpty(t, dirSyncs)
	require.Equal(t, filepath.Dir(final), dirSyncs[0])
	require.Equal(t, dir, dirSyncs[len(dirSyncs)-1])

	data, err := os.ReadFile(final)
	require.NoError(t, err)
	require.Equal(t, content, data)
}

// TestPutSyncsOnlyTheDirectoriesThatAreNotDurable checks that a Put fsyncs the
// directories up to the bucket root once, then only the directory of its key,
// and that a directory created again (another inode) is fsynced again.
func TestPutSyncsOnlyTheDirectoriesThatAreNotDurable(t *testing.T) {
	dir := t.TempDir()
	var dirSyncs []string
	s := openFileStoreForTest(t, dir, func(path string, isDir bool) error {
		if isDir {
			dirSyncs = append(dirSyncs, path)
		}
		return nil
	})

	put := func(repoID int64) {
		dirSyncs = nil
		content := []byte("pack content")
		key := corestore.NewPackKey(repoID, sha256.Sum256(content))
		require.NoError(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))
	}
	repoDir := filepath.Join(dir, "repos", "1")
	packsDir := filepath.Join(repoDir, "packs")

	put(1)
	require.Equal(t, []string{packsDir, repoDir, filepath.Join(dir, "repos"), dir}, dirSyncs)

	put(1)
	require.Equal(t, []string{packsDir}, dirSyncs)

	put(2)
	repo2Dir := filepath.Join(dir, "repos", "2")
	require.Equal(t, []string{filepath.Join(repo2Dir, "packs"), repo2Dir, filepath.Join(dir, "repos")}, dirSyncs)

	// Moving the directory aside keeps its inode in use, so the new directory gets another one.
	require.NoError(t, os.Rename(repoDir, filepath.Join(t.TempDir(), "old")))
	put(1)
	require.Equal(t, []string{packsDir, repoDir, filepath.Join(dir, "repos")}, dirSyncs)
}

// TestPutSyncsAgainAfterAFailedDirectorySync checks that a Put whose fsync of an
// ancestor failed remembers none of its directories: the next Put fsyncs them
// all up to the root.
func TestPutSyncsAgainAfterAFailedDirectorySync(t *testing.T) {
	dir := t.TempDir()
	reposDir := filepath.Join(dir, "repos")
	fail := true
	var dirSyncs []string
	s := openFileStoreForTest(t, dir, func(path string, isDir bool) error {
		if !isDir {
			return nil
		}
		if fail && path == reposDir {
			return errors.New("input/output error")
		}
		dirSyncs = append(dirSyncs, path)
		return nil
	})

	content := []byte("pack content")
	key := corestore.NewPackKey(1, sha256.Sum256(content))
	require.Error(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))

	fail = false
	dirSyncs = nil
	key = corestore.NewPackKey(1, sha256.Sum256(content))
	require.NoError(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))
	repoDir := filepath.Join(reposDir, "1")
	require.Equal(t, []string{filepath.Join(repoDir, "packs"), repoDir, reposDir, dir}, dirSyncs)
}

// TestPutConcurrentWithTheFirstPutSyncsUpToTheRoot checks that a Put that runs
// while the first Put of a repository is still fsyncing its directories does not
// stop at one of them: it fsyncs its own way up to the root.
func TestPutConcurrentWithTheFirstPutSyncsUpToTheRoot(t *testing.T) {
	dir := t.TempDir()
	reposDir := filepath.Join(dir, "repos")
	reached, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	held := false
	var dirSyncs []string
	s := openFileStoreForTest(t, dir, func(path string, isDir bool) error {
		if !isDir {
			return nil
		}
		mu.Lock()
		hold := !held && path == reposDir
		held = held || hold
		dirSyncs = append(dirSyncs, path)
		mu.Unlock()
		if hold {
			close(reached)
			<-release
		}
		return nil
	})
	put := func() error {
		content := []byte("pack content")
		return s.Put(testhelper.Context(t), corestore.NewPackKey(1, sha256.Sum256(content)), bytes.NewReader(content))
	}

	// The first Put is held while it fsyncs repos/, after packs/ and repos/1/.
	first := make(chan error, 1)
	go func() { first <- put() }()
	<-reached
	mu.Lock()
	dirSyncs = nil
	mu.Unlock()

	require.NoError(t, put())
	repoDir := filepath.Join(reposDir, "1")
	mu.Lock()
	require.Equal(t, []string{filepath.Join(repoDir, "packs"), repoDir, reposDir, dir}, dirSyncs)
	mu.Unlock()

	close(release)
	require.NoError(t, <-first)
}

func TestPutFailedFileSyncLeavesNoObject(t *testing.T) {
	dir := t.TempDir()
	content := []byte("pack content whose sync fails")
	key := corestore.NewPackKey(1, sha256.Sum256(content))
	final := filepath.Join(dir, filepath.FromSlash(key))

	syncErr := errors.New("disk on fire")
	s := openFileStoreForTest(t, dir, func(_ string, isDir bool) error {
		if isDir {
			return nil
		}
		return syncErr
	})

	err := s.Put(testhelper.Context(t), key, bytes.NewReader(content))
	require.ErrorIs(t, err, syncErr)

	_, statErr := os.Stat(final)
	require.True(t, os.IsNotExist(statErr), "the key must not be committed")

	// The temporary file is discarded too: only directories are left behind.
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return errors.New("left behind: " + path)
		}
		return nil
	}))
}

func TestPutWithoutFileSyncHookSucceeds(t *testing.T) {
	// The fsync steps run for real on a file bucket: the write is observable
	// afterwards. A second Put of the key is refused and leaves the object as it
	// is (PLAN7, PACK OWNERSHIP RULE 2: Put is strictly create-only; before phase 7
	// a repeated Put was a successful no-op).
	dir := t.TempDir()
	s := openFileStoreForTest(t, dir, nil)
	ctx := testhelper.Context(t)

	content := []byte("durable content")
	key := corestore.NewPackKey(2, sha256.Sum256(content))
	require.NoError(t, s.Put(ctx, key, bytes.NewReader(content)))
	require.ErrorIs(t, s.Put(ctx, key, bytes.NewReader(content)), corestore.ErrBlobExists)

	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
	require.NoError(t, err)
	require.Equal(t, content, data)
}

func TestPublishFileIsCreateOnly(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}
	final := filepath.Join(dir, "k.pack")

	first := write("k.pack.18f3a.tmp", "first")
	require.NoError(t, publishFile(first))
	// A network filesystem that lost the reply of a successful link retransmits
	// it and reports EEXIST: the name is then the same file, and the publish
	// succeeded.
	require.NoError(t, publishFile(first))

	second := write("k.pack.18f3b.tmp", "second")
	require.ErrorIs(t, publishFile(second), corestore.ErrBlobExists)

	data, err := os.ReadFile(final)
	require.NoError(t, err)
	require.Equal(t, "first", string(data), "the first publish is kept")
}

func TestFinalPath(t *testing.T) {
	for tmp, want := range map[string]string{
		"/b/repos/1/packs/k.pack.18f3a2b4c.tmp": "/b/repos/1/packs/k.pack",
		"/b/plain.ff.tmp":                       "/b/plain",
	} {
		got, ok := finalPath(tmp)
		require.True(t, ok, tmp)
		require.Equal(t, want, got)
	}
	for _, tmp := range []string{"/b/k.pack", "/b/k.pack.tmp", "/b/k.pack.xyz.tmp", "/b/k.pack..tmp", ".1.tmp"} {
		_, ok := finalPath(tmp)
		require.False(t, ok, tmp)
	}
}

func TestPutLeavesOnlyTheObject(t *testing.T) {
	// The writer's temporary name is removed after the link published the
	// object: only the object and its directories remain.
	dir := t.TempDir()
	s := openFileStoreForTest(t, dir, nil)

	content := []byte("published by a link")
	key := corestore.NewPackKey(4, sha256.Sum256(content))
	require.NoError(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))

	var files []string
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			files = append(files, path)
		}
		return nil
	}))
	require.Equal(t, []string{filepath.Join(dir, filepath.FromSlash(key))}, files)
}

func TestPutMemBucketWithoutFileSync(t *testing.T) {
	// mem buckets have no *os.File: Put must not require one and must not sync.
	s, err := Open(testhelper.Context(t), "mem://")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	syncHook = func(string, bool) error { return errors.New("no sync expected for mem buckets") }
	t.Cleanup(func() { syncHook = nil })

	content := []byte("mem content")
	key := corestore.NewPackKey(3, sha256.Sum256(content))
	require.NoError(t, s.Put(testhelper.Context(t), key, bytes.NewReader(content)))
}

func TestOpenTrimsSchemePrefix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prefixed")
	s, err := Open(testhelper.Context(t), "service+file://"+dir)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	require.Equal(t, dir, s.root, "the prefix is trimmed and the file driver is used")
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.True(t, info.IsDir())
}

func TestWithS3DefaultChecksumCalculation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "custom endpoint without checksum setting",
			input:    "s3://bucket?endpoint=minio.example.com",
			expected: "s3://bucket?endpoint=minio.example.com&request_checksum_calculation=when_required",
		},
		{
			name:     "explicit setting is kept",
			input:    "s3://bucket?endpoint=minio.example.com&request_checksum_calculation=when_supported",
			expected: "s3://bucket?endpoint=minio.example.com&request_checksum_calculation=when_supported",
		},
		{
			name:     "no custom endpoint is left alone",
			input:    "s3://bucket?region=us-east-1",
			expected: "s3://bucket?region=us-east-1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.Parse(tc.input)
			require.NoError(t, err)
			require.Equal(t, tc.expected, withS3DefaultChecksumCalculation(parsed))
		})
	}
}
