package localrefs

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// MarkerDirName is the directory of the dirty markers in a storage, under
// Gitaly's data directory. It is not the state directory, whose files the disk
// cache walker removes after an hour.
const MarkerDirName = config.GitalyDataPrefix + "/graft-ref-durability"

// ownerLockName is the file in MarkerDirName whose exclusive lock makes a
// process the owner of the storage's deferred durability.
const ownerLockName = "owner.lock"

// markerTempPrefix prefixes the temporary files of marker writes.
const markerTempPrefix = ".marker-"

// stepTempPrefix prefixes the temporary files the durable step links its
// reference lock from, in the repository directory.
const stepTempPrefix = ".graft-durable-step-"

// bootIDPath is where Linux exposes the identity of the running kernel boot.
const bootIDPath = "/proc/sys/kernel/random/boot_id"

const (
	// DurableStepInterval is how often the durable step runs for every dirty
	// repository: it bounds how long a write failure the kernel reports goes
	// unnoticed. A process that does not own the deferred durability tries to
	// take it as often.
	DurableStepInterval = time.Second
	// DurableStepIdle is how long a dirty repository must be without a reference
	// write before the durable step removes its marker.
	DurableStepIdle = time.Second
	// RecoveryGrace is how long a repository found dirty when the process takes
	// the ownership keeps its marker at least. The rule that decides is that no
	// git lock or temporary file is in its reftable directory (gitActive); the
	// grace only covers the instant between a commit's rename and the start of
	// its auto-compaction, when a git process has neither.
	RecoveryGrace = 2 * time.Second
)

// WithRecoveryGrace replaces RecoveryGrace. Tests shorten it.
func WithRecoveryGrace(grace time.Duration) DurabilityOption {
	return func(d *Durability) { d.recoveryGrace = grace }
}

// CrashEnv names the test-only environment variable that makes the process
// exit (exit code 75) in the durable step, while it holds git's reference lock
// after making the stack durable and before it removes the marker: "durable_step".
// The crash tests with real binaries use it. It is read once by NewDurability.
const CrashEnv = "GRAFT_STORAGE_TEST_CRASH"

const (
	crashDurableStep = "durable_step"
	crashExitCode    = 75
)

// staleGitFileAge is the age past which a git lock or temporary file of a
// reftable stack belongs to a git process that died: Gitaly's housekeeping
// removes such a lock (housekeeping.ReftableLockfileGracePeriod, the same
// value), and no git process holds a stack's lock for that long. Such a file is
// not git activity, and the durable step removes a stale lock of the list as
// housekeeping would, so that it never keeps a marker forever.
const staleGitFileAge = time.Hour

// stackLockTag starts the content of the reference lock the durable step
// takes, followed by a random nonce of that lock. git never writes it (a lock
// of git holds table names), so a lock with this tag found by a new owner was
// left by a durable step that died, and a step removes a lock only when it
// still holds its own nonce.
const stackLockTag = "graft durable step "

// ErrUntrusted is returned by Settle when the kernel reported that making the
// references of a repository durable failed: it may have dropped some of their
// writes, so the local repository must not be served before it is rebuilt.
var ErrUntrusted = errors.New("storage core: the local references may have lost writes")

// errReboot makes a repository untrusted whose marker is of another kernel
// boot: the kernel restarted while its references had writes that were not
// durable, so its stack may be torn.
var errReboot = errors.New("the kernel restarted while the references had writes that were not durable")

// UntrustedError is ErrUntrusted for one repository.
type UntrustedError struct {
	Repo RepoRef
	Path string
	Err  error
}

func (e *UntrustedError) Error() string {
	return fmt.Sprintf("%v: %s: %v", ErrUntrusted, e.Path, e.Err)
}

// Unwrap returns ErrUntrusted and the cause.
func (e *UntrustedError) Unwrap() []error { return []error{ErrUntrusted, e.Err} }

// writebackError is an fsync that failed: the kernel reports a write it could
// not make durable. Every other error of the durable step (opening a file,
// reading the table list) is transient and retried at the next step.
type writebackError struct{ err error }

func (e writebackError) Error() string { return "fsync: " + e.err.Error() }
func (e writebackError) Unwrap() error { return e.err }

