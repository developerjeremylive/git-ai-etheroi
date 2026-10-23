package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/featureflag"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/service/setup"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/corestore"
	"github.com/feder-cr/git-ai/internal/graft/corestore/coretest"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/feder-cr/git-ai/internal/testhelper/testserver"
	"github.com/feder-cr/git-ai/sdk"
)

// repoCLI runs graft repo create against server and decodes its JSON output
// when the command succeeds.
func repoCLI(t *testing.T, server []string, args ...string) (int, repoCreateResult, string) {
	t.Helper()
	code, stdout, stderr := runCLI(t, append(append(append([]string{"repo", "create"}, args...), server...), "--json")...)
	var res repoCreateResult
	if code == exitOK {
		require.NoError(t, json.Unmarshal([]byte(stdout), &res), stdout)
	}
	return code, res, stderr
}

// requireRefs checks the references of the repository at path: name to commit.
func requireRefs(t *testing.T, cfg config.Cfg, path string, want map[string]string) {
	t.Helper()
	out := gittest.Exec(t, cfg, "-C", path, "for-each-ref", "--format=%(refname) %(objectname)")
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name, oid, ok := strings.Cut(line, " "); ok {
			got[name] = oid
		}
	}
	require.Equal(t, want, got)
}

// TestCLIRepoCreateAgainstServer drives graft repo create against a server
// without a storage core: an empty repository, AlreadyExists, a repository
// with its initial commit and a second branch that the CLI then clones, and a
// run on the complete repository that changes nothing. It runs serially because
// the CLI tests share the package-level backend factory.
func TestCLIRepoCreateAgainstServer(t *testing.T) {
	env := startServer(t)
	server := []string{"-address", env.addr, "-storage", env.storage}
	storagePath := env.cfg.Storages[0].Path

	code, stdout, stderr := runCLI(t, append([]string{"repo", "create", "empty.git"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "created repository empty.git on storage "+env.storage+"\n", stdout)
	requireRefs(t, env.cfg, filepath.Join(storagePath, "empty.git"), map[string]string{})

	code, _, stderr = repoCLI(t, server, "empty.git")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "repository empty.git exists already\n")

	// The initial commit and a second branch make the repository clonable.
	code, seeded, stderr := repoCLI(t, server, "seeded.git", "--initial-commit", "--branch", "feature/demo")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, repoCreateResult{
		Repository:           "seeded.git",
		Storage:              env.storage,
		Created:              true,
		DefaultBranch:        "main",
		InitialCommit:        seeded.InitialCommit,
		InitialCommitCreated: true,
		BranchesCreated:      []string{"feature/demo"},
	}, seeded)
	requireRefs(t, env.cfg, filepath.Join(storagePath, "seeded.git"), map[string]string{
		"refs/heads/main": seeded.InitialCommit, "refs/heads/feature/demo": seeded.InitialCommit,
	})
	readme, err := env.backend(t).client.Repo("seeded.git").ReadFile(env.ctx, "main", "README.md")
	require.NoError(t, err)
	require.Equal(t, "# seeded\n", string(readme))

	dir := filepath.Join(t.TempDir(), "seeded")
	code, _, stderr = runCLI(t, "clone", env.addr, "seeded.git", dir, "-storage", env.storage, "--branch", "feature/demo")
	require.Equal(t, exitOK, code, stderr)
	require.FileExists(t, filepath.Join(dir, "README.md"))

	// The same command again converges: nothing is created.
	code, stdout, stderr = runCLI(t, append([]string{"repo", "create", "seeded.git", "--initial-commit", "--branch", "feature/demo"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "repository seeded.git exists\nmain is "+seeded.InitialCommit+"\nbranch feature/demo exists\n", stdout)

	code, sha256Repo, stderr := repoCLI(t, server, "sha256.git", "--object-format", "sha256", "--initial-commit")
	require.Equal(t, exitOK, code, stderr)
	require.Len(t, sha256Repo.InitialCommit, 64)
}

// TestCLIRepoCreateConverges runs graft repo create on existing repositories in
// the states that a partial failure or another client leaves, and checks that it
// completes them exactly or refuses without writing anything.
func TestCLIRepoCreateConverges(t *testing.T) {
	env := startServer(t)
	server := []string{"-address", env.addr, "-storage", env.storage}
	storagePath := env.cfg.Storages[0].Path

	t.Run("empty repository gets its initial commit", func(t *testing.T) {
		code, _, stderr := repoCLI(t, server, "resumed.git")
		require.Equal(t, exitOK, code, stderr)
		code, res, stderr := repoCLI(t, server, "resumed.git", "--initial-commit", "--branch", "next")
		require.Equal(t, exitOK, code, stderr)
		require.False(t, res.Created)
		require.True(t, res.InitialCommitCreated)
		requireRefs(t, env.cfg, filepath.Join(storagePath, "resumed.git"), map[string]string{
			"refs/heads/main": res.InitialCommit, "refs/heads/next": res.InitialCommit,
		})
	})

	t.Run("HEAD on another branch", func(t *testing.T) {
		code, _, stderr := repoCLI(t, server, "trunk.git", "--default-branch", "trunk")
		require.Equal(t, exitOK, code, stderr)

		// The default branch of the command is main, but HEAD points to trunk.
		code, _, stderr = repoCLI(t, server, "trunk.git", "--initial-commit")
		require.Equal(t, exitError, code)
		require.Contains(t, stderr, "HEAD points to refs/heads/trunk, not refs/heads/main: pass --default-branch trunk")
		requireRefs(t, env.cfg, filepath.Join(storagePath, "trunk.git"), map[string]string{})

		code, res, stderr := repoCLI(t, server, "trunk.git", "--initial-commit", "--default-branch", "trunk")
		require.Equal(t, exitOK, code, stderr)
		requireRefs(t, env.cfg, filepath.Join(storagePath, "trunk.git"), map[string]string{"refs/heads/trunk": res.InitialCommit})
	})

	t.Run("references but no default branch", func(t *testing.T) {
		for _, ref := range []string{"refs/tags/v1", "refs/merge-requests/1/head"} {
			rel := strings.ReplaceAll(ref, "/", "-") + ".git"
			code, _, stderr := repoCLI(t, server, rel)
			require.Equal(t, exitOK, code, stderr)
			path := filepath.Join(storagePath, rel)
			commit := gittest.WriteCommit(t, env.cfg, path, gittest.WithReference(ref))

			code, _, stderr = repoCLI(t, server, rel, "--initial-commit")
			require.Equal(t, exitError, code, ref)
			require.Contains(t, stderr, "the repository has references but no refs/heads/main: it is not empty, so no initial commit is made", ref)
			requireRefs(t, env.cfg, path, map[string]string{ref: commit.String()})
		}
	})

	t.Run("existing default branch is the initial commit", func(t *testing.T) {
		code, _, stderr := repoCLI(t, server, "existing.git")
		require.Equal(t, exitOK, code, stderr)
		path := filepath.Join(storagePath, "existing.git")
		commit := gittest.WriteCommit(t, env.cfg, path, gittest.WithBranch("main"))

		code, res, stderr := repoCLI(t, server, "existing.git", "--initial-commit", "--branch", "side", "--branch", "side", "--branch", "main")
		require.Equal(t, exitOK, code, stderr)
		require.Equal(t, commit.String(), res.InitialCommit)
		require.False(t, res.InitialCommitCreated)
		require.Equal(t, []string{"side"}, res.BranchesCreated, "duplicates and the default branch are dropped")
		requireRefs(t, env.cfg, path, map[string]string{"refs/heads/main": commit.String(), "refs/heads/side": commit.String()})
	})

	t.Run("concurrent runs converge", func(t *testing.T) {
		const runs = 4
		var wg sync.WaitGroup
		results := make([]repoCreateResult, runs)
		codes := make([]int, runs)
		stderrs := make([]string, runs)
		for i := 0; i < runs; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				codes[i], results[i], stderrs[i] = repoCLI(t, server, "raced.git", "--initial-commit", "--branch", "feature")
			}(i)
		}
		wg.Wait()

		// Every run converges on the repository, or fails cleanly because another
		// run held the repository's lock while creating it.
		created, commits := 0, 0
		for i := 0; i < runs; i++ {
			if codes[i] != exitOK {
				require.Equal(t, exitError, codes[i], "run %d: %s", i, stderrs[i])
				require.Contains(t, stderrs[i], "repository raced.git is being created or removed by another request", "run %d", i)
				continue
			}
			if results[i].Created {
				created++
			}
			if results[i].InitialCommitCreated {
				commits++
			}
		}
		require.Equal(t, 1, created, "one run created the repository")
		require.Equal(t, 1, commits, "one run made the initial commit")

		// The same command afterwards changes nothing.
		path := filepath.Join(storagePath, "raced.git")
		tip := strings.TrimSpace(string(gittest.Exec(t, env.cfg, "-C", path, "rev-parse", "refs/heads/main")))
		code, again, stderr := repoCLI(t, server, "raced.git", "--initial-commit", "--branch", "feature")
		require.Equal(t, exitOK, code, stderr)
		require.Equal(t, tip, again.InitialCommit)
		require.False(t, again.InitialCommitCreated)
		require.Equal(t, []string{"feature"}, again.BranchesExisting)
		for i := 0; i < runs; i++ {
			if codes[i] == exitOK {
				require.Equal(t, tip, results[i].InitialCommit, "run %d", i)
			}
		}
		requireRefs(t, env.cfg, path, map[string]string{"refs/heads/main": tip, "refs/heads/feature": tip})
	})
}

