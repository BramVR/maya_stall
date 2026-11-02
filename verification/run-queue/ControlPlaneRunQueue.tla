------------------------- MODULE ControlPlaneRunQueue -------------------------
(***************************************************************************)
(* Control Plane durable Run queue across several Maya Hosts (ADR 0050).   *)
(*                                                                         *)
(* Each action names the Go function in internal/cli whose locked section  *)
(* or durable write it models: admission (queueHostAgentRun), dispatch     *)
(* (dispatchQueuedRuns), the acceptance half of runScenarioThroughHostAgent *)
(* up to Commit(assigned), operator cancel (cancelQueuedRun), and restart  *)
(* (loadControlPlaneQueue, resumeControlPlaneQueue).                       *)
(*                                                                         *)
(* Durable file writes that can fail are nondeterministic. Leases, Host    *)
(* capability changes, and Control Plane crashes may happen at any moment. *)
(* What happens after Commit(assigned) is verification/host-lock's job;    *)
(* here an assignment either stays active or completes and frees the Host. *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets, TLC

CONSTANTS Runs, Hosts, NoRun, NoHost, MaxQueued, MaxRestarts,
  RenameThenFail, \* allow the cancel intent write to fail after its rename landed
  ReleaseFails    \* allow sharedFakeHostRelease (fake-host lock removal) to fail

Terminal == {"completed", "failed", "canceled", "cleanup-failed"}  \* terminalRunLedgerRecord
AdmPhases == {"adm1", "adm2", "adm3", "adm4"}
AccPhases == {"acc1", "acc2", "commit", "rel1", "rel2", "requeue", "readm1", "readm2", "failb"}

VARIABLES
  \* ---- durable Control Plane data dir ----
  intent,      \* queued-runs/<run>.json state: none | admitting | queued | canceling
  ledger,      \* Run Ledger state: none|submitted|queued|canceling|<Terminal>
  ord,         \* acceptedAt order (FIFO key); 0 = not accepted
  asgD,        \* Host Agent assignment: none | active | quarantined | done
  asgHost,     \* Host of the assignment
  fakeLock,    \* fake-host shared lock file activeRun per Host (stale after restart)
  marker,      \* per-Host quarantine write-ahead marker run
  \* ---- Control Plane memory ----
  inMap,       \* handler.queuedRuns[run] != nil
  dispatching, canceled, cancelActive, released,  \* controlPlaneQueuedRun flags
  selected,    \* controlPlaneQueuedRun.selected Host
  contended,   \* lockContended
  hState,      \* agent.status.State per Host
  hRun,        \* agent.status.RunID per Host
  live,        \* agent session lease live per Host
  admLock,     \* queueAdmissionMu holder
  admissions,  \* handler.queueAdmissions
  disp,        \* in-flight dispatchQueuedRuns claim [h |-> host, r |-> run] or NoClaim
  nextOrd,
  \* ---- goroutines ----
  wpc,         \* submitter / resumed waiter running runScenarioThroughHostAgent
  accHost,     \* Host the waiter is accepting on
  xpc,         \* in-flight cancelQueuedRun request
  \* ---- ghosts ----
  everCanceled, assignCount, restarts,
  stuck        \* Hosts whose shared-lock release failed (fail-closed until restart)

NoClaim == [h |-> NoHost, r |-> NoRun]

durable == <<intent, ledger, ord, asgD, asgHost, fakeLock, marker>>
memRun == <<inMap, dispatching, canceled, cancelActive, released, selected, contended>>
memHost == <<hState, hRun, live>>
memGlobal == <<admLock, admissions, disp, nextOrd>>
procs == <<wpc, accHost, xpc>>
ghosts == <<everCanceled, assignCount, restarts, stuck>>
vars == <<durable, memRun, memHost, memGlobal, procs, ghosts>>

Init ==
  /\ intent = [r \in Runs |-> "none"] /\ ledger = [r \in Runs |-> "none"]
  /\ ord = [r \in Runs |-> 0]
  /\ asgD = [r \in Runs |-> "none"] /\ asgHost = [r \in Runs |-> NoHost]
  /\ fakeLock = [h \in Hosts |-> NoRun] /\ marker = [h \in Hosts |-> NoRun]
  /\ inMap = [r \in Runs |-> FALSE] /\ dispatching = [r \in Runs |-> FALSE]
  /\ canceled = [r \in Runs |-> FALSE] /\ cancelActive = [r \in Runs |-> FALSE]
  /\ released = [r \in Runs |-> FALSE] /\ selected = [r \in Runs |-> NoHost]
  /\ contended = [r \in Runs |-> FALSE]
  /\ hState = [h \in Hosts |-> "offline"] /\ hRun = [h \in Hosts |-> NoRun]
  /\ live = [h \in Hosts |-> FALSE]
  /\ admLock = NoRun /\ admissions = 0 /\ disp = NoClaim /\ nextOrd = 1
  /\ wpc = [r \in Runs |-> "idle"] /\ accHost = [r \in Runs |-> NoHost]
  /\ xpc = [r \in Runs |-> "idle"]
  /\ everCanceled = [r \in Runs |-> FALSE] /\ assignCount = [r \in Runs |-> 0]
  /\ restarts = 0 /\ stuck = {}

Waiting == Cardinality({r \in Runs : inMap[r] /\ ~canceled[r]})  \* waitingQueueCountLocked

\* resetQueuedDispatchLocked
ResetRun(r) ==
  /\ dispatching' = [dispatching EXCEPT ![r] = FALSE]
  /\ selected' = [selected EXCEPT ![r] = NoHost]
  /\ contended' = [contended EXCEPT ![r] = FALSE]

SetW(r, v) == wpc' = [wpc EXCEPT ![r] = v]

-----------------------------------------------------------------------------
(* Admission: queueHostAgentRun else-branch, under queueAdmissionMu          *)

\* bound check + queueAdmissionLocked (a compatible live Host must exist)
Submit(r) ==
  /\ wpc[r] = "idle" /\ admLock = NoRun
  /\ IF Waiting + admissions >= MaxQueued
       THEN SetW(r, "done") /\ UNCHANGED <<admLock, admissions>>
       ELSE \/ SetW(r, "done") /\ UNCHANGED <<admLock, admissions>>   \* no compatible Host
            \/ admLock' = r /\ admissions' = admissions + 1 /\ SetW(r, "adm1")
  /\ UNCHANGED <<durable, memRun, memHost, disp, nextOrd, accHost, xpc, ghosts>>

\* writePrivateJSON(admitting)
AdmWriteIntent(r) ==
  /\ wpc[r] = "adm1"
  /\ intent' = [intent EXCEPT ![r] = "admitting"] /\ SetW(r, "adm2")
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, marker, memRun, memHost, memGlobal, accHost, xpc, ghosts>>

\* run.accept: Run Ledger submitted, acceptedAt fixes FIFO order
AdmAccept(r) ==
  /\ wpc[r] = "adm2"
  /\ ledger' = [ledger EXCEPT ![r] = "submitted"]
  /\ ord' = [ord EXCEPT ![r] = nextOrd] /\ nextOrd' = nextOrd + 1
  /\ SetW(r, "adm3")
  /\ UNCHANGED <<intent, asgD, asgHost, fakeLock, marker, memRun, memHost, admLock, admissions, disp, accHost, xpc, ghosts>>

\* run.accept failed: cleanupUnacceptedOwnership + removeQueueIntent
AdmAcceptFail(r) ==
  /\ wpc[r] = "adm2"
  /\ intent' = [intent EXCEPT ![r] = "none"]
  /\ admLock' = NoRun /\ admissions' = admissions - 1 /\ SetW(r, "done")
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, marker, memRun, memHost, disp, nextOrd, accHost, xpc, ghosts>>

\* run.queued event, markControlPlaneRunQueued, ensureDurableQueuedRunState
AdmMarkQueued(r) ==
  /\ wpc[r] = "adm3"
  /\ ledger' = [ledger EXCEPT ![r] = "queued"] /\ SetW(r, "adm4")
  /\ UNCHANGED <<intent, ord, asgD, asgHost, fakeLock, marker, memRun, memHost, memGlobal, accHost, xpc, ghosts>>

\* writePrivateJSON(queued), then publish into handler.queuedRuns
AdmPublish(r) ==
  /\ wpc[r] = "adm4"
  /\ intent' = [intent EXCEPT ![r] = "queued"]
  /\ inMap' = [inMap EXCEPT ![r] = TRUE]
  /\ admLock' = NoRun /\ admissions' = admissions - 1 /\ SetW(r, "wait")
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, marker, dispatching, canceled, cancelActive,
                 released, selected, contended, memHost, disp, nextOrd, accHost, xpc, ghosts>>

\* a write after acceptance failed: finishEarlyFailure, Finalize, removeQueuedRunLocked
AdmFail(r) ==
  /\ wpc[r] \in {"adm3", "adm4"}
  /\ ledger' = [ledger EXCEPT ![r] = "failed"]
  /\ \/ intent' = [intent EXCEPT ![r] = "none"]
     \/ UNCHANGED intent                          \* removeQueueIntent failed
  /\ admLock' = NoRun /\ admissions' = admissions - 1 /\ SetW(r, "done")
  /\ UNCHANGED <<ord, asgD, asgHost, fakeLock, marker, memRun, memHost, disp, nextOrd, accHost, xpc, ghosts>>

-----------------------------------------------------------------------------
(* Waiter loop in queueHostAgentRun                                         *)

WaitSelected(r) ==
  /\ wpc[r] = "wait" /\ inMap[r] /\ selected[r] # NoHost
  /\ accHost' = [accHost EXCEPT ![r] = selected[r]] /\ SetW(r, "acc1")
  /\ UNCHANGED <<durable, memRun, memHost, memGlobal, xpc, ghosts>>

WaitCanceled(r) ==
  /\ wpc[r] = "wait" /\ selected[r] = NoHost /\ released[r]
  /\ SetW(r, "done")
  /\ UNCHANGED <<durable, memRun, memHost, memGlobal, accHost, xpc, ghosts>>

-----------------------------------------------------------------------------
(* Acceptance in runScenarioThroughHostAgent, before and at Commit(assigned) *)

ReservationHeld(h) == hState[h] = "reserving" /\ hRun[h] = NoRun /\ live[h]

\* first eligibility check; on success write the quarantine marker
AccFirstCheck(r) ==
  LET h == accHost[r] IN
  /\ wpc[r] = "acc1"
  /\ IF ReservationHeld(h)
       THEN /\ marker' = [marker EXCEPT ![h] = r] /\ SetW(r, "acc2")
            /\ UNCHANGED <<dispatching, selected, contended, hState>>
       ELSE /\ IF inMap[r] THEN ResetRun(r) ELSE UNCHANGED <<dispatching, selected, contended>>
            /\ hState' = IF hState[h] = "reserving" /\ hRun[h] = NoRun /\ ~live[h]
                           THEN [hState EXCEPT ![h] = "offline"] ELSE hState
            /\ SetW(r, "rel1") /\ UNCHANGED marker
  /\ UNCHANGED <<intent, ledger, ord, asgD, asgHost, fakeLock, inMap, canceled, cancelActive, released,
                 hRun, live, memGlobal, accHost, xpc, ghosts>>

\* sharedFakeHostRelease after the reset; failure goes to failBeforeTransition
AccRelease1(r) ==
  LET h == accHost[r] IN
  /\ wpc[r] = "rel1"
  /\ \/ /\ fakeLock' = IF fakeLock[h] = r THEN [fakeLock EXCEPT ![h] = NoRun] ELSE fakeLock
        /\ SetW(r, "requeue")
     \/ ReleaseFails /\ SetW(r, "failb") /\ UNCHANGED fakeLock /\ stuck' = stuck \cup {h}
  /\ \/ wpc'[r] = "requeue" /\ UNCHANGED stuck
     \/ wpc'[r] = "failb"
  /\ UNCHANGED <<intent, ledger, ord, asgD, asgHost, marker, memRun, memHost, memGlobal, accHost, xpc,
                 everCanceled, assignCount, restarts>>

\* recursive runScenarioThroughHostAgent -> queueHostAgentRun lookup
AccRequeue(r) ==
  /\ wpc[r] = "requeue"
  /\ IF inMap[r]
       THEN SetW(r, "wait") /\ UNCHANGED <<admLock, admissions>>
       ELSE \* run already left the queue: a fresh admission with the same Run ID
            IF admLock = NoRun /\ Waiting + admissions < MaxQueued
              THEN \/ SetW(r, "done") /\ UNCHANGED <<admLock, admissions>>
                   \/ admLock' = r /\ admissions' = admissions + 1 /\ SetW(r, "readm1")
              ELSE SetW(r, "done") /\ UNCHANGED <<admLock, admissions>>
  /\ UNCHANGED <<durable, memRun, memHost, disp, nextOrd, accHost, xpc, ghosts>>

\* writePrivateJSON(admitting) overwrites whatever intent file exists
ReAdmWrite(r) ==
  /\ wpc[r] = "readm1"
  /\ intent' = [intent EXCEPT ![r] = "admitting"] /\ SetW(r, "readm2")
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, marker, memRun, memHost, memGlobal, accHost, xpc, ghosts>>

\* run.accept fails (Run ID exists): removeQueueIntent; failHostAgentSelection also fails to accept
ReAdmFail(r) ==
  /\ wpc[r] = "readm2"
  /\ intent' = [intent EXCEPT ![r] = "none"]
  /\ admLock' = NoRun /\ admissions' = admissions - 1 /\ SetW(r, "done")
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, marker, memRun, memHost, disp, nextOrd, accHost, xpc, ghosts>>

\* eligibility recheck, refreshQueuedReservationCapabilitiesLocked, Commit(assigned).
\* The assigned Run Ledger keeps state "submitted" (assignedLedger.State).
AccCommit(r) ==
  LET h == accHost[r] IN
  /\ wpc[r] = "acc2" /\ ReservationHeld(h)
  /\ asgD' = [asgD EXCEPT ![r] = "active"] /\ asgHost' = [asgHost EXCEPT ![r] = h]
  /\ ledger' = [ledger EXCEPT ![r] = "submitted"]
  /\ assignCount' = [assignCount EXCEPT ![r] = @ + 1]
  /\ SetW(r, "commit")
  /\ UNCHANGED <<intent, ord, fakeLock, marker, memRun, memHost, memGlobal, accHost, xpc,
                 everCanceled, restarts, stuck>>

\* same handler.mu section, after Commit (only a crash can land in between):
\* removeQueuedRunLocked, remove marker, Agent locked to the Run
AccPublish(r) ==
  LET h == accHost[r] IN
  /\ wpc[r] = "commit"
  /\ intent' = [intent EXCEPT ![r] = "none"] /\ inMap' = [inMap EXCEPT ![r] = FALSE]
  /\ marker' = [marker EXCEPT ![h] = NoRun]
  /\ hState' = [hState EXCEPT ![h] = "locked"] /\ hRun' = [hRun EXCEPT ![h] = r]
  /\ SetW(r, "done")
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, dispatching, canceled, cancelActive, released,
                 selected, contended, live, memGlobal, accHost, xpc, ghosts>>

\* recheck failed (or capabilities changed): remove marker, release shared lock
AccEligFail(r) ==
  LET h == accHost[r] IN
  /\ wpc[r] = "acc2"
  /\ marker' = [marker EXCEPT ![h] = NoRun]
  /\ \/ /\ fakeLock' = IF fakeLock[h] = r THEN [fakeLock EXCEPT ![h] = NoRun] ELSE fakeLock
        /\ SetW(r, "rel2")
     \/ ReleaseFails /\ SetW(r, "failb") /\ UNCHANGED fakeLock /\ stuck' = stuck \cup {h}
  /\ \/ wpc'[r] = "rel2" /\ UNCHANGED stuck
     \/ wpc'[r] = "failb"
  /\ UNCHANGED <<intent, ledger, ord, asgD, asgHost, memRun, memHost, memGlobal, accHost, xpc,
                 everCanceled, assignCount, restarts>>

\* rollback status check: Agent back to ready/offline and requeue, else fail
AccRollbackStatus(r) ==
  LET h == accHost[r] IN
  /\ wpc[r] = "rel2"
  /\ IF hState[h] = "reserving" /\ hRun[h] = NoRun
       THEN /\ hState' = [hState EXCEPT ![h] = IF live[h] THEN "ready" ELSE "offline"]
            /\ IF inMap[r] THEN ResetRun(r) ELSE UNCHANGED <<dispatching, selected, contended>>
            /\ SetW(r, "requeue")
       ELSE SetW(r, "failb") /\ UNCHANGED <<hState, dispatching, selected, contended>>
  /\ UNCHANGED <<durable, inMap, canceled, cancelActive, released, hRun, live, memGlobal, accHost, xpc, ghosts>>

\* failBeforeTransition: finishAcceptedHostAgentFailure (Finalize overwrites the
\* ledger), then removeQueuedRunLocked once the ledger is terminal
AccFailBefore(r) ==
  LET h == accHost[r] IN
  /\ wpc[r] = "failb"
  /\ hState' = IF hState[h] = "reserving" THEN [hState EXCEPT ![h] = "ready"] ELSE hState
  /\ ledger' = [ledger EXCEPT ![r] = "failed"]
  /\ \/ /\ intent' = [intent EXCEPT ![r] = "none"] /\ inMap' = [inMap EXCEPT ![r] = FALSE]
        /\ UNCHANGED canceled
     \/ \* removeQueueIntent failed: removeQueuedRunLocked keeps the entry inert
        /\ canceled' = IF inMap[r] THEN [canceled EXCEPT ![r] = TRUE] ELSE canceled
        /\ UNCHANGED <<intent, inMap>>
  /\ SetW(r, "done")
  /\ UNCHANGED <<ord, asgD, asgHost, fakeLock, marker, dispatching, cancelActive, released,
                 selected, contended, hRun, live, memGlobal, accHost, xpc, ghosts>>

-----------------------------------------------------------------------------
(* dispatchQueuedRuns (serialized by queueDispatchMu)                        *)

Eligible(r) == inMap[r] /\ ~canceled[r] /\ ~dispatching[r]

\* claim: each free Host takes the first compatible Run in FIFO order
DispClaim(h) ==
  /\ disp = NoClaim
  /\ hState[h] = "ready" /\ hRun[h] = NoRun /\ live[h]
  /\ \E r \in Runs :
       /\ Eligible(r)
       /\ \A q \in Runs : Eligible(q) => ord[r] <= ord[q]
       /\ dispatching' = [dispatching EXCEPT ![r] = TRUE]
       /\ hState' = [hState EXCEPT ![h] = "reserving"]
       /\ disp' = [h |-> h, r |-> r]
  /\ UNCHANGED <<durable, inMap, canceled, cancelActive, released, selected, contended, hRun, live,
                 admLock, admissions, nextOrd, procs, ghosts>>

\* queueAcquireHostLock, then claim recheck under handler.mu
DispAcquire ==
  LET h == disp.h
      r == disp.r
      candCur == inMap[r] /\ dispatching[r] /\ ~canceled[r] /\ selected[r] = NoHost
      hostCur == hState[h] = "reserving" /\ hRun[h] = NoRun
      resetHost == IF hostCur THEN [hState EXCEPT ![h] = IF live[h] THEN "ready" ELSE "offline"]
                   ELSE hState
  IN
  /\ disp # NoClaim
  /\ disp' = NoClaim
  /\ IF fakeLock[h] # NoRun
       THEN \* locked: lockContended
            /\ hState' = resetHost
            /\ dispatching' = IF candCur THEN [dispatching EXCEPT ![r] = FALSE] ELSE dispatching
            /\ contended' = IF candCur THEN [contended EXCEPT ![r] = TRUE] ELSE contended
            /\ UNCHANGED <<fakeLock, selected>>
       ELSE \E compatible \in BOOLEAN :
              IF candCur /\ hostCur /\ live[h] /\ compatible
                THEN /\ fakeLock' = [fakeLock EXCEPT ![h] = r]
                     /\ selected' = [selected EXCEPT ![r] = h]
                     /\ contended' = [contended EXCEPT ![r] = FALSE]
                     /\ UNCHANGED <<hState, dispatching>>
                ELSE /\ hState' = resetHost
                     /\ dispatching' = IF candCur THEN [dispatching EXCEPT ![r] = FALSE] ELSE dispatching
                     /\ UNCHANGED <<fakeLock, selected, contended>>
  /\ UNCHANGED <<intent, ledger, ord, asgD, asgHost, marker, inMap, canceled, cancelActive, released,
                 hRun, live, admLock, admissions, nextOrd, procs, ghosts>>

-----------------------------------------------------------------------------
(* cancelQueuedRun                                                          *)

CancelStart(r) ==
  /\ xpc[r] = "idle" /\ inMap[r] /\ ~dispatching[r] /\ ~cancelActive[r]
  /\ IF canceled[r]
       THEN cancelActive' = [cancelActive EXCEPT ![r] = TRUE] /\ xpc' = [xpc EXCEPT ![r] = "prepare"]
            /\ UNCHANGED <<canceled, intent>>
       ELSE \* writePrivateJSON(canceling): ok, failed before rename, failed after rename
            \/ /\ canceled' = [canceled EXCEPT ![r] = TRUE]
               /\ intent' = [intent EXCEPT ![r] = "canceling"]
               /\ cancelActive' = [cancelActive EXCEPT ![r] = TRUE]
               /\ xpc' = [xpc EXCEPT ![r] = "prepare"]
            \/ UNCHANGED <<canceled, intent, cancelActive, xpc>>
            \/ /\ RenameThenFail
               /\ intent' = [intent EXCEPT ![r] = "canceling"]
               /\ UNCHANGED <<canceled, cancelActive, xpc>>
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, marker, inMap, dispatching, released, selected,
                 contended, memHost, memGlobal, wpc, accHost, ghosts>>

\* completeQueuedRunCancellation -> prepareQueuedRunCancellation (ledger canceling)
CancelPrepare(r) ==
  /\ xpc[r] = "prepare"
  /\ IF ledger[r] \in {"canceling", "canceled", "cleanup-failed"}
       THEN xpc' = [xpc EXCEPT ![r] = "cleanup"] /\ UNCHANGED <<ledger, everCanceled>>
       ELSE \/ /\ ledger' = [ledger EXCEPT ![r] = "canceling"]
               /\ everCanceled' = [everCanceled EXCEPT ![r] = TRUE]
               /\ xpc' = [xpc EXCEPT ![r] = "cleanup"]
            \/ xpc' = [xpc EXCEPT ![r] = "error"] /\ UNCHANGED <<ledger, everCanceled>>
  /\ UNCHANGED <<intent, ord, asgD, asgHost, fakeLock, marker, memRun, memHost, memGlobal, wpc, accHost,
                 assignCount, restarts, stuck>>

\* cleanupQueuedRunCancellation
CancelCleanup(r) ==
  /\ xpc[r] = "cleanup"
  /\ \/ ledger' = [ledger EXCEPT ![r] = "canceled"] /\ xpc' = [xpc EXCEPT ![r] = "remove"]
     \/ ledger' = [ledger EXCEPT ![r] = "cleanup-failed"] /\ xpc' = [xpc EXCEPT ![r] = "error"]
  /\ UNCHANGED <<intent, ord, asgD, asgHost, fakeLock, marker, memRun, memHost, memGlobal, wpc, accHost, ghosts>>

CancelError(r) ==
  /\ xpc[r] = "error"
  /\ cancelActive' = [cancelActive EXCEPT ![r] = FALSE]
  /\ released' = IF ledger[r] \in {"canceled", "cleanup-failed"} THEN [released EXCEPT ![r] = TRUE] ELSE released
  /\ xpc' = [xpc EXCEPT ![r] = "idle"]
  /\ UNCHANGED <<durable, inMap, dispatching, canceled, selected, contended, memHost, memGlobal, wpc, accHost, ghosts>>

\* removeQueueIntent, delete from handler.queuedRuns, releaseQueuedWaiterLocked
CancelRemove(r) ==
  /\ xpc[r] = "remove"
  /\ \/ /\ intent' = [intent EXCEPT ![r] = "none"] /\ inMap' = [inMap EXCEPT ![r] = FALSE]
        /\ UNCHANGED cancelActive
     \/ cancelActive' = [cancelActive EXCEPT ![r] = FALSE] /\ UNCHANGED <<intent, inMap>>
  /\ released' = [released EXCEPT ![r] = TRUE]
  /\ xpc' = [xpc EXCEPT ![r] = "idle"]
  /\ UNCHANGED <<ledger, ord, asgD, asgHost, fakeLock, marker, dispatching, canceled, selected, contended,
                 memHost, memGlobal, wpc, accHost, ghosts>>

-----------------------------------------------------------------------------
(* Agents and assignments                                                   *)

Register(h) ==
  /\ ~live[h]
  /\ live' = [live EXCEPT ![h] = TRUE]
  /\ hState' = [hState EXCEPT ![h] = IF hRun[h] = NoRun THEN "ready" ELSE "locked"]
  /\ UNCHANGED <<durable, memRun, hRun, memGlobal, procs, ghosts>>

LeaseExpire(h) ==
  /\ live[h]
  /\ live' = [live EXCEPT ![h] = FALSE]
  /\ UNCHANGED <<durable, memRun, hState, hRun, memGlobal, procs, ghosts>>

\* acceptHostAgentSession / dispatchQueuedRuns clearing an expired session
ClearExpired(h) ==
  /\ ~live[h] /\ hState[h] \in {"ready", "reserving"}
  /\ hState' = [hState EXCEPT ![h] = "offline"]
  /\ UNCHANGED <<durable, memRun, hRun, live, memGlobal, procs, ghosts>>

\* finishHostAgentAssignment: Commit(completed) frees Host and shared lock
AsgComplete(r) ==
  LET h == asgHost[r] IN
  /\ asgD[r] = "active" /\ wpc[r] # "commit"   \* Agent sees it only after AccPublish
  /\ asgD' = [asgD EXCEPT ![r] = "done"] /\ ledger' = [ledger EXCEPT ![r] = "completed"]
  /\ fakeLock' = [fakeLock EXCEPT ![h] = NoRun]
  /\ hRun' = [hRun EXCEPT ![h] = NoRun] /\ hState' = [hState EXCEPT ![h] = "offline"]
  /\ live' = [live EXCEPT ![h] = FALSE]
  /\ UNCHANGED <<intent, ord, asgHost, marker, memRun, memGlobal, procs, ghosts>>

-----------------------------------------------------------------------------
(* Restart: loadHostAgentState, loadControlPlaneQueue, resumeControlPlaneQueue *)

MarkerRun(r) == \E h \in Hosts : marker[h] = r
\* after recoverHostAgentQuarantineMarkers
RAsg(r) == IF MarkerRun(r) THEN "quarantined" ELSE asgD[r]
RAsgHost(r) == IF MarkerRun(r) THEN CHOOSE h \in Hosts : marker[h] = r ELSE asgHost[r]
RLedger(r) == IF MarkerRun(r) THEN "failed" ELSE ledger[r]
InAssignments(r) == RAsg(r) \in {"active", "quarantined"}

\* loadControlPlaneQueue result per run: [intent, ledger, queued, canceled]
Load(r) ==
  LET l == RLedger(r) IN
  IF intent[r] = "none" \/ InAssignments(r) THEN [i |-> "none", l |-> l, q |-> FALSE, c |-> FALSE]
  ELSE IF intent[r] = "admitting" /\ l = "none" THEN [i |-> "none", l |-> l, q |-> FALSE, c |-> FALSE]
  ELSE IF intent[r] = "admitting" /\ l \in Terminal THEN [i |-> "none", l |-> l, q |-> FALSE, c |-> FALSE]
  ELSE IF intent[r] = "canceling" THEN [i |-> "none", l |-> "canceled", q |-> FALSE, c |-> FALSE]
  ELSE IF l \in Terminal THEN [i |-> "none", l |-> l, q |-> FALSE, c |-> FALSE]
  ELSE [i |-> "queued", l |-> "queued", q |-> TRUE, c |-> FALSE]

\* conditions under which loadControlPlaneQueue returns a startup error
LoadErrorFor(r) ==
  LET l == RLedger(r) IN
  /\ intent[r] # "none" /\ ~InAssignments(r)
  /\ \/ intent[r] = "admitting" /\ l \notin {"none", "submitted", "queued"} \cup Terminal
     \/ intent[r] = "canceling" /\ l \notin {"submitted", "queued", "canceling", "canceled", "cleanup-failed"}
     \/ intent[r] = "queued" /\ l \notin {"submitted", "queued"} \cup Terminal

LoadError == \E r \in Runs : LoadErrorFor(r)

Restart ==
  /\ restarts < MaxRestarts /\ ~LoadError
  /\ restarts' = restarts + 1
  /\ intent' = [r \in Runs |-> Load(r).i]
  /\ ledger' = [r \in Runs |-> Load(r).l]
  /\ asgD' = [r \in Runs |-> RAsg(r)]
  /\ asgHost' = [r \in Runs |-> RAsgHost(r)]
  /\ marker' = [h \in Hosts |-> NoRun]
  \* reestablishHostAgentSharedLock for held assignments; other owners are stale
  /\ fakeLock' = [h \in Hosts |-> IF \E r \in Runs : InAssignments(r) /\ RAsgHost(r) = h
                                    THEN CHOOSE r \in Runs : InAssignments(r) /\ RAsgHost(r) = h
                                    ELSE NoRun]
  /\ hRun' = [h \in Hosts |-> fakeLock'[h]]
  /\ hState' = [h \in Hosts |-> IF fakeLock'[h] # NoRun THEN "locked" ELSE "offline"]
  /\ live' = [h \in Hosts |-> FALSE]
  /\ inMap' = [r \in Runs |-> Load(r).q]
  /\ canceled' = [r \in Runs |-> Load(r).c]
  /\ dispatching' = [r \in Runs |-> FALSE] /\ cancelActive' = [r \in Runs |-> FALSE]
  /\ released' = [r \in Runs |-> FALSE] /\ selected' = [r \in Runs |-> NoHost]
  /\ contended' = [r \in Runs |-> FALSE]
  /\ admLock' = NoRun /\ admissions' = 0 /\ disp' = NoClaim
  \* resumeControlPlaneQueue starts one waiter per queued Run; the rest died
  /\ wpc' = [r \in Runs |-> IF Load(r).q THEN "wait" ELSE IF wpc[r] = "idle" THEN "idle" ELSE "done"]
  /\ xpc' = [r \in Runs |-> "idle"]
  /\ stuck' = {}
  /\ UNCHANGED <<ord, nextOrd, accHost, everCanceled, assignCount>>

