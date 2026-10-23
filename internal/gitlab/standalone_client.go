package gitlab

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/log"
)

// standaloneClient is a Client for Graft's standalone mode. It never contacts
// GitLab: access checks are answered from the ordered [graft] policy instead.
type standaloneClient struct {
	// policy holds the policy in force, which the server may reload (doc/graft/PLAN12.md).
	policy *policystore.Store
	logger log.Logger
}

// NewStandaloneClient returns a Client that answers access checks from
// cfg.Policy without any GitLab instance. When the policy is empty every
// update is allowed. When it is not empty, an update is allowed only if every
// changed reference is allowed by the first policy rule that applies to it;
// a reference that no rule applies to is denied.
func NewStandaloneClient(cfg config.Graft, logger log.Logger) Client {
	return NewStandaloneClientWithPolicy(policystore.New(cfg.Policy), logger)
}

// NewStandaloneClientWithPolicy is NewStandaloneClient for the policy that store holds, which the
// server reloads: every access check is answered with the policy in force when it runs.
func NewStandaloneClientWithPolicy(store *policystore.Store, logger log.Logger) Client {
	return &standaloneClient{
		policy: store,
		logger: logger.WithField("component", "standalone_client"),
	}
}

// Allowed evaluates the policy for every reference named in params.Changes.
// The actor is params.GLID. A denial is reported as (false, message, nil); a
// malformed change set or an invalid glob in the policy is reported as an
// error.
func (c *standaloneClient) Allowed(_ context.Context, params AllowedParams) (bool, string, error) {
	allowed, message, err := EvaluatePolicy(c.policy.Current().Rules, params.GLID, params.Changes)
	if err != nil || allowed {
		return allowed, message, err
	}

	c.logger.WithFields(log.Fields{
		"actor":  params.GLID,
		"reason": message,
	}).Info("standalone policy denied reference update")

	return false, message, nil
}

// EvaluatePolicy decides whether actor may apply changes under the ordered
// [graft] policy. changes uses the "<old-oid> <new-oid> <ref>" per-line format
// of the pre-receive and reference-transaction hooks. An empty policy allows
// everything. Otherwise every changed reference must be allowed by the first
// rule that applies to it, and a reference that no rule applies to is denied.
// A denial is reported as (false, message, nil); a malformed change set or an
// invalid glob in the policy is reported as an error.
//
// The pre-receive access check of the standalone client and the
// reference-transaction hook both use it, so that a reference update is held to
// the same policy whichever RPC performs it.
func EvaluatePolicy(policy []config.GraftPolicyRule, actor, changes string) (bool, string, error) {
	if len(policy) == 0 {
		return true, "", nil
	}

	refs, err := parseChangedRefs(changes)
	if err != nil {
		return false, "", err
	}

	for _, ref := range refs {
		decision, err := decide(policy, actor, ref)
		if err != nil {
			return false, "", err
		}

		if decision.allowed {
			continue
		}

		if decision.rule < 0 {
			return false, fmt.Sprintf("policy denies %q to update %q: no matching rule", actor, ref), nil
		}

		return false, fmt.Sprintf("policy denies %q to update %q (rule %d)", actor, ref, decision.rule+1), nil
	}

	return true, "", nil
}

// PreReceive always succeeds: there is no GitLab reference counter to increase.
func (c *standaloneClient) PreReceive(context.Context, string) (bool, error) {
	return true, nil
}

// PostReceive always succeeds and produces no messages.
func (c *standaloneClient) PostReceive(context.Context, string, string, string, []byte, ...string) (bool, []PostReceiveMessage, error) {
	return true, nil, nil
}

// ObjectPoolMembers returns no members: object pools are not linked to GitLab
// projects in standalone mode.
func (c *standaloneClient) ObjectPoolMembers(context.Context, []string, string, bool) (map[string][]ObjectPoolMember, error) {
	return map[string][]ObjectPoolMember{}, nil
}

// Check succeeds without contacting anything and describes the standalone mode.
func (c *standaloneClient) Check(context.Context) (*CheckInfo, error) {
	return &CheckInfo{
		Version:    "standalone",
		APIVersion: "none",
	}, nil
}

// policyDecision is the outcome of evaluating the policy for one reference.
type policyDecision struct {
	allowed bool
	// rule is the zero-based index of the deciding rule, or -1 when no rule applies.
	rule int
}

