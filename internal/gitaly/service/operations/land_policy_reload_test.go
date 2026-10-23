package operations

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

// policyReloader reloads the policy of a running service at a point of a land: once armed, right
// before the next reference-transaction hook reaches the hook manager. That is after the land's
// policy check and its pre-receive, which decided with the policy before, and before the hook
// decides in the prepared phase.
type policyReloader struct {
	hook.Manager
	store   *policystore.Store
	running config.Graft
	armed   atomic.Pointer[[]config.GraftPolicyRule]
	fired   atomic.Bool
	// failure is the error of a reload that did not apply. The hook runs on a goroutine of the
	// server, which must not fail the test: the error is returned to the hook's caller and kept
	// for requireReloaded.
	failure atomic.Pointer[error]
}

// requireReloaded asserts that the armed reload ran and applied its policy.
func (r *policyReloader) requireReloaded(t *testing.T) {
	t.Helper()
	require.True(t, r.fired.Load(), "the policy was reloaded in flight")
	if failure := r.failure.Load(); failure != nil {
		require.NoError(t, *failure, "the reload in flight")
	}
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
		if err == nil && outcome.Result != policystore.Applied {
			err = fmt.Errorf("the reload in flight was %s, not applied", outcome.Result)
		}
		r.fired.Store(true)
		if err != nil {
			r.failure.Store(&err)
			return err
		}
	}
	return r.Manager.ReferenceTransactionHook(ctx, state, env, stdin)
}

// setupPolicyReload starts the agents' land service of graft, with more server options, with a
// policyReloader around its hook manager.
func setupPolicyReload(t *testing.T, ctx context.Context, graft config.Graft, options ...testserver.GitalyServerOpt) (context.Context, config.Cfg, agentLandClient, *policyReloader) {
	reloader := &policyReloader{store: policystore.New(graft.Policy), running: graft}
	ctx, cfg, client := setupAgentLandServiceWith(t, ctx, graft, reloader.store, append(options, testserver.WithHookManagerWrapper(func(manager hook.Manager) hook.Manager {
		reloader.Manager = manager
		return reloader
	}))...)
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

		logger := testhelper.NewLogger(t)
		logs := testhelper.AddLoggerHook(logger)
		ctx, cfg, client, reloader := setupPolicyReload(t, ctx, unbatchedGraft(nil), testserver.WithLogger(logger))
		repo, repoPath, base, change := policyReloadRepo(t, ctx, cfg)

		reloader.arm(denyMainFor("bot"))
		_, errs := landInOrder(t, ctx, client, repo, agentLanding{agent: "bot", commit: change("b")})

		reloader.requireReloaded(t)
		testhelper.RequireGrpcCode(t, errs[0], codes.PermissionDenied)
		require.Equal(t, base.String(), text.ChompBytes(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "refs/heads/"+git.DefaultBranch)))
		// The hook denied it, with the policy of the reload, not an earlier check.
		var deniedByHook bool
		for _, entry := range logs.AllEntries() {
			if entry.Message == "agent reference update denied" && entry.Data["actor"] == "bot" {
				require.Equal(t, uint64(2), entry.Data["policy_generation"])
				deniedByHook = true
			}
		}
		require.True(t, deniedByHook, "the reference-transaction hook denied the update")
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

	t.Run("a batch admitted under another policy lands its requests alone, and the newly denied one fails alone", func(t *testing.T) {
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

		reloader.requireReloaded(t)
		testhelper.RequireGrpcCode(t, errs[0], codes.PermissionDenied)
		require.Equal(t, "bot", landErrorDetail(t, errs[0]).GetAccessCheck().GetUserId())
		require.NoError(t, errs[1])
		require.NoError(t, errs[2])
		require.ElementsMatch(t, []string{"base", "a", "c"}, mainFiles(t, cfg, repoPath))
	})
}

// TestUserLand_accessCheckNamesTheAgent: the denial of an agent's land names the agent as the user
// of its access check whichever check denied it: the admission of a batch, the pre-receive access
// check of a land that does not batch, or the pre-receive of a member that lands alone after its
// batch was refused because the policy changed.
func TestUserLand_accessCheckNamesTheAgent(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandAccessCheckNamesTheAgent)
}

