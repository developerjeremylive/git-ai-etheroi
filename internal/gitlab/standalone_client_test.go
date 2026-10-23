package gitlab

import (
	"fmt"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/policyeval"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

const (
	testOldOID = "1111111111111111111111111111111111111111"
	testNewOID = "2222222222222222222222222222222222222222"
)

// changesFor builds a change set in the "<old> <new> <ref>" per-line format.
func changesFor(refs ...string) string {
	out := ""
	for _, ref := range refs {
		out += testOldOID + " " + testNewOID + " " + ref + "\n"
	}
	return out
}

func TestStandaloneClient_Allowed(t *testing.T) {
	t.Parallel()

	denyMain := config.GraftPolicyRule{Actor: "*", Refs: []string{"refs/heads/main"}, Allow: false}
	allowAll := config.GraftPolicyRule{Actor: "*", Allow: true}

	for _, tc := range []struct {
		desc         string
		policy       []config.GraftPolicyRule
		actor        string
		changes      string
		expectAllow  bool
		expectMsg    string
		expectErrMsg string
	}{
		{
			desc:        "empty policy allows every update",
			policy:      nil,
			actor:       "user-1",
			changes:     changesFor("refs/heads/main"),
			expectAllow: true,
		},
		{
			desc:        "empty policy allows an empty change set",
			policy:      []config.GraftPolicyRule{},
			actor:       "user-1",
			changes:     "",
			expectAllow: true,
		},
		{
			desc:        "empty policy ignores malformed changes",
			policy:      nil,
			actor:       "user-1",
			changes:     "garbage",
			expectAllow: true,
		},
		{
			desc:        "wildcard actor and empty refs allow",
			policy:      []config.GraftPolicyRule{allowAll},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/main", "refs/tags/v1"),
			expectAllow: true,
		},
		{
			desc:        "empty actor string in rule matches everybody",
			policy:      []config.GraftPolicyRule{{Actor: "", Allow: true}},
			actor:       "anything",
			changes:     changesFor("refs/heads/dev"),
			expectAllow: true,
		},
		{
			desc:        "exact actor match allows",
			policy:      []config.GraftPolicyRule{{Actor: "user-1", Allow: true}},
			actor:       "user-1",
			changes:     changesFor("refs/heads/dev"),
			expectAllow: true,
		},
		{
			desc:      "actor without a matching rule is denied",
			policy:    []config.GraftPolicyRule{{Actor: "user-1", Allow: true}},
			actor:     "user-2",
			changes:   changesFor("refs/heads/dev"),
			expectMsg: `policy denies "user-2" to update "refs/heads/dev": no matching rule`,
		},
		{
			desc:        "actor glob matches",
			policy:      []config.GraftPolicyRule{{Actor: "agent-*", Allow: true}},
			actor:       "agent-7",
			changes:     changesFor("refs/heads/dev"),
			expectAllow: true,
		},
		{
			desc:      "actor glob does not match a different prefix",
			policy:    []config.GraftPolicyRule{{Actor: "agent-*", Allow: true}},
			actor:     "human-7",
			changes:   changesFor("refs/heads/dev"),
			expectMsg: `policy denies "human-7" to update "refs/heads/dev": no matching rule`,
		},
		{
			desc:      "actor glob does not cross a slash",
			policy:    []config.GraftPolicyRule{{Actor: "agent-*", Allow: true}},
			actor:     "agent-7/x",
			changes:   changesFor("refs/heads/dev"),
			expectMsg: `policy denies "agent-7/x" to update "refs/heads/dev": no matching rule`,
		},
		{
			desc:      "empty GLID does not match a named actor",
			policy:    []config.GraftPolicyRule{{Actor: "user-1", Allow: true}},
			actor:     "",
			changes:   changesFor("refs/heads/dev"),
			expectMsg: `policy denies "" to update "refs/heads/dev": no matching rule`,
		},
		{
			desc:        "empty GLID matches a wildcard actor",
			policy:      []config.GraftPolicyRule{{Actor: "*", Allow: true}},
			actor:       "",
			changes:     changesFor("refs/heads/dev"),
			expectAllow: true,
		},
		{
			desc:      "protected branch denied by first rule",
			policy:    []config.GraftPolicyRule{denyMain, allowAll},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/main"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/main" (rule 1)`,
		},
		{
			desc:        "other branch allowed by second rule",
			policy:      []config.GraftPolicyRule{denyMain, allowAll},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/dev"),
			expectAllow: true,
		},
		{
			desc:      "deny precedence: a change touching a denied and an allowed ref is denied",
			policy:    []config.GraftPolicyRule{denyMain, allowAll},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/dev", "refs/heads/main"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/main" (rule 1)`,
		},
		{
			desc:      "deny precedence: denied ref first in the change set",
			policy:    []config.GraftPolicyRule{denyMain, allowAll},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/main", "refs/heads/dev"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/main" (rule 1)`,
		},
		{
			desc: "first matching rule decides even when a later rule would allow",
			policy: []config.GraftPolicyRule{
				{Actor: "*", Allow: false, Refs: []string{"refs/heads/dev"}},
				{Actor: "*", Allow: true},
			},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/dev"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/dev" (rule 1)`,
		},
		{
			desc: "first matching allow rule wins over a later deny",
			policy: []config.GraftPolicyRule{
				{Actor: "*", Allow: true, Refs: []string{"refs/heads/main"}},
				{Actor: "*", Allow: false},
			},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/main"),
			expectAllow: true,
		},
		{
			desc: "rule for another actor is skipped",
			policy: []config.GraftPolicyRule{
				{Actor: "admin", Allow: true},
				{Actor: "*", Allow: false},
			},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/dev"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/dev" (rule 2)`,
		},
		{
			desc:        "single-star ref glob matches one segment",
			policy:      []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/*"}, Allow: true}},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/feature"),
			expectAllow: true,
		},
		{
			desc:      "single-star ref glob does not match nested segments",
			policy:    []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/*"}, Allow: true}},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/a/b"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/a/b": no matching rule`,
		},
		{
			desc:        "trailing /** matches any depth",
			policy:      []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/**"}, Allow: true}},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/a/b/c"),
			expectAllow: true,
		},
		{
			desc:      "trailing /** does not match other namespaces",
			policy:    []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/**"}, Allow: true}},
			actor:     "agent-a",
			changes:   changesFor("refs/tags/v1"),
			expectMsg: `policy denies "agent-a" to update "refs/tags/v1": no matching rule`,
		},
		{
			desc:      "trailing /** does not match the namespace itself",
			policy:    []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/**"}, Allow: true}},
			actor:     "agent-a",
			changes:   changesFor("refs/heads"),
			expectMsg: `policy denies "agent-a" to update "refs/heads": no matching rule`,
		},
		{
			desc:        "trailing /** with a glob prefix",
			policy:      []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/feat-*/**"}, Allow: true}},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/feat-login/x/y"),
			expectAllow: true,
		},
		{
			desc:      "trailing /** with a glob prefix does not match a different prefix",
			policy:    []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/feat-*/**"}, Allow: true}},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/fix/x"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/fix/x": no matching rule`,
		},
		{
			desc:      "trailing /** with a glob prefix does not match the directory itself",
			policy:    []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/feat-*/**"}, Allow: true}},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/feat-login"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/feat-login": no matching rule`,
		},
		{
			desc:        "bare ** matches every reference",
			policy:      []config.GraftPolicyRule{{Actor: "*", Refs: []string{"**"}, Allow: true}},
			actor:       "agent-a",
			changes:     changesFor("refs/tags/v1", "refs/heads/a/b"),
			expectAllow: true,
		},
		{
			desc: "multiple globs in one rule: any match applies",
			policy: []config.GraftPolicyRule{
				{Actor: "*", Refs: []string{"refs/tags/*", "refs/heads/dev"}, Allow: true},
			},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/dev", "refs/tags/v2"),
			expectAllow: true,
		},
		{
			desc: "multiple globs in one rule: no match falls through",
			policy: []config.GraftPolicyRule{
				{Actor: "*", Refs: []string{"refs/tags/*", "refs/heads/dev"}, Allow: true},
			},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/main"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/main": no matching rule`,
		},
		{
			desc:        "all refs allowed by different rules",
			policy:      []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/dev"}, Allow: true}, {Actor: "*", Refs: []string{"refs/tags/*"}, Allow: true}},
			actor:       "agent-a",
			changes:     changesFor("refs/heads/dev", "refs/tags/v1"),
			expectAllow: true,
		},
		{
			desc:      "change set with one unmatched ref is denied by default",
			policy:    []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/dev"}, Allow: true}},
			actor:     "agent-a",
			changes:   changesFor("refs/heads/dev", "refs/heads/other"),
			expectMsg: `policy denies "agent-a" to update "refs/heads/other": no matching rule`,
		},
		{
			desc:        "blank lines and surrounding whitespace are ignored",
			policy:      []config.GraftPolicyRule{allowAll},
			actor:       "agent-a",
			changes:     "\n  " + changesFor("refs/heads/dev") + "\n\n",
			expectAllow: true,
		},
		{
			desc:        "empty change set with policy is allowed",
			policy:      []config.GraftPolicyRule{denyMain},
			actor:       "agent-a",
			changes:     "",
			expectAllow: true,
		},
		{
			desc:      "ref deletion is subject to the policy",
			policy:    []config.GraftPolicyRule{denyMain, allowAll},
			actor:     "agent-a",
			changes:   testOldOID + " 0000000000000000000000000000000000000000 refs/heads/main\n",
			expectMsg: `policy denies "agent-a" to update "refs/heads/main" (rule 1)`,
		},
		{
			desc:         "malformed change line is an error",
			policy:       []config.GraftPolicyRule{allowAll},
			actor:        "agent-a",
			changes:      "refs/heads/main\n",
			expectErrMsg: `malformed change "refs/heads/main": expected "<old> <new> <ref>"`,
		},
		{
			desc:         "change line with too many fields is an error",
			policy:       []config.GraftPolicyRule{allowAll},
			actor:        "agent-a",
			changes:      testOldOID + " " + testNewOID + " refs/heads/main extra\n",
			expectErrMsg: `malformed change`,
		},
		{
			desc:         "invalid actor glob is an error",
			policy:       []config.GraftPolicyRule{{Actor: "[", Allow: true}},
			actor:        "agent-a",
			changes:      changesFor("refs/heads/dev"),
			expectErrMsg: `policy actor "[": syntax error in pattern`,
		},
		{
			desc:         "invalid ref glob is an error",
			policy:       []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/["}, Allow: true}},
			actor:        "agent-a",
			changes:      changesFor("refs/heads/dev"),
			expectErrMsg: `policy ref "refs/heads/[": syntax error in pattern`,
		},
		{
			desc:         "invalid prefix of a trailing /** glob is an error",
			policy:       []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/[/**"}, Allow: true}},
			actor:        "agent-a",
			changes:      changesFor("refs/heads/dev"),
			expectErrMsg: `policy ref "refs/heads/[/**": syntax error in pattern`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			client := NewStandaloneClient(config.Graft{Policy: tc.policy}, testhelper.SharedLogger(t))

			allowed, msg, err := client.Allowed(testhelper.Context(t), AllowedParams{
				GLID:    tc.actor,
				Changes: tc.changes,
			})

			if tc.expectErrMsg != "" {
				require.ErrorContains(t, err, tc.expectErrMsg)
				require.False(t, allowed)
				require.Empty(t, msg)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expectAllow, allowed)
			if tc.expectAllow {
				require.Empty(t, msg)
			} else {
				require.Equal(t, tc.expectMsg, msg)
			}
		})
	}
}

func TestStandaloneClient_Allowed_IgnoresOtherParams(t *testing.T) {
	t.Parallel()

	client := NewStandaloneClient(config.Graft{Policy: []config.GraftPolicyRule{
		{Actor: "user-1", Allow: true, Refs: []string{"refs/heads/dev"}},
	}}, testhelper.SharedLogger(t))

	// Repository, protocol, push options and client context do not influence the policy.
	allowed, msg, err := client.Allowed(testhelper.Context(t), AllowedParams{
		RepoPath:           "/some/where",
		RelativePath:       "@hashed/ab/cd.git",
		GitObjectDirectory: "objects",
		GLRepository:       "graft:default/repo.git",
		GLID:               "user-1",
		GLProtocol:         "ssh",
		Changes:            changesFor("refs/heads/dev"),
		PushOptions:        []string{"ci.skip"},
		ClientContext:      []byte("ctx"),
	})
	require.NoError(t, err)
	require.True(t, allowed)
	require.Empty(t, msg)
}

func TestStandaloneClient_Allowed_DoesNotShareConfigPolicy(t *testing.T) {
	t.Parallel()

	policy := []config.GraftPolicyRule{{Actor: "*", Allow: false}}
	client := NewStandaloneClient(config.Graft{Policy: policy}, testhelper.SharedLogger(t))

	// Mutating the caller's slice after construction must not change the policy.
	policy[0].Allow = true

	allowed, _, err := client.Allowed(testhelper.Context(t), AllowedParams{
		GLID:    "user-1",
		Changes: changesFor("refs/heads/dev"),
	})
	require.NoError(t, err)
	require.False(t, allowed)
}

func TestStandaloneClient_Allowed_Concurrent(t *testing.T) {
	t.Parallel()

	client := NewStandaloneClient(config.Graft{Policy: []config.GraftPolicyRule{
		{Actor: "admin", Allow: true},
		{Actor: "*", Refs: []string{"refs/heads/main"}, Allow: false},
		{Actor: "*", Refs: []string{"refs/heads/**"}, Allow: true},
	}}, testhelper.SharedLogger(t))

	const goroutines = 64

	ctx := testhelper.Context(t)

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Admin may update main; agents may update other branches but not main.
			allowed, _, err := client.Allowed(ctx, AllowedParams{GLID: "admin", Changes: changesFor("refs/heads/main")})
			if err != nil || !allowed {
				t.Errorf("goroutine %d: admin on main: allowed=%v err=%v", i, allowed, err)
			}

			allowed, msg, err := client.Allowed(ctx, AllowedParams{GLID: fmt.Sprintf("agent-%d", i), Changes: changesFor("refs/heads/dev", "refs/heads/main")})
			if err != nil || allowed || msg == "" {
				t.Errorf("goroutine %d: agent on main: allowed=%v msg=%q err=%v", i, allowed, msg, err)
			}

			allowed, _, err = client.Allowed(ctx, AllowedParams{GLID: fmt.Sprintf("agent-%d", i), Changes: changesFor("refs/heads/dev/x")})
			if err != nil || !allowed {
				t.Errorf("goroutine %d: agent on dev: allowed=%v err=%v", i, allowed, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestStandaloneClient_OtherCallbacks(t *testing.T) {
	t.Parallel()

	client := NewStandaloneClient(config.Graft{}, testhelper.SharedLogger(t))
	ctx := testhelper.Context(t)

	t.Run("PreReceive succeeds", func(t *testing.T) {
		ok, err := client.PreReceive(ctx, "graft:default/repo.git")
		require.NoError(t, err)
		require.True(t, ok)
	})

	t.Run("PostReceive succeeds without messages", func(t *testing.T) {
		ok, msgs, err := client.PostReceive(ctx, "graft:default/repo.git", "user-1", changesFor("refs/heads/dev"), nil, "ci.skip")
		require.NoError(t, err)
		require.True(t, ok)
		require.Nil(t, msgs)
	})

	t.Run("ObjectPoolMembers returns an empty, non-nil map", func(t *testing.T) {
		members, err := client.ObjectPoolMembers(ctx, []string{"@pools/ab/cd.git"}, "default", true)
		require.NoError(t, err)
		require.NotNil(t, members)
		require.Empty(t, members)
	})

	t.Run("Check describes standalone mode", func(t *testing.T) {
		info, err := client.Check(ctx)
		require.NoError(t, err)
		require.NotNil(t, info)
		require.Equal(t, "standalone", info.Version)
		require.Equal(t, "none", info.APIVersion)
		require.False(t, info.RedisReachable)
	})
}

// TestStandaloneClient_AllowedDecidesAsEvaluate checks the policy evaluation that the
// reference-transaction hook shares with Allowed (policyeval.Evaluate): the standalone client must
// decide exactly like it.
func TestStandaloneClient_AllowedDecidesAsEvaluate(t *testing.T) {
	t.Parallel()

	policy := []config.GraftPolicyRule{
		{Actor: "planner", Refs: []string{"refs/heads/main"}, Allow: true},
		{Refs: []string{"refs/heads/main"}, Allow: false},
		{Refs: []string{"refs/heads/**"}, Allow: true},
	}
	client := NewStandaloneClient(config.Graft{Policy: policy}, testhelper.SharedLogger(t))

	for _, tc := range []struct {
		desc    string
		policy  []config.GraftPolicyRule
		actor   string
		changes string
		allowed bool
		message string
		err     string
	}{
		{desc: "empty policy allows", actor: "builder", changes: changesFor("refs/heads/main"), allowed: true},
		{desc: "privileged actor allowed", policy: policy, actor: "planner", changes: changesFor("refs/heads/main"), allowed: true},
		{desc: "open branch allowed", policy: policy, actor: "builder", changes: changesFor("refs/heads/feature"), allowed: true},
		{desc: "protected branch denied", policy: policy, actor: "builder", changes: changesFor("refs/heads/feature", "refs/heads/main"), message: `policy denies "builder" to update "refs/heads/main" (rule 2)`},
		{desc: "ungoverned reference denied", policy: policy, actor: "builder", changes: changesFor("refs/tags/v1"), message: `policy denies "builder" to update "refs/tags/v1": no matching rule`},
		{desc: "malformed changes", policy: policy, actor: "builder", changes: "only two\n", err: "malformed change"},
		{desc: "reference with Unicode whitespace allowed by its name", policy: policy, actor: "builder", changes: changesFor("refs/heads/a\u00a0b", "refs/heads/x\u0085y"), allowed: true},
		{desc: "reference with Unicode whitespace denied by its name", policy: policy, actor: "builder", changes: changesFor("refs/tags/v\u2000main"), message: `policy denies "builder" to update "refs/tags/v\u2000main": no matching rule`},
		{desc: "reference that is not valid UTF-8 allowed by its name", policy: policy, actor: "builder", changes: changesFor("refs/heads/caf\xe9"), allowed: true},
		{desc: "tab-separated change is malformed", policy: policy, actor: "builder", changes: testOldOID + "\t" + testNewOID + "\trefs/heads/feature\n", err: "malformed change"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			allowed, message, err := policyeval.Evaluate(tc.policy, tc.actor, tc.changes)
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.allowed, allowed)
			require.Equal(t, tc.message, message)

			if tc.policy != nil {
				clientAllowed, clientMessage, err := client.Allowed(testhelper.Context(t), AllowedParams{GLID: tc.actor, Changes: tc.changes})
				require.NoError(t, err)
				require.Equal(t, allowed, clientAllowed)
				require.Equal(t, message, clientMessage)
			}
		})
	}
}
