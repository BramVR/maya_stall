---- MODULE MC ----
\* Small bounds for every check. Canary* predicates are reachability probes:
\* check.sh expects TLC to violate each one, which shows the model reaches
\* that situation and the safety invariants are not passing vacuously.
EXTENDS HostAgentHostLock
CONSTANTS r1, r2, r3, p1, p2, p3, p4
MCSymmetry == Permutations(Runs) \cup Permutations(Procs)

CanaryNoMaya == \A p \in Procs : ~mayaLive[p]
CanaryHostNeverReused == Cardinality({r \in Runs : dAsg[r] = "completed"}) < 2
CanaryNoQuarantineOverLiveMaya ==
  ~(lockState = "quarantined" /\ \E p \in Procs : mayaLive[p])
CanaryNoConfirmedAcrossRestart ==
  ~(restarts > 0 /\ \E r \in Runs : asg[r] = "confirmed" /\ graceActive)
CanaryNoTakeoverOfAssigned ==
  ~(\E p, q \in Procs : p # q /\ prun[p] = prun[q] /\ prun[p] # NoRun /\ pc[q] = "confirmed" /\ pc[p] = "dead")
\* every quarantine path except marker recovery needs a process that polled the run
CanaryNoMarkerRecovery ==
  ~(lockState = "quarantined" /\ \A p \in Procs : prun[p] # lockRun)
====
