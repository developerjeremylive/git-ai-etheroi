package sdk

import (
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

// Code returns the gRPC status code of err. It returns codes.OK for nil and
// codes.Unknown for errors that carry no status.
func Code(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	return status.Code(err)
}

// IsNotFound reports whether err is a NotFound error, for example a missing
// path, revision or object.
func IsNotFound(err error) bool {
	return Code(err) == codes.NotFound
}

// IsAlreadyExists reports whether err is an AlreadyExists error, for example
// Create of a repository that exists already.
func IsAlreadyExists(err error) bool {
	return Code(err) == codes.AlreadyExists
}

// LandError returns the structured details of a failed Land call, if any.
func LandError(err error) (*gitalypb.UserLandError, bool) {
	return detailOf[*gitalypb.UserLandError](err)
}

// IsMergeConflict reports whether a Land call failed because the change does
// not apply cleanly onto the branch tip. The conflicting files are available
// through LandError.
func IsMergeConflict(err error) bool {
	e, ok := LandError(err)
	return ok && e.GetMergeConflict() != nil
}

// IsNotAncestor reports whether a fast-forward-only Land call failed because
// the branch tip is not an ancestor of the commit.
func IsNotAncestor(err error) bool {
	e, ok := LandError(err)
	return ok && e.GetNotAncestor() != nil
}

// IsReferenceUpdateConflict reports whether a Land call gave up because the
// branch kept moving and the server exhausted its compare-and-swap attempts.
func IsReferenceUpdateConflict(err error) bool {
	e, ok := LandError(err)
	return ok && e.GetReferenceUpdate() != nil
}

// IsRejectedByHook reports whether a Land or CommitFiles call was rejected by
// a custom hook or by an access check.
func IsRejectedByHook(err error) bool {
	if e, ok := LandError(err); ok && (e.GetCustomHook() != nil || e.GetAccessCheck() != nil) {
		return true
	}
	if e, ok := CommitFilesError(err); ok && (e.GetCustomHook() != nil || e.GetAccessCheck() != nil) {
		return true
	}
	return false
}

// CommitFilesError returns the structured details of a failed CommitFiles
// call, if any.
func CommitFilesError(err error) (*gitalypb.UserCommitFilesError, bool) {
	return detailOf[*gitalypb.UserCommitFilesError](err)
}

// detailOf returns the first detail of err's gRPC status that has type T.
func detailOf[T proto.Message](err error) (T, bool) {
	var zero T

	st, ok := status.FromError(err)
	if !ok {
		return zero, false
	}

	for _, detail := range st.Details() {
		if msg, ok := detail.(T); ok {
			return msg, true
		}
	}

	return zero, false
}
