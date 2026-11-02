---
name: verify-run-queue
description: "Check changes to the Control Plane durable Run queue (admission, dispatch across Hosts, queued-Run cancel, restart recovery) against the TLA+ model in verification/run-queue. Use when a diff touches internal/cli/run_queue.go, queue handling in control_plane.go, or the acceptance part of runScenarioThroughHostAgent in host_agent.go."
---

# Verify the Control Plane Run queue

`verification/run-queue/` holds a TLA+ model of ADR 0050: how the Control Plane
admits a Run into the durable queue, hands queued Runs to free Hosts in FIFO
order, accepts or rolls back a reservation, cancels a queued Run, and rebuilds
all of it after a crash. TLC checks every interleaving with two Hosts, two Runs,
one Control Plane restart, and nondeterministic file-write failures.

What happens after `Commit(assigned)` (confirm, complete, quarantine) belongs to
`verification/host-lock` (skill `verify-host-lock-protocol`). Here an assignment
either stays active or completes and frees its Host.

## What it promises

Checked by the safety pass (must hold, with fake-host lock release and
after-rename write failures switched off):
- `CanceledNeverAssigned`: a Run that recorded cancellation (`canceling` ledger)
  never gets a Host assignment. Harm: the operator cancels a Run and Maya still
  runs it.
- `AssignOnce`: a Run ID becomes an assignment at most once, across restarts.
  Harm: one submission runs twice.
- `HostExclusive`: two Runs never both hold one Host (wrote its quarantine
  marker or committed), and one Run never holds two Hosts. Harm: two Runs share
  a Maya desktop, or one Run occupies two Hosts.
- `QueueBound`: waiting Runs plus in-flight admissions never exceed the bound
  (1,000 in Go, `MaxQueued` here). Harm: unbounded durable queue.
- `QueuedRecoverable`: an accepted, unassigned Run with a `submitted`/`queued`
  ledger always has a durable queue intent and is in memory (or still being
  admitted). Harm: a Run is accepted and then never dispatched.
- `RestartLoadable`: durable queue state always passes `loadControlPlaneQueue`.
  Harm: the Control Plane refuses to start.

- `ReservationOwned` and `TerminalLedgerStable`, described below.

Open findings: with file-write faults on (`MC.findings.cfg`: `ReleaseFails`,
`RenameThenFail`) the code at the sync commit violates these, and `check.sh`
expects it. Filed as https://github.com/BramVR/maya_stall/issues/172 (open;
still reproduces on `5c955db`):
- `ReservationOwned`: a reserved Host or held fake-host shared lock always has a
  live owner. Violated by the failed-release path below (Host stuck in
  `reserving` until restart). Go: `runScenarioThroughHostAgent` calls
  `resetQueuedDispatchLocked` before `sharedFakeHostRelease`; on release failure
  `failBeforeTransition` removes the Run while the dispatcher may already have
  reserved a second Host for it.
- `TerminalLedgerStable` (action property): a terminal Run Ledger outcome is
  never rewritten. Violated by cancel racing `failBeforeTransition`: after the
  reset, `cancelQueuedRun` may write `canceled`, then
  `finishAcceptedHostAgentFailure` `Finalize`s `failed` over it.
- `RestartLoadable`: two file-write faults (release + `removeQueueIntent`, or
  cancel-intent write after rename + `removeQueueIntent`) leave a `canceling`
  intent beside a `failed` ledger, which `loadControlPlaneQueue` rejects.

## Does my change touch it?

Yes, if the diff changes any function in the coverage table, the flags on
`controlPlaneQueuedRun` (`dispatching`, `canceled`, `cancellationActive`,
`selected`, `lockContended`, `done`), queue intent states, the order of durable
writes during admission/cancel/acceptance, or `loadControlPlaneQueue` branches.

```sh
git diff --stat 5c955db -- internal/cli/run_queue.go internal/cli/control_plane.go internal/cli/host_agent.go internal/cli/fresh_run.go
```

Then read the hunks: only changes to the functions below matter. Status
rendering (`addControlPlaneQueueStatus`, `queueWaitReasonLocked`) and event
payloads do not.

## Run the check

```sh
verification/run-queue/check.sh                # ~20-60s: safety, findings, canaries, mutants
verification/run-queue/check.sh --only-safety  # ~10-30s
```

Needs Java 11+ and network once. The script downloads `tla2tools.jar` v1.7.4
into `${XDG_CACHE_HOME:-~/.cache}/maya-stall-verification` and checks its sha256.

Output lines:
- `PASS safety`: the six safety properties hold.
- `ok finding: X violated as expected`: an open finding still reproduces. If it
  prints `X was never violated` after a fix, the fix works in the model. The
  safety pass already checks X with faults off: turn the matching fault on in
  `MC.quick.cfg` and delete the probe line.
- `ok canary`: the model still reaches that situation (assignment, both Hosts
  busy, cancel, lock contention, requeue after rollback, queue after restart,
  re-admission of a departed Run, inert queue entry, full queue). A canary that
  stops firing means the passes mean less.
- `ok mutant:<name>`: removing that guard breaks the named property. `matched
  nothing` means the spec text moved; fix the perl expression in `check.sh`.

## Read a failure

TLC prints the shortest trace, each state tagged with its action. Read every
result, pass or fail, with the pstack `/statechart` skill: it turns an
invariant violation into the shortest path in plain words and, on a pass, draws
how the system moves between states.

