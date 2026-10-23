package ref

import (
	"context"

	"github.com/feder-cr/git-ai/internal/command"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// FindDefaultBranchName returns the default branch name for the given repository
func (s *server) FindDefaultBranchName(ctx context.Context, in *gitalypb.FindDefaultBranchNameRequest) (*gitalypb.FindDefaultBranchNameResponse, error) {
	repository := in.GetRepository()
	if err := s.locator.ValidateRepository(ctx, repository); err != nil {
		return nil, structerr.NewInvalidArgument("%w", err)
	}
	repo := s.localRepoFactory.Build(repository)

	if in.GetHeadOnly() {
		head, err := repo.HeadReference(ctx)
		if err != nil {
			if headIsDetached(ctx, repo) {
				return nil, structerr.NewFailedPrecondition("HEAD is detached: it does not point to a branch")
			}
			return nil, structerr.NewInternal("head reference: %w", err)
		}
		return &gitalypb.FindDefaultBranchNameResponse{Name: []byte(head)}, nil
	}

	defaultBranch, err := repo.GetDefaultBranch(ctx)
	if err != nil {
		return nil, structerr.NewInternal("get default branch: %w", err)
	}
	return &gitalypb.FindDefaultBranchNameResponse{Name: []byte(defaultBranch)}, nil
}

// headIsDetached reports whether HEAD of repo is not a symbolic reference: `git symbolic-ref -q HEAD`
// exits with 1, silently, exactly in that case.
func headIsDetached(ctx context.Context, repo *localrepo.Repo) bool {
	err := repo.ExecAndWait(ctx, gitcmd.Command{
		Name:  "symbolic-ref",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "-q"}},
		Args:  []string{"HEAD"},
	}, gitcmd.WithDisabledHooks())
	code, ok := command.ExitStatus(err)
	return ok && code == 1
}
