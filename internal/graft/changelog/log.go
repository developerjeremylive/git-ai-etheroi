package changelog

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/graft/refchange"
)

const (
	// dirName is the directory inside a repository's git directory that holds
	// Graft's data.
	dirName = "graft"
	// logFileName is the name of the changeset log inside dirName.
	logFileName = "changesets.jsonl"
	// lockSuffix is appended to the log path to name the lock file that
	// serializes writers across processes.
	lockSuffix = ".lock"

	// indexInterval is the number of sequences between two points of the offset
	// index. A scan starts at most indexInterval entries before its cursor.
	indexInterval = 256
	// maxIdleStates bounds the number of logs whose cached state is kept while
	// no caller uses them. The least recently needed states beyond it are not
	// kept, and their next use rescans the file.
	maxIdleStates = 1024
	// backwardChunkSize is the number of bytes read at once when looking for the
	// last complete line of the log.
	backwardChunkSize = 64 << 10

	// UnknownOperation is the operation recorded for updates whose operation
	// cannot be determined, such as updates made outside of a gRPC call.
	UnknownOperation = "unknown"

	// OperationLandBatch is the operation recorded for the update that lands a
	// batch of changes with a single reference update. Its updates carry Members.
	OperationLandBatch = "land_batch"
)

// Entry is one line of the changeset log, that is one recorded reference
// update.
type Entry struct {
	// Sequence is the position of the entry in the log. Sequences are dense and
	// start at 1.
	Sequence uint64 `json:"sequence"`
	// Reference is the fully qualified reference that was updated, byte for byte.
	// Reference names may contain bytes that are not valid UTF-8, which JSON
	// strings cannot hold, so such a name is stored in the reference_b64 field
	// too, see MarshalJSON.
	Reference string `json:"reference"`
	// OldOID is the previous target of the reference. It is the zero OID when the
	// reference was created.
	//
	// The reference-transaction hook does not report the previous value of an
	// update that was made without an expected old value, such as the deletions
	// of RefService.DeleteRefs, and reports the zero OID instead. Such a forced
	// update therefore looks like a creation when its new value is not zero. A
	// deletion without an expected old value has the zero OID as both OldOID and
	// NewOID. NewOID alone tells that the reference was deleted.
	OldOID string `json:"old_oid"`
	// NewOID is the new target of the reference, the zero OID when it was deleted.
	NewOID string `json:"new_oid"`
	// Operation names the operation that performed the update. It is derived from
	// the gRPC method (see OperationFromContext), so pushes are recorded as
	// "ssh_receive_pack" or "post_receive_pack", not as the "receive_pack" name
	// used in the ChangesetService proto comment.
	Operation string `json:"operation"`
	// Actor identifies who performed the update. It is valid UTF-8: Append
	// replaces invalid bytes with U+FFFD.
	Actor string `json:"actor"`
	// RecordedAt is when the entry was recorded.
	RecordedAt time.Time `json:"recorded_at"`
	// Rewritten is true when the update rewrote history.
	Rewritten bool `json:"rewritten,omitempty"`
	// Members lists the changes that a batched land folded into this update, in
	// the order they were landed. It is empty for updates that are not batches.
	// See Member.
	Members []Member `json:"members,omitempty"`
	// MembersTruncated is true when Members is not the complete list of the changes
	// folded into this update, because the list was cut to fit the size limit that
	// the hooks payload imposes (see gitcmd.ChangelogMeta).
	MembersTruncated bool `json:"members_truncated,omitempty"`
	// Lands are the identities of the changes that the update landed on the branch Reference, in
	// the order they were landed: every member of a batch, or the one change of a land. They are
	// recorded on every land path and are never cut (see Land, WithLands and doc/graft/PLAN11.md).
	// They are empty for updates that are not lands.
	Lands []Land `json:"lands,omitempty"`
	// LandInvocation identifies the update that landed Lands: the invocation of its reference
	// transaction (see gitcmd.ChangelogMeta.Invocation), which its land intents carry too. A land
	// intent is confirmed, and may go, once the entry of its own update is durable. It is empty
	// without Lands.
	LandInvocation string `json:"land_invocation,omitempty"`
}

// entryFields is Entry without its JSON methods.
type entryFields Entry

