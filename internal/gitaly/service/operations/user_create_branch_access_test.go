package operations

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// TestUserCreateBranch_accessCheck checks that an access check that refuses the
// new branch is reported as PermissionDenied, as UserDeleteBranch reports it, and
// that the branch is not created.
func TestUserCreateBranch_accessCheck(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	ctx, cfg, client := setupOperationsService(t, ctx, testserver.WithGitLabClient(
		gitlab.NewMockClient(t, func(context.Context, gitlab.AllowedParams) (bool, string, error) {
			return false, "policy denies the branch", nil
		}, gitlab.MockPreReceive, gitlab.MockPostReceive),
	))

	repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	response, err := client.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
		Repository: repo,
		BranchName: []byte("denied"),
		StartPoint: []byte(commitID),
		User:       gittest.TestUser,
	})
	testhelper.RequireGrpcError(t, structerr.NewPermissionDenied("creation denied by access checks: running pre-receive hooks: GitLab: policy denies the branch"), err)
	require.Nil(t, response)

	refs := gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "--format=%(refname)", "refs/heads/denied")
	require.Empty(t, refs, "the denied branch is not created")
}
