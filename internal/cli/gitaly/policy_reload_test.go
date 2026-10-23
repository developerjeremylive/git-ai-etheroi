package gitaly

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

var (
	startupRules = []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/**"}, Allow: true}}
	tighterRules = []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/feature/**"}, Allow: true}}
)

// startupConfig returns the configuration with startupRules as the server loads it at startup.
func startupConfig(t *testing.T) config.Cfg {
	cfg := testcfg.Build(t)
	cfg.Graft.Policy = startupRules
	loaded, err := loadConfig(testcfg.WriteTemporaryGitalyConfigFile(t, cfg))
	require.NoError(t, err)
	return loaded
}

// reloadSetup returns the startup configuration, a store in force with its policy, the reload of
// a configuration file by a server started with it, and a hook on the logger of the reload.
func reloadSetup(t *testing.T) (config.Cfg, *policystore.Store, testhelper.LoggerHook, func(string) policystore.Outcome) {
	cfg := startupConfig(t)
	store := policystore.New(cfg.Graft.Policy)
	logger := testhelper.NewLogger(t)
	hook := testhelper.AddLoggerHook(logger)
	reload := func(configPath string) policystore.Outcome {
		return reloadPolicy(logger, configPath, cfg.Graft, store)
	}
	return cfg, store, hook, reload
}

// lastEntry returns the level and the message of the last log entry.
func lastEntry(t *testing.T, hook testhelper.LoggerHook) (logrus.Level, string) {
	entry := hook.LastEntry()
	require.NotNil(t, entry)
	return entry.Level, entry.Message
}

func TestReloadPolicy(t *testing.T) {
	t.Parallel()

	t.Run("a valid new policy is put in force", func(t *testing.T) {
		t.Parallel()

		cfg, store, hook, reload := reloadSetup(t)
		cfg.Graft.Policy = tighterRules

		outcome := reload(testcfg.WriteTemporaryGitalyConfigFile(t, cfg))
		require.Equal(t, policystore.Applied, outcome.Result)
		require.Equal(t, uint64(2), store.Current().Generation)
		require.Equal(t, tighterRules, store.Current().Rules)
		require.Equal(t, policystore.Hash(tighterRules), store.Current().SHA256)
		level, message := lastEntry(t, hook)
		require.Equal(t, logrus.InfoLevel, level)
		require.Equal(t, "policy reloaded", message)
	})

	t.Run("the same policy changes nothing", func(t *testing.T) {
		t.Parallel()

		cfg, store, hook, reload := reloadSetup(t)

		outcome := reload(testcfg.WriteTemporaryGitalyConfigFile(t, cfg))
		require.Equal(t, policystore.Unchanged, outcome.Result)
		require.Same(t, outcome.Previous, store.Current())
		require.Equal(t, uint64(1), store.Current().Generation)
		level, message := lastEntry(t, hook)
		require.Equal(t, logrus.InfoLevel, level)
		require.Equal(t, "policy reload: the policy is unchanged", message)
	})

	for desc, write := range map[string]func(t *testing.T, cfg config.Cfg) string{
		"an unreadable file": func(t *testing.T, cfg config.Cfg) string {
			return filepath.Join(testhelper.TempDir(t), "missing.toml")
		},
		"a file that is no TOML": func(t *testing.T, cfg config.Cfg) string {
			path := filepath.Join(testhelper.TempDir(t), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[graft\npolicy = "), mode.File))
			return path
		},
		"a policy with an invalid glob": func(t *testing.T, cfg config.Cfg) string {
			cfg.Graft.Policy = []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/["}, Allow: true}}
			return testcfg.WriteTemporaryGitalyConfigFile(t, cfg)
		},
		"a policy with an empty refs entry": func(t *testing.T, cfg config.Cfg) string {
			cfg.Graft.Policy = []config.GraftPolicyRule{{Actor: "*", Refs: []string{""}, Allow: true}}
			return testcfg.WriteTemporaryGitalyConfigFile(t, cfg)
		},
	} {
		t.Run(desc+" is rejected and the policy in force stays", func(t *testing.T) {
			t.Parallel()

			cfg, store, hook, reload := reloadSetup(t)
			before := store.Current()

			outcome := reload(write(t, cfg))
			require.Equal(t, policystore.Rejected, outcome.Result)
			require.Same(t, before, store.Current())
			level, message := lastEntry(t, hook)
			require.Equal(t, logrus.ErrorLevel, level)
			require.Equal(t, "policy reload rejected: the policy in force stays", message)
		})
	}

	t.Run("changed agents need a restart and are not applied; the policy is", func(t *testing.T) {
		t.Parallel()

		cfg, store, hook, reload := reloadSetup(t)
		cfg.Graft.Policy = tighterRules
		cfg.Graft.Agents = []config.GraftAgent{{Name: "alice", Token: "alice-secret-token"}}

		outcome := reload(testcfg.WriteTemporaryGitalyConfigFile(t, cfg))
		require.Equal(t, policystore.Applied, outcome.Result)
		require.Equal(t, []string{"graft.agent"}, outcome.NeedsRestart)
		require.Equal(t, tighterRules, store.Current().Rules)
		var warned bool
		for _, entry := range hook.AllEntries() {
			if entry.Level == logrus.WarnLevel {
				require.Contains(t, entry.Message, "need a restart and were not applied")
				require.Equal(t, []string{"graft.agent"}, entry.Data["needs_restart"])
				warned = true
			}
		}
		require.True(t, warned, "the changes that need a restart are logged")
	})
}

// TestReloadPolicyOnSignal sends the reload signal to the process. It does not run in parallel: it
// replaces reloaded, which the reloads of other tests would call.
func TestReloadPolicyOnSignal(t *testing.T) {
	cfg := startupConfig(t)
	store := policystore.New(cfg.Graft.Policy)
	running := cfg.Graft
	cfg.Graft.Policy = tighterRules
	configPath := testcfg.WriteTemporaryGitalyConfigFile(t, cfg)

	outcomes := make(chan policystore.Outcome, 1)
	original := reloaded
	reloaded = func(outcome policystore.Outcome) { outcomes <- outcome }
	t.Cleanup(func() { reloaded = original })

	stop := reloadPolicyOnSignal(testhelper.NewLogger(t), configPath, running, store)
	defer stop()

	require.NoError(t, syscall.Kill(os.Getpid(), policyReloadSignal))
	select {
	case outcome := <-outcomes:
		require.Equal(t, policystore.Applied, outcome.Result)
	case <-testhelper.Context(t).Done():
		require.FailNow(t, "no reload after the signal")
	}
	require.Equal(t, uint64(2), store.Current().Generation)
	require.Equal(t, tighterRules, store.Current().Rules)
}