// entryJSON is the JSON form of an entry.
type entryJSON struct {
	entryFields
	// ReferenceBase64 holds the reference, base64 encoded, when it is not valid
	// UTF-8. The reference field then holds a readable but lossy form of it, in
	// which the invalid bytes are replaced with U+FFFD.
	ReferenceBase64 string `json:"reference_b64,omitempty"`
}

// MarshalJSON encodes the entry as one line of the log. A reference that is not
// valid UTF-8 is also encoded in base64, because encoding/json would replace its
// invalid bytes and the entry would then name another reference.
func (e Entry) MarshalJSON() ([]byte, error) {
	encoded := entryJSON{entryFields: entryFields(e)}
	if !utf8.ValidString(e.Reference) {
		encoded.Reference = strings.ToValidUTF8(e.Reference, string(utf8.RuneError))
		encoded.ReferenceBase64 = base64.StdEncoding.EncodeToString([]byte(e.Reference))
	}

	return json.Marshal(encoded)
}

// UnmarshalJSON decodes one line of the log, see MarshalJSON.
func (e *Entry) UnmarshalJSON(data []byte) error {
	var decoded entryJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}

	if decoded.ReferenceBase64 != "" {
		reference, err := base64.StdEncoding.DecodeString(decoded.ReferenceBase64)
		if err != nil {
			return fmt.Errorf("decoding reference_b64: %w", err)
		}
		decoded.Reference = string(reference)
	}

	*e = Entry(decoded.entryFields)

	return nil
}

// Update is one reference update to record.
type Update struct {
	// Reference is the fully qualified reference that is updated.
	Reference string
	// OldOID is the previous target (zero OID when the reference is created, see
	// Entry.OldOID).
	OldOID string
	// NewOID is the new target (zero OID when the reference is deleted).
	NewOID string
}

// Meta describes the operation that performed the updates passed to a single
// Append call. All updates of one call share it.
type Meta struct {
	// Operation names the operation, see OperationFromContext.
	Operation string
	// Actor identifies who performed the operation.
	Actor string
	// Rewritten is true when the operation rewrote history.
	Rewritten bool
	// RecordedAt is the timestamp of the entries. The zero value means now.
	RecordedAt time.Time
	// Members lists the changes folded into the updates of this call by a batched
	// land. Every entry of the call carries a copy of them. See Member.
	Members []Member
	// MembersTruncated is true when Members is not the complete list of the changes
	// folded into the updates, see Entry.MembersTruncated.
	MembersTruncated bool
	// LandReference is the branch that Lands landed on. Only the entry of the update of that
	// branch carries them (see LandsOf).
	LandReference string
	// LandInvocation identifies the update that recorded Lands, as its land intents do (see
	// Entry.LandInvocation).
	LandInvocation string
	// Lands are the identities of the changes that the updates landed, see Entry.Lands.
	Lands []Land
}

// Path returns the path of the changeset log of the repository whose git
// directory is repoPath.
func Path(repoPath string) string {
	return filepath.Join(repoPath, dirName, logFileName)
}

// IsZeroOID reports whether oid is the zero object ID of either hash function.
func IsZeroOID(oid string) bool {
	return oid != "" && strings.Trim(oid, "0") == ""
}

// logState is the in-process view of one changeset log. It caches the last
// sequence and the end of the last complete entry, and it keeps an index of
// the offsets of some entries, so that appends and scans do not have to read
// the whole file. The cache is only trusted while the file size matches the
// size observed when the cache was last updated, which detects appends made by
// other processes. Index points are verified before they are used.
type logState struct {
	// mu serializes the appends of this process to the log, and guards the fields
	// below.
	mu sync.Mutex
	// users counts the callers that hold the state. It is guarded by statesMu.
	users int
	// loaded is false until the cache has been filled from the file.
	loaded bool
	// lastSeq is the sequence of the last valid entry.
	lastSeq uint64
	// lastReference and lastNewOID are the reference and the new value of the last valid entry,
	// empty when there is none. With lastSeq they identify the last entry (see LogHead).
	lastReference string
	lastNewOID    string
	// validEnd is the offset just after the last complete, valid entry.
	validEnd int64
	// fileSize is the size of the file when the cache was last updated.
	fileSize int64
	// rescans counts the times the cache was filled from the file. Tests use it.
	rescans int
	// untrusted is set when an append failed after writing and could not cut its
	// bytes off again: the file from untrustedFrom on may hold bytes that are not
	// durable although they read back (after a failed fsync, the kernel may have
	// dropped them), so loading the state ignores them and the next append
	// truncates them.
	untrusted     bool
	untrustedFrom int64
	// dirsSynced is true once this process has fsync'd the directories that hold
	// the log (see syncLogDirs) and the append succeeded. It is reset when an
	// append fails. While it is false every append syncs them, so that the
	// directory entries of a log or a graft directory created by an append that
	// failed, or by a process that crashed, become durable before an entry is
	// acknowledged.
	dirsSynced bool

	// indexMu guards index.
	indexMu sync.Mutex
	// index holds points of the log in ascending order of sequence: the offset of
	// the entry with the sequence, for sequences that are multiples of
	// indexInterval. Scans start at the nearest point before their cursor.
	index []indexPoint
}