func testUserLandAccessCheckNamesTheAgent(t *testing.T, ctx context.Context) {
	t.Parallel()

	requireDeniedAs := func(t *testing.T, err error, agent string) {
		t.Helper()
		testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)
		require.Equal(t, agent, landErrorDetail(t, err).GetAccessCheck().GetUserId())
	}

	t.Run("the admission of a batch", func(t *testing.T) {
		t.Parallel()

		ctx, cfg, client := setupAgentLandService(t, ctx, agentBatchingGraft(2, denyMainFor("bot")))
		repo, _, _, change := policyReloadRepo(t, ctx, cfg)

		_, errs := landInOrder(t, ctx, client, repo,
			agentLanding{agent: "bot", commit: change("b")},
			agentLanding{agent: "alice", commit: change("a")},
		)
		requireDeniedAs(t, errs[0], "bot")
		require.NoError(t, errs[1])
	})

	t.Run("the access check of a land that does not batch", func(t *testing.T) {
		t.Parallel()

		ctx, cfg, client := setupAgentLandService(t, ctx, unbatchedGraft(denyMainFor("bot")))
		repo, _, _, change := policyReloadRepo(t, ctx, cfg)

		_, errs := landInOrder(t, ctx, client, repo, agentLanding{agent: "bot", commit: change("b")})
		requireDeniedAs(t, errs[0], "bot")
	})

	t.Run("the access check of a member that lands alone", func(t *testing.T) {
		t.Parallel()

		ctx, cfg, client, reloader := setupPolicyReload(t, ctx, agentBatchingGraft(2, nil))
		repo, _, _, change := policyReloadRepo(t, ctx, cfg)

		reloader.arm(denyMainFor("bot"))
		_, errs := landInOrder(t, ctx, client, repo,
			agentLanding{agent: "bot", commit: change("b")},
			agentLanding{agent: "alice", commit: change("a")},
		)
		reloader.requireReloaded(t)
		requireDeniedAs(t, errs[0], "bot")
		require.NoError(t, errs[1])
	})
}

// TestUserLand_policyReloadsDuringConcurrentLands lands changes of several agents concurrently
// while the policy alternates between two versions, with invalid reloads in between (PLAN12,
// section 8). agent-0 is allowed by one version and denied by the other. Every land either lands
// or is denied; every decision of the hook names one of the two versions, never another one or
// none; and every update of agent-0 that the hook allowed was allowed by the version that allows
// it.
func TestUserLand_policyReloadsDuringConcurrentLands(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testUserLandPolicyReloadsDuringConcurrentLands)
}

func testUserLandPolicyReloadsDuringConcurrentLands(t *testing.T, ctx context.Context) {
	t.Parallel()

	allowAll := []config.GraftPolicyRule{{Actor: "*", Allow: true}}
	t.Run("unbatched", func(t *testing.T) {
		t.Parallel()
		concurrentLandsDuringReloads(t, ctx, unbatchedGraft(allowAll), false)
	})
	t.Run("batched", func(t *testing.T) {
		t.Parallel()
		concurrentLandsDuringReloads(t, ctx, config.Graft{
			Standalone: true,
			Policy:     allowAll,
			Land:       config.GraftLand{Batch: config.GraftLandBatch{Window: "20ms"}},
		}, true)
	})
}

