package manager

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/git/housekeeping"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/stats"
	gitalycfgprom "github.com/feder-cr/git-ai/internal/gitaly/config/prometheus"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"
)

// setupPackRefsRepository creates a repository with the given reference backend and tables branches,
// so that git-pack-refs(1) --auto has work to do: on reftable each branch is a table of its own that
// auto-compaction left alone, on files a loose reference (files packs from 16 of them).
func setupPackRefsRepository(t *testing.T, ctx context.Context, backend git.ReferenceBackend, tables int) (*localrepo.Repo, string) {
	t.Helper()

	cfg := testcfg.Build(t)
	repoProto, repoPath := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
		ReferenceBackend:       backend,
	})
	commitID := gittest.WriteCommit(t, cfg, repoPath, gittest.WithBranch("main"))
	for i := 1; i < tables; i++ {
		gittest.ExecOpts(t, cfg, gittest.ExecConfig{
			Env: []string{"GIT_TEST_REFTABLE_AUTOCOMPACTION=false"},
		}, "-C", repoPath, "update-ref", fmt.Sprintf("refs/heads/branch-%d", i), commitID.String())
	}

	return localrepo.NewTestRepo(t, cfg, repoProto), repoPath
}

func alwaysPackReferences(context.Context) bool { return true }

// forEachBundledGit runs test once with each bundled git, master and prev, selected by the feature
// flag in the context, instead of the one testhelper.Context picks at random: the reftable
// contention errors differ between them.
func forEachBundledGit(t *testing.T, test func(t *testing.T, ctx context.Context)) {
	t.Helper()

	gitCmdFactory := gittest.NewCommandFactory(t, testcfg.Build(t))
	versions := map[bool]string{}
	for _, master := range []bool{true, false} {
		version, err := gitCmdFactory.GitVersion(featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GitMaster, master))
		require.NoError(t, err)
		versions[master] = version
	}
	require.NotEqual(t, versions[true], versions[false], "the feature flag does not select another git")

	for _, master := range []bool{true, false} {
		t.Run(testhelper.DependingOn(master, "git-master", "git-prev"), func(t *testing.T) {
			t.Parallel()
			t.Logf("git %s", versions[master])

			test(t, featureflag.ContextWithFeatureFlag(testhelper.Context(t), featureflag.GitMaster, master))
		})
	}
}

// TestPackRefsIfNeeded_reftableContention runs the real git-pack-refs(1) of both bundled gits while
// reftable's tables.list.lock is held, as a concurrent writer holds it, and pins the exit codes and
// stderr that isReftableContention classifies: a git that changes them fails here.
func TestPackRefsIfNeeded_reftableContention(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		tables      int
		expectedErr string
	}{
		{
			// Nothing to compact: reftable_stack_clean fails to take the lock.
			desc:        "lock held, nothing to compact",
			tables:      1,
			expectedErr: `reftable compaction contended: packing refs: exit status 251, stderr: ""`,
		},
		{
			// The compaction fails to take the lock.
			desc:        "lock held, stack to compact",
			tables:      10,
			expectedErr: `reftable compaction contended: packing refs: exit status 255, stderr: "error: unable to compact stack: data is locked\n"`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			forEachBundledGit(t, func(t *testing.T, ctx context.Context) {
				repo, repoPath := setupPackRefsRepository(t, ctx, git.ReferenceBackendReftables, tc.tables)
				manager := New(gitalycfgprom.Config{}, testhelper.NewLogger(t), nil, nil)
				strategy := mockOptimizationStrategy{shouldRepackReferences: alwaysPackReferences}

				lockPath := filepath.Join(repoPath, "reftable", "tables.list.lock")
				require.NoError(t, os.WriteFile(lockPath, nil, 0o644))

				didPackRefs, err := manager.packRefsIfNeeded(ctx, repo, strategy)
				require.ErrorIs(t, err, errReftableCompactionContended)
				require.EqualError(t, err, tc.expectedErr)
				require.False(t, didPackRefs)

				// Once the writer is gone, the stack is compacted into a single table.
				require.NoError(t, os.Remove(lockPath))
				didPackRefs, err = manager.packRefsIfNeeded(ctx, repo, strategy)
				require.NoError(t, err)
				require.True(t, didPackRefs)
				require.Len(t, reftableTables(t, repoPath), 1)
			})
		})
	}
}

