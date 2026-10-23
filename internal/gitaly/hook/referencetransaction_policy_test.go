package hook

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	policyOldOID = "0000000000000000000000000000000000000000"
	policyNewOID = "1111111111111111111111111111111111111111"
)

// policyEnv returns the hook environment that carries the changeset log metadata
// of an update performed by actor, and whether actor is an agent.
func policyEnv(t *testing.T, actor string, agent bool) []string {
	t.Helper()

	payload := fmt.Sprintf(`{"graft_changelog":{"actor":%q,"agent":%t}}`, actor, agent)
	return []string{gitcmd.EnvHooksPayload + "=" + base64.StdEncoding.EncodeToString([]byte(payload))}
}

// policyChanges returns the reference-transaction hook input for the given
// references.
func policyChanges(refs ...string) []byte {
	var b strings.Builder
	for _, ref := range refs {
		fmt.Fprintf(&b, "%s %s %s\n", policyOldOID, policyNewOID, ref)
	}
	return []byte(b.String())
}

func graftPolicyManager(t *testing.T, graft config.Graft) *GitLabHookManager {
	t.Helper()

	return &GitLabHookManager{
		cfg:    config.Cfg{Graft: graft},
		logger: testhelper.SharedLogger(t),
	}
}

// TestEnforceGraftPolicy_HEAD checks that the standalone policy governs symbolic
// updates of HEAD made by agents, and not only the refs/ namespace.
func TestEnforceGraftPolicy_HEAD(t *testing.T) {
	t.Parallel()

	allowMain := []config.GraftPolicyRule{
		{Actor: "agent-a", Refs: []string{"refs/heads/**"}, Allow: true},
	}

	for _, tc := range []struct {
		desc     string
		policy   []config.GraftPolicyRule
		refs     []string
		wantCode codes.Code
	}{
		{
			desc:     "agent may not update HEAD when no rule covers it",
			policy:   allowMain,
			refs:     []string{"HEAD"},
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "agent may update HEAD when a rule allows it",
			policy:   []config.GraftPolicyRule{{Actor: "agent-a", Refs: []string{"HEAD"}, Allow: true}},
			refs:     []string{"HEAD"},
			wantCode: codes.OK,
		},
		{
			desc:     "a denied HEAD fails the whole update even with allowed refs",
			policy:   []config.GraftPolicyRule{{Refs: []string{"refs/**"}, Allow: true}},
			refs:     []string{"refs/heads/feature", "HEAD"},
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "a rule that denies HEAD is honoured",
			policy:   []config.GraftPolicyRule{{Actor: "agent-a", Refs: []string{"HEAD"}, Allow: false}, {Allow: true}},
			refs:     []string{"HEAD"},
			wantCode: codes.PermissionDenied,
		},
		{
			desc:     "pseudo references other than HEAD are not governed",
			policy:   []config.GraftPolicyRule{{Allow: false}},
			refs:     []string{"FETCH_HEAD", "ORIG_HEAD"},
			wantCode: codes.OK,
		},
		{
			desc:     "refs are governed as before",
			policy:   allowMain,
			refs:     []string{"refs/heads/main"},
			wantCode: codes.OK,
		},
		{
			desc:     "refs outside the allowed namespace are denied",
			policy:   allowMain,
			refs:     []string{"refs/tags/v1"},
			wantCode: codes.PermissionDenied,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			m := graftPolicyManager(t, config.Graft{Standalone: true, Policy: tc.policy})

			err := m.enforceGraftPolicy(context.Background(), policyEnv(t, "agent-a", true), policyChanges(tc.refs...))
			require.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
			if tc.wantCode == codes.PermissionDenied {
				require.NotContains(t, err.Error(), "GitLab:")
			}
		})
	}
}

// TestEnforceGraftPolicy_OnlyAgentsAndStandalone checks that the policy does
// not restrict callers other than agents, and does nothing outside standalone
// mode or with an empty policy.
func TestEnforceGraftPolicy_OnlyAgentsAndStandalone(t *testing.T) {
	t.Parallel()

	deny := []config.GraftPolicyRule{{Allow: false}}

	for _, tc := range []struct {
		desc  string
		graft config.Graft
		actor string
		agent bool
	}{
		{desc: "shared token is not restricted", graft: config.Graft{Standalone: true, Policy: deny}, actor: "admin", agent: false},
		{desc: "not standalone", graft: config.Graft{Standalone: false, Policy: deny}, actor: "agent-a", agent: true},
		{desc: "empty policy", graft: config.Graft{Standalone: true}, actor: "agent-a", agent: true},
		{desc: "agent without actor", graft: config.Graft{Standalone: true, Policy: deny}, actor: "", agent: true},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			m := graftPolicyManager(t, tc.graft)
			err := m.enforceGraftPolicy(context.Background(), policyEnv(t, tc.actor, tc.agent), policyChanges("HEAD", "refs/heads/main"))
			require.NoError(t, err)
		})
	}
}