// Durability defers the durability of the local reference writes of the
// storage core in authoritative mode (doc/graft/PLAN7.md, LOCAL REFERENCE
// WRITES RULE). The database commit of a transaction is its durable commit
// point and the local repository is a cache of it, so git writes the references
// without fsync and Durability keeps the cache safe to serve:
//
//   - one process per storage owns the deferred durability (an exclusive lock
//     held for the life of the process); only the owner writes references
//     without fsync, runs the durable steps and touches the durable step's
//     locks. Another process on the same storage (a graceful upgrade overlaps
//     the old and the new process) writes them with fsync until it owns;
//   - before the first reference write that is not durable, the repository
//     gets a dirty marker, written durably, that holds the identity of the
//     kernel boot (MarkDirty);
//   - a durable step makes the references durable under git's reference lock
//     (Settle); one function removes markers, every path through it, and only
//     when nothing can leave a write that is not durable in the stack any more
//     (dropMarker);
//   - a repository whose writes may be lost (a failed fsync, a marker of
//     another boot) is untrusted: never served, its marker never removed, until
//     it is moved aside (Forget);
//   - when it takes the ownership, a marker of another boot means the kernel
//     restarted while the repository had writes that may never have reached the
//     disk: its reftable stack may be torn, which git reads as an empty
//     repository, so the repository is moved aside and hydrated again
//     (Recover). A marker of the running boot is a process restart: the kernel
//     kept every write.
//
// A nil *Durability is off: every reference write is durable by itself
// (core.fsync with references), as outside authoritative mode.
type Durability struct {
	bootID  string
	locator storage.Locator
	// idle is how long a dirty repository must be without a reference write
	// before Settle removes its marker.
	idle time.Duration
	// sync makes a file or directory durable; tests replace it.
	sync func(f *os.File) error
	// crashAt is the crash point selected with CrashEnv.
	crashAt string
	// watermark returns the path of the watermark of the repository at repoPath;
	// nil unless the syncer may write it without fsync (WithWatermark).
	watermark func(repoPath string) string

	// ownerFiles hold the owner locks, taken by TryOwn. owner is set by
	// Activate once the process has recovered the storages: from then on its
	// writes are deferred. Neither goes back.
	owner      atomic.Bool
	ownerMu    sync.Mutex
	ownerFiles []*os.File
	locked     bool
	// recoveryGrace is how long a repository recovered dirty keeps its marker
	// at least, and how long after git activity was last seen in a stack: a git
	// process of the previous owner that outlived it (killed with it, but not
	// reaped yet) may still be committing for that long.
	recoveryGrace time.Duration

	mu sync.Mutex
	// repos holds the state of every repository that is dirty or that a live
	// git process counts on (one entry per repository; dropMarker drops it).
	repos map[string]*refState

	marked     prometheus.Counter
	cleared    prometheus.Counter
	untrusted  *prometheus.CounterVec
	stepErrors prometheus.Counter
	dirty      prometheus.Gauge
	inflight   prometheus.Gauge
	owning     prometheus.Gauge
}

var _ prometheus.Collector = (*Durability)(nil)

// refState is what Durability knows of one local repository, by its path. Its
// fields are guarded by Durability.mu, except marker writes and removals,
// which file serializes. A state leaves the map only in dropMarker, under its
// file lock, so whoever holds the file lock of the map's state keeps it current
// (lockState).
type refState struct {
	// file serializes the writes and the removal of the marker file.
	file   sync.Mutex
	repo   RepoRef
	marker string
	// dirty is set while the marker may be on disk: from before it is written
	// until dropMarker removed it.
	dirty bool
	// markerPlain is set when the marker on disk is a plain marker of the
	// running boot: neither one that records untrusted nor one of another boot,
	// which would make a restart distrust what is at the path.
	markerPlain bool
	// nLive counts the git processes of this process that write the
	// repository's references without fsync and have not exited: any of them
	// may still write a table or compact the stack.
	nLive     int
	lastWrite time.Time
	// settleAfter is the earliest time the marker may go: the recovery grace of
	// a repository found dirty when the process took the ownership (a git
	// process of the previous owner that outlived it may still be committing),
	// pushed forward whenever git activity is seen in its stack.
	settleAfter time.Time
	// untrusted is the first failed fsync of the repository, or the restart of
	// the kernel that a marker of another boot shows. The kernel reports a
	// failed writeback once, so a later fsync can succeed although writes were
	// lost: the repository is never served and never becomes clean, it is only
	// moved aside (Forget). The marker records a failed fsync, so that a restart
	// of the process keeps it.
	untrusted error
	// synced holds the tables made durable during the current dirty period that
	// the stack still lists. Tables are immutable, so one fsync each is enough.
	synced map[string]struct{}
}

// RepoRef names a repository of a storage.
type RepoRef struct {
	Storage      string `json:"storage"`
	RelativePath string `json:"relative_path"`
}

func repoOf(r RepoRef) *gitalypb.Repository {
	return &gitalypb.Repository{StorageName: r.Storage, RelativePath: r.RelativePath}
}

// marker is the content of a dirty marker.
type marker struct {
	RepoRef
	BootID string `json:"boot_id"`
	// Untrusted is the failed fsync that made the repository untrusted, empty
	// while it is only dirty.
	Untrusted string `json:"untrusted,omitempty"`
}

// Reasons of graft_storage_ref_untrusted_total.
const (
	// ReasonReboot is a dirty repository found after a kernel restart.
	ReasonReboot = "reboot"
	// ReasonSyncFailed is a durable step whose fsync failed.
	ReasonSyncFailed = "sync_failed"
)

// DurabilityOption configures a Durability.
type DurabilityOption func(*Durability)

// WithBootID sets the identity of the kernel boot instead of reading it from
// the kernel. Tests use it to simulate a kernel restart.
func WithBootID(bootID string) DurabilityOption {
	return func(d *Durability) { d.bootID = bootID }
}

// WithSync replaces how a file or directory is made durable. Tests use it to
// simulate a write the kernel failed to do.
func WithSync(sync func(f *os.File) error) DurabilityOption {
	return func(d *Durability) { d.sync = sync }
}

// WithWatermark lets the syncer write the watermark of a dirty repository
// without fsync (DefersWatermark); path returns the watermark of a repository,
// which the durable step then makes durable before the marker goes.
func WithWatermark(path func(repoPath string) string) DurabilityOption {
	return func(d *Durability) { d.watermark = path }
}

// NewDurability returns the deferred durability of the node, or nil when the
// kernel does not expose its boot identity (not Linux) and no option sets one:
// the references are then written durably by every transaction.
func NewDurability(locator storage.Locator, idle time.Duration, opts ...DurabilityOption) (*Durability, error) {
	d := newDurability(locator, idle)
	for _, opt := range opts {
		opt(d)
	}
	if d.bootID != "" {
		return d, nil
	}

	content, err := os.ReadFile(bootIDPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the boot identity: %w", err)
	}
	d.bootID = strings.TrimSpace(string(content))
	if d.bootID == "" {
		return nil, fmt.Errorf("read the boot identity: %s is empty", bootIDPath)
	}
	return d, nil
}