// indexPoint is a point of the offset index.
type indexPoint struct {
	sequence uint64
	offset   int64
}

var (
	statesMu sync.Mutex
	// states holds the state of the logs. A state is kept after its last user
	// releases it, so that the next use does not have to read the file. Idle
	// states are bounded by maxIdleStates. A state that was dropped starts
	// unloaded and rescans its file, which is the source of truth.
	states = map[string]*logState{}
)

// acquireState returns the state of the log at path, and the function that
// releases it. The state must not be used after it is released.
func acquireState(path string) (*logState, func()) {
	statesMu.Lock()
	state, ok := states[path]
	if !ok {
		state = &logState{}
		states[path] = state
	}
	state.users++
	statesMu.Unlock()

	return state, func() {
		statesMu.Lock()
		defer statesMu.Unlock()

		state.users--
		evictIdleStates()
	}
}

// evictIdleStates drops idle states until at most maxIdleStates remain idle. The
// caller must hold statesMu.
func evictIdleStates() {
	if len(states) <= maxIdleStates {
		return
	}

	idle := 0
	for _, state := range states {
		if state.users == 0 {
			idle++
		}
	}

	for path, state := range states {
		if idle <= maxIdleStates {
			return
		}
		if state.users == 0 {
			delete(states, path)
			idle--
		}
	}
}

// Record is the updates of one reference transaction with the metadata of the
// operation that performed them. AppendGroup records several of them at once.
type Record struct {
	Meta    Meta
	Updates []Update
}

// Append records updates in the changeset log of the repository whose git
// directory is repoPath and returns the recorded entries. It is AppendGroup with
// one record.
func Append(repoPath string, meta Meta, updates []Update) ([]Entry, error) {
	entries, err := AppendGroup(repoPath, []Record{{Meta: meta, Updates: updates}})
	if err != nil {
		return nil, err
	}

	return entries[0], nil
}

// AppendGroup records the records in the changeset log of the repository whose
// git directory is repoPath, in the order given, and returns the entries of each
// record. The entries of all records get consecutive sequences and are written
// with a single write and a single fsync. Appends are serialized within the
// process and across processes with an flock on the log's lock file, and are
// fsync'd before AppendGroup returns.
//
// The entries become visible to the readers of the log (Scan, List,
// LastSequence) only once they are durable, so a reader never returns an entry
// that a crash could take back. When AppendGroup fails, none of its entries is
// durable or was visible, and the next append writes over whatever it wrote.
//
// A torn last line, left behind by a crashed writer, is ignored and truncated
// before the new entries are written.
//
// Limits, which the callers must keep in mind:
//   - The log is written to the repository directory it is given. Under the
//     experimental WAL, the directory of a transaction is a snapshot that is not
//     the repository itself, so entries written there would be lost.
//   - Each Gitaly node keeps its own log and its own sequences. Behind Praefect,
//     reads may be routed to a replica whose log lags or differs.
//   - Each wait for the lock file is bounded by lockWaitTimeout: an append that
//     does not get it fails. The appends of this process queue on the state's
//     mutex before that wait, which is not bounded. An fsync that never returns
//     is not bounded either: its append holds the lock, and the appends of other
//     processes fail at lockWaitTimeout.
func AppendGroup(repoPath string, records []Record) ([][]Entry, error) {
	entries := make([][]Entry, len(records))

	empty := true
	for i := range records {
		if len(records[i].Updates) > 0 {
			empty = false
		}

		// The updates are already committed when they are recorded, so a malformed
		// member must not fail the call and lose the entries of the whole batch.
		// Such a member is dropped instead, and the entries say that their members
		// are incomplete.
		meta := &records[i].Meta
		if members, dropped := validMembers(meta.Members); dropped {
			meta.Members = members
			meta.MembersTruncated = true
		}

		// Proto strings, which serve these fields, must be valid UTF-8, and JSON
		// would replace the invalid bytes anyway. Replace them here, so that the
		// returned entries match the recorded ones.
		meta.Operation = strings.ToValidUTF8(meta.Operation, string(utf8.RuneError))
		meta.Actor = strings.ToValidUTF8(meta.Actor, string(utf8.RuneError))
	}
	if empty {
		return entries, nil
	}

	path := Path(repoPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating changelog directory: %w", err)
	}

	state, release := acquireState(path)
	defer release()

	state.mu.Lock()
	err := state.appendLocked(repoPath, path, records, entries)
	if err != nil {
		// Whatever state we cached may be stale now, so force a rescan on the
		// next append. The failed append may also have created the log or its
		// directory without syncing their directory entries, so the next append
		// syncs the directories again.
		state.loaded = false
		state.dirsSynced = false
	}
	state.mu.Unlock()

	if err != nil {
		return nil, err
	}

	notify(path)

	return entries, nil
}