// concurrentLandsDuringReloads is the test of TestUserLand_policyReloadsDuringConcurrentLands on a
// service of graft, whose policy allows every agent. When batched, the hook decides only the
// requests that land alone, so the checks of its decisions that need one for every land are not
// made; a batch admitted under a generation that is no longer in force is refused by the hook.
func concurrentLandsDuringReloads(t *testing.T, ctx context.Context, graft config.Graft, batched bool) {
	const agents, landsPerAgent = 4, 4
	allowAll := graft.Policy
	denyAgent0 := denyMainFor("agent-0")
	versions := []any{policystore.Hash(allowAll), policystore.Hash(denyAgent0)}

	// The allowed updates are logged at DEBUG.
	logger := testhelper.NewLogger(t, testhelper.WithLevel(logrus.DebugLevel))
	logs := testhelper.AddLoggerHook(logger)
	ctx, cfg, client, reloader := setupPolicyReload(t, ctx, graft, testserver.WithLogger(logger))
	repo, repoPath, _, change := policyReloadRepo(t, ctx, cfg)

	commits := make([][]git.ObjectID, agents)
	for a := range commits {
		for i := 0; i < landsPerAgent; i++ {
			commits[a] = append(commits[a], change(fmt.Sprintf("agent-%d-%d", a, i)))
		}
	}

	// The policy alternates between the two versions every millisecond until the lands are over;
	// between each two, an invalid policy and an unreadable configuration are rejected. The store
	// is read after each step: it always holds one of the two versions.
	landsDone := make(chan struct{})
	reloadsDone := make(chan []string)
	go func() {
		var problems []string
		check := func(step string, outcome policystore.Outcome) {
			current := reloader.store.Current()
			if current == nil || !slices.Contains(versions, any(current.SHA256)) {
				problems = append(problems, fmt.Sprintf("%s: the policy in force is neither version: %+v", step, current))
			}
			if outcome.Result == policystore.Rejected && outcome.Current != outcome.Previous {
				problems = append(problems, step+": a rejected reload replaced the policy")
			}
		}
		reload := func(rules []config.GraftPolicyRule) (policystore.Outcome, error) {
			next := reloader.running
			next.Policy = rules
			return reloader.store.Reload(next, reloader.running)
		}

		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-landsDone:
				reloadsDone <- problems
				return
			case <-ticker.C:
			}

			outcome, err := reload([]config.GraftPolicyRule{{Actor: "*", Refs: []string{""}, Allow: true}})
			if err == nil || outcome.Result != policystore.Rejected {
				problems = append(problems, "an invalid policy was not rejected")
			}
			check("invalid policy", outcome)
			check("unreadable configuration", reloader.store.Reject())

			rules := allowAll
			if i%2 == 0 {
				rules = denyAgent0
			}
			outcome, err = reload(rules)
			if err != nil {
				problems = append(problems, fmt.Sprintf("a valid policy was rejected: %v", err))
			}
			check("valid policy", outcome)
		}
	}()

	var wg sync.WaitGroup
	errs := make([][]error, agents)
	// lastStart is the generation in force when each agent's last land started. The hook decides
	// after that, so it decides with that generation or a later one.
	lastStart := make([]uint64, agents)
	for a := 0; a < agents; a++ {
		errs[a] = make([]error, landsPerAgent)
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			agentCtx := metadata.AppendToOutgoingContext(ctx, testAgentHeader, fmt.Sprintf("agent-%d", a))
			for i, commit := range commits[a] {
				lastStart[a] = reloader.store.Current().Generation
				_, errs[a][i] = client.UserLand(agentCtx, landRequest(repo, git.DefaultBranch, commit))
			}
		}(a)
	}
	wg.Wait()
	close(landsDone)
	require.Empty(t, <-reloadsDone)
	require.Greater(t, reloader.store.Current().Generation, uint64(2), "the policy alternated while the agents landed")

	landed := map[string]int{}
	for a := range errs {
		for i, err := range errs[a] {
			if err != nil {
				testhelper.RequireGrpcCode(t, err, codes.PermissionDenied)
				require.Zero(t, a, "only agent-0 is ever denied, land %d", i)
				continue
			}
			landed[fmt.Sprintf("agent-%d", a)]++
		}
	}
	for a := 1; a < agents; a++ {
		require.Equal(t, landsPerAgent, landed[fmt.Sprintf("agent-%d", a)])
	}

	allowedOfAgent0, decisions := 0, 0
	lastDecided := map[any]uint64{}
	for _, entry := range logs.AllEntries() {
		allowed := entry.Message == "agent reference update allowed by the policy"
		if !allowed && entry.Message != "agent reference update denied" {
			continue
		}
		decisions++
		sha := entry.Data["policy_sha256"]
		require.Contains(t, versions, sha, "a decision names one of the two versions")
		generation, ok := entry.Data["policy_generation"].(uint64)
		require.True(t, ok, "a decision names its generation: %v", entry.Data["policy_generation"])
		lastDecided[entry.Data["actor"]] = generation
		if entry.Data["actor"] != "agent-0" {
			continue
		}
		if allowed {
			require.Equal(t, policystore.Hash(allowAll), sha, "agent-0 is allowed only by the version that allows it")
			allowedOfAgent0++
		} else {
			require.Equal(t, policystore.Hash(denyAgent0), sha, "agent-0 is denied only by the version that denies it")
		}
	}
	if !batched {
		require.LessOrEqual(t, landed["agent-0"], allowedOfAgent0, "every land of agent-0 was allowed by the hook")
		require.GreaterOrEqual(t, decisions, landsPerAgent*(agents-1)+landed["agent-0"], "the hook decided every land")
		// The agents that every version allows reach the hook with their last land: the hook
		// decided it with the policy in force then, not an older one.
		for a := 1; a < agents; a++ {
			require.Greater(t, lastStart[a], uint64(1), "agent-%d's last land started after reloads", a)
			require.GreaterOrEqual(t, lastDecided[fmt.Sprintf("agent-%d", a)], lastStart[a], "agent-%d's last land was decided by the policy in force", a)
		}
	}

	// The changeset log records each land with the agent that made it, the member of a batched
	// land with its own: as many as landed for each agent, none for a denied land.
	entries, err := changelog.List(repoPath, 0, 1000)
	require.NoError(t, err)
	recorded := map[string]int{}
	for _, entry := range entries {
		if entry.Reference != git.DefaultRef.String() {
			continue
		}
		if entry.Operation == changelog.OperationLandBatch {
			for _, member := range entry.Members {
				recorded[member.Actor]++
			}
			continue
		}
		recorded[entry.Actor]++
	}
	require.Equal(t, landed, recorded)

	require.Len(t, mainFiles(t, cfg, repoPath), 1+landsPerAgent*(agents-1)+landed["agent-0"])
}

// TestServer_graftInForce: a policy check decides with the snapshot its context carries, which a
// land batch attempt gives its members, even after a reload; without one, with the policy in force.
func TestServer_graftInForce(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	admitting := []config.GraftPolicyRule{{Actor: "*", Allow: true}}
	reloaded := denyMainFor("bot")
	store := policystore.New(admitting)
	s := &Server{graft: config.Graft{Standalone: true}, graftPolicy: store}
	snapshot := store.Current()

	outcome, err := store.Reload(config.Graft{Standalone: true, Policy: reloaded}, config.Graft{Standalone: true})
	require.NoError(t, err)
	require.Equal(t, policystore.Applied, outcome.Result)

	require.Equal(t, reloaded, s.graftInForce(ctx).Policy)
	require.Equal(t, admitting, s.graftInForce(policystore.WithSnapshot(ctx, snapshot)).Policy)
}
