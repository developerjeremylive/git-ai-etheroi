package operations

import (
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	gitalycfg "github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/identity"
)

// TestLandPrecheckResolution_policyFirst: an agent the policy does not let update the branch gets
// PermissionDenied before its tree is verified (the resolver's check runs behind the same line):
// verify.command does not run, so that a denied agent cannot hold the repository's verify slot.
func TestLandPrecheckResolution_policyFirst(t *testing.T) {
	t.Parallel()

	vdir, vargv := countingVerify(t, "exit 0")
	rcfg := withVerify(gitalycfg.GraftResolver{}, vargv, "")
	fx := newResolveFixture(t, rcfg)
	s := fx.server(t, rcfg)
	s.graft = gitalycfg.Graft{Standalone: true, Policy: []gitalycfg.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/other"}, Allow: true}}}
	s.graftPolicy = policystore.New(s.graft.Policy)
	ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})
	in := fx.request("Land")
	in.Resolution = resolutionOf(fx, "agent-a\n", "agent-b\n")

	pc, err := s.landPrecheckResolution(ctx, in, fx.repo)
	require.Nil(t, pc)
	require.Equal(t, codes.PermissionDenied, status.Code(err), "err = %v", err)
	require.Contains(t, err.Error(), `policy denies "agent-7"`)
	require.Equal(t, 0, verifyRuns(t, vdir), "nothing was verified")

	// An agent the policy lets update the branch gets its resolution checked and verified.
	s.graft.Policy = []gitalycfg.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/**"}, Allow: true}}
	s.graftPolicy = policystore.New(s.graft.Policy)
	pc, err = s.landPrecheckResolution(ctx, in, fx.repo)
	require.NoError(t, err)
	require.NotNil(t, pc)
	require.Empty(t, pc.skipped)
	require.NotEmpty(t, pc.verified, "verifyErr = %v", pc.verifyErr)
	require.Equal(t, 1, verifyRuns(t, vdir))
}
