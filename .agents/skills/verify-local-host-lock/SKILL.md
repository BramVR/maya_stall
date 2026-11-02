---
name: verify-local-host-lock
description: "Check changes to the local file Host Lock (repo .maya-stall/state/locks/hosts/<host>.lock, the local-sessiond workRoot host.lock, withLocalHostSideMutex, acquireHostLockAtDirWithContent, isStaleHostLock, acquireLocalHostSideLock, markHostLockActive/Kept, removeRepoHostLockForRun, acquireRunHostLock ordering) against the TLA+ model in verification/local-host-lock. Use when a diff touches those functions in internal/cli/host_pool.go or run_state.go."
---

# Verify the local file Host Lock

`verification/local-host-lock/` models Embedded Mode processes in one checkout
(`run`, `doctor`, `stop`) contending for one Maya Host through two files:

- the mutex `<lockDir>/.mutexes/<host>.lock`, taken by `withLocalHostSideMutex`
- the lock `<lockDir>/<host>.lock`, taken by `acquireHostLock` inside the mutex,
  then rewritten by `markHostLockActive` / `markHostLockKept`

Both are taken by `acquireHostLockAtDirWithContent`: `os.Link`, and on EEXIST
read the file, judge `isStaleHostLock` (owner pid gone; `keptRun` never stale),
`os.Remove(path)`, retry. Processes can be killed at any step; their files stay.

Three modes:
- `ssh`: `acquireRunHostLock` takes the host-side lock first (an ideal lock
  here; its own model is `verification/remote-host-lock`, skill
  `verify-remote-host-lock`) and releases it after the local lock.
- `local`: the file lock is the only Host Lock. This is the same code shape as
  `acquireLocalHostSideLock`, which on main backs the production
  `local-sessiond` Windows runtime (`transport: local`,
  `<workRoot>/state/locks/hosts/host.lock`, staleness
  `isRecoverableLocalSessiondHostLock`: lease expired and broker inactive), the
  fake Host with absolute `workRoot`, the fake-SSH hook, and the Control Plane
  shared fake-host lock used across processes. The model's pid-based lock
  staleness stands in for the lease check (a dead owner stops renewing).
- `local-fixed`: `local` with a candidate fix (`AtomicBreak`): a stale break
  removes the file only if its content still equals what the staleness read saw.

## What it promises

- `LockExclusive`: two live controllers never both hold the local Host Lock,
  and nobody holds it while another run's Kept Session is live.
  Harm if broken: two runs drive one Maya Host, or a Kept Session the user is
  inspecting gets replaced.
- `HolderFileIntact`: a live holder's lock file still names it.
  Harm if broken: another process removed or replaced a lock that is in use, so
  the next acquirer walks in.

## Known finding (expected failure)

Filed as https://github.com/BramVR/maya_stall/issues/176.

`finding:stale-break-toctou`: in `local` mode, one process killed inside
`withLocalHostSideMutex` (holding the mutex, its pid also in the lock) leaves
both files naming a dead pid. Two new processes both read the mutex as stale;
one removes it and links its own; the other then `os.Remove`s that fresh mutex
by path and links its own. Both are now inside the mutex, and the same race on
the lock file lets both believe they hold it. Go repro (scratch copy):
`acquireHostLockAtDirWithContent` with the real `isStaleHostLock`, interleaving
pinned, leaves process A holding a lock whose file now names B.
`ssh` mode is safe because the host-side lock is taken first.

Re-checked on `5c955db`: still reproduces. `withLocalHostSideMutex`
(host_pool.go:1015) takes the mutex with `acquireHostLockAtDir(mutexDir)`
(:1032), and `acquireHostLockAtDirWithContent` still reads `isStale(lockPath)`
(:1072) and then `os.Remove(lockPath)` by path (:1079) with nothing in
between. A scratch Go test (copy of main, `isStale` wrapper around the real
`isStaleHostLock` pinning the interleaving) returned acquired for both callers
with the file naming the second. Main widens the reach: `local-sessiond`
(54ab1cd) uses this path as its authoritative cross-checkout Host Lock
(`acquireLocalHostSideLock`), and 5c955db's local fence (`hostSideLock.claim`
-> `claimOwner`) and `keptHostLockFence` run under the same mutex. Breaking the
lock file there also needs an expired lease and an inactive broker; breaking
the mutex needs only a dead pid. When the Go code
gets a compare-and-remove (or OS file locks), move `local` from the finding
probe to `safety` in `check.sh` and drop `AtomicBreak` if it no longer differs.

## Does my change touch it?

```sh
git diff --stat 5c955db -- internal/cli/host_pool.go internal/cli/run_state.go internal/cli/run_ledger.go internal/cli/process_posix.go internal/cli/process_windows.go
```

Matters: `acquireHostLock`, `acquireRunHostLock` (order of host-side vs local,
release order), `withLocalHostSideMutex`, `acquireHostLockAtDir*`,
`isStaleHostLock`, `processExists` (`process_*.go`), `markHostLock*`,
`replaceHostLockOwner*`, `removeRepoHostLockForRun`, the unmarked `localRelease`
closure, `acquireLocalHostSideLock` (and its `replace`/`claimOwner`/`remove`
closures), `isRecoverableLocalSessiondHostLock`, `stopKeptRunAfterBrokerStop` /
`finishReleasedStopRun` order, `requireStaleSubmittedRunController`. Lock
content formatting that keeps `pid:` / `activeRun:` / `keptRun:` semantics does
not; read the diff to confirm.

