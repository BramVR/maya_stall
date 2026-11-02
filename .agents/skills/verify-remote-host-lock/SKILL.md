---
name: verify-remote-host-lock
description: "Check changes to the Embedded Mode host-side Host Lock over SSH (acquire, stale reclaim, renew, markActive/markKept, bindSession, release, removeForRun, the Host Lock fence on broker calls, the PowerShell lock scripts) against the TLA+ model in verification/remote-host-lock. Use when a diff touches internal/cli/host_lock_ssh.go, hostSideLock/hostLockFence in host_pool.go, renewHostLockNow/startFreshSession in fresh_run.go, or gg_mayasessiond StartFreshSession/StopSession/runHostPowerShell/StopRetainedRun."
---

# Verify the remote host-side Host Lock

`verification/remote-host-lock/` models several controllers (checkouts, CI
runners) contending over SSH for one Maya Host work root
(`<workRoot>/state/locks/host.lock`). It also models the one
`gg_mayasessiond` Maya UI Session on that Host. TLC checks every interleaving
within small bounds, including script timeouts, delayed scripts, controller
crashes and pauses, lease expiry, and Maya crashing on its own.

## What it promises

- `SessionUnderOwnersLock`: a live Maya UI Session always belongs to the
  controller that currently holds the Host Lock.
  Harm if broken: two controllers drive one Maya desktop, so runs kill each
  other's Maya or produce wrong evidence.
- `NoForeignSessionTouch`: no controller's broker call stops or drives a
  session it did not start.
  Harm if broken: one run's StartFreshSession or StopSession kills another
  run's Maya.
- `KeptNeverReclaimed`: a `keptRun` lock is never reclaimed as stale.
  Harm if broken: a Kept Session the user is inspecting is taken over.
- `NoReclaimOverLiveSession`: reclaim never deletes the lock of an owner whose
  Maya UI Session is live.
  Harm if broken: a busy Host is handed to a second run.

## Host Lock fence (issue 170, fixed)

https://github.com/BramVR/maya_stall/issues/170 (closed, fixed in `5c955db`):
a run could renew, stall past its lease, lose the lock to a reclaim, and then
start Maya under the new owner's lock. Main now fences every broker call that
starts, stops, or drives Maya (`hostLockFence`, `remoteHostLockFenceScript`):
the same Host script first proves under `Invoke-WithLockMutex` that the
lockToken (or keptRun for a Kept Session) still matches and the lease is
unexpired, touches `LastWriteTimeUtc`, then runs the action. The model's
`FenceOK` guards `StartSession`, `ScenarioStep`, and `StopFenced`.

`MC.unfenced.cfg` drops `RenewOutlivesOp` (any stall, including between a
renew and the next broker call) and must pass as a normal `safety` run. The
three `*-without-fence` mutants remove one fence each and must break.

## Does my change touch it?

```sh
git diff --stat 5c955db -- internal/cli/host_lock_ssh.go internal/cli/host_pool.go internal/cli/fresh_run.go internal/cli/sessiond_broker.go internal/cli/run_state.go
```

Look at the coverage record below. Changes to PowerShell script bodies,
`Locked-Result` expiry rules, CAS checks, retry or timeout handling, owner
content fields (lockToken, activeRun, keptRun, brokerSessionId,
leaseDurationSeconds), the renew-before-op pattern, the fence preamble and
which broker calls are fenced (`sessiondCommandTouchesMaya`,
`runHostPowerShell`), or broker start/stop identity checks all matter.
Lock path, logging, and validation of broker paths do not, but read the diff to
confirm.

## Run the check

```sh
verification/remote-host-lock/check.sh          # ~25s: three configs, canaries, mutants
verification/remote-host-lock/check.sh --quick  # ~15s: skip the 3-controller config
```

Needs Java 11+. The pinned `tla2tools.jar` v1.7.4 is shared with
`verification/host-lock` and lives in
`${XDG_CACHE_HOME:-~/.cache}/maya-stall-verification`, checked by sha256.

- `PASS safety [pause]`: 2 controllers, stalls allowed outside the renew window.
- `PASS safety [nopause]`: 3 controllers, no stalls.
- `PASS safety [unfenced]`: 2 controllers, stalls allowed anywhere (no
  `RenewOutlivesOp`); the fence alone carries safety.
- `ok canary:...`: the model still reaches a started session, a reclaim, a kept
  lock, a delayed script creating an orphan lock, and Host reuse.
- `ok mutant:...`: removing that guard breaks the named property. The
  `*-without-fence` mutants run on `MC.unfenced.cfg`, the rest on `MC.pause.cfg`.
  `substitution matched nothing` means the spec text moved; fix the perl
  expression in `check.sh` so it removes the same guard.

## Read a failure

Read every result with the pstack `/statechart` skill. It retells a violation
as the shortest path in plain words and charts the state machine on a pass.
Map each action to Go with the coverage record, then retell the trace as script
and broker calls. Real bug or modelling error: if every guard the trace passes
exists in Go/PowerShell and every guard it skips is really absent, it is a real
bug. Report it in Go terms, and don't weaken the property or the model. If the
trace passes a guard the code has, or splits one `Invoke-WithLockMutex` script
or one broker call into two steps, fix the model.

## Update the model for an intended change

1. Find the actions for the changed functions (coverage record).
2. Edit `RemoteHostLock.tla`. One PowerShell script = one atomic host step. One
   broker call = one step. Any lock write must give a new `writeSeq` (content +
   `LastWriteTimeUtc` change). A timed-out script can have run, or can still
   run later (`pending`).
3. Run `check.sh` and fix any mutant regex that no longer matches.
4. Confirm the model matches the code at the new commit, then move the sync
   commit here and in the diff command above. Only after that confirmation.

