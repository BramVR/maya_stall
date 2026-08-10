# Admit Declared Runtime Inputs And Run Local Sessiond

A Scenario may declare a required named Runtime Input only as a file slot with
an explicit extension allowlist and deterministic destination. The user binds
that name to one absolute local file with `--input`. Both `plan` and Embedded
Mode `run` use the same normalization and path-safety decision. Directories,
implicit siblings, scripts, credential formats, undeclared names, duplicate
bindings, symlinks, and reparse points are outside this boundary.

Planning reads and hashes the selected file without acquiring a Host Lock or
contacting Maya. Execution snapshots it before Host Lock acquisition and Maya
launch, then verifies the source and snapshot identity again. The snapshot is
an immutable Run Payload item named `runtimeInput:file`; the Scenario receives
only its staged path through `MAYA_STALL_RUNTIME_INPUTS`. Private run evidence
records name, kind, destination, size, and SHA-256, never the user's absolute
source path. This deliberately extends the repo-owned Run Payload boundary to
user-selected content admitted through a Scenario-owned slot.

Embedded Mode also supports a `local-sessiond` runtime on a logged-in Windows
workstation. It reuses the existing Fresh Run lifecycle, Run ID, Host Lock,
manifest, Evidence Bundle, Validators, Stop Policy, Run Ledger, and cleanup.
It invokes one explicitly configured `gg_mayasessiond` checkout directly and
uses local filesystem staging/collection. SSH, SFTP, scheduled remote recovery,
Configured Control Plane, and Windows Host Agent paths are not fallback paths.
For both local and SSH adapters, `running` plus `call_server_ready` is only an
intermediate state: Fresh Run waits until a read-only `scene.info` call and a
small `viewport.capture` both succeed before treating the new Maya UI Session
as executable and evidence-ready.

The configured work root identifies one workstation execution slot, independent
of consuming-repo host aliases. Its fixed `state/locks/hosts/host.lock` is
authoritative across Consuming Repo checkouts. After
Sessiond launches, the exact broker session ID is added to that lock before
staging or execution. Stop checks the current Sessiond ID before mutation;
cleanup removes only `workRoot/runs/<run-id>`. Uncertain identity, stop,
evidence, or cleanup fails the run closed and retains ownership for recovery.

Kangaroo folder discovery and plug-in-specific checks remain in a Consuming
Repo or thin launcher. Configuring or installing Maya, Sessiond, MayaMCP, and
Windows login policy remains operator-owned.
