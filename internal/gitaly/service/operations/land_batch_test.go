package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/landlane"
	"github.com/feder-cr/git-ai/internal/graft/policyeval"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// scriptedUpdate is the outcome of the next Update call of fakeLandStepper.
type scriptedUpdate struct {
	kind landUpdateKind
	err  error
}

// fakeUpdate records one Update call of fakeLandStepper.
type fakeUpdate struct {
	members   []changelog.Member
	rewritten bool
	old, new  git.ObjectID
}

// fakeLandStepper is an in-memory branch. Commits are identified by their commit ID, and a landing
// fast-forwards the tip onto the commit unless the test scripts otherwise.
type fakeLandStepper struct {
	tip git.ObjectID
	// tipIndependent marks the commits whose failed landing does not depend on the tip, as an unparsable
	// commit ID does.
	tipIndependent map[string]bool
	// landErr fails the landing of a commit.
	landErr map[string]error
	// landed marks commits that are already reachable from the tip.
	landed map[string]bool
	// dropped marks commits whose changes the tip already holds, so the rebase drops them.
	dropped map[string]bool
	// landedOnTop marks commits that are reachable from the given tip only, as the base of a
	// stacked change is reachable from the change on top of it.
	landedOnTop map[string]git.ObjectID
	// conflictsOnTop marks commits that conflict with the given tip only.
	conflictsOnTop map[string]git.ObjectID
	// rewrite marks commits that are rewritten when they land.
	rewrite map[string]bool
	// resolutions holds the resolution of commits that are resolved when they land.
	resolutions map[string]*landResolution

	landCalls []string
	updates   []fakeUpdate
	fallbacks []string
	// script holds the outcomes of the next Update calls. Once it is empty, Update performs the
	// compare-and-swap against tip.
	script []scriptedUpdate
}

func (f *fakeLandStepper) Tip(_ context.Context, _ git.ReferenceName) (git.ObjectID, error) {
	return f.tip, nil
}

func (f *fakeLandStepper) Land(_ context.Context, tip git.ObjectID, req *landBatchRequest) (landStep, error) {
	commit := req.in.GetCommitId()
	f.landCalls = append(f.landCalls, commit)

	if err := f.landErr[commit]; err != nil {
		return landStep{tipIndependent: f.tipIndependent[commit]}, err
	}

	id := git.ObjectID(commit)
	if tip == "" {
		if !req.in.GetCreateBranch() {
			return landStep{}, structerr.NewNotFound("branch does not exist")
		}
		return landStep{commit: id, tip: id, created: true}, nil
	}

	if top, ok := f.conflictsOnTop[commit]; ok && top == tip {
		// The error carries the merge conflict detail, as the errors of the real landing do.
		return landStep{}, landMergeConflictError(tip, git.ObjectID(commit), [][]byte{[]byte("a")}, fmt.Errorf("merge conflict with %s", top))
	}

	switch {
	case f.landed[commit]:
		return landStep{commit: id, tip: tip, alreadyLanded: true}, nil
	case f.landedOnTop[commit] != "" && f.landedOnTop[commit] == tip:
		return landStep{commit: id, tip: tip, alreadyLanded: true}, nil
	case f.dropped[commit]:
		return landStep{commit: id, tip: tip}, nil
	default:
		return landStep{
			commit:     id,
			tip:        id,
			rewritten:  f.rewrite[commit],
			resolution: f.resolutions[commit],
		}, nil
	}
}

func (f *fakeLandStepper) Update(_ context.Context, _ git.ReferenceName, members []changelog.Member, rewritten bool, old, newTip git.ObjectID) (landUpdateKind, error) {
	f.updates = append(f.updates, fakeUpdate{members: members, rewritten: rewritten, old: old, new: newTip})

	if len(f.script) > 0 {
		next := f.script[0]
		f.script = f.script[1:]

		switch next.kind {
		case landUpdateLost:
			// A concurrent writer moved the branch.
			f.tip = "external"
		case landUpdated:
			f.tip = newTip
		}

		return next.kind, next.err
	}

	if old != f.tip {
		f.tip = "external"
		return landUpdateLost, errors.New("reference moved")
	}

	f.tip = newTip
	return landUpdated, nil
}

func newFakeLandExecutor(s *fakeLandStepper, maxRebuilds int) landBatchExecutor {
	return landBatchExecutor{
		maxRebuilds: maxRebuilds,
		newStepper: func(context.Context) (landBatchStepper, func(), error) {
			return s, func() {}, nil
		},
		fallback: func(req *landBatchRequest) (*gitalypb.UserLandResponse, error) {
			s.fallbacks = append(s.fallbacks, req.in.GetCommitId())
			return &gitalypb.UserLandResponse{LandedCommitId: req.in.GetCommitId()}, nil
		},
		giveUp: func(_ *landBatchRequest, cause error, attempts int) error {
			return fmt.Errorf("gave up after %d attempts: %w", attempts, cause)
		},
		backoff: func(context.Context, int) error { return nil },
	}
}

