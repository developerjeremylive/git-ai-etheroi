package operations

import (
	"errors"
	"fmt"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/updateref"
	gitalycfg "github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/proposals"
	"github.com/feder-cr/git-ai/internal/graft/resolver"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// modelAnswer is validAnswer as the llm strategy gives it: both files proposals.
func modelAnswer() resolver.Response {
	answer := validAnswer()
	answer.Resolver = "graft-resolve/llm"
	answer.Explanation = "patched by the model"
	for i := range answer.Files {
		answer.Files[i].Origin = resolver.OriginModel
		answer.Files[i].Proposal = true
	}
	return answer
}

// proposalRefTarget returns the commit the proposal reference of the fixture's land points at, or
// "" when there is none.
func proposalRefTarget(t *testing.T, fx resolveFixture) string {
	t.Helper()
	out, err := gittest.NewCommand(t, fx.cfg, "-C", fx.repoPath, "rev-parse", "--verify", "--quiet", proposals.Ref(git.DefaultBranch, fx.commit).String()).Output()
	if err != nil {
		return ""
	}
	return text.ChompBytes(out)
}

func TestUserLand_proposals(t *testing.T) {
	t.Parallel()

	t.Run("a model's answer without a verify.command is proposed, not landed", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg)
		require.Equal(t, proposals.Ref(git.DefaultBranch, fx.commit).String(), pkg.GetProposalRef())

		target := proposalRefTarget(t, fx)
		require.NotEmpty(t, target, "the proposal reference exists")
		parents := strings.Fields(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "rev-list", "--parents", "-n1", target)))
		require.Equal(t, []string{target, fx.tip.String(), fx.commit.String()}, parents)
		// Who caused the proposals is in their commit (the reference is written by the server).
		message := string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%B", target))
		require.Contains(t, message, "Resolver: graft-resolve/llm\n")
		require.Contains(t, message, "Requested-By: "+gittest.TestUser.GetGlId()+"\n")
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Contains(t, f.GetReason(), "no verify.command is configured", p)
			prop := f.GetProposal()
			require.NotNil(t, prop, p)
			require.Equal(t, resolver.ActionWrite, prop.GetAction())
			require.Equal(t, resolver.ModeRegular, prop.GetMode())
			require.Equal(t, "graft-resolve/llm", prop.GetResolver())
			require.Equal(t, "patched by the model", prop.GetExplanation())
			// The blob is kept by the reference, at the file's path, and reads by its oid.
			require.Equal(t, prop.GetOid(), text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "rev-parse", target+":"+p)))
			require.Equal(t, "merged-"+p+"\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", prop.GetOid())))
		}
	})

	// The origin of a file decides, never the name of the resolver (PLAN10 15.9): a file lands without
	// the operator's verification only if it says it is a rule's. Anything else is a model's, and
	// with no verify.command it is proposed. The names are the joined and the cut ones a resolver
	// gives after rounds, which a test of the names could not tell from a rule's.
	for desc, tc := range map[string]struct {
		resolverName string
		origin       string
		proposal     bool
		lands        bool
	}{
		"nothing says what made the files, a plain name":     {"graft-resolve/trivial", "", false, false},
		"nothing says what made the files, joined names":     {"graft-resolve/trivial,graft-resolve/llm", "", false, false},
		"nothing says what made the files, a list cut to +N": {"graft-resolve/trivial,+1", "", false, false},
		"nothing says what made the files, other names":      {"acme/union,acme/llm", "", false, false},
		"an origin nobody knows":                             {"graft-resolve/trivial", "wizard", false, false},
		"the files are marked as a model's":                  {"graft-resolve/trivial", resolver.OriginModel, false, false},
		"the files are marked as rules and as proposals":     {"graft-resolve/trivial", resolver.OriginRule, true, false},
		"rules' files under joined names that say llm":       {"graft-resolve/trivial,graft-resolve/llm", resolver.OriginRule, false, true},
		"rules' files under a list cut to +N":                {"graft-resolve/trivial,+1", resolver.OriginRule, false, true},
		"rules' files under a plain name":                    {"graft-resolve/trivial", resolver.OriginRule, false, true},
	} {
		t.Run("the origin decides, not the name: "+desc, func(t *testing.T) {
			t.Parallel()

			dir, argv := resolverScript(t, "")
			answer := validAnswer()
			answer.Resolver = tc.resolverName
			for i := range answer.Files {
				answer.Files[i].Origin = tc.origin
				answer.Files[i].Proposal = tc.proposal
			}
			writeAnswer(t, dir, answer)
			fx := newResolveFixture(t, enabledResolver(argv))

			resp, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
			if tc.lands {
				require.NoError(t, err)
				requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, git.ObjectID(resp.GetLandedCommitId()))
				return
			}
			requireMergeConflict(t, err)
			requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
			pkg := landConflictDetail(err)
			for _, p := range []string{"a", "b"} {
				f := packageFile(t, pkg, p)
				require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
				require.Contains(t, f.GetReason(), "no verify.command is configured", p)
			}
		})
	}

	t.Run("a file the rules resolved keeps its outcome and gets its proposal", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		answer := modelAnswer()
		answer.Resolver = "graft-resolve/trivial,llm"
		answer.Files[1].Origin = resolver.OriginRule
		answer.Files[1].Proposal = false
		writeAnswer(t, dir, answer)
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, packageFile(t, pkg, "a").GetOutcome())
		b := packageFile(t, pkg, "b")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, b.GetOutcome())
		require.Equal(t, "merged-b\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", b.GetProposal().GetOid())))
	})

	t.Run("the rules' answer of a land that fails for another file is kept", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		answer := validAnswer()
		answer.Files[1].Mode = resolver.ModeExecutable // no side has it: b is rejected
		writeAnswer(t, dir, answer)
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		pkg := landConflictDetail(err)
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_REJECTED, packageFile(t, pkg, "b").GetOutcome())
		a := packageFile(t, pkg, "a")
		require.Equal(t, gitalypb.LandConflictFile_OUTCOME_RESOLVED, a.GetOutcome())
		require.NotEmpty(t, a.GetProposal().GetOid())
		require.NotEmpty(t, pkg.GetProposalRef())
	})

	t.Run("a rules' answer that lands keeps nothing", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, validAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		require.NoError(t, err)
		require.Empty(t, proposalRefTarget(t, fx))
	})

	t.Run("the agent's land of the change drops its proposal", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		require.NotEmpty(t, proposalRefTarget(t, fx))

		// The agent lands the proposals as its own resolution; the resolver checks them.
		writeRawAnswer(t, dir, resolver.CheckResponse{
			Version: resolver.ProtocolVersion, Operation: resolver.OperationCheck, Resolver: "graft-resolve",
			Files: []resolver.CheckFile{{Path: "a", OK: true, Syntax: resolver.SyntaxNone}, {Path: "b", OK: true, Syntax: resolver.SyntaxNone}},
		})
		req := fx.request("Land")
		req.ResolveConflicts = false
		req.Resolution = resolutionOf(fx, "merged-a\n", "merged-b\n")
		_, err = fx.client.UserLand(fx.ctx, req)
		require.NoError(t, err)
		require.Empty(t, proposalRefTarget(t, fx), "the proposal of the change that landed is gone")
	})

	t.Run("a branch keeps at most 20 proposals, none older than proposal_ttl", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		branchDir := proposals.BranchDir(git.DefaultBranch)
		now := time.Now()
		// Proposal references are named by the commit they are for: fake commit IDs name these.
		name := func(i int) string { return fmt.Sprintf("%s%040x", branchDir, i+1) }
		old := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage("old"), gittest.WithCommitterDate(now.Add(-8*24*time.Hour)))
		gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", name(100), old.String())
		for i := 0; i < 21; i++ {
			c := gittest.WriteCommit(t, fx.cfg, fx.repoPath, gittest.WithMessage(fmt.Sprintf("recent %d", i)), gittest.WithCommitterDate(now.Add(-time.Hour-time.Duration(i)*time.Minute)))
			gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", name(i), c.String())
		}

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)

		refs := strings.Fields(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "for-each-ref", "--format=%(refname)", branchDir)))
		require.Len(t, refs, 20)
		require.Contains(t, refs, proposals.Ref(git.DefaultBranch, fx.commit).String(), "the newest is kept")
		require.NotContains(t, refs, name(100), "past proposal_ttl")
		require.NotContains(t, refs, name(20), "the oldest beyond 20")
		require.NotContains(t, refs, name(19), "the oldest beyond 20")
		require.Contains(t, refs, name(18))
	})

	t.Run("proposals that cannot be stored still give the package, saying so", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		// A reference where the branch's directory of proposals goes: the update cannot create it.
		gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", strings.TrimSuffix(proposals.BranchDir(git.DefaultBranch), "/"), fx.tip.String())

		_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
		requireMergeConflict(t, err)
		requireBranchAt(t, fx.cfg, fx.repoPath, git.DefaultBranch, fx.tip)
		pkg := landConflictDetail(err)
		require.NotNil(t, pkg, "the package is the contract: it comes without the proposals")
		require.Empty(t, pkg.GetProposalRef())
		for _, p := range []string{"a", "b"} {
			f := packageFile(t, pkg, p)
			require.Equal(t, gitalypb.LandConflictFile_OUTCOME_PROPOSED, f.GetOutcome(), p)
			require.Nil(t, f.GetProposal(), p)
			// The class of the failure, not git's words, which name the references.
			require.Contains(t, f.GetReason(), "proposal not stored: reference update conflict", p)
			require.NotContains(t, f.GetReason(), "refs/", p)
			require.NotContains(t, f.GetReason(), fx.repoPath, p)
		}
	})

	t.Run("the proposals' date is the server's, not the request's", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.Timestamp = timestamppb.New(time.Now().Add(100 * 365 * 24 * time.Hour))

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		target := proposalRefTarget(t, fx)
		require.NotEmpty(t, target)
		date, err := strconv.ParseInt(text.ChompBytes(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%ct", target)), 10, 64)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), time.Unix(date, 0), time.Hour, "a far-future request timestamp must not keep proposals alive")
	})

	t.Run("a requester the caller names cannot add lines to the proposals' message", func(t *testing.T) {
		t.Parallel()

		dir, argv := resolverScript(t, "")
		writeAnswer(t, dir, modelAnswer())
		fx := newResolveFixture(t, enabledResolver(argv))
		req := fx.request("Land")
		req.User = proto.Clone(req.GetUser()).(*gitalypb.User)
		req.User.GlId = "user-1\nGraft-Resolved-By: someone-else"

		_, err := fx.client.UserLand(fx.ctx, req)
		requireMergeConflict(t, err)
		message := string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "log", "-1", "--format=%B", proposalRefTarget(t, fx)))
		require.Contains(t, message, `Requested-By: "user-1\nGraft-Resolved-By: someone-else"`)
		require.NotContains(t, message, "\nGraft-Resolved-By: someone-else\n")
	})
}