// decide returns the decision of the first rule that applies to actor and ref.
// A reference that no rule applies to is denied.
func decide(policy []config.GraftPolicyRule, actor, ref string) (policyDecision, error) {
	for i, rule := range policy {
		matched, err := ruleApplies(rule, actor, ref)
		if err != nil {
			return policyDecision{}, err
		}

		if matched {
			return policyDecision{allowed: rule.Allow, rule: i}, nil
		}
	}

	return policyDecision{allowed: false, rule: -1}, nil
}

// ruleApplies reports whether rule governs the update of ref by actor. An
// empty or "*" actor matches everybody. Empty Refs match every reference.
func ruleApplies(rule config.GraftPolicyRule, actor, ref string) (bool, error) {
	if rule.Actor != "" && rule.Actor != "*" {
		ok, err := path.Match(rule.Actor, actor)
		if err != nil {
			return false, fmt.Errorf("policy actor %q: %w", rule.Actor, err)
		}

		if !ok {
			return false, nil
		}
	}

	if len(rule.Refs) == 0 {
		return true, nil
	}

	for _, glob := range rule.Refs {
		ok, err := matchRef(glob, ref)
		if err != nil {
			return false, fmt.Errorf("policy ref %q: %w", glob, err)
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}

// matchRef matches a fully qualified reference name against a policy glob.
// Globs use path.Match syntax. A trailing "/**" matches the prefix at any
// depth: "refs/heads/**" matches "refs/heads/main" and "refs/heads/a/b" but
// not "refs/heads" itself. Any other "**" suffix matches any remainder,
// including "/": "refs/heads/release**" matches "refs/heads/release/1.0" and
// "refs/heads/release-2". A bare "**" matches every reference.
func matchRef(glob, ref string) (bool, error) {
	if glob == "**" {
		return true, nil
	}

	prefix, deep := strings.CutSuffix(glob, "/**")
	if !deep {
		if loose, ok := strings.CutSuffix(glob, "**"); ok {
			return matchRefPrefix(loose, ref)
		}

		return path.Match(glob, ref)
	}

	// Validate the pattern up front: path.Match only reports a malformed
	// pattern when it gets to examine it against a candidate.
	if _, err := path.Match(prefix, ""); err != nil {
		return false, err
	}

	// Every "/" in ref ends a candidate directory prefix of ref. Any of them
	// matching the glob prefix means ref lies at or below that directory.
	for i := 0; i < len(ref); i++ {
		if ref[i] != '/' {
			continue
		}

		ok, err := path.Match(prefix, ref[:i])
		if err != nil {
			return false, err
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}

// matchRefPrefix reports whether some prefix of ref (possibly ref itself)
// matches the path.Match pattern prefix, so that the rest of ref is matched by
// a trailing "**".
func matchRefPrefix(prefix, ref string) (bool, error) {
	if _, err := path.Match(prefix, ""); err != nil {
		return false, err
	}

	for i := len(ref); i >= 0; i-- {
		ok, err := path.Match(prefix, ref[:i])
		if err != nil {
			return false, err
		}

		if ok {
			return true, nil
		}
	}

	return false, nil
}

// asciiWhitespace is the whitespace that parseChangedRefs trims around a line.
const asciiWhitespace = " \t\n\v\f\r"

// parseChangedRefs extracts the reference names from a change set in the
// "<old-oid> <new-oid> <ref>" per-line format used by the hooks. The reference
// is taken verbatim, see changelog.ParseHookLine, so that a reference whose
// name holds Unicode whitespace is matched by its real name. Only ASCII
// whitespace, which reference names cannot hold, is trimmed around a line.
// Blank lines are skipped; any other line that is not well formed is an error.
func parseChangedRefs(changes string) ([]string, error) {
	var refs []string

	for _, line := range strings.Split(changes, "\n") {
		line = strings.Trim(line, asciiWhitespace)
		if line == "" {
			continue
		}

		_, _, reference, ok := changelog.ParseHookLine(line)
		if !ok {
			return nil, fmt.Errorf("malformed change %q: expected \"<old> <new> <ref>\"", line)
		}

		refs = append(refs, reference)
	}

	return refs, nil
}