// testLandRequests returns requests for the commits on main. Each request is from the agent of its
// commit, and creates the branch when createBranch is set.
func testLandRequests(createBranch bool, commits ...string) []*landBatchRequest {
	reqs := make([]*landBatchRequest, 0, len(commits))
	for _, commit := range commits {
		reqs = append(reqs, &landBatchRequest{
			ctx: context.Background(),
			in: &gitalypb.UserLandRequest{
				CommitId:     commit,
				CreateBranch: createBranch,
				Branch:       []byte(git.DefaultBranch),
			},
			actor: "agent-" + commit,
		})
	}
	return reqs
}

func TestLandBatchExecutorLandsBatchWithOneUpdate(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{tip: "main0"}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b", "c"))

	require.Len(t, stepper.updates, 1)
	require.Equal(t, []string{"a", "b", "c"}, stepper.updates[0].members2Commits())
	require.Equal(t, git.ObjectID("main0"), stepper.updates[0].old)
	require.Equal(t, git.ObjectID("c"), stepper.updates[0].new)
	require.False(t, stepper.updates[0].rewritten)

	// The old tip of a request is the running tip it landed onto, so every request of the batch
	// reports the tip that the previous request left.
	oldTips := []git.ObjectID{"main0", "a", "b"}
	for pos, commit := range []string{"a", "b", "c"} {
		require.NoError(t, outcomes[pos].Err)
		require.Equal(t, git.ObjectID(commit), outcomes[pos].LandedCommit)
		require.Equal(t, oldTips[pos], outcomes[pos].OldTip)
		require.Equal(t, 3, outcomes[pos].BatchSize)
		require.Equal(t, pos, outcomes[pos].BatchPosition)
		require.Equal(t, 1, outcomes[pos].Attempts)
	}
}

func TestLandBatchExecutorRecordsMembersInChangelog(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip:         "main0",
		rewrite:     map[string]bool{"b": true},
		resolutions: map[string]*landResolution{"b": {resolvedPaths: [][]byte{[]byte("shared.txt")}}},
	}
	newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b"))

	require.Len(t, stepper.updates, 1)
	require.True(t, stepper.updates[0].rewritten)
	require.Equal(t, []changelog.Member{
		{OriginalCommit: "a", Commit: "a", Actor: "agent-a"},
		{OriginalCommit: "b", Commit: "b", Actor: "agent-b", Rewritten: true, ResolvedPaths: []string{"shared.txt"}},
	}, stepper.updates[0].members)
}

func TestLandBatchExecutorFailsOnlyTheConflictingRequest(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip:     "main0",
		landErr: map[string]error{"b": structerr.NewFailedPrecondition("landing commit: merge conflict")},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b", "c"))

	require.NoError(t, outcomes[0].Err)
	require.Error(t, outcomes[1].Err)
	require.NoError(t, outcomes[2].Err)

	require.Equal(t, []string{"a", "c"}, stepper.updates[0].members2Commits())
	// Every response of the batch reports the size of the batch and the index of its request in it, though
	// only two of the requests landed.
	require.Equal(t, 3, outcomes[0].BatchSize)
	require.Equal(t, 0, outcomes[0].BatchPosition)
	require.Equal(t, 3, outcomes[2].BatchSize)
	require.Equal(t, 2, outcomes[2].BatchPosition)
}

func TestLandBatchExecutorRebuildsBatchAfterLostCompareAndSwap(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip:    "main0",
		script: []scriptedUpdate{{kind: landUpdateLost, err: errors.New("lost")}},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b"))

	require.Len(t, stepper.updates, 2)
	require.Equal(t, git.ObjectID("main0"), stepper.updates[0].old)
	require.Equal(t, git.ObjectID("external"), stepper.updates[1].old)

	// The rebuilt batch starts on the external tip.
	require.Equal(t, git.ObjectID("external"), outcomes[0].OldTip)
	require.Equal(t, git.ObjectID("a"), outcomes[1].OldTip)
	for pos := range outcomes {
		require.NoError(t, outcomes[pos].Err)
		require.Equal(t, 2, outcomes[pos].Attempts)
	}
}

