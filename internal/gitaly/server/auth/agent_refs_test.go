package auth

import (
	"context"
	"sort"
	"strings"
	"testing"

	gitalycfgauth "github.com/feder-cr/git-ai/internal/gitaly/config/auth"
	"github.com/feder-cr/git-ai/internal/graft/identity"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// TestAgentRefFields_everyAllowedRPCIsClassified: every string, bytes, bool or enum field of the
// requests and responses of every RPC agents may call by default, at any depth, is classified by
// the exact table of agentRefField, by message and field: as a reference or not, or as a flag that
// walks every reference. A new RPC or field fails here until it is classified, so the filter cannot
// miss it, nor a new field that a generic name would have passed.
func TestAgentRefFields_everyAllowedRPCIsClassified(t *testing.T) {
	t.Parallel()

	var unclassified []string
	for method := range builtinAgentRPCs {
		service, name, ok := splitFullMethod(method)
		require.True(t, ok, method)
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(service))
		require.NoError(t, err, method)
		md := d.(protoreflect.ServiceDescriptor).Methods().ByName(protoreflect.Name(name))
		require.NotNil(t, md, method)
		for _, root := range []protoreflect.MessageDescriptor{md.Input(), md.Output()} {
			seen := map[protoreflect.FullName]bool{}
			var walk func(protoreflect.MessageDescriptor)
			walk = func(m protoreflect.MessageDescriptor) {
				if seen[m.FullName()] {
					return
				}
				seen[m.FullName()] = true
				for i := 0; i < m.Fields().Len(); i++ {
					fd := m.Fields().Get(i)
					switch fd.Kind() {
					case protoreflect.StringKind, protoreflect.BytesKind, protoreflect.BoolKind, protoreflect.EnumKind:
						if _, exact := agentRefField(fd); !exact {
							unclassified = append(unclassified, string(m.FullName())+"."+string(fd.Name())+" ("+method+")")
						}
					case protoreflect.MessageKind, protoreflect.GroupKind:
						if fd.IsMap() {
							// The filter does not look into maps: a map field of an agent RPC must be
							// classified, and given its handling, when one appears.
							unclassified = append(unclassified, string(fd.FullName())+" (map field, "+method+")")
							continue
						}
						walk(fd.Message())
					}
				}
			}
			walk(root)
		}
	}
	sort.Strings(unclassified)
	require.Empty(t, unclassified, "fields of agent RPCs that agentRefField does not classify")
}

func agentContext() context.Context {
	return identity.NewContext(context.Background(), identity.Identity{Name: "agent-a", Source: identity.SourceAgent})
}