func TestLandAnswerProposes(t *testing.T) {
	t.Parallel()

	rule := resolver.ResponseFile{Path: "a", Origin: resolver.OriginRule}
	model := resolver.ResponseFile{Path: "b", Origin: resolver.OriginModel}
	for desc, tc := range map[string]struct {
		files []resolver.ResponseFile
		want  bool
	}{
		"a rule's file":                         {[]resolver.ResponseFile{rule}, false},
		"two rules' files":                      {[]resolver.ResponseFile{rule, {Path: "c", Origin: resolver.OriginRule}}, false},
		"a model's file":                        {[]resolver.ResponseFile{model}, true},
		"a rule's and a model's":                {[]resolver.ResponseFile{rule, model}, true},
		"a file that says nothing":              {[]resolver.ResponseFile{{Path: "a"}}, true},
		"a rule's and one that says nothing":    {[]resolver.ResponseFile{rule, {Path: "b"}}, true},
		"an origin nobody knows":                {[]resolver.ResponseFile{{Path: "a", Origin: "wizard"}}, true},
		"the origin in another case":            {[]resolver.ResponseFile{{Path: "a", Origin: "Rule"}}, true},
		"a rule's mark with the older proposal": {[]resolver.ResponseFile{{Path: "a", Origin: resolver.OriginRule, Proposal: true}}, true},
		"the older proposal mark alone":         {[]resolver.ResponseFile{{Path: "a", Proposal: true}}, true},
		"no file":                               {nil, false},
	} {
		// The name of the resolver decides nothing, whatever it says.
		for _, name := range []string{
			"graft-resolve/trivial", "graft-resolve/llm", "graft-resolve/trivial,graft-resolve/llm",
			"acme/union,acme/llm", "graft-resolve/trivial,+1", "graft-resolve/trivial, llm", "",
		} {
			answer := &resolver.Response{Resolver: name, Files: tc.files}
			require.Equal(t, tc.want, landAnswerProposes(answer), "%s, resolver %q", desc, name)
		}
	}
}

