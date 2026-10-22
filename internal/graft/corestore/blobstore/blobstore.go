// Package blobstore implements corestore.BlobStore on a gocloud bucket. Owned
// by WP52 (doc/graft/PLAN6.md).
//
// Create-only writes. Every upload uses a new owned key (corestore.NewPackKey),
// so an object is never shared between two writers, and Put never succeeds on a
// key that exists: it returns corestore.ErrBlobExists and leaves the stored
// object as it is (doc/graft/PLAN7.md, PACK OWNERSHIP RULE). Put writes with
// blob.WriterOptions{IfNotExist: true}; the drivers report an existing key from
// the writer's Close as gcerrors.FailedPrecondition (the gocloud documentation
// names gcerrors.PreconditionFailed, which they do not return). Every registered
// driver implements IfNotExist (checked against gocloud.dev v0.46.0: s3blob,
// gcsblob, azureblob, memblob and fileblob), so no plain-write fallback exists.
//
// Exactly one of concurrent Puts of a key wins. memblob checks and inserts
// under its bucket-wide mutex, and S3, GCS and Azure use conditional requests
// that the server decides. fileblob does not: its writer's Close stats the key
// and then renames the temporary file onto it, under a mutex created per writer
// (gocloud.dev v0.46.0, fileblob.go 764 and 789), so two writers of one key both
// succeed and the second rename overwrites the first, in one process or across
// processes and hosts sharing the bucket. For file buckets Put therefore
// publishes the object itself with link(2) (publishFile), which the filesystem
// makes atomic and create-only for every writer: fileblob's writer creates the
// temporary file (<path>.<hex nanos>.tmp, so the key-to-path mapping stays
// fileblob's) and receives the content, the link gives it the key's name, and
// the writer is then discarded, which only removes the temporary name. File
// buckets therefore need a filesystem with hard links; one without them fails
// every Put with a clear error. No .attrs sidecar is bypassed: file buckets are
// opened with MetadataDontWrite.
//
// Atomic objects. gocloud writers commit the object on Close and discard it
// when the writer's context is cancelled before Close (probed: a cancelled
// writer leaves no object). Put cancels before Close on every failure, so a
// reader never sees a partial object.
//
// Durability of file buckets. Neither fileblob's writer nor a link fsyncs
// anything, and a changeset row in the database (synchronous=FULL) could
// outlive a pack whose data never reached the disk. For file buckets Put fsyncs
// the content before the link publishes it, and fsyncs the directories that
// hold the new entry after it, up to the first one whose whole chain up to the
// bucket root an earlier Put made durable. That chain is recognized by the
// directory's identity (device and inode), so bucket directories are never
// removed while a server uses the bucket: a directory removed and created again
// could reuse the inode of one that was durable. The mem and remote drivers
// commit their objects durably themselves.
//
// Listing. List reports objects only. fileblob also reports a writer's
// temporary file (<key>.<nanos>.tmp, in the key's directory because file
// buckets are opened with NoTempDir) while the write is in flight, and one that
// a crashed writer left behind. Callers that act on a listing (the garbage
// collector) must treat keys they did not write accordingly.
//
// Integrity. Pack keys (corestore.ParsePackKey) are verified: Put hashes the
// content while writing and refuses content that does not match its key, and
// Get returns a reader that hashes the content and returns
// corestore.ErrBlobCorrupt instead of io.EOF on a mismatch or a short read.
// Content delivered before the end of the stream is not verified until the
// reader reaches its end, so callers must treat any read error as failure.
package blobstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/s3blob"
	"gocloud.dev/gcerrors"

	_ "gocloud.dev/blob/azureblob" // register Azure driver
	_ "gocloud.dev/blob/gcsblob"   // register Google Cloud driver
	_ "gocloud.dev/blob/memblob"   // register in-memory driver
)

// Store is a bucket of the storage core.
type Store struct {
	bucket *blob.Bucket
	// root is the cleaned directory of a file bucket and empty for any other
	// driver. Put makes the writes of a file bucket durable from it.
	root string
	// durableDirs holds, by path, the os.FileInfo of the directories of a file
	// bucket whose entries up to the root were made durable (syncDirs).
	durableDirs sync.Map
}

var _ corestore.BlobStore = (*Store)(nil)

// fileContentType is the content type of file bucket writers. Setting it makes
// the driver open its file immediately instead of after sniffing the first 512
// bytes. File buckets store no metadata, so the type is not persisted.
const fileContentType = "application/octet-stream"