// appendLocked writes the entries of the records to the log at path, which
// belongs to the repository whose git directory is repoPath, and stores the
// entries of record i in entries[i]. The caller must hold s.mu. The cached state,
// and so what readers see, moves past the new entries only after they are
// durable.
func (s *logState) appendLocked(repoPath, path string, records []Record, entries [][]Entry) error {
	unlock, err := lockFile(path + lockSuffix)
	if err != nil {
		return err
	}
	defer unlock()

	// A log created by this call, for example in a repository that was recreated
	// at the same path, needs its directory entries synced too, or a crash could
	// lose the file together with the entries that were fsync'd.
	_, statErr := os.Stat(path)
	created := errors.Is(statErr, fs.ErrNotExist)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("opening changelog: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat changelog: %w", err)
	}

	if valid, err := s.cacheDescribes(f, info.Size()); err != nil {
		return err
	} else if !valid {
		if err := s.load(f, info.Size()); err != nil {
			return err
		}
	}

	// Drop a torn tail left behind by a crashed writer, or the bytes of an append
	// that failed. They were never durable and never visible, so nothing that was
	// acknowledged is lost.
	if s.fileSize > s.validEnd {
		if err := truncateFile(f, s.validEnd); err != nil {
			return fmt.Errorf("truncating torn changelog tail: %w", err)
		}
		s.fileSize = s.validEnd
		s.untrusted = false
	}

	now := time.Now()

	var (
		buf    bytes.Buffer
		points []indexPoint
		last   Entry
	)
	seq := s.lastSeq
	for i, record := range records {
		meta := record.Meta
		recordedAt := meta.RecordedAt
		if recordedAt.IsZero() {
			recordedAt = now
		}
		recordedAt = recordedAt.UTC()

		recorded := make([]Entry, 0, len(record.Updates))
		for _, update := range record.Updates {
			seq++
			entry := Entry{
				Sequence:         seq,
				Reference:        update.Reference,
				OldOID:           update.OldOID,
				NewOID:           update.NewOID,
				Operation:        meta.Operation,
				Actor:            meta.Actor,
				RecordedAt:       recordedAt,
				Rewritten:        meta.Rewritten,
				Members:          normalizeMembers(meta.Members),
				MembersTruncated: meta.MembersTruncated,
				Lands:            LandsOf(meta, update.Reference),
				LandInvocation:   LandInvocationOf(meta, update.Reference),
			}

			line, err := json.Marshal(entry)
			if err != nil {
				return fmt.Errorf("encoding changelog entry: %w", err)
			}

			if seq%indexInterval == 0 {
				points = append(points, indexPoint{sequence: seq, offset: s.validEnd + int64(buf.Len())})
			}

			buf.Write(line)
			buf.WriteByte('\n')
			recorded = append(recorded, entry)
			last = entry
		}
		entries[i] = recorded
	}

	if _, err := f.WriteAt(buf.Bytes(), s.validEnd); err != nil {
		s.discardFrom(f, s.validEnd)
		return fmt.Errorf("writing changelog: %w", err)
	}

	if err := syncFile(f); err != nil {
		s.discardFrom(f, s.validEnd)
		return fmt.Errorf("syncing changelog: %w", err)
	}

	// The entries are only durable once the directory entries of the log file and
	// of the graft directory are, which may have been created by this append, by
	// an earlier one that failed, or by a process that crashed before syncing
	// them. Only the first case can be told from the file, so both directories
	// are also synced on the first append of this process and after a failed
	// append.
	if created || !s.dirsSynced {
		if err := syncLogDirs(repoPath, path); err != nil {
			s.discardFrom(f, s.validEnd)
			return err
		}
		s.dirsSynced = true
	}

	s.lastSeq = seq
	s.lastReference, s.lastNewOID = last.Reference, last.NewOID
	s.validEnd += int64(buf.Len())
	s.fileSize = s.validEnd
	s.loaded = true

	for _, point := range points {
		s.addIndexPoint(point.sequence, point.offset)
	}

	return nil
}

