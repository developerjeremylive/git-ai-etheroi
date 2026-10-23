package quarantine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/log"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/tempdir"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"google.golang.org/protobuf/proto"
)

// untrustedMigrationsTotal counts the migrations that fsynced the files of their quarantine
// because a writer that Gitaly does not control ran with it.
var untrustedMigrationsTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "graft_quarantine_untrusted_migrations_total",
	Help: "Quarantine migrations that fsynced the files of the quarantine because a custom hook ran with it.",
})

// Dir is a quarantine directory for Git objects. Instead of writing new commits into the main
// repository, they're instead written into a temporary quarantine directory. This staging area can
// either be migrated into the main repository or, alternatively, will automatically be discarded
// when the context gets cancelled. If the quarantine environment is discarded without being staged,
// then none of the objects which have been created in the quarantine directory will end up in the
// main repository.
//
// Durability contract: every file in a quarantine is durable when Migrate starts, its data and its
// inode. The Gitaly writers make it so: they are git processes that the command factory starts with
// core.fsync covering objects, or helpers that write without fsync and pack their objects into one
// fsynced pack before the update (see localrepo.WithoutObjectFsync). Migrate then only makes the
// new names durable, with one fsync per directory. A writer that Gitaly does not control, a custom
// hook, is reported with MarkUntrustedWriter, and Migrate then fsyncs the files of that quarantine
// itself.
type Dir struct {
	repo            *gitalypb.Repository
	quarantinedRepo *gitalypb.Repository
	dir             tempdir.Dir
	locator         storage.Locator
	// untrustedWriter is set when a writer that Gitaly does not control may have written into
	// the quarantine, see MarkUntrustedWriter.
	untrustedWriter atomic.Bool
}

// quarantines holds the quarantines of this process by the absolute path of their object
// directory, so that MarkUntrustedWriter finds the quarantine that a hook was given.
var quarantines sync.Map

// MarkUntrustedWriter records that a writer that Gitaly does not control, such as a custom hook, ran
// with objectDir as its object directory. When objectDir is a quarantine of this process, its
// files are not trusted to be durable, and its migration fsyncs them first.
func MarkUntrustedWriter(objectDir string) {
	if d, ok := quarantines.Load(filepath.Clean(objectDir)); ok {
		d.(*Dir).untrustedWriter.Store(true)
	}
}

// New creates a new quarantine directory and returns the directory and a cleanup function.
// The cleanup function must be called to remove the quarantine directory.
func New(ctx context.Context, repo *gitalypb.Repository, logger log.Logger, locator storage.Locator) (*Dir, func(), error) {
	repoPath, err := locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return nil, nil, structerr.NewInternal("getting repo path: %w", err)
	}

	quarantineDir, cleanup, err := tempdir.NewWithPrefix(ctx, repo.GetStorageName(),
		storage.QuarantineDirectoryPrefix(repo), logger, locator)
	if err != nil {
		return nil, nil, fmt.Errorf("creating quarantine: %w", err)
	}

	quarantinedRepo, err := Apply(repoPath, repo, quarantineDir.Path())
	if err != nil {
		cleanup() // Clean up if we fail after creating the temp directory
		return nil, nil, err
	}

	d := &Dir{
		repo:            repo,
		quarantinedRepo: quarantinedRepo,
		locator:         locator,
		dir:             quarantineDir,
	}

	key := filepath.Clean(quarantineDir.Path())
	quarantines.Store(key, d)

	return d, func() {
		quarantines.CompareAndDelete(key, d)
		cleanup()
	}, nil
}

// Apply applies the quarantine on the repository. This is done by setting the quarantineDirectory
// as the repository's object directory, and configuring the repository's object directory as an alternate.
func Apply(repoPath string, repo *gitalypb.Repository, quarantineDir string) (*gitalypb.Repository, error) {
	relativePath, err := filepath.Rel(repoPath, quarantineDir)
	if err != nil {
		return nil, fmt.Errorf("creating quarantine: %w", err)
	}

	// All paths are relative to the repository root.
	objectDir := repo.GetGitObjectDirectory()
	if objectDir == "" {
		// Set the default object directory as an alternate if the repository didn't
		// have the object directory overwritten yet.
		objectDir = "objects"
	}

	alternateObjectDirs := make([]string, 0, len(repo.GetGitAlternateObjectDirectories())+1)
	alternateObjectDirs = append(alternateObjectDirs, objectDir)
	alternateObjectDirs = append(alternateObjectDirs, repo.GetGitAlternateObjectDirectories()...)

	quarantinedRepo := proto.Clone(repo).(*gitalypb.Repository)
	quarantinedRepo.GitObjectDirectory = relativePath
	quarantinedRepo.GitAlternateObjectDirectories = alternateObjectDirs

	return quarantinedRepo, nil
}

// QuarantinedRepo returns a Repository protobuf message with adjusted main and alternate object
// directories. If passed e.g. to the `git.ExecCommandFactory`, then all new objects will end up in
// the quarantine directory.
func (d *Dir) QuarantinedRepo() *gitalypb.Repository {
	return d.quarantinedRepo
}