func newDurability(locator storage.Locator, idle time.Duration) *Durability {
	return &Durability{
		locator:       locator,
		idle:          idle,
		sync:          (*os.File).Sync,
		crashAt:       os.Getenv(CrashEnv),
		recoveryGrace: RecoveryGrace,
		repos:         map[string]*refState{},
		marked: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_dirty_marked_total",
			Help: "Number of dirty markers written: a repository got local reference writes that are not durable yet.",
		}),
		cleared: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_dirty_cleared_total",
			Help: "Number of dirty markers removed: the repository's reference writes are durable, or the repository was moved aside, and no git process can still write it.",
		}),
		untrusted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_storage_ref_untrusted_total",
			Help: "Number of local repositories whose references could not be trusted and were moved aside to be hydrated again, by reason (reboot, sync_failed).",
		}, []string{"reason"}),
		stepErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "graft_storage_ref_durable_step_errors_total",
			Help: "Number of durable steps that failed with a transient error (not a failed fsync) and are retried at the next step.",
		}),
		dirty: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_dirty_repositories",
			Help: "Number of local repositories with reference writes that are not durable yet.",
		}),
		inflight: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_writes_inflight",
			Help: "Number of git processes of this process that write references without fsync and have not exited; their repositories keep their markers.",
		}),
		owning: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_storage_ref_durability_owner",
			Help: "1 when this process owns the deferred durability of its storages and writes references without fsync, 0 while another process owns it.",
		}),
	}
}

// Enabled reports whether the node defers the durability of its reference
// writes once it owns it.
func (d *Durability) Enabled() bool { return d != nil }

// Deferring reports whether this process owns the deferred durability, so its
// reference writes are made without fsync. It goes from false to true at most
// once.
func (d *Durability) Deferring() bool { return d != nil && d.owner.Load() }

// TryOwn takes the owner locks of the storages without waiting. It returns
// false when another process holds one of them; the locks taken are kept, and
// a later call takes the rest. Owning the locks lets the process recover the
// storages (Recover); its writes are deferred only after Activate, so that no
// writer of this process writes a marker that the recovery could take for one
// left behind.
func (d *Durability) TryOwn(ctx context.Context, storages []string) (bool, error) {
	if d == nil {
		return false, nil
	}

	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	if d.locked {
		return true, nil
	}

	for _, storageName := range storages[len(d.ownerFiles):] {
		dir, err := d.markerDir(ctx, storageName)
		if err != nil {
			return false, err
		}
		if err := d.ensureDir(dir); err != nil {
			return false, fmt.Errorf("create the dirty marker directory: %w", err)
		}
		f, err := os.OpenFile(filepath.Join(dir, ownerLockName), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			return false, fmt.Errorf("open the owner lock: %w", err)
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return false, nil
			}
			return false, fmt.Errorf("lock the owner lock: %w", err)
		}
		d.ownerFiles = append(d.ownerFiles, f)
	}

	d.locked = true
	return true, nil
}

// Activate makes the writes of the process deferred; it holds the owner locks
// (TryOwn) and has recovered the storages.
func (d *Durability) Activate() {
	if d == nil {
		return
	}

	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	if !d.locked {
		panic("localrefs: Activate without the owner locks")
	}
	d.owner.Store(true)
	d.owning.Set(1)
}

// ownsLocks reports whether the process holds the owner locks.
func (d *Durability) ownsLocks() bool {
	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	return d.locked
}

// Close releases the ownership. The process must not write references
// without fsync afterwards; only tests and the end of the process call it.
func (d *Durability) Close() error {
	if d == nil {
		return nil
	}

	d.ownerMu.Lock()
	defer d.ownerMu.Unlock()
	var errs []error
	for _, f := range d.ownerFiles {
		errs = append(errs, f.Close())
	}
	d.ownerFiles = nil
	d.locked = false
	return errors.Join(errs...)
}

// markerPath returns the path of the dirty marker of the repository at path.
func (d *Durability) markerPath(ctx context.Context, repo RepoRef, path string) (string, error) {
	dir, err := d.markerDir(ctx, repo.Storage)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(dir, hex.EncodeToString(sum[:])+".json"), nil
}

func (d *Durability) markerDir(ctx context.Context, storageName string) (string, error) {
	root, err := d.locator.GetStorageByName(ctx, storageName)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, MarkerDirName), nil
}

// stateLocked returns the state of the repository at path, creating it. The
// caller holds d.mu.
func (d *Durability) stateLocked(path string) *refState {
	st := d.repos[path]
	if st == nil {
		st = &refState{synced: map[string]struct{}{}}
		d.repos[path] = st
	}
	return st
}