// TestEnforceGraftPolicy_MalformedAndMissingMetadata checks that a malformed
// change set is rejected and that missing metadata is reported as an error.
func TestEnforceGraftPolicy_MalformedAndMissingMetadata(t *testing.T) {
	t.Parallel()

	m := graftPolicyManager(t, config.Graft{Standalone: true, Policy: []config.GraftPolicyRule{{Allow: true}}})

	t.Run("malformed change line is rejected", func(t *testing.T) {
		t.Parallel()

		err := m.enforceGraftPolicy(context.Background(), policyEnv(t, "agent-a", true), []byte("garbage\n"))
		require.Error(t, err)
		require.Equal(t, codes.Unknown, status.Code(err))
	})

	t.Run("missing metadata is an error", func(t *testing.T) {
		t.Parallel()

		err := m.enforceGraftPolicy(context.Background(), nil, policyChanges("refs/heads/main"))
		require.Error(t, err)
	})
}

// TestEnforceGraftPolicy_Concurrent evaluates the policy for many agents at the
// same time, for the race detector.
func TestEnforceGraftPolicy_Concurrent(t *testing.T) {
	t.Parallel()

	m := graftPolicyManager(t, config.Graft{Standalone: true, Policy: []config.GraftPolicyRule{
		{Actor: "agent-a", Refs: []string{"refs/heads/**"}, Allow: true},
	}})

	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			ref := "refs/heads/main"
			wantCode := codes.OK
			if i%2 == 1 {
				ref = "HEAD"
				wantCode = codes.PermissionDenied
			}

			err := m.enforceGraftPolicy(context.Background(), policyEnv(t, "agent-a", true), policyChanges(ref))
			if status.Code(err) != wantCode {
				errs <- fmt.Errorf("ref %s: got %v", ref, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.NoError(t, err)
	}
}

// TestEnforceGraftPolicy_refsGraftIsReserved: an agent may never update a reference under
// refs/graft/, whatever the policy says, even with no policy at all; the server and callers that
// are not agents may.
func TestEnforceGraftPolicy_refsGraftIsReserved(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	allowAll := []config.GraftPolicyRule{{Actor: "agent-a", Refs: []string{"**"}, Allow: true}}
	for name, policy := range map[string][]config.GraftPolicyRule{"a policy that allows all": allowAll, "no policy": nil} {
		m := graftPolicyManager(t, config.Graft{Standalone: true, Policy: policy})
		for _, ref := range []string{"refs/graft/proposals/0123456789abcdef/1", "refs/graft/green/main"} {
			err := m.enforceGraftPolicy(ctx, policyEnv(t, "agent-a", true), policyChanges("refs/heads/main", ref))
			require.Equal(t, codes.PermissionDenied, status.Code(err), "%s: %s", name, ref)
			require.ErrorContains(t, err, "reserved for the server", "%s: %s", name, ref)
		}
		// Not an agent (the server, the shared token): allowed.
		require.NoError(t, m.enforceGraftPolicy(ctx, policyEnv(t, "", false), policyChanges("refs/graft/proposals/0123456789abcdef/1")), name)
		// Other references of an agent follow the policy as before.
		require.NoError(t, m.enforceGraftPolicy(ctx, policyEnv(t, "agent-a", true), policyChanges("refs/heads/main")), name)
	}

	// Outside standalone mode the reservation still holds for agents: it is an invariant of the
	// server, not a rule of the policy. The policy itself does not apply there.
	for name, graft := range map[string]config.Graft{
		"not standalone":            {Agents: []config.GraftAgent{{Name: "agent-a"}}},
		"not standalone, a policy":  {Agents: []config.GraftAgent{{Name: "agent-a"}}, Policy: []config.GraftPolicyRule{{Allow: false}}},
		"standalone, policy denies": {Standalone: true, Policy: []config.GraftPolicyRule{{Allow: false}}},
	} {
		m := graftPolicyManager(t, graft)
		err := m.enforceGraftPolicy(ctx, policyEnv(t, "agent-a", true), policyChanges("refs/graft/green/main"))
		require.Equal(t, codes.PermissionDenied, status.Code(err), name)
		require.ErrorContains(t, err, "reserved for the server", name)
		require.NoError(t, m.enforceGraftPolicy(ctx, policyEnv(t, "", false), policyChanges("refs/graft/green/main")), name)
	}
	m := graftPolicyManager(t, config.Graft{Agents: []config.GraftAgent{{Name: "agent-a"}}, Policy: []config.GraftPolicyRule{{Allow: false}}})
	require.NoError(t, m.enforceGraftPolicy(ctx, policyEnv(t, "agent-a", true), policyChanges("refs/heads/main")),
		"outside standalone mode the policy does not apply")
}

// TestEnforceGraftPolicy_batchIsHeldToTheReservation: the combined update of a land batch runs
// without an identity and skips the policy (its members were admitted one by one), but not the
// reservation of refs/graft/, in every mode.
func TestEnforceGraftPolicy_batchIsHeldToTheReservation(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	payload := `{"graft_changelog":{"operation":"land_batch","members":[{"original_commit":"a","commit":"b","actor":"agent-a"}]}}`
	env := []string{gitcmd.EnvHooksPayload + "=" + base64.StdEncoding.EncodeToString([]byte(payload))}

	for name, graft := range map[string]config.Graft{
		"standalone, policy denies": {Standalone: true, Policy: []config.GraftPolicyRule{{Allow: false}}},
		"not standalone":            {Agents: []config.GraftAgent{{Name: "agent-a"}}},
	} {
		m := graftPolicyManager(t, graft)
		err := m.enforceGraftPolicy(ctx, env, policyChanges("refs/graft/green/main"))
		require.Equal(t, codes.PermissionDenied, status.Code(err), name)
		require.ErrorContains(t, err, "reserved for the server", name)
		require.NoError(t, m.enforceGraftPolicy(ctx, env, policyChanges("refs/heads/main")), "%s: the batch skips the policy", name)
	}
}

// TestCheckAgentPolicy checks the policy check of the RPCs that skip the pre-receive hook: the
// reservation of refs/graft/ refuses an agent in every mode, the rest of the policy only in
// standalone mode, and callers that are not agents are not restricted.
func TestCheckAgentPolicy(t *testing.T) {
	t.Parallel()

	agentCtx := identity.NewContext(testhelper.Context(t), identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
	deny := []config.GraftPolicyRule{{Allow: false}}
	reserved := fmt.Sprintf("%s %s refs/graft/green/main\n", policyOldOID, policyNewOID)
	branch := fmt.Sprintf("%s %s refs/heads/main\n", policyOldOID, policyNewOID)

	for name, graft := range map[string]config.Graft{
		"standalone":     {Standalone: true, Policy: deny},
		"not standalone": {Policy: deny},
	} {
		err := CheckAgentPolicy(agentCtx, graft, reserved)
		require.Equal(t, codes.PermissionDenied, status.Code(err), name)
		require.ErrorContains(t, err, "reserved for the server", name)
		require.NoError(t, CheckAgentPolicy(testhelper.Context(t), graft, reserved), "%s: not an agent", name)
	}

	err := CheckAgentPolicy(agentCtx, config.Graft{Standalone: true, Policy: deny}, branch)
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.NoError(t, CheckAgentPolicy(agentCtx, config.Graft{Policy: deny}, branch), "outside standalone mode the policy does not apply")
}

// TestEnforceGraftPolicy_missingMetadataWithAgents: an update whose metadata cannot be read cannot be
// told apart from an agent's, so it is refused whenever agents are configured, in every mode; without
// agents and without a policy to apply there is nothing to decide.
func TestEnforceGraftPolicy_missingMetadataWithAgents(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	agents := []config.GraftAgent{{Name: "agent-a"}}
	require.Error(t, graftPolicyManager(t, config.Graft{Agents: agents}).enforceGraftPolicy(ctx, nil, policyChanges("refs/heads/main")))
	require.Error(t, graftPolicyManager(t, config.Graft{Standalone: true, Agents: agents}).enforceGraftPolicy(ctx, nil, policyChanges("refs/heads/main")))
	require.NoError(t, graftPolicyManager(t, config.Graft{}).enforceGraftPolicy(ctx, nil, policyChanges("refs/heads/main")))
	require.NoError(t, graftPolicyManager(t, config.Graft{Standalone: true}).enforceGraftPolicy(ctx, nil, policyChanges("refs/heads/main")))
}
