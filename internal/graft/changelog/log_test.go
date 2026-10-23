package changelog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	zeroOID = "0000000000000000000000000000000000000000"
	oidA    = "1111111111111111111111111111111111111111"
	oidB    = "2222222222222222222222222222222222222222"
	oidC    = "3333333333333333333333333333333333333333"

	zeroSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
)

// newRepoPath returns a fresh directory that stands in for a git directory.
func newRepoPath(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// forgetState drops the in-process cache of the log at repoPath, as if the
// appends were made by another process.
func forgetState(repoPath string) {
	state, release := acquireState(Path(repoPath))
	defer release()

	state.mu.Lock()
	defer state.mu.Unlock()
	state.loaded = false
}

// cachedState returns the state of the log at path, which must be cached.
func cachedState(t *testing.T, path string) *logState {
	t.Helper()

	statesMu.Lock()
	defer statesMu.Unlock()

	state, ok := states[path]
	require.True(t, ok, "the state of the log must be cached")
	return state
}

func TestAppend_keepsStateCached(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	_, err := Append(repoPath, Meta{Operation: "land", Actor: "agent-a"}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	// The state outlives the append, so that the next appends continue from the
	// cached last sequence instead of reading the whole log again.
	state := cachedState(t, Path(repoPath))
	state.mu.Lock()
	rescans := state.rescans
	state.mu.Unlock()

	for i := 0; i < 10; i++ {
		entries, err := Append(repoPath, Meta{Operation: "land", Actor: "agent-a"}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB}})
		require.NoError(t, err)
		require.Equal(t, uint64(i+2), entries[0].Sequence)
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	require.Equal(t, rescans, state.rescans, "sequential appends must not rescan the log")
}

func TestAcquireState_sharedUntilLastRelease(t *testing.T) {
	t.Parallel()

	path := Path(newRepoPath(t))

	first, releaseFirst := acquireState(path)
	second, releaseSecond := acquireState(path)
	require.Same(t, first, second, "concurrent users share one state")

	releaseFirst()
	statesMu.Lock()
	require.Same(t, second, states[path], "the state is kept while a user remains")
	statesMu.Unlock()

	releaseSecond()
	statesMu.Lock()
	require.Same(t, first, states[path], "the state is kept after its last user releases it")
	require.Zero(t, first.users)
	statesMu.Unlock()
}

func TestAcquireState_evictsIdleStates(t *testing.T) {
	t.Parallel()

	// Acquiring a state does not touch the disk, so the paths need not exist.
	for i := 0; i < maxIdleStates+10; i++ {
		_, release := acquireState(fmt.Sprintf("/nonexistent/evict-%d/%s", i, logFileName))
		release()
	}

	statesMu.Lock()
	defer statesMu.Unlock()

	idle := 0
	for _, state := range states {
		if state.users == 0 {
			idle++
		}
	}
	require.LessOrEqual(t, idle, maxIdleStates, "idle states must be bounded")
}

func readLines(t *testing.T, repoPath string) []string {
	t.Helper()

	data, err := os.ReadFile(Path(repoPath))
	require.NoError(t, err)
	if len(data) == 0 {
		return nil
	}
	require.True(t, strings.HasSuffix(string(data), "\n"), "log must end with a newline")

	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

func TestPath(t *testing.T) {
	t.Parallel()

	require.Equal(t, filepath.Join("/repos/a.git", "graft", "changesets.jsonl"), Path("/repos/a.git"))
}

func TestAppend_noUpdates(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	entries, err := Append(repoPath, Meta{Operation: "land", Actor: "agent"}, nil)
	require.NoError(t, err)
	require.Nil(t, entries)

	_, err = os.Stat(Path(repoPath))
	require.True(t, os.IsNotExist(err), "no log must be created without updates")
}

func TestAppend_assignsDenseSequences(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)
	recordedAt := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

	entries, err := Append(repoPath, Meta{Operation: "land", Actor: "agent-a", RecordedAt: recordedAt}, []Update{
		{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidA},
		{Reference: "refs/heads/feature", OldOID: oidA, NewOID: oidB},
	})
	require.NoError(t, err)
	require.Equal(t, []Entry{
		{Sequence: 1, Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidA, Operation: "land", Actor: "agent-a", RecordedAt: recordedAt},
		{Sequence: 2, Reference: "refs/heads/feature", OldOID: oidA, NewOID: oidB, Operation: "land", Actor: "agent-a", RecordedAt: recordedAt},
	}, entries)

	entries, err = Append(repoPath, Meta{Operation: "ff_branch", Actor: "user-1", Rewritten: true}, []Update{
		{Reference: "refs/tags/v1", OldOID: zeroOID, NewOID: oidC},
	})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, uint64(3), entries[0].Sequence)
	require.True(t, entries[0].Rewritten)
	require.False(t, entries[0].RecordedAt.IsZero(), "a zero RecordedAt must default to now")

	all, err := List(repoPath, 0, 100)
	require.NoError(t, err)
	require.Len(t, all, 3)
	for i, entry := range all {
		require.Equal(t, uint64(i+1), entry.Sequence)
	}
}

