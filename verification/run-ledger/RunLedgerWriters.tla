-------------------------- MODULE RunLedgerWriters --------------------------
(***************************************************************************)
(* Writers of one Host-Agent-backed run's Control Plane Run Ledger:        *)
(*   E = .maya-stall/state/ledger/runs/<id>/events.jsonl                   *)
(*   R = .maya-stall/state/ledger/runs/<id>/run.json (active or terminal)  *)
(*   L = .maya-stall/state/runs/<id>/events.jsonl, the Control Plane's     *)
(*       local Run State events that syncRunLedgerArtifacts copies from.   *)
(*                                                                         *)
(* Each file write is atomic (writeRunLedgerBytes: temp, fsync, rename).   *)
(* A multi-file update is not. "lock" is withRunLedgerLock; readers        *)
(* (ReadEvents, Snapshot) take it, so they never see a half-done locked    *)
(* update. Event payloads are abstracted to their origin:                  *)
(*   "P" assigned prefix, "A" Agent progress, "T" Agent terminal transfer, *)
(*   "Fail" run.failed written by the Control Plane                        *)
(*                                                                         *)
(* FixLock / FixSync switch on the candidate fixes (see check.sh). Both    *)
(* FALSE is the code at the sync commit.                                   *)
(***************************************************************************)
EXTENDS Naturals, Sequences, TLC

CONSTANTS MaxProgress, MaxAttempts, MaxCrashes, FixLock, FixSync

VARIABLES
  E, R, L, lExists,       \* durable files
  dPhase,                 \* durable assignment state (assignments/<id>.json)
  phase,                  \* Control Plane memory: assignment state
  op,                     \* in-flight handler step
  ack,                    \* Snapshot events the in-flight handler holds
  lock,                   \* withRunLedgerLock holder
  follow,                 \* a refreshTransient follow stream is connected
  rf, rr, stamp,          \* follow-stream refresh: phase, run.json read, L stamp
  agentAlive, progress, attempts, crashes,
  streamed                \* ghost: seq -> origin a client has received

vars == <<E, R, L, lExists, dPhase, phase, op, ack, lock, follow, rf, rr, stamp,
          agentAlive, progress, attempts, crashes, streamed>>

Prefix == <<"P">>

Init ==
  /\ E = Prefix /\ R = "active" /\ L = Prefix /\ lExists = TRUE
  /\ dPhase = "confirmed" /\ phase = "confirmed" /\ op = "none"
  /\ ack = <<>> /\ lock = "none"
  /\ follow = TRUE /\ rf = "idle" /\ rr = "active" /\ stamp = Len(Prefix)
  /\ agentAlive = TRUE /\ progress = 0 /\ attempts = 0 /\ crashes = 0
  /\ streamed = <<>>

HasFail(s) == \E i \in 1..Len(s) : s[i] = "Fail"
IsPrefix(a, b) == Len(a) <= Len(b) /\ SubSeq(b, 1, Len(a)) = a

(* syncRunLedgerArtifacts: copy L over E when L's last sequence is larger    *)
(* (selectRunLedgerArtifactSource / lastRunLedgerEventSequence).            *)
(* FixSync: an assignment-backed ledger never copies from L.                  *)
Sync(e, l, exists) ==
  IF exists /\ Len(l) > Len(e) /\ ~FixSync THEN l ELSE e

(* preserveAcknowledgedFailure: acknowledged events + run.failed if missing  *)
Preserve(a) == IF HasFail(a) THEN a ELSE Append(a, "Fail")

\* transition-store Replace of run.json; unlocked in the code, locked under FixLock
UnlockedWriteOK == FixLock => lock = "none"

-----------------------------------------------------------------------------
(* serveHostAgentProgress -> UpdateSnapshot (locked; events then run.json)    *)
Progress ==
  /\ agentAlive /\ phase = "confirmed" /\ op = "none" /\ lock = "none"
  /\ progress < MaxProgress
  /\ E' = Append(E, "A") /\ progress' = progress + 1
  /\ UNCHANGED <<R, L, lExists, dPhase, phase, op, ack, follow, lock, rf, rr, stamp,
                 agentAlive, attempts, crashes, streamed>>

(* serveHostAgentFail: Snapshot, then finishEarlyFailure appends run.failed to L *)
FailStart ==
  /\ agentAlive /\ phase = "confirmed" /\ op = "none" /\ attempts < MaxAttempts /\ lock = "none"
  /\ ack' = E /\ L' = Append(L, "Fail") /\ op' = "fail.sync"
  /\ attempts' = attempts + 1
  /\ UNCHANGED <<E, R, lExists, dPhase, phase, follow, lock, rf, rr, stamp,
                 agentAlive, progress, crashes, streamed>>

(* FinalizeAcknowledgedFailure step 1: finalizeRunLedgerUnlocked             *)
FailSync ==
  /\ op = "fail.sync" /\ lock = "none"
  /\ lock' = "op" /\ R' = "terminal" /\ E' = Sync(E, L, lExists) /\ op' = "fail.preserve"
  /\ UNCHANGED <<L, lExists, dPhase, phase, ack, follow, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

(* step 2: preserveAcknowledgedFailure                                       *)
FailPreserve ==
  /\ op = "fail.preserve"
  /\ E' = Preserve(ack) /\ op' = "fail.restore"
  /\ UNCHANGED <<R, L, lExists, dPhase, phase, ack, follow, lock, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

(* step 3: restore = &originalLedger, then release the ledger lock           *)
FailRestore ==
  /\ op = "fail.restore"
  /\ R' = "active" /\ lock' = "none" /\ op' = "fin.commit"
  /\ UNCHANGED <<E, L, lExists, dPhase, phase, ack, follow, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

(* serveHostAgentComplete -> acceptHostAgentCompletion: copyHostAgentResultFiles *)
(* writes the merged terminal events before any commit, without the lock     *)
CompleteCopy ==
  /\ agentAlive /\ phase = "confirmed" /\ op = "none" /\ attempts < MaxAttempts /\ UnlockedWriteOK
  /\ E' = Append(E, "T") /\ op' = "fin.commit"
  /\ attempts' = attempts + 1
  /\ UNCHANGED <<R, L, lExists, dPhase, phase, ack, follow, lock, rf, rr, stamp,
                 agentAlive, progress, crashes, streamed>>

(* finishHostAgentAssignment: Commit(finishing) -> apply -> Replace(terminal) *)
FinishCommit ==
  /\ op = "fin.commit" /\ UnlockedWriteOK
  /\ R' = "terminal" /\ dPhase' = "finishing" /\ phase' = "finishing" /\ op' = "fin.cleanup"
  /\ UNCHANGED <<E, L, lExists, ack, follow, lock, rf, rr, stamp, agentAlive, progress, attempts, crashes, streamed>>

(* session recheck or commit failed: resetHostAgentFinishing Replace(original) *)
FinishReset ==
  /\ op = "fin.commit" /\ UnlockedWriteOK
  /\ R' = "active" /\ op' = "none"
  /\ UNCHANGED <<E, L, lExists, dPhase, phase, ack, follow, lock, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

(* cleanupRunState removes the local Run State directory (L)                  *)
FinishCleanup ==
  /\ op = "fin.cleanup"
  /\ lExists' = FALSE /\ op' = "fin.completed"
  /\ UNCHANGED <<E, R, L, dPhase, phase, ack, follow, lock, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

(* Commit(completed); assignment leaves handler.assignments, so readers now   *)
(* read run.json from disk (readControlPlaneRunRecord)                        *)
FinishCompleted ==
  /\ op = "fin.completed" /\ UnlockedWriteOK
  /\ R' = "terminal" /\ dPhase' = "completed" /\ phase' = "completed" /\ op' = "none"
  /\ agentAlive' = FALSE
  /\ UNCHANGED <<E, L, lExists, ack, follow, lock, rf, rr, stamp, progress, attempts, crashes, streamed>>

-----------------------------------------------------------------------------
(* quarantineHostAgentAssignmentLocked (Agent fail with Quarantine, session   *)
(* lease expiry, process disappeared, restart quarantine marker):            *)
(* Snapshot, then ensureAcceptedFailureEvidence appends run.failed to L       *)
QStart ==
  /\ phase = "confirmed" /\ op = "none" /\ attempts < MaxAttempts /\ lock = "none"
  /\ ack' = E /\ L' = Append(L, "Fail") /\ op' = "q.sync"
  /\ attempts' = attempts + 1
  /\ UNCHANGED <<E, R, lExists, dPhase, phase, follow, lock, rf, rr, stamp,
                 agentAlive, progress, crashes, streamed>>

QSync ==
  /\ op = "q.sync" /\ lock = "none"
  /\ lock' = "op" /\ R' = "terminal" /\ E' = Sync(E, L, lExists) /\ op' = "q.preserve"
  /\ UNCHANGED <<L, lExists, dPhase, phase, ack, follow, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

QPreserve ==
  /\ op = "q.preserve"
  /\ E' = Preserve(ack) /\ lock' = "none" /\ op' = "q.commit"
  /\ UNCHANGED <<R, L, lExists, dPhase, phase, ack, follow, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

(* Commit(quarantined). Main has no Control Plane deadline sweep, so nothing  *)
(* appends to L or refreshes the ledger after a quarantine.                   *)
QCommit ==
  /\ op = "q.commit" /\ UnlockedWriteOK
  /\ R' = "terminal" /\ dPhase' = "quarantined" /\ phase' = "quarantined" /\ op' = "none"
  /\ agentAlive' = FALSE
  /\ UNCHANGED <<E, L, lExists, ack, follow, lock, rf, rr, stamp, progress, attempts, crashes, streamed>>

-----------------------------------------------------------------------------
(* serveControlPlaneEventStream with refreshTransient: decided once, when the *)
(* follow stream starts while the run is queued, and kept for the whole run.  *)
(* refreshControlPlaneEventSource runs Refresh when L's stamp changes.        *)
FollowRefreshRead ==
  /\ follow /\ rf = "idle" /\ lock = "none" /\ lExists /\ Len(L) # stamp
  /\ lock' = "refresh" /\ rr' = R /\ E' = Sync(E, L, lExists)
  /\ rf' = "held" /\ stamp' = Len(L)
  /\ UNCHANGED <<R, L, lExists, dPhase, phase, op, ack, follow, agentAlive, progress, attempts, crashes, streamed>>

FollowRefreshWrite ==
  /\ rf = "held"
  /\ R' = rr /\ lock' = "none" /\ rf' = "idle"
  /\ UNCHANGED <<E, L, lExists, dPhase, phase, op, ack, follow, rr, stamp,
                 agentAlive, progress, attempts, crashes, streamed>>

(* ReadEvents / Snapshot under the ledger lock; the stream emits new sequences *)
Read ==
  /\ lock = "none"
  /\ Len(E) > Len(streamed)
  /\ streamed' = streamed \o SubSeq(E, Len(streamed) + 1, Len(E))
  /\ UNCHANGED <<E, R, L, lExists, dPhase, phase, op, ack, follow, lock, rf, rr, stamp,
                 agentAlive, progress, attempts, crashes>>

(* Control Plane process crash + restart: in-flight handlers vanish, flock is *)
(* released; loadHostAgentState completes a finishing assignment.             *)
Crash ==
  /\ crashes < MaxCrashes
  /\ crashes' = crashes + 1
  \* connections die with the process; a reconnect to an assignment-backed
  \* run gets refreshTransient = FALSE (shouldRefreshControlPlaneEventStream)
  /\ lock' = "none" /\ op' = "none" /\ rf' = "idle" /\ follow' = FALSE /\ agentAlive' = FALSE
  /\ IF dPhase = "finishing"
       THEN R' = "terminal" /\ lExists' = FALSE /\ dPhase' = "completed" /\ phase' = "completed"
       ELSE UNCHANGED <<R, lExists, dPhase>> /\ phase' = dPhase
  /\ UNCHANGED <<E, L, ack, rr, stamp, progress, attempts, streamed>>

Next ==
  \/ Progress \/ FailStart \/ FailSync \/ FailPreserve \/ FailRestore
  \/ CompleteCopy \/ FinishCommit \/ FinishReset \/ FinishCleanup \/ FinishCompleted
  \/ QStart \/ QSync \/ QPreserve \/ QCommit
  \/ FollowRefreshRead \/ FollowRefreshWrite \/ Read \/ Crash

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* An event identity a client received keeps its payload (ADR 0048). Judged  *)
(* whenever the ledger lock is free, i.e. whenever a reader could look; a    *)
(* crash frees the lock, so a torn locked update counts too.                 *)
IdentityStable ==
  lock = "none" =>
    /\ Len(streamed) <= Len(E)
    /\ \A s \in 1..Len(streamed) : E[s] = streamed[s]

(* Once the assignment completed and left memory, run.json on disk is the only *)
(* record readers have; it must be terminal or the run never ends.           *)
CompletedTerminal == phase = "completed" => R = "terminal"

=============================================================================
