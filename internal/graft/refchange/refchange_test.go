package refchange

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	oidA        = "1111111111111111111111111111111111111111"
	oidB        = "2222222222222222222222222222222222222222"
	fuzzZeroOID = "0000000000000000000000000000000000000000"
	fuzzOID     = "1111111111111111111111111111111111111111"
)

func TestParseHookLine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc      string
		line      string
		reference string
		ok        bool
	}{
		{desc: "update", line: oidA + " " + oidB + " refs/heads/main", reference: "refs/heads/main", ok: true},
		{desc: "symbolic values", line: "ref:refs/heads/a ref:refs/heads/b HEAD", reference: "HEAD", ok: true},
		{desc: "no-break space", line: oidA + " " + oidB + " refs/heads/a\u00a0b", reference: "refs/heads/a\u00a0b", ok: true},
		{desc: "next line", line: oidA + " " + oidB + " refs/heads/x\u0085y", reference: "refs/heads/x\u0085y", ok: true},
		{desc: "trailing Unicode whitespace", line: oidA + " " + oidB + " refs/heads/z\u2028", reference: "refs/heads/z\u2028", ok: true},
		{desc: "invalid UTF-8", line: oidA + " " + oidB + " refs/heads/caf\xe9", reference: "refs/heads/caf\xe9", ok: true},
		{desc: "trailing carriage return", line: oidA + " " + oidB + " refs/heads/main\r", reference: "refs/heads/main", ok: true},
		{desc: "empty", line: ""},
		{desc: "two fields", line: oidA + " " + oidB},
		{desc: "empty reference", line: oidA + " " + oidB + " "},
		{desc: "space in the reference", line: oidA + " " + oidB + " refs/heads/main extra"},
		{desc: "tab separators", line: oidA + "\t" + oidB + "\trefs/heads/main"},
		{desc: "control character in the reference", line: oidA + " " + oidB + " refs/heads/a\x01b"},
		{desc: "DEL in the reference", line: oidA + " " + oidB + " refs/heads/a\x7fb"},
		{desc: "leading space", line: " " + oidA + " " + oidB + " refs/heads/main"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			_, _, reference, ok := ParseHookLine(tc.line)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.reference, reference)
		})
	}
}

// FuzzParseHookLine fuzzes the parser of the reference-transaction hook lines.
// An accepted line must be exactly the three fields joined by single spaces.
func FuzzParseHookLine(f *testing.F) {
	for _, seed := range []string{
		fuzzZeroOID + " " + fuzzOID + " refs/heads/main",
		fuzzOID + " " + fuzzZeroOID + " refs/tags/v1.0.0",
		fuzzOID + " " + fuzzOID + " refs/heads/main\r",
		"refs/heads/a b",
		fuzzOID + " " + fuzzOID + " refs/heads/café/\xff\xfe",
		fuzzOID + " " + fuzzOID + " refs/heads/with space",
		"ref:refs/heads/main",
		fuzzOID + " " + fuzzOID,
		"  ",
		"",
		fuzzOID + "\t" + fuzzOID + " refs/heads/main",
		fuzzOID + " " + fuzzOID + " ",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, line string) {
		oldValue, newValue, reference, ok := ParseHookLine(line)

		if !ok {
			if oldValue != "" || newValue != "" || reference != "" {
				t.Fatalf("rejected line returned fields: %q %q %q", oldValue, newValue, reference)
			}
			return
		}

		if oldValue == "" || newValue == "" || reference == "" {
			t.Fatalf("accepted line with an empty field: %q %q %q", oldValue, newValue, reference)
		}

		for _, field := range []string{oldValue, newValue} {
			if strings.ContainsFunc(field, isASCIISpaceOrControl) {
				t.Fatalf("accepted object ID with an ASCII space or control character: %q", field)
			}
		}

		if got, want := oldValue+" "+newValue+" "+reference, strings.TrimSuffix(line, "\r"); got != want {
			t.Fatalf("fields do not reassemble the line: got %q, want %q", got, want)
		}

		o2, n2, r2, ok2 := ParseHookLine(line)
		if o2 != oldValue || n2 != newValue || r2 != reference || ok2 != ok {
			t.Fatal("non-deterministic parse")
		}
	})
}
