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

// notifyPolicyReload returns a channel that receives policyReloadSignal from now on. One signal waits
// in it until reloadPolicyOnSignal reads it, and those that come meanwhile make no more reloads.
func notifyPolicyReload() chan os.Signal {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, policyReloadSignal)
	return signals
}

// reloadPolicyOnSignal reloads the policy of store from the configuration file at configPath every
// time signals, which notifyPolicyReload returned, receives the signal, one reload at a time; a
// signal that came before is handled at once. running is the configuration the server started
// with, as the file gave it. The returned function stops it.
func reloadPolicyOnSignal(logger log.Logger, configPath string, running config.Cfg, store *policystore.Store, signals chan os.Signal) func() {
	done := make(chan struct{})

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
// is valid; otherwise the policy in force stays. running is the configuration the server started
// with: the settings of the file that differ from it, other than the policy, need a restart. The
// outcome is logged: an invalid configuration at ERROR, a new policy at INFO, and the settings that
// need a restart and the actors that name no configured agent at WARN.
func reloadPolicy(logger log.Logger, configPath string, running config.Cfg, store *policystore.Store) policystore.Outcome {
	logger = logger.WithField("config_path", configPath)

	next, err := loadConfig(configPath)
	var outcome policystore.Outcome
	if err != nil {
		outcome = store.Reject()
	} else {
		outcome, err = store.Reload(next.Graft, running.Graft)
		if err == nil {
			outcome.NeedsRestart = append(policystore.NeedsRestartOutsideGraft(next, running), outcome.NeedsRestart...)
		}
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
		logger.WithField("needs_restart", outcome.NeedsRestart).Warn("policy reload: these settings changed outside the policy; they need a restart and were not applied")
	}
	if len(outcome.UnknownActors) > 0 {
		logger.WithField("actors", outcome.UnknownActors).Warn("policy reload: actors of the policy that name no configured agent (user IDs of callers with the shared token?)")
	}
	return outcome
}
