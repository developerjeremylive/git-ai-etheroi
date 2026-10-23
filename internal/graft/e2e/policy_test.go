package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/sdk"
)

// TestPolicyHoldsForEveryReferenceUpdate checks that the policy protects main
// against every RPC that can update a reference. The builder, whom the policy
// denies main, may not move main or delete it, with RepositoryService.WriteRef
// or RefService.DeleteRefs, loose or packed, and may not remove the repository.
// Those two RPCs are not allowed for agents at all. The builder may still
// create and delete its own branch with the OperationService RPCs, which the
// policy checks. A denied land names the policy rule, without a GitLab prefix,
// and the planner may move main with UserLand. No changeset on main may name
// the builder. Covers WP1, WP2 and WP15.
//
// The layers it reaches: WriteRef, DeleteRefs and RemoveRepository stop at the
// agent allowlist, and the land stops at the pre-receive hook. It does not reach
// the reference-transaction hook: TestPolicyHoldsOnAgentPushes covers the push
// path (receive.hideRefs, then the pre-receive hook), and
// TestPolicyHookDecidesForAgentUpdates the reference-transaction hook alone.
func TestPolicyHoldsForEveryReferenceUpdate(t *testing.T) {
	t.Parallel()

	s := startExample(t)
	base := writeMain(t, s, map[string]string{"README.md": "every update\n"})
	builder := s.repoAs(t, exampleTokens["builder"])

	builderCommit, err := commitFile(s.ctx, builder, "builder", "builder/every", base, "builder.txt", "denied\n")
	require.NoError(t, err)

	repo := &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath}
	builderConn := s.dialAgent(t, exampleTokens["builder"])
	builderRepos := gitalypb.NewRepositoryServiceClient(builderConn)
	builderRefs := gitalypb.NewRefServiceClient(builderConn)
	builderOps := gitalypb.NewOperationServiceClient(builderConn)

	_, err = builderRepos.WriteRef(s.ctx, &gitalypb.WriteRefRequest{
		Repository: repo,
		Ref:        []byte("refs/heads/main"),
		Revision:   []byte(builderCommit),
	})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "WriteRef is not allowed for agents: %v", err)
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	deleteMain := func() error {
		_, err := builderRefs.DeleteRefs(s.ctx, &gitalypb.DeleteRefsRequest{
			Repository: repo,
			Refs:       [][]byte{[]byte("refs/heads/main")},
		})
		return err
	}
	require.Equal(t, codes.PermissionDenied, sdk.Code(deleteMain()), "DeleteRefs is not allowed for agents")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	s.git(t, "pack-refs", "--all")
	require.Equal(t, codes.PermissionDenied, sdk.Code(deleteMain()), "DeleteRefs is not allowed for agents, packed refs included")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	_, err = builderRepos.RemoveRepository(s.ctx, &gitalypb.RemoveRepositoryRequest{Repository: repo})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)
	require.DirExists(t, s.repoPath)

	// The operations the policy opens to the builder keep working on its own branch.
	_, err = builderOps.UserCreateBranch(s.ctx, &gitalypb.UserCreateBranchRequest{
		Repository: repo,
		BranchName: []byte("feature/builder-every"),
		User:       &gitalypb.User{},
		StartPoint: []byte(builderCommit),
	})
	require.NoError(t, err)
	require.Equal(t, builderCommit, s.git(t, "rev-parse", "refs/heads/feature/builder-every"))

	_, err = builderOps.UserDeleteBranch(s.ctx, &gitalypb.UserDeleteBranchRequest{
		Repository: repo,
		BranchName: []byte("feature/builder-every"),
		User:       &gitalypb.User{},
	})
	require.NoError(t, err)
	require.Empty(t, s.git(t, "for-each-ref", "refs/heads/feature/builder-every"))

	// A land of the builder onto main is denied with the policy message, which
	// names the rule and carries no GitLab prefix.
	_, err = landAs(s.ctx, builder, "builder", "main", builderCommit)
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "got %v", err)
	require.ErrorContains(t, err, `policy denies "builder" to update "refs/heads/main" (rule 2)`)
	require.NotContains(t, err.Error(), "GitLab:")
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))

	// The planner may move main.
	_, err = landAs(s.ctx, s.repoAs(t, exampleTokens["planner"]), "planner", "main", builderCommit)
	require.NoError(t, err)
	require.Equal(t, builderCommit, s.git(t, "rev-parse", "refs/heads/main"))

	for _, c := range allChangesets(s.ctx, t, builder) {
		if string(c.GetReference()) == "refs/heads/main" {
			require.NotEqual(t, "builder", c.GetActor(), "a denied update of main must not be recorded")
		}
	}
}

