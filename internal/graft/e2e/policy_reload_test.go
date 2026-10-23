package e2e

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// TestPolicyReloadOnSignal reloads the policy of a server process with SIGUSR1 (PLAN12): a valid
// policy is put in force, an invalid one is rejected and the process keeps serving with the policy
// before, and SIGHUP keeps its meaning, the end of the process.
func TestPolicyReloadOnSignal(t *testing.T) {
	t.Parallel()

	n := newRealNode(t)
	n.start(t)
	require.EqualValues(t, 1, n.counter(t, "graft_policy_generation"))

	// rewrite replaces the node's configuration file with one whose policy is policy, as an
	// editor that writes a new file and renames it over the old one does.
	rewrite := func(policy []config.GraftPolicyRule) {
		cfg := n.cfg
		cfg.Graft.Policy = policy
		require.NoError(t, os.Rename(testcfg.WriteTemporaryGitalyConfigFile(t, cfg), n.cfgPath))
	}
	// waitFor waits until the series name of the server's metrics is want. The series of a
	// labelled counter exists only once it was first counted, so a missing one is 0.
	waitFor := func(name string, want float64) {
		require.Eventually(t, func() bool { return metricOrZero(t, n, name) == want }, time.Minute, 10*time.Millisecond,
			"%s did not reach %v\n%s", name, want, n.log(t))
	}

	rewrite(append([]config.GraftPolicyRule{{Actor: "nobody", Refs: []string{"refs/heads/main"}, Allow: false}}, n.cfg.Graft.Policy...))
	require.NoError(t, n.cmd.Process.Signal(syscall.SIGUSR1))
	waitFor(`graft_policy_reloads_total{result="applied"}`, 1)
	require.EqualValues(t, 2, n.counter(t, "graft_policy_generation"))
	// Only the policy changed: the server compares the file with the configuration it started
	// with, as the file gave it, and finds nothing else.
	require.NotContains(t, n.log(t), "they need a restart")

	rewrite([]config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/["}, Allow: true}})
	require.NoError(t, n.cmd.Process.Signal(syscall.SIGUSR1))
	waitFor(`graft_policy_reloads_total{result="rejected"}`, 1)
	require.EqualValues(t, 2, n.counter(t, "graft_policy_generation"), "a rejected reload keeps the policy in force")
	require.Contains(t, n.log(t), "policy reload rejected: the policy in force stays")
	// The process still serves.
	n.createRepository(t)

	require.NoError(t, n.cmd.Process.Signal(syscall.SIGHUP))
	n.waitExit(t)
}

// metricOrZero returns the value of the series name of the node's metrics, or 0 when the node does
// not export it.
func metricOrZero(t *testing.T, n *realNode, name string) float64 {
	t.Helper()

	resp, err := http.Get("http://" + n.cfg.PrometheusListenAddr + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	for _, line := range strings.Split(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, name+" "); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			require.NoError(t, err)
			return v
		}
	}
	return 0
}
