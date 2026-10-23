package operations

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
)

// policyReloader reloads the policy of a running service at a point of a land: once armed, right
// before the next reference-transaction hook reaches the hook manager. That is after the land's
// policy check and its pre-receive, which decided with the policy before, and before the hook
// decides in the prepared phase.
type policyReloader struct {
	hook.Manager
	t       *testing.T
	store   *policystore.Store
	running config.Graft
	armed   atomic.Pointer[[]config.GraftPolicyRule]
	fired   atomic.Bool
}

// arm makes the next reference-transaction hook reload the policy to rules.
func (r *policyReloader) arm(rules []config.GraftPolicyRule) {
	r.armed.Store(&rules)
}

// ReferenceTransactionHook reloads the policy when armed, then runs the hook.
func (r *policyReloader) ReferenceTransactionHook(ctx context.Context, state hook.ReferenceTransactionState, env []string, stdin io.Reader) error {
	if rules := r.armed.Swap(nil); rules != nil {
		next := r.running
		next.Policy = *rules
		outcome, err := r.store.Reload(next, r.running)
		require.NoError(r.t, err)
		require.Equal(r.t, policystore.Applied, outcome.Result)
		r.fired.Store(true)
	}
	return r.Manager.ReferenceTransactionHook(ctx, state, env, stdin)
}

// setupPolicyReload starts the agents' land service of graft with a policyReloader around its hook
// manager.
func setupPolicyReload(t *testing.T, ctx context.Context, graft config.Graft) (context.Context, config.Cfg, agentLandClient, *policyReloader) {
	reloader := &policyReloader{t: t, store: policystore.New(graft.Policy), running: graft}
	ctx, cfg, client := setupAgentLandServiceWith(t, ctx, graft, reloader.store, testserver.WithHookManagerWrapper(func(manager hook.Manager) hook.Manager {
		reloader.Manager = manager
		return reloader
	}))
	return ctx, cfg, client, reloader
}

// policyReloadRepo is a repository whose main is at base, and changes of it, each adding a file.
func policyReloadRepo(t *testing.T, ctx context.Context, cfg config.Cfg) (*gitalypb.Repository, string, git.ObjectID, func(string) git.ObjectID) {
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(baseEntry))
	change := func(name string) git.ObjectID {
		return gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}))
	}
	return repoProto, repoPath, base, change
}

// unbatchedGraft is a standalone configuration with policy whose lands do not batch, so that the
// reference-transaction hook decides each agent update.
func unbatchedGraft(policy []config.GraftPolicyRule) config.Graft {
	return config.Graft{
		Standalone: true,
		Policy:     policy,
		Land:       config.GraftLand{Batch: config.GraftLandBatch{Disabled: true}},
	}
}

func mainFiles(t *testing.T, cfg config.Cfg, repoPath string) []string {
	return strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "ls-tree", "--name-only", "refs/heads/"+git.DefaultBranch)))
}

// TestUserLand_policyReloadedInFlight reloads the policy while a land is in flight: a tightening
// applies to an update that reaches the reference-transaction hook after it, whatever the earlier
// checks allowed (PLAN12, section 5).
func TestUserLand_policyReloadedInFlight(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandPolicyReloadedInFlight)
}

func testUserLandPolicyReloadedInFlight(t *testing.T, ctx context.Context) {
	t.Parallel()

	t.Run("a tightening refuses an update its earlier checks allowed", func(t *testing.T) {
		t.Parallel()

		ctx, cfg, client, reloader := setupPolicyReload(t, ctx, unbatchedGraft(nil))
		repo, repoPath, base, change := policyReloadRepo(t, ctx, cfg)

		reloader.arm(denyMainFor("bot"))
		_, errs := landInOrder(t, ctx, client, repo, agentLanding{agent: "bot", commit: change("b")})

		require.True(t, reloader.fired.Load(), "the policy was reloaded in flight")
		testhelper.RequireGrpcCode(t, errs[0], codes.PermissionDenied)
		require.Equal(t, base.String(), text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "refs/heads/"+git.DefaultBranch)))
	})

	t.Run("a loosening lets the refused land again", func(t *testing.T) {
		t.Parallel()

		ctx, cfg, client, reloader := setupPolicyReload(t, ctx, unbatchedGraft(denyMainFor("bot")))
		repo, repoPath, _, change := policyReloadRepo(t, ctx, cfg)
		commit := change("b")

		_, errs := landInOrder(t, ctx, client, repo, agentLanding{agent: "bot", commit: commit})
		testhelper.RequireGrpcCode(t, errs[0], codes.PermissionDenied)

		next := reloader.running
		next.Policy = nil
		outcome, err := reloader.store.Reload(next, reloader.running)
		require.NoError(t, err)
		require.Equal(t, policystore.Applied, outcome.Result)

		_, errs = landInOrder(t, ctx, client, repo, agentLanding{agent: "bot", commit: commit})
		require.NoError(t, errs[0])
		require.ElementsMatch(t, []string{"base", "b"}, mainFiles(t, cfg, repoPath))
	})

	t.Run("a batch admitted under another policy is admitted again, and the newly denied member fails alone", func(t *testing.T) {
		t.Parallel()

		ctx, cfg, client, reloader := setupPolicyReload(t, ctx, agentBatchingGraft(3, nil))
		repo, repoPath, _, change := policyReloadRepo(t, ctx, cfg)

		// The first reference-transaction hook is the batch's combined update, admitted with the
		// policy before the reload.
		reloader.arm(denyMainFor("bot"))
		_, errs := landInOrder(t, ctx, client, repo,
			agentLanding{agent: "bot", commit: change("b")},
			agentLanding{agent: "alice", commit: change("a")},
			agentLanding{agent: "carol", commit: change("c")},
		)

		require.True(t, reloader.fired.Load(), "the policy was reloaded in flight")
		testhelper.RequireGrpcCode(t, errs[0], codes.PermissionDenied)
		// Denied alone, by its access check: the detail is the access check's, whose user ID is the
		// request's (pre-receive), not the agent's name as an admission in a batch reports it.
		require.NotNil(t, landErrorDetail(t, errs[0]).GetAccessCheck())
		require.NoError(t, errs[1])
		require.NoError(t, errs[2])
		require.ElementsMatch(t, []string{"base", "a", "c"}, mainFiles(t, cfg, repoPath))
	})
}