// TestCLIRepoCreateOperatorOnly checks that only the shared token creates a
// repository, that the initial commit and the branches pass the access policy
// as the configured user, and that a branch the policy denies is reported as a
// denial with what is done and left, and is completed by the same command with
// a user the policy allows.
func TestCLIRepoCreateOperatorOnly(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = "operator-secret"
	cfg.Graft.Standalone = true
	cfg.Graft.Agents = []config.GraftAgent{{Name: "alice", Token: "alice-secret"}}
	cfg.Graft.Policy = []config.GraftPolicyRule{
		{Actor: "alice", Refs: []string{"refs/heads/main"}, Allow: true},
		{Actor: "bob", Refs: []string{"refs/heads/release"}, Allow: true},
		{Actor: "*", Allow: false},
	}
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))))
	server := []string{"-address", addr, "-storage", cfg.Storages[0].Name}
	path := filepath.Join(cfg.Storages[0].Path, "a.git")

	code, _, stderr := repoCLI(t, append([]string{"-token", "alice-secret"}, server...), "a.git")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "PermissionDenied")
	require.Contains(t, stderr, "hint: creating a repository needs the shared (operator) token")
	require.NoDirExists(t, path)

	operator := append([]string{"-token", "operator-secret"}, server...)
	code, _, stderr = repoCLI(t, append([]string{"-user", "alice"}, operator...), "a.git", "--initial-commit", "--branch", "release")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "repository a.git: denied by the server: ")
	require.Contains(t, stderr, "\ndone: created the repository; initial commit ")
	require.Contains(t, stderr, "\nleft: branch release\n")
	require.Contains(t, stderr, "the same command is denied again until the policy allows")
	require.NotContains(t, stderr, "run the same command again")

	code, res, stderr := repoCLI(t, append([]string{"-user", "bob"}, operator...), "a.git", "--initial-commit", "--branch", "release")
	require.Equal(t, exitOK, code, stderr)
	require.False(t, res.Created)
	require.False(t, res.InitialCommitCreated)
	require.Equal(t, []string{"release"}, res.BranchesCreated)
	requireRefs(t, cfg, path, map[string]string{"refs/heads/main": res.InitialCommit, "refs/heads/release": res.InitialCommit})

	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(cfg.Storages[0].Name), sdk.WithAgentToken("alice-secret"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	readme, err := client.Repo("a.git").ReadFile(ctx, "main", "README.md")
	require.NoError(t, err)
	require.Equal(t, "# a\n", string(readme))
}

