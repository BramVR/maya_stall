---
name: verify-embedded-ledger
description: "Check Embedded Mode run lifecycle changes (accept, host lock, retain/preserve, finish cleanup, Finalize, crash-recovery stop, kept-session sweep, Run Ledger prune) against the TLA+ model in verification/embedded-ledger. Use when a diff touches fresh_run.go setup/retain/finish, run_state.go stop, run_ledger.go stop/prune/finalize, or run_retention_expiry.go sweepKeptSessions."
---

# Verify Embedded run ledger, stop, sweep, and prune

`verification/embedded-ledger/` models one repo and one Maya Host in Embedded
Mode: Fresh Run controllers (which can crash between any two steps), operator
`stop <run-id>`, the kept-session sweep each `run` does before taking a Host
Lock, and Run Ledger retention prune. TLC checks every interleaving.

Per run the model tracks the ledger record (`.maya-stall/state/ledger/runs/<id>/run.json`),
the Run State dir and `run-record.json` StopPhase (`.maya-stall/state/runs/<id>/`),
and the repo Host Lock (`.maya-stall/state/locks/hosts/<host>.lock`). No lock
serializes `stop` or the sweep with a finishing controller, so they can land
between any two controller steps.

## What it promises

- `CrashedRunRecoverable`: a crashed run whose ledger is not terminal can always
  be finished by `maya-stall stop <run-id>`.
  Harm if broken: the run shows `submitted`/`cleanup-failed` in `status`/`history`
  forever, its Run State is never cleaned, and prune never removes it.
- `PruneKeepsUnfinished`: retention prune never removes the ledger of a run that
  still has Run State.
  Harm: a kept or cleanup-failed run loses its history while still holding state.
- `LiveRunIntact`: a live controller's Run State is never cleaned up under it by
  `stop` or the sweep (between `writeRunRetentionRecord("running")` and
  `preserveStoppedRunForCleanup`).
  Harm: a running run's state and Host Lock vanish mid-run.
- `SweepOnlyExpired`: the sweep stops a kept run only after its keep deadline.
  Harm: kept sessions are torn down early.
- `TerminalStable`: a terminal ledger stays terminal until prune removes it. One
  allowed exception: the run's own finishing controller checkpoints
  cleanup-failed over a terminal record that a concurrent `stop` wrote, and
  `Finalize` then rewrites it as terminal.
  Harm: finished runs flip back to kept or cleanup-failed.
- `LiveRunOwnsLock` (`MC.tla`): a live controller from Host Lock acquire until
  preserve still owns the repo Host Lock.
  Harm: two runs share one Maya Host.

## Status at the sync commit

Open findings, each reported as `KNOWN` by `check.sh` (code config):

