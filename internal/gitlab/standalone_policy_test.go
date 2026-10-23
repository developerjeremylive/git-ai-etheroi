package gitlab

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

const (
	policyTestOld = "0000000000000000000000000000000000000000"
	policyTestNew = "2222222222222222222222222222222222222222"
)

func policyTestChanges(refs ...string) string {
	var b strings.Builder
	for _, ref := range refs {
		fmt.Fprintf(&b, "%s %s %s\n", policyTestOld, policyTestNew, ref)
	}
	return b.String()
}

// TestStandaloneClient_DenialIsNotGitLabMessage checks the standalone client's
// denial for an update performed with the policy.
func TestStandaloneClient_DenialIsNotGitLabMessage(t *testing.T) {
	t.Parallel()

	client := NewStandaloneClient(config.Graft{
		Standalone: true,
		Policy:     []config.GraftPolicyRule{{Refs: []string{"refs/heads/**"}, Allow: true}},
	}, testhelper.SharedLogger(t))

	allowed, message, err := client.Allowed(context.Background(), AllowedParams{
		GLID:    "agent-a",
		Changes: policyTestChanges("HEAD"),
	})
	require.NoError(t, err)
	require.False(t, allowed)
	require.NotEmpty(t, message)
	require.False(t, strings.HasPrefix(message, "GitLab:"))
}