-----------------------------------------------------------------------------

Next ==
  \/ \E r \in Runs :
       \/ Submit(r) \/ AdmWriteIntent(r) \/ AdmAccept(r) \/ AdmAcceptFail(r) \/ AdmMarkQueued(r)
       \/ AdmPublish(r) \/ AdmFail(r)
       \/ WaitSelected(r) \/ WaitCanceled(r)
       \/ AccFirstCheck(r) \/ AccRelease1(r) \/ AccRequeue(r) \/ ReAdmWrite(r) \/ ReAdmFail(r)
       \/ AccCommit(r) \/ AccPublish(r) \/ AccEligFail(r) \/ AccRollbackStatus(r) \/ AccFailBefore(r)
       \/ CancelStart(r) \/ CancelPrepare(r) \/ CancelCleanup(r) \/ CancelError(r) \/ CancelRemove(r)
       \/ AsgComplete(r)
  \/ \E h \in Hosts : DispClaim(h) \/ Register(h) \/ LeaseExpire(h) \/ ClearExpired(h)
  \/ DispAcquire
  \/ Restart

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* Properties                                                               *)

\* A Run that recorded cancellation never gets a Host assignment.
CanceledNeverAssigned == \A r \in Runs : ~(everCanceled[r] /\ assignCount[r] > 0)

