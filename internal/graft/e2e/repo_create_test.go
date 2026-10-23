package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestRepoCreateWithTheCLI runs the operator's graft repo create with the real
// graft binary against a real server in authoritative mode, as the quickstart
// does: the repository is created with its initial commit and a second branch,
// registered in the storage core, and an agent then clones it and lands a
// change. An agent token cannot create a repository.
func TestRepoCreateWithTheCLI(t *testing.T) {
	t.Parallel()

	n := newRealNode(t)
	n.start(t)
	bin := buildGraftCLI(t)

	const rel = "demo.git"
	base := append(os.Environ(),
		"GRAFT_ADDRESS="+n.clientCfg.SocketPath,
		"GRAFT_STORAGE="+n.cfg.Storages[0].Name,
		"GRAFT_USER="+workerName(0),
		"GRAFT_AUTHOR="+workerName(0)+" <"+workerName(0)+"@example.com>",
	)
	operator := append(append([]string{}, base...), "GRAFT_TOKEN="+sharedTestToken)
	agent := append(append([]string{}, base...), "GRAFT_TOKEN="+n.tokens[workerName(0)])

	// An agent may never create a repository.
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "repo", "create", rel, "--initial-commit")
	cmd.Env = agent
	cmd.Stderr = &stderr
	require.Error(t, cmd.Run())
	require.Contains(t, stderr.String(), "PermissionDenied")
	require.Contains(t, stderr.String(), "creating a repository needs the shared (operator) token")

	var created struct {
		Repository    string   `json:"repository"`
		Created       bool     `json:"created"`
		DefaultBranch string   `json:"default_branch"`
		InitialCommit string   `json:"initial_commit"`
		Branches      []string `json:"branches_created"`
	}
	out := runGraft(t, bin, operator, "repo", "create", rel, "--initial-commit", "--branch", "feature/demo", "--json")
	require.NoError(t, json.Unmarshal([]byte(out), &created))
	require.True(t, created.Created)
	require.Equal(t, "main", created.DefaultBranch)
	require.NotEmpty(t, created.InitialCommit)
	require.Equal(t, []string{"feature/demo"}, created.Branches)

	state, err := n.operator(t, rel).StorageState(n.ctx)
	require.NoError(t, err)
	require.True(t, state.Tracked, "the gate registers the repository it creates")

	// The agent clones the new repository and lands a change on main.
	dir := filepath.Join(t.TempDir(), "demo")
	runGraft(t, bin, agent, "clone", n.clientCfg.SocketPath, rel, dir, "--branch", "main")
	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	require.NoError(t, err)
	require.Equal(t, "# demo\n", string(readme))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello\n"), 0o644))
	runGraft(t, bin, agent, "commit", "-C", dir, "-m", "Add hello", "--land", "main")

	landed, err := n.agent(t, rel).ResolveRevision(n.ctx, "main")
	require.NoError(t, err)
	require.NotEqual(t, created.InitialCommit, landed)
	n.requireServed(t, rel, map[string]string{"main": landed, "feature/demo": created.InitialCommit})
}
