-------------------------- MODULE EmbeddedLedger --------------------------
(***************************************************************************)
(* Embedded Mode: Fresh Run controllers, explicit `stop`, the kept-session *)
(* sweep, and Run Ledger retention prune acting on one repo and one Maya   *)
(* Host. Per run:                                                          *)
(*   ledger = .maya-stall/state/ledger/runs/<id>/run.json state            *)
(*   rs     = .maya-stall/state/runs/<id>/ (Run State with manifest.json)  *)
(*   rphase = run-record.json StopPhase                                    *)
(*   lock   = .maya-stall/state/locks/hosts/<host>.lock (repo Host Lock)   *)
(* Each controller action is one Go step: a ledger-lock section, a local   *)
(* Host Lock mutex section, or one atomic file write. Controllers may      *)
(* crash between any two actions. No lock serializes `stop` or the sweep   *)
(* with a finishing controller (main has no deadline-authority lock), so   *)
(* they interleave between controller steps; `stop` itself is one atomic   *)
(* action (its internal interleavings are not explored).                   *)
(*                                                                         *)
(* Lock checks follow the repo Host Lock (non-authoritative hosts: the     *)
(* fake host, or a host with no host-side lock dir). The host-side (SSH)   *)
(* Host Lock is abstracted: reclaimable whenever the repo lock's owner is  *)
(* dead. Its protocol is in verification/remote-host-lock.                 *)
(*                                                                         *)
(* FixOrphan switches on the candidate fix. FALSE = code at sync commit.   *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS Runs, NoRun, FixOrphan

LedgerStates == {"none", "submitted", "cleanup", "kept", "terminal"}
\* rs: "pre" = Run State without run-record.json (before writeRunRetentionRecord("running"))
RSStates == {"none", "pre", "running", "kept"}
\* rphase: run-record.json StopPhase "", session-stopped, broker-cleaned, host-lock-released
RPhases == {"none", "stopped", "cleaned", "released"}
LockKinds == {"none", "pid", "active", "kept"}
PCs == {"idle", "accepted", "pruned", "swept", "locked", "recorded", "active",
        "stopped", "preserving", "preserved", "brokered", "released",
        "keeping", "keptrec", "cleaned", "cleanfail", "done", "dead"}

VARIABLES
  ledger, lphase,   \* ledger state; ledger StopPhase = host-lock-released
  rs, rphase,       \* Run State; run-record StopPhase
  lockRun, lockKind,\* repo Host Lock owner
  due,              \* run's keep deadline has passed
  expired,          \* ghost: the sweep stopped this run
  pc,               \* controller process
  everAccepted

vars == <<ledger, lphase, rs, rphase, lockRun, lockKind, due, expired, pc, everAccepted>>

Init ==
  /\ ledger = [x \in Runs |-> "none"] /\ lphase = [x \in Runs |-> FALSE]
  /\ rs = [x \in Runs |-> "none"] /\ rphase = [x \in Runs |-> "none"]
  /\ lockRun = NoRun /\ lockKind = "none"
  /\ due = [x \in Runs |-> FALSE]
  /\ expired = [x \in Runs |-> FALSE]
  /\ pc = [x \in Runs |-> "idle"]
  /\ everAccepted = [x \in Runs |-> FALSE]

Dead(x) == pc[x] = "dead"
\* The recorded controller process no longer runs (candidate fix reads its pid).
ControllerGone(x) == pc[x] \in {"dead", "done"}
LockNames(x) == lockRun = x /\ lockKind # "none"
\* isStaleHostLock: pid-based; a kept lock (keptRun, no pid) is never stale
LockFree == lockKind = "none" \/ (lockKind \in {"pid", "active"} /\ Dead(lockRun))
\* removeRepoHostLockForRun: removes only a lock naming x as activeRun/keptRun
ReleaseLockOf(x) ==
  IF lockRun = x /\ lockKind \in {"active", "kept"}
    THEN lockRun' = NoRun /\ lockKind' = "none"
    ELSE UNCHANGED <<lockRun, lockKind>>
\* replaceHostLockOwner (markActive/markKept) overwrites whatever lock file
\* exists; it fails only when the file is missing.
Overwrite(x, kind) == lockKind # "none" /\ lockRun' = x /\ lockKind' = kind

SetPC(x, v) == pc' = [pc EXCEPT ![x] = v]

-----------------------------------------------------------------------------
(* Operator `stop <x>`: stopRun -> prepareRunLedgerForStop -> stopKeptRun   *)
(* -> updateRunLedgerAfterStop (run_state.go, run_ledger.go)                *)

\* reconcileSubmittedRunLedgerForStopUnlocked guards (ledger "submitted")
ReconcileOK(x) ==
  CASE rs[x] = "none" -> FALSE                    \* manifest not found
    [] rs[x] = "pre" -> FixOrphan /\ Dead(x)      \* fallback Run Record: kept, no bundle -> ErrNotExist
    [] rs[x] = "running" /\ rphase[x] = "none" ->
         IF FixOrphan THEN Dead(x)                \* fix: controller pid in Run Record
         \* repoHostLockUsesActiveRun: activeRun -> requireStaleSubmittedRunController;
         \* keptRun -> no controller check; other owner or missing -> error
         ELSE \/ lockRun = x /\ lockKind = "active" /\ Dead(x)
              \/ lockRun = x /\ lockKind = "kept"
    [] OTHER -> TRUE                              \* kept, or StopPhase set: no controller check

\* stopKeptRunAfterBrokerStop lock checks, after the ledger left "submitted"
KeptStopOK(x) ==
  CASE rs[x] = "none" ->                          \* manifest not found
         \/ lphase[x]                             \* ledger host-lock-released: cleanupRunState
         \/ lockRun = x /\ lockKind = "kept"      \* releaseMatchingKeptHostLock
    [] FixOrphan -> TRUE                          \* fix: Run State decides, not the repo lock
    [] rphase[x] = "released" \/ lphase[x] -> TRUE \* finishReleasedStopRun
    [] rs[x] = "running" /\ rphase[x] = "none" -> \* cleanupFailedRunning
         \/ lockRun = x /\ lockKind = "active" /\ Dead(x)
         \/ lockRun = x /\ lockKind = "kept"
    [] rphase[x] \in {"stopped", "cleaned"} ->    \* ensureRunHasCleanupPendingLock
         \/ lockRun = x /\ lockKind \in {"active", "kept"}
         \/ rphase[x] = "cleaned" /\ lockKind = "none"  \* allowMissingMirror for broker-cleaned
    [] rs[x] = "kept" -> lockRun = x /\ lockKind = "kept"  \* ensureRunHasKeptLock
    [] OTHER -> FALSE                             \* "pre": LegacyMissingRecord

StopOK(x) ==
  /\ FixOrphan => ControllerGone(x)               \* fix: refuse while the recorded controller lives
  /\ CASE ledger[x] = "submitted" -> ReconcileOK(x) /\ KeptStopOK(x)
       [] ledger[x] \in {"cleanup", "kept"} -> KeptStopOK(x)
       [] OTHER -> FALSE

\* Successful stop: broker stop, release, finishReleasedStopRun, ledger terminal
RecoverEffects(y) ==
  /\ rs' = [rs EXCEPT ![y] = "none"]
  /\ rphase' = [rphase EXCEPT ![y] = "none"]
  /\ ledger' = [ledger EXCEPT ![y] = "terminal"]
  /\ lphase' = [lphase EXCEPT ![y] = FALSE]
  /\ ReleaseLockOf(y)

Stop(x) ==
  /\ ledger[x] \in {"submitted", "cleanup", "kept"} /\ StopOK(x)
  /\ RecoverEffects(x)
  /\ UNCHANGED <<due, expired, pc, everAccepted>>

\* stopKeptRun fails after prepareRunLedgerForStop: reconcile moved a submitted
\* ledger to cleanup-failed, or updateRunLedgerAfterStopUnlocked records
\* cleanup-failed only over kept or cleanup-failed, never over terminal.
StopFails(x) ==
  /\ FixOrphan => ControllerGone(x)
  /\ \/ ledger[x] \in {"cleanup", "kept", "terminal"}
     \/ ledger[x] = "submitted" /\ ReconcileOK(x)
  /\ ledger' = [ledger EXCEPT ![x] = IF ledger[x] = "terminal" THEN "terminal" ELSE "cleanup"]
  /\ UNCHANGED <<lphase, rs, rphase, lockRun, lockKind, due, expired, pc, everAccepted>>

-----------------------------------------------------------------------------
(* Controller: freshRunLifecycle.Run in fresh_run.go                        *)

\* accept: Run State dir + manifest, ledger Initialize (submitted)
Accept(x) ==
  /\ pc[x] = "idle"
  /\ ledger' = [ledger EXCEPT ![x] = "submitted"]
  /\ rs' = [rs EXCEPT ![x] = "pre"]
  /\ everAccepted' = [everAccepted EXCEPT ![x] = TRUE]
  /\ SetPC(x, "accepted")
  /\ UNCHANGED <<lphase, rphase, lockRun, lockKind, due, expired>>

\* setup: ledger Prune under the exclusive root lock
Prune(x) ==
  /\ pc[x] = "accepted"
  /\ ledger' = [y \in Runs |-> IF y # x /\ ledger[y] = "terminal" THEN "none" ELSE ledger[y]]
  /\ SetPC(x, "pruned")
  /\ UNCHANGED <<lphase, rs, rphase, lockRun, lockKind, due, expired, everAccepted>>

\* setup BeforeHostLock: sweepKeptSessions, once per host. listKeptRuns finds
\* runs only through a repo Host Lock naming them as keptRun; an expired one
\* goes through stopRunWithKeptExpiryEvent (the same stop path).
Sweep(x) ==
  /\ pc[x] = "pruned"
  /\ SetPC(x, "swept")
  /\ IF FixOrphan
       THEN \* candidate fix: enumerate Run State, not lock files
            \E z \in Runs \cup {NoRun} :
              IF z # NoRun /\ rs[z] \in {"pre", "running", "kept"} /\ due[z] /\ StopOK(z)
                THEN RecoverEffects(z) /\ expired' = [expired EXCEPT ![z] = TRUE]
                     /\ UNCHANGED <<due, everAccepted>>
                ELSE UNCHANGED <<ledger, lphase, rs, rphase, lockRun, lockKind, due, expired, everAccepted>>
       ELSE LET y == lockRun IN
            IF lockKind = "kept" /\ rs[y] \in {"running", "kept"} /\ due[y] /\ StopOK(y)
              THEN RecoverEffects(y) /\ expired' = [expired EXCEPT ![y] = TRUE]
                   /\ UNCHANGED <<due, everAccepted>>
              ELSE UNCHANGED <<ledger, lphase, rs, rphase, lockRun, lockKind, due, expired, everAccepted>>

\* acquireRunHostLock: host-side lock, then repo lock (stale pid is broken)
Acquire(x) ==
  /\ pc[x] = "swept" /\ LockFree
  /\ lockRun' = x /\ lockKind' = "pid"
  /\ SetPC(x, "locked")
  /\ UNCHANGED <<ledger, lphase, rs, rphase, due, expired, everAccepted>>

\* host selection gives up (HostLockWait over): early failure cleans own state
GiveUp(x) ==
  /\ pc[x] = "swept" /\ ~LockFree
  /\ rs' = [rs EXCEPT ![x] = "none"]
  /\ ledger' = [ledger EXCEPT ![x] = "terminal"]
  /\ SetPC(x, "done")
  /\ UNCHANGED <<lphase, rphase, lockRun, lockKind, due, expired, everAccepted>>

\* setup: writeRunRetentionRecord(... "running")
WriteRecord(x) ==
  /\ pc[x] = "locked"
  /\ rs' = [rs EXCEPT ![x] = "running"]
  /\ SetPC(x, "recorded")
  /\ UNCHANGED <<ledger, lphase, rphase, lockRun, lockKind, due, expired, everAccepted>>

\* setup: host.markActive (replaceHostLockOwner)
MarkActive(x) ==
  /\ pc[x] = "recorded" /\ Overwrite(x, "active")
  /\ SetPC(x, "active")
  /\ UNCHANGED <<ledger, lphase, rs, rphase, due, expired, everAccepted>>

\* Scenario runs, stop-after always stops Maya
StopMaya(x) ==
  /\ pc[x] = "active"
  /\ SetPC(x, "stopped")
  /\ UNCHANGED <<ledger, lphase, rs, rphase, lockRun, lockKind, due, expired, everAccepted>>

\* retainSession (stop-after never / keep-on-failure): host.markKept ...
KeepMark(x) ==
  /\ pc[x] = "active" /\ Overwrite(x, "kept")
  /\ SetPC(x, "keeping")
  /\ UNCHANGED <<ledger, lphase, rs, rphase, due, expired, everAccepted>>

\* ... RetainRun, then writeRunRetentionRecord(kept)
KeepRecord(x) ==
  /\ pc[x] = "keeping"
  /\ IF rs[x] # "none"   \* a write after `stop` removed Run State leaves no manifest
       THEN rs' = [rs EXCEPT ![x] = "kept"] /\ rphase' = [rphase EXCEPT ![x] = "none"]
       ELSE UNCHANGED <<rs, rphase>>
  /\ SetPC(x, "keptrec")
  /\ UNCHANGED <<ledger, lphase, lockRun, lockKind, due, expired, everAccepted>>

\* preserveStoppedRunForCleanup: writeRunRetentionRecord(kept, session-stopped) ...
PreserveRecord(x) ==
  /\ pc[x] = "stopped"
  /\ rs' = [rs EXCEPT ![x] = "kept"]
  /\ rphase' = [rphase EXCEPT ![x] = "stopped"]
  /\ SetPC(x, "preserving")
  /\ UNCHANGED <<ledger, lphase, lockRun, lockKind, due, expired, everAccepted>>

\* ... then host.markKept. A missing lock file fails markKept, and
\* completeCleanupAfterRecoveryFailure runs the same checkpoints, release, and
\* cleanup as finishStoppedRunCleanup, so both continue at "preserved".
PreserveMark(x) ==
  /\ pc[x] = "preserving"
  /\ IF lockKind # "none" THEN Overwrite(x, "kept") ELSE UNCHANGED <<lockRun, lockKind>>
  /\ SetPC(x, "preserved")
  /\ UNCHANGED <<ledger, lphase, rs, rphase, due, expired, everAccepted>>

\* finishStoppedRunCleanup: record broker-cleaned, then
\* checkpointRunLedgerStopPhase(broker-cleaned) -> cleanup-failed. The record
\* write recreates a manifest-less dir if `stop` already removed Run State;
\* that dir is not Run State to stop or the sweep, so rs stays "none".
\* A missing Run Record falls back (readRunRetentionRecord), so only a pruned
\* ledger fails the checkpoint.
CleanBroker(x) ==
  /\ pc[x] = "preserved"
  /\ IF ledger[x] # "none"
       THEN /\ ledger' = [ledger EXCEPT ![x] = "cleanup"]
            /\ rphase' = [rphase EXCEPT ![x] = IF rs[x] # "none" THEN "cleaned" ELSE @]
            /\ SetPC(x, "brokered")
       ELSE /\ SetPC(x, "cleanfail")
            /\ UNCHANGED <<ledger, rphase>>
  /\ UNCHANGED <<lphase, rs, lockRun, lockKind, due, expired, everAccepted>>

\* finishStoppedRunCleanup: host.release (removeRepoHostLockForRun: only a lock naming x)
ReleaseLock(x) ==
  /\ pc[x] = "brokered"
  /\ ReleaseLockOf(x)
  /\ SetPC(x, "released")
  /\ UNCHANGED <<ledger, lphase, rs, rphase, due, expired, everAccepted>>

\* finishStoppedRunCleanup: record + ledger checkpoint host-lock-released, cleanupRunState
FinishRelease(x) ==
  /\ pc[x] = "released"
  /\ IF ledger[x] # "none"
       THEN /\ ledger' = [ledger EXCEPT ![x] = "cleanup"]
            /\ lphase' = [lphase EXCEPT ![x] = TRUE]
            /\ rs' = [rs EXCEPT ![x] = "none"]
            /\ rphase' = [rphase EXCEPT ![x] = "none"]
            /\ SetPC(x, "cleaned")
       ELSE /\ SetPC(x, "cleanfail")
            /\ UNCHANGED <<ledger, lphase, rs, rphase>>
  /\ UNCHANGED <<lockRun, lockKind, due, expired, everAccepted>>

\* Run() defer: ledger Finalize -> finalizeRunLedgerUnlocked overwrites any state.
\* A pruned ledger makes withRunLedgerLock fail with "not found".
Finalize(x) ==
  /\ pc[x] \in {"cleaned", "cleanfail", "keptrec"}
  /\ IF ledger[x] = "none" THEN UNCHANGED ledger
     ELSE ledger' = [ledger EXCEPT ![x] = IF pc[x] = "keptrec" THEN "kept" ELSE "terminal"]
  /\ SetPC(x, "done")
  /\ UNCHANGED <<lphase, rs, rphase, lockRun, lockKind, due, expired, everAccepted>>

Crash(x) ==
  /\ pc[x] \notin {"idle", "done", "dead"}
  /\ SetPC(x, "dead")
  /\ UNCHANGED <<ledger, lphase, rs, rphase, lockRun, lockKind, due, expired, everAccepted>>

\* Keep deadline passes; only for dead controllers or finished kept runs.
DeadlineElapse(x) ==
  /\ (Dead(x) \/ (pc[x] = "done" /\ rs[x] = "kept")) /\ rs[x] # "none" /\ ~due[x]
  /\ due' = [due EXCEPT ![x] = TRUE]
  /\ UNCHANGED <<ledger, lphase, rs, rphase, lockRun, lockKind, expired, pc, everAccepted>>

-----------------------------------------------------------------------------

Next ==
  \E x \in Runs :
    \/ Accept(x) \/ Prune(x) \/ Sweep(x) \/ Acquire(x) \/ GiveUp(x)
    \/ WriteRecord(x) \/ MarkActive(x) \/ StopMaya(x) \/ KeepMark(x) \/ KeepRecord(x)
    \/ PreserveRecord(x) \/ PreserveMark(x) \/ CleanBroker(x) \/ ReleaseLock(x)
    \/ FinishRelease(x) \/ Finalize(x) \/ Crash(x) \/ DeadlineElapse(x)
    \/ Stop(x) \/ StopFails(x)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* Properties                                                               *)

TypeOK ==
  /\ ledger \in [Runs -> LedgerStates] /\ rs \in [Runs -> RSStates]
  /\ rphase \in [Runs -> RPhases]
  /\ lockKind \in LockKinds /\ pc \in [Runs -> PCs]
  /\ expired \in [Runs -> BOOLEAN]

\* A crashed run that is not finished can always be finished by `stop`.
CrashedRunRecoverable ==
  \A x \in Runs : (Dead(x) /\ ledger[x] \in {"submitted", "cleanup", "kept"}) => StopOK(x)

\* Retention prune never removes the ledger of a run that still has Run State.
PruneKeepsUnfinished ==
  \A x \in Runs : (everAccepted[x] /\ ledger[x] = "none") => rs[x] = "none"

\* A live controller's Run State is never cleaned up under it.
LiveRunIntact ==
  \A x \in Runs : pc[x] \in {"recorded", "active", "stopped"} => rs[x] = "running"

\* The sweep stops a run only after its keep deadline passed.
SweepOnlyExpired == \A x \in Runs : expired[x] => due[x]

\* Once terminal, a ledger stays terminal until prune removes it. Exception:
\* the run's own finishing controller checkpoints cleanup-failed over a
\* terminal record that a concurrent `stop` wrote, then Finalize restores it.
TerminalStable ==
  [][\A x \in Runs : (ledger[x] = "terminal" /\ pc[x] \notin {"preserved", "released"})
                     => ledger'[x] \in {"terminal", "none"}]_vars

=============================================================================