// reftableTables returns the tables of the reftable stack of the repository at repoPath.
func reftableTables(t *testing.T, repoPath string) []string {
	t.Helper()

	return strings.Fields(string(testhelper.MustReadFile(t, filepath.Join(repoPath, "reftable", "tables.list"))))
}

// TestOptimizeRepository_staleReftableTableLock leaves the lock of a table being compacted behind, as
// a git killed during the compaction does, and checks that housekeeping recovers: once the lock is
// older than the grace period, CleanStaleData removes it and the stack is compacted again. While it
// is fresh, it is a concurrent compaction: the lock stays and the reference packing is skipped.
func TestOptimizeRepository_staleReftableTableLock(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc               string
		age                time.Duration
		expectedLock       bool
		expectedPackedRefs map[string]float64
		expectedTables     int
	}{
		{
			desc:               "fresh lock",
			age:                0,
			expectedLock:       true,
			expectedPackedRefs: map[string]float64{"success": 1, "skipped": 1},
			expectedTables:     2,
		},
		{
			desc:               "stale lock",
			age:                housekeeping.ReftableLockfileGracePeriod + time.Minute,
			expectedLock:       false,
			expectedPackedRefs: map[string]float64{"success": 2, "skipped": 0},
			expectedTables:     1,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			forEachBundledGit(t, func(t *testing.T, ctx context.Context) {
				repo, repoPath := setupPackRefsRepository(t, ctx, git.ReferenceBackendReftables, 10)

				// The lock of the oldest table: a compaction locks the tables from the newest
				// down, so git compacts the newer ones around it, and the oldest table and the
				// compacted one stay apart for as long as the lock is there.
				lockPath := filepath.Join(repoPath, "reftable", reftableTables(t, repoPath)[0]+".lock")
				require.NoError(t, os.WriteFile(lockPath, nil, 0o644))
				lockTime := time.Now().Add(-tc.age)
				require.NoError(t, os.Chtimes(lockPath, lockTime, lockTime))

				manager := New(gitalycfgprom.Config{}, testhelper.NewLogger(t), nil, nil)
				optimize := func() {
					require.NoError(t, manager.OptimizeRepository(ctx, repo, WithOptimizationStrategyConstructor(
						func(stats.RepositoryInfo) housekeeping.OptimizationStrategy {
							return mockOptimizationStrategy{shouldRepackReferences: alwaysPackReferences}
						},
					)))
				}

				// The first run compacts the tables newer than a held lock; the second finds only
				// the locked table and the compacted one, and cannot compact them.
				optimize()
				optimize()

				_, err := os.Stat(lockPath)
				require.Equal(t, tc.expectedLock, err == nil, "lock exists: %v", err)
				require.Len(t, reftableTables(t, repoPath), tc.expectedTables)
				for status, count := range tc.expectedPackedRefs {
					require.Equal(t, count, testutil.ToFloat64(
						manager.metrics.TasksTotal.WithLabelValues("packed_refs", status)), status)
				}
			})
		})
	}
}

