// Package policystore holds the standalone access policy in force ([[graft.policy]]) and replaces
// it while the server runs (doc/graft/PLAN12.md).
//
// Every reader of the policy (the standalone access check of the pre-receive hook, the
// reference-transaction hook, the policy checks of the operation RPCs and the admission of a land
// batch) reads the Snapshot in force from one Store, once per decision. A Store always holds
// exactly one valid policy: the startup policy before the server serves, and afterwards each policy
// that Reload validated, published with one atomic store. An invalid configuration never replaces
// it.
package policystore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/prometheus/client_golang/prometheus"
)

// Snapshot is one version of the policy. It is never changed once published.
type Snapshot struct {
	// Rules are the rules of the policy, in order.
	Rules []config.GraftPolicyRule
	// Generation numbers the versions of the policy of a Store: 1 at startup, one more for every
	// policy that Reload applies.
	Generation uint64
	// SHA256 is the hash of the rules (Hash), which tells an operator which policy a node runs.
	SHA256 string
}

// Result is the outcome of a reload.
type Result string

const (
	// Applied means that the new policy is in force.
	Applied Result = "applied"
	// Unchanged means that the new configuration has the policy in force.
	Unchanged Result = "unchanged"
	// Rejected means that the configuration could not be read or its policy is invalid: the
	// policy in force stays.
	Rejected Result = "rejected"
)

// Outcome describes a reload.
type Outcome struct {
	Result Result
	// Previous is the policy in force before the reload and Current the one after it, the same
	// Snapshot unless the reload applied a new policy.
	Previous, Current *Snapshot
	// NeedsRestart names the changes of the configuration, outside the policy, that a reload does
	// not apply.
	NeedsRestart []string
	// UnknownActors lists the actors of the new policy that name no configured agent. They are not
	// an error: an actor can be the user ID of a caller with the shared token.
	UnknownActors []string
}

// Store holds the policy in force.
type Store struct {
	current atomic.Pointer[Snapshot]
	// reload serializes reloads, so that generations follow each other.
	reload sync.Mutex

	reloads    *prometheus.CounterVec
	generation prometheus.Gauge
	info       *prometheus.GaugeVec
}

// New returns a Store whose policy is rules, as generation 1.
func New(rules []config.GraftPolicyRule) *Store {
	s := &Store{
		reloads: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "graft_policy_reloads_total",
			Help: "Reloads of the standalone access policy, by result (applied, unchanged, rejected).",
		}, []string{"result"}),
		generation: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "graft_policy_generation",
			Help: "Generation of the standalone access policy in force: 1 at startup, one more for every reload that applied a new policy.",
		}),
		info: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "graft_policy_info",
			Help: "1 for the SHA-256 of the standalone access policy in force.",
		}, []string{"sha256"}),
	}
	s.publish(&Snapshot{Rules: clone(rules), Generation: 1, SHA256: Hash(rules)})
	return s
}

// Current returns the policy in force. It is never nil.
func (s *Store) Current() *Snapshot {
	return s.current.Load()
}

// Describe implements prometheus.Collector.
func (s *Store) Describe(descs chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(s, descs)
}

// Collect implements prometheus.Collector.
func (s *Store) Collect(metrics chan<- prometheus.Metric) {
	s.reloads.Collect(metrics)
	s.generation.Collect(metrics)
	s.info.Collect(metrics)
}

// Reject records a reload whose configuration could not be read: the policy in force stays.
func (s *Store) Reject() Outcome {
	s.reload.Lock()
	defer s.reload.Unlock()

	s.reloads.WithLabelValues(string(Rejected)).Inc()
	current := s.Current()
	return Outcome{Result: Rejected, Previous: current, Current: current}
}

// Reload replaces the policy in force with the policy of next, a configuration that was read
// again, when that policy is valid and different. running is the [graft] section the server
// started with: next must agree with it outside the policy for the server to be consistent, and
// what differs is reported in Outcome.NeedsRestart and not applied. An invalid policy returns its
// error with the Rejected result, and the policy in force stays.
func (s *Store) Reload(next config.Graft, running config.Graft) (Outcome, error) {
	s.reload.Lock()
	defer s.reload.Unlock()

	previous := s.Current()
	if err := next.Validate(); err != nil {
		s.reloads.WithLabelValues(string(Rejected)).Inc()
		return Outcome{Result: Rejected, Previous: previous, Current: previous}, err
	}

	outcome := Outcome{
		Previous:      previous,
		Current:       previous,
		NeedsRestart:  needsRestart(next, running),
		UnknownActors: unknownActors(next.Policy, running.Agents),
	}

	hash := Hash(next.Policy)
	if hash == previous.SHA256 {
		outcome.Result = Unchanged
		s.reloads.WithLabelValues(string(Unchanged)).Inc()
		return outcome, nil
	}

	outcome.Current = &Snapshot{Rules: clone(next.Policy), Generation: previous.Generation + 1, SHA256: hash}
	s.publish(outcome.Current)
	outcome.Result = Applied
	s.reloads.WithLabelValues(string(Applied)).Inc()
	return outcome, nil
}