// TestCheckAgentRefs: an agent may not name a reference under refs/graft/, in full, through git's
// revision rules (graft/...), or in a pattern; paths, contents and branch names that only look
// alike pass, and callers that are not agents are not restricted.
func TestCheckAgentRefs(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		req    proto.Message
		denied bool
	}{
		"ListRefs pattern under refs/graft":   {&gitalypb.ListRefsRequest{Patterns: [][]byte{[]byte("refs/heads/"), []byte("refs/graft/")}}, true},
		"ListRefs pattern of every ref":       {&gitalypb.ListRefsRequest{Patterns: [][]byte{[]byte("refs/")}}, false},
		"FindCommit by the short name":        {&gitalypb.FindCommitRequest{Revision: []byte("graft/green/main")}, true},
		"FindCommit by a range":               {&gitalypb.FindCommitRequest{Revision: []byte("main..refs/graft/green/main")}, true},
		"FindCommit of a branch":              {&gitalypb.FindCommitRequest{Revision: []byte("refs/heads/graft/x")}, false},
		"GetBlobs of a proposal by reference": {&gitalypb.GetBlobsRequest{RevisionPaths: []*gitalypb.GetBlobsRequest_RevisionPath{{Revision: "refs/graft/proposals/0123456789abcdef/1", Path: []byte("a")}}}, true},
		"GetBlobs of a path named graft":      {&gitalypb.GetBlobsRequest{RevisionPaths: []*gitalypb.GetBlobsRequest_RevisionPath{{Revision: "main", Path: []byte("graft/x.go")}}}, false},
		"TreeEntry of a proposal":             {&gitalypb.TreeEntryRequest{Revision: []byte("refs/graft/proposals/0123456789abcdef/1"), Path: []byte("a")}, true},
		"FindRefsByOID pattern":               {&gitalypb.FindRefsByOIDRequest{Oid: "0123", RefPatterns: []string{"refs/graft/green"}}, true},
		"FindBranch of a full graft name":     {&gitalypb.FindBranchRequest{Name: []byte("refs/graft/green/main")}, true},
		"FindBranch of a branch named graft/": {&gitalypb.FindBranchRequest{Name: []byte("graft/x")}, false},
		"UserCommitFiles content":             {&gitalypb.UserCommitFilesRequest{UserCommitFilesRequestPayload: &gitalypb.UserCommitFilesRequest_Action{Action: &gitalypb.UserCommitFilesAction{UserCommitFilesActionPayload: &gitalypb.UserCommitFilesAction_Content{Content: []byte("see refs/graft/green/main")}}}}, false},
		"GetObjects of an object ID":          {&gitalypb.GetObjectsRequest{Oids: []string{"0123456789abcdef0123456789abcdef01234567"}}, false},
		"ListCommits, a name after a line":    {&gitalypb.ListCommitsRequest{Revisions: []string{"--glob=x\nrefs/graft/proposals/0123456789abcdef/1"}}, true},
		"ListCommits, a short name after one": {&gitalypb.ListCommitsRequest{Revisions: []string{"--branches=x\ngraft/proposals/0123456789abcdef/1"}}, true},
		"ListBlobs, a name after a line":      {&gitalypb.ListBlobsRequest{Revisions: []string{"--glob=x\nrefs/graft/green/main"}}, true},
		"ListCommits, a name after a CR":      {&gitalypb.ListCommitsRequest{Revisions: []string{"--glob=x\rrefs/graft/green/main"}}, true},
		"ListCommits, a name after a NUL":     {&gitalypb.ListCommitsRequest{Revisions: []string{"--glob=x\x00refs/graft/green/main"}}, true},
		"GetArchive of a proposal":            {&gitalypb.GetArchiveRequest{CommitId: "refs/graft/proposals/0123456789abcdef/1"}, true},
		"GetArchive of a branch":              {&gitalypb.GetArchiveRequest{CommitId: "main"}, false},
	} {
		err := checkAgentRefs(agentContext(), tc.req)
		if tc.denied {
			require.Equal(t, codes.PermissionDenied, status.Code(err), "%s: %v", name, err)
			require.ErrorContains(t, err, "internal to the server", name)
		} else {
			require.NoError(t, err, name)
		}
		require.NoError(t, checkAgentRefs(context.Background(), tc.req), "%s: not an agent", name)
	}
}

// TestFilterAgentRefs: a response to an agent loses every reference under refs/graft/ it lists;
// a response to another caller is unchanged.
func TestFilterAgentRefs(t *testing.T) {
	t.Parallel()

	refs := func() *gitalypb.ListRefsResponse {
		return &gitalypb.ListRefsResponse{References: []*gitalypb.ListRefsResponse_Reference{
			{Name: []byte("refs/heads/main"), Target: "1"},
			{Name: []byte("refs/graft/green/main"), Target: "1"},
			{Name: []byte("refs/graft/proposals/0123456789abcdef/1"), Target: "2"},
		}}
	}
	r := refs()
	filterAgentRefs(agentContext(), r)
	require.Len(t, r.GetReferences(), 1)
	require.Equal(t, "refs/heads/main", string(r.GetReferences()[0].GetName()))
	r = refs()
	filterAgentRefs(context.Background(), r)
	require.Len(t, r.GetReferences(), 3, "not an agent")

	byOID := &gitalypb.FindRefsByOIDResponse{Refs: []string{"refs/heads/a", "refs/graft/green/main", "refs/tags/v1"}}
	filterAgentRefs(agentContext(), byOID)
	require.Equal(t, []string{"refs/heads/a", "refs/tags/v1"}, byOID.GetRefs())

	commits := &gitalypb.FindCommitsResponse{Commits: []*gitalypb.GitCommit{{Id: "1", ReferencedBy: [][]byte{[]byte("refs/heads/a"), []byte("refs/graft/green/main")}}}}
	filterAgentRefs(agentContext(), commits)
	require.Len(t, commits.GetCommits(), 1, "a commit is kept; only the hidden name goes")
	require.Equal(t, [][]byte{[]byte("refs/heads/a")}, commits.GetCommits()[0].GetReferencedBy())

	def := &gitalypb.FindDefaultBranchNameResponse{Name: []byte("refs/graft/green/main")}
	filterAgentRefs(agentContext(), def)
	require.Empty(t, def.GetName(), "a top-level hidden name is cleared")
}

// recordingServerStream records what a handler sends.
type recordingServerStream struct {
	grpc.ServerStream
	ctx  context.Context
	sent []proto.Message
}

func (s *recordingServerStream) Context() context.Context { return s.ctx }

func (s *recordingServerStream) SendMsg(m any) error {
	s.sent = append(s.sent, proto.Clone(m.(proto.Message)))
	return nil
}