// syncHook, when set by a test, observes every fsync of a file bucket (isDir
// is true for a directory) and may fail it. Production code leaves it nil.
var syncHook func(path string, isDir bool) error

// Open opens the bucket at a gocloud URL: file:///abs/dir, mem://, s3://...,
// gs://..., azblob://... (drivers registered as in internal/backup/sink.go).
// As in internal/backup/sink.go, a scheme may carry a prefix before a "+" (the
// part after the last "+" selects the driver), and S3 endpoints default to
// request_checksum_calculation=when_required.
//
// Put is create-only: it writes with blob.WriterOptions{IfNotExist: true} and
// treats gcerrors.FailedPrecondition (the key exists) as success, because
// keys are content-addressed. Writes go to the final key only once the whole
// content was written (gocloud writers commit on Close), so a reader never
// sees a partial object. Get returns corestore.ErrBlobNotFound for
// gcerrors.NotFound, and for pack keys a reader that hashes the content and
// returns corestore.ErrBlobCorrupt instead of io.EOF on mismatch.
func Open(ctx context.Context, bucketURL string) (*Store, error) {
	u, err := url.Parse(bucketURL)
	if err != nil {
		return nil, fmt.Errorf("blobstore: parse bucket URL: %w", err)
	}

	scheme := u.Scheme
	if i := strings.LastIndex(scheme, "+"); i > 0 {
		scheme = scheme[i+1:]
	}
	u.Scheme = scheme

	var bucket *blob.Bucket
	var root string
	if scheme == fileblob.Scheme {
		if u.Path == "" {
			return nil, errors.New("blobstore: file bucket URL without a path")
		}
		root = filepath.Clean(u.Path)
		// fileblob creates missing directories with mode 0777, so the root
		// directory is created here with 0700 (see internal/backup/sink.go).
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, fmt.Errorf("blobstore: create bucket directory: %w", err)
		}
		bucket, err = fileblob.OpenBucket(root, &fileblob.Options{
			NoTempDir: true,
			Metadata:  fileblob.MetadataDontWrite,
		})
	} else {
		if scheme == s3blob.Scheme {
			bucketURL = withS3DefaultChecksumCalculation(u)
		} else {
			bucketURL = u.String()
		}
		bucket, err = blob.OpenBucket(ctx, bucketURL)
	}
	if err != nil {
		return nil, fmt.Errorf("blobstore: open bucket: %w", err)
	}

	return &Store{bucket: bucket, root: root}, nil
}

// withS3DefaultChecksumCalculation adds request_checksum_calculation=when_required
// to the S3 URL if a custom endpoint is configured and the parameter is not
// already specified. This keeps third-party S3 providers working that do not
// support the AWS SDK checksums. It mirrors internal/backup/sink.go.
func withS3DefaultChecksumCalculation(parsed *url.URL) string {
	query := parsed.Query()
	hasCustomEndpoint := query.Get("endpoint") != ""
	checksumNotSpecified := query.Get("request_checksum_calculation") == ""

	if hasCustomEndpoint && checksumNotSpecified {
		query.Set("request_checksum_calculation", "when_required")
		parsed.RawQuery = query.Encode()
	}
	return parsed.String()
}

