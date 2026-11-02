---- MODULE MC ----
\* Bounds for every check. Canary* predicates are reachability probes:
\* check.sh expects TLC to violate each one, so the safety passes are not vacuous.
EXTENDS ControlPlaneRunQueue
CONSTANTS r1, r2, r3, h1, h2
MCSymmetry == Permutations(Runs) \cup Permutations(Hosts)

CanaryNoAssignment == \A r \in Runs : assignCount[r] = 0
CanaryNoParallelAssignments == ~(\A h \in Hosts : hRun[h] # NoRun)
CanaryNoCancel == \A r \in Runs : ledger[r] # "canceled"
CanaryNoContention == \A r \in Runs : ~contended[r]
CanaryNoRequeue == \A r \in Runs : wpc[r] # "requeue"
CanaryNoRestartWithQueue == ~(restarts > 0 /\ \E r \in Runs : inMap[r])
CanaryNoQueueFull == ~(Waiting + admissions = MaxQueued /\ \E r \in Runs : wpc[r] = "idle")
CanaryNoReadmission == \A r \in Runs : wpc[r] # "readm2"
CanaryNoInertEntry == \A r \in Runs : ~(inMap[r] /\ canceled[r] /\ ledger[r] = "failed")
====
