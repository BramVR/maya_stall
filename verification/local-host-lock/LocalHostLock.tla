--------------------------- MODULE LocalHostLock ---------------------------
(***************************************************************************)
(* File-based local Host Lock for one Maya Host, as used by Embedded Mode   *)
(* processes (`maya-stall run`, `doctor`, `stop`) in one checkout.          *)
(*                                                                         *)
(* Two files, both taken with acquireHostLockAtDirWithContent              *)
(* (internal/cli/host_pool.go):                                            *)
(*   mutex  <lockDir>/.mutexes/<host>.lock, taken by withLocalHostSideMutex *)
(*          with content "pid: <p>"                                        *)
(*   lock   <lockDir>/<host>.lock, taken by acquireHostLock inside the      *)
(*          mutex, later rewritten by markHostLockActive/markHostLockKept   *)
(* Acquire = os.Link(temp, path). On EEXIST: read the file, decide          *)
(* isStaleHostLock (owner pid gone; keptRun never stale), os.Remove(path),  *)
(* retry. Read+decide and remove are separate steps (MutexCheck /           *)
(* MutexRemoveStale, LockCheck / LockRemoveStale): nothing serialises them  *)
(* for the mutex file itself.                                               *)
(*                                                                         *)
(* Mode "ssh": acquireRunHostLock takes the authoritative host-side lock    *)
(* first (an ideal lock here; verified in verification/remote-host-lock)    *)
(* and releases it after the local lock.                                    *)
(* Mode "local": the local file lock is the only Host Lock. Same code shape *)
(* as acquireLocalHostSideLock: the local-sessiond Windows runtime          *)
(* (transport: local, <workRoot>/state/locks/hosts/host.lock, stale =       *)
(* isRecoverableLocalSessiondHostLock), the fake Host with absolute         *)
(* workRoot, the fake-SSH hook, and the Control Plane shared fake-host lock. *)
(* AtomicBreak = TRUE models a candidate fix: a stale break removes the file *)
(* only if it still names the dead owner that was read.                     *)
(***************************************************************************)
EXTENDS Naturals, FiniteSets

CONSTANTS Procs, NoProc, Mode, MaxCrashes, AtomicBreak

VARIABLES
  mutexOwner,  \* pid in the mutex file; NoProc = absent
  lockKind,    \* main lock content: "none" (absent), "pid", "active", "kept"
  lockPid,     \* pid in the main lock (NoProc for kept/absent)
  lockRun,     \* run named by activeRun/keptRun (run = its controller process)
  remote,      \* holder of the host-side lock (Mode "ssh"); NoProc = free
  remoteKept,  \* host-side lock is a keptRun lock
  alive,       \* process (pid) still exists
  pc,
  intent,      \* what the process does once it holds the mutex
  holds,       \* process believes it holds the local Host Lock
  marked,      \* runHostLock.ownedRunID set (markActive succeeded)
  target,      \* kept run a `stop` process removes
  obsMutex,    \* dead mutex owner seen by the last staleness read
  obsLock,     \* lock content seen by the last staleness read
  keptSession, \* ghost: run whose Kept Session is live (set by markKept, cleared by stop)
  crashes

vars == <<mutexOwner, lockKind, lockPid, lockRun, remote, remoteKept, alive, pc,
          intent, holds, marked, target, obsMutex, obsLock, keptSession, keptSession, crashes>>

Init ==
  /\ mutexOwner = NoProc
  /\ lockKind = "none" /\ lockPid = NoProc /\ lockRun = NoProc
  /\ remote = NoProc /\ remoteKept = FALSE
  /\ alive = [p \in Procs |-> TRUE]
  /\ pc = [p \in Procs |-> "idle"]
  /\ intent = [p \in Procs |-> "l_link"]
  /\ holds = [p \in Procs |-> FALSE]
  /\ marked = [p \in Procs |-> FALSE]
  /\ target = [p \in Procs |-> NoProc]
  /\ obsMutex = [p \in Procs |-> NoProc]
  /\ obsLock = [p \in Procs |-> <<"none", NoProc, NoProc>>]
  /\ keptSession = NoProc
  /\ crashes = 0