## Coverage record

Sync commit: `5c955db`.

Model actions -> code (`internal/cli/`):
- `AcquireSend`: `acquireRemoteHostLock` first call, `remoteHostLockAcquireScript`, `New-LockFile`
- `AcquireRecover`: `acquireRemoteHostLock` `err != nil` branch, `readRemoteHostLockState`, exact content match (`state.Raw == content`)
- `StaleCheck`: `isStaleHostSideOwner` (host_pool.go; `HostClockLease` -> `LeaseExpired`, keptRun never stale), `Locked-Result` `leaseExpired` (age vs `leaseDurationSeconds`), `remoteHostLockSessionInactive`, `sessiondStatusProvesInactive`, `remoteHostLockReclaimAllowed`, `maxRemoteHostLockReclaimAttempts`
- `ReclaimSend`: `remoteHostLockReclaimScript` (exact content + leaseVersion compare, delete, create)
- `RunPending`: a timed-out acquire/reclaim script still queued on `Invoke-WithLockMutex`
- `MarkActive`, `MarkKept`: `hostSideLock.markActive`/`markKept` (host_pool.go), `replaceRemoteHostLockOwner`, `remoteHostLockReplaceScript`
- `Heartbeat`: `startHostLockHeartbeat`, `hostSideLock.renew` (fresh_run.go, host_pool.go); exact-content CAS, no expiry check
- `RenewBeforeStart`, `RenewBeforeStep`: `renewHostLockNow` before `startFreshSession`, `stagePayloadWithCancellation`, `execute`, `settle` (fresh_run.go)
- `StartSession`: `ggMayaSessiondBroker.StartFreshSession` (sessiond_broker.go): status, fenced `stopSessiondSession`, fenced `restartSessionBrokerTask`, `awaitFreshSession`; fence = `runHostPowerShell` with `hostLockFence.hostScript` = `remoteHostLockFenceScript(lockPath, "lockToken", ...)` (`newRemoteHostSideLock`)
- `BindSession`: `hostSideLock.bindSession` from `startFreshSession` (brokerSessionId, exact-content CAS)
- `ScenarioStep`: `Broker.RunScenario` and other fenced calls (`runSessiondCLIWithMarker`, `sessiondCommandTouchesMaya`: start/stop/call), payload staging, artifact collection
- `StopSession`: `ggMayaSessiondBroker.StopSession` status read + session-id check
- `StopFenced`: the fenced `stopSessiondSession` inside `StopSession`
- `Release`: `hostSideLock.release`, `removeRemoteHostLock`, `remoteHostLockRemoveScript`, `Clear-OwnedLiveLockMutex`
- `StopKept`: `stopKeptRunAfterBrokerStop` (run_state.go): `verifyKeptHostSideLockForRun`, `StopRetainedRun` fenced by `keptHostLockFence` (`remoteHostLockFenceScript(lockPath, "keptRun", runID)`), `releaseHostSideLock` -> `removeRemoteHostLockForRun`
- `IdleExpire`: `Locked-Result` (age since `LastWriteTimeUtc` >= `leaseDurationSeconds`, `hostSideLockLeaseDuration` 1h vs `hostSideLockHeartbeatInterval` 1m). Main has no hard lifetime.
- `FenceOK`: `remoteHostLockFenceScript`: lock exists, field matches, kept -> ok, else `leaseExpired` refuses, then `SetLastWriteTimeUtc` (`Touch`)
- Mutex: `Invoke-WithLockMutex`. The mutex file is held open with FileShare None, so stale-break and `Clear-OwnedLiveLockMutex` can delete only an abandoned mutex. That is why each script is atomic.

Mutants (check.sh) -> guard removed:
- `reclaim-ignores-broker`: `remoteHostLockSessionInactive` / `sessiondStatusProvesInactive`
- `reclaim-kept`: `isStaleHostSideOwner` keptRun check and `remoteHostLockReclaimAllowed` `State != "kept"`
- `reclaim-without-cas`: `remoteHostLockReclaimScript` exact content + leaseVersion compare
- `release-without-cas`: `remoteHostLockRemoveScript` exact-content compare
- `start-without-fence`, `step-without-fence`, `stop-without-fence`: `remoteHostLockFenceScript` preamble on `restartSessionBrokerTask`/`stopSessiondSession`, `call`, and `StopSession`'s stop

`StopSession`'s session-id check is not a mutant: with the fence, a foreign
live session cannot coexist with our lock in this model. Go keeps it for what
the model omits (standing session, Maya restarts).

## Bounds and assumptions

- pause: 2 controllers, stalls allowed outside the renew window, 2 reclaim
  attempts. nopause: 3 controllers, no stalls. unfenced: 2 controllers,
  stalls allowed anywhere. One run per controller. Maya may crash at any time.
- `RenewOutlivesOp` (pause, nopause): after a successful `renewHostLockNow`
  the next broker call lands before the lease can expire. `unfenced` drops it.
- Windows file semantics as above. Content equality and LastWriteTimeUtc ticks
  are abstracted to "no write since observed".
- The fence + broker action is one step: one Host script, under the mutex,
  then the action with a lease just renewed to 1h.
- Not covered: the local repo lock (`acquireHostLock` and its pid-based
  stale check), `withLocalHostSideMutex`, local-sessiond and fake-SSH
  directory locks (`acquireLocalHostSideLock`, `hostSideLock.claim`), the
  Embedded deadline sweep, `verifyHostSideLockForRun` in run-scoped ops, clock
  skew, the 10-minute mutex stale-break while a holder is alive but stuck, and
  liveness.
