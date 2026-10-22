package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/git/remoterepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

//nolint:revive // This is unintentionally missing documentation.
func (s *Server) UserRevert(ctx context.Context, req *gitalypb.UserRevertRequest) (*gitalypb.UserRevertResponse, error) {
	if err := validateCherryPickOrRevertRequest(ctx, s.locator, req); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	startRevision, err := s.fetchStartRevision(ctx, quarantineRepo, req)
	if err != nil {
		return nil, err
	}

	repoHadBranches, err := quarantineRepo.HasBranches(ctx)
	if err != nil {
		return nil, structerr.NewInternal("has branches: %w", err)
	}

	committerSignature, err := git.SignatureFromRequest(req)
	if err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	var newrev git.ObjectID

	oursCommit, err := quarantineRepo.ReadCommit(ctx, startRevision.Revision())
	if err != nil {
		if errors.Is(err, localrepo.ErrObjectNotFound) {
			return nil, structerr.NewNotFound("ours commit lookup: commit not found").
				WithMetadata("revision", startRevision)
		}

		return nil, structerr.NewInternal("read commit: %w", err)
	}
	revertCommit, err := quarantineRepo.ReadCommit(ctx, git.Revision(req.GetCommit().GetId()))
	if err != nil {
		if errors.Is(err, localrepo.ErrObjectNotFound) {
			return nil, structerr.NewNotFound("revert commit lookup: commit not found").
				WithMetadata("revision", req.GetCommit().GetId())
		}

		return nil, structerr.NewInternal("read commit: %w", err)
	}

	var theirs git.ObjectID
	if len(revertCommit.GetParentIds()) > 0 {
		// Use the first parent as `theirs` to implement mainline = 1.
		theirs = git.ObjectID(revertCommit.GetParentIds()[0])
	} else {
		// It is a root commit, use empty tree oid as `theirs`.
		hash, err := quarantineRepo.ObjectHash(ctx)
		if err != nil {
			return nil, structerr.NewInternal("detecting object hash: %w", err)
		}
		theirs = hash.EmptyTreeOID
	}

	// We "merge" in the "changes" from parent to child, which would apply the "opposite"
	// patch to "ours", thus reverting the commit.
	treeOID, err := quarantineRepo.MergeTree(
		ctx,
		oursCommit.GetId(),
		theirs.String(),
		localrepo.WithMergeBase(git.Revision(revertCommit.GetId())),
		localrepo.WithConflictingFileNamesOnly(),
	)
	if err != nil {
		var conflictErr *localrepo.MergeTreeConflictError
		if errors.As(err, &conflictErr) {
			return nil, structerr.NewFailedPrecondition("revert: there are conflicting files").WithDetail(
				&gitalypb.UserRevertError{
					Error: &gitalypb.UserRevertError_MergeConflict{
						MergeConflict: &gitalypb.MergeConflictError{
							ConflictingFiles: conflictingFileNames(conflictErr),
						},
					},
				})
		}

		return nil, structerr.NewInternal("merge-tree: %w", err)
	}

	if oursCommit.GetTreeId() == treeOID.String() {
		return nil, structerr.NewFailedPrecondition("revert: could not apply because the result was empty").WithDetail(
			&gitalypb.UserRevertError{
				Error: &gitalypb.UserRevertError_ChangesAlreadyApplied{
					ChangesAlreadyApplied: &gitalypb.ChangesAlreadyAppliedError{},
				},
			})
	}

	newrev, err = quarantineRepo.WriteCommit(
		ctx,
		localrepo.WriteCommitConfig{
			TreeID:         treeOID,
			Message:        string(req.GetMessage()),
			Parents:        []git.ObjectID{startRevision},
			AuthorName:     committerSignature.Name,
			AuthorEmail:    committerSignature.Email,
			AuthorDate:     committerSignature.When,
			CommitterName:  committerSignature.Name,
			CommitterEmail: committerSignature.Email,
			CommitterDate:  committerSignature.When,
			GitConfig:      s.gitConfig,
			Sign:           req.GetSign(),
		},
	)
	if err != nil {
		return nil, structerr.NewInternal("write commit: %w", err)
	}

	referenceName := git.NewReferenceNameFromBranchName(string(req.GetBranchName()))
	branchCreated := false
	var oldrev git.ObjectID

	objectHash, err := quarantineRepo.ObjectHash(ctx)
	if err != nil {
		return nil, structerr.NewInternal("detecting object hash: %w", err)
	}

	if expectedOldOID := req.GetExpectedOldOid(); expectedOldOID != "" {
		oldrev, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("invalid expected old object ID: %w", err).WithMetadata("old_object_id", expectedOldOID)
		}

		oldrev, err = quarantineRepo.ResolveRevision(
			ctx, git.Revision(fmt.Sprintf("%s^{object}", oldrev)),
		)
		if err != nil {
			return nil, structerr.NewInvalidArgument("cannot resolve expected old object ID: %w", err).
				WithMetadata("old_object_id", expectedOldOID)
		}
	} else {
		oldrev, err = s.resolveBranchCommitExactly(ctx, quarantineRepo, referenceName)
		if errors.Is(err, git.ErrReferenceNotFound) {
			branchCreated = true
			oldrev = objectHash.ZeroOID
		} else if err != nil {
			return nil, structerr.NewInvalidArgument("resolve ref: %w", err)
		}
	}

	if req.GetDryRun() {
		newrev = startRevision
	}

	if !branchCreated {
		ancestor, err := quarantineRepo.IsAncestor(ctx, oldrev.Revision(), newrev.Revision())
		if err != nil {
			return nil, structerr.NewInternal("checking for ancestry: %w", err)
		}
		if !ancestor {
			return nil, structerr.NewFailedPrecondition("revert: branch diverged").WithDetail(
				&gitalypb.UserRevertError{
					Error: &gitalypb.UserRevertError_NotAncestor{
						NotAncestor: &gitalypb.NotAncestorError{
							ParentRevision: []byte(oldrev.Revision()),
							ChildRevision:  []byte(newrev.Revision()),
						},
					},
				})
		}
	}

	if err := s.updateReferenceWithHooks(ctx, req.GetRepository(), req.GetUser(), quarantineDir, referenceName, newrev, oldrev); err != nil {
		var customHookErr updateref.CustomHookError
		if errors.As(err, &customHookErr) {
			return nil, structerr.NewPermissionDenied("revert: custom hook error").WithDetail(
				&gitalypb.UserRevertError{
					Error: &gitalypb.UserRevertError_CustomHook{
						CustomHook: customHookErr.Proto(),
					},
				})
		}

		return nil, fmt.Errorf("update reference with hooks: %w", err)
	}

	return &gitalypb.UserRevertResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      newrev.String(),
			BranchCreated: branchCreated,
			RepoCreated:   !repoHadBranches,
		},
	}, nil
}