## Run the check

```sh
verification/local-host-lock/check.sh                # ~15s: safety, finding, canaries, mutants
verification/local-host-lock/check.sh --only-safety
```

Java 11+; the pinned `tla2tools.jar` v1.7.4 is cached in
`${XDG_CACHE_HOME:-~/.cache}/maya-stall-verification` with a sha256 check.

- `PASS safety [ssh|ssh-full|local-fixed|local-fixed-full]`: properties hold.
- `ok finding:stale-break-toctou`: the known finding still reproduces.
- `ok canary: ...`: the model reaches kept locks left behind, stale lock and
  mutex breaks, `stop` of a kept run, and Host reuse. A canary that stops firing
  means the model lost behaviour.
- `ok mutant:<name>`: removing that guard breaks a property. `substitution
  matched nothing` means the spec text moved; fix the perl in `check.sh`.

## Read a failure

TLC prints the shortest trace with an action per state. Read every result,
pass or fail, with the pstack `/statechart` skill: it explains a violation as
its shortest path in plain words and, on a pass, charts the state machine.

Retell the trace with the coverage table. `MutexCheck` then `MutexRemoveStale`
is "read the mutex file, judged stale, `os.Remove`d it"; `LockCheck` then
`LockRemoveStale` is the same for the lock file; `Crash` is a killed process.

Real bug or modelling error: open the Go for each step. If Go really allows
every step (guards present where the trace passes, absent where it skips), it
is a real bug: report it in Go terms; do not weaken the property or model, and
do not change product code unless asked. If Go would block a step, or does two
steps the model splits under one mutex (or one step the model merges), fix the
action and rerun.

## Update the model for an intended change

1. Find the actions for the changed functions (table below).
2. Edit `LocalHostLock.tla`: guards and effects match the Go. A step under
   `withLocalHostSideMutex` happens between `MutexLink` and the action that
   sets `mutexOwner' = NoProc`. File reads and removes that Go does separately
   stay separate actions.
3. Run `check.sh`; fix mutant perl expressions that no longer match.
4. Confirm each action matches the Go at the new commit, then move the sync
   commit here and in the `git diff` command. Only after that confirmation.

## Coverage record

Sync commit: `5c955db`.

Actions -> Go (`internal/cli/`):
- `Start`: `acquireRunHostLock` (host-side first in ssh mode), `acquireHostLock`
- `MutexLink`, `MutexCheck`, `MutexRemoveStale`, `MutexRelease`: `withLocalHostSideMutex` -> `acquireHostLockAtDir(mutexDir)` -> `acquireHostLockAtDirWithContent`, `isStaleHostLock`, `processExists`; 5s timeout
- `LockLink`, `LockCheck`, `LockRemoveStale`: `acquireHostLock` -> `acquireHostLockAtDir(lockDir)` (same primitive; `acquireLocalHostSideLock` with `isStaleHostSideLock` or `isRecoverableLocalSessiondHostLock` has the same shape)
- `WantMark`, `Mark`: `runHostLock.markActive` / `markKept` -> `markHostLockActiveWithAuthority` / `markHostLockKeptWithAuthority` -> `replaceHostLockOwner` (no content check); kept hand-off `run.releaseHostLock = false` in `fresh_run.go`
- `WantRelease`, `ReleaseOwned`: `runHostLock.release` with `ownedRunID` -> `removeRepoHostLockForRun` (run_state.go)
- `ReleaseUnmarked`: `runHostLock.release` without `ownedRunID` -> `localRelease` = `os.Remove(lockPath)`, no mutex
- `ReleaseRemote`: `hostSideLock.release` after the local release
- `StartStop`, `StopRemove`: `stopKeptRunAfterBrokerStop` (run_state.go): `StopRetainedRun`, `releaseHostSideLock`, `finishReleasedStopRun` -> `removeRepoHostLockForRun`
- `RemoteReclaim`: host-side reclaim of a dead active owner (`remoteHostLockReclaimAllowed`); kept never reclaimed

Mutants (check.sh) -> guard removed:
- `local-before-remote`: `acquireRunHostLock` taking `acquireHostSideLock` first
- `kept-is-stale`, `live-pid-is-stale`: `isStaleHostLock` keptRun and `processExists` checks
- `stop-without-owner-check`: `removeRepoHostLockForRun` keptRun/activeRun match
- `Crash`: process killed anywhere

## Bounds and assumptions

- quick: 3 processes, 1 crash. full: 4 processes, 2 crashes.
- One Maya Host, one checkout. Each process runs once (acquire, mark, release
  or keep, or `stop` a kept run).
- File link, rename, remove, and read are each atomic. The host-side lock is
  ideal (see `verify-remote-host-lock`).
- Not covered: pid reuse and Windows `OpenProcess` on exited processes (both
  make a dead owner look alive: fail-closed, availability only), the
  content-checked `replace`/`claimOwner`/`remove` closures of
  `acquireLocalHostSideLock` and the 5c955db local fence
  (`hostSideLock.claim`, `keptHostLockFence` claim), the lease and
  broker-inactivity form of local-sessiond staleness (abstracted to a dead
  pid), several checkouts sharing one `local-sessiond` work root as distinct
  repo locks, liveness.