Goto(p, s) == pc' = [pc EXCEPT ![p] = s]
Lock == <<lockKind, lockPid, lockRun>>
Remote == <<remote, remoteKept>>
ClearLock == lockKind' = "none" /\ lockPid' = NoProc /\ lockRun' = NoProc
AfterGiveUp(p) == IF Mode = "ssh" /\ intent[p] = "l_link" THEN "rel_remote" ELSE "done"

\* ---- acquireRunHostLock (ssh: host-side first) / acquireHostLock (local) ----
Start(p) ==
  /\ pc[p] = "idle" /\ alive[p]
  /\ IF Mode = "ssh"
       THEN IF remote = NoProc
              THEN remote' = p /\ remoteKept' = FALSE /\ Goto(p, "m_link")
              ELSE UNCHANGED Remote /\ Goto(p, "done")
       ELSE UNCHANGED Remote /\ Goto(p, "m_link")
  /\ UNCHANGED <<mutexOwner, Lock, alive, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* ---- withLocalHostSideMutex: acquireHostLockAtDir(mutexDir) loop ----
MutexLink(p) ==
  /\ pc[p] = "m_link" /\ alive[p]
  /\ IF mutexOwner = NoProc
       THEN mutexOwner' = p /\ Goto(p, intent[p])
       ELSE UNCHANGED mutexOwner /\ Goto(p, "m_check")
  /\ UNCHANGED <<Lock, Remote, alive, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

MutexCheck(p) ==
  /\ pc[p] = "m_check" /\ alive[p]
  /\ \/ mutexOwner = NoProc /\ Goto(p, "m_link") /\ UNCHANGED obsMutex
     \/ /\ mutexOwner # NoProc /\ ~alive[mutexOwner]
        /\ obsMutex' = [obsMutex EXCEPT ![p] = mutexOwner]
        /\ Goto(p, "m_remove")
     \/ mutexOwner # NoProc /\ alive[mutexOwner] /\ Goto(p, "m_link") /\ UNCHANGED obsMutex
     \* 5s timeout
     \/ mutexOwner # NoProc /\ alive[mutexOwner] /\ Goto(p, AfterGiveUp(p)) /\ UNCHANGED obsMutex
  /\ UNCHANGED <<mutexOwner, Lock, Remote, alive, intent, holds, marked, target, obsLock, keptSession, crashes>>

\* os.Remove(path) removes whatever mutex file is there now
MutexRemoveStale(p) ==
  /\ pc[p] = "m_remove" /\ alive[p]
  /\ mutexOwner' = IF AtomicBreak /\ mutexOwner # obsMutex[p] THEN mutexOwner ELSE NoProc
  /\ Goto(p, "m_link")
  /\ UNCHANGED <<Lock, Remote, alive, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* ---- acquireHostLock body: acquireHostLockAtDir(lockDir) ----
LockLink(p) ==
  /\ pc[p] = "l_link" /\ alive[p]
  /\ IF lockKind = "none"
       THEN /\ lockKind' = "pid" /\ lockPid' = p /\ lockRun' = NoProc
            /\ holds' = [holds EXCEPT ![p] = TRUE]
            /\ Goto(p, "m_release")
       ELSE UNCHANGED <<Lock, holds>> /\ Goto(p, "l_check")
  /\ UNCHANGED <<mutexOwner, Remote, alive, intent, marked, target, obsMutex, obsLock, keptSession, crashes>>

LockCheck(p) ==
  /\ pc[p] = "l_check" /\ alive[p]
  /\ IF lockKind = "none"
       THEN Goto(p, "l_link") /\ UNCHANGED obsLock
       ELSE IF lockKind # "kept" /\ ~alive[lockPid]
              THEN Goto(p, "l_remove") /\ obsLock' = [obsLock EXCEPT ![p] = <<lockKind, lockPid, lockRun>>]
              ELSE Goto(p, "m_release") /\ UNCHANGED obsLock   \* locked
  /\ UNCHANGED <<mutexOwner, Lock, Remote, alive, intent, holds, marked, target, obsMutex, keptSession, crashes>>