// TestCLIRepoCreateStorageCore checks graft repo create with a storage core: in
// authoritative mode the repository is registered with its initial commit and
// the verification finds the local copy equal to the database; in shadow mode
// the command says that the repository is not tracked yet. It runs serially
// because the CLI tests share the package-level backend factory.
func TestCLIRepoCreateStorageCore(t *testing.T) {
	for _, mode := range []corestore.Mode{corestore.ModeAuthoritative, corestore.ModeShadow} {
		t.Run(string(mode), func(t *testing.T) {
			cfg := testcfg.Build(t)
			cfg.Graft.Standalone = true
			testcfg.BuildGitalySSH(t, cfg)
			testcfg.BuildGitalyHooks(t, cfg)
			core := coretest.Open(t, coretest.Options{Mode: mode})
			addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(core))
			target := []string{"-address", addr, "-storage", cfg.Storages[0].Name}

			code, stdout, stderr := runCLI(t, append([]string{"repo", "create", "core.git", "--initial-commit", "--branch", "feature/demo"}, target...)...)
			require.Equal(t, exitOK, code, stderr)
			hint := "the storage core runs in shadow mode: run graft storage import -repo core.git to track the repository\n"
			if mode == corestore.ModeShadow {
				require.Contains(t, stdout, hint)
				return
			}
			require.NotContains(t, stdout, hint)

			code, stdout, stderr = runCLI(t, append([]string{"storage", "state", "--json", "-repo", "core.git"}, target...)...)
			require.Equal(t, exitOK, code, stderr)
			var state sdk.StorageState
			require.NoError(t, json.Unmarshal([]byte(stdout), &state))
			require.True(t, state.Tracked)
			require.EqualValues(t, 3, state.HeadSequence, "the creation, the initial commit and the branch")

			code, stdout, stderr = runCLI(t, append([]string{"storage", "verify", "--connectivity", "-repo", "core.git"}, target...)...)
			require.Equal(t, exitOK, code, stderr)
			require.Equal(t, "head sequence 3\nconnectivity ok\nconsistent\n", stdout)
			format := gittest.Exec(t, cfg, "-C", filepath.Join(cfg.Storages[0].Path, "core.git"), "rev-parse", "--show-ref-format")
			require.Equal(t, "reftable", strings.TrimSpace(string(format)), "authoritative mode creates reftable repositories")
		})
	}
}

