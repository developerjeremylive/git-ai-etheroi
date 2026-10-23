package repository

import (
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// The replication onto an existing replica fetches without checking the objects. A checked fetch
// keeps a pack (fetch-pack hands it to index-pack); an unchecked one unpacks fewer than 100
// objects into loose objects, whose write would race housekeeping's removal of empty loose object
// directories of the replica (gittest.NewFanOutRemovingCommandFactory). The fetch sets
// fetch.unpackLimit=1, so it keeps a pack anyway.
func TestReplicateRepository_fanOutDirectoryRemoved(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfgBuilder := testcfg.NewGitalyCfgBuilder(testcfg.WithStorages("default", "replica"))
	cfg := cfgBuilder.Build(t)
	testcfg.BuildGitalyHooks(t, cfg)
	testcfg.BuildGitalySSH(t, cfg)
	factory, arm := gittest.NewFanOutRemovingCommandFactory(t, ctx, cfg)
	client, serverSocketPath := runRepositoryService(t, cfg, testserver.WithDisablePraefect(), testserver.WithGitCommandFactory(factory))
	cfg.SocketPath = serverSocketPath
	ctx = testhelper.MergeOutgoingMetadata(ctx, testcfg.GitalyServersMetadataFromCfg(t, cfg))

	source, sourcePath := gittest.CreateRepository(t, ctx, cfg)
	first := gittest.WriteCommit(t, cfg, sourcePath, gittest.WithBranch("main"))
	target := proto.Clone(source).(*gitalypb.Repository)
	target.StorageName = cfg.Storages[1].Name

	// The first replication creates the replica from a snapshot.
	_, err := client.ReplicateRepository(ctx, &gitalypb.ReplicateRepositoryRequest{Repository: target, Source: source})
	require.NoError(t, err)

	second := gittest.WriteCommit(t, cfg, sourcePath, gittest.WithParents(first), gittest.WithBranch("main"))
	targetPath := filepath.Join(cfg.Storages[1].Path, gittest.GetReplicaPath(t, ctx, cfg, target))
	arm(targetPath)

	_, err = client.ReplicateRepository(ctx, &gitalypb.ReplicateRepositoryRequest{Repository: target, Source: source})
	require.NoError(t, err)

	gittest.RequireObjectExists(t, cfg, targetPath, second)
	gittest.Exec(t, cfg, "-C", targetPath, "fsck", "--strict")
}
