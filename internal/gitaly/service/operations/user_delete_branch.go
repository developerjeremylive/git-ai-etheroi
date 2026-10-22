package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/hook"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitlab/client"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func validateUserDeleteBranchRequest(ctx context.Context, locator storage.Locator, in *gitalypb.UserDeleteBranchRequest) error {
	if err := locator.ValidateRepository(ctx, in.GetRepository()); err != nil {
		return err
	}
	if len(in.GetBranchName()) == 0 {
		return errors.New("bad request: empty branch name")
	}
	if in.GetUser() == nil {
		return errors.New("bad request: empty user")
	}
	return nil
}

// UserDeleteBranch force-deletes a single branch in the context of a specific user. It executes
// hooks and contacts Rails to verify that the user is indeed allowed to delete that branch.
func (s *Server) UserDeleteBranch(ctx context.Context, req *gitalypb.UserDeleteBranchRequest) (*gitalypb.UserDeleteBranchResponse, error) {
	if err := validateUserDeleteBranchRequest(ctx, s.locator, req); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	referenceName := git.NewReferenceNameFromBranchName(string(req.GetBranchName()))

	repo := s.localRepoFactory.Build(req.GetRepository())
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return nil, fmt.Errorf("detecting object format: %w", err)
	}

	var referenceValue git.ObjectID

	if expectedOldOID := req.GetExpectedOldOid(); expectedOldOID != "" {
		referenceValue, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return nil, structerr.NewInvalidArgument("invalid expected old object ID: %w", err).WithMetadata("old_object_id", expectedOldOID)
		}
		referenceValue, err = repo.ResolveRevision(
			ctx, git.Revision(fmt.Sprintf("%s^{object}", referenceValue)),
		)
		if err != nil {
			return nil, structerr.NewInvalidArgument("cannot resolve expected old object ID: %w", err).
				WithMetadata("old_object_id", expectedOldOID)
		}
	} else {
		// By its exact name: a missing branch must not resolve through a tag or another reference
		// that git's DWIM rules would try after it.
		referenceValue, err = repo.ResolveReferenceExactly(ctx, referenceName)
		if err != nil {
			return nil, structerr.NewFailedPrecondition("branch not found: %q", req.GetBranchName())
		}
	}

	if err := s.updateReferenceWithHooks(ctx, req.GetRepository(), req.GetUser(), nil, referenceName, objectHash.ZeroOID, referenceValue); err != nil {
		var notAllowedError hook.NotAllowedError
		var customHookErr updateref.CustomHookError
		var updateRefError updateref.Error

		// Allow the statushandler middleware to sort this out.
		if errors.Is(err, client.RailsRateLimitedError{}) {
			return nil, err
		}

		if errors.As(err, &notAllowedError) {
			return nil, structerr.NewPermissionDenied("deletion denied by access checks: %w", err).WithDetail(
				&gitalypb.UserDeleteBranchError{
					Error: &gitalypb.UserDeleteBranchError_AccessCheck{
						AccessCheck: &gitalypb.AccessCheckError{
							ErrorMessage: notAllowedError.Message,
							UserId:       notAllowedError.UserID,
							Protocol:     notAllowedError.Protocol,
							Changes:      notAllowedError.Changes,
						},
					},
				},
			)
		} else if errors.As(err, &customHookErr) {
			return nil, structerr.NewPermissionDenied("deletion denied by custom hooks: %w", err).WithDetail(
				&gitalypb.UserDeleteBranchError{
					Error: &gitalypb.UserDeleteBranchError_CustomHook{
						CustomHook: customHookErr.Proto(),
					},
				},
			)
		} else if errors.As(err, &updateRefError) {
			return nil, structerr.NewFailedPrecondition("reference update failed: %w", updateRefError).WithDetail(
				&gitalypb.UserDeleteBranchError{
					Error: &gitalypb.UserDeleteBranchError_ReferenceUpdate{
						ReferenceUpdate: &gitalypb.ReferenceUpdateError{
							ReferenceName: []byte(updateRefError.Reference.String()),
							OldOid:        updateRefError.OldOID.String(),
							NewOid:        updateRefError.NewOID.String(),
						},
					},
				},
			)
		}

		return nil, structerr.NewInternal("deleting reference: %w", err)
	}

	return &gitalypb.UserDeleteBranchResponse{}, nil
}
