package auth

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// explicitlyAllowedAgentRPCs are the built-in agent RPCs that are not covered
// by a whole-service or accessor rule. They are the RPCs that the SDK, the CLI
// and graft-cache use from agents, see builtinAgentRPCs.
var explicitlyAllowedAgentRPCs = []string{
	gitalypb.CommitService_TreeEntry_FullMethodName,
	gitalypb.DiffService_DiffStats_FullMethodName,
	gitalypb.ServerService_ServerInfo_FullMethodName,
	gitalypb.RepositoryService_RepositoryExists_FullMethodName,
	gitalypb.OperationService_UserLand_FullMethodName,
	gitalypb.OperationService_UserCommitFiles_FullMethodName,
	gitalypb.OperationService_UserCreateBranch_FullMethodName,
	gitalypb.OperationService_UserDeleteBranch_FullMethodName,
}

// sdkAndCLIAgentRPCs are the RPCs that the SDK (sdk/), the CLI (cmd/graft),
// the workspace package and graft-cache call. Agents must be able to call all
// of them without any agent_rpcs entry.
var sdkAndCLIAgentRPCs = []string{
	gitalypb.ObjectService_GetObjects_FullMethodName,
	gitalypb.ObjectService_GetTree_FullMethodName,
	gitalypb.ChangesetService_ListChangesets_FullMethodName,
	gitalypb.ChangesetService_WatchChangesets_FullMethodName,
	gitalypb.DerivedDataService_Blame_FullMethodName,
	gitalypb.DerivedDataService_PathHistory_FullMethodName,
	gitalypb.CommitService_FindCommit_FullMethodName,
	gitalypb.CommitService_TreeEntry_FullMethodName,
	gitalypb.DiffService_DiffStats_FullMethodName,
	gitalypb.OperationService_UserCommitFiles_FullMethodName,
	gitalypb.OperationService_UserLand_FullMethodName,
	gitalypb.OperationService_UserCreateBranch_FullMethodName,
	gitalypb.OperationService_UserDeleteBranch_FullMethodName,
	gitalypb.RepositoryService_RepositoryExists_FullMethodName,
	gitalypb.SearchService_SearchCode_FullMethodName,
	gitalypb.ReviewService_CreateReview_FullMethodName,
	gitalypb.ReviewService_GetReview_FullMethodName,
	gitalypb.ReviewService_ListReviews_FullMethodName,
	gitalypb.ReviewService_UpdateReview_FullMethodName,
	gitalypb.ReviewService_AddReviewComment_FullMethodName,
	gitalypb.ReviewService_SubmitReviewVerdict_FullMethodName,
	gitalypb.ReviewService_CloseReview_FullMethodName,
}

// expectedAgentRPC is the expectation for the built-in allowlist: the rules
// that builtinAgentRPCs is documented with, written independently of the
// implementation.
func expectedAgentRPC(fullMethod string) bool {
	for _, service := range []string{
		gitalypb.ObjectService_ServiceDesc.ServiceName,
		gitalypb.ChangesetService_ServiceDesc.ServiceName,
		gitalypb.DerivedDataService_ServiceDesc.ServiceName,
		gitalypb.SearchService_ServiceDesc.ServiceName,
		gitalypb.ReviewService_ServiceDesc.ServiceName,
	} {
		if strings.HasPrefix(fullMethod, "/"+service+"/") {
			return true
		}
	}

	for _, service := range []string{
		gitalypb.CommitService_ServiceDesc.ServiceName,
		gitalypb.RefService_ServiceDesc.ServiceName,
		gitalypb.BlobService_ServiceDesc.ServiceName,
		gitalypb.DiffService_ServiceDesc.ServiceName,
	} {
		prefix := "/" + service + "/"
		if strings.HasPrefix(fullMethod, prefix) {
			method := strings.TrimPrefix(fullMethod, prefix)
			if strings.HasPrefix(method, "Find") || strings.HasPrefix(method, "List") || strings.HasPrefix(method, "Get") {
				return true
			}
		}
	}

	for _, explicit := range explicitlyAllowedAgentRPCs {
		if explicit == fullMethod {
			return true
		}
	}

	return false
}

