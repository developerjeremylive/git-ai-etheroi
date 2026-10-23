package objectpool

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	housekeepingmgr "github.com/feder-cr/git-ai/internal/git/housekeeping/manager"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/objectpool"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

func TestCreate(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	logger := testhelper.NewLogger(t)
	cfg, repo, repoPath, _, client := setup(t, ctx)
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

	txManager := transaction.NewManager(cfg, logger, nil)
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)

	poolProto := &gitalypb.ObjectPool{
		Repository: &gitalypb.Repository{
			StorageName:  cfg.Storages[0].Name,
			RelativePath: gittest.NewObjectPoolName(t),
		},
	}

	_, err := client.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: poolProto,
		Origin:     repo,
	})
	require.NoError(t, err)

	pool, err := objectpool.FromProto(
		ctx,
		logger,
		config.NewLocator(cfg),
		gittest.NewCommandFactory(t, cfg),
		catfileCache,
		txManager,
		housekeepingmgr.New(cfg.Prometheus, logger, txManager, nil),
		&gitalypb.ObjectPool{
			Repository: &gitalypb.Repository{
				StorageName:  cfg.Storages[0].Name,
				RelativePath: gittest.GetReplicaPath(t, ctx, cfg, poolProto.GetRepository()),
			},
		},
	)
	require.NoError(t, err)
	poolPath := gittest.RepositoryPath(t, ctx, pool)

	// Assert that the now-created object pool exists and is valid.
	require.True(t, pool.IsValid(ctx))
	require.NoDirExists(t, filepath.Join(poolPath, "hooks"))
	gittest.RequireObjectExists(t, cfg, poolPath, commitID)

	// Making the same request twice should result in an error.
	_, err = client.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: poolProto,
		Origin:     repo,
	})
	require.Error(t, err)
	require.True(t, pool.IsValid(ctx))
}

func TestCreate_emptySource(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, repoProto, _, _, client := setup(t, ctx)

	objectPoolProto := &gitalypb.ObjectPool{
		Repository: &gitalypb.Repository{
			StorageName:  repoProto.GetStorageName(),
			RelativePath: gittest.NewObjectPoolName(t),
		},
	}

	response, err := client.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: objectPoolProto,
		Origin:     repoProto,
	})
	require.NoError(t, err)
	testhelper.ProtoEqual(t, &gitalypb.CreateObjectPoolResponse{}, response)

	objectPoolRepo := localrepo.NewTestRepo(t, cfg, objectPoolProto.GetRepository())

	repoClient := gitalypb.NewRepositoryServiceClient(gittest.DialService(t, ctx, cfg))

	// Assert that the created object pool is indeed empty.
	resp, err := repoClient.RepositorySize(ctx, &gitalypb.RepositorySizeRequest{Repository: objectPoolProto.GetRepository()})
	require.NoError(t, err)
	require.Equal(t, int64(0), resp.GetSize())

	// And furthermore assert that the object hash of the new object pool matches what we
	// expect.
	objectHash, err := objectPoolRepo.ObjectHash(ctx)
	require.NoError(t, err)
	require.Equal(t, gittest.DefaultObjectHash.Format, objectHash.Format)
}

func TestCreate_mvccOrigin(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, _, _, _, client := setup(t, ctx, testserver.WithDisablePraefect())

	origin, _ := gittest.CreateMVCCRepository(t, ctx, cfg)

	_, err := client.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: &gitalypb.ObjectPool{
			Repository: &gitalypb.Repository{
				StorageName:  cfg.Storages[0].Name,
				RelativePath: gittest.NewObjectPoolName(t),
			},
		},
		Origin: origin,
	})
	testhelper.RequireGrpcError(t, structerr.NewInvalidArgument("object pools are not supported for repositories using the MVCC reference backend"), err)
}

