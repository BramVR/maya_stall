# Enforce Shared Host Lock Deadlines

Every configured Control Plane Host Lock carries durable `lastHeartbeatAt`,
`idleDeadline`, and `hardDeadline` timestamps in both the assignment journal and
Host Lock record. The default idle timeout is 30 minutes and the default hard
lifetime is 6 hours. `control-plane serve` may set positive values with
`--host-lock-idle-timeout` and `--host-lock-hard-lifetime`; the hard lifetime
must exceed the idle timeout. Run progress and Windows Host Agent heartbeats
refresh the idle deadline but never move the hard deadline.

Any Control Plane request observes these timestamps before serving its normal
operation. An elapsed idle or hard deadline atomically moves the assignment and
Host Lock to `expiring`, records the reason, and directs the current or
replacement Agent to clean the durable assignment. A disappeared Agent no
longer quarantines an otherwise verified assignment immediately: its Host Lock
becomes reclaimable at the idle deadline. Existing pre-deadline Host Locks gain
one full policy window when first loaded after upgrade.

A retained run moves the same Host Lock to `kept` and adds `keepDeadline`.
Configured and embedded `extend --by <duration> <run-id>` is an explicit
authorized action. It extends from the current keep deadline, records an event,
and fails if the result would exceed the Host Lock hard deadline. Configured
status exposes idle, hard, and keep deadlines plus remaining keep time.

Expiry asks the Windows Host Agent to use the existing retained-run cleanup
path. Before invoking the Session Broker, the Agent verifies that the private
Run Manifest and Run Record identify the same Run ID, Broker adapter, and Maya
UI Session stored on the shared Host Lock. The Broker checks the current
session identity again. A different session fails closed without a stop call;
an already-stopped matching session records an idempotent observation. Only
after exact-session stop, run-owned state cleanup, terminal event transfer, and
durable completion does the Control Plane release the Host Lock.

Before removing its per-run workspace, the Agent writes a private completion
outbox entry containing the fenced terminal mutation. A replacement
process-session can replay that entry after a response loss or Agent crash; an
acknowledged terminal assignment clears the outbox without rerunning Maya.

The required real SSH proof includes a shared Kept Session subtest that extends
within policy, advances a controllable clock to expiry, stops the exact bound
Maya UI Session, and verifies zero Agent, remote workspace, or Host Lock
residue.

This extends [ADR 0033](0033-manage-run-retention-on-owned-hosts.md),
[ADR 0046](0046-complete-a-fake-scenario-through-a-registered-windows-host-agent.md),
[ADR 0047](0047-complete-a-real-scenario-through-the-shared-path.md), and
[ADR 0048](0048-detach-runs-and-stream-sequenced-events.md).
