package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Error strings present in the legacy Ruby implementation.
const (
	legacyErrPrefixInvalidSubmodulePath = "Invalid submodule path"
)

// UserUpdateSubmodule updates a submodule to point to a new commit.
func (s *Server) UserUpdateSubmodule(ctx context.Context, req *gitalypb.UserUpdateSubmoduleRequest) (*gitalypb.UserUpdateSubmoduleResponse, error) {
	if err := s.locator.ValidateRepository(ctx, req.GetRepository()); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, req.GetRepository())
	if err != nil {
		return nil, err
	}
	defer cleanup()

	objectHash, err := quarantineRepo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object hash: %w", err)
	}

	if err := validateUserUpdateSubmoduleRequest(s.locator, objectHash, req); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}

	branches, err := quarantineRepo.GetBranches(ctx)
	if err != nil {
		return nil, structerr.NewInternal("get branches: %w", err)
	}
	if len(branches) == 0 {
		return nil, structerr.NewFailedPrecondition("Repository is empty")
	}

	referenceName := git.NewReferenceNameFromBranchName(string(req.GetBranch()))

	var oldOID git.ObjectID
	// branchCommit is the branch as read for the compare-and-swap, which the new commit then builds
	// on; empty when the request gives the expected old object ID instead.
	var branchCommit git.ObjectID
	if expectedOldOID := req.GetExpectedOldOid(); expectedOldOID != "" {
		objectHash, err := quarantineRepo.ObjectHash(ctx)
		if err != nil {
			return nil, structerr.NewInternal("detecting object hash: %w", err)
		}

		oldOID, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("invalid expected old object ID: %w", err).WithMetadata("old_object_id", expectedOldOID)
		}

		oldOID, err = quarantineRepo.ResolveRevision(ctx, git.Revision(fmt.Sprintf("%s^{object}", oldOID)))
		if err != nil {
			return nil, structerr.NewInvalidArgument("cannot resolve expected old object ID: %w", err).
				WithMetadata("old_object_id", expectedOldOID)
		}
	} else {
		// By its exact name: a missing branch must not resolve through a tag or another reference
		// that git's DWIM rules would try after it.
		oldOID, err = quarantineRepo.ResolveReferenceExactly(ctx, referenceName)
		if err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				return nil, structerr.NewInvalidArgument("Cannot find branch")
			}
			return nil, structerr.NewInternal("resolving revision: %w", err)
		}
		branchCommit = oldOID
	}

	commitID, err := s.updateSubmodule(ctx, quarantineRepo, req, branchCommit)
	if err != nil {
		errStr := strings.TrimSpace(err.Error())

		if strings.Contains(errStr, legacyErrPrefixInvalidSubmodulePath) {
			s.logger.
				WithError(err).
				ErrorContext(ctx, "UserUpdateSubmodule: git2go subcommand failure")
			return nil, structerr.NewInvalidArgument(legacyErrPrefixInvalidSubmodulePath).WithDetail(
				&gitalypb.UserUpdateSubmoduleError{
					Error: &gitalypb.UserUpdateSubmoduleError_PathError{
						PathError: &gitalypb.PathError{
							Path:      req.GetSubmodule(),
							ErrorType: gitalypb.PathError_ERROR_TYPE_INVALID_PATH,
						},
					},
				},
			)
		}
		if strings.Contains(errStr, "is already at") {
			return nil, structerr.NewInvalidArgument("%s", errStr).WithDetail(
				&gitalypb.UserUpdateSubmoduleError{
					Error: &gitalypb.UserUpdateSubmoduleError_PathError{
						PathError: &gitalypb.PathError{
							Path:      req.GetSubmodule(),
							ErrorType: gitalypb.PathError_ERROR_TYPE_PATH_EXISTS,
						},
					},
				},
			)
		}

		return nil, structerr.NewInternal("submodule subcommand: %w", err)
	}

	commitOID, err := objectHash.FromHex(commitID)
	if err != nil {
		return nil, structerr.NewInvalidArgument("cannot parse commit ID: %w", err)
	}

	if err := s.updateReferenceWithHooks(
		ctx,
		req.GetRepository(),
		req.GetUser(),
		quarantineDir,
		referenceName,
		commitOID,
		oldOID,
	); err != nil {
		var customHookErr updateref.CustomHookError
		if errors.As(err, &customHookErr) {
			return nil, structerr.NewPermissionDenied("%s", customHookErr.Error()).WithDetail(
				&gitalypb.UserUpdateSubmoduleError{
					Error: &gitalypb.UserUpdateSubmoduleError_CustomHook{
						CustomHook: customHookErr.Proto(),
					},
				},
			)
		}

		var updateRefError updateref.Error
		if errors.As(err, &updateRefError) {
			message := updateRefError.Error()
			return nil, structerr.NewFailedPrecondition("%s", message).WithDetail(
				&gitalypb.UserUpdateSubmoduleError{
					Error: &gitalypb.UserUpdateSubmoduleError_ReferenceUpdate{
						ReferenceUpdate: updateRefError.Proto(),
					},
				},
			)
		}

		return nil, structerr.NewInternal("updating ref with hooks: %w", err)
	}

	return &gitalypb.UserUpdateSubmoduleResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      commitID,
			BranchCreated: false,
			RepoCreated:   false,
		},
	}, nil
}

