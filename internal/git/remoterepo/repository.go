package remoterepo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Repo represents a Git repository on a different Gitaly storage
type Repo struct {
	*gitalypb.Repository
	conn *grpc.ClientConn

	detectObjectHashOnce sync.Once
	objectHash           git.ObjectHash
	objectHashErr        error
}

// New creates a new remote Repository from its protobuf representation.
func New(ctx context.Context, repo *gitalypb.Repository, pool *client.Pool) (*Repo, error) {
	server, err := storage.ExtractGitalyServer(ctx, repo.GetStorageName())
	if err != nil {
		return nil, fmt.Errorf("remote repository: %w", err)
	}

	cc, err := pool.Dial(ctx, server.Address, server.Token)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}

	return &Repo{
		Repository: repo,
		conn:       cc,
	}, nil
}

// ObjectHash detects the object hash used by this particular repository.
func (rr *Repo) ObjectHash(ctx context.Context) (git.ObjectHash, error) {
	rr.detectObjectHashOnce.Do(func() {
		rr.objectHash, rr.objectHashErr = func() (git.ObjectHash, error) {
			client := gitalypb.NewRepositoryServiceClient(rr.conn)

			response, err := client.ObjectFormat(ctx, &gitalypb.ObjectFormatRequest{
				Repository: rr.Repository,
			})
			if err != nil {
				return git.ObjectHash{}, err
			}

			return git.ObjectHashByProto(response.GetFormat())
		}()
	})

	return rr.objectHash, rr.objectHashErr
}

// ResolveRevision will dial to the remote repository and attempt to resolve the
// revision string via the gRPC interface.
func (rr *Repo) ResolveRevision(ctx context.Context, revision git.Revision) (git.ObjectID, error) {
	cli := gitalypb.NewCommitServiceClient(rr.conn)
	resp, err := cli.FindCommit(ctx, &gitalypb.FindCommitRequest{
		Repository: rr.Repository,
		Revision:   []byte(revision.String()),
	})
	if err != nil {
		return "", err
	}

	oidHex := resp.GetCommit().GetId()
	if oidHex == "" {
		return "", git.ErrReferenceNotFound
	}

	objectHash, err := rr.ObjectHash(ctx)
	if err != nil {
		return "", fmt.Errorf("detecting object hash: %w", err)
	}

	oid, err := objectHash.FromHex(oidHex)
	if err != nil {
		return "", fmt.Errorf("parsing object ID: %w", err)
	}

	return oid, nil
}

// ResolveBranch will dial to the remote repository and resolve the branch named branch (without
// refs/heads/) by its exact reference name to the commit it points to, as
// `refs/heads/<branch>^{commit}` peels it. Unlike ResolveRevision, whose revision git resolves
// with its DWIM rules, a missing branch never resolves through refs/tags/refs/heads/<branch> or
// another reference. It returns git.ErrReferenceNotFound when the branch does not exist (also
// when branches below its name do, refs/heads/<branch>/x) or does not peel to a commit.
func (rr *Repo) ResolveBranch(ctx context.Context, branch string) (git.ObjectID, error) {
	return rr.ResolveReference(ctx, git.NewReferenceNameFromBranchName(branch))
}

// ResolveReference is ResolveBranch for a reference given by its full name: it resolves name by
// its exact reference name to the commit it points to, as `<name>^{commit}` peels it, and returns
// git.ErrReferenceNotFound when the reference does not exist (also when references below its name
// do) or does not peel to a commit.
func (rr *Repo) ResolveReference(ctx context.Context, name git.ReferenceName) (git.ObjectID, error) {
	commit, exists, err := rr.exactCommit(ctx, name)
	if err != nil {
		return "", err
	}
	if !exists || commit == "" {
		return "", git.ErrReferenceNotFound
	}
	return commit, nil
}

// ErrNotACommit is returned by ReadReference for a reference that exists but does not point to a
// commit (another object, or an object that is missing).
var ErrNotACommit = errors.New("the reference does not point to a commit")

// ReadReference will dial to the remote repository and read the reference named name by its exact
// name, to the commit it points to. It returns git.ErrReferenceNotFound when the reference does not
// exist (a dangling symbolic reference included), and ErrNotACommit when it exists but does not
// point to a commit, as a local read of its value would then fail to peel it. Of the root
// references, which RefExists does not take, only HEAD is read, by FindCommit: a dangling HEAD falls
// through git's rules there.
func (rr *Repo) ReadReference(ctx context.Context, name git.ReferenceName) (git.ObjectID, error) {
	if !strings.HasPrefix(name.String(), "refs/") {
		if name != "HEAD" {
			return "", git.ErrReferenceNotFound
		}
		return rr.ResolveRevision(ctx, name.Revision())
	}

	commit, exists, err := rr.exactCommit(ctx, name)
	switch {
	case err != nil:
		return "", err
	case !exists:
		return "", git.ErrReferenceNotFound
	case commit == "":
		return "", fmt.Errorf("%q: %w", name, ErrNotACommit)
	}
	return commit, nil
}

