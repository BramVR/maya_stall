---
name: verify-host-lock-protocol
description: "Check changes to the Control Plane / Windows Host Agent Host Lock protocol (dispatch, register, confirm, complete/fail, quarantine, lease-loss quarantine, Control Plane restart recovery) against the TLA+ model in verification/host-lock. Use when a diff touches internal/cli/host_agent.go, host_agent_transition_store.go, or Host Agent dispatch in run_queue.go."
---

# Verify the Host Lock protocol

`verification/host-lock/` holds a TLA+ model of how the Control Plane hands one
Maya Host to one Windows Host Agent run at a time, and how it gets the Host back
(completion, failure, quarantine, lost process-session lease, Control Plane
restart). TLC
checks every interleaving of those steps within small bounds.

## What it promises

- `MayaHeldByHostLock`: while a Maya UI Session started by a run may still be
  alive, the durable Host Lock (`host-locks/<host>.json`) and the Agent slot
  (`agent.status.RunID`) both still name that run.
  Harm if broken: the Control Plane hands the Host to the next run while the
  old Maya is still on the desktop. Runs collide and evidence is wrong.
- `OneMayaPerHost`: two Agent processes never both have a live Maya UI Session
  on the one Host.
  Harm if broken: a restarted or stalled Agent and its replacement both drive
  Maya on the same desktop.
- `AssignOnce`: a Run ID becomes a Host Agent assignment at most once.
  Harm if broken: one submitted run executes twice.
- `RestartLoadable`: durable state always passes `loadHostAgentState`
  consistency checks (assignment and Host Lock agree, quarantine marker and
  quarantined status match the lock).
  Harm if broken: the Control Plane refuses to start after a crash.

A quarantined Host Lock stays held forever in the model. Operator recovery is
out of scope. Fail-closed is the safe outcome.

## Does my change touch it?

Yes, if the diff changes any function in the coverage table below, any guard on
`assignment.record.State`, `assignment.finishing`, `agent.status.{State,RunID,SessionID}`,
`sessionExpiresAt`, `takeoverNotBefore`, the quarantine marker, the shared
fake-host lock, or the order of durable writes in `hostAgentTransitionStore`.
Adding a Control Plane Host Lock deadline or expiry sweep on this path is new
behaviour: model it (main has none; a lost lease is the only expiry).
To list covered code changed since the last sync:

```sh
git diff --stat 5c955db -- internal/cli/host_agent.go internal/cli/host_agent_transition_store.go internal/cli/run_queue.go internal/cli/control_plane.go
```

No output means the model still describes the code. Changes only to progress
payloads, evidence files, ledger content, capability matching, or messages do
not affect it, but read the diff to confirm.

## Run the check

```sh
verification/host-lock/check.sh          # ~15s: both bounds, canaries, mutants
verification/host-lock/check.sh --quick  # ~10s: skip the larger bound
```

Needs Java 11+ and network once. The script downloads `tla2tools.jar` v1.7.4
into `${XDG_CACHE_HOME:-~/.cache}/maya-stall-verification` and checks its sha256.

Output lines:
- `PASS safety [quick|full]`: all four properties hold at that bound.
- `ok canary: ...`: the model reaches that situation (started Maya, Host reused
  by a second run, quarantine over live Maya, confirmed run across restart,
  takeover of an unconfirmed run, marker recovery). If a canary stops being
  violated, the model lost behaviour and the passes mean less.
- No known findings: every property must pass.
- `ok mutant:<name>`: removing that guard breaks the named property. If a
  mutant prints `substitution matched nothing`, the spec text moved; fix the
  perl expression in `check.sh` to hit the same guard.

## Read a failure

TLC prints the shortest trace as numbered states, each tagged with the action
that produced it. Read every result, pass or fail, with the pstack
`/statechart` skill: it turns an invariant violation into the shortest path in
plain words and, on a pass, draws how the system moves between states.

Map each action name to the Go function in the coverage table, then retell the
trace as requests and handler steps. Example (mutant `register-takeover`):
`Register` (process A gets a session) -> `Reserve`/`Accept*` (run r1 assigned)
-> `Poll`, `Confirm` (A gets 200) -> `LeaseExpire` (A stalls) -> `Register`
(process B registers) -> `StartMaya` (A) -> `Poll`, `Confirm`, `StartMaya`
(B) -> two live Maya UI Sessions.

Real bug or modelling error? Open the Go function for each step in the trace.
- Every guard the trace passes exists in Go, and every guard the trace skips is
  missing from Go: real bug. Report it with the trace in Go terms. Do not
  weaken the invariant or the model to make it pass. Do not change product code
  unless asked.