1. `CrashedRunRecoverable` (https://github.com/BramVR/maya_stall/issues/177).
   Crash-recovery `stop` reaches a crashed running run only while the repo Host
   Lock names it as `activeRun`; the sweep (`listKeptRuns`) only finds runs whose
   lock names them as `keptRun`, so it never reaches a crashed running run.
   Windows:
   1. after `accept` and before `writeRunRetentionRecord("running")` (`fresh_run.go:520`):
      `reconcileSubmittedRunLedgerForStopUnlocked` uses the fallback Run Record and
      fails on the missing Evidence Bundle;
   2. between `writeRunRetentionRecord("running")` and `host.markActive` (`fresh_run.go:523`):
      `repoHostLockUsesActiveRun` says "not owned by run";
   3. after `markActive`, if a later `run` breaks the stale-pid repo lock, uses it,
      and releases it: stop fails on the missing lock.
   4. (main only; `CrashedRunRecoverableAfterStop`) `stop` finishes a run whose
      live controller is in `preserveStoppedRunForCleanup`/`finishStoppedRunCleanup`;
      the controller then checkpoints broker-cleaned (ledger back to
      cleanup-failed, no host-lock-released) and crashes. No Run State manifest,
      no lock: `releaseMatchingKeptHostLock` says "not found".
2. `TerminalStable` (not filed). `stop` on a run whose live controller is in
   `retainSession` (after `markKept`) or before its `Run()` defer finishes the
   kept run (terminal, Run State and lock gone); `Finalize` then overwrites the
   terminal ledger with `kept` (`finalizeRunLedgerUnlocked` has no guard). The
   record is never pruned and every later `stop` fails.
3. `LiveRunOwnsLock` (not filed). `stop` finishes a run between its
   `preserveStoppedRunForCleanup` record write and `markKept`
   (kept record, no controller check), another `run` takes the free lock, then
   `markKept` -> `replaceHostLockOwner` overwrites that run's repo lock with
   `keptRun`. Only on hosts without a host-side lock (`hasAuthoritativeHostSideLock`
   false: fake host, no absolute `WorkRoot`); with one, `hostSideLock.markKept`
   fails on the changed owner.

`MC.fixed*.cfg` turns on `FixOrphan`: stop and sweep judge a crashed controller
from Run State (e.g. a controller pid recorded in `run-record.json` at accept),
not from the repo Host Lock; the sweep enumerates Run State; `stop` refuses while
the recorded controller is alive. With it, every property passes. When the code
is fixed, mirror the fix in the spec, move the property into `MC.code.cfg`, and
drop its `KNOWN` probe from `check.sh`.

## Does my change touch it?

Yes if the diff changes the order or guards of: `freshRunLifecycle.accept`,
`setup` (prune, `BeforeHostLock` sweep, `writeRunRetentionRecord("running")`,
`markActive`), `retainSession`, `preserveStoppedRunForCleanup`,
`finishStoppedRunCleanup`, `completeCleanupAfterRecoveryFailure`, the `Run()`
defer `Finalize`, `stopRun`, `prepareRunLedgerForStop`,
`reconcileSubmittedRunLedgerForStopUnlocked`, `requireStaleSubmittedRunController`,
`repoHostLockUsesActiveRun`, `stopKeptRunAfterBrokerStop`,
`ensureRunHasKeptLock`/`ensureRunHasCleanupPendingLock`, `finishReleasedStopRun`,
`releaseMatchingKeptHostLock`, `updateRunLedgerAfterStopUnlocked`,
`checkpointRunLedgerStopPhaseUnlocked`, `finalizeRunLedgerUnlocked`,
`pruneRunLedgerUnlocked`, `sweepKeptSessions`, `listKeptRuns`,
`replaceHostLockOwner`, `acquireRunHostLock`/`acquireHostLock` stale breaking,
or adds a lock that serializes `stop` with a finishing controller.

```sh
git diff --stat 5c955db -- internal/cli/fresh_run.go internal/cli/run_state.go internal/cli/run_ledger.go internal/cli/run_ledger_store.go internal/cli/run_retention.go internal/cli/run_retention_expiry.go internal/cli/run_ledger_lock.go internal/cli/host_pool.go
```

## Run the check

```sh
verification/embedded-ledger/check.sh               # ~15s
verification/embedded-ledger/check.sh --only-safety # code + fixed only
```

Needs Java 11+. Uses the pinned `tla2tools.jar` v1.7.4 (sha256-checked) in
`${XDG_CACHE_HOME:-~/.cache}/maya-stall-verification`, downloading it once.

Output lines:
- `PASS code` / `PASS fixed` / `PASS fixed-full`: properties hold for that config.
- `KNOWN finding: ...`: an open finding still reproduces, with its trace.
- `ok canary: ...`: the model reaches that situation (finished run, prune,
  crash-recovery stop, lock takeover, sweep expiry, stop during finish).
- `ok mutant:<name>`: removing that guard breaks the named property. If a
  mutant prints `substitution matched nothing`, the spec text moved; fix the
  perl expression in `check.sh` so it removes the same guard.

## Read a failure

TLC prints the shortest trace; each state names the action. Read results with
the pstack `/statechart` skill: it retells an invariant violation as its
shortest path in plain words and charts the state machine on a pass. Map each
action to Go with the coverage record below.

Real bug or modelling error? Check each step against Go. If every guard the
trace passes exists in Go and every guard it skips is absent, it is a real bug:
report it in Go terms and do not weaken the property or model. If the trace
passes a step Go would reject, or splits/merges lock sections differently from
Go, fix the action and rerun. Do not change product code unless asked.

## Update the model for an intended change

1. Find the actions for the functions you changed (coverage record).
2. Edit `EmbeddedLedger.tla` so guards and effects match the new Go. One action
   = one ledger-lock section, one local Host Lock mutex section, or one atomic
   file write.
3. New behaviour gets a new action and a coverage entry.
4. Run `check.sh`; keep the mutant perl expressions hitting the same guards.
5. After confirming every action matches Go at the new commit, move the sync
   commit here and in the `git diff` command. Only then, not because it passed.

## Coverage record

Sync commit: `5c955db`.

Model actions -> Go (`internal/cli/`):
- `Accept`: `freshRunLifecycle.accept` (fresh_run.go:545), `runLedgerStore.Initialize` (fresh_run.go:625) -> `initializeRunLedger` (run_ledger.go:127)
- `Prune`: `setup` -> `runLedgerStore.Prune` (fresh_run.go:373) -> `pruneRunLedgerUnlocked` (run_ledger.go:1173)
- `Sweep`: `setup` `BeforeHostLock` (fresh_run.go:410, host_pool.go:489) once per host -> `sweepKeptSessions` (run_retention_expiry.go:64), `listKeptRuns` (run_state.go:611), `stopRunWithKeptExpiryEvent` (run_retention_expiry.go:139)
- `Acquire`, `GiveUp`: `selectHostForRunValidated` (host_pool.go:451), `acquireRunHostLock` (host_pool.go:715), `acquireHostLock` (host_pool.go:693), `isStaleHostLock` (host_pool.go:1151)
- `WriteRecord`: fresh_run.go:520 `writeRunRetentionRecord(... "running")`
- `MarkActive`: fresh_run.go:523 `host.markActive` -> `markHostLockActiveWithAuthority` -> `replaceHostLockOwner` (host_pool.go:1118, overwrites any existing lock file)
- `StopMaya`: scenario execution and stop-after Stop Policy in `Run`/`settle`
- `KeepMark`, `KeepRecord`: `retainSession` (fresh_run.go:1497): `host.markKept` (fresh_run.go:1509), then `writeRunRetentionRecord(kept)`
- `PreserveRecord`, `PreserveMark`: `preserveStoppedRunForCleanup` (fresh_run.go:1602): record kept/session-stopped, then `host.markKept` (fresh_run.go:1624); a failure runs `completeCleanupAfterRecoveryFailure` (fresh_run.go:1631)
- `CleanBroker`, `ReleaseLock`, `FinishRelease`: `finishStoppedRunCleanup` (fresh_run.go:1565): record + `checkpointRunLedgerStopPhaseUnlocked` broker-cleaned (run_ledger.go:1400), `host.release` (fresh_run.go:1585) -> `removeRepoHostLockForRun` (run_state.go:508), record + checkpoint host-lock-released, `cleanupRunState` (fresh_run.go:1596); `readRunRetentionRecord` fallback (run_retention.go:122)
- `Finalize`: `Run()` defer (fresh_run.go:201) -> `finalizeRunLedgerUnlocked` (run_ledger.go:279), `terminalRunLedgerState` (run_ledger.go:989)
- `Crash`, `DeadlineElapse`: process death; keep deadline
- `Stop`, `StopFails`, `StopOK`: `stopRun` (run_state.go:360), `prepareRunLedgerForStop` (run_ledger.go:1427), `reconcileSubmittedRunLedgerForStopUnlocked` (run_ledger.go:1493), `repoHostLockUsesActiveRun` (run_ledger.go:1546), `requireStaleSubmittedRunController` (run_ledger.go:1569), `stopKeptRunAfterBrokerStop` (run_state.go:376), `ensureRunHasKeptLock`/`ensureRunHasCleanupPendingLock` (run_state.go:777/801), `finishReleasedStopRun` (run_state.go:492), `releaseMatchingKeptHostLock` (run_state.go:562), `updateRunLedgerAfterStopUnlocked` (run_ledger.go:1350)

## Bounds and assumptions

- code: 3 runs (~127k distinct states); fixed, orphan: 2 runs; fixed-full: 3
  runs. Each run is accepted once; controllers crash at most once (a crash is
  final).
- Each controller step is atomic; crashes land only between steps. `stop` (and
  each sweep expiry) is one atomic action: interleavings inside `stopRun` are
  not explored.
- Repo Host Lock checks follow hosts without a host-side lock
  (`hasAuthoritativeHostSideLock` false). With one, `stop` also accepts a
  missing repo mirror and verifies the host-side lock instead; that protocol is
  in `verification/remote-host-lock`, abstracted here as reclaimable whenever
  the repo lock's owner is dead. The Host Lock fence on session start and
  broker calls (https://github.com/BramVR/maya_stall/issues/170) is out of
  scope (no session actions here).
- A record write after `stop` removed Run State recreates a manifest-less dir;
  modelled as no Run State (stop and the sweep cannot see it).
- Keep deadlines pass only for dead controllers or finished kept runs.
  A stalled live controller is `verification/retention`'s territory.
- Not modelled: `stop` against a live controller inside `retainSession` before
  its record write (allowed by Go; outside `LiveRunIntact`), write/broker
  failures other than `StopFails`, Run Ledger events/log content (see
  `verification/run-ledger`), `doctor` beyond its sweep, `attach`/`status`
  reads, Control Plane mode, multiple Hosts, ledger reconstruction from corrupt
  records.
