# PLAN12: live policy reload and a policy dry-run

Status: approved by the PM on 2026-10-23, with the additions marked (PM).

Scope: the standalone access policy (`[[graft.policy]]`) only. Agents, tokens, `agent_rpcs` and
every other key still need a restart.

## 1. Where the policy lives today (facts)

The policy is part of the server configuration (`config.Graft.Policy`), validated at load by
`config.Graft.Validate` (config_graft.go:541). It is copied by value into four readers at
construction, which is why a change needs a restart:

| reader | where | when it decides |
|---|---|---|
| standalone GitLab client (pre-receive `/allowed`) | `gitlab.NewStandaloneClient(cfg.Graft)`, serve.go:222; copy at standalone_client.go:28 | pre-receive of pushes and OperationService RPCs, before the references are locked |
| reference-transaction hook | `GitLabHookManager.cfg.Graft.Policy`, referencetransaction.go (graftPolicyDecision) | prepared phase, references locked, before the update is committed |
| `CheckAgentPolicy(ctx, graft, …)` | operations server field `graft config.Graft` (server.go:36): UserMergeToRef/RebaseToRef, land_inline, land_reland, land_resolve | before the RPC writes |
| land batch admission | `hook.EvaluateGraftPolicy(st.server.graft, …)`, land_batch_server.go:278 | before a member enters the running tip |

All four run in the gitaly server process (the hook binary calls back into HookService), so one
in-process store serves them all.

## 2. The invariant and the atomic swap

- One `policy.Store` per server process, created in serve.go from the startup configuration and
  injected into the four readers instead of the copied slices. It holds an
  `atomic.Pointer[Snapshot]`; `Snapshot{Rules []config.GraftPolicyRule, Generation uint64,
  SHA256 string}` is immutable once published.
- Invariant: **there is always exactly one valid policy in force.** The pointer is set before
  the server serves and is never nil; a reload builds and validates a complete new snapshot
  first and publishes it with one atomic store. There is no moment with no policy, or with a
  half-applied one.
- Every decision reads `store.Current()` once and decides with that snapshot only (a
  transaction with several references is judged by one generation).
- `config.Graft.Validate` stays the only validation, used at startup and on reload: one rule set,
  one validator.

## 3. An invalid file keeps the old policy

A reload re-reads the configuration file the server was started with, parses it with
`config.Load`, and validates the policy with `config.Graft.Validate`. Any failure (unreadable
file, TOML error, an invalid glob, an empty refs entry) leaves the current snapshot in force and:
- logs at ERROR `policy reload rejected` with the error and the file;
- counts `graft_policy_reloads_total{result="rejected"}`.

A valid file with the same rules (same SHA-256 of the canonical rule list) counts
`result="unchanged"` and publishes nothing. A valid, different file publishes the new snapshot,
logs at INFO `policy reloaded` with the old and new generation, hash and rule count, and counts
`result="applied"`. `graft_policy_generation` (gauge) and `graft_policy_info{sha256}` (1 for the
policy in force) let an operator check what each node runs. Other keys that differ from the
running configuration are named in a WARN (`needs a restart: graft.agent, …`) and not applied.

(PM) The new policy must also be consistent with the agents the server started with. The agents
are not reloaded: a file whose `[[graft.agent]]` entries differ from the running ones is named
in the WARN as needing a restart, and its agents are not applied. An actor of the new policy that
is a plain name (not a glob) and names no configured agent is reported in a WARN too, but does not
reject the policy: an actor can be the user ID of a caller with the shared token, which the policy
also decides on. Without agents every actor is such a user ID, and nothing is reported.

## 4. The trigger: SIGUSR1

- SIGHUP is taken: the default bootstrap treats it as shutdown (bootstrap_default.go:28) and the
  tableflip bootstrap as a graceful upgrade (bootstrap_tableflip.go:57). Changing its meaning
  would break existing operators.
- **Proposed: SIGUSR1** (unused by gitaly and by the Go runtime; gitaly-wrapper forwards every
  signal to the child, main.go:159). systemd: `ExecReload=kill -USR1 $MAINPID`.
- Rejected, a file watch: a save that is half written, an editor's rename, a Kubernetes
  ConfigMap's symlink swap each produce intermediate states; with "invalid keeps the old" they
  are safe but noisy, and a reload then happens when the file system says so, not when the
  operator decides. An explicit signal makes the moment of change the operator's, and
  `graft policy check` (section 6) runs before it. A watch can be added later on top of the
  same reload function if asked.
- Rejected for now, an admin RPC: new proto surface for one operation the signal covers.

Reloads are serialized (one at a time); a signal during a reload starts one more after it.

## 5. Which policy applies to an update in flight

