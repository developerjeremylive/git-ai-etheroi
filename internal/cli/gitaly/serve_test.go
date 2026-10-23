package gitaly

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

// These tests set environment variables and register Prometheus collectors, so
// they must not run in parallel.

func TestNewGitlabAPIClient_Standalone(t *testing.T) {
	t.Setenv("GITALY_TESTING_NO_GIT_HOOKS", "false")

	ctx := testhelper.Context(t)

	cfg := config.Cfg{
		Gitlab: config.Gitlab{
			// Unreachable on purpose: standalone mode must never contact GitLab.
			URL: "http://127.0.0.1:1",
		},
		Graft: config.Graft{
			Standalone: true,
			Policy: []config.GraftPolicyRule{
				{Actor: "user-*", Refs: []string{"refs/heads/main"}, Allow: false},
				{Actor: "*", Allow: true},
			},
		},
	}

	client, err := newGitlabAPIClient(testhelper.SharedLogger(t), cfg, policystore.New(cfg.Graft.Policy))
	require.NoError(t, err)

	// The policy is enforced by the built-in client.
	allowed, msg, err := client.Allowed(ctx, gitlab.AllowedParams{
		GLID:    "user-1",
		Changes: "0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 refs/heads/main\n",
	})
	require.NoError(t, err)
	require.False(t, allowed)
	require.Contains(t, msg, `policy denies "user-1" to update "refs/heads/main"`)

	// Check would fail against the stub client and against an unreachable GitLab,
	// so a successful Check shows that the standalone client is in use.
	info, err := client.Check(ctx)
	require.NoError(t, err)
	require.NotNil(t, info)
}

func TestNewGitlabAPIClient_StandaloneTakesPrecedenceOverBypass(t *testing.T) {
	t.Setenv("GITALY_TESTING_NO_GIT_HOOKS", "true")

	cfg := config.Cfg{
		Graft: config.Graft{
			Standalone: true,
			Policy:     []config.GraftPolicyRule{{Actor: "*", Allow: false}},
		},
	}

	client, err := newGitlabAPIClient(testhelper.SharedLogger(t), cfg, policystore.New(cfg.Graft.Policy))
	require.NoError(t, err)

	allowed, _, err := client.Allowed(testhelper.Context(t), gitlab.AllowedParams{
		GLID:    "user-1",
		Changes: "0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 refs/heads/dev\n",
	})
	require.NoError(t, err)
	require.False(t, allowed, "the policy must apply, not the stub client's allow-all")
}

func TestNewGitlabAPIClient_BypassUsesStubClient(t *testing.T) {
	t.Setenv("GITALY_TESTING_NO_GIT_HOOKS", "true")

	client, err := newGitlabAPIClient(testhelper.SharedLogger(t), config.Cfg{}, policystore.New(nil))
	require.NoError(t, err)

	// The stub client does not connect to the internal API, so Check fails.
	_, err = client.Check(testhelper.Context(t))
	require.EqualError(t, err, "stub client does not connect to internal API")

	ok, err := client.PreReceive(testhelper.Context(t), "graft:default/repo.git")
	require.NoError(t, err)
	require.True(t, ok)
}

func TestNewGitlabAPIClient_HTTPClient(t *testing.T) {
	t.Setenv("GITALY_TESTING_NO_GIT_HOOKS", "false")

	cfg := config.Cfg{
		Gitlab: config.Gitlab{
			URL:    "http://127.0.0.1:1",
			Secret: "secret",
		},
	}

	client, err := newGitlabAPIClient(testhelper.SharedLogger(t), cfg, policystore.New(cfg.Graft.Policy))
	require.NoError(t, err)

	httpClient, ok := client.(*gitlab.HTTPClient)
	require.True(t, ok, "expected the GitLab HTTP client, got %T", client)
	t.Cleanup(func() { prometheus.Unregister(httpClient) })
}

func TestNewGitlabAPIClient_HTTPClientConfigurationError(t *testing.T) {
	t.Setenv("GITALY_TESTING_NO_GIT_HOOKS", "false")

	cfg := config.Cfg{
		Gitlab: config.Gitlab{
			URL:    "http://127.0.0.1:1",
			Secret: "secret",
		},
		// Unreadable client certificate makes the HTTP client construction fail.
		TLS: config.TLS{
			CertPath: "/does/not/exist/cert.pem",
			KeyPath:  "/does/not/exist/key.pem",
		},
	}

	client, err := newGitlabAPIClient(testhelper.SharedLogger(t), cfg, policystore.New(cfg.Graft.Policy))
	require.Error(t, err)
	require.Contains(t, err.Error(), "could not create GitLab API client")
	require.Nil(t, client)
}

// TestNewDerivedPool_RegistersWarmMetrics requires the server's derived-data pool to
// be registered, so that the graft_derived_warm_* metrics are exported.
func TestNewDerivedPool_RegistersWarmMetrics(t *testing.T) {
	registry := prometheus.NewRegistry()
	pool := newDerivedPool(testhelper.SharedLogger(t), config.Cfg{}, nil, nil, registry)
	t.Cleanup(pool.Close)

	// A closed pool drops what it is offered, which the dropped counter must show.
	pool.Close()
	pool.Enqueue(nil, []git.ObjectID{git.ObjectID("1111111111111111111111111111111111111111")})

	families, err := registry.Gather()
	require.NoError(t, err)

	values := make(map[string]float64, len(families))
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			switch {
			case metric.GetCounter() != nil:
				values[family.GetName()] = metric.GetCounter().GetValue()
			case metric.GetGauge() != nil:
				values[family.GetName()] = metric.GetGauge().GetValue()
			}
		}
	}
	require.Equal(t, map[string]float64{
		"graft_derived_warm_queue_depth":    0,
		"graft_derived_warm_dropped_total":  1,
		"graft_derived_warm_computed_total": 0,
		"graft_derived_warm_errors_total":   0,
	}, values)
}