// Migrate migrates all objects part of the quarantine directory into the repository and thus makes
// them generally available. This implementation follows the git.git's `tmp_objdir_migrate()`.
func (d *Dir) Migrate(ctx context.Context) error {
	repoPath, err := d.locator.GetRepoPath(ctx, d.repo, storage.WithRepositoryVerificationSkipped())
	if err != nil {
		return fmt.Errorf("migrating quarantine: %w", err)
	}

	objectDir := d.repo.GetGitObjectDirectory()
	if objectDir == "" {
		// Migrate the objects to the default object directory if the repository
		// didn't have an object directory explicitly configured.
		objectDir = "objects"
	}

	return migrate(ctx, d.dir.Path(), filepath.Join(repoPath, objectDir), d.untrustedWriter.Load())
}

// migrate moves the objects of the quarantine at sourcePath into the object directory at
// targetPath, in two passes.
//
// The first pass links every file into the target directories, in the order of sortEntries, so
// that a pack's index appears after its data. An empty fan-out directory of the target may be
// removed at any time: git's housekeeping (prune-packed, repack -d, prune) removes empty fan-out
// directories, and git's own writers create their temporary file inside the fan-out directory,
// which pins it. A quarantine is a separate directory, so a target directory that migrate created
// or found can vanish before its first link. A link that fails because its target directory is
// gone creates the directory again and links again; once the link succeeds the directory is not
// empty and stays. Each round needs another removal between the creation and the link, so the loop
// ends; it also ends with the context.
//
// The descriptor of a target directory is taken right after a link into it succeeded, never
// before: the link pins the directory, so the descriptor is of the directory that holds the link.
// A descriptor taken earlier could be of a directory that housekeeping removes, and that another
// writer then creates again, before the link (the link then succeeds into the new directory, which
// the second pass would not fsync). If the directory is gone even after the link, housekeeping
// removed the linked object because a pack holds it (git prune-packed), and nothing of the
// migration is left there to make durable.
//
// The second pass, unless a transaction manager owns durability (storage.NeedsSync), fsyncs each
// target directory that received a link, once, through those descriptors, and the object
// directory itself. The files are not fsynced: they are durable before the migration
// (see Dir), unless untrustedWriter is set, in which case they are fsynced in parallel before the
// first pass.
//
// Filesystem assumption (local ext4 in any data mode, or xfs; not NFS): a new hard link is
// durable once its directory is fsynced; a link is one journaled operation that changes the
// directory entry and the link count together; the journal commits its transactions in order, and
// the fsync of a directory forces it up to the directory's last change. So after the second pass
// every link is durable, and a crash before leaves a prefix of the links in their order.
func migrate(ctx context.Context, sourcePath, targetPath string, untrustedWriter bool) error {
	sync := storage.NeedsSync(ctx)

	if sync && untrustedWriter {
		untrustedMigrationsTotal.Inc()
		if err := syncFilesInParallel(ctx, sourcePath); err != nil {
			return fmt.Errorf("syncing the files of an untrusted writer: %w", err)
		}
	}

	m := migration{ctx: ctx, dirs: map[string]*os.File{}}
	defer m.closeDirs()

	// The object directory is not a fan-out directory, so housekeeping never removes it. Its
	// descriptor is taken first, so the second pass fsyncs it last.
	if err := ensureDir(targetPath); err != nil {
		return err
	}
	if err := m.track(targetPath); err != nil {
		return err
	}
	if _, ok := m.dirs[targetPath]; !ok {
		return fmt.Errorf("opening target directory %q: %w", targetPath, fs.ErrNotExist)
	}
	if err := m.link(sourcePath, targetPath); err != nil {
		return err
	}

	if !sync {
		return nil
	}

	// The object directory last: it holds the entries of the fan-out directories that the first
	// pass created.
	for i := len(m.order) - 1; i >= 0; i-- {
		if err := syncDirFile(m.dirs[m.order[i]]); err != nil {
			return fmt.Errorf("sync directory %q: %w", m.order[i], err)
		}
	}

	return nil
}

// migration is the state of the first pass of migrate.
type migration struct {
	ctx context.Context
	// dirs holds a descriptor of each target directory, by path.
	dirs map[string]*os.File
	// order holds the target directories in the order they were opened, parents first.
	order []string
}

// ensureDir creates the target directory dir. An existing entry at dir is accepted as it is, as
// before: a file there (a conflict with the quarantine's directory) is left to the links into it
// to report.
func ensureDir(dir string) error {
	if err := os.Mkdir(dir, mode.Directory); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating target directory %q: %w", dir, err)
	}
	return nil
}

