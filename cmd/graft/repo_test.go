package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
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

// repoCreated is the JSON output of graft repo create.
type repoCreated struct {
	Repository    string   `json:"repository"`
	Storage       string   `json:"storage"`
	Created       bool     `json:"created"`
	DefaultBranch string   `json:"default_branch"`
	InitialCommit string   `json:"initial_commit"`
	Branches      []string `json:"branches"`
}

// TestCLIRepoCreateAgainstServer drives graft repo create against a server
// without a storage core: an empty repository, a repository that exists
// already, a repository with its initial commit and a second branch that the
// CLI then clones, and the retry that adds the initial commit to an existing
// empty repository. It runs serially because the CLI tests share the
// package-level backend factory.
func TestCLIRepoCreateAgainstServer(t *testing.T) {
	env := startServer(t)
	server := []string{"-address", env.addr, "-storage", env.storage}
	create := func(args ...string) (int, string, string) {
		return runCLI(t, append(append([]string{"repo", "create"}, args...), server...)...)
	}
	b := env.backend(t)

	code, stdout, stderr := create("empty.git")
	require.Equal(t, exitOK, code, stderr)
	require.Equal(t, "created repository empty.git on storage "+env.storage+"\n", stdout)
	require.DirExists(t, filepath.Join(env.cfg.Storages[0].Path, "empty.git"))

	code, _, stderr = create("empty.git")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "repository empty.git exists already\n")

	// The initial commit and a second branch make the repository clonable.
	code, stdout, stderr = create("seeded.git", "--initial-commit", "--branch", "feature/demo", "--json")
	require.Equal(t, exitOK, code, stderr)
	var seeded repoCreated
	require.NoError(t, json.Unmarshal([]byte(stdout), &seeded))
	require.Equal(t, repoCreated{
		Repository:    "seeded.git",
		Storage:       env.storage,
		Created:       true,
		DefaultBranch: "main",
		InitialCommit: seeded.InitialCommit,
		Branches:      []string{"feature/demo"},
	}, seeded)
	repo := b.client.Repo("seeded.git")
	for _, branch := range []string{"main", "feature/demo"} {
		commit, err := repo.ResolveRevision(env.ctx, branch)
		require.NoError(t, err, branch)
		require.Equal(t, seeded.InitialCommit, commit, branch)
	}
	readme, err := repo.ReadFile(env.ctx, "main", "README.md")
	require.NoError(t, err)
	require.Equal(t, "# seeded\n", string(readme))

	dir := filepath.Join(t.TempDir(), "seeded")
	code, _, stderr = runCLI(t, "clone", env.addr, "seeded.git", dir, "-storage", env.storage, "--branch", "feature/demo")
	require.Equal(t, exitOK, code, stderr)
	require.FileExists(t, filepath.Join(dir, "README.md"))

	// An earlier run that created the repository but not its initial commit is
	// completed by running the command again.
	code, _, stderr = create("empty.git", "--initial-commit")
	require.Equal(t, exitOK, code, stderr)
	code, stdout, stderr = create("resumed.git")
	require.Equal(t, exitOK, code, stderr)
	code, stdout, stderr = create("resumed.git", "--initial-commit", "--default-branch", "trunk")
	require.Equal(t, exitOK, code, stderr)
	require.Regexp(t, `^repository resumed.git exists without branches: adding the initial commit\ninitial commit [0-9a-f]+ on trunk\n$`, stdout)
	_, err = b.client.Repo("resumed.git").ResolveRevision(env.ctx, "trunk")
	require.NoError(t, err)

	// A repository with branches is not touched again.
	code, _, stderr = create("resumed.git", "--initial-commit")
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "repository resumed.git exists already and has branches\n")

	code, stdout, stderr = create("sha256.git", "--object-format", "sha256", "--initial-commit", "--json")
	require.Equal(t, exitOK, code, stderr)
	var sha256Repo repoCreated
	require.NoError(t, json.Unmarshal([]byte(stdout), &sha256Repo))
	require.Len(t, sha256Repo.InitialCommit, 64)
}