// resolveStartBranch resolves the start branch name of a request, a revision, to its commit.
// objectHash is the object format of the repository the start branch is read from; exact reads a
// reference there by its exact name, peeled to its commit, and resolve is git's resolution of a
// revision there.
//
// git resolves a bare name x with its DWIM rules, to refs/tags/x before refs/heads/x, so a tag named
// like the start branch would answer for the branch. The revision is split into the name it starts
// with and its suffix (~1, ^{}, @{1}, :path), and the name is resolved by these rules:
//   - a full object ID is the object, as git takes it, even when a reference is named like it;
//   - a full reference name (refs/...) is read exactly as given, with no fallback: a missing
//     refs/heads/x does not resolve through refs/refs/heads/x or another decoy;
//   - any other name x is read as the branch refs/heads/x first. When there is no such branch, the
//     revision is resolved as before this rule, by design: callers pass a SHA or a tag as a start
//     point, and a missing branch keeps that DWIM resolution for compatibility.
//
// The suffix then applies to the reference that was read, by its full name: that is the first name
// git's DWIM rules try, so a reference that exists is read exactly.
func resolveStartBranch(
	name string,
	objectHash git.ObjectHash,
	exact func(git.ReferenceName) (git.ObjectID, error),
	resolve func(git.Revision) (git.ObjectID, error),
) (git.ObjectID, error) {
	base, suffix := splitRevisionSuffix(name)
	if objectHash.ValidateHex(base) == nil {
		return resolve(git.Revision(name + "^{commit}"))
	}

	reference := git.NewReferenceNameFromBranchName(base)
	if strings.HasPrefix(base, "refs/") {
		reference = git.ReferenceName(base)
	}

	commit, err := exact(reference)
	switch {
	case err == nil:
		if suffix == "" {
			return commit, nil
		}
		return resolve(git.Revision(reference.String() + suffix + "^{commit}"))
	case !errors.Is(err, git.ErrReferenceNotFound) || strings.HasPrefix(base, "refs/"):
		return "", err
	default:
		return resolve(git.Revision(name + "^{commit}"))
	}
}

