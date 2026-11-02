---- MODULE MC ----
\* Bounds for check.sh. Canary* predicates are reachability probes that
\* check.sh expects TLC to violate (the model reaches that situation).
EXTENDS LocalHostLock, TLC
CONSTANTS p1, p2, p3, p4
MCSymmetry == Permutations(Procs)

CanaryNoKeptLeftBehind == ~(lockKind = "kept" /\ \A p \in Procs : pc[p] \in {"idle", "done"})
CanaryNoStaleLockTakeover == ~(\E p \in Procs : pc[p] = "l_remove")
CanaryNoStaleMutexBreak == ~(\E p \in Procs : pc[p] = "m_remove")
CanaryNoKeptStop == ~(\E p \in Procs : pc[p] = "stop" /\ lockKind = "none")
CanaryNoHostReuse == ~(\E p, q \in Procs : p # q /\ pc[p] = "done" /\ pc[q] = "holding" /\ alive[p])
====
