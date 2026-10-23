package gittest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/feder-cr/git-ai/internal/git/gitcmd"
	"github.com/feder-cr/git-ai/internal/gitaly/config"
	"github.com/feder-cr/git-ai/internal/gitaly/storage/mode"
	"github.com/feder-cr/git-ai/internal/testhelper"
	"github.com/stretchr/testify/require"
)

// fanOutRemovalShim is an LD_PRELOAD library whose mkdir removes a loose object directory
// (objects/xx) of a repository right after creating it, as housekeeping's git prune-packed or git
// prune may at that moment: git's create_tmpfile then fails its single retry. Directories under any
// other parent than one named "objects", such as those of a quarantine, are left alone.
const fanOutRemovalShim = `#define _GNU_SOURCE
#include <ctype.h>
#include <dlfcn.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

int mkdir(const char *path, mode_t mode) {
	int (*real_mkdir)(const char *, mode_t) = dlsym(RTLD_NEXT, "mkdir");
	int ret = real_mkdir(path, mode);
	size_t len = strlen(path);
	if (ret == 0 && len >= 10 && strncmp(path + len - 10, "objects/", 8) == 0 &&
	    (len == 10 || path[len - 11] == '/') &&
	    isxdigit((unsigned char)path[len - 2]) && isxdigit((unsigned char)path[len - 1]))
		rmdir(path);
	return ret;
}
`

// NewFanOutRemovingCommandFactory returns a command factory whose Git commands lose every loose
// object directory they create in a repository's object directory right after creating it. A Git
// command that writes a loose object straight into a repository fails with "unable to create
// temporary file"; one that writes into a quarantine, which Migrate links into the repository,
// does not. The library is compiled with cc, and checked to take effect before it is returned.
func NewFanOutRemovingCommandFactory(tb testing.TB, ctx context.Context, cfg config.Cfg) *InterceptingCommandFactory {
	tb.Helper()

	dir := testhelper.TempDir(tb)
	source := filepath.Join(dir, "fanout_removal.c")
	library := filepath.Join(dir, "fanout_removal.so")
	require.NoError(tb, os.WriteFile(source, []byte(fanOutRemovalShim), mode.File))
	if out, err := exec.Command("cc", "-shared", "-fPIC", "-o", library, source, "-ldl").CombinedOutput(); err != nil {
		require.FailNow(tb, "compiling the fan-out removal library", "%v: %s", err, out)
	}

	factory := NewInterceptingCommandFactory(tb, ctx, cfg, func(execEnv gitcmd.ExecutionEnvironment) string {
		return fmt.Sprintf("#!/usr/bin/env bash\nLD_PRELOAD=%q exec %q \"$@\"\n", library, execEnv.BinaryPath)
	})

	// The library must take effect, or a test using it would pass without exercising anything.
	repo, _ := CreateRepository(tb, ctx, cfg, CreateRepositoryConfig{SkipCreationViaService: true})
	var stderr bytes.Buffer
	cmd, err := factory.New(ctx, repo, gitcmd.Command{
		Name:  "hash-object",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "-w"}, gitcmd.Flag{Name: "--stdin"}},
	}, gitcmd.WithStdin(bytes.NewReader([]byte("fan-out removal check"))), gitcmd.WithStderr(&stderr))
	require.NoError(tb, err)
	require.Error(tb, cmd.Wait(), "the fan-out removal library does not take effect")
	require.Contains(tb, stderr.String(), "unable to create temporary file")

	return factory
}
