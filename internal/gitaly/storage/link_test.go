package storage

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

func TestLinkRecreatingDirectory(t *testing.T) {
	t.Parallel()

	setup := func(t *testing.T) (string, string) {
		root := t.TempDir()
		source := filepath.Join(root, "source")
		require.NoError(t, os.WriteFile(source, []byte("object"), mode.File))
		return source, filepath.Join(root, "objects", "0a", "object")
	}

	t.Run("a directory that vanishes again is created again until the link holds", func(t *testing.T) {
		t.Parallel()

		source, target := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(target), mode.Directory))

		links := 0
		require.NoError(t, LinkRecreatingDirectory(testhelper.Context(t), source, target, func() error {
			links++
			if links <= 3 {
				require.NoError(t, os.Remove(filepath.Dir(target)))
			}
			return os.Link(source, target)
		}))
		require.Equal(t, 4, links)
		require.FileExists(t, target)
	})

	t.Run("a missing source fails at once", func(t *testing.T) {
		t.Parallel()

		source, target := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(target), mode.Directory))
		require.NoError(t, os.Remove(source))

		links := 0
		err := LinkRecreatingDirectory(testhelper.Context(t), source, target, func() error {
			links++
			return os.Link(source, target)
		})
		require.ErrorIs(t, err, os.ErrNotExist)
		require.ErrorContains(t, err, "(source: ")
		require.Equal(t, 1, links)
	})

	t.Run("a directory replaced by a dangling symbolic link fails at once", func(t *testing.T) {
		t.Parallel()

		source, target := setup(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Dir(target)), mode.Directory))
		require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Dir(target)))

		links := 0
		err := LinkRecreatingDirectory(testhelper.Context(t), source, target, func() error {
			links++
			return os.Link(source, target)
		})
		require.ErrorIs(t, err, os.ErrNotExist)
		require.ErrorContains(t, err, "is no directory")
		require.Equal(t, 1, links)
	})
}