LockRemoveStale(p) ==
  /\ pc[p] = "l_remove" /\ alive[p]
  /\ IF AtomicBreak /\ Lock # obsLock[p]
       THEN UNCHANGED Lock
       ELSE ClearLock
  /\ Goto(p, "l_link")
  /\ UNCHANGED <<mutexOwner, Remote, alive, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* mutex release(): os.Remove(mutex path), unconditional
MutexRelease(p) ==
  /\ pc[p] = "m_release" /\ alive[p]
  /\ mutexOwner' = NoProc
  /\ Goto(p, IF holds[p] THEN "holding" ELSE AfterGiveUp(p))
  /\ UNCHANGED <<Lock, Remote, alive, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* ---- holder: markHostLockActive/Kept (under mutex) or release ----
WantMark(p) ==
  /\ pc[p] = "holding" /\ alive[p] /\ ~marked[p]
  /\ intent' = [intent EXCEPT ![p] = "mark"]
  /\ Goto(p, "m_link")
  /\ UNCHANGED <<mutexOwner, Lock, Remote, alive, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* replaceHostLockOwner: rename over the lock file, no content check.
\* markKept hands the Host to the Kept Session (run.releaseHostLock = false).
Mark(p) ==
  /\ pc[p] = "mark" /\ alive[p]
  /\ mutexOwner' = NoProc
  /\ \E k \in {"active", "kept"} :
       /\ lockKind' = k /\ lockRun' = p
       /\ lockPid' = IF k = "kept" THEN NoProc ELSE p
       /\ IF k = "kept" /\ Mode = "ssh" THEN remoteKept' = TRUE ELSE UNCHANGED remoteKept
       /\ IF k = "kept"
            THEN holds' = [holds EXCEPT ![p] = FALSE] /\ Goto(p, "done") /\ keptSession' = p
            ELSE UNCHANGED <<holds, keptSession>> /\ Goto(p, "holding")
  /\ marked' = [marked EXCEPT ![p] = TRUE]
  /\ UNCHANGED <<remote, alive, intent, target, obsMutex, obsLock, crashes>>

\* runHostLock.release: ownedRunID set -> removeRepoHostLockForRun under mutex
WantRelease(p) ==
  /\ pc[p] = "holding" /\ alive[p] /\ marked[p]
  /\ intent' = [intent EXCEPT ![p] = "rel_owned"]
  /\ Goto(p, "m_link")
  /\ UNCHANGED <<mutexOwner, Lock, Remote, alive, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

ReleaseOwned(p) ==
  /\ pc[p] = "rel_owned" /\ alive[p]
  /\ mutexOwner' = NoProc
  /\ IF lockRun = p THEN ClearLock ELSE UNCHANGED Lock
  /\ holds' = [holds EXCEPT ![p] = FALSE]
  /\ Goto(p, IF Mode = "ssh" THEN "rel_remote" ELSE "done")
  /\ UNCHANGED <<Remote, alive, intent, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* runHostLock.release before markActive: localRelease = os.Remove(lockPath),
