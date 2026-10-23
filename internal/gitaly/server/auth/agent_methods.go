package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/internal/structerr"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// agentNeverAllowedServices are the gRPC services that no caller authenticated
// with a per-agent token may call, even when [graft] agent_rpcs names one of
// their methods:
//
//   - HookService is called back by Git hooks over the internal socket. Called
//     directly, it would let an agent forge hook payloads, for example an access
//     check or changeset log entries for updates that never happened.
//   - ObjectPoolService manages the object pools that repositories borrow
//     objects from. Deleting or disconnecting a pool can corrupt the
//     repositories that use it.
//   - CleanupService and PartitionService are administrative: they rewrite and
//     clean up repositories and partitions outside the reference policy.
//   - InternalGitaly serves the internal calls between Gitaly nodes.
//   - StorageCoreService is the storage core's administration: imports,
//     checkpoints and garbage collection, which deletes packs of the bucket.
//     It is reserved for operators (doc/graft/STORAGE.md).
var agentNeverAllowedServices = []string{
	gitalypb.HookService_ServiceDesc.ServiceName,
	gitalypb.ObjectPoolService_ServiceDesc.ServiceName,
	gitalypb.CleanupService_ServiceDesc.ServiceName,
	gitalypb.PartitionService_ServiceDesc.ServiceName,
	gitalypb.InternalGitaly_ServiceDesc.ServiceName,
	gitalypb.StorageCoreService_ServiceDesc.ServiceName,
}

// agentNeverAllowedMethods are RPCs that no caller authenticated with a
// per-agent token may call, even when [graft] agent_rpcs names them. Each of
// them replaces, removes or rewrites a whole repository, writes references
// outside the reference transaction policy when it is not checked, or installs
// server-side hooks. They are administrative operations, reserved for the
// shared auth.token.
//
// The repository creation RPCs are never allowed as a whole, not only through
// the default allowlist, so that [graft] agent_rpcs cannot re-enable them.
// CreateRepositoryFromSnapshot is one of them: it extracts a tar archive,
// downloaded from a URL that the client chooses, into the new repository
// without filtering it. The archive can carry custom_hooks, a config or
// symbolic links, so an agent could install hooks that later run as the Gitaly
// user. CreateRepositoryFromURL, CreateRepositoryFromBundle, CreateRepository
// and CreateFork create repositories from data or URLs the agent chooses, and
// CreateRepositoryFromURL makes the server fetch any URL (server-side request
// forgery). BackupRepository writes repository data to a location chosen by the
// caller.
//
// RemoteService.UpdateRemoteMirror pushes a repository to a URL that the caller
// chooses, so an agent could send repository contents to any server it likes
// (exfiltration). It is never allowed, even through [graft] agent_rpcs.
// RemoteService.FindRemoteRepository and FindRemoteRootRef run `git ls-remote`
// against a URL that the caller chooses, FindRemoteRootRef with a caller-chosen
// authorization header and a resolved address that pins DNS. Like
// CreateRepositoryFromURL, they would let an agent make the server send requests
// to any address, including internal ones (server-side request forgery).
//
// SSHService.SSHUploadArchive runs git-upload-archive, which takes the revision
// to archive in its own pkt-line input, not in a field of the request, and does
// not honour uploadpack.hideRefs. The request filter cannot see that revision,
// so an agent could read refs/graft/ by name with `git archive --remote`. No
// graft client needs it (RepositoryService.GetArchive names its revision in a
// field the filter checks), so it is never allowed rather than parsed.
var agentNeverAllowedMethods = map[string]struct{}{
	gitalypb.SSHService_SSHUploadArchive_FullMethodName:                    {},
	gitalypb.RemoteService_UpdateRemoteMirror_FullMethodName:               {},
	gitalypb.RemoteService_FindRemoteRepository_FullMethodName:             {},
	gitalypb.RemoteService_FindRemoteRootRef_FullMethodName:                {},
	gitalypb.RepositoryService_CreateRepository_FullMethodName:             {},
	gitalypb.RepositoryService_CreateFork_FullMethodName:                   {},
	gitalypb.RepositoryService_CreateRepositoryFromURL_FullMethodName:      {},
	gitalypb.RepositoryService_CreateRepositoryFromBundle_FullMethodName:   {},
	gitalypb.RepositoryService_BackupRepository_FullMethodName:             {},
	gitalypb.RepositoryService_RemoveRepository_FullMethodName:             {},
	gitalypb.RepositoryService_ReplicateRepository_FullMethodName:          {},
	gitalypb.RepositoryService_RestoreRepository_FullMethodName:            {},
	gitalypb.RepositoryService_CreateRepositoryFromSnapshot_FullMethodName: {},
	gitalypb.RepositoryService_SetCustomHooks_FullMethodName:               {},
	gitalypb.RepositoryService_RestoreCustomHooks_FullMethodName:           {},
	gitalypb.RepositoryService_WriteRef_FullMethodName:                     {},
	gitalypb.RepositoryService_FetchRemote_FullMethodName:                  {},
	gitalypb.RepositoryService_FetchSourceBranch_FullMethodName:            {},
	gitalypb.RepositoryService_FetchBundle_FullMethodName:                  {},
	gitalypb.RepositoryService_OptimizeRepository_FullMethodName:           {},
	gitalypb.RepositoryService_PruneUnreachableObjects_FullMethodName:      {},
	gitalypb.RefService_UpdateReferences_FullMethodName:                    {},
	gitalypb.RefService_DeleteRefs_FullMethodName:                          {},
}

