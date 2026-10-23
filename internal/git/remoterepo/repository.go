package remoterepo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/feder-cr/git-ai/internal/git"
	"github.com/feder-cr/git-ai/internal/gitaly/storage"
	"github.com/feder-cr/git-ai/internal/grpc/client"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
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
	target, err := rr.referenceTarget(ctx, name)
	if err != nil {
		return "", err
	}
	if target == "" {
		return "", git.ErrReferenceNotFound
	}

	// The object ID read above, peeled as `<name>^{commit}` is: an object ID is never
	// resolved through a reference.
	return rr.ResolveRevision(ctx, git.Revision(target)+"^{commit}")
}

// ReadReference will dial to the remote repository and read the reference named name by its exact
// name, to the commit it points to, or return git.ErrReferenceNotFound when it does not exist (a
// dangling symbolic reference included) or does not peel to a commit. RefExists checks the exact
// name (show-ref --verify, without listing the references below it), and only then FindCommit
// resolves it, by the full name, which is the first name git's DWIM rules try. Of the root
// references, which RefExists does not take, only HEAD is read, by FindCommit: a dangling HEAD falls
// through git's rules there.
func (rr *Repo) ReadReference(ctx context.Context, name git.ReferenceName) (git.ObjectID, error) {
	if !strings.HasPrefix(name.String(), "refs/") {
		if name != "HEAD" {
			return "", git.ErrReferenceNotFound
		}
		return rr.ResolveRevision(ctx, name.Revision())
	}

	resp, err := gitalypb.NewRefServiceClient(rr.conn).RefExists(ctx, &gitalypb.RefExistsRequest{
		Repository: rr.Repository,
		Ref:        []byte(name),
	})
	if err != nil {
		return "", fmt.Errorf("ref exists: %w", err)
	}
	if !resp.GetValue() {
		return "", git.ErrReferenceNotFound
	}

	return rr.ResolveRevision(ctx, name.Revision())
}

// referenceTarget returns the object ID that the reference named name points to, read by its exact
// name with ListRefs, or an empty string when it does not exist (also when references below its
// name do).
func (rr *Repo) referenceTarget(ctx context.Context, name git.ReferenceName) (string, error) {
	// ListRefs matches references below the pattern too: only the exact name counts.
	stream, err := gitalypb.NewRefServiceClient(rr.conn).ListRefs(ctx, &gitalypb.ListRefsRequest{
		Repository: rr.Repository,
		Patterns:   [][]byte{[]byte(name)},
	})
	if err != nil {
		return "", fmt.Errorf("list refs: %w", err)
	}
	var target string
	for {
		resp, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", fmt.Errorf("list refs: %w", err)
		}
		for _, ref := range resp.GetReferences() {
			if git.ReferenceName(ref.GetName()) == name {
				target = ref.GetTarget()
			}
		}
	}

	return target, nil
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