func TestLandBatchExecutorGivesUpAfterMaxRebuilds(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip: "main0",
		script: []scriptedUpdate{
			{kind: landUpdateLost, err: errors.New("lost")},
			{kind: landUpdateLost, err: errors.New("lost again")},
		},
	}
	outcomes := newFakeLandExecutor(stepper, 1).Execute(context.Background(), testLandRequests(true, "a", "b"))

	require.Len(t, stepper.updates, 2)
	for pos := range outcomes {
		require.ErrorContains(t, outcomes[pos].Err, "gave up after 2 attempts")
		require.ErrorContains(t, outcomes[pos].Err, "lost again")
	}
}

func TestLandBatchExecutorRetriesRequestsAloneAfterHookRejection(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip: "main0",
		script: []scriptedUpdate{{
			kind: landUpdateRejected,
			err:  errors.New("rejected by the pre-receive hook"),
		}},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b"))

	// The combined update is not retried. Each request lands alone, in order.
	require.Len(t, stepper.updates, 1)
	require.Equal(t, []string{"a", "b"}, stepper.fallbacks)

	for pos, commit := range []string{"a", "b"} {
		require.NoError(t, outcomes[pos].Err)
		require.Equal(t, commit, outcomes[pos].Response.GetLandedCommitId())
	}
}

func TestLandBatchExecutorFailedUpdateFailsMembersWithoutRetry(t *testing.T) {
	t.Parallel()

	updateErr := structerr.NewInternal("updating ref with hooks: disk full")
	stepper := &fakeLandStepper{
		tip:    "main0",
		script: []scriptedUpdate{{kind: landUpdateFailed, err: updateErr}},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b"))

	require.Len(t, stepper.updates, 1)
	require.Empty(t, stepper.fallbacks)
	for pos := range outcomes {
		require.ErrorContains(t, outcomes[pos].Err, "disk full")
		require.Equal(t, 1, outcomes[pos].Attempts)
	}
}

func TestLandBatchExecutorFailedUpdateRelandsRequestsThatDependOnMembers(t *testing.T) {
	t.Parallel()

	// "top" is stacked on "base", so "base" is already landed once "top" is in the running tip. The
	// combined update fails, so the branch never gets "top": "base" must not report that it is
	// already landed, but land on its own.
	stepper := &fakeLandStepper{
		tip:         "main0",
		landedOnTop: map[string]git.ObjectID{"base": "top"},
		script:      []scriptedUpdate{{kind: landUpdateFailed, err: structerr.NewInternal("boom")}},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "top", "base"))

	require.ErrorContains(t, outcomes[0].Err, "boom")
	require.Equal(t, 1, outcomes[0].Attempts)

	require.NoError(t, outcomes[1].Err)
	require.False(t, outcomes[1].AlreadyLanded)
	require.Equal(t, git.ObjectID("main0"), outcomes[1].OldTip)
	require.Equal(t, git.ObjectID("base"), outcomes[1].LandedCommit)
	require.Equal(t, 2, outcomes[1].BatchSize)
	require.Equal(t, 1, outcomes[1].BatchPosition)
	require.Equal(t, 2, outcomes[1].Attempts)

	require.Len(t, stepper.updates, 2)
	require.Equal(t, []string{"top"}, stepper.updates[0].members2Commits())
	require.Equal(t, []string{"base"}, stepper.updates[1].members2Commits())
	require.Equal(t, git.ObjectID("base"), stepper.tip)
}

func TestLandBatchExecutorFailedUpdateFailsRelandedDependents(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip:         "main0",
		landedOnTop: map[string]git.ObjectID{"base": "top"},
		script: []scriptedUpdate{
			{kind: landUpdateFailed, err: structerr.NewInternal("boom")},
			{kind: landUpdateFailed, err: structerr.NewInternal("boom again")},
		},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "top", "base"))

	require.ErrorContains(t, outcomes[0].Err, "boom")
	require.ErrorContains(t, outcomes[1].Err, "boom again")
	require.Equal(t, git.ObjectID("main0"), stepper.tip)
}