// cacheDescribes reports whether the cached state still describes the file f, whose size is
// size, so that it need not be loaded again. The caller must hold s.mu.
//
// The size alone does not tell: while the cache knows of bytes after its last valid entry (a torn
// tail, or the untrusted bytes of a failed append), another process may have cut them off and
// appended durable entries that end at the same size, and acting on the cache would cut those
// off. Such a cache is therefore never trusted. A cache without such bytes can only see the file
// grow, as no process cuts the file below the end of the durable entries; it is trusted when the
// size is unchanged and the line before its end still holds its last entry.
func (s *logState) cacheDescribes(f *os.File, size int64) (bool, error) {
	if !s.loaded || size != s.fileSize || s.fileSize != s.validEnd {
		return false, nil
	}
	if s.validEnd == 0 {
		return s.lastSeq == 0, nil
	}

	nl := s.validEnd - 1
	prev, err := lastNewlineBefore(f, nl)
	if err != nil {
		return false, err
	}
	line := make([]byte, nl-(prev+1)+1)
	if _, err := f.ReadAt(line, prev+1); err != nil {
		return false, fmt.Errorf("reading changelog: %w", err)
	}
	if line[len(line)-1] != '\n' {
		return false, nil
	}

	var entry Entry
	if err := json.Unmarshal(line[:len(line)-1], &entry); err != nil {
		return false, nil
	}
	return entry.Sequence == s.lastSeq, nil
}

// discardFrom cuts off the bytes that a failed append wrote from end on. When
// that fails too, the bytes are remembered as untrusted, so that no reader takes
// them for entries. The caller must hold s.mu.
func (s *logState) discardFrom(f *os.File, end int64) {
	if err := truncateFile(f, end); err == nil {
		return
	}

	if !s.untrusted || end < s.untrustedFrom {
		s.untrusted = true
		s.untrustedFrom = end
	}
}

// load fills the cache from the file, whose size is size, ignoring the
// untrusted bytes of a failed append (see discardFrom). The caller must hold
// s.mu.
func (s *logState) load(f *os.File, size int64) error {
	scanned := size
	if s.untrusted && scanned > s.untrustedFrom {
		scanned = s.untrustedFrom
	}

	if err := s.rescan(f, scanned); err != nil {
		return err
	}
	s.fileSize = size

	return nil
}

// rescan fills the cache from the file, whose size is size. It reads backwards
// from the end of the file to the last complete line that holds a valid entry,
// so that its cost does not depend on the size of the log. The caller must hold
// s.mu.
func (s *logState) rescan(f *os.File, size int64) error {
	s.rescans++
	s.lastSeq = 0
	s.lastReference, s.lastNewOID = "", ""
	s.validEnd = 0

	// The bytes after the last newline are a torn tail, which is not part of the
	// log. Each iteration examines the complete line that ends at the newline
	// before end, and moves end before that line if the line is not valid.
	end := size
	for {
		nl, err := lastNewlineBefore(f, end)
		if err != nil {
			return err
		}
		if nl < 0 {
			break
		}

		prev, err := lastNewlineBefore(f, nl)
		if err != nil {
			return err
		}
		lineStart := prev + 1

		line := make([]byte, nl-lineStart)
		if _, err := f.ReadAt(line, lineStart); err != nil {
			return fmt.Errorf("reading changelog: %w", err)
		}

		var entry Entry
		if err := json.Unmarshal(line, &entry); err == nil && entry.Sequence != 0 {
			s.lastSeq = entry.Sequence
			s.lastReference, s.lastNewOID = entry.Reference, entry.NewOID
			s.validEnd = nl + 1
			break
		}

		end = lineStart
	}

	s.fileSize = size
	s.loaded = true

	return nil
}