// TestDecideLand: the one decision point lands only an answer of rules that resolved every file;
// a file that proposes is never landed, whatever else holds.
func TestDecideLand(t *testing.T) {
	t.Parallel()

	resolved := func() []*landFileState {
		return []*landFileState{{outcome: gitalypb.LandConflictFile_OUTCOME_RESOLVED}, {outcome: gitalypb.LandConflictFile_OUTCOME_RESOLVED}}
	}
	rules := &resolver.Response{Resolver: "graft-resolve/trivial,union", Files: []resolver.ResponseFile{
		{Path: "a", Origin: resolver.OriginRule}, {Path: "b", Origin: resolver.OriginRule},
	}}
	require.True(t, decideLand(rules, resolved(), "", "", false), "rules resolved every file")
	require.True(t, decideLand(rules, resolved(), "t1", "t2", false), "rules land whatever tree was verified when no verification is configured")
	require.False(t, decideLand(nil, resolved(), "t1", "t1", false), "no answer")

	left := resolved()
	left[1].outcome = gitalypb.LandConflictFile_OUTCOME_NOT_SENT
	require.False(t, decideLand(rules, left, "", "", false), "a file left to the agent")
	rejected := resolved()
	rejected[0].outcome = gitalypb.LandConflictFile_OUTCOME_REJECTED
	require.False(t, decideLand(rules, rejected, "", "", false), "a file the answer is refused for")
	proposedByRules := resolved()
	proposedByRules[0].outcome = gitalypb.LandConflictFile_OUTCOME_PROPOSED
	require.False(t, decideLand(rules, proposedByRules, "", "", false), "a PROPOSED file in an answer that does not propose")

	// With a verification configured the rule holds for rules as well (15.9): their resolution lands
	// only on the tree that passed, and the lane runs nothing.
	require.True(t, decideLand(rules, resolved(), "t1", "t1", true), "rules, verified on this very tree")
	require.False(t, decideLand(rules, resolved(), "t1", "", true), "rules, nothing verified")
	require.False(t, decideLand(rules, resolved(), "t1", "t2", true), "rules, verified on another tree (the tip moved)")
	require.False(t, decideLand(rules, resolved(), "", "", true), "rules, no tree at all: empty is not equal")

	// A file the resolver holds back (the second model did not agree) never lands, whatever was
	// verified, whatever its origin says.
	held := &resolver.Response{Resolver: "graft-resolve/llm", Files: []resolver.ResponseFile{
		{Path: "a", Origin: resolver.OriginModel, Proposal: true, Hold: "the second model disagreed"}, {Path: "b", Origin: resolver.OriginRule},
	}}
	heldOutcomes := []*landFileState{{outcome: gitalypb.LandConflictFile_OUTCOME_PROPOSED}, {outcome: gitalypb.LandConflictFile_OUTCOME_RESOLVED}}
	require.False(t, decideLand(held, heldOutcomes, "tree-1", "tree-1", false), "held, verified on this very tree")
	require.False(t, decideLand(held, heldOutcomes, "tree-1", "tree-1", true), "held, verified on this very tree, verification required")
	require.True(t, held.Files[0].FromModel())
	ruleHeld := resolver.ResponseFile{Path: "a", Origin: resolver.OriginRule, Hold: "x"}
	require.True(t, ruleHeld.FromModel(), "a held file is a model's, whatever its origin says")

	// THE rule (PLAN10 15.9): a file that proposes may land only if verify.command passed on the exact
	// tree that lands, compared by object ID.
	flagged := &resolver.Response{Resolver: "graft-resolve/trivial", Files: []resolver.ResponseFile{
		{Path: "a", Origin: resolver.OriginRule}, {Path: "b", Origin: resolver.OriginRule, Proposal: true},
	}}
	unmarked := &resolver.Response{Resolver: "graft-resolve/trivial", Files: []resolver.ResponseFile{{Path: "a", Origin: resolver.OriginRule}, {Path: "b"}}}
	proposing := func() []*landFileState {
		return []*landFileState{{outcome: gitalypb.LandConflictFile_OUTCOME_RESOLVED}, {outcome: gitalypb.LandConflictFile_OUTCOME_PROPOSED}}
	}
	for desc, answer := range map[string]*resolver.Response{"a file marked as a proposal": flagged, "a file that says nothing": unmarked} {
		require.True(t, decideLand(answer, proposing(), "tree-1", "tree-1", false), "%s: verified on this very tree", desc)
		require.False(t, decideLand(answer, proposing(), "tree-1", "", false), "%s: nothing verified", desc)
		require.False(t, decideLand(answer, proposing(), "tree-1", "tree-2", false), "%s: verified on another tree (the tip moved)", desc)
		require.False(t, decideLand(answer, proposing(), "", "", false), "%s: no tree at all: empty is not equal", desc)
		require.False(t, decideLand(answer, resolved(), "tree-1", "", false), "%s: even with every outcome RESOLVED", desc)

		leftOver := proposing()
		leftOver[0].outcome = gitalypb.LandConflictFile_OUTCOME_FAILED
		require.False(t, decideLand(answer, leftOver, "tree-1", "tree-1", false), "%s: verified, but a file is left to the agent", desc)
		refused := proposing()
		refused[1].outcome = gitalypb.LandConflictFile_OUTCOME_REJECTED
		require.False(t, decideLand(answer, refused, "tree-1", "tree-1", false), "%s: verified, but a file is refused", desc)
	}
}