// MarkDirty records that the repository at path is about to get reference
// writes that are not durable. When it returns nil the repository has a durable
// dirty marker; the writer must not write before. It is only called while the
// process owns the deferred durability (Deferring). The caller holds git's
// reference lock of the repository (the voter's Prepared phase) or the syncer's
// repository lock, under which Settle decides to clear the marker, and the
// marker file is only written or removed under the state's file lock, so a
// marker is never removed after a writer relied on it.
func (d *Durability) MarkDirty(ctx context.Context, repo RepoRef, path string) error {
	if d == nil {
		return nil
	}

	st := d.lockState(path)
	defer st.file.Unlock()
	d.mu.Lock()
	st.lastWrite = time.Now()
	dirty := st.dirty
	d.mu.Unlock()
	if dirty {
		return nil
	}

	markerPath, err := d.markerPath(ctx, repo, path)
	if err != nil {
		return err
	}
	content, err := json.Marshal(marker{RepoRef: repo, BootID: d.bootID})
	if err != nil {
		return fmt.Errorf("encode the dirty marker: %w", err)
	}
	if err := d.writeDurably(markerPath, content); err != nil {
		return fmt.Errorf("write the dirty marker: %w", err)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st.repo = repo
	st.marker = markerPath
	st.markerPlain = true
	d.marked.Inc()
	d.setDirtyLocked(st)
	return nil
}

// lockState returns the state of the repository at path, created when there is
// none, with its file lock held: the state stays the map's until the lock is
// released, because only dropMarker removes a state, under its file lock.
func (d *Durability) lockState(path string) *refState {
	for {
		d.mu.Lock()
		st := d.stateLocked(path)
		d.mu.Unlock()
		st.file.Lock()
		d.mu.Lock()
		current := d.repos[path] == st
		d.mu.Unlock()
		if current {
			return st
		}
		st.file.Unlock()
	}
}

// setDirtyLocked marks the state dirty. The caller holds d.mu.
func (d *Durability) setDirtyLocked(st *refState) {
	if !st.dirty {
		st.dirty = true
		d.dirty.Inc()
	}
}

// Begin records that a git process that writes the references of the
// repository at path without fsync is about to start; the caller calls end
// once the process has exited. Until then the process may write a table or
// compact the stack, whatever hooks git runs or skips (a failed prepare, a
// failed commit, a kill), so the durable step keeps the marker. end may be
// called more than once. A state with a live process never leaves the map
// (dropMarker), so end always counts down the state Begin counted up.
func (d *Durability) Begin(path string) (end func()) {
	if d == nil {
		return func() {}
	}

	d.mu.Lock()
	st := d.stateLocked(path)
	st.nLive++
	d.inflight.Inc()
	d.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			d.mu.Lock()
			defer d.mu.Unlock()
			st.nLive--
			d.inflight.Dec()
		})
	}
}