func TestAppend_lineFormat(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	_, err := Append(repoPath, Meta{Operation: "ff_branch", Actor: "user-1", RecordedAt: time.Date(2026, 5, 6, 7, 8, 9, 10, time.UTC)}, []Update{
		{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidA},
	})
	require.NoError(t, err)

	lines := readLines(t, repoPath)
	require.Len(t, lines, 1)

	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &fields))
	require.Equal(t, map[string]any{
		"sequence":    float64(1),
		"reference":   "refs/heads/main",
		"old_oid":     zeroOID,
		"new_oid":     oidA,
		"operation":   "ff_branch",
		"actor":       "user-1",
		"recorded_at": "2026-05-06T07:08:09.00000001Z",
	}, fields, "rewritten must be omitted when false")
}

func TestAppend_tornLastLineIsIgnoredAndTruncated(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	_, err := Append(repoPath, Meta{Operation: "land"}, []Update{
		{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA},
		{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB},
	})
	require.NoError(t, err)

	// Simulate a crash in the middle of writing the third entry.
	f, err := os.OpenFile(Path(repoPath), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"sequence":3,"reference":"refs/heads/c","old_oid"`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	entries, err := List(repoPath, 0, 100)
	require.NoError(t, err)
	require.Len(t, entries, 2, "the torn line must be invisible to readers")

	entries, err = Append(repoPath, Meta{Operation: "land"}, []Update{
		{Reference: "refs/heads/c", OldOID: zeroOID, NewOID: oidC},
	})
	require.NoError(t, err)
	require.Equal(t, uint64(3), entries[0].Sequence)

	lines := readLines(t, repoPath)
	require.Len(t, lines, 3, "the torn bytes must have been truncated")
	for i, line := range lines {
		var entry Entry
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "line %d", i)
		require.Equal(t, uint64(i+1), entry.Sequence)
	}
}

func TestAppend_tornLineAfterRestartIsTruncated(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	// A torn tail, written by a crashed process, must not confuse a fresh
	// process whose in-process cache is empty.
	f, err := os.OpenFile(Path(repoPath), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"seq`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	forgetState(repoPath)

	entries, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB}})
	require.NoError(t, err)
	require.Equal(t, uint64(2), entries[0].Sequence)
	require.Len(t, readLines(t, repoPath), 2)
}

func TestAppend_skipsCorruptCompleteLines(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(Path(repoPath)), 0o755))
	require.NoError(t, os.WriteFile(Path(repoPath), []byte(
		`{"sequence":1,"reference":"refs/heads/a","old_oid":"`+zeroOID+`","new_oid":"`+oidA+`"}`+"\n"+
			"not json at all\n"+
			`{"sequence":0,"reference":"refs/heads/zero"}`+"\n",
	), 0o644))

	entries, err := List(repoPath, 0, 100)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	appended, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidB}})
	require.NoError(t, err)
	require.Equal(t, uint64(2), appended[0].Sequence)
}

func TestAppend_picksUpEntriesWrittenByAnotherProcess(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	// Another process appends entry 2 behind our back.
	f, err := os.OpenFile(Path(repoPath), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString(`{"sequence":2,"reference":"refs/heads/x","old_oid":"` + zeroOID + `","new_oid":"` + oidB + `"}` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	entries, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidC}})
	require.NoError(t, err)
	require.Equal(t, uint64(3), entries[0].Sequence)
	require.Len(t, readLines(t, repoPath), 3)
}

