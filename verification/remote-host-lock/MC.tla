---- MODULE MC ----
\* Canary* predicates are reachability probes: check.sh expects TLC to
\* violate each one, so the safety passes are not vacuous.
EXTENDS RemoteHostLock, TLC
CONSTANTS c1, c2, c3
MCSymmetry == Permutations(Clients)

CanaryNoSession == sess = None
\* someone else holds the lock while an earlier owner never released it
CanaryNoReclaim ==
  ~(\E c, d \in Clients : c # d /\ lockOwner = c /\ myInc[d] # 0
                          /\ pc[d] \in {"owned", "active", "steps", "stopped", "dead"})
CanaryNoKept == \A c \in Clients : pc[c] # "kept"
CanaryNoDelayedCreate == ~(\E c \in Clients : lockOwner = c /\ pc[c] = "failed")
CanaryNoHostReuse == Cardinality({c \in Clients : pc[c] = "done"}) < 2
====
