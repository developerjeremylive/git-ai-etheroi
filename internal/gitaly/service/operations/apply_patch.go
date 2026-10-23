package operations

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/git/localrepo"
	"github.com/feder-cr/git-ai/internal/gitaly/hook/updateref"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/helper/text"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/feder-cr/git-ai/streamio"
)

var errNoDefaultBranch = errors.New("no default branch")

// UserApplyPatch applies patches to a given branch.
func (s *Server) UserApplyPatch(stream gitalypb.OperationService_UserApplyPatchServer) error {
	firstRequest, err := stream.Recv()
	if err != nil {
		return err
	}

	header := firstRequest.GetHeader()
	if header == nil {
		return structerr.NewInvalidArgument("empty UserApplyPatch_Header")
	}

	if err := validateUserApplyPatchHeader(stream.Context(), s.locator, header); err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	if err := s.userApplyPatch(stream.Context(), header, stream); err != nil {
		var customHookErr updateref.CustomHookError
		if errors.As(err, &customHookErr) {
			return structerr.NewPermissionDenied("denied by custom hooks").WithDetail(
				&gitalypb.UserApplyPatchError{
					Error: &gitalypb.UserApplyPatchError_CustomHook{
						CustomHook: customHookErr.Proto(),
					},
				},
			)
		}

		return structerr.NewInternal("%w", err)
	}

	return nil
}

func (s *Server) userApplyPatch(ctx context.Context, header *gitalypb.UserApplyPatchRequest_Header, stream gitalypb.OperationService_UserApplyPatchServer) (returnedErr error) {
	branchCreated := false
	targetBranch := git.NewReferenceNameFromBranchName(string(header.GetTargetBranch()))

	repo := s.localRepoFactory.Build(header.GetRepository())

	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return fmt.Errorf("detecting object hash: %w", err)
	}

	// The branches are read by their exact names: a missing branch must not resolve through a tag
	// or another reference that git's DWIM rules would try after it.
	parentCommitID, err := s.resolveBranchCommitExactly(ctx, repo, targetBranch)
	if err != nil {
		if !errors.Is(err, git.ErrReferenceNotFound) {
			return fmt.Errorf("resolve target branch: %w", err)
		}

		defaultBranch, err := repo.GetDefaultBranch(ctx)
		if err != nil {
			return fmt.Errorf("default branch name: %w", err)
		}
		// A repository without a branch has no default branch.
		if defaultBranch == "" {
			return errNoDefaultBranch
		}

		parentCommitID, err = s.resolveBranchCommitExactly(ctx, repo, defaultBranch)
		if errors.Is(err, git.ErrReferenceNotFound) {
			return errNoDefaultBranch
		} else if err != nil {
			return fmt.Errorf("resolve default branch commit: %w", err)
		}

		branchCreated = true
	}

	committerSignature, err := git.SignatureFromRequest(header)
	if err != nil {
		return structerr.NewInvalidArgument("%w", err)
	}

	// The patches are written into a quarantine, which the reference update migrates: a loose object
	// written straight into the repository would race housekeeping's removal of empty loose object
	// directories, which the migration survives.
	quarantineDir, quarantineRepo, cleanup, err := s.quarantinedRepo(ctx, header.GetRepository())
	if err != nil {
		return err
	}
	defer cleanup()

	patchedCommit, err := s.applyPatchesWithIndex(ctx, quarantineRepo, parentCommitID, committerSignature, stream)
	if err != nil {
		return err
	}

	currentCommit := parentCommitID
	if branchCreated {
		currentCommit = objectHash.ZeroOID
	}

	// If the client provides an expected old object ID, we should use that to prevent any race
	// conditions wherein the ref was concurrently updated by different processes.
	if expectedOldOID := header.GetExpectedOldOid(); expectedOldOID != "" {
		objectHash, err := repo.ObjectHash(ctx)
		if err != nil {
			return fmt.Errorf("detecting object hash: %w", err)
		}

		currentCommit, err = objectHash.FromHex(expectedOldOID)
		if err != nil {
			return fmt.Errorf("expected old object id not expected SHA format: %w", err)
		}

		currentCommit, err = resolveRevision(ctx, repo, currentCommit)
		if err != nil {
			return fmt.Errorf("expected old object cannot be resolved: %w", err)
		}
	}

	if err := s.updateReferenceWithHooks(ctx, header.GetRepository(), header.GetUser(), quarantineDir, targetBranch, patchedCommit, currentCommit); err != nil {
		return fmt.Errorf("update reference: %w", err)
	}

	if err := stream.SendAndClose(&gitalypb.UserApplyPatchResponse{
		BranchUpdate: &gitalypb.OperationBranchUpdate{
			CommitId:      patchedCommit.String(),
			BranchCreated: branchCreated,
		},
	}); err != nil {
		return fmt.Errorf("send and close: %w", err)
	}

	return nil
}