func TestLandBatchExecutorFailedUpdateKeepsRequestsBeforeFirstMember(t *testing.T) {
	t.Parallel()

	// "pre" is already landed on the published tip, before any member entered the running tip, so its
	// outcome holds. "b" only conflicts with the unpublished member "a", so it lands again without it.
	stepper := &fakeLandStepper{
		tip:            "main0",
		landed:         map[string]bool{"pre": true},
		conflictsOnTop: map[string]git.ObjectID{"b": "a"},
		script:         []scriptedUpdate{{kind: landUpdateFailed, err: structerr.NewInternal("boom")}},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "pre", "a", "b"))

	require.NoError(t, outcomes[0].Err)
	require.True(t, outcomes[0].AlreadyLanded)
	require.Equal(t, git.ObjectID("main0"), outcomes[0].LandedCommit)
	require.Equal(t, 3, outcomes[0].BatchSize)
	require.Equal(t, 1, outcomes[0].Attempts)

	require.ErrorContains(t, outcomes[1].Err, "boom")

	require.NoError(t, outcomes[2].Err)
	require.Equal(t, git.ObjectID("b"), outcomes[2].LandedCommit)
	require.Equal(t, git.ObjectID("main0"), outcomes[2].OldTip)
	require.Equal(t, git.ObjectID("b"), stepper.tip)
}

func TestLandBatchExecutorGivingUpFailsRequestsThatDependOnMembers(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip:            "main0",
		landed:         map[string]bool{"pre": true},
		landedOnTop:    map[string]git.ObjectID{"base": "top"},
		conflictsOnTop: map[string]git.ObjectID{"c": "top"},
		script:         []scriptedUpdate{{kind: landUpdateLost, err: errors.New("lost")}},
	}
	outcomes := newFakeLandExecutor(stepper, 0).Execute(context.Background(), testLandRequests(true, "pre", "top", "base", "c"))

	require.Len(t, stepper.updates, 1)
	require.Equal(t, git.ObjectID("external"), stepper.tip)

	// The request before the first member landed onto the published tip.
	require.NoError(t, outcomes[0].Err)
	require.True(t, outcomes[0].AlreadyLanded)
	require.Equal(t, 4, outcomes[0].BatchSize)

	// Every later request depends on the member that never reached the branch.
	for pos := 1; pos < len(outcomes); pos++ {
		require.ErrorContains(t, outcomes[pos].Err, "gave up after 1 attempts", "request %d", pos)
		require.ErrorContains(t, outcomes[pos].Err, "lost", "request %d", pos)
		require.False(t, outcomes[pos].AlreadyLanded, "request %d", pos)
		require.Empty(t, outcomes[pos].LandedCommit, "request %d", pos)
	}
}

func TestLandBatchExecutorDoesNotUpdateWithoutChange(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip:     "main0",
		landed:  map[string]bool{"a": true},
		dropped: map[string]bool{"b": true},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b"))

	require.Empty(t, stepper.updates)
	require.True(t, outcomes[0].AlreadyLanded)
	require.Equal(t, git.ObjectID("main0"), outcomes[0].LandedCommit)
	require.Equal(t, git.ObjectID("main0"), outcomes[1].LandedCommit)
	require.Equal(t, 2, outcomes[0].BatchSize)
}

func TestLandBatchExecutorDeniedRequestDoesNotLand(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{
		tip:     "main0",
		landErr: map[string]error{"b": structerr.NewPermissionDenied("policy denies agent-b")},
	}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b", "c"))

	require.Equal(t, codes.PermissionDenied, status.Code(outcomes[1].Err))
	require.Equal(t, []string{"a", "c"}, stepper.updates[0].members2Commits())
	require.NoError(t, outcomes[0].Err)
	require.NoError(t, outcomes[2].Err)
}

func TestLandBatchExecutorCreatesBranch(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(true, "a", "b"))

	require.Len(t, stepper.updates, 1)
	require.Equal(t, git.ObjectID(""), stepper.updates[0].old)
	require.Equal(t, git.ObjectID("b"), stepper.updates[0].new)
	require.True(t, outcomes[0].Created)
	require.NoError(t, outcomes[1].Err)
}

func TestLandBatchExecutorFailsMissingBranchPerRequest(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{}
	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), testLandRequests(false, "a", "b"))

	require.Empty(t, stepper.updates)
	for pos := range outcomes {
		require.Equal(t, codes.NotFound, status.Code(outcomes[pos].Err))
	}
}

func TestLandBatchExecutorSkipsCanceledRequest(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{tip: "main0"}
	reqs := testLandRequests(true, "a", "b", "c")

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	reqs[1].ctx = canceled

	outcomes := newFakeLandExecutor(stepper, 3).Execute(context.Background(), reqs)

	require.Equal(t, codes.Canceled, status.Code(outcomes[1].Err))
	require.NotContains(t, stepper.landCalls, "b")
	require.Equal(t, []string{"a", "c"}, stepper.updates[0].members2Commits())
	require.NoError(t, outcomes[0].Err)
	require.NoError(t, outcomes[2].Err)
	require.Equal(t, 3, outcomes[0].BatchSize)
}

