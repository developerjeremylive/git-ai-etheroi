package operations

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// setupFanOutRemoval runs the operations service with Git commands that lose every loose object
// directory they create in a repository right after creating it, as housekeeping's git
// prune-packed may at that moment (gittest.NewFanOutRemovingCommandFactory). A write RPC that
// writes into a quarantine and migrates it succeeds; one that writes straight into the repository
// fails.
func setupFanOutRemoval(t *testing.T) (context.Context, config.Cfg, gitalypb.OperationServiceClient) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	factory := gittest.NewFanOutRemovingCommandFactory(t, ctx, cfg)
	return setupOperationsServiceWithCfg(t, ctx, cfg, testserver.WithGitCommandFactory(factory))
}

// packWithoutFanOut packs every object of the repository and removes the emptied loose object
// directories, as housekeeping does, so that every object the RPC writes needs one created.
func packWithoutFanOut(t *testing.T, cfg config.Cfg, repoPath string) {
	gittest.Exec(t, cfg, "-C", repoPath, "repack", "-a", "-d")
	gittest.Exec(t, cfg, "-C", repoPath, "prune-packed")
	fanOut, err := filepath.Glob(filepath.Join(repoPath, "objects", "[0-9a-f][0-9a-f]"))
	require.NoError(t, err)
	require.Empty(t, fanOut)
}

func TestUserApplyPatch_fanOutDirectoryRemoved(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupFanOutRemoval(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	parentCommitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "file", Mode: "100644", Content: "change me\n"},
	))
	packWithoutFanOut(t, cfg, repoPath)

	stream, err := client.UserApplyPatch(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
		UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Header_{
			Header: &gitalypb.UserApplyPatchRequest_Header{
				Repository:   repoProto,
				User:         gittest.TestUser,
				TargetBranch: []byte(git.DefaultBranch),
				Timestamp:    &timestamppb.Timestamp{Seconds: 1234512345},
			},
		},
	}))
	require.NoError(t, stream.Send(&gitalypb.UserApplyPatchRequest{
		UserApplyPatchRequestPayload: &gitalypb.UserApplyPatchRequest_Patches{
			Patches: []byte(fmt.Sprintf(`From %s Mon Sep 17 00:00:00 2001
From: Patch User <patchuser@gitlab.org>
Date: Thu, 18 Oct 2018 13:40:35 +0200
Subject: [PATCH] A commit from a patch

---
 file | 2 +-
 1 file changed, 1 insertion(+), 1 deletion(-)

diff --git a/file b/file
index 3742e48..e40a3b9 100644
--- a/file
+++ b/file
@@ -1 +1 @@
-change me
+changed
--
2.19.1
`, parentCommitID)),
		},
	}))

	response, err := stream.CloseAndRecv()
	require.NoError(t, err)

	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	patched, err := repo.ReadCommit(ctx, git.Revision(git.DefaultBranch))
	require.NoError(t, err)
	require.Equal(t, response.GetBranchUpdate().GetCommitId(), patched.GetId())
	require.Equal(t, []string{parentCommitID.String()}, patched.GetParentIds())
	require.Equal(t, []byte("changed\n"), gittest.Exec(t, cfg, "-C", repoPath, "show", git.DefaultBranch+":file"))
	gittest.Exec(t, cfg, "-C", repoPath, "fsck", "--strict")
}

func TestUserMergeToRef_fanOutDirectoryRemoved(t *testing.T) {
	t.Parallel()

	ctx, cfg, client := setupFanOutRemoval(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg)
	base := gittest.WriteCommit(t, cfg, repoPath, gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
	))
	ours := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch(git.DefaultBranch), gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
		gittest.TreeEntry{Path: "ours", Mode: "100644", Content: "ours\n"},
	))
	theirs := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("theirs"), gittest.WithParents(base), gittest.WithTreeEntries(
		gittest.TreeEntry{Path: "base", Mode: "100644", Content: "base\n"},
		gittest.TreeEntry{Path: "theirs", Mode: "100644", Content: "theirs\n"},
	))
	packWithoutFanOut(t, cfg, repoPath)

	response, err := client.UserMergeToRef(ctx, &gitalypb.UserMergeToRefRequest{
		Repository:     repoProto,
		User:           gittest.TestUser,
		FirstParentRef: []byte("refs/heads/" + git.DefaultBranch),
		TargetRef:      []byte("refs/merge-requests/1/merge"),
		SourceSha:      theirs.String(),
		Message:        []byte("merge"),
	})
	require.NoError(t, err)

	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	merged, err := repo.ReadCommit(ctx, git.Revision("refs/merge-requests/1/merge"))
	require.NoError(t, err)
	require.Equal(t, response.GetCommitId(), merged.GetId())
	require.Equal(t, []string{ours.String(), theirs.String()}, merged.GetParentIds())
	gittest.Exec(t, cfg, "-C", repoPath, "fsck", "--strict")
}

// The proposals that UserLand keeps of a resolver's answer are written outside the land's
// quarantine, which a failed land discards, but in a quarantine of their own.
func TestUserLand_proposalsFanOutDirectoryRemoved(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	factory := gittest.NewFanOutRemovingCommandFactory(t, ctx, cfg)

	dir, argv := resolverScript(t, "")
	writeAnswer(t, dir, modelAnswer())
	fx := newResolveFixtureOn(t, cfg, config.Graft{Land: config.GraftLand{Resolver: enabledResolver(argv)}},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "base\n"},
			{Mode: "100644", Path: "b", Content: "base\n"},
		},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "ours\n"},
			{Mode: "100644", Path: "b", Content: "ours\n"},
		},
		[]gittest.TreeEntry{
			{Mode: "100644", Path: "a", Content: "theirs\n"},
			{Mode: "100644", Path: "b", Content: "theirs\n"},
		},
		testserver.WithGitCommandFactory(factory),
	)
	gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "update-ref", "refs/heads/landed", fx.commit.String())
	packWithoutFanOut(t, fx.cfg, fx.repoPath)

	_, err := fx.client.UserLand(fx.ctx, fx.request("Land"))
	requireMergeConflict(t, err)
	pkg := landConflictDetail(err)
	require.NotNil(t, pkg)

	target := proposalRefTarget(t, fx)
	require.NotEmpty(t, target, "the proposal reference exists")
	for _, p := range []string{"a", "b"} {
		require.Equal(t, "merged-"+p+"\n", string(gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "cat-file", "blob", packageFile(t, pkg, p).GetProposal().GetOid())))
	}
	gittest.Exec(t, fx.cfg, "-C", fx.repoPath, "fsck", "--strict")
}
