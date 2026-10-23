# Graft quickstart

This walks through a single-machine setup: one standalone Gitaly, two agents
with different policy rules, a `graft-cache` daemon in front of it, and the
`graft` CLI as an agent workspace. It takes about ten minutes by hand.

For requests made with an agent token, the policy decides on the agent of the
token, so one agent cannot act as the other. Read
[LIMITATIONS.md](LIMITATIONS.md), Security limitations, before you give tokens
to anything you do not control.

[`examples/graft/quickstart.sh`](../../examples/graft/quickstart.sh) runs the
same steps non-interactively, under one work directory, and is the fastest way
to see Graft work. The steps below explain what the script does.

## 1. Build the binaries

From a checkout of this repository, with the Gitaly build dependencies
installed (see "Installation" in the Gitaly part of the [README](../../README.md)):

```shell
make build
mkdir -p /var/lib/graft/bin
cp _build/bin/gitaly _build/bin/gitaly-hooks _build/bin/gitaly-ssh /var/lib/graft/bin/
cp _build/bin/graft _build/bin/graft-cache /usr/local/bin/
```

`make build` compiles the bundled Git and embeds it, with `gitaly-hooks` and
`gitaly-ssh`, into the `gitaly` binary. A plain `go build ./cmd/gitaly` does not
build them and fails on a fresh checkout. The example configuration sets
`bin_dir` to `/var/lib/graft/bin`.

## 2. Create the configuration

```shell
mkdir -p /var/lib/graft/repos /var/lib/graft/run /etc/graft/tokens
cp examples/graft/config.toml /etc/graft/config.toml
```

The example defines two agents, `planner` and `builder`, a storage at
`/var/lib/graft/repos`, and a policy. `planner` may land on `main`. Everybody
else may not, and every branch is open to all callers. See
[CONFIGURATION.md](CONFIGURATION.md) for every key.

Create the agent token files that the configuration references:

```shell
for agent in planner builder; do
  openssl rand -hex 32 > "/etc/graft/tokens/$agent"
  chmod 0600 "/etc/graft/tokens/$agent"
done
```

## 3. Start Gitaly in standalone mode

```shell
/var/lib/graft/bin/gitaly serve /etc/graft/config.toml
```

With `[graft] standalone = true`, Gitaly needs no GitLab URL or secret. Every
access check is answered from the policy.

## 4. Create a repository

Creating a repository is an operator action: it needs the shared token of the
`[auth]` section, and an agent token is refused. `--initial-commit` gives the
repository a first commit (a `README.md`) on its default branch, `main`,
because a clone needs an existing branch:

```shell
GRAFT_ADDRESS=unix:/var/lib/graft/run/gitaly.sock \
GRAFT_TOKEN=change-me-shared-token \
GRAFT_USER=planner \
GRAFT_AUTHOR="planner <planner@example.com>" \
graft repo create demo.git --initial-commit --branch feature/demo
```

The initial commit passes the policy like any write, as the user of
`GRAFT_USER`: here `planner`, who may write `main`. `--branch feature/demo`
also creates `feature/demo` at the initial commit, which the builder lands on
below. If the initial commit fails, run the same command again: on a
repository without branches it only adds the commit.

## 5. Start the cache daemon

`graft-cache` sits between clients and Gitaly. Run one daemon per node:

```shell
graft-cache \
  --listen unix:/var/lib/graft/run/cache.sock \
  --upstream unix:/var/lib/graft/run/gitaly.sock \
  --tier daemon \
  --max-bytes 1073741824 \
  --metrics-addr 127.0.0.1:9236
```

The daemon keeps objects in memory only. For a regional tier that also stores
objects on disk, run another `graft-cache --tier cache --disk-dir /var/lib/graft/cache`
whose `--upstream` is the Gitaly socket, and point the daemon's `--upstream` at
it. Tiers chain: a daemon may point at a cache tier, which points at Gitaly.

Clients connect to the daemon's socket. Gitaly's own socket is for the cache
tier and administrative tools.

## 6. Clone and commit as the planner

The CLI reads its settings from the environment. Set `GRAFT_USER` to the agent
name of the token: it names the temporary branches of commits
(`graft/<user>/...`). With an agent token, the server uses the agent name as the
user ID of every request whatever `GRAFT_USER` says, and the policy decides on
the agent of the token. The user ID is used verbatim in the branch name when it
is a valid reference name component, so a policy can grant `refs/heads/graft/<agent>/**`
to each agent. Other names, such as names with spaces, become a lowercase slug
of letters, digits and dashes.

```shell
export GRAFT_ADDRESS=unix:/var/lib/graft/run/cache.sock
export GRAFT_STORAGE=default
export GRAFT_TOKEN="$(cat /etc/graft/tokens/planner)"
export GRAFT_USER=planner
export GRAFT_AUTHOR="planner <planner@example.com>"

graft clone "$GRAFT_ADDRESS" demo.git ./demo --branch main
cd ./demo
printf 'hello graft\n' > README.md
graft commit -m "Add README" --land main
```

`graft commit` writes the commit on a temporary branch,
`refs/heads/graft/<user>/<random>`. `--land main` then lands it on `main` with
`UserLand`. Without `--land`, the commit stays on the temporary branch, and
`graft land` lands it later.

## 7. Land a feature as the builder

The builder may not write to `main`, but it may write to any other branch:

```shell
export GRAFT_TOKEN="$(cat /etc/graft/tokens/builder)"
export GRAFT_USER=builder
export GRAFT_AUTHOR="builder <builder@example.com>"

printf 'feature\n' > feature.txt
graft commit -m "Add feature" --land feature/demo    # allowed
printf 'nope\n' >> README.md
graft commit -m "Denied change" --land main          # refused by the policy
```

The refusal names the rule that decided it, for example `policy denies "builder"
to update "refs/heads/main" (rule 2)`. The commit stays on the builder's
temporary branch, and the land is pending. `graft land --abort` drops the
pending commit and keeps the local changes. `graft revert README.md` takes the
server's version.

## 8. Read history and follow the log

```shell
graft status
graft log                  # changeset log: sequence, reference, OIDs, operation, actor, time
graft blame README.md      # who last changed each line
graft history README.md    # commits that changed the file
graft cat main:README.md   # a file at a revision
graft watch               # follow new changesets until interrupted
```

The log lists every reference update the server accepted, including the
temporary branches. A land appears as `land` next to the `commit_files` and
`delete_refs` entries of its temporary branch. Each entry names the actor.

## 9. Sparse workspaces

Materialize only part of a large tree:

```shell
graft sparse docs/ src/api/
graft update               # moves the base to the branch tip; refuses if local changes touch updated files
```

## 10. Check the cache

With `--metrics-addr` set, the daemon serves its statistics as JSON:

```shell
curl -s http://127.0.0.1:9236/stats
```

`object_hits` grows when a read is served from memory. A second clone of the
same tip is a typical example.

## Next steps

- [ARCHITECTURE.md](ARCHITECTURE.md) explains how the pieces interact.
- [CONFIGURATION.md](CONFIGURATION.md) lists the `[graft]` keys and the CLI settings.
- [LIMITATIONS.md](LIMITATIONS.md) lists what is not done yet.
