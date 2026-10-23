package manager

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/catfile"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/quarantine"
	"github.com/feder-cr/git-ai/internal/git/stats"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
)

func TestWriteCommitGraphIfIdle(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)

	setup := func(t *testing.T) (*RepositoryManager, *localrepo.Repo, string) {
		repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
			SkipCreationViaService: true,
		})
		gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))

		return New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil), localrepo.NewTestRepo(t, cfg, repoProto), repoPath
	}

	chainPath := func(repoPath string) string {
		return filepath.Join(repoPath, "objects", "info", "commit-graphs", "commit-graph-chain")
	}

	t.Run("idle repository", func(t *testing.T) {
		manager, repo, repoPath := setup(t)

		wrote, err := manager.WriteCommitGraphIfIdle(ctx, repo)
		require.NoError(t, err)
		require.True(t, wrote)
		require.FileExists(t, chainPath(repoPath))

		require.Empty(t, manager.repositoryStates.values, "the state is released after the write")
	})

	t.Run("housekeeping running", func(t *testing.T) {
		manager, repo, repoPath := setup(t)

		ok, cleanup := manager.repositoryStates.tryRunningHousekeeping(repo)
		require.True(t, ok)

		wrote, err := manager.WriteCommitGraphIfIdle(ctx, repo)
		require.NoError(t, err)
		require.False(t, wrote, "the write is skipped while housekeeping runs")
		require.NoFileExists(t, chainPath(repoPath))

		cleanup()

		wrote, err = manager.WriteCommitGraphIfIdle(ctx, repo)
		require.NoError(t, err)
		require.True(t, wrote, "the write runs once housekeeping finished")
		require.Empty(t, manager.repositoryStates.values)
	})

	t.Run("another background write running", func(t *testing.T) {
		manager, repo, _ := setup(t)

		ok, cleanup := manager.repositoryStates.tryRunningBackgroundWrite(repo)
		require.True(t, ok)
		defer cleanup()

		wrote, err := manager.WriteCommitGraphIfIdle(ctx, repo)
		require.NoError(t, err)
		require.False(t, wrote)
	})

	t.Run("housekeeping waits for a running background write", func(t *testing.T) {
		manager, repo, _ := setup(t)

		optimized := make(chan struct{})
		manager.optimizeFunc = func(context.Context, *localrepo.Repo, housekeeping.OptimizationStrategy) error {
			close(optimized)
			return nil
		}

		ok, cleanup := manager.repositoryStates.tryRunningBackgroundWrite(repo)
		require.True(t, ok)

		done := make(chan error, 1)
		go func() { done <- manager.OptimizeRepository(ctx, repo) }()

		select {
		case <-optimized:
			require.FailNow(t, "housekeeping ran while a background commit-graph write was running")
		case <-time.After(50 * time.Millisecond):
		}

		cleanup()

		<-optimized
		require.NoError(t, <-done)
		require.Empty(t, manager.repositoryStates.values)
	})

	t.Run("housekeeping waiting for a background write honours cancellation", func(t *testing.T) {
		manager, repo, _ := setup(t)
		manager.optimizeFunc = func(context.Context, *localrepo.Repo, housekeeping.OptimizationStrategy) error {
			require.FailNow(t, "housekeeping must not run")
			return nil
		}

		ok, cleanup := manager.repositoryStates.tryRunningBackgroundWrite(repo)
		require.True(t, ok)
		defer cleanup()

		cancelCtx, cancel := context.WithCancel(ctx)
		cancel()

		require.ErrorIs(t, manager.OptimizeRepository(cancelCtx, repo), context.Canceled)
	})
}

// TestWriteCommitGraphIfIdle_ConcurrentWithOptimizeRepository runs background commit-graph writes
// while OptimizeRepository writes the commit-graph of the same repository. Without the shared
// guard, one of the two failed on the commit-graph chain lock in most rounds. On reftable the
// commits race OptimizeRepository's git-pack-refs(1) too, whose contention errors differ between the
// bundled gits: the test runs with both.
func TestWriteCommitGraphIfIdle_ConcurrentWithOptimizeRepository(t *testing.T) {
	t.Parallel()

	forEachBundledGit(t, testWriteCommitGraphIfIdleConcurrentWithOptimizeRepository)
}