\* A Run ID becomes a Host assignment at most once.
AssignOnce == \A r \in Runs : assignCount[r] <= 1

\* A Run holds a Host once it wrote that Host's quarantine marker or committed.
Holds(r, h) ==
  \/ wpc[r] \in {"acc2", "commit"} /\ accHost[r] = h
  \/ asgD[r] \in {"active", "quarantined"} /\ asgHost[r] = h

\* Two Runs never hold one Host; one Run never holds two Hosts.
HostExclusive ==
  /\ \A h \in Hosts, r1, r2 \in Runs : Holds(r1, h) /\ Holds(r2, h) => r1 = r2
  /\ \A r \in Runs, h1, h2 \in Hosts : Holds(r, h1) /\ Holds(r, h2) => h1 = h2

\* A reserved Host or held shared lock always has a live owner that will
\* finish or release it (otherwise the Host is stuck until restart).
WaiterAlive(r) == wpc[r] \in {"wait"} \cup AccPhases
ReservationOwned ==
  \A h \in Hosts :
    LET r == fakeLock[h] IN
    /\ hState[h] = "reserving" =>
         \/ disp.h = h
         \/ \E q \in Runs : (inMap[q] /\ selected[q] = h /\ WaiterAlive(q))
                            \/ (wpc[q] \in AccPhases /\ accHost[q] = h)
    /\ r # NoRun /\ h \notin stuck =>
         \/ inMap[r] /\ selected[r] = h /\ WaiterAlive(r)
         \/ wpc[r] \in AccPhases /\ accHost[r] = h
         \/ asgD[r] \in {"active", "quarantined"} /\ asgHost[r] = h

\* The durable queue never holds more waiting Runs than the bound.
QueueBound == Waiting + admissions <= MaxQueued

\* A queued, not yet assigned Run is never stranded outside the queue.
QueuedRecoverable ==
  \A r \in Runs :
    ledger[r] \in {"submitted", "queued"} /\ asgD[r] = "none" =>
      /\ intent[r] # "none"
      /\ inMap[r] \/ wpc[r] \in AdmPhases

\* Durable state always passes loadControlPlaneQueue.
RestartLoadable == ~LoadError

\* A terminal Run Ledger outcome is never rewritten (action property).
\* canceled <-> cleanup-failed are both cancellation outcomes: a retried
\* cancel may redo cleanupQueuedRunCancellation.
OutcomeClass(l) == IF l \in {"canceled", "cleanup-failed"} THEN "canceled" ELSE l
TerminalLedgerStable ==
  [][\A r \in Runs : ledger[r] \in Terminal => OutcomeClass(ledger'[r]) = OutcomeClass(ledger[r])]_vars

=============================================================================