// registeredFullMethods returns the full method names of every RPC in the
// global protobuf registry.
func registeredFullMethods(t *testing.T) []string {
	t.Helper()

	var methods []string
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		services := file.Services()
		for i := 0; i < services.Len(); i++ {
			service := services.Get(i)
			rpcs := service.Methods()
			for j := 0; j < rpcs.Len(); j++ {
				methods = append(methods, "/"+string(service.FullName())+"/"+string(rpcs.Get(j).Name()))
			}
		}
		return true
	})

	require.NotEmpty(t, methods)
	return methods
}

// TestAgentAllowlist_EveryRegisteredRPC checks the built-in allowlist against
// every RPC of the protobuf registry: each one is either allowlisted, as the
// rules say, or denied for agents. The never-allowed RPCs are denied even
// when the allowlist would have them.
func TestAgentAllowlist_EveryRegisteredRPC(t *testing.T) {
	t.Parallel()

	allowlist, err := newAgentAllowlist(nil)
	require.NoError(t, err)

	allowed := 0
	for _, fullMethod := range registeredFullMethods(t) {
		want := expectedAgentRPC(fullMethod)
		require.Equal(t, want, allowlist.mayCall(fullMethod), "agent may call %s", fullMethod)

		if want {
			allowed++
		}
	}
	require.Positive(t, allowed)

	for _, fullMethod := range []string{
		gitalypb.RepositoryService_WriteRef_FullMethodName,
		gitalypb.RefService_DeleteRefs_FullMethodName,
		gitalypb.RefService_UpdateReferences_FullMethodName,
		gitalypb.RepositoryService_FetchRemote_FullMethodName,
		gitalypb.RepositoryService_CreateRepository_FullMethodName,
		gitalypb.RepositoryService_CreateFork_FullMethodName,
		gitalypb.RepositoryService_CreateRepositoryFromURL_FullMethodName,
		gitalypb.RepositoryService_CreateRepositoryFromBundle_FullMethodName,
		gitalypb.RepositoryService_CreateRepositoryFromSnapshot_FullMethodName,
		gitalypb.RepositoryService_BackupRepository_FullMethodName,
		gitalypb.RepositoryService_RemoveRepository_FullMethodName,
		gitalypb.RepositoryService_ReplicateRepository_FullMethodName,
		gitalypb.RepositoryService_RestoreRepository_FullMethodName,
		gitalypb.RepositoryService_SetCustomHooks_FullMethodName,
		gitalypb.RepositoryService_RestoreCustomHooks_FullMethodName,
		gitalypb.RepositoryService_FetchBundle_FullMethodName,
		gitalypb.RepositoryService_OptimizeRepository_FullMethodName,
		gitalypb.RepositoryService_PruneUnreachableObjects_FullMethodName,
		gitalypb.OperationService_UserUpdateBranch_FullMethodName,
		gitalypb.HookService_PreReceiveHook_FullMethodName,
		gitalypb.HookService_ReferenceTransactionHook_FullMethodName,
		gitalypb.ObjectPoolService_DeleteObjectPool_FullMethodName,
		gitalypb.CleanupService_RewriteHistory_FullMethodName,
		gitalypb.PartitionService_BackupPartition_FullMethodName,
		gitalypb.StorageCoreService_CollectGarbage_FullMethodName,
		gitalypb.StorageCoreService_CollectRemovedRepositories_FullMethodName,
		gitalypb.StorageCoreService_GetStorageState_FullMethodName,
	} {
		require.False(t, allowlist.mayCall(fullMethod), "agent may not call %s", fullMethod)
	}
}

// TestAgentAllowlist_NeverAllowedIsNotInBuiltin checks that no never-allowed
// RPC is on the built-in list, so the list never contradicts the denials.
func TestAgentAllowlist_NeverAllowedIsNotInBuiltin(t *testing.T) {
	t.Parallel()

	for fullMethod := range builtinAgentRPCs {
		require.False(t, agentNeverAllowed(fullMethod), "built-in agent RPC %s is never allowed", fullMethod)
	}
}

