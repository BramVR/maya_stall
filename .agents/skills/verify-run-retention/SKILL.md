---
name: verify-run-retention
description: "Check changes to Embedded Mode run retention (Fresh Run Host Lock heartbeat, fenced session start/stop, Kept Session retain/sweep/stop, crash-recovery stop, Host Lock release) against the TLA+ model in verification/retention. Use when a diff touches fresh_run.go lifecycle/heartbeat code, run_retention*.go, run_state.go stop path, sessiond_broker.go start/stop/fence, or host-side lock renew/release/reclaim."
---

# Verify Embedded Mode run retention

`verification/retention/` holds a TLA+ model of one Maya Host shared by
Fresh Runs from different repos. It covers the `run` controller
(`freshRunLifecycle`), the Kept Session sweep that `run` and `doctor` call,
explicit `stop` (kept, cleanup-retry, and crash-recovery), and the Host Lock
release that follows. No lock serializes the controller against the sweep or
`stop`, so TLC interleaves them freely, with an integer clock that can tick
between any two steps.

## What it promises

- `MayaHeldByHostLock`: a live Maya UI Session whose run can still use it (a
  live controller, or a Kept Session whose Run Record still says kept) belongs
  to the run that holds the host-side Host Lock.
  Harm if broken: a run drives Maya with no lock. Another run acquires the Host
  and the two collide on one desktop.
  A session left by a dead controller is out of scope: it is the Host's
  standing broker session, and the next owner's fenced `StartFreshSession`
  stops it. `CanaryNoStandingSession` shows the model reaches it (crash after
  `StartFreshSession`, before the Run Record has the session id, then
  crash-recovery `stop` releases the lock without stopping Maya).
- `NoForeignKill`: no broker call ends a session whose run still holds the
  Host Lock.
  Harm if broken: a run that lost its lock kills the new owner's Maya.
  `StartFreshSession` stops any inherited session.

## Findings

- https://github.com/BramVR/maya_stall/issues/170 (unfenced broker start,
  other-client reclaim): fixed in `5c955db`. `MC.code.cfg` has
  `FenceStart = TRUE` and passes. `check.sh` also runs the same config with
  `FenceStart = FALSE` and expects `MayaHeldByHostLock` to fail
  (`ok unfenced-start`), so the fence stays the guard that makes it pass.
- https://github.com/BramVR/maya_stall/issues/171 (same-repo cleanup-retry
  releases a starting run's lock): the path it describes does not exist at
  the sync commit. `sweepKeptSessions` only enumerates repo mirror locks that
  name a `keptRun` (`listKeptRuns` / `readHostLockKeptRun`), so it never
  touches an active run. `StopPhase=session-stopped` is written only by the
  controller after its own session stopped (`preserveStoppedRunForCleanup`,
  `completeCleanupAfterRecoveryFailure`). Explicit `stop` of an active run
  goes through `requireStaleSubmittedRunController` in both
  `reconcileSubmittedRunLedgerForStop` and `stopKeptRunAfterBrokerStop`.
  Mutant `stop-ignores-live-controller` shows that check is what blocks the
  issue-171 shape.

## Does my change touch it?

Yes, if the diff changes any function in the coverage table, the order of the
Run Record / Run Ledger `StopPhase` checkpoints, which runs the sweep or `stop`
accept, the host-side lock compare-and-swap, lease, fence, or reclaim rules,
or how the broker starts or stops sessions.

```sh
git diff --stat 5c955db -- internal/cli/fresh_run.go internal/cli/run_retention.go internal/cli/run_retention_expiry.go internal/cli/run_state.go internal/cli/run_ledger.go internal/cli/run_ledger_lock.go internal/cli/host_pool.go internal/cli/host_lock_ssh.go internal/cli/sessiond_broker.go
```

No output means the model still describes the code. Changes to evidence
content, log text, or Scenario execution do not affect it, but read the diff
to confirm.

## Run the check

```sh
verification/retention/check.sh          # ~15s
verification/retention/check.sh --quick  # skip the larger bound
```

Needs Java 11+ and network once. It reuses the pinned `tla2tools.jar` v1.7.4
(sha256-checked) in `${XDG_CACHE_HOME:-~/.cache}/maya-stall-verification`.

Output lines:
- `PASS safety [code|full]`: all properties hold with the fenced broker.
- `ok unfenced-start`: without the issue-170 fence, `MayaHeldByHostLock`
  fails. A `FAIL` here means the fence no longer matters to the model; find
  out why before you change anything else.
- `ok canary: ...`: the model still reaches a Kept Session, a Kept Session
  stop, the cleanup-pending kept record, crash-recovery stop of a bound and of
  an unbound run, reclaim from a dead run, Host reuse, heartbeat failure, a
  refused fence, and a standing broker session.
- `ok mutant:<name>`: removing that Go guard breaks the property. On
  `substitution matched nothing`, fix the perl expression in `check.sh` so it
  removes the same guard.

## Read a failure

TLC prints the shortest trace with the action name for each step. Read every
result, pass or fail, with the pstack `/statechart` skill: it explains an
invariant violation as the shortest path in plain words and, on a pass, draws
how the system moves between states. Map each action to Go with the coverage
table and retell the trace as process steps.

Real bug or modelling error? Check each step against Go. If every guard the
trace relies on exists in Go and every guard it skips is missing in Go, it is
real: report it with the Go-terms trace and a Go repro if you can. Do not
weaken a property or the model, and do not change product code unless asked.
If the trace takes a step Go forbids, or splits or merges steps differently
from Go, fix the model and rerun.