\* no mutex and no content check (acquireHostLockAtDirWithContent's closure)
ReleaseUnmarked(p) ==
  /\ pc[p] = "holding" /\ alive[p] /\ ~marked[p]
  /\ ClearLock
  /\ holds' = [holds EXCEPT ![p] = FALSE]
  /\ Goto(p, IF Mode = "ssh" THEN "rel_remote" ELSE "done")
  /\ UNCHANGED <<mutexOwner, Remote, alive, intent, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* hostSideLock.release after the local release
ReleaseRemote(p) ==
  /\ pc[p] = "rel_remote" /\ alive[p]
  /\ IF remote = p THEN remote' = NoProc /\ remoteKept' = FALSE ELSE UNCHANGED Remote
  /\ Goto(p, "done")
  /\ UNCHANGED <<mutexOwner, Lock, alive, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* `stop <kept run>` from a new process (stopKeptRunAfterBrokerStop ->
\* releaseHostSideLock, then finishReleasedStopRun -> removeRepoHostLockForRun)
StartStop(p) ==
  /\ pc[p] = "idle" /\ alive[p]
  /\ lockKind = "kept" /\ lockRun \notin {p, NoProc}
  /\ target' = [target EXCEPT ![p] = lockRun]
  \* StopRetainedRun stops the Kept Session before any lock is released
  /\ keptSession' = IF keptSession = lockRun THEN NoProc ELSE keptSession
  /\ IF Mode = "ssh" /\ remote = lockRun /\ remoteKept
       THEN remote' = NoProc /\ remoteKept' = FALSE
       ELSE UNCHANGED Remote
  /\ intent' = [intent EXCEPT ![p] = "stop"]
  /\ Goto(p, "m_link")
  /\ UNCHANGED <<mutexOwner, Lock, alive, holds, marked, obsMutex, obsLock, crashes>>

StopRemove(p) ==
  /\ pc[p] = "stop" /\ alive[p]
  /\ mutexOwner' = NoProc
  /\ IF lockRun = target[p] THEN ClearLock ELSE UNCHANGED Lock
  /\ Goto(p, "done")
  /\ UNCHANGED <<Remote, alive, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

\* process killed (CI cancel, Ctrl-C, crash); its files stay behind
Crash(p) ==
  /\ alive[p] /\ pc[p] \notin {"idle", "done"} /\ crashes < MaxCrashes
  /\ alive' = [alive EXCEPT ![p] = FALSE]
  /\ crashes' = crashes + 1
  /\ UNCHANGED <<mutexOwner, Lock, Remote, pc, intent, holds, marked, target, obsMutex, obsLock, keptSession>>

\* host-side lease of a dead active owner expires and the broker proves
\* inactivity (remoteHostLockReclaimAllowed); kept locks are not reclaimed
RemoteReclaim ==
  /\ Mode = "ssh" /\ remote # NoProc /\ ~alive[remote] /\ ~remoteKept
  /\ remote' = NoProc
  /\ UNCHANGED <<mutexOwner, Lock, remoteKept, alive, pc, intent, holds, marked, target, obsMutex, obsLock, keptSession, crashes>>

Next ==
  \/ \E p \in Procs :
       \/ Start(p) \/ MutexLink(p) \/ MutexCheck(p) \/ MutexRemoveStale(p)
       \/ LockLink(p) \/ LockCheck(p) \/ LockRemoveStale(p) \/ MutexRelease(p)
       \/ WantMark(p) \/ Mark(p) \/ WantRelease(p) \/ ReleaseOwned(p)
       \/ ReleaseUnmarked(p) \/ ReleaseRemote(p) \/ StartStop(p) \/ StopRemove(p)
       \/ Crash(p)
  \/ RemoteReclaim

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------
(* Properties                                                               *)

\* Kept Session owner: a run whose Kept Session is still live
KeptOwner(p) == keptSession = p
Owner(p) == (alive[p] /\ holds[p]) \/ KeptOwner(p)

\* Two live controllers never both hold the local Host Lock, and nobody
\* holds it while another run's Kept Session is live.
LockExclusive == \A p, q \in Procs : Owner(p) /\ Owner(q) => p = q

\* A live holder's lock file still names it: nobody removed or replaced it.
HolderFileIntact ==
  \A p \in Procs : alive[p] /\ holds[p] /\ pc[p] = "holding" => (lockPid = p \/ lockRun = p)

TypeOK ==
  /\ lockKind \in {"none", "pid", "active", "kept"}
  /\ mutexOwner \in Procs \cup {NoProc} /\ remote \in Procs \cup {NoProc}
=============================================================================