// TestPackRefsIfNeeded_failuresStayFatal checks that git-pack-refs(1) failures other than reftable
// contention are returned as they are: a broken reftable stack, and a held lock on the files backend,
// whose behaviour does not change.
func TestPackRefsIfNeeded_failuresStayFatal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc        string
		backend     git.ReferenceBackend
		setup       func(t *testing.T, repoPath string)
		expectedErr string
	}{
		{
			desc:    "reftable stack naming a missing table",
			backend: git.ReferenceBackendReftables,
			setup: func(t *testing.T, repoPath string) {
				tablesList, err := os.OpenFile(filepath.Join(repoPath, "reftable", "tables.list"), os.O_APPEND|os.O_WRONLY, 0)
				require.NoError(t, err)
				_, err = tablesList.WriteString("0x000000000009-0x000000000009-deadbeef.ref\n")
				require.NoError(t, err)
				require.NoError(t, tablesList.Close())
			},
			expectedErr: `packing refs: exit status 252, stderr: ""`,
		},
		{
			desc:    "files packed-refs.lock held",
			backend: git.ReferenceBackendFiles,
			setup: func(t *testing.T, repoPath string) {
				require.NoError(t, os.WriteFile(filepath.Join(repoPath, "packed-refs.lock"), nil, 0o644))
			},
			expectedErr: "packing refs: exit status 128",
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			repo, repoPath := setupPackRefsRepository(t, ctx, tc.backend, 32)
			tc.setup(t, repoPath)
			manager := New(gitalycfgprom.Config{}, testhelper.NewLogger(t), nil, nil)

			didPackRefs, err := manager.packRefsIfNeeded(ctx, repo, mockOptimizationStrategy{
				shouldRepackReferences: alwaysPackReferences,
			})
			require.False(t, errors.Is(err, errReftableCompactionContended), "classified as contention: %v", err)
			require.ErrorContains(t, err, tc.expectedErr)
			require.False(t, didPackRefs)
		})
	}
}

// TestOptimizeRepository_packRefsContention checks how OptimizeRepository reports git-pack-refs(1)
// failures: reftable contention is skipped, logged as a warning and counted with the "skipped"
// status while the commit-graph is still written; on the files backend a held lock still fails the
// housekeeping.
func TestOptimizeRepository_packRefsContention(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc            string
		backend         git.ReferenceBackend
		lockFile        string
		expectedErr     string
		expectedMetrics string
		expectedWarning bool
	}{
		{
			desc:        "reftable",
			backend:     git.ReferenceBackendReftables,
			lockFile:    filepath.Join("reftable", "tables.list.lock"),
			expectedErr: "",
			expectedMetrics: `gitaly_housekeeping_tasks_total{housekeeping_task="packed_refs", status="skipped"} 1
gitaly_housekeeping_tasks_total{housekeeping_task="total", status="success"} 1
gitaly_housekeeping_tasks_total{housekeeping_task="written_commit_graph_incremental", status="success"} 1
`,
			expectedWarning: true,
		},
		{
			desc:        "files",
			backend:     git.ReferenceBackendFiles,
			lockFile:    "packed-refs.lock",
			expectedErr: "could not pack refs: packing refs: exit status 128",
			expectedMetrics: `gitaly_housekeeping_tasks_total{housekeeping_task="packed_refs", status="failure"} 1
gitaly_housekeeping_tasks_total{housekeeping_task="total", status="failure"} 1
`,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			repo, repoPath := setupPackRefsRepository(t, ctx, tc.backend, 32)
			require.NoError(t, os.WriteFile(filepath.Join(repoPath, tc.lockFile), nil, 0o644))

			logger := testhelper.NewLogger(t)
			hook := testhelper.AddLoggerHook(logger)
			manager := New(gitalycfgprom.Config{}, logger, nil, nil)

			err := manager.OptimizeRepository(ctx, repo, WithOptimizationStrategyConstructor(
				func(stats.RepositoryInfo) housekeeping.OptimizationStrategy {
					return mockOptimizationStrategy{
						shouldRepackReferences: alwaysPackReferences,
						shouldWriteCommitGraph: true,
					}
				},
			))
			if tc.expectedErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.expectedErr)
			}

			testhelper.RequirePromMetrics(t, manager.metrics.TasksTotal,
				"# HELP gitaly_housekeeping_tasks_total Total number of housekeeping tasks performed in the repository\n"+
					"# TYPE gitaly_housekeeping_tasks_total counter\n"+tc.expectedMetrics)

			var warnings []string
			for _, entry := range hook.AllEntries() {
				if entry.Level <= logrus.WarnLevel {
					warnings = append(warnings, entry.Message)
				}
			}
			if tc.expectedWarning {
				require.Equal(t, []string{"skipped packing references: reftable stack locked or modified concurrently"}, warnings)
			} else {
				require.Empty(t, warnings)
			}
		})
	}
}
