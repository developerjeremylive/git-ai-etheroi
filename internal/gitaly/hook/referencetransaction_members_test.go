package hook

import (
	"fmt"
	"strings"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/gittest"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/transaction"
	"github.com/feder-cr/git-ai/internal/gitlab"
	"github.com/feder-cr/git-ai/internal/graft/changelog"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/grpc/backchannel"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/internal/testhelper/testcfg"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newMembersTestManager returns the hook manager that the tests of batch members
// call the reference-transaction hook of.
func newMembersTestManager(t *testing.T, cfg config.Cfg) *GitLabHookManager {
	t.Helper()

	return NewManager(
		cfg,
		config.NewLocator(cfg),
		testhelper.SharedLogger(t),
		gittest.NewCommandFactory(t, cfg),
		nil,
		transaction.NewManager(cfg, testhelper.SharedLogger(t), backchannel.NewRegistry()),
		gitlab.NewMockClient(t, gitlab.MockAllowed, gitlab.MockPreReceive, gitlab.MockPostReceive),
		NewPostReceiveRegistry(),
	)
}

// TestReferenceTransactionHook_recordsBatchMembers checks that a committed batched
// land records its members in the changeset log, with the operation and the actor
// of the batch, and that a resolved path that is not valid UTF-8 is recorded
// byte for byte.
func TestReferenceTransactionHook_recordsBatchMembers(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	cfg := testcfg.Build(t)
	cfg.Graft.Standalone = true

	repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
		SkipCreationViaService: true,
	})

	members := []changelog.Member{
		{OriginalCommit: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Actor: "agent-1", Rewritten: true, ResolvedPaths: []string{"dir/\xff\xfe.txt"}},
		{OriginalCommit: strings.Repeat("c", 40), Commit: strings.Repeat("c", 40), Actor: "agent-2"},
	}

	batchCtx := identity.NewContext(ctx, identity.Identity{Name: "agent-1", Source: identity.SourceAgent})
	batchCtx = changelog.WithBatch(changelog.WithOperation(batchCtx, changelog.OperationLandBatch), members)

	env, err := gitcmd.NewHooksPayload(
		batchCtx,
		cfg,
		repo,
		gittest.DefaultObjectHash,
		nil,
		nil,
		gitcmd.ReferenceTransactionHook,
		nil,
	).Env()
	require.NoError(t, err)

	repoPath, err := config.NewLocator(cfg).GetRepoPath(ctx, repo)
	require.NoError(t, err)

	changes := fmt.Sprintf("%s %s refs/heads/main\n", gittest.DefaultObjectHash.EmptyTreeOID, strings.Repeat("b", 40))

	err = newMembersTestManager(t, cfg).ReferenceTransactionHook(ctx, ReferenceTransactionCommitted, []string{env}, strings.NewReader(changes))
	require.NoError(t, err)

	entries, err := changelog.List(repoPath, 0, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, changelog.OperationLandBatch, entries[0].Operation)
	require.Equal(t, "agent-1", entries[0].Actor)
	require.Equal(t, "refs/heads/main", entries[0].Reference)
	require.False(t, entries[0].MembersTruncated)
	require.Equal(t, members, entries[0].Members)
}

// TestReferenceTransactionHook_batchPolicySkip checks when the standalone policy is
// not evaluated again for the combined update of a batch. Only a batched land that
// carries members skips it, because its members were checked in process. Every
// other update of an agent is still evaluated, and so is a batch without members.
func TestReferenceTransactionHook_batchPolicySkip(t *testing.T) {
	t.Parallel()

	policy := []config.GraftPolicyRule{
		{Refs: []string{"refs/heads/main"}, Allow: false},
		{Refs: []string{"refs/heads/**"}, Allow: true},
	}

	member := []changelog.Member{{OriginalCommit: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40), Actor: "builder"}}

	oldOID := gittest.DefaultObjectHash.EmptyTreeOID
	changes := fmt.Sprintf("%s %s refs/heads/main\n", oldOID, strings.Repeat("b", 40))

	for _, tc := range []struct {
		desc      string
		operation string
		members   []changelog.Member
		denied    bool
	}{
		{
			desc:      "batched land with members is not checked again",
			operation: changelog.OperationLandBatch,
			members:   member,
		},
		{
			desc:      "batched land without members is checked",
			operation: changelog.OperationLandBatch,
			denied:    true,
		},
		{
			desc:      "land with members is checked",
			operation: "land",
			members:   member,
			denied:    true,
		},
		{
			desc:      "plain agent update is checked",
			operation: "ff_branch",
			denied:    true,
		},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			ctx := testhelper.Context(t)
			cfg := testcfg.Build(t)
			cfg.Graft.Standalone = true
			cfg.Graft.Policy = policy
			cfg.Graft.Changelog.Disabled = true

			repo, _ := gittest.CreateRepository(t, ctx, cfg, gittest.CreateRepositoryConfig{
				SkipCreationViaService: true,
			})

			payloadCtx := identity.NewContext(ctx, identity.Identity{Name: "builder", Source: identity.SourceAgent})
			payloadCtx = changelog.WithOperation(payloadCtx, tc.operation)
			payloadCtx = changelog.WithBatch(payloadCtx, tc.members)
			// A batch carries the generation of the policy it admitted its members under: the
			// startup policy here, as in the server.
			payloadCtx = policystore.WithAdmissionGeneration(payloadCtx, 1)

			env, err := gitcmd.NewHooksPayload(
				payloadCtx,
				cfg,
				repo,
				gittest.DefaultObjectHash,
				nil,
				nil,
				gitcmd.ReferenceTransactionHook,
				nil,
			).Env()
			require.NoError(t, err)

			err = newMembersTestManager(t, cfg).ReferenceTransactionHook(ctx, ReferenceTransactionPrepared, []string{env}, strings.NewReader(changes))
			if !tc.denied {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			require.Equal(t, codes.PermissionDenied, status.Code(err), "got %v", err)
		})
	}
}