// TestAgentAllowlist_SDKAndCLIPaths checks that the RPCs used by the SDK, the
// CLI and graft-cache are allowed, and that agents can call them through the
// interceptors.
func TestAgentAllowlist_SDKAndCLIPaths(t *testing.T) {
	t.Parallel()

	allowlist, err := newAgentAllowlist(nil)
	require.NoError(t, err)

	for _, fullMethod := range sdkAndCLIAgentRPCs {
		require.True(t, allowlist.mayCall(fullMethod), "agent must be able to call %s", fullMethod)
	}

	unary, stream, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), true)
	require.NoError(t, err)

	agentAuth := "Bearer " + agentASecret
	for _, fullMethod := range sdkAndCLIAgentRPCs {
		t.Run(fullMethod, func(t *testing.T) {
			t.Parallel()

			ctx := contextWithAuthorization(t, agentAuth)

			unaryCalled := false
			_, err := unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: fullMethod},
				func(context.Context, any) (any, error) {
					unaryCalled = true
					return nil, nil
				})
			require.NoError(t, err)
			require.True(t, unaryCalled)

			streamCalled := false
			err = stream(nil, fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: fullMethod},
				func(any, grpc.ServerStream) error {
					streamCalled = true
					return nil
				})
			require.NoError(t, err)
			require.True(t, streamCalled)
		})
	}
}

// TestAgentAllowlist_StreamDenied checks that a denied streaming RPC is refused
// before its handler runs, and an allowed one is not.
func TestAgentAllowlist_StreamDenied(t *testing.T) {
	t.Parallel()

	_, stream, err := Interceptors(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), true)
	require.NoError(t, err)

	for _, tc := range []struct {
		method   string
		wantCode codes.Code
	}{
		{method: gitalypb.RepositoryService_FetchBundle_FullMethodName, wantCode: codes.PermissionDenied},
		{method: gitalypb.ObjectService_GetObjects_FullMethodName, wantCode: codes.OK},
	} {
		t.Run(tc.method, func(t *testing.T) {
			t.Parallel()

			ctx := contextWithAuthorization(t, "Bearer "+agentASecret)
			called := false
			err := stream(nil, fakeServerStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: tc.method},
				func(any, grpc.ServerStream) error {
					called = true
					return nil
				})

			require.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
			require.Equal(t, tc.wantCode == codes.OK, called)
		})
	}
}

// TestAgentAllowlist_ExtraRPCs checks that [graft] agent_rpcs extends the
// allowlist for agents, and that the extra RPC stays subject to the
// never-allowed denials.
func TestAgentAllowlist_ExtraRPCs(t *testing.T) {
	t.Parallel()

	const extra = "/gitaly.RepositoryService/RepositorySize"

	unary, _, err := InterceptorsWithAgentRPCs(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), []string{extra}, true)
	require.NoError(t, err)

	for _, tc := range []struct {
		desc     string
		method   string
		wantCode codes.Code
	}{
		{desc: "extra RPC is allowed", method: extra, wantCode: codes.OK},
		{desc: "RPC that is not extra stays denied", method: gitalypb.RepositoryService_CreateFork_FullMethodName, wantCode: codes.PermissionDenied},
		{desc: "built-in RPC stays allowed", method: gitalypb.ObjectService_GetTree_FullMethodName, wantCode: codes.OK},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			called := false
			_, err := unary(contextWithAuthorization(t, "Bearer "+agentBSecret), nil,
				&grpc.UnaryServerInfo{FullMethod: tc.method},
				func(context.Context, any) (any, error) {
					called = true
					return nil, nil
				})

			require.Equal(t, tc.wantCode, status.Code(err), "error: %v", err)
			require.Equal(t, tc.wantCode == codes.OK, called)
		})
	}

	// The extra RPCs apply to the agents only; the shared token is not restricted.
	sharedUnary, _, err := InterceptorsWithAgentRPCs(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), nil, true)
	require.NoError(t, err)

	called := false
	_, err = sharedUnary(contextWithAuthorization(t, v2Authorization(sharedSecretTest, time.Now())), nil,
		&grpc.UnaryServerInfo{FullMethod: gitalypb.RepositoryService_RemoveRepository_FullMethodName},
		func(context.Context, any) (any, error) {
			called = true
			return nil, nil
		})
	require.NoError(t, err)
	require.True(t, called)
}

