package policyeval

import (
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/stretchr/testify/require"
)

const (
	testOldOID    = "1111111111111111111111111111111111111111"
	testNewOID    = "2222222222222222222222222222222222222222"
	policyTestOld = "0000000000000000000000000000000000000000"
	policyTestNew = "2222222222222222222222222222222222222222"
)

// changesFor builds a change set in the "<old> <new> <ref>" per-line format.
func changesFor(refs ...string) string {
	out := ""
	for _, ref := range refs {
		out += testOldOID + " " + testNewOID + " " + ref + "\n"
	}
	return out
}

func policyTestChanges(refs ...string) string {
	var b strings.Builder
	for _, ref := range refs {
		fmt.Fprintf(&b, "%s %s %s\n", policyTestOld, policyTestNew, ref)
	}
	return b.String()
}

func TestMatchRef(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		glob    string
		ref     string
		matches bool
		err     bool
	}{
		{glob: "refs/heads/main", ref: "refs/heads/main", matches: true},
		{glob: "refs/heads/main", ref: "refs/heads/dev", matches: false},
		{glob: "refs/heads/*", ref: "refs/heads/main", matches: true},
		{glob: "refs/heads/*", ref: "refs/heads/a/main", matches: false},
		{glob: "refs/heads/**", ref: "refs/heads/main", matches: true},
		{glob: "refs/heads/**", ref: "refs/heads/a/b/c", matches: true},
		{glob: "refs/heads/**", ref: "refs/heads", matches: false},
		{glob: "refs/heads/**", ref: "refs/tags/v1", matches: false},
		{glob: "refs/heads/**", ref: "refs/headsx/main", matches: false},
		{glob: "refs/*/**", ref: "refs/tags/v1/x", matches: true},
		{glob: "refs/*/**", ref: "refs/tags/v1", matches: true},
		{glob: "**", ref: "refs/heads/main", matches: true},
		{glob: "**", ref: "main", matches: true},
		{glob: "refs/heads/release**", ref: "refs/heads/release/1.0", matches: true},
		{glob: "refs/heads/release**", ref: "refs/heads/release-2", matches: true},
		{glob: "refs/heads/release**", ref: "refs/heads/release", matches: true},
		{glob: "refs/heads/release**", ref: "refs/heads/main", matches: false},
		{glob: "refs/*/rel**", ref: "refs/heads/rel/a/b", matches: true},
		{glob: "refs/heads/[**", ref: "refs/heads/main", err: true},
		{glob: "refs/heads/[", ref: "refs/heads/main", err: true},
		{glob: "refs/heads/[/**", ref: "refs/heads/main", err: true},
		{glob: "refs/heads/[/**", ref: "x", err: true},
	} {
		t.Run(fmt.Sprintf("%s:%s", tc.glob, tc.ref), func(t *testing.T) {
			t.Parallel()

			matches, err := matchRef(tc.glob, tc.ref)
			if tc.err {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.matches, matches)
		})
	}
}

func TestParseChangedRefs(t *testing.T) {
	t.Parallel()

	refs, err := parseChangedRefs("")
	require.NoError(t, err)
	require.Empty(t, refs)

	refs, err = parseChangedRefs(changesFor("refs/heads/a", "refs/tags/b"))
	require.NoError(t, err)
	require.Equal(t, []string{"refs/heads/a", "refs/tags/b"}, refs)

	_, err = parseChangedRefs("only two\n")
	require.Error(t, err)
}

// TestEvaluatePolicy_HEAD checks that HEAD is decided by the policy like any
// other reference: it is allowed only by a rule that names it.
func TestEvaluate_HEAD(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc    string
		policy  []config.GraftPolicyRule
		changes string
		allowed bool
	}{
		{
			desc:    "HEAD is denied when only branch rules exist",
			policy:  []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}},
			changes: policyTestChanges("HEAD"),
			allowed: false,
		},
		{
			desc:    "HEAD is allowed by a rule that names it",
			policy:  []config.GraftPolicyRule{{Refs: []string{"HEAD"}, Allow: true}},
			changes: policyTestChanges("HEAD"),
			allowed: true,
		},
		{
			desc:    "a wildcard rule covers HEAD",
			policy:  []config.GraftPolicyRule{{Allow: true}},
			changes: policyTestChanges("HEAD"),
			allowed: true,
		},
		{
			desc:    "HEAD and a branch are both needed",
			policy:  []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}, {Refs: []string{"HEAD"}, Allow: true}},
			changes: policyTestChanges("refs/heads/main", "HEAD"),
			allowed: true,
		},
		{
			desc:    "one denied HEAD among allowed branches denies the change set",
			policy:  []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}},
			changes: policyTestChanges("refs/heads/main", "HEAD"),
			allowed: false,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			allowed, _, err := Evaluate(tc.policy, "agent-a", tc.changes)
			require.NoError(t, err)
			require.Equal(t, tc.allowed, allowed)
		})
	}
}

// TestEvaluatePolicy_DenialMessages checks the denial messages. They describe
// the policy decision and do not carry the "GitLab:" prefix of GitLab denials.
func TestEvaluate_DenialMessages(t *testing.T) {
	t.Parallel()

	policy := []config.GraftPolicyRule{
		{Actor: "agent-*", Refs: []string{"refs/heads/feature/**"}, Allow: true},
		{Actor: "agent-*", Refs: []string{"refs/heads/main"}, Allow: false},
	}

	for _, tc := range []struct {
		desc        string
		actor       string
		changes     string
		wantMessage string
	}{
		{
			desc:        "explicit deny names the rule",
			actor:       "agent-a",
			changes:     policyTestChanges("refs/heads/main"),
			wantMessage: `policy denies "agent-a" to update "refs/heads/main" (rule 2)`,
		},
		{
			desc:        "no matching rule",
			actor:       "agent-a",
			changes:     policyTestChanges("HEAD"),
			wantMessage: `policy denies "agent-a" to update "HEAD": no matching rule`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			allowed, message, err := Evaluate(policy, tc.actor, tc.changes)
			require.NoError(t, err)
			require.False(t, allowed)
			require.Equal(t, tc.wantMessage, message)
			require.False(t, strings.HasPrefix(message, "GitLab:"))
		})
	}
}

// TestEvaluatePolicy_InvalidInput checks that a malformed change set and an
// invalid glob are reported as errors, not as decisions.
func TestEvaluate_InvalidInput(t *testing.T) {
	t.Parallel()

	t.Run("malformed change", func(t *testing.T) {
		t.Parallel()

		allowed, _, err := Evaluate([]config.GraftPolicyRule{{Allow: true}}, "agent-a", "garbage\n")
		require.ErrorContains(t, err, "malformed change")
		require.False(t, allowed)
	})

	t.Run("invalid ref glob", func(t *testing.T) {
		t.Parallel()

		allowed, _, err := Evaluate([]config.GraftPolicyRule{{Refs: []string{"refs/[heads"}, Allow: true}}, "agent-a", policyTestChanges("HEAD"))
		require.Error(t, err)
		require.False(t, allowed)
	})
}
