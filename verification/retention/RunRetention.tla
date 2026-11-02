---------------------------- MODULE RunRetention ----------------------------
(***************************************************************************)
(* Embedded Mode run retention on one Maya Host: a Fresh Run controller     *)
(* (freshRunLifecycle in fresh_run.go), the Kept Session sweep             *)
(* (sweepKeptSessions) and explicit `stop` (stopRun), and the stop path     *)
(* (stopKeptRunAfterBrokerStop) that releases the Host Lock.                *)
(*                                                                         *)
(* Each run lives in its own repo (its own Run Record and repo mirror       *)
(* lock); runs share the authoritative host-side Host Lock and the single   *)
(* gg_mayasessiond broker on the Maya Host.                                 *)
(*                                                                         *)
(* No lock serializes the controller against the sweep or `stop`: Run       *)
(* Record writes are single atomic file writes and withRunLedgerLock only   *)
(* covers one Run Ledger record update. One action = one Run Record write,  *)
(* one host-side lock compare-and-swap, or one broker call. Time is an      *)
(* integer clock with ticks between any two steps.                          *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS
  Runs,          \* Fresh Runs (each from its own repo) competing for the Host
  NoRun,
  Lease,         \* hostSideLockLeaseDuration, in ticks
  MaxTime,
  FenceStart     \* TRUE = current code (hostLockFence, issue 170): every broker
                 \* call that starts, stops, or drives Maya runs in a Host step
                 \* that first proves the caller still owns the lock (an
                 \* unexpired lease for an active run, keptRun for a Kept
                 \* Session). FALSE = the unfenced broker before that fix

Phases == {"", "session-stopped", "broker-cleaned", "host-lock-released"}

VARIABLES
  now,
  \* ---- Maya Host ----
  lockRun,    \* host-side Host Lock owner (activeRun/keptRun); NoRun = absent
  lockKind,   \* "active" | "kept" | "none"
  lease,      \* active lease expiry (lock file LastWriteTimeUtc + lease duration)
  broker,     \* run whose Maya UI Session gg_mayasessiond currently runs
  \* ---- per repo durable state ----
  rec,        \* run-record.json status: "none" (state dir absent) | "running" | "kept"
  phase,      \* Run Record / Run Ledger StopPhase
  bound,      \* Run Record RemoteSession.SessionID is set
  mirror,     \* repo .maya-stall/state/locks/hosts/<host>.lock: "none"|"active"|"kept"
  \* ---- processes ----
  pc,         \* controller (`maya-stall run`) program counter
  hbErr,      \* sticky host-lock heartbeat error seen by the controller
  snap,       \* StopPhase the controller read before rewriting the Run Record
  sw,         \* stopper (Kept Session sweep or `maya-stall stop`) stage for this run
  \* ---- ghosts ----
  killed,     \* a broker call ended a session whose run still held the Host Lock
  refused     \* some fenced broker call was refused (reachability probe only)

vars == <<now, lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
          pc, hbErr, snap, sw, killed, refused>>

Init ==
  /\ now = 0
  /\ lockRun = NoRun /\ lockKind = "none" /\ lease = 0 /\ broker = NoRun
  /\ rec = [r \in Runs |-> "none"] /\ phase = [r \in Runs |-> ""]
  /\ bound = [r \in Runs |-> FALSE]
  /\ mirror = [r \in Runs |-> "none"]
  /\ pc = [r \in Runs |-> "idle"] /\ hbErr = [r \in Runs |-> FALSE]
  /\ snap = [r \in Runs |-> ""] /\ sw = [r \in Runs |-> "idle"]
  /\ killed = FALSE /\ refused = FALSE

Owns(r) == lockRun = r
\* The controller process is still running (its repo mirror pid is alive).
\* "unresolved" ends the process right after, so isStaleHostLock sees it dead.
ControllerLive(r) == pc[r] \notin {"idle", "dead", "done", "unresolved"}
\* sessiondStatusProvesInactive
BrokerInactive == broker = NoRun
\* hostSideLock.replaceOwner: exact-content compare-and-swap. Content changes
\* only by this run or by a reclaim, so it succeeds iff r still holds its
\* active lock. It does not check the lease.
CasActive(r) == Owns(r) /\ lockKind = "active"
\* remoteHostLockFenceScript / hostSideLock.claim: the lock still names r and,
\* for an active lock, the lease is unexpired. Kept locks have no lease.
FenceOK(r) == Owns(r) /\ (lockKind = "kept" \/ now < lease)
Renew == lease' = now + Lease

Tick ==
  /\ now < MaxTime /\ now' = now + 1
  /\ UNCHANGED <<lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
                 pc, hbErr, snap, sw, killed, refused>>

-----------------------------------------------------------------------------
(* Controller: freshRunLifecycle.setup / startFreshSession / execute /      *)
(* settle / Run's deferred cleanup                                          *)

\* selectHostForRunValidated -> acquireRunHostLock -> acquireRemoteHostLock:
\* create, or reclaim an expired active lease whose broker proves inactive
Acquire(r) ==
  /\ pc[r] = "idle"
  /\ \/ lockRun = NoRun
     \/ lockKind = "active" /\ now >= lease /\ BrokerInactive
  /\ lockRun' = r /\ lockKind' = "active" /\ Renew
  /\ mirror' = [mirror EXCEPT ![r] = "active"]
  /\ pc' = [pc EXCEPT ![r] = "acquired"]
  /\ UNCHANGED <<now, broker, rec, phase, bound, hbErr, snap, sw, killed, refused>>

\* writeRunRetentionRecord(newRunRetentionRecord("running")), host.markActive (CAS)
WriteRunningRecord(r) ==
  /\ pc[r] = "acquired"
  /\ rec' = [rec EXCEPT ![r] = "running"]
  /\ phase' = [phase EXCEPT ![r] = ""]
  /\ bound' = [bound EXCEPT ![r] = FALSE]
  /\ IF CasActive(r)
       THEN Renew /\ pc' = [pc EXCEPT ![r] = "recorded"]
       ELSE pc' = [pc EXCEPT ![r] = "abort"] /\ UNCHANGED lease
  /\ UNCHANGED <<now, lockRun, lockKind, broker, mirror, hbErr, snap, sw, killed, refused>>

\* background startHostLockHeartbeat tick -> hostSideLock.renew (CAS)
Heartbeat(r) ==
  /\ pc[r] \in {"acquired", "recorded", "renewed", "started", "binding", "bound", "ran", "failing"}
  /\ IF CasActive(r)
       THEN Renew /\ UNCHANGED hbErr
       ELSE hbErr' = [hbErr EXCEPT ![r] = TRUE] /\ UNCHANGED lease
  /\ UNCHANGED <<now, lockRun, lockKind, broker, rec, phase, bound, mirror,
                 pc, snap, sw, killed, refused>>

\* startFreshSession: renewHostLockNow (heartbeat error check + renew)
RenewBeforeStart(r) ==
  /\ pc[r] = "recorded"
  /\ IF CasActive(r) /\ ~hbErr[r]
       THEN Renew /\ pc' = [pc EXCEPT ![r] = "renewed"]
       ELSE pc' = [pc EXCEPT ![r] = "abort"] /\ UNCHANGED lease
  /\ UNCHANGED <<now, lockRun, lockKind, broker, rec, phase, bound, mirror,
                 hbErr, snap, sw, killed, refused>>

\* ggMayaSessiondBroker.StartFreshSession: stops any inherited session, then
\* restarts the broker task. Fenced: each step proves an unexpired lease for r
\* and renews it; a refused fence fails the run before Maya is touched.
StartSession(r) ==
  /\ pc[r] = "renewed"
  /\ IF FenceStart => FenceOK(r)
       THEN /\ broker' = r
            /\ killed' = (killed \/ (broker # NoRun /\ broker # r /\ Owns(broker)))
            /\ pc' = [pc EXCEPT ![r] = "started"]
            /\ IF FenceStart THEN Renew ELSE UNCHANGED lease
            /\ UNCHANGED refused
       ELSE /\ pc' = [pc EXCEPT ![r] = "abort"] /\ refused' = TRUE
            /\ UNCHANGED <<broker, killed, lease>>
  /\ UNCHANGED <<now, lockRun, lockKind, rec, phase, bound, mirror, hbErr, snap, sw>>

\* startFreshSession: readRunRetentionRecord
BindRead(r) ==
  /\ pc[r] = "started"
  /\ IF rec[r] = "none"
       THEN pc' = [pc EXCEPT ![r] = "failing"] /\ UNCHANGED snap
       ELSE pc' = [pc EXCEPT ![r] = "binding"] /\ snap' = [snap EXCEPT ![r] = phase[r]]
  /\ UNCHANGED <<now, lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
                 hbErr, sw, killed, refused>>

\* writeRunRetentionRecord(record with RemoteSession) writes the copy read
\* above, then host.bindSession (CAS) records the session id in the lock
BindWrite(r) ==
  /\ pc[r] = "binding"
  /\ rec' = [rec EXCEPT ![r] = "running"]
  /\ phase' = [phase EXCEPT ![r] = snap[r]]
  /\ bound' = [bound EXCEPT ![r] = TRUE]
  /\ IF CasActive(r)
       THEN Renew /\ pc' = [pc EXCEPT ![r] = "bound"]
       ELSE pc' = [pc EXCEPT ![r] = "failing"] /\ UNCHANGED lease
  /\ UNCHANGED <<now, lockRun, lockKind, broker, mirror, hbErr, snap, sw, killed, refused>>

\* setup's renewHostLockNow, stage payload, execute (renew, fenced RunScenario,
\* renew), heartbeat checks
RunScenario(r) ==
  /\ pc[r] = "bound"
  /\ IF CasActive(r) /\ ~hbErr[r]
       THEN Renew /\ pc' = [pc EXCEPT ![r] = "ran"]
       ELSE pc' = [pc EXCEPT ![r] = "failing"] /\ UNCHANGED lease
  /\ UNCHANGED <<now, lockRun, lockKind, broker, rec, phase, bound, mirror,
                 hbErr, snap, sw, killed, refused>>

\* ggMayaSessiondBroker.StopSession: stops only if the broker still reports
\* this run's session id; the stop itself is fenced. Any refusal leaves the
\* Host Lock held (releaseHostLock = false, cleanup "failed").
StopOwnSession(r) ==
  /\ pc[r] \in {"ran", "failing"}
  /\ IF broker = r /\ (FenceStart => FenceOK(r))
       THEN /\ broker' = NoRun /\ pc' = [pc EXCEPT ![r] = "stopped"]
            /\ killed' = (killed \/ (broker # r /\ Owns(broker)))
            /\ IF FenceStart /\ lockKind = "active" THEN Renew ELSE UNCHANGED lease
            /\ UNCHANGED refused
       ELSE /\ pc' = [pc EXCEPT ![r] = "unresolved"]
            /\ refused' = (refused \/ broker = r)
            /\ UNCHANGED <<broker, killed, lease>>
  /\ UNCHANGED <<now, lockRun, lockKind, rec, phase, bound, mirror, hbErr, snap, sw>>

\* preserveStoppedRunForCleanup: Run Record "kept" + StopPhase session-stopped,
\* then host.markKept (CAS). On CAS failure completeCleanupAfterRecoveryFailure
\* cannot release (content changed), so the run ends unresolved.
Preserve(r) ==
  /\ pc[r] = "stopped"
  /\ rec' = [rec EXCEPT ![r] = "kept"]
  /\ phase' = [phase EXCEPT ![r] = "session-stopped"]
  /\ IF CasActive(r)
       THEN /\ lockKind' = "kept" /\ mirror' = [mirror EXCEPT ![r] = "kept"]
            /\ pc' = [pc EXCEPT ![r] = "preserved"]
       ELSE /\ pc' = [pc EXCEPT ![r] = "unresolved"]
            /\ UNCHANGED <<lockKind, mirror>>
  /\ UNCHANGED <<now, lockRun, lease, broker, bound, hbErr, snap, sw, killed, refused>>

\* finishStoppedRunCleanup after preserve: broker-cleaned, host.release (CAS on
\* the exact kept content; an absent lock is fine), host-lock-released,
\* cleanupRunState, repo mirror removal
CleanupRelease(r) ==
  /\ pc[r] = "preserved"
  /\ IF Owns(r) \/ lockRun = NoRun
       THEN /\ lockRun' = NoRun /\ lockKind' = "none"
            /\ mirror' = [mirror EXCEPT ![r] = "none"]
            /\ rec' = [rec EXCEPT ![r] = "none"]
            /\ phase' = [phase EXCEPT ![r] = "host-lock-released"]
            /\ pc' = [pc EXCEPT ![r] = "done"]
       ELSE /\ phase' = [phase EXCEPT ![r] = "broker-cleaned"]
            /\ pc' = [pc EXCEPT ![r] = "unresolved"]
            /\ UNCHANGED <<lockRun, lockKind, mirror, rec>>
  /\ UNCHANGED <<now, lease, broker, bound, hbErr, snap, sw, killed, refused>>

\* Run's deferred cleanup when no session started: host.release removes the
\* host-side lock by CAS and the repo mirror unconditionally; Run State stays
ReleaseUnstarted(r) ==
  /\ pc[r] = "abort"
  /\ IF CasActive(r) THEN lockRun' = NoRun /\ lockKind' = "none"
                     ELSE UNCHANGED <<lockRun, lockKind>>
  /\ mirror' = [mirror EXCEPT ![r] = "none"]
  /\ pc' = [pc EXCEPT ![r] = "done"]
  /\ UNCHANGED <<now, lease, broker, rec, phase, bound, hbErr, snap, sw, killed, refused>>

\* retainSession: stop heartbeat (fail on its error), host.markKept (CAS),
\* RetainRun (broker must still report this run's session), kept Run Record
Retain(r) ==
  /\ pc[r] = "ran"
  /\ IF ~hbErr[r] /\ CasActive(r)
       THEN /\ lockKind' = "kept" /\ mirror' = [mirror EXCEPT ![r] = "kept"]
            /\ IF broker = r
                 THEN /\ rec' = [rec EXCEPT ![r] = "kept"]
                      /\ pc' = [pc EXCEPT ![r] = "done"]
                 ELSE /\ pc' = [pc EXCEPT ![r] = "unresolved"] /\ UNCHANGED rec
       ELSE /\ pc' = [pc EXCEPT ![r] = "failing"]
            /\ UNCHANGED <<lockKind, mirror, rec>>
  /\ UNCHANGED <<now, lockRun, lease, broker, phase, bound, hbErr, snap, sw, killed, refused>>

\* process death; Maya UI Session and locks stay behind
ControllerCrash(r) ==
  /\ ControllerLive(r)
  /\ pc' = [pc EXCEPT ![r] = "dead"]
  /\ UNCHANGED <<now, lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
                 hbErr, snap, sw, killed, refused>>

-----------------------------------------------------------------------------
(* Stoppers: sweepKeptSessions (from `run` before each Host Lock attempt     *)
(* and from `doctor`) and explicit `stop <run-id>`, both through            *)
(* stopKeptRunAfterBrokerStop. Entry checks run once; the later steps run   *)
(* without re-checking. One stopper per run (concurrent stoppers of the     *)
(* same run are not modelled).                                              *)

\* Kept run, StopPhase empty: listKeptRuns/readKeptRunState (sweep) or
\* ensureRunHasKeptLock + verifyKeptHostSideLockForRun (`stop`). The sweep also
\* needs KeepDeadline <= now; `stop` does not, and the model allows any time.
StopKept(r) ==
  /\ sw[r] = "idle"
  /\ rec[r] = "kept" /\ phase[r] = ""
  /\ mirror[r] = "kept" /\ Owns(r) /\ lockKind = "kept"
  /\ sw' = [sw EXCEPT ![r] = "stop"]
  /\ UNCHANGED <<now, lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
                 pc, hbErr, snap, killed, refused>>

\* Cleanup retry: StopPhase session-stopped or broker-cleaned.
\* ensureRunHasCleanupPendingLock accepts a mirror naming r or a missing mirror
\* (authoritative lock); no host-side check and no controller check.
StopRetry(r) ==
  /\ sw[r] = "idle"
  /\ rec[r] # "none" /\ phase[r] \in {"session-stopped", "broker-cleaned"}
  /\ sw' = [sw EXCEPT ![r] = "stop"]
  /\ UNCHANGED <<now, lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
                 pc, hbErr, snap, killed, refused>>

\* Crash-recovery `stop` of an active run: reconcileSubmittedRunLedgerForStop and
\* stopKeptRunAfterBrokerStop both call requireStaleSubmittedRunController
\* (repo mirror names r as activeRun, its pid is dead), then
\* verifyCleanupHostSideLockForRun (host lock names r, any lease state).
StopActiveDead(r) ==
  /\ sw[r] = "idle"
  /\ rec[r] = "running" /\ phase[r] = ""
  /\ mirror[r] = "active" /\ ~ControllerLive(r)
  /\ Owns(r)
  /\ sw' = [sw EXCEPT ![r] = "stop"]
  /\ UNCHANGED <<now, lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
                 pc, hbErr, snap, killed, refused>>

\* StopRetainedRun when StopPhase is "" or session-stopped: StatusRetainedRun
\* reports "kept" only for the recorded session id; that session is stopped
\* behind keptHostLockFence (host lock must name r as keptRun). Without a
\* recorded or live session it only cleans the workspace. Then checkpoint
\* broker-cleaned.
SweepBrokerStop(r) ==
  /\ sw[r] = "stop"
  /\ IF phase[r] \in {"", "session-stopped"} /\ bound[r] /\ broker = r
       THEN IF FenceStart => (Owns(r) /\ lockKind = "kept")
              THEN /\ broker' = NoRun
                   /\ phase' = [phase EXCEPT ![r] = "broker-cleaned"]
                   /\ sw' = [sw EXCEPT ![r] = "release"]
                   /\ UNCHANGED refused
              ELSE /\ sw' = [sw EXCEPT ![r] = "idle"] /\ refused' = TRUE
                   /\ UNCHANGED <<broker, phase>>
       ELSE /\ phase' = [phase EXCEPT ![r] = "broker-cleaned"]
            /\ sw' = [sw EXCEPT ![r] = "release"]
            /\ UNCHANGED <<broker, refused>>
  /\ UNCHANGED <<now, lockRun, lockKind, lease, rec, bound, mirror,
                 pc, hbErr, snap, killed>>

\* releaseHostSideLock -> removeRemoteHostLockForRun: removes the lock only
\* if it names r (activeRun or keptRun); ownership changed is not an error
SweepRelease(r) ==
  /\ sw[r] = "release"
  /\ IF Owns(r) THEN lockRun' = NoRun /\ lockKind' = "none" ELSE UNCHANGED <<lockRun, lockKind>>
  /\ sw' = [sw EXCEPT ![r] = "finish"]
  /\ UNCHANGED <<now, lease, broker, rec, phase, bound, mirror,
                 pc, hbErr, snap, killed, refused>>

\* finishReleasedStopRun: removeRepoHostLockForRun, host-lock-released, cleanupRunState
SweepFinish(r) ==
  /\ sw[r] = "finish"
  /\ mirror' = [mirror EXCEPT ![r] = "none"]
  /\ phase' = [phase EXCEPT ![r] = "host-lock-released"]
  /\ rec' = [rec EXCEPT ![r] = "none"]
  /\ sw' = [sw EXCEPT ![r] = "idle"]
  /\ UNCHANGED <<now, lockRun, lockKind, lease, broker, bound, pc, hbErr, snap, killed, refused>>

\* stopper process dies or a step fails (e.g. a failed ledger checkpoint)
SweeperCrash(r) ==
  /\ sw[r] # "idle"
  /\ sw' = [sw EXCEPT ![r] = "idle"]
  /\ UNCHANGED <<now, lockRun, lockKind, lease, broker, rec, phase, bound, mirror,
                 pc, hbErr, snap, killed, refused>>

-----------------------------------------------------------------------------

Next ==
  \/ Tick
  \/ \E r \in Runs :
       \/ Acquire(r) \/ WriteRunningRecord(r) \/ Heartbeat(r) \/ RenewBeforeStart(r)
       \/ StartSession(r) \/ BindRead(r) \/ BindWrite(r) \/ RunScenario(r)
       \/ StopOwnSession(r) \/ Preserve(r) \/ CleanupRelease(r) \/ ReleaseUnstarted(r)
       \/ Retain(r) \/ ControllerCrash(r)
       \/ StopKept(r) \/ StopRetry(r) \/ StopActiveDead(r)
       \/ SweepBrokerStop(r) \/ SweepRelease(r) \/ SweepFinish(r) \/ SweeperCrash(r)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* Properties                                                               *)

\* A live Maya UI Session whose run can still use it (a live controller, or a
\* Kept Session whose Run Record still says kept) belongs to the run that
\* holds the host-side Host Lock. A session left behind by a dead controller
\* is the Host's standing broker session: StartFreshSession stops it behind
\* the next owner's fence (CanaryNoStandingSession shows it is reachable).
MayaHeldByHostLock ==
  broker # NoRun /\ (ControllerLive(broker) \/ rec[broker] = "kept") => lockRun = broker

\* No broker call ends a session whose run still holds the Host Lock.
NoForeignKill == ~killed

=============================================================================