func (s *Server) applyPatchesWithIndex(
	ctx context.Context,
	repo *localrepo.Repo,
	parentCommitID git.ObjectID,
	committerSignature git.Signature,
	stream gitalypb.OperationService_UserApplyPatchServer,
) (git.ObjectID, error) {
	tempDir, err := repo.StorageTempDir()
	if err != nil {
		return "", fmt.Errorf("locating temporary directory: %w", err)
	}

	workDir, err := os.MkdirTemp(tempDir, "apply-patch-")
	if err != nil {
		return "", fmt.Errorf("creating work directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(workDir)
	}()

	mboxReader := streamio.NewReader(func() ([]byte, error) {
		req, err := stream.Recv()
		return req.GetPatches(), err
	})

	patches, err := splitAndParseMailbox(ctx, repo, mboxReader, workDir)
	if err != nil {
		return "", fmt.Errorf("parsing mailbox: %w", err)
	}

	currentCommitID := parentCommitID
	for i, patch := range patches {
		perPatchWorkdir := filepath.Join(workDir, strconv.Itoa(i))
		if err := os.Mkdir(perPatchWorkdir, mode.Directory); err != nil {
			return "", fmt.Errorf("creating work directory for patch %d: %w", i, err)
		}

		treeID, err := applyPatchToTreeish(ctx, repo, perPatchWorkdir, currentCommitID, patch)
		if err != nil {
			var conflictErr *localrepo.MergeTreeConflictError
			if errors.As(err, &conflictErr) {
				return "", structerr.NewFailedPrecondition("Patch failed at %04d %s: %w", i+1, patch.subject, err)
			}

			return "", fmt.Errorf("applying patch %d: %w", i+1, err)
		}

		_ = os.RemoveAll(perPatchWorkdir)

		authorDate := patch.authorDate
		if authorDate.IsZero() {
			authorDate = committerSignature.When
		}

		commitMessage := patch.subject + "\n"
		if body := strings.Trim(patch.body, "\n"); body != "" {
			commitMessage += "\n" + body + "\n"
		}

		commitID, err := repo.WriteCommit(ctx, localrepo.WriteCommitConfig{
			Parents:        []git.ObjectID{currentCommitID},
			TreeID:         treeID,
			AuthorName:     patch.authorName,
			AuthorEmail:    patch.authorEmail,
			AuthorDate:     authorDate,
			CommitterName:  committerSignature.Name,
			CommitterEmail: committerSignature.Email,
			CommitterDate:  committerSignature.When,
			Message:        commitMessage,
		})
		if err != nil {
			return "", fmt.Errorf("committing patch %d: %w", i+1, err)
		}

		currentCommitID = commitID
	}

	return currentCommitID, nil
}

func applyPatchToTreeish(
	ctx context.Context,
	repo *localrepo.Repo,
	workDir string,
	treeish git.ObjectID,
	patch parsedPatch,
) (git.ObjectID, error) {
	// We first try simple patch application. This mechanism is not able to do rename detection, but we have to use
	// this mechanism anyway because the patches may not give us enough information to do a proper three-way merge.
	objectID, err := applyPatchSimple(ctx, repo, workDir, treeish, patch)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// The direct application failed, most likely due to a merge conflict. Fall back to the
			// three-way merge strategy.
			return applyPatchThreeWay(ctx, repo, workDir, treeish, patch)
		}

		return "", err
	}

	return objectID, nil
}

