package sdk

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// CreateOptions describes a repository created by Create.
type CreateOptions struct {
	// DefaultBranch is the branch that HEAD of the new repository points to,
	// without the refs/heads/ prefix. If empty, the server's default (main) is
	// used.
	DefaultBranch string
	// ObjectFormat is the object hash of the new repository: "sha1" or
	// "sha256". If empty, the server's default is used.
	ObjectFormat string
}

// Create creates the repository on the server, empty. Only an operator may
// create repositories: the call needs the shared token (WithToken), and an
// agent token fails with PermissionDenied. A repository that exists already
// fails with AlreadyExists (see IsAlreadyExists).
//
// With the storage core in authoritative mode the server registers the new
// repository in the storage core; in shadow mode it is created untracked and
// StorageImport brings it in (see doc/graft/STORAGE.md).
//
// An empty repository has no branch to clone: CommitFiles onto its default
// branch, without StartRev, makes its first commit.
func (r *Repo) Create(ctx context.Context, opts CreateOptions) error {
	format, err := objectFormat(opts.ObjectFormat)
	if err != nil {
		return err
	}

	_, err = r.repositories().CreateRepository(ctx, &gitalypb.CreateRepositoryRequest{
		Repository:    r.repository(),
		DefaultBranch: []byte(trimBranchPrefix(opts.DefaultBranch)),
		ObjectFormat:  format,
	})
	return err
}

// HasBranches reports whether the repository has at least one branch. It
// fails with NotFound if the repository does not exist. Like Create, it needs
// the shared token.
func (r *Repo) HasBranches(ctx context.Context) (bool, error) {
	resp, err := r.repositories().HasLocalBranches(ctx, &gitalypb.HasLocalBranchesRequest{
		Repository: r.repository(),
	})
	if err != nil {
		return false, err
	}
	return resp.GetValue(), nil
}

// objectFormat maps the object format of CreateOptions to its protobuf value.
func objectFormat(format string) (gitalypb.ObjectFormat, error) {
	switch format {
	case "":
		return gitalypb.ObjectFormat_OBJECT_FORMAT_UNSPECIFIED, nil
	case "sha1":
		return gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1, nil
	case "sha256":
		return gitalypb.ObjectFormat_OBJECT_FORMAT_SHA256, nil
	default:
		return gitalypb.ObjectFormat_OBJECT_FORMAT_UNSPECIFIED,
			status.Errorf(codes.InvalidArgument, "object format %q: want sha1 or sha256", format)
	}
}

// repositories returns the repository service client on the connection of the client.
func (r *Repo) repositories() gitalypb.RepositoryServiceClient {
	return gitalypb.NewRepositoryServiceClient(r.client.conn)
}