// builtinAgentRPCs is the set of full method names that callers authenticated
// with a per-agent token may call without any [graft] agent_rpcs entry. It
// holds what the SDK (sdk/), the CLI (cmd/graft) and the graft-cache
// pass-through use from agents, and the read-only accessors:
//
//   - ObjectService, ChangesetService and DerivedDataService: every RPC. They
//     read objects, changesets and derived data.
//   - SearchService (`graft search`): every RPC. It only reads a commit.
//   - ReviewService (`graft review`): every RPC. Its mutators write the review
//     store of the repository and the keep-around references of the review's
//     head and base commits (refs/keep-around/review/<commit>, REVIEW ROOTS
//     RULE): the name is derived from a commit that must already be in the
//     repository, so they cannot write a branch, a tag or any other name, nor
//     add an object. They record the authenticated identity as the actor, so
//     an agent cannot approve its own change on behalf of another agent.
//   - The Find, List and Get RPCs of CommitService, RefService, BlobService and
//     DiffService, which are read-only accessors.
//   - CommitService.TreeEntry, which the SDK reads trees with.
//   - DiffService.DiffStats, which `graft show` reads the changed files and
//     line counts of a commit with. It is read-only.
//   - OperationService: UserLand, UserCommitFiles, UserCreateBranch and
//     UserDeleteBranch. Each updates references through the reference
//     transaction, so the policy decides on them. Every other OperationService
//     RPC is left out.
//   - ServerService.ServerInfo and RepositoryService.RepositoryExists, which
//     graft-cache uses to authorize a caller for a repository.
//
// Health checks are not in the list: they are answered without authentication.
//
// SSHService (SSHUploadPackWithSidechannel, SSHReceivePack) is deliberately not
// in the list. Agents can therefore not fetch or push over Git through Gitaly,
// including the graft-cache sidechannel pass-through. An operator who wants
// that enables the two RPCs with a [graft] agent_rpcs entry; the trade-off is
// documented in AGENT-SECURITY.md (WP20).
var builtinAgentRPCs = buildBuiltinAgentRPCs()

func buildBuiltinAgentRPCs() map[string]struct{} {
	allowed := make(map[string]struct{})
	add := func(fullMethod string) { allowed[fullMethod] = struct{}{} }

	addService := func(desc *grpc.ServiceDesc, accessorsOnly bool) {
		for _, method := range desc.Methods {
			if !accessorsOnly || isAccessorMethod(method.MethodName) {
				add(fullMethodName(desc.ServiceName, method.MethodName))
			}
		}
		for _, stream := range desc.Streams {
			if !accessorsOnly || isAccessorMethod(stream.StreamName) {
				add(fullMethodName(desc.ServiceName, stream.StreamName))
			}
		}
	}

	addService(&gitalypb.ObjectService_ServiceDesc, false)
	addService(&gitalypb.ChangesetService_ServiceDesc, false)
	addService(&gitalypb.DerivedDataService_ServiceDesc, false)
	addService(&gitalypb.SearchService_ServiceDesc, false)
	addService(&gitalypb.ReviewService_ServiceDesc, false)

	addService(&gitalypb.CommitService_ServiceDesc, true)
	addService(&gitalypb.RefService_ServiceDesc, true)
	addService(&gitalypb.BlobService_ServiceDesc, true)
	addService(&gitalypb.DiffService_ServiceDesc, true)

	add(gitalypb.CommitService_TreeEntry_FullMethodName)
	add(gitalypb.DiffService_DiffStats_FullMethodName)
	add(gitalypb.ServerService_ServerInfo_FullMethodName)
	add(gitalypb.RepositoryService_RepositoryExists_FullMethodName)
	add(gitalypb.OperationService_UserLand_FullMethodName)
	add(gitalypb.OperationService_UserCommitFiles_FullMethodName)
	add(gitalypb.OperationService_UserCreateBranch_FullMethodName)
	add(gitalypb.OperationService_UserDeleteBranch_FullMethodName)

	return allowed
}

// isAccessorMethod reports whether an RPC name is a read-only accessor by
// convention: it starts with Find, List or Get.
func isAccessorMethod(name string) bool {
	return strings.HasPrefix(name, "Find") || strings.HasPrefix(name, "List") || strings.HasPrefix(name, "Get")
}