// Put implements corestore.BlobStore.
func (s *Store) Put(ctx context.Context, key string, r io.Reader) error {
	writeCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	opts := &blob.WriterOptions{IfNotExist: true}
	var file *os.File
	if s.root != "" {
		opts.ContentType = fileContentType
		opts.BeforeWrite = func(asFunc func(any) bool) error {
			var f *os.File
			if !asFunc(&f) {
				return errors.New("blobstore: file bucket writer does not expose *os.File")
			}
			file = f
			return nil
		}
	}

	w, err := s.bucket.NewWriter(writeCtx, key, opts)
	if err != nil {
		return fmt.Errorf("blobstore: new writer %q: %w", key, err)
	}

	// abort discards the object: a writer whose context is cancelled before
	// Close commits nothing.
	abort := func(err error) error {
		cancel()
		_ = w.Close()
		return err
	}

	_, want, isPack := corestore.ParsePackKey(key)
	var dst io.Writer = w
	var hasher hash.Hash
	if isPack {
		hasher = sha256.New()
		dst = io.MultiWriter(w, hasher)
	}

	if _, err := io.Copy(dst, r); err != nil {
		return abort(fmt.Errorf("blobstore: write %q: %w", key, err))
	}

	if isPack && !bytes.Equal(hasher.Sum(nil), want[:]) {
		return abort(fmt.Errorf("blobstore: put %q: %w", key, corestore.ErrBlobCorrupt))
	}

	if file != nil {
		// The link below publishes the file. The content must reach the disk
		// first: a database row may reference this object after a power loss.
		if err := syncFile(file); err != nil {
			return abort(fmt.Errorf("blobstore: sync %q: %w", key, err))
		}
	}

	if file != nil {
		// fileblob's own Close is not a create-only publish (see the package doc):
		// the content is published by a link, and the writer is then discarded
		// (abort), which only removes the temporary name.
		if err := publishFile(file.Name()); err != nil {
			return abort(fmt.Errorf("blobstore: put %q: %w", key, err))
		}
		_ = abort(nil)

		// The new name is durable only once its directory is synced.
		if err := s.syncDirs(filepath.Dir(file.Name())); err != nil {
			return fmt.Errorf("blobstore: sync directories of %q: %w", key, err)
		}
		return nil
	}

	closeErr := w.Close()
	if gcerrors.Code(closeErr) == gcerrors.FailedPrecondition {
		// The writer's Close discarded its content: the stored object is untouched.
		return fmt.Errorf("blobstore: put %q: %w", key, corestore.ErrBlobExists)
	}
	if closeErr != nil {
		return fmt.Errorf("blobstore: close writer %q: %w", key, closeErr)
	}
	return nil
}

// publishFile gives tmp, the temporary file of a fileblob writer, the name of
// its key, create-only and atomically: link(2) fails with EEXIST when the name
// exists, for every writer of the filesystem, other hosts of a network
// filesystem included. A network filesystem can also report EEXIST for a link
// that succeeded but whose reply was lost and retransmitted: when the name is
// then the same file as tmp, the link was this writer's and the publish
// succeeded. A conflict is corestore.ErrBlobExists.
func publishFile(tmp string) error {
	final, ok := finalPath(tmp)
	if !ok {
		return fmt.Errorf("unexpected temporary file name %q", tmp)
	}

	err := os.Link(tmp, final)
	if err == nil {
		return nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("link %q (file buckets need a filesystem with hard links): %w", final, err)
	}

	tmpInfo, statErr := os.Stat(tmp)
	if statErr != nil {
		return fmt.Errorf("stat %q after a link conflict: %w", tmp, statErr)
	}
	finalInfo, statErr := os.Stat(final)
	if statErr != nil {
		return fmt.Errorf("stat %q after a link conflict: %w", final, statErr)
	}
	if os.SameFile(tmpInfo, finalInfo) {
		return nil
	}
	return corestore.ErrBlobExists
}

// finalPath returns the path of the object whose temporary file fileblob named
// tmp: "<path>.<hex nanoseconds>.tmp" in the object's directory (file buckets
// are opened with NoTempDir).
func finalPath(tmp string) (string, bool) {
	rest, ok := strings.CutSuffix(tmp, ".tmp")
	if !ok {
		return "", false
	}
	i := strings.LastIndex(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return "", false
	}
	if _, err := strconv.ParseUint(rest[i+1:], 16, 64); err != nil {
		return "", false
	}
	return rest[:i], true
}

// syncFile fsyncs the temporary file of a file bucket writer.
func syncFile(f *os.File) error {
	if syncHook != nil {
		if err := syncHook(f.Name(), false); err != nil {
			return err
		}
	}
	return f.Sync()
}

// syncDirs fsyncs dir and, when dir is inside the bucket root, each of its
// ancestors: the directories that the driver may have created, and the entry
// that the link created. The walk stops at the root, or after the first
// ancestor whose entries up to the root an earlier walk made durable. The
// directories it visited are remembered only when it got there without an
// error, so that a concurrent Put never stops at a directory whose own entry is
// not durable yet.
func (s *Store) syncDirs(dir string) error {
	inside := dir == s.root || strings.HasPrefix(dir, s.root+string(filepath.Separator))
	visited := map[string]os.FileInfo{}
	for d := dir; ; d = filepath.Dir(d) {
		if err := syncDir(d); err != nil {
			return err
		}
		if !inside || d == s.root {
			break
		}

		info, err := os.Stat(d)
		if err != nil {
			return fmt.Errorf("stat directory: %w", err)
		}
		if known, ok := s.durableDirs.Load(d); ok && os.SameFile(known.(os.FileInfo), info) {
			break
		}
		visited[d] = info
	}

	for d, info := range visited {
		s.durableDirs.Store(d, info)
	}
	return nil
}