func TestLandBatchExecutorEmptyBatch(t *testing.T) {
	t.Parallel()

	stepper := &fakeLandStepper{tip: "main0"}
	require.Empty(t, newFakeLandExecutor(stepper, 3).Execute(context.Background(), nil))
	require.Empty(t, stepper.updates)
}

func TestLandBatchOutcomesBecomeLandResults(t *testing.T) {
	t.Parallel()

	result := landBatchResult(landBatchOutcome{
		OldTip:        "main0",
		LandedCommit:  "c",
		Rewritten:     true,
		BatchSize:     3,
		BatchPosition: 1,
		Attempts:      2,
		Resolution:    &landResolution{resolvedPaths: [][]byte{[]byte("a")}, resolver: "graft-resolve/fake"},
	})

	require.NoError(t, result.Err)
	require.Equal(t, int32(3), result.Value.GetBatchSize())
	// Positions are 1-based in the response.
	require.Equal(t, int32(2), result.Value.GetBatchPosition())
	require.Equal(t, int32(2), result.Value.GetAttempts())
	require.Equal(t, "c", result.Value.GetLandedCommitId())
	require.Equal(t, [][]byte{[]byte("a")}, result.Value.GetResolvedPaths())
	require.Equal(t, "graft-resolve/fake", result.Value.GetResolver())

	failed := landBatchResult(landBatchOutcome{Err: errors.New("boom")})
	require.Error(t, failed.Err)
	require.Nil(t, failed.Value)
}

func TestNewServerSharesTheLandLanesOfTheProcess(t *testing.T) {
	t.Parallel()

	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Graft: config.Graft{Standalone: true}}))
	laneCfg, err := landlane.ConfigFromGraft(cfg.Graft.Land.Batch)
	require.NoError(t, err)

	shared := landlane.NewShared(laneCfg)
	defer shared.Close()

	// serve.go registers the services once per listener, each time with dependencies of its own
	// that carry the lanes of the process.
	newDeps := func(cfg config.Cfg) *service.Dependencies {
		return &service.Dependencies{Cfg: cfg, Logger: testhelper.SharedLogger(t), GraftLandLanes: shared}
	}

	unixServer := NewServer(newDeps(cfg))
	tcpServer := NewServer(newDeps(cfg))
	require.NotNil(t, unixServer.landLanes)
	require.Same(t, unixServer.landLanes, tcpServer.landLanes,
		"requests for one branch must share a lane whichever listener they arrive on")

	disabled := cfg
	disabled.Graft.Land.Batch.Disabled = true
	require.Nil(t, NewServer(newDeps(disabled)).landLanes)

	// Dependencies without lanes, as tests build them, still batch, with lanes of their own.
	alone := NewServer(&service.Dependencies{Cfg: cfg, Logger: testhelper.SharedLogger(t)})
	require.NotNil(t, alone.landLanes)
	require.NotSame(t, unixServer.landLanes, alone.landLanes)
	alone.landLanes.Close()
}

func TestLandBatchIdentityIsSynthetic(t *testing.T) {
	t.Parallel()

	require.Equal(t, "graft-batch", landBatchIdentity.GetGlUsername())
	require.Equal(t, []byte("Graft"), landBatchIdentity.GetName())
	require.Equal(t, []byte("graft@localhost"), landBatchIdentity.GetEmail())
}

func TestEvaluateGraftPolicyForBatchedActors(t *testing.T) {
	t.Parallel()

	policy := config.Graft{Policy: []config.GraftPolicyRule{
		{Actor: "bot-*", Refs: []string{"refs/heads/main"}, Allow: false},
		{Actor: "*", Allow: true},
	}}
	const zero = "0000000000000000000000000000000000000000"
	const newOID = "1111111111111111111111111111111111111111"

	allowed, message, err := policyeval.EvaluateUpdate(policy.Policy, "bot-1", "refs/heads/main", zero, newOID)
	require.NoError(t, err)
	require.False(t, allowed)
	require.Contains(t, message, "bot-1")

	allowed, message, err = policyeval.EvaluateUpdate(policy.Policy, "alice", "refs/heads/main", zero, newOID)
	require.NoError(t, err)
	require.True(t, allowed)
	require.Empty(t, message)
}

// setupLandBatchService starts an operations service with the given Graft configuration.
func setupLandBatchService(t *testing.T, ctx context.Context, graft config.Graft) (context.Context, config.Cfg, gitalypb.OperationServiceClient) {
	t.Helper()

	cfg := testcfg.Build(t, testcfg.WithBase(config.Cfg{Graft: graft}))
	return setupOperationsServiceWithCfg(t, ctx, cfg)
}