func fullMethodName(service, method string) string {
	return "/" + service + "/" + method
}

// agentAllowlist is the set of full method names ("/package.Service/Method")
// that callers authenticated with a per-agent token may call. Every other
// method is denied.
type agentAllowlist map[string]struct{}

// newAgentAllowlist returns the built-in allowlist extended with extraRPCs, the
// [graft] agent_rpcs. Every extra name must be a full method name of a
// registered RPC that is not in the never-allowed lists, otherwise an error is
// returned. The configuration is therefore rejected at startup instead of
// silently ignoring an entry.
func newAgentAllowlist(extraRPCs []string) (agentAllowlist, error) {
	allowlist := make(agentAllowlist, len(builtinAgentRPCs)+len(extraRPCs))
	for fullMethod := range builtinAgentRPCs {
		allowlist[fullMethod] = struct{}{}
	}

	for i, fullMethod := range extraRPCs {
		if err := validateAgentRPC(fullMethod); err != nil {
			return nil, fmt.Errorf("graft agent_rpcs entry %d: %w", i+1, err)
		}

		allowlist[fullMethod] = struct{}{}
	}

	return allowlist, nil
}

// validateAgentRPC checks one [graft] agent_rpcs entry.
func validateAgentRPC(fullMethod string) error {
	service, method, ok := splitFullMethod(fullMethod)
	if !ok {
		return fmt.Errorf("%q is not a full gRPC method name such as /gitaly.RepositoryService/RepositorySize", fullMethod)
	}

	if agentNeverAllowed(fullMethod) {
		return fmt.Errorf("%q may never be called by an agent", fullMethod)
	}

	if !isRegisteredMethod(service, method) {
		return fmt.Errorf("%q is not a registered gRPC method", fullMethod)
	}

	return nil
}

// splitFullMethod splits "/package.Service/Method" into its service and method
// names.
func splitFullMethod(fullMethod string) (string, string, bool) {
	rest, ok := strings.CutPrefix(fullMethod, "/")
	if !ok {
		return "", "", false
	}

	service, method, ok := strings.Cut(rest, "/")
	if !ok || service == "" || method == "" || strings.Contains(method, "/") {
		return "", "", false
	}

	return service, method, true
}

// isRegisteredMethod reports whether the protobuf registry knows the method of
// the service.
func isRegisteredMethod(service, method string) bool {
	desc, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
	if err != nil {
		return false
	}

	serviceDesc, ok := desc.(protoreflect.ServiceDescriptor)
	if !ok {
		return false
	}

	return serviceDesc.Methods().ByName(protoreflect.Name(method)) != nil
}

// agentNeverAllowed reports whether no agent may call the RPC fullMethod,
// whatever the allowlist says.
func agentNeverAllowed(fullMethod string) bool {
	if _, denied := agentNeverAllowedMethods[fullMethod]; denied {
		return true
	}

	for _, service := range agentNeverAllowedServices {
		if strings.HasPrefix(fullMethod, "/"+service+"/") {
			return true
		}
	}

	return false
}

// mayCall reports whether a caller authenticated with a per-agent token may
// call the RPC fullMethod. The RPC must be on the allowlist and must not be
// never-allowed.
func (a agentAllowlist) mayCall(fullMethod string) bool {
	if agentNeverAllowed(fullMethod) {
		return false
	}

	_, ok := a[fullMethod]
	return ok
}

// authorizeAgent rejects with PermissionDenied a request of a caller that
// authenticated with a per-agent token for an RPC that is not on the allowlist.
// Every other caller is left alone.
func authorizeAgent(ctx context.Context, allowlist agentAllowlist, fullMethod string) error {
	id, ok := identity.FromContext(ctx)
	if !ok || id.Source != identity.SourceAgent {
		return nil
	}

	if !allowlist.mayCall(fullMethod) {
		return structerr.NewPermissionDenied("agent %q may not call %s: the method is not allowed for agents", id.Name, fullMethod)
	}

	return nil
}

// withAgentAuthorization returns unary and stream interceptors that run the
// given authentication interceptors and then authorizeAgent on the
// authenticated context, before the handler.
func withAgentAuthorization(allowlist agentAllowlist, unary grpc.UnaryServerInterceptor, stream grpc.StreamServerInterceptor) (grpc.UnaryServerInterceptor, grpc.StreamServerInterceptor) {
	authorizedUnary := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return unary(ctx, req, info, func(ctx context.Context, req any) (any, error) {
			if err := authorizeAgent(ctx, allowlist, info.FullMethod); err != nil {
				return nil, err
			}

			return handler(ctx, req)
		})
	}

	authorizedStream := func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return stream(srv, ss, info, func(srv any, ss grpc.ServerStream) error {
			if err := authorizeAgent(ss.Context(), allowlist, info.FullMethod); err != nil {
				return err
			}

			return handler(srv, ss)
		})
	}

	return authorizedUnary, authorizedStream
}