// startWithoutPreReceive starts the example configuration with the RPCs that
// update a reference without the pre-receive hook opened to agents through
// [graft] agent_rpcs. The policy of the example is changed by policy when it is
// not nil.
func startWithoutPreReceive(t *testing.T, policy func([]config.GraftPolicyRule) []config.GraftPolicyRule) gitalyStack {
	t.Helper()

	ex := loadExample(t)
	ex.graft.AgentRPCs = []string{
		"/gitaly.OperationService/UserMergeToRef",
		"/gitaly.OperationService/UserRebaseToRef",
	}
	if policy != nil {
		ex.graft.Policy = policy(ex.graft.Policy)
	}

	return startGitaly(t, ex.graft, ex.sharedToken)
}

// TestPolicyDenialSurfacesWithoutPreReceive checks the RPCs that update a
// reference without running the pre-receive hook, which an operator may open to
// agents with [graft] agent_rpcs. A denial by the policy is PermissionDenied
// with the policy message, not a generic failure of the reference update.
//
// Both RPCs write only refs/merge-requests, and the example policy opens no rule
// for them, so the builder is denied.
func TestPolicyDenialSurfacesWithoutPreReceive(t *testing.T) {
	t.Parallel()

	s := startWithoutPreReceive(t, nil)

	base := writeMain(t, s, map[string]string{"README.md": "opt in\n"})
	builderCommit, err := commitFile(s.ctx, s.repoAs(t, exampleTokens["builder"]), "builder", "builder/opt-in", base, "builder.txt", "denied\n")
	require.NoError(t, err)

	repo := &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath}
	ops := gitalypb.NewOperationServiceClient(s.dialAgent(t, exampleTokens["builder"]))
	target := "refs/merge-requests/1/head"

	_, err = ops.UserMergeToRef(s.ctx, &gitalypb.UserMergeToRefRequest{
		Repository:     repo,
		User:           &gitalypb.User{Name: []byte("builder")},
		SourceSha:      builderCommit,
		TargetRef:      []byte(target),
		FirstParentRef: []byte("refs/heads/main"),
		Message:        []byte("merge"),
	})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "UserMergeToRef: %v", err)
	require.ErrorContains(t, err, `policy denies "builder" to update "`+target+`": no matching rule`)
	require.NotContains(t, err.Error(), "GitLab:")

	_, err = ops.UserRebaseToRef(s.ctx, &gitalypb.UserRebaseToRefRequest{
		Repository:     repo,
		User:           &gitalypb.User{Name: []byte("builder")},
		SourceSha:      builderCommit,
		TargetRef:      []byte(target),
		FirstParentRef: []byte("refs/heads/main"),
	})
	require.Equal(t, codes.PermissionDenied, sdk.Code(err), "UserRebaseToRef: %v", err)
	require.ErrorContains(t, err, `policy denies "builder" to update "`+target+`": no matching rule`)
	require.NotContains(t, err.Error(), "GitLab:")

	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))
	require.Empty(t, s.git(t, "for-each-ref", target))
}

// TestPolicyAllowsWithoutPreReceive checks that the RPCs that run without the
// pre-receive hook still work for an agent that the policy allows: the builder
// may rebase onto a merge request reference once the policy opens it.
func TestPolicyAllowsWithoutPreReceive(t *testing.T) {
	t.Parallel()

	s := startWithoutPreReceive(t, func(policy []config.GraftPolicyRule) []config.GraftPolicyRule {
		return append([]config.GraftPolicyRule{{
			Actor: "builder",
			Refs:  []string{"refs/merge-requests/**"},
			Allow: true,
		}}, policy...)
	})

	base := writeMain(t, s, map[string]string{"README.md": "opt in\n"})
	builderCommit, err := commitFile(s.ctx, s.repoAs(t, exampleTokens["builder"]), "builder", "builder/opt-in", base, "builder.txt", "allowed\n")
	require.NoError(t, err)

	ops := gitalypb.NewOperationServiceClient(s.dialAgent(t, exampleTokens["builder"]))
	_, err = ops.UserRebaseToRef(s.ctx, &gitalypb.UserRebaseToRefRequest{
		Repository:     &gitalypb.Repository{StorageName: s.storage, RelativePath: s.relPath},
		User:           &gitalypb.User{Name: []byte("builder")},
		SourceSha:      builderCommit,
		TargetRef:      []byte("refs/merge-requests/1/head"),
		FirstParentRef: []byte("refs/heads/main"),
	})
	require.NoError(t, err)
	require.NotEmpty(t, s.git(t, "rev-parse", "refs/merge-requests/1/head"))
	require.Equal(t, base, s.git(t, "rev-parse", "refs/heads/main"))
}
