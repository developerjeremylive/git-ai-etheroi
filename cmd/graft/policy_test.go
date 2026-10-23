package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/graft/workspace"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
)

// candidateFile writes a configuration whose [graft] section has the given policy and returns its
// path. The rest of the configuration is a test server's, as an operator's file has one.
func candidateFile(t *testing.T, policy []config.GraftPolicyRule) string {
	t.Helper()

	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true
	cfg.Graft.Policy = policy
	return testcfg.WriteTemporaryGitalyConfigFile(t, cfg)
}

// runPolicyCheck runs graft with args and returns its exit code, standard output and standard
// error.
func runPolicyCheck(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"policy", "check"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestPolicyCheckOffline(t *testing.T) {
	t.Parallel()

	rules := []config.GraftPolicyRule{
		{Actor: "alice", Refs: []string{"refs/heads/main"}, Allow: true},
		{Actor: "*", Allow: false},
	}

	t.Run("a valid policy is printed with its rules and hash", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := runPolicyCheck(candidateFile(t, rules))
		require.Equal(t, exitOK, code, stderr)
		require.Contains(t, stdout, "2 rules, sha256 "+policystore.Hash(rules))
		require.Contains(t, stdout, "  1. allow alice: refs/heads/main\n")
		require.Contains(t, stdout, "  2. deny *: every reference\n")
	})

	t.Run("JSON", func(t *testing.T) {
		t.Parallel()

		code, stdout, stderr := runPolicyCheck(candidateFile(t, rules), "--json")
		require.Equal(t, exitOK, code, stderr)
		var res policyCheckResult
		require.NoError(t, json.Unmarshal([]byte(stdout), &res))
		require.Equal(t, policystore.Hash(rules), res.SHA256)
		require.Equal(t, []policyRule{
			{Number: 1, Actor: "alice", Refs: []string{"refs/heads/main"}, Allow: true},
			{Number: 2, Actor: "*", Refs: []string{}, Allow: false},
		}, res.Rules)
		require.Empty(t, res.Repositories)
	})

	for desc, path := range map[string]func(t *testing.T) string{
		"an invalid glob": func(t *testing.T) string {
			return candidateFile(t, []config.GraftPolicyRule{{Actor: "*", Refs: []string{"refs/heads/["}, Allow: true}})
		},
		"an empty refs entry": func(t *testing.T) string {
			return candidateFile(t, []config.GraftPolicyRule{{Actor: "*", Refs: []string{""}, Allow: true}})
		},
		"a file that is no TOML": func(t *testing.T) string {
			path := filepath.Join(t.TempDir(), "config.toml")
			require.NoError(t, os.WriteFile(path, []byte("[graft\n"), mode.File))
			return path
		},
		"an unreadable file": func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "missing.toml")
		},
	} {
		t.Run(desc+" exits 2, as a reload rejects it", func(t *testing.T) {
			t.Parallel()

			file := path(t)
			code, stdout, stderr := runPolicyCheck(file)
			require.Equal(t, exitUsage, code)
			require.Empty(t, stdout)
			require.Contains(t, stderr, "graft policy: "+file+": ")
		})
	}

	t.Run("-repo needs the address", func(t *testing.T) {
		t.Parallel()

		code, _, stderr := runPolicyCheck(candidateFile(t, rules), "-repo", "r.git", "-address", "")
		require.Equal(t, exitUsage, code)
		require.Contains(t, stderr, "-address or GRAFT_ADDRESS")
	})
}

