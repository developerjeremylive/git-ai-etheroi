package ref

import (
	"context"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestFindDefaultBranchName(t *testing.T) {
	t.Parallel()

	type setupData struct {
		request     *gitalypb.FindDefaultBranchNameRequest
		expectedErr error
	}

	ctx := testhelper.Context(t)
	cfg, client := setupRefService(t)

	for _, tc := range []struct {
		desc             string
		setup            func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData
		expectedResponse *gitalypb.FindDefaultBranchNameResponse
	}{
		{
			desc: "successful",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oid := gittest.WriteCommit(t, cfg, repoPath, gittest.WithReference(git.DefaultRef.String()))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithReference(git.LegacyDefaultRef.String()), gittest.WithParents(oid))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("apple"), gittest.WithParents(oid))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("banana"), gittest.WithParents(oid))

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
					},
				}
			},
			expectedResponse: &gitalypb.FindDefaultBranchNameResponse{
				Name: []byte(git.DefaultRef),
			},
		},
		{
			desc: "successful, HEAD only",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oid := gittest.WriteCommit(t, cfg, repoPath, gittest.WithReference(git.DefaultRef.String()))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithReference(git.LegacyDefaultRef.String()), gittest.WithParents(oid))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("apple"), gittest.WithParents(oid))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("banana"), gittest.WithParents(oid))

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
						HeadOnly:   true,
					},
				}
			},
			expectedResponse: &gitalypb.FindDefaultBranchNameResponse{
				Name: []byte(git.DefaultRef),
			},
		},
		{
			desc: "successful, single branch",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("banana"))

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
					},
				}
			},
			expectedResponse: &gitalypb.FindDefaultBranchNameResponse{
				Name: []byte("refs/heads/banana"),
			},
		},
		{
			desc: "successful, single branch, HEAD only",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("banana"))

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
						HeadOnly:   true,
					},
				}
			},
			expectedResponse: &gitalypb.FindDefaultBranchNameResponse{
				Name: []byte(git.DefaultRef),
			},
		},
		{
			desc: "successful, updated default",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oid := gittest.WriteCommit(t, cfg, repoPath, gittest.WithReference(git.LegacyDefaultRef.String()))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("apple"), gittest.WithParents(oid))
				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("banana"), gittest.WithParents(oid))

				gittest.Exec(t, cfg, "-C", repoPath, "symbolic-ref", "HEAD", "refs/heads/banana")

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
					},
				}
			},
			expectedResponse: &gitalypb.FindDefaultBranchNameResponse{
				Name: []byte("refs/heads/banana"),
			},
		},
		{
			desc: "empty repository",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, _ := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
					},
				}
			},
			expectedResponse: &gitalypb.FindDefaultBranchNameResponse{
				Name: []byte{},
			},
		},
		{
			desc: "empty repository, HEAD only",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, _ := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
						HeadOnly:   true,
					},
				}
			},
			expectedResponse: &gitalypb.FindDefaultBranchNameResponse{
				Name: []byte(git.DefaultRef),
			},
		},
		{
			desc: "detached HEAD, HEAD only",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, repoPath := gittest.CreateRepository(t, ctx, cfg)

				oid := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
				gittest.Exec(t, cfg, "-C", repoPath, "update-ref", "--no-deref", "HEAD", oid.String())

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: repo,
						HeadOnly:   true,
					},
					expectedErr: structerr.NewFailedPrecondition("HEAD is detached: it does not point to a branch"),
				}
			},
		},
		{
			desc: "repository not provided",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				return setupData{
					request:     &gitalypb.FindDefaultBranchNameRequest{},
					expectedErr: structerr.NewInvalidArgument("%w", storage.ErrRepositoryNotSet),
				}
			},
		},
		{
			desc: "repository doesn't exist on disk",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: &gitalypb.Repository{StorageName: cfg.Storages[0].Name, RelativePath: "made/up/path"},
					},
					expectedErr: testhelper.ToInterceptedMetadata(
						structerr.New("%w", storage.NewRepositoryNotFoundError(cfg.Storages[0].Name, "made/up/path")),
					),
				}
			},
		},
		{
			desc: "unknown storage",
			setup: func(t *testing.T, ctx context.Context, cfg config.Cfg) setupData {
				repo, _ := gittest.CreateRepository(t, ctx, cfg)

				return setupData{
					request: &gitalypb.FindDefaultBranchNameRequest{
						Repository: &gitalypb.Repository{StorageName: "invalid", RelativePath: repo.GetRelativePath()},
					},
					expectedErr: testhelper.ToInterceptedMetadata(structerr.NewInvalidArgument(
						"%w", storage.NewStorageNotFoundError("invalid"),
					)),
				}
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			data := tc.setup(t, ctx, cfg)

			response, err := client.FindDefaultBranchName(ctx, data.request)
			testhelper.RequireGrpcError(t, data.expectedErr, err)
			testhelper.ProtoEqual(t, tc.expectedResponse, response)
		})
	}
}