// splitRevisionSuffix splits a revision into the name it starts with and the suffix that applies to
// it (~<n>, ^<n>, ^{<type>}, @{<n>}, :<path>). A reference name never holds ~, ^, : or @{ (git
// check-ref-format), so the name ends at the first of them.
func splitRevisionSuffix(revision string) (string, string) {
	end := len(revision)
	for _, marker := range []string{"~", "^", ":", "@{"} {
		if i := strings.Index(revision, marker); i >= 0 && i < end {
			end = i
		}
	}
	return revision[:end], revision[end:]
}

type requestFetchingStartRevision interface {
	GetBranchName() []byte
	GetStartRepository() *gitalypb.Repository
	GetStartBranchName() []byte
}

func (s *Server) fetchStartRevision(
	ctx context.Context,
	localRepo *localrepo.Repo,
	req requestFetchingStartRevision,
) (git.ObjectID, error) {
	startBranchName := req.GetStartBranchName()
	if len(startBranchName) == 0 {
		startBranchName = req.GetBranchName()
	}

	// The start branch is read in the start repository, or in the local one without it.
	objectHashOf := localRepo.ObjectHash
	exact := func(name git.ReferenceName) (git.ObjectID, error) {
		return s.resolveBranchCommitExactly(ctx, localRepo, name)
	}
	resolve := func(revision git.Revision) (git.ObjectID, error) {
		return localRepo.ResolveRevision(ctx, revision)
	}
	if startRepository := req.GetStartRepository(); startRepository != nil {
		remoteRepo, err := remoterepo.New(ctx, startRepository, s.conns)
		if err != nil {
			return "", structerr.NewInternal("%w", err)
		}
		objectHashOf = remoteRepo.ObjectHash
		exact = func(name git.ReferenceName) (git.ObjectID, error) {
			return remoteRepo.ResolveReference(ctx, name)
		}
		resolve = func(revision git.Revision) (git.ObjectID, error) {
			return remoteRepo.ResolveRevision(ctx, revision)
		}
	}
	objectHash, err := objectHashOf(ctx)
	if err != nil {
		return "", structerr.NewInternal("detecting object hash: %w", err)
	}

	startRevision, err := resolveStartBranch(string(startBranchName), objectHash, exact, resolve)
	if err != nil {
		return "", structerr.NewInvalidArgument("resolve start ref: %w", err)
	}

	if req.GetStartRepository() == nil {
		return startRevision, nil
	}

	_, err = localRepo.ResolveRevision(ctx, startRevision.Revision()+"^{commit}")
	if errors.Is(err, git.ErrReferenceNotFound) {
		if err := localRepo.FetchInternal(
			ctx,
			req.GetStartRepository(),
			[]string{startRevision.String()},
			localrepo.FetchOpts{Tags: localrepo.FetchOptsTagsNone},
		); err != nil {
			return "", structerr.NewInternal("fetch start: %w", err)
		}
	} else if err != nil {
		return "", structerr.NewInvalidArgument("resolve start: %w", err)
	}

	return startRevision, nil
}