// runLandBatchTest runs an integration test of batched landing under the feature sets that the
// landing code checks, as the other UserLand tests do.
func runLandBatchTest(t *testing.T, test func(t *testing.T, ctx context.Context)) {
	testhelper.NewFeatureSets(featureflag.GPGSigning).Run(t, test)
}

// batchingGraft is a standalone configuration in which the lanes collect requests for 50ms.
func batchingGraft() config.Graft {
	return config.Graft{
		Standalone: true,
		Land:       config.GraftLand{Batch: config.GraftLandBatch{Window: "50ms"}},
	}
}

// landAll lands the changes concurrently, one UserLand request each, and returns the responses and
// errors in the order of changes.
func landAll(ctx context.Context, client gitalypb.OperationServiceClient, repo *gitalypb.Repository, changes []git.ObjectID) ([]*gitalypb.UserLandResponse, []error) {
	responses := make([]*gitalypb.UserLandResponse, len(changes))
	errs := make([]error, len(changes))

	var wg sync.WaitGroup
	for i, change := range changes {
		wg.Add(1)
		go func(i int, change git.ObjectID) {
			defer wg.Done()
			responses[i], errs[i] = client.UserLand(ctx, landRequest(repo, git.DefaultBranch, change))
		}(i, change)
	}
	wg.Wait()

	return responses, errs
}

func TestLandBatch_concurrentDisjointLandings(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchConcurrentDisjointLandings)
}

func testLandBatchConcurrentDisjointLandings(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(baseEntry),
	)

	const requests = 30
	changes := make([]git.ObjectID, 0, requests)
	for i := 0; i < requests; i++ {
		name := fmt.Sprintf("file-%02d", i)
		changes = append(changes, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithMessage("land "+name),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}),
		))
	}

	responses, errs := landAll(ctx, client, repoProto, changes)

	maxBatch, attempts := 0, 0
	for i := range changes {
		require.NoError(t, errs[i])

		resp := responses[i]
		require.NotEmpty(t, resp.GetLandedCommitId())
		require.GreaterOrEqual(t, resp.GetBatchPosition(), int32(1))
		require.LessOrEqual(t, resp.GetBatchPosition(), resp.GetBatchSize())

		maxBatch = max(maxBatch, int(resp.GetBatchSize()))
		attempts += int(resp.GetAttempts())
	}

	require.Greater(t, maxBatch, 1, "requests must share a batch")
	require.LessOrEqual(t, attempts, 2*requests)

	// Every change is in the linear history of the branch.
	files := strings.Fields(string(gittest.Exec(t, cfg, "-C", repoPath, "ls-tree", "--name-only", "refs/heads/"+git.DefaultBranch)))
	require.Len(t, files, requests+1)
	require.Empty(t, strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-list", "--merges", "refs/heads/"+git.DefaultBranch))))
}

func TestLandBatch_conflictingMemberFailsAlone(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchConflictingMemberFailsAlone)
}

func testLandBatchConflictingMemberFailsAlone(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	shared := func(content string) gittest.TreeEntry {
		return gittest.TreeEntry{Mode: "100644", Path: "shared", Content: content}
	}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(shared("base")),
	)

	// Both x and y change the same file from the base, so only one of them can land.
	x := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(shared("x")))
	y := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(shared("y")))
	z := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(shared("base"), gittest.TreeEntry{Mode: "100644", Path: "z", Content: "z"}))

	responses, errs := landAll(ctx, client, repoProto, []git.ObjectID{x, y, z})

	require.NoError(t, errs[2], "the disjoint change lands")
	require.NotNil(t, responses[2])

	landed := 0
	for _, i := range []int{0, 1} {
		if errs[i] == nil {
			landed++
			continue
		}
		require.Equal(t, codes.FailedPrecondition, status.Code(errs[i]), "error: %v", errs[i])
	}
	require.Equal(t, 1, landed, "exactly one of the conflicting changes lands")
}

func TestLandBatch_resolveConflictsInsideBatch(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchResolveConflictsInsideBatch)
}