// publish puts snapshot in force and updates the gauges.
func (s *Store) publish(snapshot *Snapshot) {
	previous := s.current.Swap(snapshot)
	s.generation.Set(float64(snapshot.Generation))
	// The new series first: a scrape between the two sees the hash in force, never none.
	s.info.WithLabelValues(snapshot.SHA256).Set(1)
	if previous != nil && previous.SHA256 != snapshot.SHA256 {
		s.info.DeleteLabelValues(previous.SHA256)
	}
}

// Hash returns the SHA-256 of rules in a canonical encoding: two policies with the same rules in
// the same order have the same hash, however their files are formatted.
func Hash(rules []config.GraftPolicyRule) string {
	canonical := make([]config.GraftPolicyRule, len(rules))
	for i, rule := range rules {
		canonical[i] = config.GraftPolicyRule{Actor: rule.Actor, Refs: rule.Refs, Allow: rule.Allow}
		if canonical[i].Refs == nil {
			canonical[i].Refs = []string{}
		}
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		// The rules are strings and booleans: they always encode.
		panic(fmt.Sprintf("encoding policy rules: %v", err))
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// needsRestart names the settings of the [graft] section that differ between next and running,
// other than the policy: a reload applies the policy only.
func needsRestart(next, running config.Graft) []string {
	return changedSettings("graft.", next, running, "policy")
}

// NeedsRestartOutsideGraft names the settings outside the [graft] section that differ between the
// configurations next and running, by their TOML keys: a reload does not apply them.
func NeedsRestartOutsideGraft(next, running config.Cfg) []string {
	return changedSettings("", next, running, "graft")
}

// changedSettings names, with prefix, the TOML keys of the fields of the structs next and running,
// of one type, whose values differ, skipping the keys in skip.
func changedSettings(prefix string, next, running any, skip ...string) []string {
	nextValue, runningValue := reflect.ValueOf(next), reflect.ValueOf(running)
	var changed []string
	for i := 0; i < nextValue.NumField(); i++ {
		field := nextValue.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		key, _, _ := strings.Cut(field.Tag.Get("toml"), ",")
		if key == "" {
			key = field.Name
		}
		if slices.Contains(skip, key) {
			continue
		}
		if !reflect.DeepEqual(nextValue.Field(i).Interface(), runningValue.Field(i).Interface()) {
			changed = append(changed, prefix+key)
		}
	}
	return changed
}

// unknownActors returns the actors of rules that are plain names, not globs, and name none of
// agents. Without agents every actor is a user ID, so none is reported.
func unknownActors(rules []config.GraftPolicyRule, agents []config.GraftAgent) []string {
	if len(agents) == 0 {
		return nil
	}
	var unknown []string
	for _, rule := range rules {
		if rule.Actor == "" || strings.ContainsAny(rule.Actor, `*?[\`) {
			continue
		}
		known := slices.ContainsFunc(agents, func(agent config.GraftAgent) bool { return agent.Name == rule.Actor })
		if !known && !slices.Contains(unknown, rule.Actor) {
			unknown = append(unknown, rule.Actor)
		}
	}
	return unknown
}

func clone(rules []config.GraftPolicyRule) []config.GraftPolicyRule {
	out := make([]config.GraftPolicyRule, len(rules))
	for i, rule := range rules {
		out[i] = config.GraftPolicyRule{Actor: rule.Actor, Refs: slices.Clone(rule.Refs), Allow: rule.Allow}
	}
	return out
}

type admissionKey struct{}

// WithAdmissionGeneration returns ctx carrying the generation of the policy under which a land
// batch admitted its members. The hooks payload of the batch's combined update carries it, and the
// reference-transaction hook refuses the update when the policy changed since (PLAN12, section 5).
func WithAdmissionGeneration(ctx context.Context, generation uint64) context.Context {
	return context.WithValue(ctx, admissionKey{}, generation)
}

// AdmissionGeneration returns the generation of WithAdmissionGeneration, or 0.
func AdmissionGeneration(ctx context.Context) uint64 {
	generation, _ := ctx.Value(admissionKey{}).(uint64)
	return generation
}
