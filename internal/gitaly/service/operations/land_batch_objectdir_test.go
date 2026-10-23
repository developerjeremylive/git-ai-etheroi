package operations

import (
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/graft/policystore"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/feder-cr/git-ai/proto/go/gitalypb"
	"github.com/stretchr/testify/require"
)

// TestLandBatchable_requestWithObjectDirectoryLandsAlone checks that a request carrying its own object
// directories does not land through a lane. The quarantine of a batch is built from its first request,
// so the objects that only the request's own directories hold would be missing from the batch.
func TestLandBatchable_requestWithObjectDirectoryLandsAlone(t *testing.T) {
	t.Parallel()

	ctx := testhelper.Context(t)
	// The lanes are only checked for presence: a request that is excluded never reaches the factory.
	s := &Server{landLanes: &landBatchLanes{}, graft: config.Graft{Standalone: true}, graftPolicy: policystore.New(nil)}

	newRequest := func(repo *gitalypb.Repository) *gitalypb.UserLandRequest {
		return &gitalypb.UserLandRequest{
			Repository: repo,
			Branch:     []byte("main"),
		}
	}

	t.Run("object directory", func(t *testing.T) {
		t.Parallel()

		in := newRequest(&gitalypb.Repository{
			StorageName:        "default",
			RelativePath:       "repo.git",
			GitObjectDirectory: "/tmp/quarantine",
		})
		require.False(t, s.landBatchable(ctx, in))
	})

	t.Run("alternate object directories", func(t *testing.T) {
		t.Parallel()

		in := newRequest(&gitalypb.Repository{
			StorageName:                   "default",
			RelativePath:                  "repo.git",
			GitAlternateObjectDirectories: []string{"/tmp/alternate"},
		})
		require.False(t, s.landBatchable(ctx, in))
	})
}
