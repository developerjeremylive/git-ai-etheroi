package sdk

import (
	"context"
	"errors"
	"io"
	"strings"

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

// Head returns the full name of the reference that HEAD of the repository
// points to, for example refs/heads/main. The reference may not exist yet, as
// in a repository without commits. A detached HEAD fails with
// FailedPrecondition. Agents may call it.
func (r *Repo) Head(ctx context.Context) (string, error) {
	resp, err := gitalypb.NewRefServiceClient(r.client.conn).FindDefaultBranchName(ctx, &gitalypb.FindDefaultBranchNameRequest{
		Repository: r.repository(),
		HeadOnly:   true,
	})
	if err != nil {
		return "", err
	}
	return string(resp.GetName()), nil
}

// HasReferences reports whether the repository has any reference at all
// (branches, tags and every other namespace below refs/). It stops reading at
// the first reference. Agents may call it.
func (r *Repo) HasReferences(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := gitalypb.NewRefServiceClient(r.client.conn).ListRefs(ctx, &gitalypb.ListRefsRequest{
		Repository: r.repository(),
		Patterns:   [][]byte{[]byte("refs/")},
	})
	if err != nil {
		return false, err
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if len(resp.GetReferences()) > 0 {
			return true, nil
		}
	}
}

// ReferenceExists reports whether the reference with the full name name (for
// example refs/heads/main) exists, by its exact name. Like Create, it needs the
// shared token.
func (r *Repo) ReferenceExists(ctx context.Context, name string) (bool, error) {
	if !strings.HasPrefix(name, "refs/") {
		return false, status.Errorf(codes.InvalidArgument, "reference %q: want a full name below refs/", name)
	}
	resp, err := gitalypb.NewRefServiceClient(r.client.conn).RefExists(ctx, &gitalypb.RefExistsRequest{
		Repository: r.repository(),
		Ref:        []byte(name),
	})
	if err != nil {
		return false, err
	}
	return resp.GetValue(), nil
}

// ZeroOID returns the zero object ID of the repository's object hash: as
// CommitOptions.ExpectedOldOID it requires that the branch does not exist yet.
// Like Create, it needs the shared token.
func (r *Repo) ZeroOID(ctx context.Context) (string, error) {
	resp, err := r.repositories().ObjectFormat(ctx, &gitalypb.ObjectFormatRequest{Repository: r.repository()})
	if err != nil {
		return "", err
	}
	switch resp.GetFormat() {
	case gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1:
		return strings.Repeat("0", 40), nil
	case gitalypb.ObjectFormat_OBJECT_FORMAT_SHA256:
		return strings.Repeat("0", 64), nil
	default:
		return "", status.Errorf(codes.Internal, "unknown object format %v", resp.GetFormat())
	}
}

// CreateBranchOptions describes a branch created by CreateBranch.
type CreateBranchOptions struct {
	// Branch is the new branch, without the refs/heads/ prefix.
	Branch string
	// StartPoint is the commit the branch points to.
	StartPoint string
	// User is the acting user, which the server's access policy checks.
	User User
}

// CreateBranch creates a branch that must not exist yet. It fails with
// FailedPrecondition when the branch exists, and with PermissionDenied when
// the server's policy refuses the update.
func (r *Repo) CreateBranch(ctx context.Context, opts CreateBranchOptions) error {
	if trimBranchPrefix(opts.Branch) == "" || opts.StartPoint == "" {
		return status.Error(codes.InvalidArgument, "branch and start point are required")
	}
	_, err := r.client.operations.UserCreateBranch(ctx, &gitalypb.UserCreateBranchRequest{
		Repository: r.repository(),
		BranchName: []byte(trimBranchPrefix(opts.Branch)),
		User:       opts.User.toProto(),
		StartPoint: []byte(opts.StartPoint),
	})
	return err
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