// syncDir fsyncs a directory so that the entries created in it are durable.
func syncDir(dir string) error {
	if syncHook != nil {
		if err := syncHook(dir, true); err != nil {
			return err
		}
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("sync directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close directory: %w", closeErr)
	}
	return nil
}

// Get implements corestore.BlobStore.
func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	rc, err := s.bucket.NewReader(ctx, key, nil)
	if err != nil {
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil, corestore.ErrBlobNotFound
		}
		return nil, fmt.Errorf("blobstore: get %q: %w", key, err)
	}

	_, want, isPack := corestore.ParsePackKey(key)
	if !isPack {
		return rc, nil
	}

	return &verifyingReader{rc: rc, want: want, hasher: sha256.New()}, nil
}

// Exists implements corestore.BlobStore.
func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	exists, err := s.bucket.Exists(ctx, key)
	if err != nil {
		return false, fmt.Errorf("blobstore: exists %q: %w", key, err)
	}
	return exists, nil
}

// List implements corestore.BlobStore. An object deleted while the listing
// runs is either listed or not, never an error: garbage collection deletes
// objects while other runs list them.
func (s *Store) List(ctx context.Context, prefix string, fn func(corestore.BlobInfo) error) error {
	if s.root != "" {
		return s.listFiles(ctx, prefix, fn)
	}

	it := s.bucket.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("blobstore: list %q: %w", prefix, err)
		}
		if obj.IsDir {
			continue
		}
		if err := fn(corestore.BlobInfo{Key: obj.Key, Size: obj.Size}); err != nil {
			return err
		}
	}
}

// listFiles lists a file bucket by walking its directory. fileblob's own
// listing reads a directory and then stats each entry, and fails with NotFound
// when an entry was deleted in between; here such an entry is an object that is
// gone and is not listed. Keys are the slash-separated paths below the root
// (the storage core writes no key that fileblob would escape); directories are
// not objects.
func (s *Store) listFiles(ctx context.Context, prefix string, fn func(corestore.BlobInfo) error) error {
	start := s.root
	if i := strings.LastIndex(prefix, "/"); i >= 0 {
		start = filepath.Join(s.root, filepath.FromSlash(prefix[:i]))
	}

	// The error of fn is returned as it is (the BlobStore contract).
	var fnErr error
	err := filepath.WalkDir(start, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// The directory, or the entry, is gone: nothing to list there.
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(s.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if !strings.HasPrefix(key, prefix) {
			return nil
		}

		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := fn(corestore.BlobInfo{Key: key, Size: info.Size()}); err != nil {
			fnErr = err
			return fs.SkipAll
		}
		return nil
	})
	if fnErr != nil {
		return fnErr
	}
	if err != nil {
		return fmt.Errorf("blobstore: list %q: %w", prefix, err)
	}
	return nil
}

// Delete implements corestore.BlobStore.
func (s *Store) Delete(ctx context.Context, key string) error {
	if err := s.bucket.Delete(ctx, key); err != nil && gcerrors.Code(err) != gcerrors.NotFound {
		return fmt.Errorf("blobstore: delete %q: %w", key, err)
	}
	return nil
}

// Close implements corestore.BlobStore.
func (s *Store) Close() error {
	if err := s.bucket.Close(); err != nil {
		return fmt.Errorf("blobstore: close bucket: %w", err)
	}
	return nil
}

// verifyingReader hashes the content of a pack and reports a mismatch with its
// key, instead of io.EOF, when the content ends.
type verifyingReader struct {
	rc     io.ReadCloser
	want   [sha256.Size]byte
	hasher hash.Hash
}

// Read implements io.Reader.
func (r *verifyingReader) Read(p []byte) (int, error) {
	n, err := r.rc.Read(p)
	// hash.Hash.Write never returns an error.
	_, _ = r.hasher.Write(p[:n])

	switch {
	case err == nil:
		return n, nil
	case err == io.EOF:
		if !bytes.Equal(r.hasher.Sum(nil), r.want[:]) {
			return n, corestore.ErrBlobCorrupt
		}
		return n, io.EOF
	case errors.Is(err, io.ErrUnexpectedEOF):
		return n, corestore.ErrBlobCorrupt
	default:
		return n, err
	}
}

// Close implements io.Closer.
func (r *verifyingReader) Close() error {
	return r.rc.Close()
}
