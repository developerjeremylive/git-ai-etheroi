package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// startWithAgentPushes starts the example server with SSHService.SSHReceivePack
// opened to agents through [graft] agent_rpcs, the operator opt-in of
// AGENT-SECURITY.md, and a policy that also lets the builder update
// refs/graft/**, so that only the reservation of that namespace can refuse it.
func startWithAgentPushes(t *testing.T, opts ...testserver.GitalyServerOpt) gitalyStack {
	t.Helper()

	ex := loadExample(t)
	ex.graft.AgentRPCs = []string{"/gitaly.SSHService/SSHReceivePack"}
	ex.graft.Policy = append([]config.GraftPolicyRule{
		{Actor: "builder", Refs: []string{"refs/graft/**"}, Allow: true},
	}, ex.graft.Policy...)

	return startGitaly(t, ex.graft, ex.sharedToken, opts...)
}

// agentPush pushes refspec from the client repository at clientPath to the
// stack's repository over gitaly-ssh, authenticated with the agent token, and
// returns the combined output of git push and whether it succeeded.
func agentPush(t *testing.T, s gitalyStack, clientPath, token, agent, refspec string) (string, bool) {
	t.Helper()

	payload, err := protojson.Marshal(&gitalypb.SSHReceivePackRequest{
		Repository: &gitalypb.Repository{
			StorageName:  s.storage,
			RelativePath: s.relPath,
			GlRepository: "graft:" + s.storage + "/" + s.relPath,
		},
		GlId:         agent,
		GlUsername:   agent,
		GlRepository: "graft:" + s.storage + "/" + s.relPath,
	})
	require.NoError(t, err)

	var flags []string
	for flag, value := range featureflag.FromContext(s.ctx) {
		flags = append(flags, flag.FormatWithValue(value))
	}

	cmd := gittest.NewCommand(t, s.cfg, "-C", clientPath, "push", "-v", "git@localhost:graft/push.git", refspec)
	cmd.Env = append(cmd.Env,
		"GITALY_PAYLOAD="+string(payload),
		"GITALY_ADDRESS="+s.addr,
		"GITALY_TOKEN="+token,
		"GITALY_FEATUREFLAGS="+strings.Join(flags, ","),
		fmt.Sprintf("GIT_SSH_COMMAND=%s receive-pack", s.cfg.BinaryPath("gitaly-ssh")),
	)
	out, err := cmd.CombinedOutput()
	return string(out), err == nil
}

// TestPolicyHoldsOnAgentPushes pushes as an agent over SSHReceivePack, which an
// operator may open to agents with agent_rpcs. A branch the policy allows is
// accepted, and one it denies is refused by the policy in the pre-receive hook.
// refs/graft/ is reserved for the server whatever the policy says: the builder
// may update refs/graft/** by policy here, and git-receive-pack still refuses its
// push there, because that namespace is hidden from agents.
func TestPolicyHoldsOnAgentPushes(t *testing.T) {
	t.Parallel()

	s := startWithAgentPushes(t)
	base := writeMain(t, s, map[string]string{"README.md": "push\n"})

	clientPath := filepath.Join(t.TempDir(), "client")
	gittest.Exec(t, s.cfg, "clone", "--quiet", s.repoPath, clientPath)
	require.NoError(t, os.WriteFile(filepath.Join(clientPath, "pushed.txt"), []byte("pushed\n"), 0o644))
	gittest.Exec(t, s.cfg, "-C", clientPath, "add", "pushed.txt")
	gittest.Exec(t, s.cfg, "-C", clientPath, "-c", "user.name=builder", "-c", "user.email=builder@example.com", "commit", "--quiet", "-m", "pushed")
	pushed := strings.TrimSpace(string(gittest.Exec(t, s.cfg, "-C", clientPath, "rev-parse", "HEAD")))

	builder := exampleTokens["builder"]

	// A branch the policy allows is accepted: the push path works for agents.
	out, ok := agentPush(t, s, clientPath, builder, "builder", "HEAD:refs/heads/feature/pushed")
	require.True(t, ok, out)
	require.Equal(t, pushed, s.git(t, "rev-parse", "refs/heads/feature/pushed"))

	// A branch the policy denies is refused by the policy.
	out, ok = agentPush(t, s, clientPath, builder, "builder", "HEAD:refs/heads/main")
	require.False(t, ok, out)
	require.Contains(t, out, `policy denies "builder" to update "refs/heads/main" (rule 3)`)
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	// refs/graft/ is the server's, although the policy allows the builder there:
	// git-receive-pack hides it from agents (receive.hideRefs), so git refuses the
	// update before any hook runs.
	out, ok = agentPush(t, s, clientPath, builder, "builder", "HEAD:refs/graft/pushed")
	require.False(t, ok, out)
	require.Contains(t, out, "refs/graft/pushed (deny updating a hidden ref)")
	require.Empty(t, s.git(t, "for-each-ref", "refs/graft/"))
}