// Untrusted reports whether the local repository at path may have lost
// reference writes: it must not be served until it is moved aside and
// hydrated again.
func (d *Durability) Untrusted(path string) bool {
	if d == nil {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.repos[path]
	return st != nil && st.untrusted != nil
}

// Dirty returns the paths of the dirty repositories.
func (d *Durability) Dirty() []string {
	if d == nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	paths := make([]string, 0, len(d.repos))
	for path, st := range d.repos {
		if st.dirty {
			paths = append(paths, path)
		}
	}
	return paths
}

// DefersWatermark reports whether the watermark of the repository at path may be
// written without fsync now: the repository is dirty and trusted, so its marker
// is durable, and the marker goes only after a durable step made the watermark
// durable (syncWatermark). The caller holds the syncer's repository lock.
func (d *Durability) DefersWatermark(path string) bool {
	if !d.Deferring() || d.watermark == nil {
		return false
	}

	// A repository without a stack has no durable step of its references: its
	// watermark is written with fsync.
	if stack, err := hasStack(path); err != nil || !stack {
		return false
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.repos[path]
	return st != nil && st.dirty && st.untrusted == nil
}

// RepoOf returns the repository whose state is kept for path.
func (d *Durability) RepoOf(path string) (RepoRef, bool) {
	if d == nil {
		return RepoRef{}, false
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.repos[path]
	if st == nil || st.repo == (RepoRef{}) {
		return RepoRef{}, false
	}
	return st.repo, true
}

// CountStepError counts a durable step that failed with a transient error.
func (d *Durability) CountStepError() {
	if d == nil {
		return
	}
	d.stepErrors.Inc()
}

// Settle is the durable step of the repository at path; only the owner runs
// it. It makes the tables that the stack lists, the list and the directory
// durable. A failed fsync is an *UntrustedError (the caller moves the
// repository aside at once); any other error is transient. When the repository
// has been idle for the idle period, or final is set (the process stops), and
// no git process of the process is live, it then takes git's reference lock
// without waiting (a busy lock leaves the marker for the next step), makes the
// stack durable again under it, and lets dropMarker decide. A repository that
// no longer exists (moved aside as a stale cache) has no stack, and its marker
// goes by the same rule: it stays while a git process that names the path is
// live. A clean state with no live process leaves the map. The caller holds the
// syncer's repository lock, so no syncer write and no replacement of the
// repository runs meanwhile.
func (d *Durability) Settle(path string, final bool) error {
	if !d.Deferring() {
		return nil
	}

	d.mu.Lock()
	st := d.repos[path]
	if st == nil {
		d.mu.Unlock()
		return nil
	}
	repo := st.repo
	if st.untrusted != nil {
		// Sticky: the move aside is retried, the marker never cleared.
		untrusted := st.untrusted
		d.mu.Unlock()
		return &UntrustedError{Repo: repo, Path: path, Err: untrusted}
	}
	dirty := st.dirty
	due := st.nLive == 0 && (final || time.Since(st.lastWrite) >= d.idle)
	d.mu.Unlock()

	stack, err := hasStack(path)
	if err != nil {
		return err
	}
	if !dirty || !stack {
		// A repository without a stack (gone, or not reftable) has no references
		// to make durable; a clean state only leaves the map. A transaction that
		// marked a repository with a stack meanwhile leaves it to the next step,
		// under git's reference lock (dropMarker refuses otherwise).
		st := d.lockState(path)
		defer st.file.Unlock()
		if dirty {
			// Its watermark may have been written without fsync while it had a
			// stack.
			if err := d.syncWatermark(path); err != nil {
				return d.stepErrorLocked(st, repo, path, err)
			}
		}
		_, err := d.dropMarker(path, st, false)
		return err
	}

	if err := d.syncStack(path, st); err != nil {
		st := d.lockState(path)
		defer st.file.Unlock()
		return d.stepErrorLocked(st, repo, path, err)
	}
	if !due {
		return nil
	}

	// The watermark is made durable before git's reference lock is taken, so that
	// its fsync does not hold the transactions of the repository.
	if err := d.syncWatermark(path); err != nil {
		st := d.lockState(path)
		defer st.file.Unlock()
		return d.stepErrorLocked(st, repo, path, err)
	}
	lock, ok, err := lockStack(path)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	err = d.settleLocked(path, repo)
	if releaseErr := lock.release(); err == nil {
		err = releaseErr
	}
	return err
}

// settleLocked makes the stack durable under git's reference lock, which the
// caller holds (no transaction commits meanwhile), and removes the marker when
// dropMarker finds it safe. The marker file goes before git's lock is
// released, under the state's file lock, which a writer's MarkDirty waits for.
func (d *Durability) settleLocked(path string, repo RepoRef) error {
	st := d.lockState(path)
	defer st.file.Unlock()

	if err := d.syncStack(path, st); err != nil {
		return d.stepErrorLocked(st, repo, path, err)
	}
	if d.crashAt == crashDurableStep {
		os.Exit(crashExitCode)
	}
	_, err := d.dropMarker(path, st, true)
	return err
}

// dropMarker is the only code that removes a dirty marker or drops a state
// from the map. It removes the marker of the repository at path only when
// nothing can still leave a reference write that is not durable in its stack:
//
//   - no git process of this process that writes the references without fsync
//     is live (nLive);
//   - the repository is not untrusted;
//   - when the repository has a reftable stack, the caller holds git's
//     reference lock (ownLock) and made the stack durable under it
//     (settleLocked): no git process can write meanwhile, and the ones that
//     exited before are in what was made durable. A repository without one
//     (gone, or not reftable: hasStack) has nothing written without fsync, and
//     none is created meanwhile (the caller holds the syncer's repository lock,
//     so it is not hydrated meanwhile);
//   - no git process writes the stack now (gitActive: a git lock or a table
//     being written; seeing one also pushes settleAfter a recovery grace
//     forward), and settleAfter has passed.
//
// The caller holds the state's file lock, which MarkDirty waits for before it
// reads whether the repository is dirty. The checks and the state change are
// made under d.mu, which Begin takes. It reports whether the marker was
// removed. A clean state that no live git process counts on and that is
// trusted leaves the map, so that the states do not grow with every repository
// ever written.
func (d *Durability) dropMarker(path string, st *refState, ownLock bool) (bool, error) {
	stack, err := hasStack(path)
	if err != nil {
		return false, err
	}
	active, err := gitActive(path, ownLock)
	if err != nil {
		return false, err
	}

	d.mu.Lock()
	now := time.Now()
	if active {
		if until := now.Add(d.recoveryGrace); until.After(st.settleAfter) {
			st.settleAfter = until
		}
	}
	safe := st.dirty && st.nLive == 0 && st.untrusted == nil && (ownLock || !stack) && !active && !now.Before(st.settleAfter)
	if safe {
		st.dirty = false
		d.dirty.Dec()
		st.synced = map[string]struct{}{}
	}
	markerPath := st.marker
	d.mu.Unlock()

	if safe && markerPath != "" {
		if err := os.Remove(markerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			d.mu.Lock()
			d.setDirtyLocked(st)
			d.mu.Unlock()
			return false, fmt.Errorf("remove the dirty marker: %w", err)
		}
	}
	if safe {
		d.cleared.Inc()
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.repos[path] == st && !st.dirty && st.nLive == 0 && st.untrusted == nil {
		delete(d.repos, path)
	}
	return safe, nil
}

// stepErrorLocked is the error of a durable step: an *UntrustedError for a
// failed fsync, which makes the repository untrusted for good, the error
// itself otherwise. The first failure is written into the marker, so that the
// repository stays untrusted across a restart of the process (the next fsync
// would succeed); when even that write fails, the disk is failing and the
// state of this process keeps it untrusted. The caller holds the state's file
// lock.
func (d *Durability) stepErrorLocked(st *refState, repo RepoRef, path string, err error) error {
	var writeback writebackError
	if !errors.As(err, &writeback) {
		return err
	}

	d.mu.Lock()
	first := st.untrusted == nil
	if first {
		st.untrusted = err
	}
	markerPath := st.marker
	dirty := st.dirty
	d.mu.Unlock()

	if first && dirty && markerPath != "" {
		content, encodeErr := json.Marshal(marker{RepoRef: repo, BootID: d.bootID, Untrusted: err.Error()})
		if encodeErr == nil {
			encodeErr = d.writeDurably(markerPath, content)
		}
		if encodeErr != nil {
			return &UntrustedError{Repo: repo, Path: path, Err: fmt.Errorf("%w (recording it in the marker failed: %w)", err, encodeErr)}
		}
		d.mu.Lock()
		st.markerPlain = false
		d.mu.Unlock()
	}
	return &UntrustedError{Repo: repo, Path: path, Err: err}
}

// gitActive reports whether a git process may be writing the reftable stack of
// the repository at path: git's lock of the list (unless ownLock: the caller
// holds it), a lock of a table being compacted, or a table being written is
// there (the names of git's reftable/stack.c). A repository without a reftable
// directory has no stack to write. A file older than staleGitFileAge is left by
// a git process that died, not activity.
func gitActive(path string, ownLock bool) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(path, "reftable"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read the reftable directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if ownLock && name == "tables.list.lock" {
			continue
		}
		if !strings.HasSuffix(name, ".lock") && !strings.Contains(name, ".temp.") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			// Gone since the listing: the git process finished with it.
			continue
		}
		if err != nil {
			return false, fmt.Errorf("stat %q: %w", name, err)
		}
		if time.Since(info.ModTime()) < staleGitFileAge {
			return true, nil
		}
	}
	return false, nil
}

// hasStack reports whether the repository at path has a reftable stack: its
// reftable directory. A repository that is gone, or one without the reftable
// backend (a files-backend copy being imported), has none, so nothing of it is
// written without fsync and nothing needs to be made durable. A reftable
// directory whose table list cannot be read is a stack all the same (git
// replaces the list by a rename, so it never lacks one): making it durable
// fails, and the marker stays.
func hasStack(path string) (bool, error) {
	if _, err := os.Lstat(filepath.Join(path, "reftable")); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("stat the reftable directory: %w", err)
	}
	return true, nil
}

// stackLock is git's reference lock of a repository, taken by the durable
// step.
type stackLock struct {
	path string
	// content is the tag and the nonce the step wrote into it, and info the file
	// it created: a lock is the step's own only when both still match (an inode
	// is reused as soon as it is freed, so the file alone does not tell).
	content []byte
	info    os.FileInfo
}

// lockStack takes git's reference lock of the repository at path without
// waiting: ok is false when it is held. The lock is created with its content
// at once (a linked temporary file), so that a lock left by a process that
// died is recognized by the next owner (removeStaleStackLock).
func lockStack(path string) (*stackLock, bool, error) {
	lockPath := filepath.Join(path, "reftable", "tables.list.lock")

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	content := []byte(stackLockTag + hex.EncodeToString(nonce) + "\n")

	tmp, err := os.CreateTemp(path, stepTempPrefix+"*")
	if err != nil {
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	info, err := tmp.Stat()
	if err != nil {
		_ = tmp.Close()
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, false, fmt.Errorf("take the reference lock: %w", err)
	}

	for attempt := 0; ; attempt++ {
		err := os.Link(tmp.Name(), lockPath)
		if err == nil {
			return &stackLock{path: lockPath, content: content, info: info}, true, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, false, fmt.Errorf("take the reference lock: %w", err)
		}
		if attempt > 0 {
			return nil, false, nil
		}
		stale, err := removeStaleGitLock(lockPath)
		if err != nil {
			return nil, false, err
		}
		if !stale {
			return nil, false, nil
		}
	}
}

// removeStaleGitLock removes the lock of a stack's table list when it is older
// than staleGitFileAge: a git process that died left it, and Gitaly's
// housekeeping would remove it by the same rule. It reports whether it did.
func removeStaleGitLock(lockPath string) (bool, error) {
	info, err := os.Lstat(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("stat the reference lock: %w", err)
	}
	if time.Since(info.ModTime()) < staleGitFileAge {
		return false, nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("remove a stale reference lock: %w", err)
	}
	return true, nil
}

// release removes the lock if it is still the one the step created (same file,
// same nonce): a lock that someone else removed (housekeeping's stale-lock
// cleanup) and that git or another step took again is never removed.
func (l *stackLock) release() error {
	current, err := os.Lstat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the reference lock: %w", err)
	}
	content, err := os.ReadFile(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("release the reference lock: %w", err)
	}
	if !os.SameFile(current, l.info) || !bytes.Equal(content, l.content) {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("release the reference lock: %w", err)
	}
	return nil
}

// removeStaleStackLock removes the reference lock of the repository at path
// when a durable step of a process that died left it: git's writes would fail
// on it until housekeeping removes stale locks. A lock of git is never
// removed. Only the owner calls it, when it takes the ownership: the durable
// steps of the process that owned the storage before have ended with it.
func removeStaleStackLock(path string) error {
	lockPath := filepath.Join(path, "reftable", "tables.list.lock")
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the reference lock: %w", err)
	}
	if !bytes.HasPrefix(content, []byte(stackLockTag)) {
		return nil
	}
	if err := os.Remove(lockPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove a stale reference lock: %w", err)
	}
	return nil
}