// TestAgentRefFilter_throughTheInterceptors: the guard runs on the interceptors of a real agent
// configuration: a unary request that names refs/graft/ is refused before its handler, and the
// responses of a unary and of a streaming RPC to an agent lose the hidden references.
func TestAgentRefFilter_throughTheInterceptors(t *testing.T) {
	t.Parallel()

	unary, stream, err := InterceptorsWithAgentRPCs(gitalycfgauth.Config{Token: sharedSecretTest}, testAgents(), nil, true)
	require.NoError(t, err)
	agentCtx := contextWithAuthorization(t, "Bearer "+agentASecret)

	called := false
	_, err = unary(agentCtx, &gitalypb.FindCommitRequest{Revision: []byte("refs/graft/green/main")},
		&grpc.UnaryServerInfo{FullMethod: gitalypb.CommitService_FindCommit_FullMethodName},
		func(context.Context, any) (any, error) { called = true; return &gitalypb.FindCommitResponse{}, nil })
	require.Equal(t, codes.PermissionDenied, status.Code(err), "err = %v", err)
	require.False(t, called)

	resp, err := unary(agentCtx, &gitalypb.FindRefsByOIDRequest{Oid: "1"},
		&grpc.UnaryServerInfo{FullMethod: gitalypb.RefService_FindRefsByOID_FullMethodName},
		func(context.Context, any) (any, error) {
			return &gitalypb.FindRefsByOIDResponse{Refs: []string{"refs/heads/a", "refs/graft/green/main"}}, nil
		})
	require.NoError(t, err)
	require.Equal(t, []string{"refs/heads/a"}, resp.(*gitalypb.FindRefsByOIDResponse).GetRefs())

	ss := &recordingServerStream{ctx: agentCtx}
	err = stream(nil, ss, &grpc.StreamServerInfo{FullMethod: gitalypb.RefService_ListRefs_FullMethodName},
		func(srv any, s grpc.ServerStream) error {
			return s.SendMsg(&gitalypb.ListRefsResponse{References: []*gitalypb.ListRefsResponse_Reference{
				{Name: []byte("refs/heads/main")}, {Name: []byte("refs/graft/green/main")},
			}})
		})
	require.NoError(t, err)
	require.Len(t, ss.sent, 1)
	var names []string
	for _, r := range ss.sent[0].(*gitalypb.ListRefsResponse).GetReferences() {
		names = append(names, string(r.GetName()))
	}
	require.Equal(t, "refs/heads/main", strings.Join(names, ","))
}

// TestAgentRefFields_entriesExist: every field the exact table classifies exists, so that a typo
// cannot leave a field unclassified; a reference is a string or bytes field, and a walk of every
// reference a bool.
func TestAgentRefFields_entriesExist(t *testing.T) {
	t.Parallel()

	for name, kind := range agentRefFields {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(name)
		require.NoError(t, err, name)
		fd, ok := d.(protoreflect.FieldDescriptor)
		require.True(t, ok, name)
		switch kind {
		case isRef, isBranchName:
			require.Contains(t, []protoreflect.Kind{protoreflect.StringKind, protoreflect.BytesKind}, fd.Kind(), name)
		case walksAllRefs:
			require.Equal(t, protoreflect.BoolKind, fd.Kind(), name)
		}
	}
}

// TestAgentRefField_byMessage: a field is classified by its message, not its name: the same name
// in two messages may differ, and a field an operator's RPC adds falls back to the conservative
// rule by name.
func TestAgentRefField_byMessage(t *testing.T) {
	t.Parallel()

	field := func(message, name string) protoreflect.FieldDescriptor {
		d, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(message))
		require.NoError(t, err, message)
		fd := d.(protoreflect.MessageDescriptor).Fields().ByName(protoreflect.Name(name))
		require.NotNil(t, fd, message+"."+name)
		return fd
	}

	kind, exact := agentRefField(field("gitaly.FindBranchRequest", "name"))
	require.True(t, exact)
	require.Equal(t, isBranchName, kind)
	kind, exact = agentRefField(field("gitaly.User", "name"))
	require.True(t, exact)
	require.Equal(t, notRef, kind)
	kind, exact = agentRefField(field("gitaly.FindCommitsRequest", "all"))
	require.True(t, exact)
	require.Equal(t, walksAllRefs, kind)

	// CountCommitsRequest is no RPC agents call by default: its fields are judged by name.
	kind, exact = agentRefField(field("gitaly.CountCommitsRequest", "revision"))
	require.False(t, exact)
	require.Equal(t, isRef, kind)
	_, exact = agentRefField(field("gitaly.CountCommitsRequest", "all"))
	require.False(t, exact)
}
