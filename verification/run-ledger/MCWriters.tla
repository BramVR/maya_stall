---- MODULE MCWriters ----
\* Canary* predicates are reachability probes: check-writers.sh expects TLC to
\* violate each one on the passing (fixed) config, so passes are not vacuous.
EXTENDS RunLedgerWriters

CanaryNoAgentEventStreamed == ~(\E i \in 1..Len(streamed) : streamed[i] = "A")
CanaryNoCompletionAfterFail ==
  ~(phase = "completed" /\ HasFail(E) /\ ~(\E i \in 1..Len(E) : E[i] = "T"))
CanaryNoCompletionViaTransfer == ~(phase = "completed" /\ \E i \in 1..Len(E) : E[i] = "T")
CanaryNoQuarantineCommitted == ~(phase = "quarantined" /\ dPhase = "quarantined" /\ op = "none" /\ HasFail(L))
CanaryNoFollowRefreshDuringFinish ==
  ~(rf = "held" /\ op \in {"fin.commit", "fin.cleanup", "fin.completed"})
CanaryNoCrashMidFail == ~(crashes > 0 /\ Len(L) > 1 /\ phase = "confirmed")
CanaryNoRetryAfterReset == ~(attempts >= 2 /\ phase = "confirmed" /\ op = "none")
====
