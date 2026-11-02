---- MODULE MC ----
\* Canary* predicates are reachability probes; check.sh expects each violated.
EXTENDS EmbeddedLedger
CONSTANTS r1, r2, r3
MCSymmetry == Permutations(Runs)

CanaryNoCompletedRun == \A x \in Runs : ~(pc[x] = "done" /\ ledger[x] = "terminal")
CanaryNoPrune == \A x \in Runs : ~(everAccepted[x] /\ ledger[x] = "none")
CanaryNoCrashRecoveryStop == \A x \in Runs : ~(Dead(x) /\ ledger[x] = "terminal")
CanaryNoLockTakeover == \A x \in Runs : ~(Dead(x) /\ rs[x] = "running" /\ lockKind = "active" /\ lockRun # x)
CanaryNoSweepRecovery == \A x \in Runs : ~expired[x]
CanaryNoStopDuringFinish == \A x \in Runs : ~(pc[x] = "preserved" /\ ledger[x] = "terminal")
\* Unfiled finding: a live controller before preserve still owns the repo
\* Host Lock. Broken when `stop` finishes a run between its
\* preserveStoppedRunForCleanup record write and markKept, another run takes
\* the lock, and markKept (replaceHostLockOwner) overwrites it.
LiveRunOwnsLock ==
  \A x \in Runs : pc[x] \in {"locked", "recorded", "active", "stopped"} => lockRun = x
\* Diagnostic: the crash windows after run-record.json exists (lock takeover,
\* crash between writeRunRetentionRecord and markActive).
CrashedRunRecoverableWithRecord ==
  \A x \in Runs : (Dead(x) /\ rs[x] \in {"running", "kept"} /\ ledger[x] \in {"submitted", "cleanup", "kept"}) => StopOK(x)
\* Diagnostic: `stop` finished the run under its live finishing controller,
\* which then checkpointed broker-cleaned and crashed.
CrashedRunRecoverableAfterStop ==
  \A x \in Runs : (Dead(x) /\ rs[x] = "none" /\ ledger[x] = "cleanup") => StopOK(x)
====
