package operations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
)

// TestUserCommitFilesResolvesBranchesExactly checks that the target and the start branch resolve
// only by their exact names: when refs/heads/x is missing, git's DWIM rules would resolve x through
// refs/refs/heads/x, refs/tags/refs/heads/x or refs/remotes/refs/heads/x, and the commit would
// silently start from a commit such a reference (a tag an agent may create) chose. Both with
// branches in the repository and without, the request must behave as if x did not exist.
func TestUserCommitFilesResolvesBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, cfg, client := setupOperationsService(t, ctx)

	for _, decoy := range []git.ReferenceName{"refs/refs/heads/x", "refs/tags/refs/heads/x", "refs/remotes/refs/heads/x"} {
		for _, tc := range []struct {
			desc        string
			hasBranch   bool
			branch      string
			startBranch string
			// expectedErr is set when the request must fail: the start branch does not exist.
			expectedErr string
			repoCreated bool
		}{
			{desc: "target branch, with branches", hasBranch: true, branch: "x"},
			{desc: "target branch, without branches", branch: "x", repoCreated: true},
			{desc: "start branch, with branches", hasBranch: true, branch: "new", startBranch: "x", expectedErr: "reference not found"},
			{desc: "start branch, without branches", branch: "new", startBranch: "x", repoCreated: true},
		} {
			t.Run(string(decoy)+", "+tc.desc, func(t *testing.T) {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				if tc.hasBranch {
					gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))
				}
				decoyCommit := gittest.WriteCommit(t, cfg, repoPath, gittest.WithMessage("decoy"),
					gittest.WithTreeEntries(gittest.TreeEntry{Path: "decoy", Mode: "100644", Content: "decoy\n"}))
				gittest.WriteRef(t, cfg, repoPath, decoy, decoyCommit)

				stream, err := client.UserCommitFiles(ctx)
				require.NoError(t, err)
				require.NoError(t, stream.Send(headerRequest(repo, gittest.TestUser, tc.branch, []byte("message"), tc.startBranch, "")))
				require.NoError(t, stream.Send(createFileHeaderRequest("file")))
				require.NoError(t, stream.Send(actionContentRequest("content\n")))
				resp, err := stream.CloseAndRecv()

				if tc.expectedErr != "" {
					require.ErrorContains(t, err, tc.expectedErr)
					require.NotContains(t, string(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "refs/heads/")), "refs/heads/"+tc.branch)
					return
				}

				require.NoError(t, err)
				require.Empty(t, resp.GetIndexError())
				require.True(t, resp.GetBranchUpdate().GetBranchCreated())
				require.Equal(t, tc.repoCreated, resp.GetBranchUpdate().GetRepoCreated())

				// An orphan commit: the decoy is not its parent, and its tree holds only the file.
				commitID := resp.GetBranchUpdate().GetCommitId()
				require.Empty(t, strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%P", commitID))))
				require.Equal(t, "file\n", string(gittest.Exec(t, cfg, "-C", repoPath, "ls-tree", "--name-only", commitID)))
				require.Equal(t, commitID, strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", "refs/heads/"+tc.branch))))
				// The decoy is untouched.
				require.Equal(t, decoyCommit.String(), strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "rev-parse", decoy.String()))))
			})
		}
	}
}

// TestUserCommitFilesResolvesStartRepositoryBranchesExactly is
// TestUserCommitFilesResolvesBranchesExactly for a branch resolved in another repository, the
// request's start repository: there too, a missing branch never resolves through a decoy.
func TestUserCommitFilesResolvesStartRepositoryBranchesExactly(t *testing.T) {
	t.Parallel()

	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	ctx, cfg, client := setupOperationsService(t, ctx)

	for _, decoy := range []git.ReferenceName{"refs/refs/heads/x", "refs/tags/refs/heads/x", "refs/remotes/refs/heads/x"} {
		for _, tc := range []struct {
			desc        string
			hasBranch   bool
			branch      string
			startBranch string
			// expectedErr is set when the request must fail: the branch does not exist in the
			// start repository.
			expectedErr string
		}{
			{desc: "start branch, with branches", hasBranch: true, branch: "new", startBranch: "x", expectedErr: "reference not found"},
			{desc: "target branch, with branches", hasBranch: true, branch: "x", expectedErr: "reference not found"},
			{desc: "start branch, without branches", branch: "new", startBranch: "x"},
		} {
			t.Run(string(decoy)+", "+tc.desc, func(t *testing.T) {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))

				startRepo, startRepoPath := gittest.CreateRepository(t, ctx, cfg)
				if tc.hasBranch {
					gittest.WriteCommit(t, cfg, startRepoPath, gittest.WithBranch("main"))
				}
				decoyCommit := gittest.WriteCommit(t, cfg, startRepoPath, gittest.WithMessage("decoy"),
					gittest.WithTreeEntries(gittest.TreeEntry{Path: "decoy", Mode: "100644", Content: "decoy\n"}))
				gittest.WriteRef(t, cfg, startRepoPath, decoy, decoyCommit)

				header := headerRequest(repo, gittest.TestUser, tc.branch, []byte("message"), tc.startBranch, "")
				setStartRepository(header, startRepo)

				stream, err := client.UserCommitFiles(ctx)
				require.NoError(t, err)
				require.NoError(t, stream.Send(header))
				require.NoError(t, stream.Send(createFileHeaderRequest("file")))
				require.NoError(t, stream.Send(actionContentRequest("content\n")))
				resp, err := stream.CloseAndRecv()

				if tc.expectedErr != "" {
					require.ErrorContains(t, err, tc.expectedErr)
					require.NotContains(t, string(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "refs/heads/")), "refs/heads/"+tc.branch)
					return
				}

				// The start repository has no branch: the orphan commit of the issue-3294
				// workaround, never the decoy's commit.
				require.NoError(t, err)
				require.Empty(t, resp.GetIndexError())
				require.True(t, resp.GetBranchUpdate().GetBranchCreated())
				commitID := resp.GetBranchUpdate().GetCommitId()
				require.Empty(t, strings.TrimSpace(string(gittest.Exec(t, cfg, "-C", repoPath, "log", "-1", "--format=%P", commitID))))
				require.Equal(t, "file\n", string(gittest.Exec(t, cfg, "-C", repoPath, "ls-tree", "--name-only", commitID)))
			})
		}
	}

	t.Run("a start branch with branches below its name", func(t *testing.T) {
		// refs/heads/x/y is below the name x, which a listing by prefix would match: x itself does
		// not exist.
		repo, repoPath := gittest.CreateRepository(t, ctx, cfg)
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("master"))

		startRepo, startRepoPath := gittest.CreateRepository(t, ctx, cfg)
		gittest.WriteCommit(t, cfg, startRepoPath, gittest.WithBranch("main"))
		gittest.WriteCommit(t, cfg, startRepoPath, gittest.WithBranch("x/y"))

		header := headerRequest(repo, gittest.TestUser, "new", []byte("message"), "x", "")
		setStartRepository(header, startRepo)

		stream, err := client.UserCommitFiles(ctx)
		require.NoError(t, err)
		require.NoError(t, stream.Send(header))
		require.NoError(t, stream.Send(createFileHeaderRequest("file")))
		require.NoError(t, stream.Send(actionContentRequest("content\n")))
		_, err = stream.CloseAndRecv()
		require.ErrorContains(t, err, "reference not found")
		require.NotContains(t, string(gittest.Exec(t, cfg, "-C", repoPath, "for-each-ref", "refs/heads/")), "refs/heads/new")
	})
}