// TestCLIRepoUsageErrorsDoNotContactServer checks the arguments of graft repo,
// the branch names included, before any server call: the address points
// nowhere, so a server call would fail with a different error.
func TestCLIRepoUsageErrorsDoNotContactServer(t *testing.T) {
	nowhere := []string{"-address", "unix://" + filepath.Join(t.TempDir(), "missing.sock")}

	cases := map[string][]string{
		"no subcommand":                 {"repo"},
		"unknown subcommand":            {"repo", "delete", "a.git"},
		"no repository":                 {"repo", "create"},
		"two repositories":              {"repo", "create", "a.git", "b.git"},
		"branch without initial commit": {"repo", "create", "a.git", "--branch", "x"},
		"unknown object format":         {"repo", "create", "a.git", "--object-format", "md5"},
		"empty default branch":          {"repo", "create", "a.git", "--default-branch", ""},
		"invalid default branch":        {"repo", "create", "a.git", "--default-branch", "a..b"},
		"default branch HEAD":           {"repo", "create", "a.git", "--default-branch", "HEAD.lock"},
		"invalid branch":                {"repo", "create", "a.git", "--initial-commit", "--branch", "bad name"},
		"invalid second branch":         {"repo", "create", "a.git", "--initial-commit", "--branch", "ok", "--branch", "x~1"},
		"author without name":           {"repo", "create", "a.git", "-author", "<a@b.c>"},
	}
	for name, args := range cases {
		code, _, stderr := runCLI(t, append(args, nowhere...)...)
		require.Equal(t, exitUsage, code, "%s: %s", name, stderr)
	}

	code, _, stderr := runCLI(t, "repo", "create", "a.git", "-address", "")
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, "needs the server address")
}

// TestCLIRepoHelp checks that graft help lists the repo command and that its
// help shows the synopsis and the rules.
func TestCLIRepoHelp(t *testing.T) {
	code, stdout, _ := runCLI(t, "help")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "  repo create REPO ")

	code, _, stderr := runCLI(t, "repo", "-h")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stderr, repoSynopsis)
	require.Contains(t, stderr, "shared (operator) token")
	require.Contains(t, stderr, "graft storage import -repo REPO")

	code, _, stderr = runCLI(t, "repo")
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, repoSynopsis)
}

func TestRepositoryName(t *testing.T) {
	require.Equal(t, "demo", repositoryName("demo.git"))
	require.Equal(t, "demo", repositoryName("group/demo.git"))
	require.Equal(t, "plain", repositoryName("plain"))
}