func TestCreate_unsuccessful(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg, repo, _, _, client := setup(t, ctx, testserver.WithDisablePraefect())

	// Precreate a stale lock for a valid object pool path so that we can verify that the lock
	// gets honored as expected.
	lockedRelativePath := gittest.NewObjectPoolName(t)
	lockedFullPath := filepath.Join(cfg.Storages[0].Path, lockedRelativePath+".lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(lockedFullPath), mode.Directory))
	require.NoError(t, os.WriteFile(lockedFullPath, nil, mode.File))

	// Create a preexisting object pool.
	preexistingPool := &gitalypb.ObjectPool{
		Repository: &gitalypb.Repository{
			StorageName:  cfg.Storages[0].Name,
			RelativePath: gittest.NewObjectPoolName(t),
		},
	}
	_, err := client.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: preexistingPool,
		Origin:     repo,
	})
	require.NoError(t, err)

	for _, tc := range []struct {
		desc        string
		request     *gitalypb.CreateObjectPoolRequest
		expectedErr error
	}{
		{
			desc: "no origin repository",
			request: &gitalypb.CreateObjectPoolRequest{
				ObjectPool: &gitalypb.ObjectPool{
					Repository: &gitalypb.Repository{
						StorageName:  cfg.Storages[0].Name,
						RelativePath: gittest.NewObjectPoolName(t),
					},
				},
			},
			expectedErr: errMissingOriginRepository,
		},
		{
			desc: "no object pool",
			request: &gitalypb.CreateObjectPoolRequest{
				Origin: repo,
			},
			expectedErr: errMissingPool,
		},
		{
			desc: "outside pools directory",
			request: &gitalypb.CreateObjectPoolRequest{
				Origin: repo,
				ObjectPool: &gitalypb.ObjectPool{
					Repository: &gitalypb.Repository{
						StorageName:  cfg.Storages[0].Name,
						RelativePath: "outside-pools",
					},
				},
			},
			expectedErr: errInvalidPoolDir,
		},
		{
			desc: "path must be lowercase",
			request: &gitalypb.CreateObjectPoolRequest{
				Origin: repo,
				ObjectPool: &gitalypb.ObjectPool{
					Repository: &gitalypb.Repository{
						StorageName:  cfg.Storages[0].Name,
						RelativePath: strings.ToUpper(gittest.NewObjectPoolName(t)),
					},
				},
			},
			expectedErr: errInvalidPoolDir,
		},
		{
			desc: "subdirectories must match first four pool digits",
			request: &gitalypb.CreateObjectPoolRequest{
				Origin: repo,
				ObjectPool: &gitalypb.ObjectPool{
					Repository: &gitalypb.Repository{
						StorageName:  cfg.Storages[0].Name,
						RelativePath: "@pools/aa/bb/ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff.git",
					},
				},
			},
			expectedErr: errInvalidPoolDir,
		},
		{
			desc: "pool path traversal fails",
			request: &gitalypb.CreateObjectPoolRequest{
				Origin: repo,
				ObjectPool: &gitalypb.ObjectPool{
					Repository: &gitalypb.Repository{
						StorageName:  cfg.Storages[0].Name,
						RelativePath: gittest.NewObjectPoolName(t) + "/..",
					},
				},
			},
			expectedErr: errInvalidPoolDir,
		},
		{
			desc: "pool is locked",
			request: &gitalypb.CreateObjectPoolRequest{
				Origin: repo,
				ObjectPool: &gitalypb.ObjectPool{
					Repository: &gitalypb.Repository{
						StorageName:  cfg.Storages[0].Name,
						RelativePath: lockedRelativePath,
					},
				},
			},
			expectedErr: structerr.NewFailedPrecondition("creating object pool: repository is already locked"),
		},
		{
			desc: "pool exists",
			request: &gitalypb.CreateObjectPoolRequest{
				Origin:     repo,
				ObjectPool: preexistingPool,
			},
			expectedErr: structerr.NewAlreadyExists("creating object pool: repository exists already"),
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			_, err := client.CreateObjectPool(ctx, tc.request)
			testhelper.RequireGrpcError(t, tc.expectedErr, err)
		})
	}
}

func TestCreate_atomic(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	gitCmdFactory := gittest.NewInterceptingCommandFactory(t, ctx, cfg, func(execEnv gitcmd.ExecutionEnvironment) string {
		return fmt.Sprintf(`#!/usr/bin/env bash
		if [[ ! "$@" =~ "clone" ]]; then
			exec %[1]q "$@"
		fi

		# If we are cloning then this must be the object pool that we try to create. We
		# execute the command, but then afterwards we pretend to fail. We should ultimately
		# see that the pool does not exist.
		%[1]q "$@" 2>/dev/null

		exit 123
		`, execEnv.BinaryPath)
	})

	cfg, repo, _, _, client := setupWithConfig(t, ctx, cfg, testserver.WithGitCommandFactory(gitCmdFactory))

	objectPool := &gitalypb.ObjectPool{
		Repository: &gitalypb.Repository{
			StorageName:  cfg.Storages[0].Name,
			RelativePath: gittest.NewObjectPoolName(t),
		},
	}

	_, err := client.CreateObjectPool(ctx, &gitalypb.CreateObjectPoolRequest{
		ObjectPool: objectPool,
		Origin:     repo,
	})
	testhelper.RequireGrpcError(t, structerr.NewInternal("creating object pool: cloning to pool: exit status 123, stderr: %q", ""), err)
	require.NoDirExists(t, filepath.Join(cfg.Storages[0].Path, objectPool.GetRepository().GetRelativePath()))
}