// removeStepTemporaries removes the temporary files that a durable step of a
// process that died left in the repository at path.
func removeStepTemporaries(path string) error {
	leftovers, err := filepath.Glob(filepath.Join(path, stepTempPrefix+"*"))
	if err != nil {
		return err
	}
	for _, leftover := range leftovers {
		if err := os.Remove(leftover); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove a temporary file of a durable step: %w", err)
		}
	}
	return nil
}

// syncStack makes durable the tables of the stack not made durable yet in this
// dirty period, then the list and the directory. synced keeps only the tables
// the stack still lists.
func (d *Durability) syncStack(path string, st *refState) error {
	reftable := filepath.Join(path, "reftable")
	names, err := readTablesList(filepath.Join(reftable, "tables.list"))
	if err != nil {
		return fmt.Errorf("read the table list: %w", err)
	}

	d.mu.Lock()
	listed := make(map[string]struct{}, len(names))
	var pending []string
	for _, name := range names {
		listed[name] = struct{}{}
		if _, ok := st.synced[name]; !ok {
			pending = append(pending, name)
		}
	}
	for name := range st.synced {
		if _, ok := listed[name]; !ok {
			delete(st.synced, name)
		}
	}
	d.mu.Unlock()

	for _, name := range pending {
		if err := d.syncPath(filepath.Join(reftable, name)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Compacted away after the list was read: the next list names
				// the table that replaced it.
				continue
			}
			return err
		}
		d.mu.Lock()
		st.synced[name] = struct{}{}
		d.mu.Unlock()
	}

	for _, p := range []string{filepath.Join(reftable, "tables.list"), reftable} {
		if err := d.syncPath(p); err != nil {
			return err
		}
	}
	return nil
}