func testWriteCommitGraphIfIdleConcurrentWithOptimizeRepository(t *testing.T, ctx context.Context) {
	cfg := testcfg.Build(t)

	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})
	repo := localrepo.NewTestRepo(t, cfg, repoProto)
	manager := New(gitalycfgprom.Config{}, testhelper.SharedLogger(t), nil, nil)

	parents := map[string]git.ObjectID{}
	commits := 0
	locator := config.NewLocator(cfg)
	objectHash := gittest.DefaultObjectHash
	// One factory serves every commit, including those of the background goroutine: building a
	// test repository there could fail the test from outside its goroutine.
	catfileCache := catfile.NewCache(cfg)
	t.Cleanup(catfileCache.Stop)
	repoFactory := localrepo.NewFactory(testhelper.SharedLogger(t), locator, gittest.NewCommandFactory(t, cfg), catfileCache)
	// writeCommit lands a commit on branch as the server's write RPCs do: it is written into a
	// quarantine, which Migrate links into the repository, and then the branch is updated. A
	// commit written straight into the repository's object directory would race housekeeping's
	// git prune-packed, which removes an empty fan-out directory between git's mkdir of it and
	// the single retry of its write (git's create_tmpfile). Migrate creates the directory again.
	writeCommit := func(branch string) error {
		commits++
		quarantineDir, cleanup, err := quarantine.New(ctx, repoProto, testhelper.SharedLogger(t), locator)
		if err != nil {
			return fmt.Errorf("quarantine: %w", err)
		}
		defer cleanup()

		var commitParents []git.ObjectID
		old := objectHash.ZeroOID
		if parent, ok := parents[branch]; ok {
			commitParents = []git.ObjectID{parent}
			old = parent
		}
		// The commits are not signed, as the server's own commits without a signing key.
		writeCtx := featureflag.ContextWithFeatureFlag(ctx, featureflag.GPGSigning, false)
		commit, err := repoFactory.Build(quarantineDir.QuarantinedRepo()).WriteCommit(writeCtx, localrepo.WriteCommitConfig{
			TreeID:        objectHash.EmptyTreeOID,
			Parents:       commitParents,
			AuthorName:    "writer",
			CommitterName: "writer",
			Message:       fmt.Sprintf("%s %d", branch, commits),
		})
		if err != nil {
			return fmt.Errorf("writing commit: %w", err)
		}
		if err := quarantineDir.Migrate(ctx); err != nil {
			return fmt.Errorf("migrating quarantine: %w", err)
		}
		if out, err := gittest.NewCommand(t, cfg, "-C", repoPath, "update-ref", "refs/heads/"+branch, commit.String(), old.String()).CombinedOutput(); err != nil {
			return fmt.Errorf("updating %s: %w: %s", branch, err, out)
		}
		parents[branch] = commit
		return nil
	}

	for i := 0; i < 50; i++ {
		require.NoError(t, writeCommit("main"))
	}

	for round := 0; round < 15; round++ {
		require.NoError(t, writeCommit("main"))

		var wg sync.WaitGroup
		var backgroundErr error
		optimizeDone := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Keep landing commits and writing the commit-graph until housekeeping finished, so
			// that the writes overlap with the commit-graph step of housekeeping. A write
			// without new commits would not take the chain lock.
			for {
				if err := writeCommit("side"); err != nil {
					backgroundErr = err
					return
				}

				wrote, err := manager.WriteCommitGraphIfIdle(ctx, repo)
				if err != nil {
					backgroundErr = err
					return
				}

				select {
				case <-optimizeDone:
					return
				default:
				}

				if !wrote {
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()

		optimizeErr := manager.OptimizeRepository(ctx, repo,
			WithOptimizationStrategyConstructor(func(info stats.RepositoryInfo) housekeeping.OptimizationStrategy {
				return housekeeping.NewEagerOptimizationStrategy(info)
			}),
		)
		close(optimizeDone)
		wg.Wait()

		require.NoError(t, optimizeErr, "round %d", round)
		require.NoError(t, backgroundErr, "round %d", round)
	}
}