// TestResolveLandConflicts_policyFirst: an agent the policy does not let update the branch gets
// PermissionDenied before anything is resolved, and no proposal is kept for the branch.
func TestResolveLandConflicts_policyFirst(t *testing.T) {
	t.Parallel()

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	fx := newResolveFixture(t, enabledResolver(argv))
	s := fx.server(t, enabledResolver(argv))
	s.graft = gitalycfg.Graft{Standalone: true, Policy: []gitalycfg.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/other"}, Allow: true}}}
	s.graftPolicy = policystore.New(s.graft.Policy)
	ctx := identity.NewContext(fx.ctx, identity.Identity{Name: "agent-7", Source: identity.SourceAgent})

	_, err := s.resolveLandConflicts(ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
	var denied *landPolicyDeniedError
	require.ErrorAs(t, err, &denied)
	merged := landWithPackage(ctx, structerr.NewFailedPrecondition("merge conflict"), err)
	require.Equal(t, codes.PermissionDenied, status.Code(merged), "err = %v", merged)
	require.Contains(t, merged.Error(), `policy denies "agent-7"`)
	require.Equal(t, 0, resolverCalls(t, dir), "nothing was resolved")
	require.Empty(t, proposalRefTarget(t, fx), "no proposal is kept")

	// An agent the policy lets update the branch gets its proposals.
	s.graft.Policy = []gitalycfg.GraftPolicyRule{{Actor: "agent-7", Refs: []string{"refs/heads/**"}, Allow: true}}
	s.graftPolicy = policystore.New(s.graft.Policy)
	_, err = s.resolveLandConflicts(ctx, fx.request("Land"), fx.repo, fx.tip, fx.commit, fx.committer())
	var unresolved *landUnresolvedError
	require.ErrorAs(t, err, &unresolved)
	require.NotEmpty(t, proposalRefTarget(t, fx))
}

// TestLandProposalWriteCause: a failed write of proposals is reported by its class.
func TestLandProposalWriteCause(t *testing.T) {
	t.Parallel()

	for want, err := range map[string]error{
		"reference update conflict": fmt.Errorf("updating x: %w", updateref.MismatchingStateError{ReferenceName: "refs/graft/proposals/x"}),
		"storage unavailable":       fmt.Errorf("writing: %w", &os.PathError{Op: "write", Path: "/storage/@hashed/ab", Err: syscall.ENOSPC}),
		"write failed":              errors.New("exit status 128: fatal: unable to write /storage/@hashed/ab"),
	} {
		require.Equal(t, want, landProposalWriteCause(err))
	}
	require.Equal(t, "storage unavailable", landProposalWriteCause(storage.NewRepositoryNotFoundError("default", "x.git")))
	require.Equal(t, "reference update conflict", landProposalWriteCause(updateref.AlreadyLockedError{ReferenceName: "refs/graft/proposals/x"}))
}