func testLandBatchResolveConflictsInsideBatch(t *testing.T, ctx context.Context) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, validAnswer())

	graft := batchingGraft()
	graft.Land.Resolver = enabledResolver(argv)
	ctx, cfg, client := setupLandBatchService(t, ctx, graft)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	entries := func(a, b string) []gittest.TreeEntry {
		return []gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: a},
			{Mode: "100644", Path: "b", Content: b},
		}
	}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(entries("base", "base")...),
	)
	gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithParents(base),
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(entries("ours", "ours")...),
	)
	conflicting := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(entries("theirs", "theirs")...))
	independent := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c"},
		),
	)

	var (
		wg           sync.WaitGroup
		resolved     *gitalypb.UserLandResponse
		resolvedErr  error
		independentE error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		req := landRequest(repoProto, git.DefaultBranch, conflicting)
		req.ResolveConflicts = true
		resolved, resolvedErr = client.UserLand(ctx, req)
	}()
	go func() {
		defer wg.Done()
		_, independentE = client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, independent))
	}()
	wg.Wait()

	require.NoError(t, resolvedErr)
	require.NoError(t, independentE)
	require.True(t, resolved.GetRewritten())
	require.Equal(t, "graft-resolve/fake", resolved.GetResolver())
	require.Equal(t, [][]byte{[]byte("a"), []byte("b")}, resolved.GetResolvedPaths())
	require.Equal(t, 1, resolverCalls(t, dir))

	require.Equal(t, "merged-a\n", string(gittest.Exec(t, cfg, "-C", repoPath, "show", "refs/heads/"+git.DefaultBranch+":a")))
}

func TestLandBatch_refusalNeverWrites(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchRefusalNeverWrites)
}

// testLandBatchRefusalNeverWrites: a resolver that refuses but marks every file resolved (a
// self-contradicting refusal) leads to no write: the request fails with the merge conflict and its
// package, and the other member of the batch lands.
func testLandBatchRefusalNeverWrites(t *testing.T, ctx context.Context) {
	t.Parallel()

	dir, argv := resolverScript(t, `cat "$DIR/answer.json"; exit 1`)
	writeRawAnswer(t, dir, resolver.ErrorResponse{
		Error: "refused",
		Files: []resolver.FileOutcome{{Path: "a", Outcome: resolver.OutcomeResolved}, {Path: "b", Outcome: resolver.OutcomeResolved}},
	})

	graft := batchingGraft()
	graft.Land.Resolver = enabledResolver(argv)
	ctx, cfg, client := setupLandBatchService(t, ctx, graft)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	entries := func(a, b string) []gittest.TreeEntry {
		return []gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: a},
			{Mode: "100644", Path: "b", Content: b},
		}
	}
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(entries("base", "base")...))
	gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(entries("ours", "ours")...))
	conflicting := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base), gittest.WithTreeEntries(entries("theirs", "theirs")...))
	independent := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base),
		gittest.WithTreeEntries(
			gittest.TreeEntry{Mode: "100644", Path: "a", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "b", Content: "base"},
			gittest.TreeEntry{Mode: "100644", Path: "c", Content: "c"},
		),
	)

	var (
		wg              sync.WaitGroup
		refusedErr      error
		independentResp *gitalypb.UserLandResponse
		independentErr  error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		req := landRequest(repoProto, git.DefaultBranch, conflicting)
		req.ResolveConflicts = true
		_, refusedErr = client.UserLand(ctx, req)
	}()
	go func() {
		defer wg.Done()
		independentResp, independentErr = client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, independent))
	}()
	wg.Wait()

	requireMergeConflict(t, refusedErr)
	pkg := landConflictDetail(refusedErr)
	require.NotNil(t, pkg)
	for _, f := range pkg.GetFiles() {
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_FAILED, f.GetOutcome(), "a self-contradicting refusal is not trusted")
	}
	require.NoError(t, independentErr, "the other member of the batch is released")
	require.Equal(t, "c", string(gittest.Exec(t, cfg, "-C", repoPath, "show", independentResp.GetLandedCommitId()+":c")))
}

func TestLandBatch_externalWriterMovesBranch(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchExternalWriterMovesBranch)
}

func testLandBatchExternalWriterMovesBranch(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	base := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(baseEntry),
	)

	const requests = 10
	changes := make([]git.ObjectID, 0, requests)
	for i := 0; i < requests; i++ {
		name := fmt.Sprintf("file-%02d", i)
		changes = append(changes, gittest.WriteCommit(t, cfg, repoPath,
			gittest.WithParents(base),
			gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: name, Content: name}),
		))
	}

	// An external writer moves the branch while the batches run. Its compare-and-swap runs against
	// the tip it reads, so a batch that started on an older tip is rebuilt on the new one.
	var external []git.ObjectID
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for i := 0; i < 3; i++ {
			tip := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch)
			next := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(tip), gittest.WithMessage(fmt.Sprintf("external %d", i)))
			_, err := client.UserUpdateBranch(ctx, &gitalypb.UserUpdateBranchRequest{
				Repository: repoProto,
				BranchName: []byte(git.DefaultBranch),
				User:       gittest.TestUser,
				Newrev:     []byte(next),
				Oldrev:     []byte(tip),
			})
			if err == nil {
				external = append(external, next)
			}
		}
	}()

	responses, errs := landAll(ctx, client, repoProto, changes)
	<-writerDone

	for i := range changes {
		require.NoError(t, errs[i])
		require.NotEmpty(t, responses[i].GetLandedCommitId())
	}

	tip := gittest.ResolveRevision(t, cfg, repoPath, "refs/heads/"+git.DefaultBranch)
	for _, commit := range external {
		gittest.Exec(t, cfg, "-C", repoPath, "merge-base", "--is-ancestor", commit.String(), tip.String())
	}
	for i := range changes {
		gittest.Exec(t, cfg, "-C", repoPath, "merge-base", "--is-ancestor", responses[i].GetLandedCommitId(), tip.String())
	}
}