// lastNewlineBefore returns the offset of the last newline of f that is before
// offset, or -1 when there is none. It reads backwards in chunks.
func lastNewlineBefore(f *os.File, offset int64) (int64, error) {
	buf := make([]byte, backwardChunkSize)
	for offset > 0 {
		n := int64(len(buf))
		if offset < n {
			n = offset
		}

		start := offset - n
		chunk := buf[:n]
		if _, err := f.ReadAt(chunk, start); err != nil {
			return -1, fmt.Errorf("reading changelog: %w", err)
		}

		if i := bytes.LastIndexByte(chunk, '\n'); i >= 0 {
			return start + int64(i), nil
		}

		offset = start
	}

	return -1, nil
}

// addIndexPoint records that the entry with the sequence is at offset, if the
// sequence is an index point that is not yet indexed.
func (s *logState) addIndexPoint(sequence uint64, offset int64) {
	if sequence%indexInterval != 0 {
		return
	}

	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	if n := len(s.index); n > 0 && s.index[n-1].sequence >= sequence {
		return
	}

	s.index = append(s.index, indexPoint{sequence: sequence, offset: offset})
}

// indexBefore returns the closest point of the index whose sequence is not
// greater than after.
func (s *logState) indexBefore(after uint64) (indexPoint, bool) {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	i := sort.Search(len(s.index), func(i int) bool { return s.index[i].sequence > after })
	if i == 0 {
		return indexPoint{}, false
	}

	return s.index[i-1], true
}

// resetIndex drops the index, which no longer describes the file.
func (s *logState) resetIndex() {
	s.indexMu.Lock()
	defer s.indexMu.Unlock()

	s.index = nil
}

// scanOffset returns the offset at which a scan for the entries after the
// sequence after starts. It is the offset of the closest index point if that
// point still describes the file, and the beginning of the file otherwise.
func (s *logState) scanOffset(f *os.File, after uint64) int64 {
	point, ok := s.indexBefore(after)
	if !ok {
		return 0
	}

	// The file may have been replaced since the point was indexed, so the entry
	// at its offset must still have its sequence.
	line, err := bufio.NewReader(io.NewSectionReader(f, point.offset, 1<<62)).ReadBytes('\n')
	if err == nil {
		var entry Entry
		if json.Unmarshal(line, &entry) == nil && entry.Sequence == point.sequence {
			return point.offset
		}
	}

	s.resetIndex()

	return 0
}

// syncLogDirs fsyncs the directory that holds the log at path, and the
// repository directory repoPath that holds that directory.
func syncLogDirs(repoPath, path string) error {
	for _, dir := range []string{filepath.Dir(path), repoPath} {
		if err := syncDir(dir); err != nil {
			return fmt.Errorf("syncing changelog directory: %w", err)
		}
	}

	return nil
}

// syncFile fsyncs the log file f. Tests replace it.
var syncFile = func(f *os.File) error {
	return f.Sync()
}

// truncateFile cuts the log file f to size. Tests replace it.
var truncateFile = func(f *os.File, size int64) error {
	return f.Truncate(size)
}

