// Package policyeval evaluates the standalone access policy ([[graft.policy]]) for reference
// updates. The pre-receive access check of the standalone client, the reference-transaction hook,
// the admission of a land batch and graft policy check all decide with it, so that an update is
// held to the same policy whoever checks it. It depends on no part of the server, so that the
// clients build for every platform.
package policyeval

import (
	"fmt"
	"path"
	"strings"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/refchange"
)

// EvaluateUpdate evaluates policy for the update of reference from oldOID to newOID by actor. It
// applies the same rules as the reference-transaction hook: references outside refs/ and HEAD are
// not governed. When allowed is false, message holds the reason of the denial. The caller decides
// whether the policy applies to the update, for example only to agent callers, as the hook does.
func EvaluateUpdate(policy []config.GraftPolicyRule, actor, reference, oldOID, newOID string) (bool, string, error) {
	changes := fmt.Sprintf("%s %s %s\n", oldOID, newOID, reference)

	message, err := Denial(policy, actor, []byte(changes))
	if err != nil {
		return false, "", fmt.Errorf("evaluating graft policy: %w", err)
	}

	return message == "", message, nil
}

// Denial returns the message of the denial of the update of the governed references in changes
// by actor, or an empty message when the policy allows the update. Only references under refs/
// and HEAD are governed, see isGovernedReference.
func Denial(policy []config.GraftPolicyRule, actor string, changes []byte) (string, error) {
	var governed strings.Builder
	for _, line := range strings.Split(string(changes), "\n") {
		// Lines that are not well formed are kept, so that the policy rejects them.
		if _, _, reference, ok := refchange.ParseHookLine(line); ok && !isGovernedReference(reference) {
			continue
		}

		governed.WriteString(line)
		governed.WriteString("\n")
	}

	allowed, message, err := Evaluate(policy, actor, governed.String())
	if err != nil {
		return "", err
	}

	if allowed {
		return "", nil
	}

	return message, nil
}

// isGovernedReference reports whether the standalone access policy governs an
// update of reference: every reference under refs/, and HEAD.
func isGovernedReference(reference string) bool {
	return reference == "HEAD" || strings.HasPrefix(reference, "refs/")
}

// Evaluate decides whether actor may apply changes under the ordered
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
func Evaluate(policy []config.GraftPolicyRule, actor, changes string) (bool, string, error) {
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
// is taken verbatim, see refchange.ParseHookLine, so that a reference whose
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

		_, _, reference, ok := refchange.ParseHookLine(line)
		if !ok {
			return nil, fmt.Errorf("malformed change %q: expected \"<old> <new> <ref>\"", line)
		}

		refs = append(refs, reference)
	}

	return refs, nil
}