func TestLandBatch_customHookRejectionIsAttributed(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchCustomHookRejectionIsAttributed)
}

func testLandBatchCustomHookRejectionIsAttributed(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	baseEntry := gittest.TreeEntry{Mode: "100644", Path: "base", Content: "base"}
	tip := gittest.WriteCommit(t, cfg, repoPath,
		gittest.WithBranch(git.DefaultBranch),
		gittest.WithTreeEntries(baseEntry),
	)

	changes := []git.ObjectID{
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(tip), gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "one", Content: "one"})),
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(tip), gittest.WithTreeEntries(baseEntry, gittest.TreeEntry{Mode: "100644", Path: "two", Content: "two"})),
	}

	gittest.WriteCustomHook(t, repoPath, "pre-receive", []byte("#!/bin/sh\necho 'rejected by policy' >&2\nexit 1\n"))

	responses, errs := landAll(ctx, client, repoProto, changes)
	for i := range changes {
		require.Nil(t, responses[i])
		testhelper.RequireGrpcCode(t, errs[i], codes.PermissionDenied)

		custom := landErrorDetail(t, errs[i]).GetCustomHook()
		require.NotNil(t, custom)
		require.Contains(t, string(custom.GetStderr()), "rejected by policy")
	}

	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, tip)
}

func TestLandBatch_disabledKeepsUnbatchedBehaviour(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchDisabledKeepsUnbatchedBehaviour)
}

func testLandBatchDisabledKeepsUnbatchedBehaviour(t *testing.T, ctx context.Context) {
	t.Parallel()

	graft := batchingGraft()
	graft.Land.Batch.Disabled = true
	ctx, cfg, client := setupLandBatchService(t, ctx, graft)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	change := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))
	require.NoError(t, err)
	require.Equal(t, change.String(), resp.GetLandedCommitId())
	require.Equal(t, int32(1), resp.GetBatchSize(), "a request that does not use the lanes is a batch of one")
	require.Equal(t, int32(1), resp.GetBatchPosition())
	requireBranchAt(t, cfg, repoPath, git.DefaultBranch, change)
}

func TestLandBatch_nonStandaloneIsUnbatched(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchNonStandaloneIsUnbatched)
}

func testLandBatchNonStandaloneIsUnbatched(t *testing.T, ctx context.Context) {
	t.Parallel()

	graft := batchingGraft()
	graft.Standalone = false
	ctx, cfg, client := setupLandBatchService(t, ctx, graft)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch))
	change := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(base))

	resp, err := client.UserLand(ctx, landRequest(repoProto, git.DefaultBranch, change))
	require.NoError(t, err)
	require.Equal(t, int32(1), resp.GetBatchSize())
	require.Equal(t, int32(1), resp.GetBatchPosition())
}

func TestLandBatch_missingBranchIsUnbatched(t *testing.T) {
	t.Parallel()

	runLandBatchTest(t, testLandBatchMissingBranchIsUnbatched)
}

func testLandBatchMissingBranchIsUnbatched(t *testing.T, ctx context.Context) {
	t.Parallel()

	ctx, cfg, client := setupLandBatchService(t, ctx, batchingGraft())
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)

	commit := gittest.WriteCommit(t, cfg, repoPath)

	req := landRequest(repoProto, "feature", commit)
	req.CreateBranch = true
	resp, err := client.UserLand(ctx, req)
	require.NoError(t, err)
	require.True(t, resp.GetBranchUpdate().GetBranchCreated())
	require.Equal(t, int32(1), resp.GetBatchSize())
	requireBranchAt(t, cfg, repoPath, "feature", commit)
}

// members2Commits returns the commit IDs of the members of the update.
func (u fakeUpdate) members2Commits() []string {
	commits := make([]string, 0, len(u.members))
	for _, member := range u.members {
		commits = append(commits, member.Commit)
	}
	return commits
}