// TestPolicyCheckAgainstChangesets: alice lands on main under the policy of startPolicyServer. A
// candidate that allows her only her temporary branches would deny her updates of main, and only
// those.
func TestPolicyCheckAgainstChangesets(t *testing.T) {
	t.Parallel()

	env := startPolicyServer(t)
	ctx := env.ctx
	b, err := openSDKBackend(ctx, target{Address: env.addr, Token: "alice-secret", Storage: env.storage, Repo: env.relPath, User: "alice"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.Close() })

	dir := filepath.Join(t.TempDir(), "ws")
	ws, err := workspace.Clone(ctx, dir, b, workspace.CloneOptions{Address: env.addr, Storage: env.storage, Repo: env.relPath, Branch: "main"})
	require.NoError(t, err)
	for _, content := range []string{"two\n", "three\n"} {
		writeLocal(t, dir, "a.txt", content)
		res, err := ws.Commit(ctx, workspace.CommitRequest{Message: "change a", Author: testAuthor(), User: "alice", LandBranch: "main"})
		require.NoError(t, err)
		require.True(t, res.Landed)
	}

	all, err := b.Changesets(ctx, 0, 100)
	require.NoError(t, err)
	var mainSequences []uint64
	for _, cs := range all {
		if cs.Reference == "refs/heads/main" && cs.Actor == "alice" {
			mainSequences = append(mainSequences, cs.Sequence)
		}
	}
	require.Len(t, mainSequences, 2)

	candidate := candidateFile(t, []config.GraftPolicyRule{
		{Actor: "alice", Refs: []string{"refs/heads/graft/alice/**"}, Allow: true},
		{Actor: "*", Allow: false},
	})
	server := []string{"-address", env.addr, "-token", "alice-secret", "-storage", env.storage}

	t.Run("the updates the candidate would deny", func(t *testing.T) {
		code, stdout, stderr := runPolicyCheck(append([]string{candidate, "-repo", env.relPath, "--json"}, server...)...)
		require.Equal(t, exitError, code, "a candidate that would deny updates fails the check")
		require.Equal(t, "graft policy: the candidate policy would deny 1 of the updates checked\n", stderr)
		var res policyCheckResult
		require.NoError(t, json.Unmarshal([]byte(stdout), &res))
		require.Len(t, res.Repositories, 1)
		repo := res.Repositories[0]
		require.Equal(t, env.relPath, repo.Repository)
		require.Equal(t, len(all), repo.Checked)
		require.Equal(t, all[len(all)-1].Sequence, repo.LastSequence)
		require.Len(t, repo.Denied, 1, "only alice's updates of main: %+v", repo.Denied)
		require.Equal(t, "alice", repo.Denied[0].Actor)
		require.Equal(t, "refs/heads/main", repo.Denied[0].Reference)
		require.Equal(t, mainSequences, repo.Denied[0].Sequences)
		require.Contains(t, repo.Denied[0].Reason, "alice")
	})

	t.Run("the text output", func(t *testing.T) {
		code, stdout, stderr := runPolicyCheck(append([]string{candidate, "-repo", env.relPath}, server...)...)
		require.Equal(t, exitError, code, stderr)
		require.Contains(t, stdout, "1 updates the candidate would deny")
		require.Contains(t, stdout, "  alice refs/heads/main (2 changesets, last ")
	})

	t.Run("--limit reads the last changesets", func(t *testing.T) {
		code, stdout, stderr := runPolicyCheck(append([]string{candidate, "-repo", env.relPath, "--limit", "1", "--json"}, server...)...)
		// The check fails exactly when the last changeset is one of alice's updates of main.
		lastChangeset := all[len(all)-1]
		wantCode := exitOK
		if lastChangeset.Reference == "refs/heads/main" && lastChangeset.Actor == "alice" {
			wantCode = exitError
		}
		require.Equal(t, wantCode, code, stderr)
		var res policyCheckResult
		require.NoError(t, json.Unmarshal([]byte(stdout), &res))
		repo := res.Repositories[0]
		require.Equal(t, 1, repo.Checked)
		last := all[len(all)-1].Sequence
		require.Equal(t, last, repo.FirstSequence)
		require.Equal(t, last, repo.LastSequence)
	})

	t.Run("the policy in force denies nothing that happened", func(t *testing.T) {
		code, stdout, stderr := runPolicyCheck(append([]string{candidateFile(t, env.cfg.Graft.Policy), "-repo", env.relPath, "--json"}, server...)...)
		require.Equal(t, exitOK, code, stderr)
		var res policyCheckResult
		require.NoError(t, json.Unmarshal([]byte(stdout), &res))
		require.Empty(t, res.Repositories[0].Denied)
	})

	t.Run("a repository that cannot be read fails, naming it", func(t *testing.T) {
		code, _, stderr := runPolicyCheck(append([]string{candidate, "-repo", "missing.git"}, server...)...)
		require.Equal(t, exitError, code)
		require.Contains(t, stderr, "graft policy: repository missing.git: ")
	})
}
