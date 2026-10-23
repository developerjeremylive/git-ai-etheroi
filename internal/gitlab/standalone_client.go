package gitlab

import (
	"context"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/policyeval"
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
	allowed, message, err := policyeval.Evaluate(c.policy.Current().Rules, params.GLID, params.Changes)
	if err != nil || allowed {
		return allowed, message, err
	}

	c.logger.WithFields(log.Fields{
		"actor":  params.GLID,
		"reason": message,
	}).Info("standalone policy denied reference update")

	return false, message, nil
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
