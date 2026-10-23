package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
)

// LinkRecreatingDirectory runs link, which links the file at sourcePath to targetPath in a
// directory of an object directory; the caller created the directory of targetPath. Housekeeping
// (git prune-packed, repack -d, prune) removes empty loose object directories at any time, so the
// directory can vanish between its creation and the link. A link that fails with fs.ErrNotExist
// while the source still exists means that the directory is gone: it is created again and the
// link runs again. Once the link holds, the directory is not empty and stays. Each round needs
// another removal between the creation and the link, so the loop ends; it also ends with ctx. A
// directory that exists but is no directory fails the link at once.
//
// The directory is created again with os.Mkdir, not through a transaction's FS. Its creation by
// the caller is recorded already (or it existed in the snapshot), and its removal was no operation
// of the transaction: the recorded operations are the same however many rounds the link took. A
// transaction's snapshot is private, so housekeeping does not remove its directories in the first
// place.
func LinkRecreatingDirectory(ctx context.Context, sourcePath, targetPath string, link func() error) error {
	dir := filepath.Dir(targetPath)
	for {
		err := link()
		if err == nil || !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		if _, statErr := os.Lstat(sourcePath); statErr != nil {
			return fmt.Errorf("%w (source: %w)", err, statErr)
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w (%w)", err, ctxErr)
		}

		// The source exists, so the directory is gone: create it again and link again. If it
		// exists, another writer created it again, unless it is no directory, which no round
		// would change.
		mkdirErr := os.Mkdir(dir, mode.Directory)
		if mkdirErr == nil {
			continue
		}
		if !errors.Is(mkdirErr, fs.ErrExist) {
			return fmt.Errorf("%w (creating %q again: %w)", err, dir, mkdirErr)
		}
		if info, statErr := os.Lstat(dir); statErr == nil && !info.IsDir() {
			return fmt.Errorf("%w (%q is no directory)", err, dir)
		}
	}
}