func TestAppend_concurrentWritersGetDenseSequences(t *testing.T) {
	t.Parallel()

	const writers = 50

	repoPath := newRepoPath(t)

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			_, err := Append(repoPath, Meta{Actor: fmt.Sprintf("writer-%d", i)}, []Update{
				{Reference: fmt.Sprintf("refs/heads/branch-%d", i), OldOID: zeroOID, NewOID: oidA},
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}

	entries, err := List(repoPath, 0, writers*2)
	require.NoError(t, err)
	require.Len(t, entries, writers)

	seen := map[string]bool{}
	for i, entry := range entries {
		require.Equal(t, uint64(i+1), entry.Sequence, "sequences must be dense")
		require.False(t, seen[entry.Reference], "reference %s recorded twice", entry.Reference)
		seen[entry.Reference] = true
	}
}

func TestAppend_concurrentWritersWithCacheResets(t *testing.T) {
	t.Parallel()

	// Writers that lose their cache, as separate processes would, must still
	// produce dense sequences because every append re-validates the file.
	const writers = 20

	repoPath := newRepoPath(t)

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			if i%3 == 0 {
				forgetState(repoPath)
			}

			_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/x", OldOID: zeroOID, NewOID: oidA}})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	entries, err := List(repoPath, 0, writers*2)
	require.NoError(t, err)
	require.Len(t, entries, writers)
	for i, entry := range entries {
		require.Equal(t, uint64(i+1), entry.Sequence)
	}
}

func TestAppend_coldStartSkipsCorruptTail(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	updates := make([]Update, 0, 600)
	for i := 0; i < 600; i++ {
		updates = append(updates, Update{Reference: fmt.Sprintf("refs/heads/b%d", i), OldOID: zeroOID, NewOID: oidA})
	}
	_, err := Append(repoPath, Meta{}, updates)
	require.NoError(t, err)

	// Complete corrupt lines and a torn line follow the last valid entry.
	f, err := os.OpenFile(Path(repoPath), os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("not json at all\n" + `{"sequence":0,"reference":"refs/heads/zero"}` + "\n" + `{"seq`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// The cache is gone, so the next append must find the end of the log from
	// the file.
	forgetState(repoPath)

	entries, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/next", OldOID: zeroOID, NewOID: oidB}})
	require.NoError(t, err)
	require.Equal(t, uint64(601), entries[0].Sequence)

	lines := readLines(t, repoPath)
	require.Len(t, lines, 601, "the corrupt tail must have been truncated")
	for i, line := range lines {
		var entry Entry
		require.NoError(t, json.Unmarshal([]byte(line), &entry), "line %d", i)
		require.Equal(t, uint64(i+1), entry.Sequence)
	}
}

func TestList_resumesFromIndexAcrossAppends(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	for i := 0; i < 2; i++ {
		updates := make([]Update, 0, 1000)
		for j := 0; j < 1000; j++ {
			updates = append(updates, Update{Reference: fmt.Sprintf("refs/heads/b%d-%d", i, j), OldOID: zeroOID, NewOID: oidA})
		}
		_, err := Append(repoPath, Meta{}, updates)
		require.NoError(t, err)
	}

	state := cachedState(t, Path(repoPath))
	state.indexMu.Lock()
	require.Len(t, state.index, 2000/indexInterval, "one index point per interval")
	state.indexMu.Unlock()

	for _, after := range []uint64{0, 1, 255, 256, 257, 511, 512, 1500, 1998, 1999, 2000} {
		entries, err := List(repoPath, after, 3)
		require.NoError(t, err)

		var want, got []uint64
		for seq := after + 1; seq <= 2000 && len(want) < 3; seq++ {
			want = append(want, seq)
		}
		for _, entry := range entries {
			got = append(got, entry.Sequence)
		}
		require.Equal(t, want, got, "after %d", after)
	}
}

func TestList_staleIndexIsIgnored(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	updates := make([]Update, 0, 1000)
	for j := 0; j < 1000; j++ {
		updates = append(updates, Update{Reference: fmt.Sprintf("refs/heads/b%d", j), OldOID: zeroOID, NewOID: oidA})
	}
	_, err := Append(repoPath, Meta{}, updates)
	require.NoError(t, err)

	// Another repository's log replaces this one behind the cache's back. The
	// index points of the old file no longer describe the new one.
	otherPath := newRepoPath(t)
	_, err = Append(otherPath, Meta{}, updates[:10])
	require.NoError(t, err)
	data, err := os.ReadFile(Path(otherPath))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(Path(repoPath), data, 0o644))

	entries, err := List(repoPath, 700, 10)
	require.NoError(t, err)
	require.Empty(t, entries, "the new log has no entries after 700")

	entries, err = List(repoPath, 5, 10)
	require.NoError(t, err)
	require.Len(t, entries, 5)
	require.Equal(t, uint64(6), entries[0].Sequence)
}

func TestList(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	var updates []Update
	for i := 0; i < 5; i++ {
		updates = append(updates, Update{Reference: fmt.Sprintf("refs/heads/b%d", i), OldOID: zeroOID, NewOID: oidA})
	}
	_, err := Append(repoPath, Meta{}, updates)
	require.NoError(t, err)

	for _, tc := range []struct {
		desc      string
		after     uint64
		limit     int
		sequences []uint64
	}{
		{desc: "from the beginning", after: 0, limit: 100, sequences: []uint64{1, 2, 3, 4, 5}},
		{desc: "limit bounds the result", after: 0, limit: 2, sequences: []uint64{1, 2}},
		{desc: "cursor is exclusive", after: 2, limit: 100, sequences: []uint64{3, 4, 5}},
		{desc: "cursor and limit", after: 1, limit: 2, sequences: []uint64{2, 3}},
		{desc: "cursor past the end", after: 5, limit: 100, sequences: nil},
		{desc: "zero limit", after: 0, limit: 0, sequences: nil},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			entries, err := List(repoPath, tc.after, tc.limit)
			require.NoError(t, err)

			var sequences []uint64
			for _, entry := range entries {
				sequences = append(sequences, entry.Sequence)
			}
			require.Equal(t, tc.sequences, sequences)
		})
	}
}