// TestCLIRepoCreateOperatorOnly checks that only the shared token creates a
// repository: an agent token is refused with a hint, and the operator's initial
// commit passes the access policy as the configured user.
func TestCLIRepoCreateOperatorOnly(t *testing.T) {
	ctx := featureflag.OutgoingCtxWithFeatureFlag(testhelper.Context(t), featureflag.GPGSigning, false)
	cfg := testcfg.Build(t)
	cfg.Auth.Token = "operator-secret"
	cfg.Graft.Standalone = true
	cfg.Graft.Agents = []config.GraftAgent{{Name: "alice", Token: "alice-secret"}}
	cfg.Graft.Policy = []config.GraftPolicyRule{
		{Actor: "alice", Refs: []string{"refs/heads/main"}, Allow: true},
		{Actor: "*", Allow: false},
	}
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll,
		testserver.WithGitLabClient(gitlab.NewStandaloneClient(cfg.Graft, testhelper.SharedLogger(t))))
	server := []string{"-address", addr, "-storage", cfg.Storages[0].Name}

	code, _, stderr := runCLI(t, append([]string{"repo", "create", "a.git", "-token", "alice-secret"}, server...)...)
	require.Equal(t, exitError, code)
	require.Contains(t, stderr, "PermissionDenied")
	require.Contains(t, stderr, "hint: creating a repository needs the shared (operator) token")
	require.NoDirExists(t, filepath.Join(cfg.Storages[0].Path, "a.git"))

	code, stdout, stderr := runCLI(t, append([]string{"repo", "create", "a.git", "--initial-commit", "-token", "operator-secret", "-user", "alice"}, server...)...)
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stdout, "created repository a.git")

	client, err := sdk.Dial(ctx, addr, sdk.WithStorage(cfg.Storages[0].Name), sdk.WithAgentToken("alice-secret"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	readme, err := client.Repo("a.git").ReadFile(ctx, "main", "README.md")
	require.NoError(t, err)
	require.Equal(t, "# a\n", string(readme))
}

// TestCLIRepoCreateAuthoritative checks that a repository created by graft repo
// create on a server whose storage core is authoritative is registered there,
// with its initial commit, and that the storage core's verification finds the
// local copy equal to the database. It runs serially because the CLI tests
// share the package-level backend factory.
func TestCLIRepoCreateAuthoritative(t *testing.T) {
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	testcfg.BuildGitalySSH(t, cfg)
	testcfg.BuildGitalyHooks(t, cfg)
	core := coretest.Open(t, coretest.Options{Mode: corestore.ModeAuthoritative})
	addr := testserver.RunGitalyServer(t, cfg, setup.RegisterAll, testserver.WithGraftStorage(core))
	target := []string{"-address", addr, "-storage", cfg.Storages[0].Name}

	code, stdout, stderr := runCLI(t, append([]string{"repo", "create", "core.git", "--initial-commit", "--branch", "feature/demo", "--json"}, target...)...)
	require.Equal(t, exitOK, code, stderr)
	var created repoCreated
	require.NoError(t, json.Unmarshal([]byte(stdout), &created))

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
}

// TestCLIRepoUsageErrorsDoNotContactServer checks the arguments of graft repo
// before any server call: the address points nowhere, so a server call would
// fail with a different error.
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
// help shows the synopsis.
func TestCLIRepoHelp(t *testing.T) {
	code, stdout, _ := runCLI(t, "help")
	require.Equal(t, exitOK, code)
	require.Contains(t, stdout, "  repo create REPO ")

	code, _, stderr := runCLI(t, "repo", "-h")
	require.Equal(t, exitOK, code, stderr)
	require.Contains(t, stderr, repoSynopsis)
	require.Contains(t, stderr, "shared (operator) token")

	code, _, stderr = runCLI(t, "repo")
	require.Equal(t, exitUsage, code, stderr)
	require.Contains(t, stderr, repoSynopsis)
}

func TestRepositoryName(t *testing.T) {
	require.Equal(t, "demo", repositoryName("demo.git"))
	require.Equal(t, "demo", repositoryName("group/demo.git"))
	require.Equal(t, "plain", repositoryName("plain"))
}
