package objectpool

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// The fetch into a pool keeps a pack: unpacked into loose objects, a write would race
// housekeeping's removal of empty loose object directories of the pool
// (gittest.NewFanOutRemovingCommandFactory). It does because it checks the objects it fetches
// (transfer.fsckObjects of every fetch), and git's fetch-pack hands a checked fetch to index-pack,
// however few its objects.
func TestFetchIntoObjectPool_fanOutDirectoryRemoved(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	factory, arm := gittest.NewFanOutRemovingCommandFactory(t, ctx, cfg)
	cfg, repo, repoPath, _, client := setupWithConfig(t, ctx, cfg, testserver.WithGitCommandFactory(factory))

	parentID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	poolProto, _, poolPath := createObjectPool(t, ctx, cfg, repo)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithParents(parentID), gittest.WithBranch("main"))
	arm(poolPath)

	_, err := client.FetchIntoObjectPool(ctx, &gitalypb.FetchIntoObjectPoolRequest{
		ObjectPool: poolProto,
		Origin:     repo,
	})
	require.NoError(t, err)

	gittest.RequireObjectExists(t, cfg, poolPath, commitID)
	gittest.Exec(t, cfg, "-C", poolPath, "fsck")
}