func TestList_missingLog(t *testing.T) {
	t.Parallel()

	entries, err := List(newRepoPath(t), 0, 10)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestScan_stopsWhenCallbackReturnsFalse(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)
	_, err := Append(repoPath, Meta{}, []Update{
		{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA},
		{Reference: "refs/heads/b", OldOID: zeroOID, NewOID: oidA},
		{Reference: "refs/heads/c", OldOID: zeroOID, NewOID: oidA},
	})
	require.NoError(t, err)

	var seen []uint64
	require.NoError(t, Scan(repoPath, 0, func(entry Entry) (bool, error) {
		seen = append(seen, entry.Sequence)
		return entry.Sequence < 2, nil
	}))
	require.Equal(t, []uint64{1, 2}, seen)
}

func TestScan_propagatesCallbackError(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)
	_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	errBoom := fmt.Errorf("boom")
	require.ErrorIs(t, Scan(repoPath, 0, func(Entry) (bool, error) { return false, errBoom }), errBoom)
}

func TestParseUpdates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc     string
		input    string
		expected []Update
	}{
		{
			desc:     "empty input",
			input:    "",
			expected: nil,
		},
		{
			desc:  "branch and tag updates",
			input: zeroOID + " " + oidA + " refs/heads/main\n" + oidA + " " + oidB + " refs/tags/v1\n",
			expected: []Update{
				{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidA},
				{Reference: "refs/tags/v1", OldOID: oidA, NewOID: oidB},
			},
		},
		{
			desc:     "deletion of a branch is recorded",
			input:    oidA + " " + zeroOID + " refs/heads/gone\n",
			expected: []Update{{Reference: "refs/heads/gone", OldOID: oidA, NewOID: zeroOID}},
		},
		{
			desc:  "deletion without an expected old value is recorded",
			input: zeroOID + " " + zeroOID + " refs/heads/forced\n" + zeroSHA256 + " " + zeroSHA256 + " refs/tags/forced-tag\n",
			expected: []Update{
				{Reference: "refs/heads/forced", OldOID: zeroOID, NewOID: zeroOID},
				{Reference: "refs/tags/forced-tag", OldOID: zeroSHA256, NewOID: zeroSHA256},
			},
		},
		{
			desc:     "symbolic reference lines are skipped",
			input:    zeroOID + " ref:refs/heads/main refs/heads/alias\n" + "ref:refs/heads/main " + oidA + " refs/heads/alias2\n",
			expected: nil,
		},
		{
			desc:     "internal references are skipped",
			input:    oidA + " " + oidB + " refs/keep-around/" + oidA + "\n" + oidA + " " + oidB + " refs/tmp/x\n" + oidA + " " + oidB + " refs/environments/prod\n",
			expected: nil,
		},
		{
			desc:     "symbolic HEAD is skipped",
			input:    oidA + " " + oidB + " HEAD\n",
			expected: nil,
		},
		{
			desc:     "no-op updates are skipped",
			input:    oidA + " " + oidA + " refs/heads/main\n",
			expected: nil,
		},
		{
			desc:     "malformed lines are skipped",
			input:    "garbage\n\n" + oidA + " " + oidB + " refs/heads/ok\nextra fields here now\n",
			expected: []Update{{Reference: "refs/heads/ok", OldOID: oidA, NewOID: oidB}},
		},
		{
			desc: "references with Unicode whitespace are kept verbatim",
			input: zeroOID + " " + oidA + " refs/heads/a\u00a0b\n" +
				oidA + " " + oidB + " refs/heads/x\u0085y\n" +
				oidB + " " + oidC + " refs/tags/p\u2000q\u3000\n",
			expected: []Update{
				{Reference: "refs/heads/a\u00a0b", OldOID: zeroOID, NewOID: oidA},
				{Reference: "refs/heads/x\u0085y", OldOID: oidA, NewOID: oidB},
				{Reference: "refs/tags/p\u2000q\u3000", OldOID: oidB, NewOID: oidC},
			},
		},
		{
			desc:     "references that are not valid UTF-8 are kept verbatim",
			input:    zeroOID + " " + oidA + " refs/heads/caf\xe9\n",
			expected: []Update{{Reference: "refs/heads/caf\xe9", OldOID: zeroOID, NewOID: oidA}},
		},
		{
			desc:     "a trailing carriage return is ignored",
			input:    zeroOID + " " + oidA + " refs/heads/main\r\n",
			expected: []Update{{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidA}},
		},
		{
			desc:     "lines not separated by single spaces are skipped",
			input:    zeroOID + "\t" + oidA + "\trefs/heads/tabs\n" + zeroOID + "  " + oidA + " refs/heads/double\n" + " " + zeroOID + " " + oidA + " refs/heads/leading\n",
			expected: nil,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			require.Equal(t, tc.expected, ParseUpdates([]byte(tc.input)))
		})
	}
}