// durableView returns the end of the durable entries of the log at path and the
// last of them (see logHead). f is the log file, opened by the caller. Readers
// read the log only up to that end.
//
// The cached state only moves past entries once they are durable, so while it
// describes the file it is the answer; taking s.mu waits for an append of this
// process that is between its write and its fsync. When the state is not loaded,
// or the file changed since it was (an append of another process), the state is
// loaded again under the log's lock file, which waits for an append of another
// process, and the file is fsync'd first: bytes that a crashed writer left in the
// page cache are durable once a reader may see them. So are the directory entries
// of the log, which that writer may have crashed before syncing (see
// syncLogDirs), unless this process synced them already.
func (s *logState) durableView(repoPath, path string, f *os.File) (int64, logHead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	info, err := f.Stat()
	if err != nil {
		return 0, logHead{}, fmt.Errorf("stat changelog: %w", err)
	}
	if valid, err := s.cacheDescribes(f, info.Size()); err != nil {
		return 0, logHead{}, err
	} else if valid && s.dirsSynced {
		return s.validEnd, s.head(), nil
	}

	unlock, err := lockFile(path + lockSuffix)
	if err != nil {
		return 0, logHead{}, err
	}
	defer unlock()

	if err := syncFile(f); err != nil {
		return 0, logHead{}, fmt.Errorf("syncing changelog: %w", err)
	}
	if !s.dirsSynced {
		if err := syncLogDirs(repoPath, path); err != nil {
			return 0, logHead{}, err
		}
		s.dirsSynced = true
	}

	info, err = f.Stat()
	if err != nil {
		return 0, logHead{}, fmt.Errorf("stat changelog: %w", err)
	}
	if err := s.load(f, info.Size()); err != nil {
		s.loaded = false
		return 0, logHead{}, err
	}

	return s.validEnd, s.head(), nil
}

// syncDir fsyncs the directory dir, so that the entries it holds are durable.
// Tests replace it.
var syncDir = func(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()

	return d.Sync()
}

// lockWaitTimeout bounds one wait for the log's lock file, which another process holds for one
// append. A holder that never releases it would otherwise block that wait forever: it fails
// instead, and an append sees the error (its updates are reported lost). The bound is per wait
// for the lock file, not per call: the appends and readers of this process that wait for the
// state's mutex (s.mu) behind such a wait are not bounded by it, and each then waits for the
// lock file in turn. A wait that failed keeps its goroutine, blocked in flock(2) on an OS
// thread, and its descriptor until the holder releases the lock. Tests lower it.
var lockWaitTimeout = 30 * time.Second

// lockFile takes an exclusive flock on the file at path and returns the
// function that releases it. It waits at most lockWaitTimeout.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening changelog lock: %w", err)
	}
	unlock := func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return unlock, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		f.Close()
		return nil, fmt.Errorf("locking changelog: %w", err)
	}

	// Another process holds the lock: wait for it in a goroutine, which owns the file if the
	// wait gives up first and releases the lock once it is granted.
	locked := make(chan error, 1)
	go func() { locked <- syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }()

	timer := time.NewTimer(lockWaitTimeout)
	defer timer.Stop()
	select {
	case err := <-locked:
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("locking changelog: %w", err)
		}
		return unlock, nil
	case <-timer.C:
		go func() {
			if err := <-locked; err == nil {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			}
			_ = f.Close()
		}()
		return nil, fmt.Errorf("locking changelog: the lock was not granted within %s", lockWaitTimeout)
	}
}

// Scan calls fn for every entry of the log of the repository whose git
// directory is repoPath with a sequence greater than after, in ascending order.
// Scanning stops when fn returns false or an error. Scan reads the durable
// entries only (see AppendGroup), so it can run concurrently with appends.
//
// The scan starts at the closest index point before after, so its cost depends
// on the entries that follow after and not on the size of the log.
func Scan(repoPath string, after uint64, fn func(Entry) (bool, error)) error {
	return scanFrom(repoPath, after, -1, func(entry Entry, _ int64) (bool, error) {
		return fn(entry)
	})
}

// scanFrom is Scan that also gives fn the offset of the line of each entry, and that starts at the
// offset hint when it is not negative and its line holds the entry with the sequence after+1: a
// caller that knows that offset from an earlier scan reads only the entries it asks for. A hint
// whose line does not hold that entry (the log was replaced) is ignored.
func scanFrom(repoPath string, after uint64, hint int64, fn func(Entry, int64) (bool, error)) error {
	path := Path(repoPath)

	state, release := acquireState(path)
	defer release()

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("opening changelog: %w", err)
	}
	defer f.Close()

	end, _, err := state.durableView(repoPath, path, f)
	if err != nil {
		return err
	}

	offset := state.scanOffset(f, after)
	if hint >= 0 && hint < end && lineHolds(f, hint, after+1) {
		offset = hint
	}
	if offset >= end {
		return nil
	}

	r := bufio.NewReader(io.NewSectionReader(f, offset, end-offset))
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading changelog: %w", err)
		}

		lineOffset := offset
		offset += int64(len(line))

		var entry Entry
		if err := json.Unmarshal(line, &entry); err != nil || entry.Sequence == 0 {
			continue
		}

		state.addIndexPoint(entry.Sequence, lineOffset)

		if entry.Sequence <= after {
			continue
		}

		more, err := fn(entry, lineOffset)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

