# Complete A Fake Scenario Through A Registered Windows Host Agent

The next shared-path slice will route one fake Scenario through a registered
Windows Host Agent without moving Windows-specific execution into the
host-neutral Control Plane core.

An operator enrolls a fixed Agent and Maya Host with a distinct scoped
credential outside Repo Run Config. The Control Plane stores only its digest.
The Agent initiates authenticated HTTPS registration and assignment polling and
advertises exactly one slot; the Control Plane never opens an inbound Host
connection.

Registration also creates a leased process-session fence. Only that process may
poll, confirm, fail, or complete work, and heartbeats keep the lease live during
execution. A concurrent registration is rejected until the lease expires; an
expired process can then be fenced out while a replacement resumes the durable
assignment. Terminal `run-once` transitions clear the session and mark the Agent
offline so readiness never outlives the polling process.

Before publishing an assignment, the Control Plane durably and atomically owns
a shared Host Lock with a unique random token. The Agent must confirm that exact
token before execution and present it again with completion. Unauthorized
credentials, stale tokens, and second assignments cannot mutate the active lock
or assignment. Active locks survive Control Plane restart and remain
unavailable until explicitly completed.

Every assignment-state change first writes a private transition journal. The
Control Plane then applies the Host Lock and assignment files and removes the
journal. Startup replays any surviving journal before loading active locks, so
a process stop between durable writes cannot leave mismatched state that blocks
restart.

The Agent materializes only the submitted repo snapshot, generates private fake
Host config outside Repo Run Config, and drives the existing host-neutral Fresh
Run lifecycle. It transfers only bounded Run Ledger and Evidence Bundle paths.
The Control Plane validates and durably stages those files before making the
terminal record visible and releasing the Host Lock. The completion journal
publishes the terminal ledger while preserving the Control Plane's original Run
ID, Scenario, Target Profile, Host, acceptance timestamp, and durable path
metadata; reads remain gated until that journal commits. Successful cleanup
removes the Agent run workspace before completion is accepted.

The command is intentionally `run-once`: this slice proves enrollment,
assignment, fencing, fake Session Broker execution, Evidence transfer, cleanup,
and release. Reconnecting background service behavior remains later work. Real
Maya execution is added by
[ADR 0047](0047-complete-a-real-scenario-through-the-shared-path.md).

Because `run-once` cannot retain an interactive session, this slice accepts only
`stop-after always`. Retaining Stop Policies fail before assignment or Host Lock
mutation.

[ADR 0051](0051-enforce-shared-host-lock-deadlines.md) supersedes that
retention limitation with durable kept-session ownership, deadline enforcement,
and exact-session cleanup.

This extends [ADR 0045](0045-complete-a-fake-control-plane-scenario.md). The
no-enrollment in-process fake path remains compatible while consumers adopt the
Agent boundary.