func validateUserUpdateSubmoduleRequest(locator storage.Locator, objectHash git.ObjectHash, req *gitalypb.UserUpdateSubmoduleRequest) error {
	if req.GetUser() == nil {
		return errors.New("empty User")
	}

	if req.GetCommitSha() == "" {
		return errors.New("empty CommitSha")
	}

	if err := objectHash.ValidateHex(req.GetCommitSha()); err != nil {
		return errors.New("invalid CommitSha")
	}

	if len(req.GetBranch()) == 0 {
		return errors.New("empty Branch")
	}

	if len(req.GetSubmodule()) == 0 {
		return errors.New("empty Submodule")
	}

	if len(req.GetCommitMessage()) == 0 {
		return errors.New("empty CommitMessage")
	}

	return nil
}

// legacyGit2GoSubmoduleAlreadyAtShaErr is used to maintain backwards
// compatibility with the git2go error.
type legacyGit2GoSubmoduleAlreadyAtShaError struct {
	submodulePath string
	commitSha     string
}

func (l *legacyGit2GoSubmoduleAlreadyAtShaError) Error() string {
	return fmt.Sprintf("The submodule %s is already at %s", l.submodulePath, l.commitSha)
}

// updateSubmodule writes the commit that updates the submodule on top of the branch. branchCommit is
// the branch as the caller read it for its compare-and-swap, so that the commit builds on the very
// value the update then expects; when it is empty (the request gives the expected old object ID),
// the branch is read here.
func (s *Server) updateSubmodule(ctx context.Context, quarantineRepo *localrepo.Repo, req *gitalypb.UserUpdateSubmoduleRequest, branchCommit git.ObjectID) (string, error) {
	var treeID git.ObjectID

	// The tree and the parent of the new commit are those of the branch, read by its exact name:
	// git's DWIM rules would resolve the branch name to a tag of the same name before the branch,
	// and a missing branch through a tag or another reference.
	currentBranchCommit := branchCommit
	if currentBranchCommit == "" {
		var err error
		currentBranchCommit, err = quarantineRepo.ResolveReferenceExactly(ctx, git.NewReferenceNameFromBranchName(string(req.GetBranch())))
		if err != nil {
			if errors.Is(err, git.ErrReferenceNotFound) {
				return "", fmt.Errorf("submodule: %s", legacyErrPrefixInvalidSubmodulePath)
			}

			return "", fmt.Errorf("resolving submodule branch: %w", err)
		}
	}

	fullTree, err := quarantineRepo.ReadTree(ctx, currentBranchCommit.Revision(), localrepo.WithRecursive())
	if err != nil {
		if errors.Is(err, git.ErrReferenceNotFound) {
			return "", fmt.Errorf("submodule: %s", legacyErrPrefixInvalidSubmodulePath)
		}

		return "", fmt.Errorf("error reading tree: %w", err)
	}

	if err := fullTree.Modify(
		string(req.GetSubmodule()),
		func(t *localrepo.TreeEntry) error {
			replaceWith := git.ObjectID(req.GetCommitSha())

			if t.Type != localrepo.Submodule {
				return fmt.Errorf("submodule: %s", legacyErrPrefixInvalidSubmodulePath)
			}

			if replaceWith == t.OID {
				return &legacyGit2GoSubmoduleAlreadyAtShaError{
					submodulePath: string(req.GetSubmodule()),
					commitSha:     string(replaceWith),
				}
			}

			t.OID = replaceWith

			return nil
		},
	); err != nil {
		if errors.Is(err, localrepo.ErrEntryNotFound) {
			return "", fmt.Errorf("submodule: %s", legacyErrPrefixInvalidSubmodulePath)
		}

		if errors.Is(err, localrepo.ErrPathTraversal) {
			return "", fmt.Errorf("submodule: %s", legacyErrPrefixInvalidSubmodulePath)
		}

		var git2GoErr *legacyGit2GoSubmoduleAlreadyAtShaError
		if errors.As(err, &git2GoErr) {
			return "", err
		}

		return "", fmt.Errorf("modifying tree: %w", err)
	}

	if err := fullTree.Write(ctx, quarantineRepo); err != nil {
		return "", fmt.Errorf("writing tree: %w", err)
	}

	treeID = fullTree.OID

	authorSignature, err := git.SignatureFromRequest(req)
	if err != nil {
		return "", structerr.NewInvalidArgument("%w", err)
	}

	newCommitID, err := quarantineRepo.WriteCommit(ctx, localrepo.WriteCommitConfig{
		Parents:        []git.ObjectID{currentBranchCommit},
		AuthorDate:     authorSignature.When,
		AuthorName:     authorSignature.Name,
		AuthorEmail:    authorSignature.Email,
		CommitterName:  authorSignature.Name,
		CommitterEmail: authorSignature.Email,
		CommitterDate:  authorSignature.When,
		Message:        string(req.GetCommitMessage()),
		TreeID:         treeID,
	})
	if err != nil {
		return "", fmt.Errorf("creating commit %w", err)
	}

	return string(newCommitID), nil
}