// TestNewAgentAllowlist_Validation checks the [graft] agent_rpcs entries.
func TestNewAgentAllowlist_Validation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		desc    string
		rpcs    []string
		wantErr string
	}{
		{desc: "no entries", rpcs: nil},
		{desc: "registered RPC", rpcs: []string{"/gitaly.RepositoryService/RepositorySize"}},
		{desc: "duplicate of a built-in RPC", rpcs: []string{gitalypb.ObjectService_GetTree_FullMethodName}},
		{desc: "missing leading slash", rpcs: []string{"gitaly.RepositoryService/RepositorySize"}, wantErr: "not a full gRPC method name"},
		{desc: "service only", rpcs: []string{"/gitaly.RepositoryService"}, wantErr: "not a full gRPC method name"},
		{desc: "empty method", rpcs: []string{"/gitaly.RepositoryService/"}, wantErr: "not a full gRPC method name"},
		{desc: "empty service", rpcs: []string{"//RepositorySize"}, wantErr: "not a full gRPC method name"},
		{desc: "too many separators", rpcs: []string{"/gitaly.RepositoryService/RepositorySize/x"}, wantErr: "not a full gRPC method name"},
		{desc: "unregistered method", rpcs: []string{"/gitaly.RepositoryService/NoSuchRPC"}, wantErr: "not a registered gRPC method"},
		{desc: "unregistered service", rpcs: []string{"/gitaly.NoSuchService/RepositorySize"}, wantErr: "not a registered gRPC method"},
		{desc: "write references", rpcs: []string{gitalypb.RepositoryService_WriteRef_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "delete references", rpcs: []string{gitalypb.RefService_DeleteRefs_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "remove repository", rpcs: []string{gitalypb.RepositoryService_RemoveRepository_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "create repository", rpcs: []string{gitalypb.RepositoryService_CreateRepository_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "fork repository", rpcs: []string{gitalypb.RepositoryService_CreateFork_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "create repository from URL", rpcs: []string{gitalypb.RepositoryService_CreateRepositoryFromURL_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "create repository from bundle", rpcs: []string{gitalypb.RepositoryService_CreateRepositoryFromBundle_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "create repository from snapshot", rpcs: []string{gitalypb.RepositoryService_CreateRepositoryFromSnapshot_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "backup repository", rpcs: []string{gitalypb.RepositoryService_BackupRepository_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "ssh upload-pack is opt-in only", rpcs: []string{gitalypb.SSHService_SSHUploadPackWithSidechannel_FullMethodName}},
		{desc: "ssh receive-pack is opt-in only", rpcs: []string{gitalypb.SSHService_SSHReceivePack_FullMethodName}},
		{desc: "hook callback", rpcs: []string{gitalypb.HookService_PreReceiveHook_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "object pool", rpcs: []string{gitalypb.ObjectPoolService_DeleteObjectPool_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "cleanup service", rpcs: []string{gitalypb.CleanupService_RewriteHistory_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "partition service", rpcs: []string{gitalypb.PartitionService_BackupPartition_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "storage core garbage collection", rpcs: []string{gitalypb.StorageCoreService_CollectGarbage_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "storage core state", rpcs: []string{gitalypb.StorageCoreService_GetStorageState_FullMethodName}, wantErr: "may never be called by an agent"},
		{desc: "one bad entry among good ones", rpcs: []string{"/gitaly.RepositoryService/RepositorySize", gitalypb.RepositoryService_FetchRemote_FullMethodName}, wantErr: "entry 2"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			t.Parallel()

			allowlist, err := newAgentAllowlist(tc.rpcs)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.Nil(t, allowlist)
				return
			}

			require.NoError(t, err)
			for _, rpc := range tc.rpcs {
				require.True(t, allowlist.mayCall(rpc), rpc)
			}
		})
	}
}

// TestInterceptorsWithAgentRPCs_InvalidConfigFailsOnBothServers checks that an
// invalid agent_rpcs entry is reported at startup on the internal and the
// external server, and even without agents.
func TestInterceptorsWithAgentRPCs_InvalidConfigFailsOnBothServers(t *testing.T) {
	t.Parallel()

	for _, external := range []bool{true, false} {
		for _, agents := range [][]config.GraftAgent{nil, testAgents()} {
			_, _, err := InterceptorsWithAgentRPCs(gitalycfgauth.Config{}, agents,
				[]string{gitalypb.RepositoryService_WriteRef_FullMethodName}, external)
			require.ErrorContains(t, err, "may never be called by an agent")
		}
	}
}

// TestAgentAllowlist_ConcurrentDecisions runs many agent calls, allowed and
// denied, at the same time. It is meant for the race detector: the allowlist
// is shared by every request and must not be written to.
func TestAgentAllowlist_ConcurrentDecisions(t *testing.T) {
	t.Parallel()

	unary, _, err := InterceptorsWithAgentRPCs(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(),
		[]string{"/gitaly.RepositoryService/RepositorySize"}, true)
	require.NoError(t, err)

	methods := []struct {
		name     string
		wantCode codes.Code
	}{
		{name: gitalypb.ObjectService_GetObjects_FullMethodName, wantCode: codes.OK},
		{name: "/gitaly.RepositoryService/RepositorySize", wantCode: codes.OK},
		{name: gitalypb.RepositoryService_WriteRef_FullMethodName, wantCode: codes.PermissionDenied},
		{name: gitalypb.HookService_PreReceiveHook_FullMethodName, wantCode: codes.PermissionDenied},
	}

	const workers = 40
	const iterations = 25

	var wg sync.WaitGroup
	errs := make(chan error, workers*iterations)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			for j := 0; j < iterations; j++ {
				m := methods[(i+j)%len(methods)]
				ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+agentASecret))

				_, err := unary(ctx, nil, &grpc.UnaryServerInfo{FullMethod: m.name},
					func(context.Context, any) (any, error) { return nil, nil })
				if status.Code(err) != m.wantCode {
					errs <- err
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		require.Failf(t, "unexpected result", "error: %v", err)
	}
}

// sdkCLIDriftAllowance lists RPCs that the SDK or the CLI source calls although
// agents may not call them. Each entry is a known gap in the caller that the
// owner must close. The test fails when an entry is no longer called, so the
// entry is removed together with the fix. The CLI deletes its temporary branches
// with OperationService.UserDeleteBranch, so the list is empty.
var sdkCLIDriftAllowance = map[string]string{}

// operatorOnlySources lists the SDK and CLI files whose calls are operator actions
// rather than agent actions, with the RPCs they may call outside the agent
// allowlist: a whole gRPC service ("gitaly.StorageCoreService") or one method
// ("gitaly.RepositoryService/CreateRepository"). The storage core's administration
// (`graft storage import|state|verify`, sdk/storage.go) runs with the server's shared
// token (doc/graft/STORAGE.md), so agents are not meant to be allowed its RPCs. So
// does the creation of a repository (`graft repo create`, sdk/repository.go): agents
// may never create one, and its exact checks read the object format and whether a
// reference exists. The scan checks these files separately: every RPC they call must
// be agent-allowed or listed here, and each entry must still be called (so an entry
// cannot go stale).
var operatorOnlySources = map[string][]string{
	"sdk/storage.go": {"gitaly.StorageCoreService"},
	"sdk/repository.go": {
		"gitaly.RepositoryService/CreateRepository",
		"gitaly.RepositoryService/ObjectFormat",
		"gitaly.RefService/RefExists",
	},
}

// operatorScopeCovers reports whether fullMethod is the method, or a method of the
// service, that scope names in operatorOnlySources.
func operatorScopeCovers(scope, fullMethod string) bool {
	if strings.Contains(scope, "/") {
		return fullMethod == "/"+scope
	}
	return strings.HasPrefix(fullMethod, "/"+scope+"/")
}

// sourceMethodCall matches a Go method call such as `refs.DeleteRefs(ctx, ...)`
// and captures the method name.
var sourceMethodCall = regexp.MustCompile(`\.([A-Z][A-Za-z0-9]*)\(`)

// TestAgentAllowlist_SourceScanMatchesSDKAndCLI guards the built-in allowlist
// against drift from the callers. It collects every method called in the SDK
// and CLI sources and checks that each Gitaly RPC of that name is either
// allowed for agents or listed in sdkCLIDriftAllowance. The names come from
// the source text, so a new call in sdk/ or cmd/graft is checked without a
// hand-written list.
func TestAgentAllowlist_SourceScanMatchesSDKAndCLI(t *testing.T) {
	t.Parallel()

	allowlist, err := newAgentAllowlist(nil)
	require.NoError(t, err)

	// The test runs in internal/gitaly/server/auth, four levels below the root.
	const root = "../../../.."
	called, operatorCalls := sourceMethodCalls(t, root, []string{"sdk", "cmd/graft"})
	require.NotEmpty(t, called)

	methodsByName := map[string][]string{}
	for _, fullMethod := range registeredFullMethods(t) {
		if !strings.HasPrefix(fullMethod, "/gitaly.") {
			continue
		}

		name := fullMethod[strings.LastIndex(fullMethod, "/")+1:]
		methodsByName[name] = append(methodsByName[name], fullMethod)
	}

	for file, scopes := range operatorOnlySources {
		used := map[string]bool{}
		for name := range operatorCalls[file] {
			for _, fullMethod := range methodsByName[name] {
				covered := false
				for _, scope := range scopes {
					if operatorScopeCovers(scope, fullMethod) {
						used[scope], covered = true, true
					}
				}
				if covered {
					continue
				}
				require.True(t, allowlist.mayCall(fullMethod),
					"%s is an operator-only source for %v but also calls %s, which agents may not call", file, scopes, fullMethod)
			}
		}
		for _, scope := range scopes {
			require.True(t, used[scope], "%s is in operatorOnlySources for %s but no longer calls it; remove the entry", file, scope)
		}
	}

	var denied []string
	drift := map[string]bool{}
	for name := range called {
		for _, fullMethod := range methodsByName[name] {
			if allowlist.mayCall(fullMethod) {
				continue
			}

			if _, known := sdkCLIDriftAllowance[fullMethod]; known {
				drift[fullMethod] = true
				continue
			}

			denied = append(denied, fullMethod)
		}
	}

	sort.Strings(denied)
	require.Empty(t, denied, "the SDK or the CLI calls RPCs that agents may not call")

	for fullMethod := range sdkCLIDriftAllowance {
		require.True(t, drift[fullMethod], "%s is in sdkCLIDriftAllowance but no caller uses it any more; remove the entry", fullMethod)
	}
}

// sourceMethodCalls returns the method names called in the non-test Go files
// below dirs (relative to root): those of the agent-facing files, and, per file,
// those of the files in operatorOnlySources.
func sourceMethodCalls(t *testing.T, root string, dirs []string) (map[string]struct{}, map[string]map[string]struct{}) {
	t.Helper()

	calls := map[string]struct{}{}
	operatorCalls := map[string]map[string]struct{}{}
	for _, dir := range dirs {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)

			target := calls
			if _, operator := operatorOnlySources[rel]; operator {
				target = map[string]struct{}{}
				operatorCalls[rel] = target
			}
			for _, match := range sourceMethodCall.FindAllStringSubmatch(string(content), -1) {
				target[match[1]] = struct{}{}
			}

			return nil
		}))
	}

	return calls, operatorCalls
}
