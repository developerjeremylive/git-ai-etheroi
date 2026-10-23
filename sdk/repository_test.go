package sdk

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/feder-cr/git-ai/proto/go/gitalypb"
)

func TestObjectFormat(t *testing.T) {
	t.Parallel()

	for format, expected := range map[string]gitalypb.ObjectFormat{
		"":       gitalypb.ObjectFormat_OBJECT_FORMAT_UNSPECIFIED,
		"sha1":   gitalypb.ObjectFormat_OBJECT_FORMAT_SHA1,
		"sha256": gitalypb.ObjectFormat_OBJECT_FORMAT_SHA256,
	} {
		got, err := objectFormat(format)
		require.NoError(t, err, format)
		require.Equal(t, expected, got, format)
	}

	_, err := objectFormat("SHA1")
	require.Equal(t, codes.InvalidArgument, Code(err))
}

// TestCreateRejectsAnUnknownObjectFormat checks that Create refuses an unknown
// object format before it contacts the server, which the address of the client
// does not lead to.
func TestCreateRejectsAnUnknownObjectFormat(t *testing.T) {
	t.Parallel()

	client, err := Dial(context.Background(), "unix:///nonexistent/graft.sock")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	err = client.Repo("a.git").Create(context.Background(), CreateOptions{ObjectFormat: "md5"})
	require.Equal(t, codes.InvalidArgument, Code(err))
	require.Contains(t, err.Error(), `object format "md5": want sha1 or sha256`)
}

func TestIsAlreadyExists(t *testing.T) {
	t.Parallel()

	require.True(t, IsAlreadyExists(status.Error(codes.AlreadyExists, "repository exists already")))
	require.False(t, IsAlreadyExists(status.Error(codes.NotFound, "x")))
	require.False(t, IsAlreadyExists(nil))
}