func applyPatchSimple(
	ctx context.Context,
	repo *localrepo.Repo,
	workDir string,
	treeish git.ObjectID,
	patch parsedPatch,
) (git.ObjectID, error) {
	indexPath := filepath.Join(workDir, "index")
	defer func() { _ = os.Remove(indexPath) }()

	applyDir := filepath.Join(workDir, "apply")
	if err := os.Mkdir(applyDir, mode.Directory); err != nil {
		return "", fmt.Errorf("creating directory for patch application: %w", err)
	}
	defer func() { _ = os.RemoveAll(applyDir) }()

	// Seed the temporary index with the merged tree so that subsequent patches can again be applied directly on top
	// of it.
	var stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "read-tree",
			Args: []string{treeish.String()},
		},
		gitcmd.WithEnv("GIT_INDEX_FILE="+indexPath),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return "", structerr.New("read-tree merged tree: %w", err).WithMetadata("stderr", stderr.String())
	}

	var stdout bytes.Buffer
	stderr.Reset()
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "apply",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--3way"},
				gitcmd.Flag{Name: "--cached"},
			},
			Args: []string{patch.patchPath},
		},
		gitcmd.WithEnv("GIT_INDEX_FILE="+indexPath),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
		// git-apply(1) may end up creating files in its current working directory, even when using
		// "--cached".
		gitcmd.WithDir(applyDir),
	); err != nil {
		return "", structerr.NewInternal("failed applying patch: %w", err)
	}

	return writeTree(ctx, repo, indexPath)
}

func applyPatchThreeWay(
	ctx context.Context,
	repo *localrepo.Repo,
	workDir string,
	treeish git.ObjectID,
	patch parsedPatch,
) (git.ObjectID, error) {
	indexPath := filepath.Join(workDir, "index")
	defer func() { _ = os.Remove(indexPath) }()

	applyDir := filepath.Join(workDir, "apply")
	if err := os.Mkdir(applyDir, mode.Directory); err != nil {
		return "", fmt.Errorf("creating directory for patch application: %w", err)
	}
	defer func() { _ = os.RemoveAll(applyDir) }()

	// Build a fake ancestor index by taking the index information that is present in the diff, if it exists at all.
	// This fake ancestor is our base commit that we use for the three-way merge.
	var stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "apply",
			Flags: []gitcmd.Option{
				gitcmd.ValueFlag{Name: "--build-fake-ancestor", Value: indexPath},
			},
			Args: []string{patch.patchPath},
		},
		gitcmd.WithStderr(&stderr),
		gitcmd.WithDir(applyDir),
	); err != nil {
		return "", structerr.NewFailedPrecondition("building fake ancestor: %w", err).WithMetadata("stderr", stderr.String())
	}

	baseTree, err := writeTree(ctx, repo, indexPath)
	if err != nil {
		return "", fmt.Errorf("writing base tree: %w", err)
	}

	// Apply the patch on top of the reconstructed base tree to obtain the "theirs" side of the
	// three-way merge.
	stderr.Reset()
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "apply",
			Flags: []gitcmd.Option{
				gitcmd.Flag{Name: "--cached"},
			},
			Args: []string{patch.patchPath},
		},
		gitcmd.WithEnv("GIT_INDEX_FILE="+indexPath),
		gitcmd.WithStderr(&stderr),
		gitcmd.WithDir(applyDir),
	); err != nil {
		return "", structerr.NewFailedPrecondition("applying patch to base tree: %w", err).WithMetadata("stderr", stderr.String())
	}

	theirTree, err := writeTree(ctx, repo, indexPath)
	if err != nil {
		return "", fmt.Errorf("writing their tree: %w", err)
	}

	// Perform the three-way with the reconstructed merge base and theirs side.
	mergedTree, err := repo.MergeTree(ctx, treeish.String(), theirTree.String(), localrepo.WithMergeBase(git.Revision(baseTree.String())))
	if err != nil {
		return "", err
	}

	return mergedTree, nil
}

func writeTree(ctx context.Context, repo *localrepo.Repo, indexPath string) (git.ObjectID, error) {
	objectHash, err := repo.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := repo.ExecAndWait(ctx,
		gitcmd.Command{
			Name: "write-tree",
		},
		gitcmd.WithEnv("GIT_INDEX_FILE="+indexPath),
		gitcmd.WithStdout(&stdout),
		gitcmd.WithStderr(&stderr),
	); err != nil {
		return "", structerr.New("write-tree: %w", err).WithMetadata("stderr", stderr.String())
	}

	treeID, err := objectHash.FromHex(text.ChompBytes(stdout.Bytes()))
	if err != nil {
		return "", fmt.Errorf("parsing tree oid: %w", err)
	}

	return treeID, nil
}

func validateUserApplyPatchHeader(ctx context.Context, locator storage.Locator, header *gitalypb.UserApplyPatchRequest_Header) error {
	if err := locator.ValidateRepository(ctx, header.GetRepository()); err != nil {
		return err
	}

	if header.GetUser() == nil {
		return errors.New("missing User")
	}

	if len(header.GetTargetBranch()) == 0 {
		return errors.New("missing Branch")
	}

	return nil
}