Map each action to Go with the coverage table and retell the trace as
goroutines: the submitter/waiter (`wpc`), the dispatcher (`disp`), the cancel
request (`xpc`). Example, finding `ReservationOwned`:
`DispClaim`/`DispAcquire` (h1 selected for r1) -> `WaitSelected` ->
`LeaseExpire` -> `AccFirstCheck` (first check fails, `resetQueuedDispatchLocked`)
-> `AccRelease1` (shared-lock release fails -> `failBeforeTransition`) ->
`DispClaim`/`DispAcquire` (r1 selected again, now for h2: h2 reserving, h2 lock
held) -> `AccFailBefore` (r1 removed from the queue) -> nobody will ever accept
or release h2.

Real bug or modelling error? Open the Go function for each step.
- Every guard the trace passes exists in Go and the steps are separate
  `handler.mu` sections in Go: real bug. Report it in Go terms. Do not weaken the
  property or the model, and do not change product code unless asked.
- The trace interleaves something Go does under one `handler.mu` section, or
  passes a guard Go has: modelling error. Fix the action, rerun.

## Update the model for an intended change

1. Find the actions for the changed functions (coverage table).
2. Edit `ControlPlaneRunQueue.tla` so guards and effects match Go. One action =
   one `handler.mu` section or one durable write that can fail on its own.
   Durable variables survive `Restart`; memory variables and goroutines do not.
3. New behaviour gets a new action and a coverage entry.
4. Run `check.sh`. Fix mutant expressions that stop matching.
5. Confirm every action still matches Go at the new commit, then move the sync
   commit below and in the `git diff` command. Never move it just because the
   check passed.

## Coverage record

Sync commit: `5c955db` (fix(host-lock): fence Maya session start and broker
calls on Host Lock ownership). `run_queue.go` and the acceptance part of
`runScenarioThroughHostAgent` match the previous sync; main has no Control Plane
Host Lock deadline sweep, so nothing outside `finishHostAgentAssignment` frees an
assigned Host.

Model actions -> Go (`internal/cli/`):
- `Submit`, `AdmWriteIntent`, `AdmAccept`, `AdmAcceptFail`, `AdmMarkQueued`, `AdmPublish`, `AdmFail`: `queueHostAgentRun` admission branch (run_queue.go), `queueAdmissionLocked`, `freshRunLifecycle.accept` (fresh_run.go), `markControlPlaneRunQueued`, `ensureDurableQueuedRunState`; failure handling in `runScenarioThroughHostAgent` (host_agent.go) after `queueHostAgentRun` errors
- `WaitSelected`, `WaitCanceled`: waiter loop in `queueHostAgentRun`, `queuedSelectionLocked`
- `AccFirstCheck`, `AccRelease1`, `AccEligFail`, `AccRollbackStatus`, `AccCommit`, `AccPublish`, `AccFailBefore`: `runScenarioThroughHostAgent` acceptance (host_agent.go): first eligibility check, quarantine marker, eligibility recheck + `refreshQueuedReservationCapabilitiesLocked`, `Commit(assigned)`, `removeQueuedRunLocked`, `failBeforeTransition` -> `finishAcceptedHostAgentFailure`
- `AccRequeue`, `ReAdmWrite`, `ReAdmFail`: the recursive `runScenarioThroughHostAgent` -> `queueHostAgentRun` lookup, including the re-admission branch when the Run already left `handler.queuedRuns`
- `DispClaim`, `DispAcquire`: `dispatchQueuedRuns`, `orderedQueuedRunsLocked`, `queueAcquireHostLock`, `resetQueueHostClaimLocked`
- `CancelStart`, `CancelPrepare`, `CancelCleanup`, `CancelError`, `CancelRemove`: `cancelQueuedRun`, `completeQueuedRunCancellation`, `prepareQueuedRunCancellation`, `cleanupQueuedRunCancellation`, `releaseQueuedWaiterLocked`
- `Register`, `LeaseExpire`, `ClearExpired`: `serveHostAgentRegistration`, `acceptHostAgentSession`, `dispatchQueuedRuns` expiry sweep (only the state transitions the queue reads)
- `AsgComplete`: `finishHostAgentAssignment` (Host freed, shared lock released)
- `Restart`: `newControlPlaneHandler` (control_plane.go), `recoverHostAgentQuarantineMarkers`, `loadHostAgentState` lock reload, `reestablishHostAgentSharedLock`, `loadControlPlaneQueue`, `resumeControlPlaneQueue`
- `ResetRun`: `resetQueuedDispatchLocked`

## Bounds and assumptions

- Safety: 2 Runs, 2 Hosts, `MaxQueued = 2`, 1 restart, 1.1M states. Queue-full
  probes use `MC.bound.cfg` (`MaxQueued = 1`). Finding probes use
  `MC.findings.cfg`. Three Runs exceeds 18M states and is not run.
- FIFO is built into `DispClaim` (it takes the minimum `ord`), so FIFO is
  assumed, not checked. Host order (Host ID, Agent ID) is not modelled.
- Capability compatibility is a nondeterministic yes/no at recheck time; any
  live Host is assumed compatible at claim.
- `writePrivateJSON` fails cleanly except the cancel-intent after-rename
  failure (`RenameThenFail`). Shared-lock release failure is `ReleaseFails`.
  Both are on only in `MC.findings.cfg`. `removeQueueIntent` and `run.accept`
  failures are always modelled. Run Ledger writes
  inside `finishEarlyFailure`/`Finalize`/cancel preparation succeed or fail as a
  whole.
- Restart recovery of a `canceling` intent is assumed to succeed.
- Not covered: Host Agent protocol after commit (see `verify-host-lock-protocol`),
  queue status/position rendering, selection errors from shared-lock acquire
  I/O errors, the `transitionErr` quarantine path, liveness (that every queued
  Run is eventually dispatched).