// exactCommit reads the reference named name by its exact name: exists tells whether it exists,
// and commit is the commit it points to, empty when it points to another object or to one that is
// missing.
//
// A branch is read with FindBranch, which reads it by its exact name and answers its commit in one
// call. Any other reference is checked with RefExists (exact: show-ref --verify, without listing the
// references below it; a dangling symbolic reference does not exist) and only then resolved with
// FindCommit, by the full name, which is the first name git's DWIM rules try. No RPC reads such a
// reference exactly with its value, so a reference deleted between the two calls resolves by git's
// rules, possibly through a decoy such as refs/refs/tags/x.
func (rr *Repo) exactCommit(ctx context.Context, name git.ReferenceName) (commit git.ObjectID, exists bool, returnedErr error) {
	if branch, ok := name.Branch(); ok {
		return rr.exactBranchCommit(ctx, name, branch)
	}

	exists, unresolvable, err := rr.refExists(ctx, name)
	if err != nil || !exists || unresolvable {
		return "", exists, err
	}

	commit, err = rr.ResolveRevision(ctx, name.Revision())
	if errors.Is(err, git.ErrReferenceNotFound) {
		return "", true, nil
	}
	return commit, true, err
}

// exactBranchCommit is exactCommit for the branch named branch, whose reference is name, with
// FindBranch. FindBranch answers no branch when it does not exist, and fails with "target reference
// is ambiguous" when it does not exist but branches below its name do (refs/heads/x/y): that is a
// missing branch too. It also fails for a branch that does not point to a commit (its object is
// missing, or it is another object). Such a failure is classified with RefExists and FindCommit, as
// exactCommit reads other references: a branch that exists but does not peel to a commit exists
// without one, one that does not exist is missing, and anything else is FindBranch's failure. The
// commit FindCommit may answer is never taken, so a branch is only ever answered by FindBranch.
func (rr *Repo) exactBranchCommit(ctx context.Context, name git.ReferenceName, branch string) (git.ObjectID, bool, error) {
	resp, err := gitalypb.NewRefServiceClient(rr.conn).FindBranch(ctx, &gitalypb.FindBranchRequest{
		Repository: rr.Repository,
		Name:       []byte(branch),
	})
	if err != nil {
		if st := status.Convert(err); st.Code() == codes.InvalidArgument && strings.HasPrefix(st.Message(), "target reference is ambiguous") {
			return "", false, nil
		}
		exists, unresolvable, existsErr := rr.refExists(ctx, name)
		switch {
		case existsErr != nil:
		case !exists:
			return "", false, nil
		case unresolvable:
			return "", true, nil
		default:
			if _, resolveErr := rr.ResolveRevision(ctx, name.Revision()); errors.Is(resolveErr, git.ErrReferenceNotFound) {
				return "", true, nil
			}
		}
		return "", false, fmt.Errorf("find branch: %w", err)
	}
	if resp.GetBranch() == nil {
		return "", false, nil
	}

	return git.ObjectID(resp.GetBranch().GetTargetCommit().GetId()), true, nil
}

// refExists reports whether the reference named name exists, with RefExists, and whether its object
// is missing (unresolvable): RefExists answers such a reference with an UnresolvableReference
// detail. Any other error, the detail missing included, is a failure.
func (rr *Repo) refExists(ctx context.Context, name git.ReferenceName) (exists, unresolvable bool, returnedErr error) {
	resp, err := gitalypb.NewRefServiceClient(rr.conn).RefExists(ctx, &gitalypb.RefExistsRequest{
		Repository: rr.Repository,
		Ref:        []byte(name),
	})
	if err != nil {
		for _, detail := range status.Convert(err).Details() {
			if refErr, ok := detail.(*gitalypb.RefExistsError); ok && refErr.GetUnresolvableReference() != nil {
				return true, true, nil
			}
		}
		return false, false, fmt.Errorf("ref exists: %w", err)
	}
	return resp.GetValue(), false, nil
}

// HasBranches will dial to the remote repository and check whether the repository has any branches.
func (rr *Repo) HasBranches(ctx context.Context) (bool, error) {
	resp, err := gitalypb.NewRepositoryServiceClient(rr.conn).HasLocalBranches(
		ctx, &gitalypb.HasLocalBranchesRequest{Repository: rr.Repository})
	if err != nil {
		return false, fmt.Errorf("has local branches: %w", err)
	}

	return resp.GetValue(), nil
}

// GetDefaultBranch returns the default branch for the remote repository. It does so by invoking
// `FindDefaultBranchName()`, which itself is a wrapper around `localrepo.GetDefaultBranch()`.
// Semantics of this function thus match the localrepo semantics.
func (rr *Repo) GetDefaultBranch(ctx context.Context) (git.ReferenceName, error) {
	resp, err := gitalypb.NewRefServiceClient(rr.conn).FindDefaultBranchName(
		ctx, &gitalypb.FindDefaultBranchNameRequest{Repository: rr.Repository})
	if err != nil {
		return "", err
	}

	return git.ReferenceName(resp.GetName()), nil
}

// HeadReference returns the reference that HEAD points to for the remote
// repository.
func (rr *Repo) HeadReference(ctx context.Context) (git.ReferenceName, error) {
	resp, err := gitalypb.NewRefServiceClient(rr.conn).FindDefaultBranchName(
		ctx, &gitalypb.FindDefaultBranchNameRequest{
			Repository: rr.Repository,
			HeadOnly:   true,
		})
	if err != nil {
		return "", err
	}

	return git.ReferenceName(resp.GetName()), nil
}