Definition: **an agent's reference update is decided by the policy generation in force when the
reference-transaction hook runs in the prepared phase, while git holds the references' locks.**
That is the last check before the update commits, so:
- a tightening applies to every agent update that reaches the prepared phase after the swap,
  whatever its earlier checks allowed (the pre-receive or CheckAgentPolicy of the old policy only
  produce an earlier, clearer denial);
- a loosening applies to an update whose earlier checks run after the swap; one refused before
  the swap stays refused and the client retries (UserLand already reports it as a denial).

The one exception today is the land batch: the hook does not re-check a batch's members
(they were admitted in process). With a reload, a tightening between a member's admission and
the batch's commit would not apply to it. Fix: the batch records the generation it admitted
its members under, and carries it in the hooks payload of its combined update; the hook refuses
a batch update whose admission generation is not the current one (`policy changed during the
batch`), and the batch executor rebuilds (its existing rebuild path), admitting every member
again under the new snapshot. A member the new policy denies then fails alone, as today.

Callers that are not agents (shared token, unauthenticated) are checked only by pre-receive,
before the references are locked: a swap between their pre-receive and their commit can let one
update through under the old policy. They are administrative callers; this is documented, not
closed.

## 6. `graft policy check FILE` (dry-run)

- Offline part, always: parse FILE as a configuration (`config.Load`), validate its policy with
  `config.Graft.Validate`, print the rules with their numbers. Exit 2 on an invalid policy:
  exactly what the server would reject.
- Against recent changesets, with `-repo R` (repeatable) and `-address`: read the last N
  changesets of each repository (`ListChangesets`, default N = 1000, `--limit`), and for each
  reference update of an agent actor evaluate the candidate policy (`hook.EvaluateGraftPolicy`).
  Print the updates the candidate would deny, with the rule that would deny them, grouped by
  actor and reference; `--json` for scripts.
- Where the data comes from: the changeset log of each repository (file log or the storage
  core's table), through the existing RPC, so no new server surface. The actor of an entry is the
  agent of the token (LIMITATIONS, Security limitations).
- Cost: one streamed RPC per repository, N entries; evaluation is in memory, O(N × rules), a few
  milliseconds for N = 1000. It does not lock or write anything.
- What it cannot show: updates that were refused under the current policy are not in the log, so
  it shows what a candidate would newly deny among what happened, not what it would newly allow.
  Stated in its help text.

## 7. Multi-node and Praefect

- Each gitaly process has its own configuration and store; a reload is per node. Nodes that
  share a storage core (several nodes, one database) can run different generations for the time
  between their reloads. Each node exposes `graft_policy_info{sha256}`; the operations doc says
  to reload every node and compare the hash.
- Praefect is not part of standalone mode (LIMITATIONS): nothing to do there. A Praefect setup
  keeps the per-node behaviour above.
- graceful upgrades (tableflip) start a new process that reads the file at startup: it gets the
  file's policy, as today.

## 8. Tests

- Store: concurrent readers and reloads under -race (readers never see nil or a mixed snapshot;
  generations only grow).
- Reload: valid file → applied, metrics and log; same rules → unchanged; each kind of invalid
  file (unreadable, TOML error, bad glob, empty refs) → rejected, old policy still decides,
  metric and ERROR log; other changed keys → WARN, not applied.
- Signal: SIGUSR1 to a test server process (internal/graft/e2e realNode) reloads; SIGHUP keeps
  its current meaning.
- In flight, real server:
  - tightening: a land of an agent whose pre-receive passed under the old policy, held at the
    prepared phase by a test hook point, while the policy is reloaded to deny it → refused by
    the hook; the branch is unchanged;
  - loosening: an agent refused, the policy reloaded to allow it, the same land again → lands;
  - batch: a batch whose admission generation changes before its commit → the hook refuses the
    combined update, the batch rebuilds under the new policy, the newly denied member fails
    alone, the others land.
- (PM) An invalid policy reloaded while agents land: no decision is ever taken without a policy
  (no nil snapshot, no empty rule list where the startup policy had rules); every land is decided
  by the policy in force before the rejected reload.
- Concurrent lands during reloads: N agents landing in a loop while the policy alternates every
  few ms between two versions; every land either lands and was allowed by the generation the
  hook logged, or was refused; no land is recorded with an actor the policy of its generation
  denied (checked against the changeset log and the hook's log).
- `graft policy check`: invalid file exit 2; a candidate that denies a past update lists it with
  its rule; JSON output; a repository the caller may not read.
- Mutants: the hook reading a stale snapshot (copied at construction) → the tightening test
  fails; the batch generation check removed → the batch test fails.

## 9. Estimate and order

1. Store, injection into the four readers, reload function, metrics, logs: 1 day.
2. SIGUSR1 wiring, the batch generation check: 0.5 day.
3. Tests (store, reload, signal, in flight, concurrent, mutants): 1 day.
4. `graft policy check`: 0.5 day. Docs (CONFIGURATION, LIMITATIONS, operations), then an
   independent review.