// syncWatermark makes the watermark of the repository at path and its directory
// durable, when the syncer writes it without fsync (WithWatermark) and the
// repository has one. The callers decide the marker after it, and hold the
// syncer's repository lock, under which the syncer writes the watermark: it
// cannot change before the marker goes.
func (d *Durability) syncWatermark(path string) error {
	if d.watermark == nil {
		return nil
	}

	file := d.watermark(path)
	for _, p := range []string{file, filepath.Dir(file)} {
		if err := d.syncPath(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
	}
	return nil
}

// Tracked returns the paths of the repositories that have a state: the dirty
// ones and the clean ones that a live git process still counts on. The durable
// steps visit them all, so that a clean state leaves the map once its last
// process ended.
func (d *Durability) Tracked() []string {
	if d == nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	paths := make([]string, 0, len(d.repos))
	for path := range d.repos {
		paths = append(paths, path)
	}
	return paths
}

// Forget hands the path of a repository over to what comes there next: the
// caller moved the local repository aside under the syncer's repository lock,
// or, after a kernel restart, decided to keep in place a repository that is not
// a cache of the storage core. The state stops being untrusted, since what made
// it so is no longer at path, and the marker goes by dropMarker's rule. A git
// process of this process that is still live names the repository by its path,
// so its later writes land in what is there now (the hydrated repository, say):
// the marker then stays, plain (without the untrusted record, so that a restart
// does not move the new repository aside), until the process exited and a
// durable step made the stack durable.
func (d *Durability) Forget(ctx context.Context, repo RepoRef, path string) error {
	if d == nil {
		return nil
	}

	markerPath, err := d.markerPath(ctx, repo, path)
	if err != nil {
		return err
	}

	st := d.lockState(path)
	defer st.file.Unlock()

	d.mu.Lock()
	// A plain marker of this boot is on disk when the state was dirty with one;
	// otherwise one is written if the marker has to stay.
	plainOnDisk := st.dirty && st.markerPlain && st.marker == markerPath
	st.repo = repo
	st.marker = markerPath
	d.setDirtyLocked(st)
	st.untrusted = nil
	st.synced = map[string]struct{}{}
	d.mu.Unlock()

	dropped, err := d.forgetLocked(path, st, repo)
	d.mu.Lock()
	untrusted := st.untrusted != nil
	d.mu.Unlock()
	if dropped || plainOnDisk || untrusted {
		// An untrusted state (a failed fsync of a repository kept in place) has
		// its record in the marker, or is kept untrusted by this process.
		return err
	}

	content, encodeErr := json.Marshal(marker{RepoRef: repo, BootID: d.bootID})
	if encodeErr == nil {
		encodeErr = d.writeDurably(markerPath, content)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if encodeErr != nil {
		// The state is dirty, so a writer's MarkDirty would not write the
		// marker: without one on disk, the repository at path cannot be
		// trusted. It is moved aside again, and Forget retried.
		if st.untrusted == nil {
			st.untrusted = fmt.Errorf("write the dirty marker: %w", encodeErr)
		}
		return errors.Join(err, st.untrusted)
	}
	st.markerPlain = true
	return err
}

// forgetLocked lets dropMarker decide the marker of a forgotten repository.
// Its watermark is made durable first. One kept in place is then made durable
// under git's reference lock, as a durable step does; when the lock is busy the
// marker stays for the next step. The caller holds the state's file lock and the
// syncer's repository lock.
func (d *Durability) forgetLocked(path string, st *refState, repo RepoRef) (bool, error) {
	stack, err := hasStack(path)
	if err != nil {
		return false, err
	}
	// The watermark goes first, outside git's reference lock, as in Settle.
	if err := d.syncWatermark(path); err != nil {
		return false, d.stepErrorLocked(st, repo, path, err)
	}
	if !stack {
		return d.dropMarker(path, st, false)
	}

	lock, ok, err := lockStack(path)
	if err != nil || !ok {
		return false, err
	}
	dropped := false
	if err = d.syncStack(path, st); err != nil {
		err = d.stepErrorLocked(st, repo, path, err)
	} else {
		dropped, err = d.dropMarker(path, st, true)
	}
	if releaseErr := lock.release(); err == nil {
		err = releaseErr
	}
	return dropped, err
}

// Recovered is a dirty repository found by Recover.
type Recovered struct {
	Repo RepoRef
	Path string
	// Reboot reports that the marker is of another kernel boot: the repository
	// must be moved aside before anything reads it.
	Reboot bool
	// Untrusted reports that a durable step of the previous process found a
	// failed fsync: the repository must be moved aside too, and is never
	// settled.
	Untrusted bool
}

// Recover reads the dirty markers of the storages; only the owner calls it,
// when it takes the ownership. Every marker makes its repository dirty again.
// One of the running boot is settled once no git process of the previous owner
// can still write the stack (dropMarker: gitActive, and the recovery grace),
// unless the marker records it untrusted; one of another boot makes the
// repository untrusted (errReboot). What a durable step of the previous owner
// left (its reference lock, its temporary files) is removed. It returns every
// repository found; the caller moves aside the ones of another boot and the
// untrusted ones before serving (Forget then hands their paths over). A marker
// that cannot be read is an error: its repository is unknown, and serving
// could read a torn stack.
func (d *Durability) Recover(ctx context.Context, storages []string) ([]Recovered, error) {
	if d == nil || !d.ownsLocks() {
		return nil, nil
	}

	var found []Recovered
	for _, storageName := range storages {
		dir, err := d.markerDir(ctx, storageName)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the dirty markers of storage %q: %w", storageName, err)
		}

		for _, entry := range entries {
			markerPath := filepath.Join(dir, entry.Name())
			if entry.Name() == ownerLockName {
				continue
			}
			if strings.HasPrefix(entry.Name(), markerTempPrefix) {
				// A temporary file of a marker write that did not complete: the
				// write never returned, so no reference write relied on it.
				if err := os.Remove(markerPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return nil, fmt.Errorf("remove an incomplete dirty marker: %w", err)
				}
				continue
			}
			if !strings.HasSuffix(entry.Name(), ".json") {
				return nil, fmt.Errorf("unexpected file %q in the dirty marker directory", markerPath)
			}

			content, err := os.ReadFile(markerPath)
			if err != nil {
				return nil, fmt.Errorf("read the dirty marker %q: %w", markerPath, err)
			}
			var m marker
			if err := json.Unmarshal(content, &m); err != nil || m.Storage != storageName || m.RelativePath == "" || m.BootID == "" {
				return nil, fmt.Errorf("corrupt dirty marker %q: the repository it names cannot be trusted and is unknown", markerPath)
			}

			path, err := d.locator.GetRepoPath(ctx, repoOf(m.RepoRef), storage.WithRepositoryVerificationSkipped())
			if err != nil {
				return nil, fmt.Errorf("dirty marker %q: %w", markerPath, err)
			}

			if err := removeStaleStackLock(path); err != nil {
				return nil, err
			}
			if err := removeStepTemporaries(path); err != nil {
				return nil, err
			}

			reboot := m.BootID != d.bootID
			st := d.lockState(path)
			d.mu.Lock()
			d.setDirtyLocked(st)
			st.repo = m.RepoRef
			st.marker = markerPath
			st.markerPlain = !reboot && m.Untrusted == ""
			switch {
			case m.Untrusted != "":
				st.untrusted = errors.New(m.Untrusted)
			case reboot:
				st.untrusted = errReboot
			}
			if !reboot {
				// A git process of the previous owner, of this boot, may still be
				// committing (dropMarker).
				st.settleAfter = time.Now().Add(d.recoveryGrace)
			}
			d.mu.Unlock()
			st.file.Unlock()
			found = append(found, Recovered{Repo: m.RepoRef, Path: path, Reboot: reboot, Untrusted: m.Untrusted != ""})
		}
	}
	return found, nil
}

// CountUntrusted counts a repository moved aside for reason.
func (d *Durability) CountUntrusted(reason string) {
	if d == nil {
		return
	}
	d.untrusted.WithLabelValues(reason).Inc()
}

// ensureDir creates the directory dir when it does not exist, and makes its
// entry and the entry of a parent it created durable.
func (d *Durability) ensureDir(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	parent := filepath.Dir(dir)
	_, parentErr := os.Stat(parent)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := d.syncPath(parent); err != nil {
		return err
	}
	if errors.Is(parentErr, fs.ErrNotExist) {
		return d.syncPath(filepath.Dir(parent))
	}
	return nil
}

// writeDurably writes content to path through a temporary file in the same
// directory, made durable before and after the rename.
func (d *Durability) writeDurably(path string, content []byte) (returnedErr error) {
	dir := filepath.Dir(path)
	if err := d.ensureDir(dir); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, markerTempPrefix+"*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if returnedErr != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		return err
	}
	if err := d.sync(tmp); err != nil {
		return writebackError{err: err}
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return d.syncPath(dir)
}

// syncPath makes the file or directory at path durable. A failed open is
// returned as it is; a failed fsync is a writebackError.
func (d *Durability) syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := d.sync(f); err != nil {
		return writebackError{err: err}
	}
	return nil
}

// readTablesList returns the table names of a reftable stack.
func readTablesList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var names []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if name := strings.TrimSpace(scanner.Text()); name != "" {
			names = append(names, name)
		}
	}
	return names, scanner.Err()
}

// Describe implements prometheus.Collector.
func (d *Durability) Describe(descs chan<- *prometheus.Desc) {
	d.marked.Describe(descs)
	d.cleared.Describe(descs)
	d.untrusted.Describe(descs)
	d.stepErrors.Describe(descs)
	d.dirty.Describe(descs)
	d.inflight.Describe(descs)
	d.owning.Describe(descs)
}

// Collect implements prometheus.Collector.
func (d *Durability) Collect(metrics chan<- prometheus.Metric) {
	d.marked.Collect(metrics)
	d.cleared.Collect(metrics)
	d.untrusted.Collect(metrics)
	d.stepErrors.Collect(metrics)
	d.dirty.Collect(metrics)
	d.inflight.Collect(metrics)
	d.owning.Collect(metrics)
}
