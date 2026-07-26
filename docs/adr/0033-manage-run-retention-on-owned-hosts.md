# Manage Run Retention On Owned Hosts

Maya Stall v1 will include basic retention cleanup for remote run workspaces and local run state, with optional Evidence Store retention. Crabbox static SSH hosts leave cleanup to the user, but Maya Stall uses persistent owned Maya Hosts where run directories and evidence can pile up; Kept Sessions still need an explicit TTL so a host is not locked forever.

Run Retention keeps public Evidence Bundles redacted and stores reconnect/cleanup details in hidden local Run Records under `.maya-stall/state/runs/<run-id>/run-record.json`. A kept run records the Stop Policy reason, Host Lock owner, local paths, remote run workspace, broker adapter, broker capabilities, and retained remote session metadata.

Each kept Run Record also stores a Go-duration `keepTTL` and UTC RFC3339Nano `keepDeadline`. The default TTL is 90 minutes and `run --keep-ttl <duration>` overrides it for that run. Maya Stall has no coordinator in v1, so `run` and `doctor` opportunistically sweep only retained Run Records for the specific Maya Host they are about to use. An overdue record stops through the same Session Broker retained-session cleanup path as explicit `stop`, updates durable run state, and records a `kept-session-expired` event. Records from older versions receive a fresh `now + TTL` grace deadline on first contact instead of deriving expiry from `createdAt`.

[ADR 0051](0051-enforce-shared-host-lock-deadlines.md) extends this embedded
behavior for Configured Control Plane Mode with shared Host Lock heartbeats,
idle and hard deadlines, authorized extension, and Agent-owned exact-session
expiry cleanup.

The sweep never enumerates arbitrary broker sessions. Missing Run Records, unsupported `StopRetainedSession` capability, invalid deadlines, and broker cleanup failures remain locked, emit a warning and run event where durable record state is available, and do not abort the surrounding `run` or `doctor` operation.

`status --run <id>` must read local state and then query the Session Broker. If the retained broker session disappeared or changed, it reports stale/orphaned state. `attach <id>` is observational: local events/logs/Scenario Result plus broker report data, not an interactive desktop viewer. `stop <id>` asks the broker to stop the retained Maya UI Session and clean the remote workspace before local run state or Host Locks are removed. Unsupported or failed broker cleanup must fail explicitly with this ADR and the Windows host setup guide as cleanup references.
