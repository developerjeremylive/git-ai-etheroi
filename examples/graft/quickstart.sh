#!/usr/bin/env bash
# Non-interactive Graft quickstart: a standalone Gitaly, a graft-cache daemon,
# two agents, a land, a policy denial and reads through the cache. Everything
# runs under one work directory; nothing is installed system-wide.
#
# Usage: examples/graft/quickstart.sh [WORKDIR]
# Requires: the Gitaly build dependencies (Go, make, and what the bundled Git
# needs to compile, see "Installation" in the Gitaly part of README.md),
# openssl, and curl (optional, for the cache statistics).
# WORKDIR defaults to a fresh temporary directory.
#
# The steps mirror doc/graft/QUICKSTART.md. The script assumes a Git checkout
# of this repository: it builds the binaries from it with `make build`, which
# also builds the bundled Git that the gitaly binary embeds. A plain
# `go build ./cmd/gitaly` is not enough for that.

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
work="${1:-$(mktemp -d)}"
mkdir -p "$work"/{bin,repos,run,tokens}

log() { printf '\n==> %s\n' "$*"; }

log "Building gitaly, the bundled Git, graft and graft-cache (make build)"
make -C "$repo_root" build
for binary in gitaly gitaly-hooks gitaly-ssh graft graft-cache; do
  cp "$repo_root/_build/bin/$binary" "$work/bin/"
done

log "Writing agent tokens"
for agent in planner builder; do
  openssl rand -hex 32 > "$work/tokens/$agent"
  chmod 0600 "$work/tokens/$agent"
done

log "Writing configuration"
# The example names /var/lib/graft and /etc/graft; point them at the work directory.
sed \
  -e "s|/var/lib/graft/bin|$work/bin|g" \
  -e "s|/var/lib/graft/run|$work/run|g" \
  -e "s|/var/lib/graft/repos|$work/repos|g" \
  -e "s|/etc/graft/tokens|$work/tokens|g" \
  "$repo_root/examples/graft/config.toml" > "$work/config.toml"

cleanup() {
  for pid in ${cache_pid:-} ${gitaly_pid:-}; do
    kill "$pid" 2>/dev/null || true
  done
}
trap cleanup EXIT

wait_for_socket() {
  local socket="$1"
  for _ in $(seq 100); do
    [ -S "$socket" ] && return 0
    sleep 0.1
  done
  echo "socket $socket did not appear" >&2
  return 1
}

log "Starting Gitaly in standalone mode"
"$work/bin/gitaly" serve "$work/config.toml" &
gitaly_pid=$!
wait_for_socket "$work/run/gitaly.sock"

log "Creating the demo repository with an initial commit on main and feature/demo"
# Creating a repository is an operator action: it needs the shared token of
# [auth] in the configuration. The initial commit passes the policy as
# GRAFT_USER, here planner, who may write main.
shared_token="$(sed -n 's/^token = "\(.*\)"$/\1/p' "$work/config.toml")"
GRAFT_ADDRESS="unix:$work/run/gitaly.sock" GRAFT_STORAGE=default \
  GRAFT_TOKEN="$shared_token" GRAFT_USER=planner GRAFT_AUTHOR="planner <planner@example.com>" \
  "$work/bin/graft" repo create demo.git --initial-commit --branch feature/demo

log "Starting the graft-cache daemon tier"
"$work/bin/graft-cache" \
  --listen "unix:$work/run/cache.sock" \
  --upstream "unix:$work/run/gitaly.sock" \
  --tier daemon \
  --max-bytes 268435456 \
  --metrics-addr 127.0.0.1:9236 &
cache_pid=$!
wait_for_socket "$work/run/cache.sock"

export GRAFT_ADDRESS="unix:$work/run/cache.sock"
export GRAFT_STORAGE=default

# as_agent runs a command with the token, identity and author of one agent.
# GRAFT_USER should match the agent name: it names the temporary branches of
# commits. The server uses the agent of the token as the user ID and the policy
# decides on it.
as_agent() {
  local agent="$1"; shift
  GRAFT_TOKEN="$(cat "$work/tokens/$agent")" \
    GRAFT_USER="$agent" \
    GRAFT_AUTHOR="$agent <$agent@example.com>" \
    "$@"
}

log "planner: clone, commit a README and land it on main"
as_agent planner "$work/bin/graft" clone "$GRAFT_ADDRESS" demo.git "$work/demo" --branch main
(
  cd "$work/demo"
  printf 'hello graft\n' > README.md
  as_agent planner "$work/bin/graft" commit -m "Add README" --land main
)

log "builder: land a feature on feature/demo (allowed by the policy)"
(
  cd "$work/demo"
  printf 'feature\n' > feature.txt
  as_agent builder "$work/bin/graft" commit -m "Add feature" --land feature/demo
)

log "builder: land on main (denied by the policy, expected to fail)"
(
  cd "$work/demo"
  printf 'nope\n' >> README.md
  if as_agent builder "$work/bin/graft" commit -m "Denied change" --land main; then
    echo "ERROR: the policy did not deny the builder" >&2
    exit 1
  fi
  echo "denied as expected"
)

log "Reading history through the cache"
(
  cd "$work/demo"
  as_agent planner "$work/bin/graft" log
  as_agent planner "$work/bin/graft" blame README.md
  as_agent planner "$work/bin/graft" history README.md
  as_agent planner "$work/bin/graft" cat main:README.md
)

# The first clone above ran before the land. The next two read the same tip,
# so the second of them is served from the daemon's memory.
log "Two more clones of main: the second one reads its objects from the cache"
for copy in demo-2 demo-3; do
  as_agent planner "$work/bin/graft" clone "$GRAFT_ADDRESS" demo.git "$work/$copy" --branch main
done

if command -v curl >/dev/null 2>&1; then
  log "Cache statistics (object_hits should be above zero)"
  curl -s http://127.0.0.1:9236/stats || true
  echo
fi

log "Done. Work directory: $work"
