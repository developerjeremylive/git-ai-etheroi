package gitaly

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/log"
)

// policyReloadSignal reloads the standalone access policy from the configuration file. SIGHUP is
// taken: the default bootstrap shuts down on it and the tableflip bootstrap upgrades
// (doc/graft/PLAN12.md, section 4).
var policyReloadSignal = syscall.SIGUSR1

// reloaded runs after each reload that a signal caused, with its outcome. Tests replace it to wait
// for the reload.
var reloaded = func(policystore.Outcome) {}

// reloadPolicyOnSignal reloads the policy of store from the configuration file at configPath every
// time the process receives policyReloadSignal, one reload at a time. running is the [graft]
// section the server started with. The returned function stops it.
func reloadPolicyOnSignal(logger log.Logger, configPath string, running config.Graft, store *policystore.Store) func() {
	signals := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(signals, policyReloadSignal)

	go func() {
		defer close(done)
		for range signals {
			reloaded(reloadPolicy(logger, configPath, running, store))
		}
	}()

	return func() {
		signal.Stop(signals)
		close(signals)
		<-done
	}
}

// reloadPolicy reads the configuration file at configPath again and puts its policy in force when it
// is valid; otherwise the policy in force stays. The outcome is logged: an invalid configuration at
// ERROR, a new policy at INFO, and the changes that need a restart and the actors that name no
// configured agent at WARN.
func reloadPolicy(logger log.Logger, configPath string, running config.Graft, store *policystore.Store) policystore.Outcome {
	logger = logger.WithField("config_path", configPath)

	next, err := loadConfig(configPath)
	var outcome policystore.Outcome
	if err != nil {
		outcome = store.Reject()
	} else {
		outcome, err = store.Reload(next.Graft, running)
	}
	if err != nil {
		logger.WithError(err).WithFields(log.Fields{
			"generation": outcome.Current.Generation,
			"sha256":     outcome.Current.SHA256,
		}).Error("policy reload rejected: the policy in force stays")
		return outcome
	}

	fields := log.Fields{
		"generation": outcome.Current.Generation,
		"sha256":     outcome.Current.SHA256,
		"rules":      len(outcome.Current.Rules),
	}
	switch outcome.Result {
	case policystore.Applied:
		fields["previous_generation"] = outcome.Previous.Generation
		fields["previous_sha256"] = outcome.Previous.SHA256
		logger.WithFields(fields).Info("policy reloaded")
	default:
		logger.WithFields(fields).Info("policy reload: the policy is unchanged")
	}
	if len(outcome.NeedsRestart) > 0 {
		logger.WithField("needs_restart", outcome.NeedsRestart).Warn("policy reload: the configuration changed outside the policy; those changes need a restart and were not applied")
	}
	if len(outcome.UnknownActors) > 0 {
		logger.WithField("actors", outcome.UnknownActors).Warn("policy reload: actors of the policy that name no configured agent (user IDs of callers with the shared token?)")
	}
	return outcome
}