## Update the model for an intended change

1. Find the actions for the changed functions in the coverage table.
2. Edit `RunRetention.tla` so each guard and effect matches the new Go code.
   An action is one Run Record write, one host-side lock compare-and-swap,
   or one broker call. If Go adds a lock that serializes the controller and
   the stoppers, model it as a shared variable in the guards.
3. New behaviour gets a new action and a coverage entry.
4. Run `check.sh`. Keep the mutants pointed at the same guards.
5. Confirm every action still matches Go at the new commit, then move the sync
   commit here and in the `git diff` command. Only then, not just because the
   check passed.

## Coverage record

Sync commit: `5c955db`.

Model actions -> Go (`internal/cli/`):
- `Acquire`: `selectHostForRunValidated`, `acquireRunHostLock`, `acquireRemoteHostLock`, `remoteHostLockReclaimAllowed`, `remoteHostLockSessionInactive`, `isStaleHostSideOwner`, host-side `Locked-Result` lease rule (host_lock_ssh.go); local Windows Maya Host: `acquireLocalHostSideLock`, `isRecoverableLocalSessiondHostLock`
- `WriteRunningRecord`: `freshRunLifecycle.setup` (`writeRunRetentionRecord(newRunRetentionRecord(... "running" ...))`, `markActive`)
- `Heartbeat`: `startHostLockHeartbeat`, `hostSideLock.renew` (`replaceRemoteHostLockOwner`: content compare-and-swap, no lease check)
- `RenewBeforeStart`, `RunScenario`: `renewHostLockNow`, `hostLockHeartbeatError`, `execute`
- `StartSession`: `startFreshSession`, `startSessionWithCancellation`, `ggMayaSessiondBroker.StartFreshSession` (fenced stop of inherited session, fenced `restartSessionBrokerTask`), `hostLockFence`, `remoteHostLockFenceScript`, `hostSideLock.claim`, `runHostPowerShell`
- `BindRead`, `BindWrite`: `startFreshSession` record update (`readRunRetentionRecord`, `writeRunRetentionRecord`), `hostSideLock.bindSession`
- `StopOwnSession`: `stopSessionDuringSettlement`, `stopSessionAfterFailure`, `ggMayaSessiondBroker.StopSession` (session id match, fenced stop)
- `Preserve`: `preserveStoppedRunForCleanup` (kept Run Record with `session-stopped`, `markKept`), `completeCleanupAfterRecoveryFailure`
- `CleanupRelease`: `finishStoppedRunCleanup`, `hostSideLock.release` (`removeRemoteHostLock`)
- `ReleaseUnstarted`: `freshRunLifecycle.Run` deferred `host.release` when no session started
- `Retain`: `retainSession`, `hostSideLock.markKept`, `ggMayaSessiondBroker.RetainRun`, `stampKeptSessionDeadline`
- `ControllerCrash`: process death; `isStaleHostLock` pid rule
- `StopKept`: `sweepKeptSessions` (`keptSessionSweepRepoDirs`, `listKeptRuns`, `readKeptRunState`, `verifyKeptHostSideLockForRun`), explicit `stopRun` (`ensureRunHasKeptLock`)
- `StopRetry`: `stopKeptRunAfterBrokerStop` with `StopPhase` set, `ensureRunHasCleanupPendingLock`
- `StopActiveDead`: `prepareRunLedgerForStop`, `reconcileSubmittedRunLedgerForStop`, `repoHostLockUsesActiveRun`, `requireStaleSubmittedRunController`, `verifyCleanupHostSideLockForRun`
- `SweepBrokerStop`: `ggMayaSessiondBroker.StopRetainedRun`/`StatusRetainedRun`, `keptHostLockFence`, `checkpointRunLedgerStopPhase`
- `SweepRelease`: `releaseHostSideLock`, `removeRemoteHostLockForRun`
- `SweepFinish`: `finishReleasedStopRun`, `removeRepoHostLockForRun`, `cleanupRunState`
- `SweeperCrash`: process death or a failed step (e.g. a failed ledger checkpoint)
- Serialization: none across steps. `withRunLedgerLock` (run_ledger_lock.go) covers single Run Ledger record updates only.

## Bounds and assumptions

- 2 runs, each in its own repo. Lease = 2 ticks. `MaxTime` is 5 for `code`
  (and the canaries, mutants, and unfenced check) and 7 for `full`.
- One gg_mayasessiond broker per Host with one session slot. Broker
  inactivity = no session running.
- Every Run Record, Run Ledger, and lock write is atomic. Run Record and Run
  Ledger `StopPhase` are one variable. `SweeperCrash` stands in for a failed
  checkpoint.
- Keep deadlines are not modelled: explicit `stop` may stop a Kept Session at
  any time, which covers the sweep's `KeepDeadline <= now` gate.
- One stopper per run; two concurrent `stop`/sweep processes for the same run
  are not modelled. The inherited-session stop and the broker restart in
  `StartFreshSession` are one fenced step.
- Client and host clocks are the same; leases are measured on the Host clock.
- Not covered: the Control Plane / Host Agent path (see `verify-host-lock-protocol`),
  Run Ledger contents and sequencing, legacy-record migration, fake
  (non-authoritative) host locks, several Hosts, broker start/stop failures
  other than id mismatch or fence refusal, and liveness.