// lineHolds reports whether the line of f at offset holds the entry with the given sequence.
func lineHolds(f *os.File, offset int64, sequence uint64) bool {
	line, err := bufio.NewReader(io.NewSectionReader(f, offset, 1<<62)).ReadBytes('\n')
	if err != nil {
		return false
	}

	var entry Entry
	return json.Unmarshal(line, &entry) == nil && entry.Sequence == sequence
}

// LastSequence returns the sequence of the last durable entry of the log of the
// repository whose git directory is repoPath, 0 when the log is empty or does
// not exist. An entry whose append has not fsync'd it yet is not included (see
// AppendGroup). It uses the cached state of the log while it describes the file,
// so its cost does not depend on the size of the log.
func LastSequence(repoPath string) (uint64, error) {
	head, err := readLogHead(repoPath)
	if err != nil {
		return 0, err
	}

	return head.sequence, nil
}

// logHead identifies the last durable entry of a log and the file that holds it.
type logHead struct {
	// sequence, reference and newOID are those of the last durable entry; the zero value when the
	// log has none.
	sequence  uint64
	reference string
	newOID    string
	// device and inode identify the log file. A log that was replaced, for example with its
	// repository, is another file.
	device uint64
	inode  uint64
}

// head returns the head of the log as the cache describes it, without the file identity. The
// caller must hold s.mu.
func (s *logState) head() logHead {
	return logHead{sequence: s.lastSeq, reference: s.lastReference, newOID: s.lastNewOID}
}

// readLogHead returns the head of the log of the repository whose git directory is repoPath: the
// zero value when the log does not exist. Like LastSequence, it uses the cached state of the log
// while it describes the file.
func readLogHead(repoPath string) (logHead, error) {
	path := Path(repoPath)

	state, release := acquireState(path)
	defer release()

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return logHead{}, nil
		}
		return logHead{}, fmt.Errorf("opening changelog: %w", err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return logHead{}, fmt.Errorf("stat changelog: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return logHead{}, fmt.Errorf("stat changelog: no file identity in %T", info.Sys())
	}

	_, head, err := state.durableView(repoPath, path, f)
	if err != nil {
		return logHead{}, err
	}
	head.device, head.inode = uint64(stat.Dev), stat.Ino

	return head, nil
}

// List returns up to limit entries with a sequence greater than after, in
// ascending order. A limit of zero or less returns no entries.
func List(repoPath string, after uint64, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}

	var entries []Entry
	err := Scan(repoPath, after, func(entry Entry) (bool, error) {
		entries = append(entries, entry)
		return len(entries) < limit, nil
	})
	if err != nil {
		return nil, err
	}

	return entries, nil
}

// ParseUpdates parses the input of the reference-transaction hook, which has
// one "<old-oid> <new-oid> <reference>" line per update, into the updates that
// are recorded. Only branches and tags are recorded, and updates that do not
// change the target of a reference (old equals new) are skipped, except for a
// deletion without an expected old value, which the hook reports with the zero
// OID as both values and which is recorded (see Entry.OldOID). Internal
// references such as refs/keep-around/*, refs/tmp/* and refs/environments/*,
// symbolic HEAD updates, and the symbolic reference lines that Git 2.46 and
// later report ("ref:<target>") are therefore not recorded. Lines are parsed
// with refchange.ParseHookLine, and lines it rejects are skipped.
func ParseUpdates(input []byte) []Update {
	var updates []Update
	for _, line := range strings.Split(string(input), "\n") {
		oldOID, newOID, reference, ok := refchange.ParseHookLine(line)
		if !ok {
			continue
		}

		if !strings.HasPrefix(reference, "refs/heads/") && !strings.HasPrefix(reference, "refs/tags/") {
			continue
		}

		if strings.HasPrefix(oldOID, "ref:") || strings.HasPrefix(newOID, "ref:") {
			continue
		}

		if oldOID == newOID && !IsZeroOID(newOID) {
			continue
		}

		updates = append(updates, Update{Reference: reference, OldOID: oldOID, NewOID: newOID})
	}

	return updates
}
