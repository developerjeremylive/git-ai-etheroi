package policyeval

import (
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/refchange"
)

// FuzzParseChangedRefs fuzzes the parser of the change set that the standalone
// policy evaluates. Every reference it returns must be a valid hook line field.
func FuzzParseChangedRefs(f *testing.F) {
	for _, seed := range []string{
		"",
		"\n\n",
		fuzzPolicyZeroOID + " " + fuzzPolicyOID + " refs/heads/main\n",
		fuzzPolicyOID + " " + fuzzPolicyOID + " refs/tags/v1.0\r\n",
		fuzzPolicyOID + " " + fuzzPolicyOID + " refs/heads/a b\n",
		fuzzPolicyOID + " " + fuzzPolicyOID + " refs/heads/main extra\n",
		"garbage\n",
		fuzzPolicyOID + " refs/heads/main\n",
		" " + fuzzPolicyOID + " " + fuzzPolicyOID + " refs/heads/main \n",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, changes string) {
		refs, err := parseChangedRefs(changes)
		if err != nil {
			if refs != nil {
				t.Fatalf("refs returned together with error: %v", refs)
			}
			return
		}

		for _, ref := range refs {
			if ref == "" {
				t.Fatal("empty reference returned")
			}

			if _, _, _, ok := refchange.ParseHookLine(fuzzPolicyOID + " " + fuzzPolicyOID + " " + ref); !ok {
				t.Fatalf("reference %q does not form a valid hook line", ref)
			}
		}
	})
}

// FuzzMatchRef fuzzes the reference glob matcher of the standalone policy.
// Literal globs and literal "/**" and trailing "**" globs match exactly the
// references their literal part describes.
func FuzzMatchRef(f *testing.F) {
	for _, seed := range []struct{ glob, ref string }{
		{"refs/heads/main", "refs/heads/main"},
		{"refs/heads/**", "refs/heads/a/b"},
		{"refs/heads/**", "refs/heads"},
		{"refs/heads/release**", "refs/heads/release/1.0"},
		{"refs/heads/*", "refs/heads/a/b"},
		{"**", "refs/tags/v1"},
		{"refs/heads/[", "refs/heads/a"},
		{"refs/heads/é/**", "refs/heads/é/x"},
		{"refs/heads/**", "refs/heads/\xff"},
	} {
		f.Add(seed.glob, seed.ref)
	}

	f.Fuzz(func(t *testing.T, glob, ref string) {
		got, err := matchRef(glob, ref)
		if err != nil {
			if got {
				t.Fatal("matched together with an error")
			}
			return
		}

		if glob == "**" && !got {
			t.Fatalf("bare ** did not match %q", ref)
		}

		if isLiteralRefGlob(glob) && got != (glob == ref) {
			t.Fatalf("literal glob %q against %q = %v", glob, ref, got)
		}

		if prefix, deep := strings.CutSuffix(glob, "/**"); deep && isLiteralRefGlob(prefix) {
			if want := strings.HasPrefix(ref, prefix+"/"); got != want {
				t.Fatalf("deep glob %q against %q = %v, want %v", glob, ref, got, want)
			}
		}

		if loose, ok := strings.CutSuffix(glob, "**"); ok && glob != "**" && !strings.HasSuffix(glob, "/**") && isLiteralRefGlob(loose) {
			if want := strings.HasPrefix(ref, loose); got != want {
				t.Fatalf("loose glob %q against %q = %v, want %v", glob, ref, got, want)
			}
		}
	})
}

// FuzzEvaluate fuzzes the policy decision for a change set. An allowed
// change set must be made of references that the single rule of the policy
// allows, and a denial must come with a message.
func FuzzEvaluate(f *testing.F) {
	f.Add(fuzzPolicyOID+" "+fuzzPolicyOID+" refs/heads/main\n", "agent-1", true, "refs/heads/**")
	f.Add(fuzzPolicyOID+" "+fuzzPolicyOID+" refs/heads/main\n", "agent-1", false, "refs/heads/main")
	f.Add(fuzzPolicyOID+" "+fuzzPolicyOID+" refs/tags/v1\n", "agent-1", true, "refs/heads/**")
	f.Add("", "agent-1", true, "")
	f.Add("garbage", "agent-1", true, "refs/heads/**")
	f.Add(fuzzPolicyOID+" "+fuzzPolicyOID+" refs/heads/x\n", "agent-1", true, "refs/heads/[")

	f.Fuzz(func(t *testing.T, changes, actor string, allow bool, refGlob string) {
		var policy []config.GraftPolicyRule
		if refGlob != "" {
			policy = []config.GraftPolicyRule{{Actor: "*", Refs: []string{refGlob}, Allow: allow}}
		}

		allowed, message, err := Evaluate(policy, actor, changes)
		if err != nil {
			if allowed {
				t.Fatal("allowed together with an error")
			}
			return
		}

		if allowed && message != "" {
			t.Fatalf("allowed with a message %q", message)
		}

		if !allowed && message == "" {
			t.Fatal("denied without a message")
		}

		if len(policy) == 0 {
			if !allowed {
				t.Fatal("an empty policy denied the change")
			}
			return
		}

		refs, err := parseChangedRefs(changes)
		if err != nil {
			t.Fatalf("EvaluatePolicy accepted a change set that parseChangedRefs rejects: %v", err)
		}

		if !allowed {
			return
		}

		for _, ref := range refs {
			matched, err := matchRef(refGlob, ref)
			if err != nil || !matched || !allow {
				t.Fatalf("allowed reference %q that the rule %q (allow=%v) does not allow", ref, refGlob, allow)
			}
		}
	})
}

const (
	fuzzPolicyZeroOID = "0000000000000000000000000000000000000000"
	fuzzPolicyOID     = "1111111111111111111111111111111111111111"
)

// isLiteralRefGlob reports whether glob has no path.Match metacharacter.
func isLiteralRefGlob(glob string) bool {
	return !strings.ContainsAny(glob, `*?[]\`)
}
