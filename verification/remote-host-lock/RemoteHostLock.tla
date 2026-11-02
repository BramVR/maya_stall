--------------------------- MODULE RemoteHostLock ---------------------------
(***************************************************************************)
(* Embedded Mode host-side Host Lock on one Maya Host (one work root), as   *)
(* used by several controller checkouts / CI runners over SSH.             *)
(*                                                                         *)
(* Atomic steps:                                                           *)
(* - Each PowerShell script in internal/cli/host_lock_ssh.go runs inside   *)
(*   Invoke-WithLockMutex. The mutex file is opened FileShare::None, so    *)
(*   while its holder lives nobody can delete it (the 10-minute stale break *)
(*   and Clear-OwnedLiveLockMutex only remove an abandoned file). One      *)
(*   script = one atomic host step.                                        *)
(* - One gg_mayasessiond broker call (status, StartFreshSession, scenario  *)
(*   step, StopSession) = one atomic step.                                 *)
(* - A broker call that starts, stops, or drives Maya is fenced            *)
(*   (hostLockFence): the same Host script first proves, under the lock    *)
(*   mutex, that the caller's lockToken holds an unexpired lease and       *)
(*   renews it by touching LastWriteTimeUtc. Fence + call = one step: no   *)
(*   controller round trip separates them, and the fresh lease outlasts    *)
(*   the gap between two statements of one Host script.                   *)
(*                                                                         *)
(* Lock content is identified by (owner, incarnation, write). Any write     *)
(* changes content and LastWriteTimeUtc, so the client's exact-content and *)
(* leaseVersion compare-and-swap reduces to "no write since I looked".     *)
(*                                                                         *)
(* Time is abstract. Lease expiry (Locked-Result: age since              *)
(* LastWriteTimeUtc >= leaseDurationSeconds, Maya Host clock) can happen   *)
(* only while the owner is not heartbeating: crashed, paused, or gone.     *)
(* There is no hard lifetime. AllowPause decides whether a live controller *)
(* can stall longer than the lease (laptop sleep, suspended CI VM, GC/IO   *)
(* stall).                                                                 *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS Clients, None, MaxReclaim, AllowPause, AllowMayaCrash,
  RenewOutlivesOp  \* timing assumption: a successful renewHostLockNow keeps the
                   \* lease unexpired until the next broker call lands

Kinds == {"none", "acquired", "active", "kept"}

VARIABLES
  \* ---- Maya Host: <workRoot>/state/locks/host.lock ----
  lockOwner,   \* lockToken owner, None = file absent
  lockInc,     \* creation counter: distinguishes a client's own incarnations
  lockKind,    \* "acquired" (no run yet), "active" (activeRun), "kept" (keptRun)
  idleExp,     \* age since LastWriteTimeUtc >= leaseDurationSeconds
  writeSeq,    \* content+leaseVersion tag; a fresh tag on every create/replace/remove
  \* ---- Maya Host: gg_mayasessiond ----
  sess,        \* client whose Maya UI Session is live, None = broker inactive
  \* ---- controllers (freshRunLifecycle) ----
  pc, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending,
  \* ---- ghosts ----
  incCounter, foreignTouch, keptReclaimed, liveReclaimed

hostVars == <<lockOwner, lockInc, lockKind, idleExp, writeSeq, sess>>
clientVars == <<pc, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending>>
ghostVars == <<incCounter, foreignTouch, keptReclaimed, liveReclaimed>>
vars == <<hostVars, clientVars, ghostVars>>

PCs == {"start", "acqRecover", "lockedSeen", "reclaimReady", "owned", "active",
        "started", "binding", "steps", "stepRenewed", "stopped", "stopping", "kept", "done", "failed", "dead"}

\* renewed, broker call not yet made: the window RenewOutlivesOp talks about
InRenewWindow(c) == pc[c] \in {"started", "stepRenewed"}
Shielded(c) == RenewOutlivesOp /\ c # None /\ InRenewWindow(c)

Init ==
  /\ lockOwner = None /\ lockInc = 0 /\ lockKind = "none"
  /\ idleExp = FALSE /\ writeSeq = 0
  /\ sess = None
  /\ pc = [c \in Clients |-> "start"] /\ myInc = [c \in Clients |-> 0]
  /\ paused = [c \in Clients |-> FALSE] /\ renewErr = [c \in Clients |-> FALSE]
  /\ obsSeq = [c \in Clients |-> 0] /\ obsExp = [c \in Clients |-> FALSE] /\ attempts = [c \in Clients |-> 0]
  /\ pending = {}
  /\ incCounter = 0 /\ foreignTouch = FALSE /\ keptReclaimed = FALSE /\ liveReclaimed = FALSE

Mine(c) == lockOwner = c /\ lockInc = myInc[c]     \* current == expected content
Live(c) == pc[c] \notin {"dead"} /\ ~paused[c]
Acting(c) == Live(c) /\ pc[c] \notin {"done", "failed", "kept"}
\* the controller's renewal goroutine runs from acquire until release/keep
Heartbeating(c) == Live(c) /\ pc[c] \in {"owned", "active", "started", "binding", "steps", "stepRenewed", "stopped", "stopping"}

\* Locked-Result leaseExpired (Maya Host clock) as the client acts on it:
\* isStaleHostSideOwner and remoteHostLockReclaimAllowed never treat a
\* keptRun lock as stale, and the fence accepts a kept lock without a lease.
LeaseExpired ==
  CASE lockKind \in {"acquired", "active"} -> idleExp
    [] OTHER                              -> FALSE

\* A tag no observer or queued reclaim holds, so "tag unchanged" == "no write
\* since I looked" (exact content + LastWriteTimeUtc ticks) with finite states.
FreshSeq ==
  CHOOSE n \in 0..12 :
    n \notin ({obsSeq[d] : d \in Clients} \cup {m.seq : m \in pending} \cup {writeSeq})

\* New-LockFile: fresh LastWriteTimeUtc and incarnation
CreateLock(c) ==
  /\ lockOwner' = c /\ lockInc' = incCounter + 1 /\ incCounter' = incCounter + 1
  /\ lockKind' = "acquired" /\ idleExp' = FALSE
  /\ writeSeq' = FreshSeq

\* Replace-LockFile: new content and LastWriteTimeUtc, same lockToken
ReplaceLock(kind) ==
  /\ lockKind' = kind /\ idleExp' = FALSE /\ writeSeq' = FreshSeq
  /\ UNCHANGED <<lockOwner, lockInc>>

\* remoteHostLockFenceScript: lockToken still names c and (kept lock, or)
\* Locked-Result reports an unexpired lease
FenceOK(c) == Mine(c) /\ ~LeaseExpired

\* the fence's renewal: SetLastWriteTimeUtc, content unchanged
Touch == ReplaceLock(lockKind)

RemoveLock ==
  /\ lockOwner' = None /\ lockInc' = 0 /\ lockKind' = "none"
  /\ idleExp' = FALSE /\ writeSeq' = FreshSeq

SetPc(c, v) == pc' = [pc EXCEPT ![c] = v]

-----------------------------------------------------------------------------
(* acquireRemoteHostLock                                                    *)

\* remoteHostLockAcquireScript. outcome: "ok" response arrives; "lost" ran,
\* response timed out; "delayed" still waiting on the mutex, runs later.
AcquireSend(c) ==
  /\ Acting(c) /\ pc[c] = "start"
  /\ \E outcome \in {"ok", "lost", "delayed"} :
       IF outcome = "delayed"
       THEN /\ pending' = pending \cup {[who |-> c, op |-> "acquire", seq |-> 0]}
            /\ SetPc(c, "acqRecover")
            /\ UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, incCounter, myInc>>
       ELSE /\ UNCHANGED pending
            /\ IF lockOwner = None
                 THEN /\ CreateLock(c)
                      /\ myInc' = [myInc EXCEPT ![c] = incCounter + 1]
                      /\ SetPc(c, IF outcome = "ok" THEN "owned" ELSE "acqRecover")
                 ELSE /\ UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, incCounter, myInc>>
                      /\ IF outcome = "ok"
                           THEN SetPc(c, "lockedSeen") ELSE SetPc(c, "acqRecover")
  /\ obsSeq' = [obsSeq EXCEPT ![c] = writeSeq]
  /\ obsExp' = [obsExp EXCEPT ![c] = LeaseExpired]
  /\ UNCHANGED <<sess, paused, renewErr, attempts, foreignTouch, keptReclaimed, liveReclaimed>>

\* err != nil: readRemoteHostLockState, adopt the lock iff its content is
\* exactly ours. Only c ever writes c's content, and only after adopting it,
\* so "content == ours" is "lockOwner = c" here.
AcquireRecover(c) ==
  /\ Acting(c) /\ pc[c] = "acqRecover"
  /\ IF lockOwner = c
       THEN /\ myInc' = [myInc EXCEPT ![c] = lockInc] /\ SetPc(c, "owned")
       ELSE /\ UNCHANGED myInc /\ SetPc(c, "failed")
  /\ UNCHANGED <<hostVars, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* result.Locked: isStaleHostSideOwner + attempt bound, then broker status
\* (remoteHostLockSessionInactive -> sessiondStatusProvesInactive)
StaleCheck(c) ==
  /\ Acting(c) /\ pc[c] = "lockedSeen"
  /\ IF attempts[c] < MaxReclaim /\ obsExp[c] /\ sess = None
       THEN SetPc(c, "reclaimReady")
       ELSE SetPc(c, "failed")                      \* returns locked=true
  /\ UNCHANGED <<hostVars, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* remoteHostLockReclaimScript: delete only the exact observed content+version
ReclaimSend(c) ==
  /\ Acting(c) /\ pc[c] = "reclaimReady"
  /\ attempts' = [attempts EXCEPT ![c] = @ + 1]
  /\ \E outcome \in {"ok", "lost", "delayed"} :
       IF outcome = "delayed"
       THEN /\ pending' = pending \cup {[who |-> c, op |-> "reclaim", seq |-> obsSeq[c]]}
            /\ SetPc(c, "acqRecover")
            /\ UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, incCounter,
                           myInc, obsSeq, obsExp, keptReclaimed, liveReclaimed>>
       ELSE /\ UNCHANGED pending
            /\ IF lockOwner = None \/ writeSeq = obsSeq[c]
                 THEN /\ keptReclaimed' = (keptReclaimed \/ (lockOwner # None /\ lockKind = "kept"))
                      /\ liveReclaimed' = (liveReclaimed \/ (lockOwner # None /\ sess = lockOwner))
                      /\ CreateLock(c)
                      /\ myInc' = [myInc EXCEPT ![c] = incCounter + 1]
                      /\ UNCHANGED <<obsSeq, obsExp>>
                      /\ SetPc(c, IF outcome = "ok" THEN "owned" ELSE "acqRecover")
                 ELSE /\ UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq,
                                     incCounter, myInc, keptReclaimed, liveReclaimed>>
                      /\ obsSeq' = [obsSeq EXCEPT ![c] = writeSeq]
                      /\ obsExp' = [obsExp EXCEPT ![c] = LeaseExpired]
                      /\ SetPc(c, IF outcome = "ok" THEN "lockedSeen" ELSE "acqRecover")
  /\ UNCHANGED <<sess, paused, renewErr, foreignTouch>>

\* a timed-out script that was still queued on the mutex finally runs
RunPending ==
  \E m \in pending :
    /\ pending' = pending \ {m}
    /\ IF m.op = "acquire"
         THEN IF lockOwner = None
                THEN CreateLock(m.who) /\ UNCHANGED <<keptReclaimed, liveReclaimed>>
                ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, incCounter,
                                 keptReclaimed, liveReclaimed>>
         ELSE IF lockOwner = None \/ writeSeq = m.seq
                THEN /\ keptReclaimed' = (keptReclaimed \/ (lockOwner # None /\ lockKind = "kept"))
                     /\ liveReclaimed' = (liveReclaimed \/ (lockOwner # None /\ sess = lockOwner))
                     /\ CreateLock(m.who)
                ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, incCounter,
                                 keptReclaimed, liveReclaimed>>
    /\ UNCHANGED <<sess, pc, myInc, paused, renewErr, obsSeq, obsExp, attempts, foreignTouch>>

-----------------------------------------------------------------------------
(* freshRunLifecycle with a held hostSideLock                               *)

\* hostSideLock.markActive -> replaceRemoteHostLockOwner (exact-content CAS)
MarkActive(c) ==
  /\ Acting(c) /\ pc[c] = "owned"
  /\ IF Mine(c)
       THEN ReplaceLock("active") /\ SetPc(c, "active")
       ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq>> /\ SetPc(c, "failed")
  /\ UNCHANGED <<sess, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* startHostLockHeartbeat goroutine: hostSideLock.renew every minute
\* (exact-content CAS; the replace script does not check lease expiry)
Heartbeat(c) ==
  /\ Heartbeating(c)
  /\ IF Mine(c)
       THEN ReplaceLock(lockKind) /\ UNCHANGED renewErr
       ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq>>
            /\ renewErr' = [renewErr EXCEPT ![c] = TRUE]
  /\ UNCHANGED <<sess, pc, myInc, paused, obsSeq, obsExp, attempts, pending, ghostVars>>

\* startFreshSession: renewHostLockNow (heartbeat error check + CAS renew).
\* Then, as a separate step, StartFreshSession.
RenewBeforeStart(c) ==
  /\ Acting(c) /\ pc[c] = "active"
  /\ IF Mine(c) /\ ~renewErr[c]
       THEN ReplaceLock("active") /\ SetPc(c, "started")
       ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq>> /\ SetPc(c, "stopped")
  /\ UNCHANGED <<sess, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* ggMayaSessiondBroker.StartFreshSession: a fenced stop of any inherited
\* session (gg_mayasessiond stop), then a fenced start (scheduled task restart).
\* A refused fence fails the run without touching Maya.
StartSession(c) ==
  /\ Acting(c) /\ pc[c] = "started"
  /\ IF FenceOK(c)
       THEN /\ Touch
            /\ foreignTouch' = (foreignTouch \/ (sess # None /\ sess # c))
            /\ IF sess = None
                 THEN sess' = c /\ SetPc(c, "binding")
                 ELSE sess' = None /\ UNCHANGED pc
       ELSE /\ UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, sess, foreignTouch>>
            /\ SetPc(c, "stopped")
  /\ UNCHANGED <<myInc, paused, renewErr, obsSeq, obsExp, attempts, pending,
                 incCounter, keptReclaimed, liveReclaimed>>

\* startFreshSession: hostSideLock.bindSession records brokerSessionId
\* (exact-content CAS); a failure fails the run with the session live
BindSession(c) ==
  /\ Acting(c) /\ pc[c] = "binding"
  /\ IF Mine(c)
       THEN ReplaceLock("active") /\ SetPc(c, "steps")
       ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq>> /\ SetPc(c, "stopped")
  /\ UNCHANGED <<sess, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* execute/settle: renewHostLockNow before each host or broker operation
RenewBeforeStep(c) ==
  /\ Acting(c) /\ pc[c] = "steps"
  /\ IF Mine(c) /\ ~renewErr[c]
       THEN ReplaceLock("active") /\ SetPc(c, "stepRenewed")
       ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq>> /\ SetPc(c, "stopped")
  /\ UNCHANGED <<sess, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* Broker.RunScenario and the other gg_mayasessiond calls (fenced). Payload
\* staging and result collection touch only the run's own workspace.
ScenarioStep(c) ==
  /\ Acting(c) /\ pc[c] = "stepRenewed"
  /\ IF FenceOK(c)
       THEN Touch /\ foreignTouch' = (foreignTouch \/ (sess # None /\ sess # c))
       ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, foreignTouch>>
  /\ SetPc(c, "stopped")
  /\ UNCHANGED <<sess, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending,
                 incCounter, keptReclaimed, liveReclaimed>>

\* StopSession: status call; refuses unless the broker still reports our session id
StopSession(c) ==
  /\ Acting(c) /\ pc[c] = "stopped" /\ sess = c
  /\ SetPc(c, "stopping")
  /\ UNCHANGED <<hostVars, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* StopSession: the fenced gg_mayasessiond stop; a refused fence keeps the lock
StopFenced(c) ==
  /\ Acting(c) /\ pc[c] = "stopping"
  /\ IF FenceOK(c)
       THEN /\ Touch
            /\ foreignTouch' = (foreignTouch \/ (sess # None /\ sess # c))
            /\ sess' = None
            /\ SetPc(c, "stopped")
       ELSE /\ UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, sess, foreignTouch>>
            /\ SetPc(c, "failed")
  /\ UNCHANGED <<myInc, paused, renewErr, obsSeq, obsExp, attempts, pending,
                 incCounter, keptReclaimed, liveReclaimed>>

\* hostSideLock.release -> removeRemoteHostLock (exact-content compare-delete)
Release(c) ==
  /\ Acting(c) /\ pc[c] = "stopped" /\ sess # c
  /\ IF Mine(c) THEN RemoveLock ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq>>
  /\ SetPc(c, "done")
  /\ UNCHANGED <<sess, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* stop-after on-failure/never: hostSideLock.markKept, controller exits
MarkKept(c) ==
  /\ Acting(c) /\ pc[c] = "stopped" /\ sess = c
  /\ IF Mine(c) THEN ReplaceLock("kept") ELSE UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq>>
  /\ SetPc(c, IF Mine(c) THEN "kept" ELSE "failed")
  /\ UNCHANGED <<sess, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* maya-stall stop <run>: verifyKeptHostSideLockForRun, StopRetainedRun (the
\* gg_mayasessiond stop fenced on keptRun under the lock mutex), then
\* releaseHostSideLock -> removeRemoteHostLockForRun (matches keptRun/activeRun).
\* A kept lock is never reclaimed or rewritten by anyone else, so the separate
\* status read, fenced stop, and remove collapse into one step.
StopKept(c) ==
  /\ pc[c] = "kept" /\ lockOwner = c /\ lockKind = "kept"
  /\ sess' = IF sess = c THEN None ELSE sess
  /\ RemoveLock
  /\ SetPc(c, "done")
  /\ UNCHANGED <<myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

-----------------------------------------------------------------------------
(* Environment                                                              *)

Crash(c) ==
  /\ pc[c] \notin {"dead", "done", "failed", "kept"}
  /\ SetPc(c, "dead")
  /\ UNCHANGED <<hostVars, myInc, paused, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

Pause(c) ==
  /\ AllowPause /\ Acting(c) /\ ~Shielded(c)
  /\ paused' = [paused EXCEPT ![c] = TRUE]
  /\ UNCHANGED <<hostVars, pc, myInc, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

Resume(c) ==
  /\ paused[c]
  /\ paused' = [paused EXCEPT ![c] = FALSE]
  /\ UNCHANGED <<hostVars, pc, myInc, renewErr, obsSeq, obsExp, attempts, pending, ghostVars>>

\* no successful renewal for the lease duration: only without a live heartbeat.
\* A kept lock ages too (Locked-Result reports it expired after the default
\* lease); only the client-side kept checks in LeaseExpired keep it safe.
IdleExpire ==
  /\ lockOwner # None /\ ~idleExp
  /\ ~(Heartbeating(lockOwner) /\ Mine(lockOwner))
  /\ ~Shielded(lockOwner)
  /\ idleExp' = TRUE
  /\ UNCHANGED <<lockOwner, lockInc, lockKind, writeSeq, sess, clientVars, ghostVars>>

MayaCrash ==
  /\ AllowMayaCrash /\ sess # None
  /\ sess' = None
  /\ UNCHANGED <<lockOwner, lockInc, lockKind, idleExp, writeSeq, clientVars, ghostVars>>

Next ==
  \/ \E c \in Clients :
       \/ AcquireSend(c) \/ AcquireRecover(c) \/ StaleCheck(c) \/ ReclaimSend(c)
       \/ MarkActive(c) \/ Heartbeat(c) \/ RenewBeforeStart(c) \/ StartSession(c)
       \/ BindSession(c) \/ RenewBeforeStep(c) \/ ScenarioStep(c) \/ StopSession(c) \/ StopFenced(c) \/ Release(c) \/ MarkKept(c) \/ StopKept(c)
       \/ Crash(c) \/ Pause(c) \/ Resume(c)
  \/ RunPending \/ IdleExpire \/ MayaCrash

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* Properties                                                               *)

TypeOK ==
  /\ lockOwner \in Clients \cup {None} /\ lockKind \in Kinds
  /\ sess \in Clients \cup {None} /\ pc \in [Clients -> PCs]

\* A live Maya UI Session always belongs to the current Host Lock owner.
SessionUnderOwnersLock == sess # None => lockOwner = sess

\* No controller's broker call stops or drives another controller's Maya.
NoForeignSessionTouch == ~foreignTouch

\* A kept-session Host Lock is never reclaimed as stale.
KeptNeverReclaimed == ~keptReclaimed

\* Reclaim never deletes the lock of an owner whose Maya UI Session is live.
NoReclaimOverLiveSession == ~liveReclaimed

=============================================================================