// track keeps a descriptor of the target directory dir for the second pass. It runs after a link
// into dir succeeded (see migrate). It keeps the descriptor it has when that is still of the
// directory at dir, and replaces it otherwise. A dir that no longer exists holds nothing of the
// migration (housekeeping removed the linked object because a pack holds it), so it is skipped.
func (m *migration) track(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("checking target directory %q: %w", dir, err)
	}

	old, known := m.dirs[dir]
	if known {
		if oldInfo, err := old.Stat(); err == nil && os.SameFile(oldInfo, info) {
			return nil
		}
	}

	f, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opening target directory %q: %w", dir, err)
	}

	if known {
		_ = old.Close()
	} else {
		m.order = append(m.order, dir)
	}
	m.dirs[dir] = f

	return nil
}

func (m *migration) closeDirs() {
	for _, f := range m.dirs {
		_ = f.Close()
	}
}

// link links the files of sourcePath into targetPath, recursively, and removes sourcePath.
func (m *migration) link(sourcePath, targetPath string) error {
	entries, err := os.ReadDir(sourcePath)
	if err != nil {
		return fmt.Errorf("reading directory: %w", err)
	}
	sortEntries(entries)

	for _, entry := range entries {
		nestedTargetPath := filepath.Join(targetPath, entry.Name())
		nestedSourcePath := filepath.Join(sourcePath, entry.Name())

		if entry.IsDir() {
			// The directory structure is migrated as it is, empty directories included. Its
			// descriptor is taken only after a link into it (linkFile), which also creates it
			// again if housekeeping removed it in between.
			if err := ensureDir(nestedTargetPath); err != nil {
				return err
			}
			if err := m.link(nestedSourcePath, nestedTargetPath); err != nil {
				return fmt.Errorf("migrating directory %q: %w", nestedSourcePath, err)
			}
			continue
		}

		if err := m.linkFile(nestedSourcePath, nestedTargetPath); err != nil {
			return fmt.Errorf("migrating object file %q: %w", nestedSourcePath, err)
		}
	}

	if err := os.Remove(sourcePath); err != nil {
		return fmt.Errorf("removing source directory: %w", err)
	}

	return nil
}

// linkFile links the file at sourcePath to targetPath, creating the target directory again when
// it was removed (see migrate).
func (m *migration) linkFile(sourcePath, targetPath string) error {
	dir := filepath.Dir(targetPath)
	if err := ensureDir(dir); err != nil {
		return err
	}

	if err := storage.LinkRecreatingDirectory(m.ctx, sourcePath, targetPath, func() error {
		beforeLink(targetPath)
		return finalizeObjectFile(sourcePath, targetPath)
	}); err != nil {
		return err
	}

	return m.track(dir)
}

// beforeLink runs before each link of the first pass of migrate. Tests replace it to remove a
// target directory at that point, as housekeeping may.
var beforeLink = func(targetPath string) {}

// syncDirFile fsyncs the descriptor of a target directory. Tests replace it to count the fsyncs.
var syncDirFile = func(f *os.File) error {
	return f.Sync()
}

// syncFile fsyncs the file at path. Tests replace it to count the fsyncs.
var syncFile = func(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	return f.Sync()
}

// maxParallelSyncs bounds the fsyncs that syncFilesInParallel runs at once.
const maxParallelSyncs = 16

// syncFilesInParallel fsyncs every file under dir. Concurrent fsyncs of the files of one
// filesystem share journal commits, which one fsync after another does not.
func syncFilesInParallel(ctx context.Context, dir string) error {
	var files []string
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			files = append(files, path)
		}
		return nil
	}); err != nil {
		return err
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	slots := make(chan struct{}, maxParallelSyncs)
	for _, path := range files {
		if err := ctx.Err(); err != nil {
			wg.Wait()
			return err
		}

		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()

			if err := syncFile(path); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("sync %q: %w", path, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	return firstErr
}

// finalizeObjectFile will move the object file (either a packfile, its metadata or or a loose
// object) to the target path. The move is either done with a hard link if supported or with a
// rename. No error is raised in case the target path exists already.
func finalizeObjectFile(sourcePath, targetPath string) error {
	// We first try to link the file via a hardlink. The benefit compared to doing a rename is
	// that in case of a collision, we do not replace the target.
	err := os.Link(sourcePath, targetPath)

	// In case the hardlink failed, we fall back to a rename.
	renamed := false
	if err != nil && !errors.Is(err, os.ErrExist) {
		err = os.Rename(sourcePath, targetPath)
		renamed = err == nil
	}

	if err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("finalizing object file: %w", err)
	}

	if !renamed {
		// It's fair to ignore the error here: we'll purge the quarantine directory anyway
		// in case the context gets cancelled.
		_ = os.Remove(sourcePath)
	}

	return nil
}

// sortEntries sorts packfiles and their associated metafiles such that we copy them over in the
// correct order.
func sortEntries(entries []os.DirEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return packCopyPriority(entries[i].Name()) < packCopyPriority(entries[j].Name())
	})
}

func packCopyPriority(name string) int {
	switch {
	case !strings.HasPrefix(name, "pack"):
		return 0
	case strings.HasSuffix(name, ".keep"):
		return 1
	case strings.HasSuffix(name, ".pack"):
		return 2
	case strings.HasSuffix(name, ".rev"):
		return 3
	case strings.HasSuffix(name, ".idx"):
		return 4
	default:
		return 5
	}
}