- The trace passes a step Go would reject (or the model splits a step that Go
  does under one `handler.mu`/`checkpointMu` section, or merges two that Go does
  in separate sections): modelling error. Fix the action so it matches Go, then
  rerun.

## Update the model for an intended change

1. Find the actions for the functions you changed (coverage table).
2. Edit `HostAgentHostLock.tla` so each action's guard and effect match the new
   Go code. One action = one locked section. Durable writes go to the durable
   variables. In-memory writes go to the memory variables, which `Restart` resets.
3. New behaviour gets a new action and an entry in the coverage table.
4. Run `check.sh`. If a mutant's perl expression no longer matches, update it
   so it still removes the same guard.
5. Confirm every action still matches the Go code at the new commit. Then move
   the sync commit below and in the `git diff` command above to that commit.
   Move it only after this confirmation, never just because the check passed.

## Coverage record

Sync commit: `5c955db` (fix(host-lock): fence Maya session start and broker
calls on Host Lock ownership). Main has no Control Plane Host Lock
idle/hard deadline, settlement grace, or deadline sweep for Host Agent
assignments; heartbeats only renew the process-session lease
(`serveHostAgentHeartbeat` -> `acceptHostAgentSession`). An `assigned` run whose
Agent vanished stays held until a new process registers and confirms it. The
issue 170 fence (https://github.com/BramVR/maya_stall/issues/170) guards the
host-side Embedded Host Lock that `runScenario` takes inside the Agent; it is
not part of this model (see `verify-remote-host-lock`).

Model actions -> Go (`internal/cli/`):
- `Reserve`: `dispatchQueuedRuns` (run_queue.go), `queueAcquireHostLock` shared fake-host lock
- `AcceptMarker`, `AcceptCommit`, `AcceptFinish`, `AcceptRollback`: `runScenarioThroughHostAgent` acceptance (host_agent.go)
- `Register`, `RegisterQuarantine`: `serveHostAgentRegistration`
- `Poll`: `serveHostAgentNextAssignment`
- `Confirm`: `serveHostAgentConfirm`
- `StartMaya`, `SkipMaya`, `StopMaya`, `StopUnresolved`, `AgentExit`: `runHostAgentOnce`, `failConfirmedHostAgentAssignment`, `quarantineConfirmedHostAgentAssignment`
- `PostTerminal`: `serveHostAgentComplete`, `serveHostAgentFail` (claim `finishing`)
- `PostQuarantine`: `serveHostAgentFail` with `quarantine`, `quarantineHostAgentAssignment`
- `CommitFinishing`, `CommitCompleted`, `AbortFinishing`: `finishHostAgentAssignment`, `resetHostAgentFinishing`, `resumeFinishingHostAgentAssignment`
- `CleanupFailQuarantine`: `quarantineHostAgentCleanupFailure`
- `Quarantine(r)` helper: `quarantineHostAgentAssignmentLocked`
- `WaitLoopQuarantine`: wait loop at the end of `runScenarioThroughHostAgent`
- `LeaseExpire`, `ClearExpiredSession`: `acceptHostAgentSession`, `dispatchQueuedRuns` expiry, `serveHostAgentEnrollment` expiry
- `GraceExpire`: `takeoverNotBefore`, set by `loadHostAgentState`
- `Restart`: `newControlPlaneHandler` (control_plane.go), `loadHostAgentState` (including its inline finishing -> completed branch), `recoverHostAgentQuarantineMarkers`, `reestablishHostAgentSharedLock`, `loadControlPlaneQueue` (run_queue.go)
- Durable commit: `hostAgentTransitionStore.Commit`/`apply` (host_agent_transition_store.go)

## Bounds and assumptions

- quick: 2 runs, 3 Agent processes, 1 Control Plane restart. full: 3 runs,
  4 processes, 2 restarts. One Maya Host, one enrolled Agent.
- `hostAgentTransitionStore.Commit` is one atomic durable write. Its journal and
  recovery are not modelled.
- Time is not modelled. Leases and takeover grace may expire at any moment.
  Lost responses and process crashes are allowed anywhere.
- `loadHostAgentState`'s finishing branch (cleanup, shared-lock release,
  `Commit(completed)`) is assumed to succeed; if it fails the Control Plane
  refuses to start, which the model does not explore.
- Not covered: several Hosts or Agents (so `loadControlPlaneQueue`'s
  "assignment already exists" filter is not exercised), Embedded Mode Host
  Locks and Kept Session retention (`run_retention*.go`), the host-side Host
  Lock fence inside the Agent's `runScenario` (issue 170), the Session Broker,
  progress/heartbeat payloads, Run Ledger contents, `status.json` write
  failures, the `transitionErr` quarantine path at acceptance, liveness
  (whether a Host is ever freed).
