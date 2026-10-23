package repoutil

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/counter"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/transaction/txinfo"
	"github.com/feder-cr/git-ai/internal/transaction/voting"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/peer"
)

func TestCreate(t *testing.T) {
	t.Parallel()
	ctx := testhelper.Context(t)

	cfg := testcfg.Build(t)

	logger := testhelper.NewLogger(t)
	txManager := &transaction.MockManager{}
	locator := config.NewLocator(cfg)
	gitCmdFactory := gittest.NewCommandFactory(t, cfg)
	catfileCache := catfile.NewCache(cfg)
	defer catfileCache.Stop()

	var votesByPhase map[voting.Phase]int

	requireFullRepackTimestampExists := func(t *testing.T, repoPath string, exists bool) {
		t.Helper()

		timestamp, err := stats.FullRepackTimestamp(repoPath)
		require.NoError(t, err)
		if exists {
			require.NotZero(t, timestamp)
		} else {
			require.Zero(t, timestamp)
		}
	}

	type requireErrorFunc func(*testing.T, config.Cfg, string, error)

	equalError := func(expected error) requireErrorFunc {
		return func(t *testing.T, _ config.Cfg, _ string, actual error) {
			require.Equal(t, actual, expected)
		}
	}

	type setup struct {
		setup         func(t *testing.T, repo *gitalypb.Repository, repoPath string)
		seed          func(t *testing.T, repo *gitalypb.Repository, repoPath string) error
		verify        func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string)
		requireError  requireErrorFunc
		desc          string
		opts          []CreateOption
		transactional bool
	}

	for _, tc := range []setup{
		{
			desc: "no seeding",
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				// Assert that the temporary repository does not exist anymore.
				require.NoDirExists(t, tempRepoPath)

				// But the new repository must exist.
				isBareRepo := gittest.Exec(t, cfg, "-C", realRepoPath, "rev-parse", "--is-bare-repository")
				require.Equal(t, "true", text.ChompBytes(isBareRepo))

				requireFullRepackTimestampExists(t, realRepoPath, true)
			},
		},
		{
			desc: "seeding",
			seed: func(t *testing.T, repo *gitalypb.Repository, _ string) error {
				// We're using the command factory on purpose here to assert that we
				// can execute regular Git commands on the temporary repository.
				cmd, err := gitCmdFactory.New(ctx, repo, gitcmd.Command{
					Name: "config",
					Args: []string{"custom.key", "value"},
				})
				require.NoError(t, err)
				require.NoError(t, cmd.Wait())
				return nil
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				value := gittest.Exec(t, cfg, "-C", realRepoPath, "config", "custom.key")
				require.Equal(t, "value", text.ChompBytes(value))

				requireFullRepackTimestampExists(t, realRepoPath, true)
			},
		},
		{
			desc: "seeding with branch",
			seed: func(t *testing.T, repo *gitalypb.Repository, _ string) error {
				repoPath, err := locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
				if err != nil {
					return err
				}

				gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("b1"), gittest.WithTreeEntries(
					gittest.TreeEntry{Path: "file", Mode: "100644", Content: "can't touch this"},
				))

				return nil
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				gittest.Exec(t, cfg, "-C", realRepoPath, "show-ref", "--verify", "--quiet", "refs/heads/b1")

				requireFullRepackTimestampExists(t, realRepoPath, true)
			},
		},
		{
			desc: "error while seeding",
			seed: func(t *testing.T, repo *gitalypb.Repository, _ string) error {
				return errors.New("some error")
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.NoDirExists(t, realRepoPath)
				require.NoDirExists(t, tempRepoPath)
			},
			requireError: equalError(errors.New("some error")),
		},
		{
			desc: "preexisting directory",
			setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
				require.NoError(t, os.MkdirAll(repoPath, mode.Directory))
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.NoDirExists(t, tempRepoPath)

				require.DirExists(t, realRepoPath)
				dirEntries, err := os.ReadDir(realRepoPath)
				require.NoError(t, err)
				require.Empty(t, dirEntries, "directory should not have been modified")

				requireFullRepackTimestampExists(t, realRepoPath, false)
			},
			requireError: equalError(structerr.NewAlreadyExists("repository exists already")),
		},
		{
			desc: "pre-lock stat fails",
			setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
				require.NoError(t, os.MkdirAll(repoPath, mode.Directory))
				parentDir := filepath.Dir(repoPath)
				// Drop permissions to trigger a stat failure.
				require.NoError(t, os.Chmod(parentDir, 0))
				// Restore the permissions so the directory can be cleaned up.
				t.Cleanup(func() { require.NoError(t, os.Chmod(parentDir, mode.Directory)) })
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				// Restore the permissions so the below checks work.
				require.NoError(t, os.Chmod(filepath.Dir(realRepoPath), mode.Directory))

				require.NoDirExists(t, tempRepoPath)

				require.DirExists(t, realRepoPath)
				dirEntries, err := os.ReadDir(realRepoPath)
				require.NoError(t, err)
				require.Empty(t, dirEntries, "directory should not have been modified")

				requireFullRepackTimestampExists(t, realRepoPath, false)
			},
			requireError: func(t *testing.T, cfg config.Cfg, relativePath string, actual error) {
				require.Equal(t, fmt.Errorf("pre-lock stat: %w", &fs.PathError{
					Op:   "stat",
					Path: filepath.Join(cfg.Storages[0].Path, relativePath),
					Err:  syscall.EACCES,
				}), actual)
			},
		},
		{
			desc: "locked",
			setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
				require.NoError(t, os.MkdirAll(filepath.Dir(repoPath), mode.Directory))

				// Lock the target repository such that we must fail.
				lock, err := os.Create(repoPath + ".lock")
				require.NoError(t, err)
				require.NoError(t, lock.Close())
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.NoDirExists(t, tempRepoPath)
				require.NoDirExists(t, realRepoPath)
				require.FileExists(t, realRepoPath+".lock")

				requireFullRepackTimestampExists(t, realRepoPath, false)
			},
			requireError: equalError(structerr.NewFailedPrecondition("repository is already locked")),
		},
		{
			desc:          "successful transaction",
			transactional: true,
			setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
				votesByPhase = map[voting.Phase]int{}
				txManager.VoteFn = func(_ context.Context, _ txinfo.Transaction, _ voting.Vote, phase voting.Phase) error {
					votesByPhase[phase]++
					return nil
				}
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.Equal(t, map[voting.Phase]int{
					voting.Preparing: 1,
					voting.Prepared:  1,
					voting.Committed: 1,
				}, votesByPhase)

				requireFullRepackTimestampExists(t, realRepoPath, true)
			},
		},
		{
			desc:          "failing preparatory vote",
			transactional: true,
			setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
				txManager.VoteFn = func(context.Context, txinfo.Transaction, voting.Vote, voting.Phase) error {
					return errors.New("vote failed")
				}
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.NoDirExists(t, tempRepoPath)
				require.NoDirExists(t, realRepoPath)
			},
			requireError: equalError(structerr.NewFailedPrecondition("preparatory vote: %w", errors.New("vote failed"))),
		},
		{
			desc:          "failing post-commit vote",
			transactional: true,
			setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
				txManager.VoteFn = func(_ context.Context, _ txinfo.Transaction, _ voting.Vote, phase voting.Phase) error {
					if phase == voting.Preparing || phase == voting.Prepared {
						return nil
					}
					return errors.New("vote failed")
				}
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.NoDirExists(t, tempRepoPath)

				// The second vote is only a confirming vote that the node did the
				// change. So if the second vote fails, then the change must have
				// been performed and thus we'd see the repository.
				require.DirExists(t, realRepoPath)

				requireFullRepackTimestampExists(t, realRepoPath, true)
			},
			requireError: equalError(structerr.NewFailedPrecondition("committing vote: %w", errors.New("vote failed"))),
		},
		{
			desc:          "voting happens after lock",
			transactional: true,
			setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
				// We both set up transactions and create the lock. Given that we
				// should try locking the repository before casting any votes, we do
				// not expect to see a voting error.

				require.NoError(t, os.MkdirAll(filepath.Dir(repoPath), mode.Directory))
				lock, err := os.Create(repoPath + ".lock")
				require.NoError(t, err)
				require.NoError(t, lock.Close())

				txManager.VoteFn = func(context.Context, txinfo.Transaction, voting.Vote, voting.Phase) error {
					require.FailNow(t, "no votes should have happened")
					return nil
				}
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.NoDirExists(t, tempRepoPath)
				require.NoDirExists(t, realRepoPath)
			},
			requireError: equalError(structerr.NewFailedPrecondition("repository is already locked")),
		},
		func() setup {
			var votingRepo *localrepo.Repo

			return setup{
				desc:          "vote is deterministic",
				transactional: true,
				setup: func(t *testing.T, repo *gitalypb.Repository, repoPath string) {
					txManager.VoteFn = func(ctx context.Context, _ txinfo.Transaction, vote voting.Vote, phase voting.Phase) error {
						// Only run on prepared phase when the temporary repo still exists
						if phase != voting.Prepared {
							return nil
						}

						hash := voting.NewVoteHash()

						err := VoteRepository(ctx, &hash, votingRepo)
						require.NoError(t, err)

						expectedVote, err := hash.Vote()
						require.NoError(t, err)
						require.Equal(t, expectedVote, vote)

						return nil
					}
				},
				seed: func(t *testing.T, repo *gitalypb.Repository, repoPath string) error {
					// Capture the repo path we'll vote on to use in txmanager.VoteFn
					votingRepo = localrepo.NewTestRepo(t, cfg, repo)
					return nil
				},
				verify: func(t *testing.T, _ *gitalypb.Repository, tempRepoPath string, _ *gitalypb.Repository, realRepoPath string) {
					require.NoDirExists(t, tempRepoPath)
					require.DirExists(t, realRepoPath)

					requireFullRepackTimestampExists(t, realRepoPath, true)
				},
			}
		}(),
		{
			desc: "override branch",
			opts: []CreateOption{
				WithBranchName("default"),
			},
			verify: func(t *testing.T, _ *gitalypb.Repository, _ string, _ *gitalypb.Repository, realRepoPath string) {
				defaultBranch := text.ChompBytes(gittest.Exec(t, cfg, "-C", realRepoPath, "symbolic-ref", "HEAD"))
				require.Equal(t, "refs/heads/default", defaultBranch)
			},
		},
		{
			desc: "skip initialization",
			opts: []CreateOption{
				WithSkipInit(),
			},
			seed: func(t *testing.T, repo *gitalypb.Repository, repoPath string) error {
				require.NoDirExists(t, repoPath)
				gittest.Exec(t, cfg, "init", "--bare", repoPath)
				return nil
			},
			verify: func(t *testing.T, tempRepo *gitalypb.Repository, tempRepoPath string, realRepo *gitalypb.Repository, realRepoPath string) {
				require.NoDirExists(t, tempRepoPath)

				// But the new repository must exist.
				isBareRepo := gittest.Exec(t, cfg, "-C", realRepoPath, "rev-parse", "--is-bare-repository")
				require.Equal(t, "true", text.ChompBytes(isBareRepo))

				requireFullRepackTimestampExists(t, realRepoPath, true)
			},
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			// Make sure that we don't leak either the context or the mocked transaction
			// manager's data.
			ctx := ctx
			*txManager = transaction.MockManager{}
			repoCounter := counter.NewRepositoryCounter(cfg.Storages)

			if gittest.ObjectHashIsSHA256() {
				tc.opts = append(tc.opts, WithObjectHash(git.ObjectHashSHA256))
			}

			repo := &gitalypb.Repository{
				StorageName:  cfg.Storages[0].Name,
				RelativePath: gittest.NewRepositoryName(t),
			}

			if tc.transactional {
				var err error
				ctx, err = txinfo.InjectTransaction(ctx, 1, "node", true)
				require.NoError(t, err)
				ctx = peer.NewContext(ctx, &peer.Peer{})
			}

			repoPath, err := locator.GetRepoPath(ctx, repo, storage.WithRepositoryVerificationSkipped())
			require.NoError(t, err)

			if tc.setup != nil {
				tc.setup(t, repo, repoPath)
			}

			var tempRepo *gitalypb.Repository

			err = Create(ctx, logger, locator, gitCmdFactory, catfileCache, txManager, repoCounter, repo, func(tr *gitalypb.Repository) error {
				tempRepo = tr

				// The temporary repository must have been created in Gitaly's
				// temporary storage path.
				require.Equal(t, repo.GetStorageName(), tempRepo.GetStorageName())
				require.True(t, strings.HasPrefix(tempRepo.GetRelativePath(), "+gitaly/tmp/repo"))

				tempRepoPath, err := locator.GetRepoPath(ctx, tempRepo, storage.WithRepositoryVerificationSkipped())
				require.NoError(t, err)

				if tc.seed != nil {
					return tc.seed(t, tempRepo, tempRepoPath)
				}

				// Verify that the repository exists now and is a real repository.
				isBareRepo := gittest.Exec(t, cfg, "-C", tempRepoPath, "rev-parse", "--is-bare-repository")
				require.Equal(t, "true", text.ChompBytes(isBareRepo))

				return nil
			}, tc.opts...)

			if tc.requireError != nil {
				tc.requireError(t, cfg, repo.GetRelativePath(), err)
			} else {
				require.NoError(t, err)
			}

			var tempRepoPath string
			if tempRepo != nil {
				tempRepoPath, err = locator.GetRepoPath(ctx, tempRepo, storage.WithRepositoryVerificationSkipped())
				require.NoError(t, err)
			}

			require.NotNil(t, tc.verify, "test must verify results")
			tc.verify(t, tempRepo, tempRepoPath, repo, repoPath)
		})
	}
}
