---- MODULE MC ----
\* Bounds for check.sh. Canary* predicates are reachability probes that
\* check.sh expects TLC to violate (the model reaches that situation).
EXTENDS RunRetention
CONSTANTS r1, r2
MCSymmetry == Permutations(Runs)

CanaryNoKept == \A r \in Runs : rec[r] # "kept"
CanaryNoKeptStop == \A r \in Runs : ~(sw[r] = "release" /\ rec[r] = "kept" /\ bound[r] /\ phase[r] = "broker-cleaned")
CanaryNoCleanupPending == \A r \in Runs : ~(rec[r] = "kept" /\ phase[r] = "session-stopped" /\ lockKind = "kept" /\ Owns(r))
CanaryNoBoundDeadStop == \A r \in Runs : ~(sw[r] = "release" /\ rec[r] = "running" /\ bound[r])
CanaryNoUnboundDeadStop == \A r \in Runs : ~(sw[r] = "release" /\ rec[r] = "running" /\ ~bound[r])
CanaryNoReclaim ==
  \A r \in Runs : ~(pc[r] = "dead" /\ mirror[r] = "active" /\ lockRun \notin {r, NoRun})
CanaryHostNeverReused == \E r \in Runs : pc[r] # "done"
CanaryNoHeartbeatFailure == \A r \in Runs : ~hbErr[r]
CanaryNoFenceRefusal == ~refused
CanaryNoStandingSession == broker = NoRun \/ lockRun = broker
====
