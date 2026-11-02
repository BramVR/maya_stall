------------------------- MODULE HostAgentHostLock -------------------------
(***************************************************************************)
(* Control Plane <-> Windows Host Agent Host Lock protocol for one Maya     *)
(* Host with one enrolled Windows Host Agent.                              *)
(*                                                                         *)
(* Every action names the Go function in internal/cli whose locked section  *)
(* it models. One action = one section that runs under handler.mu (and     *)
(* the assignment checkpointMu where the code takes it). Commit() of        *)
(* hostAgentTransitionStore is treated as one atomic durable write.        *)
(*                                                                         *)
(* Time is not modelled. The process-session lease and the restart        *)
(* takeover grace may elapse at any moment, which covers stalled agents,   *)
(* slow networks, and lost responses. The Control Plane enforces no Host   *)
(* Lock idle/hard deadline on this path: a lost lease is the only expiry.  *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Runs,        \* Run IDs submitted to the Control Plane queue
  Procs,       \* successive Windows Host Agent processes (run-once each)
  NoRun, NoProc,
  MaxRestarts  \* Control Plane process restarts to explore

AsgStates == {"none", "assigned", "confirmed", "finishing", "quarantined", "completed"}
LockStates == {"none", "assigned", "confirmed", "finishing", "quarantined"}
AgentStates == {"offline", "ready", "reserving", "locked", "running", "quarantined"}
PCs == {"init", "registered", "polled", "confirmed", "maya", "stopped",
        "unresolved", "awaiting", "dead"}

VARIABLES
  \* ---- durable Control Plane data dir: survives restart ----
  lockRun,      \* host-locks/<host>.json runId; NoRun = file absent
  lockState,    \* host-locks/<host>.json state
  dAsg,         \* assignments/<run>.json state ("none" = absent)
  dQueued,      \* queued-runs/<run>.json present
  marker,       \* host-agents/<agent> quarantine write-ahead marker run
  dStatusQuar,  \* host-agents/<agent>/status.json says quarantined for run
  sharedLock,   \* fake-host .maya-stall/state/locks/hosts/<host>.lock activeRun
  \* ---- Control Plane memory: lost on restart ----
  asg,          \* handler.assignments[run].record.State ("none" = not in map)
  finFlag,      \* assignment.finishing
  finBy,        \* process whose complete/fail request set finishing
  waiter,       \* runScenarioThroughHostAgent still waits on this run
  agentState,   \* agent.status.State
  agentRun,     \* agent.status.RunID
  sess,         \* agent.status.SessionID (owning process)
  leaseLive,    \* now < agent.sessionExpiresAt
  graceActive,  \* now < agent.takeoverNotBefore
  reserving,    \* run whose dispatch/acceptance is in flight
  \* ---- Windows Host Agent processes (runHostAgentOnce) ----
  pc, prun,
  \* ---- ghosts ----
  mayaLive,     \* process p started a Maya UI Session that is still alive
  assignCount,  \* times an assignment was created for a run
  restarts

durable == <<lockRun, lockState, dAsg, dQueued, marker, dStatusQuar, sharedLock>>
memory == <<asg, finFlag, finBy, waiter, agentState, agentRun, sess, leaseLive, graceActive, reserving>>
agentVars == <<pc, prun>>
ghosts == <<mayaLive, assignCount, restarts>>
vars == <<durable, memory, agentVars, ghosts>>

Init ==
  /\ lockRun = NoRun /\ lockState = "none"
  /\ dAsg = [r \in Runs |-> "none"]
  /\ dQueued = [r \in Runs |-> TRUE]
  /\ marker = NoRun /\ dStatusQuar = NoRun /\ sharedLock = NoRun
  /\ asg = [r \in Runs |-> "none"]
  /\ finFlag = [r \in Runs |-> FALSE] /\ finBy = [r \in Runs |-> NoProc]
  /\ waiter = [r \in Runs |-> FALSE]
  /\ agentState = "offline" /\ agentRun = NoRun /\ sess = NoProc
  /\ leaseLive = FALSE /\ graceActive = FALSE /\ reserving = NoRun
  /\ pc = [p \in Procs |-> "init"] /\ prun = [p \in Procs |-> NoRun]
  /\ mayaLive = [p \in Procs |-> FALSE]
  /\ assignCount = [r \in Runs |-> 0] /\ restarts = 0

SessionOK(p) == sess = p /\ leaseLive          \* acceptHostAgentSession
LeaseLost == sess # NoProc /\ ~leaseLive
RestartGraceOver == sess = NoProc /\ ~graceActive

(* quarantineHostAgentAssignmentLocked / quarantineHostAgentCleanupFailure:  *)
(* Commit(quarantined) rewrites the Host Lock for run r and keeps it.        *)
Quarantine(r) ==
  /\ asg' = [asg EXCEPT ![r] = "quarantined"]
  /\ dAsg' = [dAsg EXCEPT ![r] = "quarantined"]
  /\ lockRun' = r /\ lockState' = "quarantined"
  /\ agentState' = "quarantined" /\ agentRun' = r
  /\ sess' = NoProc /\ leaseLive' = FALSE
  /\ dStatusQuar' = r

QuarantineKeeps == <<dQueued, marker, sharedLock,
                     finBy, waiter, graceActive, reserving, ghosts>>

-----------------------------------------------------------------------------
(* Dispatch: dispatchQueuedRuns reserves, runScenarioThroughHostAgent accepts *)

AgentFree == agentState = "ready" /\ agentRun = NoRun /\ sess # NoProc /\ leaseLive

\* dispatchQueuedRuns: claim a ready Agent and acquire the shared fake-host lock
Reserve(r) ==
  /\ dQueued[r] /\ reserving = NoRun /\ AgentFree /\ sharedLock = NoRun
  /\ agentState' = "reserving" /\ reserving' = r /\ sharedLock' = r
  /\ UNCHANGED <<lockRun, lockState, dAsg, dQueued, marker, dStatusQuar,
                 asg, finFlag, finBy, waiter, agentRun, sess, leaseLive, graceActive,
                 agentVars, ghosts>>

ReservationHeld == agentState = "reserving" /\ agentRun = NoRun /\ sess # NoProc /\ leaseLive

\* runScenarioThroughHostAgent: eligibility check, then write quarantine marker
AcceptMarker(r) ==
  /\ reserving = r /\ lockRun # r /\ ReservationHeld
  /\ marker' = r   \* writePrivateJSON overwrites any earlier marker
  /\ UNCHANGED <<lockRun, lockState, dAsg, dQueued, dStatusQuar, sharedLock,
                 memory, agentVars, ghosts>>

\* runScenarioThroughHostAgent: eligibility recheck, Commit(assigned)
AcceptCommit(r) ==
  /\ reserving = r /\ marker = r /\ lockRun # r /\ ReservationHeld
  /\ lockRun' = r /\ lockState' = "assigned"
  /\ dAsg' = [dAsg EXCEPT ![r] = "assigned"]
  /\ assignCount' = [assignCount EXCEPT ![r] = @ + 1]
  /\ UNCHANGED <<dQueued, marker, dStatusQuar, sharedLock, memory, agentVars,
                 mayaLive, restarts>>

\* same locked section, after the commit (a crash can land in between):
\* removeQueuedRunLocked, remove marker, publish assignment in memory
AcceptFinish(r) ==
  /\ reserving = r /\ lockRun = r /\ asg[r] = "none" /\ dAsg[r] = "assigned"
  /\ dQueued' = [dQueued EXCEPT ![r] = FALSE]
  /\ marker' = NoRun
  /\ agentRun' = r /\ agentState' = "locked"
  /\ asg' = [asg EXCEPT ![r] = "assigned"]
  /\ waiter' = [waiter EXCEPT ![r] = TRUE]
  /\ reserving' = NoRun
  /\ UNCHANGED <<lockRun, lockState, dAsg, dStatusQuar, sharedLock,
                 finFlag, finBy, sess, leaseLive, graceActive, agentVars, ghosts>>

\* reservation changed or lease expired before commit: roll back and requeue
AcceptRollback(r) ==
  /\ reserving = r /\ lockRun # r /\ ~ReservationHeld
  /\ marker' = NoRun /\ sharedLock' = NoRun /\ reserving' = NoRun
  /\ IF agentState = "reserving"
       THEN IF sess # NoProc /\ leaseLive
              THEN agentState' = "ready" /\ sess' = sess
              ELSE agentState' = "offline" /\ sess' = NoProc
       ELSE UNCHANGED <<agentState, sess>>
  /\ UNCHANGED <<lockRun, lockState, dAsg, dQueued, dStatusQuar,
                 asg, finFlag, finBy, waiter, agentRun, leaseLive, graceActive,
                 agentVars, ghosts>>

-----------------------------------------------------------------------------
(* Windows Host Agent process (runHostAgentOnce) and its requests           *)

RegisterBlockedByConfirmed ==
  /\ agentRun # NoRun
  /\ asg[agentRun] = "confirmed" /\ ~finFlag[agentRun]
  /\ LeaseLost \/ RestartGraceOver

\* serveHostAgentRegistration: confirmed run whose process vanished -> quarantine
RegisterQuarantine(p) ==
  /\ pc[p] = "init" /\ agentState # "quarantined"
  /\ RegisterBlockedByConfirmed
  /\ Quarantine(agentRun)
  /\ pc' = [pc EXCEPT ![p] = "dead"]
  /\ UNCHANGED <<prun, finFlag>> /\ UNCHANGED QuarantineKeeps

\* serveHostAgentRegistration: new process session for this Agent
Register(p) ==
  /\ pc[p] = "init" /\ agentState # "quarantined"
  /\ agentRun # NoRun =>
       /\ asg[agentRun] \notin {"quarantined", "finishing"}
       /\ ~finFlag[agentRun]
       /\ ~RegisterBlockedByConfirmed
  /\ ~(agentRun # NoRun /\ sess = NoProc /\ graceActive)
  /\ ~(sess # NoProc /\ leaseLive)
  /\ sess' = p /\ leaseLive' = TRUE /\ graceActive' = FALSE
  /\ agentState' = IF agentRun = NoRun THEN "ready" ELSE "locked"
  /\ pc' = [pc EXCEPT ![p] = "registered"]
  /\ UNCHANGED <<durable, asg, finFlag, finBy, waiter, agentRun, reserving, prun, ghosts>>

\* serveHostAgentNextAssignment
Poll(p) ==
  /\ pc[p] = "registered" /\ SessionOK(p)
  /\ agentRun # NoRun /\ asg[agentRun] \in {"assigned", "confirmed"}
  /\ prun' = [prun EXCEPT ![p] = agentRun]
  /\ pc' = [pc EXCEPT ![p] = "polled"]
  /\ UNCHANGED <<durable, memory, ghosts>>

\* serveHostAgentConfirm; the agent runs only after a 200 reaches it
Confirm(p) ==
  LET r == prun[p] IN
  /\ pc[p] = "polled" /\ SessionOK(p)
  /\ asg[r] \in {"assigned", "confirmed"}
  /\ asg' = [asg EXCEPT ![r] = "confirmed"]
  /\ dAsg' = [dAsg EXCEPT ![r] = "confirmed"]
  /\ lockRun' = r /\ lockState' = "confirmed"
  /\ agentState' = "running"
  /\ \E delivered \in BOOLEAN :
        pc' = [pc EXCEPT ![p] = IF delivered THEN "confirmed" ELSE "polled"]
  /\ UNCHANGED <<dQueued, marker, dStatusQuar, sharedLock,
                 finFlag, finBy, waiter, agentRun, sess, leaseLive, graceActive, reserving,
                 prun, ghosts>>

\* runScenario starts the broker-owned Maya UI Session
StartMaya(p) ==
  /\ pc[p] = "confirmed"
  /\ mayaLive' = [mayaLive EXCEPT ![p] = TRUE]
  /\ pc' = [pc EXCEPT ![p] = "maya"]
  /\ UNCHANGED <<durable, memory, prun, assignCount, restarts>>

\* failure before any Maya UI Session existed
SkipMaya(p) ==
  /\ pc[p] = "confirmed"
  /\ pc' = [pc EXCEPT ![p] = "stopped"]
  /\ UNCHANGED <<durable, memory, prun, ghosts>>

\* stop-after always: broker verified shutdown (normal end or heartbeat cancel)
StopMaya(p) ==
  /\ pc[p] = "maya"
  /\ mayaLive' = [mayaLive EXCEPT ![p] = FALSE]
  /\ pc' = [pc EXCEPT ![p] = "stopped"]
  /\ UNCHANGED <<durable, memory, prun, assignCount, restarts>>

\* StopPolicy unresolved / cleanup failed: Maya may still be running
StopUnresolved(p) ==
  /\ pc[p] = "maya"
  /\ pc' = [pc EXCEPT ![p] = "unresolved"]
  /\ UNCHANGED <<durable, memory, prun, ghosts>>

\* serveHostAgentComplete / serveHostAgentFail (non-quarantine): claim finishing
PostTerminal(p) ==
  LET r == prun[p] IN
  /\ pc[p] = "stopped" /\ SessionOK(p)
  /\ asg[r] = "confirmed" /\ ~finFlag[r]
  /\ finFlag' = [finFlag EXCEPT ![r] = TRUE]
  /\ finBy' = [finBy EXCEPT ![r] = p]
  /\ pc' = [pc EXCEPT ![p] = "awaiting"]
  /\ UNCHANGED <<durable, asg, waiter, agentState, agentRun, sess, leaseLive,
                 graceActive, reserving, prun, ghosts>>

\* serveHostAgentFail with quarantine=true
PostQuarantine(p) ==
  LET r == prun[p] IN
  /\ pc[p] = "unresolved" /\ SessionOK(p)
  /\ asg[r] = "confirmed" /\ ~finFlag[r]
  /\ Quarantine(r)
  /\ pc' = [pc EXCEPT ![p] = "dead"]
  /\ UNCHANGED <<prun, finFlag>> /\ UNCHANGED QuarantineKeeps

\* process crash or a rejected request; a started Maya UI Session survives it
AgentExit(p) ==
  /\ pc[p] \notin {"init", "dead"}
  /\ pc' = [pc EXCEPT ![p] = "dead"]
  /\ UNCHANGED <<durable, memory, prun, ghosts>>

-----------------------------------------------------------------------------
(* Control Plane finishing of agent-reported terminal state                 *)

\* finishHostAgentAssignment: session recheck, Commit(finishing)
CommitFinishing(r) ==
  /\ finFlag[r] /\ asg[r] = "confirmed" /\ SessionOK(finBy[r])
  /\ asg' = [asg EXCEPT ![r] = "finishing"]
  /\ dAsg' = [dAsg EXCEPT ![r] = "finishing"]
  /\ lockRun' = r /\ lockState' = "finishing"
  /\ UNCHANGED <<dQueued, marker, dStatusQuar, sharedLock,
                 finFlag, finBy, waiter, agentState, agentRun, sess, leaseLive,
                 graceActive, reserving, agentVars, ghosts>>

\* acceptHostAgentCompletion or session recheck failed: resetHostAgentFinishing
AbortFinishing(r) ==
  /\ finFlag[r] /\ asg[r] = "confirmed"
  /\ finFlag' = [finFlag EXCEPT ![r] = FALSE]
  /\ UNCHANGED <<durable, asg, finBy, waiter, agentState, agentRun, sess, leaseLive,
                 graceActive, reserving, agentVars, ghosts>>

\* cleanupRunState failed: quarantineHostAgentCleanupFailure
CleanupFailQuarantine(r) ==
  /\ finFlag[r] /\ asg[r] = "finishing"
  /\ Quarantine(r)
  /\ finFlag' = [finFlag EXCEPT ![r] = FALSE]
  /\ UNCHANGED agentVars /\ UNCHANGED QuarantineKeeps

\* release shared lock, Commit(completed) removes the Host Lock, free the Agent
CommitCompleted(r) ==
  /\ finFlag[r] /\ asg[r] = "finishing"
  /\ lockRun' = NoRun /\ lockState' = "none"
  /\ dAsg' = [dAsg EXCEPT ![r] = "completed"]
  /\ sharedLock' = NoRun
  /\ agentRun' = NoRun /\ agentState' = "offline" /\ sess' = NoProc /\ leaseLive' = FALSE
  /\ asg' = [asg EXCEPT ![r] = "none"]
  /\ finFlag' = [finFlag EXCEPT ![r] = FALSE]
  /\ waiter' = [waiter EXCEPT ![r] = FALSE]
  /\ UNCHANGED <<dQueued, marker, dStatusQuar, finBy,
                 graceActive, reserving, agentVars, ghosts>>

-----------------------------------------------------------------------------
(* Time and Control Plane enforcement                                       *)

LeaseExpire ==
  /\ sess # NoProc /\ leaseLive
  /\ leaseLive' = FALSE
  /\ UNCHANGED <<durable, asg, finFlag, finBy, waiter, agentState, agentRun, sess,
                 graceActive, reserving, agentVars, ghosts>>

\* acceptHostAgentSession / dispatchQueuedRuns clearing an expired session
ClearExpiredSession ==
  /\ LeaseLost /\ agentState # "quarantined"
  /\ sess' = NoProc
  /\ agentState' = IF agentRun = NoRun THEN "offline" ELSE "locked"
  /\ UNCHANGED <<durable, asg, finFlag, finBy, waiter, agentRun, leaseLive,
                 graceActive, reserving, agentVars, ghosts>>

GraceExpire ==
  /\ graceActive
  /\ graceActive' = FALSE
  /\ UNCHANGED <<durable, asg, finFlag, finBy, waiter, agentState, agentRun, sess,
                 leaseLive, reserving, agentVars, ghosts>>

\* runScenarioThroughHostAgent wait loop: process-session lease lost
WaitLoopQuarantine(r) ==
  /\ waiter[r] /\ asg[r] = "confirmed" /\ ~finFlag[r]
  /\ LeaseLost \/ RestartGraceOver
  /\ Quarantine(r)
  /\ UNCHANGED <<agentVars, finFlag>> /\ UNCHANGED QuarantineKeeps

-----------------------------------------------------------------------------
(* Control Plane restart: newControlPlaneHandler -> loadHostAgentState,     *)
(* recoverHostAgentQuarantineMarkers, host-lock load, loadControlPlaneQueue *)

Restart ==
  /\ restarts < MaxRestarts
  /\ restarts' = restarts + 1
  \* recoverHostAgentQuarantineMarkers: Commit(quarantined) for the marker run
  /\ LET mLockRun   == IF marker # NoRun THEN marker ELSE lockRun
         mLockState == IF marker # NoRun THEN "quarantined" ELSE lockState
         mDAsg      == IF marker # NoRun THEN [dAsg EXCEPT ![marker] = "quarantined"] ELSE dAsg
         mQuar      == IF marker # NoRun THEN marker ELSE dStatusQuar
         loaded     == mLockRun # NoRun /\ mLockState # "finishing"
         finishing  == mLockRun # NoRun /\ mLockState = "finishing"
         quarantined == loaded /\ (mLockState = "quarantined" \/ mQuar # NoRun)
         newAsg     == [r \in Runs |-> IF loaded /\ r = mLockRun
                                         THEN IF quarantined THEN "quarantined" ELSE mLockState
                                         ELSE "none"]
     IN
     /\ marker' = NoRun
     /\ dStatusQuar' = mQuar
     \* finishing locks: loadHostAgentState runs cleanup, removes the shared
     \* lock, and Commit(completed) removes the Host Lock (else startup fails)
     /\ lockRun' = IF finishing THEN NoRun ELSE mLockRun
     /\ lockState' = IF finishing THEN "none" ELSE mLockState
     /\ dAsg' = IF finishing THEN [mDAsg EXCEPT ![mLockRun] = "completed"] ELSE mDAsg
     \* reestablishHostAgentSharedLock; a dead owner's shared lock is stale
     /\ sharedLock' = IF loaded THEN mLockRun ELSE NoRun
     /\ asg' = newAsg
     /\ agentRun' = IF loaded THEN mLockRun ELSE mQuar
     /\ agentState' = IF quarantined \/ (~loaded /\ mQuar # NoRun) THEN "quarantined" ELSE "offline"
     /\ graceActive' = loaded
     \* loadControlPlaneQueue drops intents that already have an assignment
     /\ dQueued' = [r \in Runs |-> dQueued[r] /\ newAsg[r] = "none"]
  /\ finFlag' = [r \in Runs |-> FALSE]
  /\ waiter' = [r \in Runs |-> FALSE]
  /\ sess' = NoProc /\ leaseLive' = FALSE /\ reserving' = NoRun
  /\ UNCHANGED <<finBy, agentVars, mayaLive, assignCount>>

-----------------------------------------------------------------------------

Next ==
  \/ \E r \in Runs :
       \/ Reserve(r) \/ AcceptMarker(r) \/ AcceptCommit(r) \/ AcceptFinish(r) \/ AcceptRollback(r)
       \/ CommitFinishing(r) \/ AbortFinishing(r) \/ CleanupFailQuarantine(r) \/ CommitCompleted(r)
       \/ WaitLoopQuarantine(r)
  \/ \E p \in Procs :
       \/ RegisterQuarantine(p) \/ Register(p) \/ Poll(p) \/ Confirm(p)
       \/ StartMaya(p) \/ SkipMaya(p) \/ StopMaya(p) \/ StopUnresolved(p)
       \/ PostTerminal(p) \/ PostQuarantine(p) \/ AgentExit(p)
  \/ LeaseExpire \/ ClearExpiredSession \/ GraceExpire
  \/ Restart

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* Properties                                                               *)

TypeOK ==
  /\ lockRun \in Runs \cup {NoRun} /\ lockState \in LockStates
  /\ dAsg \in [Runs -> AsgStates] /\ asg \in [Runs -> AsgStates]
  /\ agentState \in AgentStates /\ agentRun \in Runs \cup {NoRun}
  /\ sess \in Procs \cup {NoProc} /\ pc \in [Procs -> PCs]

\* A Maya UI Session that may still be running keeps its run's Host Lock,
\* both on disk and in the Agent slot the dispatcher reads.
MayaHeldByHostLock ==
  \A p \in Procs : mayaLive[p] => lockRun = prun[p] /\ agentRun = prun[p]

\* Never two live Maya UI Sessions from Maya Stall runs on the one Maya Host.
OneMayaPerHost ==
  \A p, q \in Procs : mayaLive[p] /\ mayaLive[q] => p = q

\* A Run ID is turned into a Host Agent assignment at most once.
AssignOnce ==
  \A r \in Runs : assignCount[r] <= 1

\* Durable state the next loadHostAgentState accepts without a startup error.
RestartLoadable ==
  /\ marker # NoRun => lockRun \in {NoRun, marker} /\ dStatusQuar \in {NoRun, marker}
  /\ lockRun # NoRun => dAsg[lockRun] = lockState /\ dStatusQuar \in {NoRun, lockRun}

=============================================================================
