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

// fanOutRemovalShim is an LD_PRELOAD library whose mkdir removes a loose object directory (xx, two
// hex digits) of the object directory GITTEST_FANOUT_REMOVAL_DIR right after creating it, as
// housekeeping's git prune-packed or git prune may at that moment: git's create_tmpfile then fails
// its single retry. Directories anywhere else, such as those of a quarantine, are left alone.
const fanOutRemovalShim = `#define _GNU_SOURCE
#include <ctype.h>
#include <dlfcn.h>
#include <limits.h>
#include <stdlib.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

int mkdir(const char *path, mode_t mode) {
	int (*real_mkdir)(const char *, mode_t) = dlsym(RTLD_NEXT, "mkdir");
	int ret = real_mkdir(path, mode);
	const char *target = getenv("GITTEST_FANOUT_REMOVAL_DIR");
	const char *base = strrchr(path, '/');
	char parent[PATH_MAX], resolved[PATH_MAX];

	base = base ? base + 1 : path;
	if (ret != 0 || !target || strlen(base) != 2 ||
	    !isxdigit((unsigned char)base[0]) || !isxdigit((unsigned char)base[1]))
		return ret;
	if (base == path) {
		strcpy(parent, ".");
	} else {
		size_t len = base - 1 - path;
		if (len >= sizeof(parent))
			return ret;
		memcpy(parent, path, len);
		parent[len] = '\0';
	}
	if (realpath(parent[0] ? parent : "/", resolved) && strcmp(resolved, target) == 0)
		rmdir(path);
	return ret;
}
`

// NewFanOutRemovingCommandFactory returns a command factory whose Git commands, once armed for a
// repository, lose every loose object directory they create in that repository's object directory
// right after creating it. A Git command that writes a loose object straight into the repository
// then fails with "unable to create temporary file"; one that writes into a quarantine, which
// Migrate links into the repository, or that keeps a pack, does not.
//
// arm packs the repository at repoPath, removes its loose object directories as housekeeping does,
// so that every loose object written afterwards needs its directory created, and arms the
// commands for it. Before, the commands run as usual, so that a test sets up its repositories
// through the server. The library is compiled with cc, and checked to take effect before it is
// returned: without cc, or where the dynamic loader does not honour LD_PRELOAD (Linux does), the
// test fails rather than pass without exercising anything.
func NewFanOutRemovingCommandFactory(tb testing.TB, ctx context.Context, cfg config.Cfg) (factory *InterceptingCommandFactory, arm func(repoPath string)) {
	tb.Helper()

	dir := testhelper.TempDir(tb)
	source := filepath.Join(dir, "fanout_removal.c")
	library := filepath.Join(dir, "fanout_removal.so")
	armed := filepath.Join(dir, "armed")
	require.NoError(tb, os.WriteFile(source, []byte(fanOutRemovalShim), mode.File))
	if out, err := exec.Command("cc", "-shared", "-fPIC", "-o", library, source, "-ldl").CombinedOutput(); err != nil {
		require.FailNow(tb, "compiling the fan-out removal library", "%v: %s", err, out)
	}

	arm = func(repoPath string) {
		tb.Helper()

		Exec(tb, cfg, "-C", repoPath, "repack", "-a", "-d")
		Exec(tb, cfg, "-C", repoPath, "prune-packed")
		fanOut, err := filepath.Glob(filepath.Join(repoPath, "objects", "[0-9a-f][0-9a-f]"))
		require.NoError(tb, err)
		require.Empty(tb, fanOut, "loose objects left: are they unreachable?")

		objectDir, err := filepath.EvalSymlinks(filepath.Join(repoPath, "objects"))
		require.NoError(tb, err)
		require.NoError(tb, os.WriteFile(armed, []byte(objectDir), mode.File))
	}

	factory = NewInterceptingCommandFactory(tb, ctx, cfg, func(execEnv gitcmd.ExecutionEnvironment) string {
		return fmt.Sprintf(`#!/usr/bin/env bash
if test -e %[1]q
then
	export GITTEST_FANOUT_REMOVAL_DIR="$(cat %[1]q)" LD_PRELOAD=%[2]q${LD_PRELOAD:+:$LD_PRELOAD}
fi
exec %[3]q "$@"
`, armed, library, execEnv.BinaryPath)
	})

	// The library must take effect, or a test using it would pass without exercising anything.
	repo, repoPath := CreateRepository(tb, ctx, cfg, CreateRepositoryConfig{SkipCreationViaService: true})
	arm(repoPath)
	var stderr bytes.Buffer
	cmd, err := factory.New(ctx, repo, gitcmd.Command{
		Name:  "hash-object",
		Flags: []gitcmd.Option{gitcmd.Flag{Name: "-w"}, gitcmd.Flag{Name: "--stdin"}},
	}, gitcmd.WithStdin(bytes.NewReader([]byte("fan-out removal check"))), gitcmd.WithStderr(&stderr))
	require.NoError(tb, err)
	require.Error(tb, cmd.Wait(), "the fan-out removal library does not take effect")
	require.Contains(tb, stderr.String(), "unable to create temporary file")
	require.NoError(tb, os.Remove(armed))

	return factory, arm
}
