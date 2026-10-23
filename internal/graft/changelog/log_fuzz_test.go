package changelog

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/feder-cr/git-ai/internal/graft/refchange"
)

const fuzzZeroOID = "0000000000000000000000000000000000000000"
const fuzzOID = "1111111111111111111111111111111111111111"

// FuzzParseUpdates fuzzes the parser of the hook input, which holds one line per
// reference update. Only branches and tags are returned, and no update is a
// no-op.
func FuzzParseUpdates(f *testing.F) {
	for _, seed := range []string{
		"",
		fuzzZeroOID + " " + fuzzOID + " refs/heads/main\n",
		fuzzZeroOID + " " + fuzzOID + " refs/heads/main\n" + fuzzOID + " " + fuzzZeroOID + " refs/tags/v1\n",
		fuzzOID + " " + fuzzOID + " refs/heads/main\n",
		fuzzOID + " " + fuzzOID + " refs/keep-around/" + fuzzOID + "\n",
		"ref:refs/heads/main\n" + fuzzOID + " " + fuzzOID + " HEAD\n",
		fuzzOID + " " + fuzzZeroOID + " refs/heads/a\r\n\r\n",
		fuzzOID + " " + fuzzOID + " refs/heads/\xff\n",
		"\n\n\n",
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, input []byte) {
		updates := ParseUpdates(input)

		for _, u := range updates {
			if !strings.HasPrefix(u.Reference, "refs/heads/") && !strings.HasPrefix(u.Reference, "refs/tags/") {
				t.Fatalf("update of a reference that is not recorded: %q", u.Reference)
			}

			if u.OldOID == u.NewOID && !IsZeroOID(u.NewOID) {
				t.Fatalf("no-op update returned: %+v", u)
			}

			line := u.OldOID + " " + u.NewOID + " " + u.Reference
			if _, _, _, ok := refchange.ParseHookLine(line); !ok {
				t.Fatalf("returned update does not reassemble into a valid line: %+v", u)
			}
		}

		if again := ParseUpdates(input); !reflect.DeepEqual(updates, again) {
			t.Fatal("non-deterministic updates")
		}
	})
}

// FuzzEntryJSON fuzzes the decoding of a changeset log line. Any line that
// decodes must encode again and decode to the same entry, including references
// that are not valid UTF-8.
func FuzzEntryJSON(f *testing.F) {
	for _, seed := range []string{
		`{"sequence":1,"reference":"refs/heads/main","old_oid":"` + fuzzZeroOID + `","new_oid":"` + fuzzOID + `",` +
			`"operation":"ssh_receive_pack","actor":"user-1","recorded_at":"2026-01-02T03:04:05Z"}`,
		`{"sequence":2,"reference":"refs/heads/x","reference_b64":"/w==","old_oid":"` + fuzzOID + `","new_oid":"` + fuzzZeroOID + `",` +
			`"operation":"land_batch","actor":"agent-7","rewritten":true,"recorded_at":"2026-01-02T03:04:05+02:00"}`,
		`{"sequence":3,"reference":"refs/heads/y","reference_b64":"!!!","recorded_at":"2026-01-02T03:04:05Z"}`,
		`{"sequence":4,"reference":"refs/tags/v1","members":[],"members_truncated":true,"recorded_at":"2026-01-02T03:04:05Z"}`,
		`{}`,
		`null`,
		`[]`,
	} {
		f.Add([]byte(seed))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var entry Entry
		if err := json.Unmarshal(data, &entry); err != nil {
			return
		}

		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encoding a decoded entry: %v", err)
		}

		var again Entry
		if err := json.Unmarshal(encoded, &again); err != nil {
			t.Fatalf("decoding the encoding %s: %v", encoded, err)
		}

		if again.Reference != entry.Reference {
			t.Fatalf("reference changed: %q != %q", again.Reference, entry.Reference)
		}

		if again.Sequence != entry.Sequence || again.OldOID != entry.OldOID ||
			again.NewOID != entry.NewOID || again.Operation != entry.Operation ||
			again.Rewritten != entry.Rewritten {
			t.Fatalf("fields changed in round trip: %+v != %+v", again, entry)
		}

		if utf8.ValidString(entry.Actor) && again.Actor != entry.Actor {
			t.Fatalf("actor changed: %q != %q", again.Actor, entry.Actor)
		}

		if !again.RecordedAt.Equal(entry.RecordedAt) {
			t.Fatalf("recorded_at changed: %v != %v", again.RecordedAt, entry.RecordedAt)
		}

		if again.MembersTruncated != entry.MembersTruncated {
			t.Fatalf("members_truncated changed: %v != %v", again.MembersTruncated, entry.MembersTruncated)
		}

		if len(again.Members) != len(entry.Members) {
			t.Fatalf("member count changed: %d != %d", len(again.Members), len(entry.Members))
		}

		for i := range entry.Members {
			if !equalFuzzMember(again.Members[i], entry.Members[i]) {
				t.Fatalf("member %d changed: %+v != %+v", i, again.Members[i], entry.Members[i])
			}
		}
	})
}

// equalFuzzMember compares two members field by field. A nil and an empty list
// of resolved paths are the same member, as the omitempty encoding makes them.
func equalFuzzMember(a, b Member) bool {
	if a.OriginalCommit != b.OriginalCommit || a.Commit != b.Commit ||
		a.Actor != b.Actor || a.Rewritten != b.Rewritten ||
		len(a.ResolvedPaths) != len(b.ResolvedPaths) {
		return false
	}

	for i := range a.ResolvedPaths {
		if a.ResolvedPaths[i] != b.ResolvedPaths[i] {
			return false
		}
	}

	return true
}

// FuzzEntryRoundTrip fuzzes the encoding of entries built from arbitrary field
// values. A reference that is not valid UTF-8 must survive the round trip byte
// for byte, through the reference_b64 field.
func FuzzEntryRoundTrip(f *testing.F) {
	f.Add(uint64(1), "refs/heads/main", fuzzZeroOID, fuzzOID, "ssh_receive_pack", "user-1", true)
	f.Add(uint64(2), "refs/heads/\xff\xfe", fuzzOID, fuzzOID, "land_batch", "agent-é", false)
	f.Add(uint64(0), "", "", "", "", "", false)

	f.Fuzz(func(t *testing.T, sequence uint64, reference, oldOID, newOID, operation, actor string, rewritten bool) {
		entry := Entry{
			Sequence:   sequence,
			Reference:  reference,
			OldOID:     oldOID,
			NewOID:     newOID,
			Operation:  operation,
			Actor:      actor,
			RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Rewritten:  rewritten,
		}

		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("encoding: %v", err)
		}

		var decoded Entry
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("decoding %s: %v", encoded, err)
		}

		if decoded.Reference != reference {
			t.Fatalf("reference not preserved: %q != %q in %s", decoded.Reference, reference, encoded)
		}

		if decoded.Sequence != sequence || decoded.Rewritten != rewritten {
			t.Fatalf("fields not preserved: %+v", decoded)
		}

		// Object IDs and operations are ASCII by contract. encoding/json replaces
		// invalid UTF-8 in them, so they are only compared when valid.
		for _, pair := range [][2]string{{decoded.OldOID, oldOID}, {decoded.NewOID, newOID}, {decoded.Operation, operation}} {
			if utf8.ValidString(pair[1]) && pair[0] != pair[1] {
				t.Fatalf("field not preserved: %q != %q", pair[0], pair[1])
			}
		}

		if utf8.ValidString(actor) && decoded.Actor != actor {
			t.Fatalf("actor not preserved: %q != %q", decoded.Actor, actor)
		}
	})
}