func TestAppend_referencesThatAreNotValidUTF8AreStoredLosslessly(t *testing.T) {
	t.Parallel()

	repoPath := newRepoPath(t)

	updates := []Update{
		{Reference: "refs/heads/caf\xe9", OldOID: zeroOID, NewOID: oidA},
		{Reference: "refs/heads/caf\xe8", OldOID: zeroOID, NewOID: oidB},
		{Reference: "refs/heads/caf\u00e9", OldOID: zeroOID, NewOID: oidC},
	}

	appended, err := Append(repoPath, Meta{Operation: "op\xff", Actor: "actor\xfe", RecordedAt: time.Unix(1, 0)}, updates)
	require.NoError(t, err)

	// Read the entries back from the file, not from anything cached.
	forgetState(repoPath)
	entries, err := List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Equal(t, appended, entries, "the recorded entries must be the returned ones")
	require.Len(t, entries, len(updates))

	for i, update := range updates {
		require.Equal(t, update.Reference, entries[i].Reference, "entry %d", i)
		require.Equal(t, "op\ufffd", entries[i].Operation)
		require.Equal(t, "actor\ufffd", entries[i].Actor)
	}

	// The line stays readable, with a lossy reference, and carries the exact one
	// in base64.
	lines := readLines(t, repoPath)
	require.Len(t, lines, len(updates))

	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &fields))
	require.Equal(t, "refs/heads/caf\ufffd", fields["reference"])
	require.Equal(t, "cmVmcy9oZWFkcy9jYWbp", fields["reference_b64"])

	var validFields map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &validFields))
	require.Equal(t, "refs/heads/caf\u00e9", validFields["reference"])
	require.NotContains(t, validFields, "reference_b64", "a valid UTF-8 reference is stored as is")

	// Appends after a rescan of the file continue the sequence.
	forgetState(repoPath)
	more, err := Append(repoPath, Meta{Operation: "op"}, []Update{{Reference: "refs/tags/\xff", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)
	require.Equal(t, uint64(4), more[0].Sequence)
}

func TestEntry_invalidReferenceBase64(t *testing.T) {
	t.Parallel()

	var entry Entry
	require.Error(t, json.Unmarshal([]byte(`{"sequence":1,"reference":"x","reference_b64":"!!"}`), &entry))
}

// dropState drops the cached state of the log at repoPath, as if the process
// had restarted.
func dropState(repoPath string) {
	statesMu.Lock()
	defer statesMu.Unlock()

	delete(states, Path(repoPath))
}

// This test replaces syncDir, so it must not run in parallel with other tests.
func TestAppend_syncsDirectoriesUntilAnAppendSucceeds(t *testing.T) {
	var (
		mu      sync.Mutex
		synced  []string
		failing bool
	)
	originalSyncDir := syncDir
	t.Cleanup(func() { syncDir = originalSyncDir })
	syncDir = func(dir string) error {
		mu.Lock()
		defer mu.Unlock()

		if failing {
			return errors.New("injected sync failure")
		}
		synced = append(synced, dir)
		return originalSyncDir(dir)
	}
	takeSynced := func() []string {
		mu.Lock()
		defer mu.Unlock()

		result := synced
		synced = nil
		return result
	}
	setFailing := func(value bool) {
		mu.Lock()
		defer mu.Unlock()
		failing = value
	}

	update := []Update{{Reference: "refs/heads/main", OldOID: zeroOID, NewOID: oidA}}

	t.Run("a new log syncs its directory and the repository", func(t *testing.T) {
		repoPath := newRepoPath(t)
		graftDir := filepath.Dir(Path(repoPath))

		_, err := Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		require.Equal(t, []string{graftDir, repoPath}, takeSynced())

		_, err = Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		require.Empty(t, takeSynced(), "the directories are synced once")
	})

	t.Run("a failed first append does not disable the sync", func(t *testing.T) {
		repoPath := newRepoPath(t)
		graftDir := filepath.Dir(Path(repoPath))

		setFailing(true)
		_, err := Append(repoPath, Meta{}, update)
		setFailing(false)
		require.ErrorContains(t, err, "injected sync failure")

		// The failed append left the log file behind.
		_, err = os.Stat(Path(repoPath))
		require.NoError(t, err)

		// The failed append cut its entry off again: the entry was never durable, since the
		// directories that hold the log were not, so no reader saw it and the next append takes
		// its sequence.
		entries, err := Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		require.Equal(t, uint64(1), entries[0].Sequence, "the entry of the failed append was cut off")
		require.Equal(t, []string{graftDir, repoPath}, takeSynced(), "the directories are synced again")

		_, err = Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		require.Empty(t, takeSynced())
	})

	t.Run("a new process syncs the directories of an existing log", func(t *testing.T) {
		repoPath := newRepoPath(t)
		graftDir := filepath.Dir(Path(repoPath))

		_, err := Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		takeSynced()

		dropState(repoPath)

		_, err = Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		require.Equal(t, []string{graftDir, repoPath}, takeSynced())
	})

	t.Run("a log recreated at the same path is synced", func(t *testing.T) {
		repoPath := newRepoPath(t)
		graftDir := filepath.Dir(Path(repoPath))

		_, err := Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		takeSynced()

		require.NoError(t, os.RemoveAll(graftDir))

		entries, err := Append(repoPath, Meta{}, update)
		require.NoError(t, err)
		require.Equal(t, uint64(1), entries[0].Sequence)
		require.Equal(t, []string{graftDir, repoPath}, takeSynced())
	})
}

func TestSubscribe_notifiedOnAppend(t *testing.T) {
	repoPath := newRepoPath(t)

	wake, unsubscribe := Subscribe(repoPath)
	defer unsubscribe()

	_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	select {
	case <-wake:
	case <-time.After(5 * time.Second):
		t.Fatal("subscriber was not notified")
	}
}

func TestSubscribe_notificationsAreCoalescedAndNonBlocking(t *testing.T) {
	repoPath := newRepoPath(t)

	wake, unsubscribe := Subscribe(repoPath)
	defer unsubscribe()

	// Nobody reads wake in between, yet appends must not block.
	for i := 0; i < 10; i++ {
		_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
		require.NoError(t, err)
	}

	<-wake
	select {
	case <-wake:
		t.Fatal("notifications must be coalesced into one pending value")
	default:
	}
}

func TestSubscribe_unsubscribeStopsNotifications(t *testing.T) {
	repoPath := newRepoPath(t)

	wake, unsubscribe := Subscribe(repoPath)
	unsubscribe()

	_, err := Append(repoPath, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	select {
	case <-wake:
		t.Fatal("unsubscribed channel must not be notified")
	default:
	}
}

func TestSubscribe_otherRepositoriesAreNotNotified(t *testing.T) {
	repoA := newRepoPath(t)
	repoB := newRepoPath(t)

	wake, unsubscribe := Subscribe(repoA)
	defer unsubscribe()

	_, err := Append(repoB, Meta{}, []Update{{Reference: "refs/heads/a", OldOID: zeroOID, NewOID: oidA}})
	require.NoError(t, err)

	select {
	case <-wake:
		t.Fatal("subscriber of another repository was notified")
	default:
	}
}
