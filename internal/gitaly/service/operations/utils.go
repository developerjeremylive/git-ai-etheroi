package operations

import (
	"context"
	"errors"
	"fmt"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

type cherryPickOrRevertRequest interface {
	GetRepository() *gitalypb.Repository
	GetUser() *gitalypb.User
	GetCommit() *gitalypb.GitCommit
	GetBranchName() []byte
	GetMessage() []byte
}

func validateCherryPickOrRevertRequest(ctx context.Context, locator storage.Locator, req cherryPickOrRevertRequest) error {
	if err := locator.ValidateRepository(ctx, req.GetRepository()); err != nil {
		return err
	}

	if req.GetUser() == nil {
		return errors.New("empty User")
	}

	if req.GetCommit() == nil {
		return errors.New("empty Commit")
	}

	if len(req.GetBranchName()) == 0 {
		return errors.New("empty BranchName")
	}

	if len(req.GetMessage()) == 0 {
		return errors.New("empty Message")
	}

	return nil
}

// resolveBranchCommitExactly returns the commit of the branch reference, read by its exact name and
// peeled as `<branch>^{commit}` is (localrepo.ResolveBranchesExactly), or git.ErrReferenceNotFound
// when the branch does not exist or does not peel to a commit. git resolves `<branch>^{commit}`
// with its DWIM rules: when refs/heads/<x> is missing, refs/refs/heads/<x>, refs/tags/refs/heads/<x>,
// refs/heads/refs/heads/<x> or refs/remotes/refs/heads/<x> answers for it.
func (s *Server) resolveBranchCommitExactly(ctx context.Context, repo *localrepo.Repo, reference git.ReferenceName) (git.ObjectID, error) {
	reader, release, err := s.catfileCache.ObjectInfoReader(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("creating object info reader: %w", err)
	}
	defer release()

	branches, err := repo.ResolveBranchesExactly(ctx, reader, reference)
	if err != nil {
		return "", err
	}
	commit, ok := branches[reference]
	if !ok {
		return "", git.ErrReferenceNotFound
	}

	return commit, nil
}

// resolveRevision is a helper function to call ResolveRevision on the repo if the existing commit is not equal to the ZeroOID.
func resolveRevision(ctx context.Context, repo *localrepo.Repo, commit git.ObjectID) (git.ObjectID, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return commit, err
	}

	if commit == objectHash.ZeroOID {
		return commit, nil
	}

	return repo.ResolveRevision(
		ctx,
		git.Revision(fmt.Sprintf("%s^{object}", commit)),
	)
}
