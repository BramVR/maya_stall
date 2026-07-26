# run

`maya-stall run` runs a named Scenario and writes an Evidence Bundle.

```sh
maya-stall run smoke
maya-stall run --json smoke
maya-stall run --host-config ci-hosts.yaml --target-profile ci smoke
maya-stall run --host-config ci-hosts.yaml --target-profile ci --host maya-win-01 smoke
maya-stall run --host-lock-wait 30s smoke
maya-stall run --host-lock-fail-fast smoke
maya-stall run --keep-on-failure smoke
maya-stall run --keep-ttl 2h --keep-on-failure smoke
maya-stall run --stop-after success smoke
maya-stall run --stop-after failure smoke
maya-stall run --stop-after always smoke
maya-stall run --stop-after never smoke
maya-stall run --control-plane https://maya-stall.example.com smoke
```

## Operating Mode

Omitting `--control-plane` selects Embedded Mode and preserves the existing
local lifecycle. `--control-plane <origin-only-https-url>` selects Configured
Control Plane Mode without changing Repo Run Config. The CLI submits the
selected Scenario, Target Profile, Stop Policy, Repo Run Config, and a safe
snapshot of declared Run Payload paths. The Control Plane allocates the Run ID
before validation and owns durable state, execution, Evidence, and cleanup.

Authentication uses a bearer token from
`MAYA_STALL_CONTROL_PLANE_TOKEN`. `--control-plane-token-env <name>` selects a
different environment variable; token values are not accepted on the command
line or in Repo Run Config. Configured mode owns Maya Host selection and rejects
client host, host-config, pin, and lock flags instead of falling back to local
ownership.

The submitting command remains attached for acceptance and terminal output when
its connection stays open. Once accepted, however, the Control Plane owns the
run independently of that connection. A later authenticated client can attach
by Run ID and inclusive event sequence while an in-process fake or registered
Windows Host Agent Scenario is running. Configured stop can cancel a queued Run
or explicitly release a Kept Session. Active run-scoped desktop mutations
remain unavailable after assignment.

When registered Agents exist, the Control Plane matches the normalized Scenario
requirements against fresh Agent capability reports before assignment, Host
Lock acquisition, or Agent-side payload staging. A no-compatible-Host result is
a durable `host-selection` failure that explains every candidate mismatch. If
at least one Host is compatible but all compatible Hosts are busy, the Run
enters the durable queue instead. Queue order is acceptance time then Run ID;
status exposes its current position, Host Pool, wait reason, and required
capabilities. Compatibility is rechecked before every assignment.

## Behavior

Syntax errors that do not identify a Scenario are usage errors: they exit `2`
and create no run. Once a Scenario is identified, Maya Stall accepts the
submission, emits its Run ID, and records its first event before Repo Run Config
validation, host selection, or remote checks. A later failure exits `1` and
still writes a minimal Evidence Bundle with a versioned manifest, ordered
events, failed layer, diagnostic, remediation hint, capture state, and cleanup
state.

Every accepted Run ID also receives a durable Run Ledger record before Scenario
execution. Embedded Mode stores it in the checkout; Configured Control Plane
Mode stores it in the server-owned run workspace. The record survives transient Run State cleanup and
is updated to `queued`, `canceled`, `completed`, `failed`, `kept`, or
`cleanup-failed` with bounded
ordered events and retained logs.

Live and historical configured reads use the same durable event sequence as
identity. Events, logs, history, and each streaming connection are bounded and
report explicit truncation metadata. Completed Run IDs remain queryable for
history, events, logs, result, Evidence metadata, and cleanup state.

Use `--json` for stable newline-delimited JSON. Accepted submissions emit an
immediate `run-accepted` record and a terminal `run` record. A usage error emits
one `usage-error` record. Runs that proceed use the same Run ID in Run State,
Evidence Bundles, output, and follow-up commands.

The command calls the Fresh Run lifecycle, which owns this accept, setup,
execute, and settle flow:

1. Accept the identified Scenario submission and create its Run ID and Run State.
2. Load Repo Run Config.
3. Select and normalize the named Scenario.
4. Resolve Target Profile, Host Pool, and Maya Host from host config if
   provided.
5. Resolve the Host/Broker runtime contract.
6. Opportunistically expire overdue Kept Sessions recorded for each candidate Maya Host, then acquire a Host Lock.
7. Run bounded live SSH and Session Broker status probes; release the Host Lock and fail at the named layer if either is unavailable.
8. Ask the Session Broker to stop any inherited Maya UI Session and start a new identified Maya UI Session.
9. Stage only declared Run Payload paths.
10. Provide `MAYA_STALL_SCENARIO_RESULT` to the Scenario.
11. Run through the resolved fake-local or ssh-sessiond runtime.
12. Collect outputs, logs, runtime metadata, broker session identity, Scenario Result, and Visual Evidence into an
   Evidence Bundle.
13. Run Validators.
14. Apply the Stop Policy to that Maya UI Session and release or retain the Host Lock.

Configured Agent runs record active Run progress and Agent heartbeats against
the durable Host Lock. Its idle deadline defaults to 30 minutes and its hard
lifetime to 6 hours under server policy. If the submitting CLI or Agent
disappears, later Control Plane or Agent contact enforces those deadlines and
routes exact-session cleanup through a replacement Agent. Retaining Stop
Policies add a keep deadline under the same hard cap; use
[`extend`](extend.md) for an explicit extension.

Supported runtime profiles:

- `fake-local`: fake Maya Host with the fake Session Broker.
- `ssh-sessiond`: SSH Windows Maya Host with `broker.type: gg-mayasessiond`.

An SSH Maya Host without structured `gg_mayasessiond` broker config fails before
payload staging. SSH Host plus fake broker, fake Host plus real broker, and
malformed broker config are not silently downgraded.

After Run ID creation and Host Lock acquisition, but before Run Payload staging,
`run` performs one bounded SSH reachability check and one bounded Session Broker
status check. Each layer has a 10-second ceiling. Failure releases the Host Lock,
leaves no remote staging residue, preserves minimal local evidence, and reports
the failing layer plus its setup-guide repair link. A canonical stopped broker
is accepted because the Fresh Run lifecycle restarts it with a new owned Maya UI
Session.

With `broker.type: gg-mayasessiond`, `run` stages declared payloads under
`workRoot/runs/<run-id>/`, writes a small Scenario wrapper into the remote
workspace, executes it through `gg_maya_sessiond.cli call ... script.execute`,
downloads declared outputs, and captures configured Visual Evidence from the
interactive Windows desktop session.
Before staging payloads, `run` checks the broker status for commandPort
readiness. If the commandPort layer is unhealthy, it restarts the configured
interactive recovery task (`broker.recoveryTask`, default
`MayaStallSessiondUI`) and retries the status preflight; if recovery is
unavailable or still unhealthy, the command fails at the `session-broker`
preflight layer instead of starting a Scenario.
If the selected Maya Host config sets `trustedPluginArtifactsRoot`, `run` also
copies declared `pluginArtifacts` to that stable host-managed root and exposes
it to Scenario scripts as `MAYA_STALL_TRUSTED_PLUGIN_ARTIFACTS_ROOT`.
Before staging Plugin Artifacts, `run` validates that Maya's durable
`SafeModeAllowedlistPaths` preference for the selected Maya version contains
the configured root, declared Plugin Artifact destinations, and parent
directories for nested `.mll` and `.py` files under directory artifacts. Python
files are treated conservatively because Maya plug-in callbacks can be
published dynamically. A missing baseline fails before upload or Scenario execution, with
an actionable TrustCenter diagnostic instead of hanging behind Maya's
untrusted plug-in modal.
The normal per-run payload copy still happens.
Remote Scenario execution through `script.execute` is capped at 10 minutes.
If `script.execute` or equivalent broker execution fails after the remote
workspace exists, `run` first attempts best-effort collection of declared
outputs, logs, screenshots, and any available Scenario Result JSON into the
local Evidence Bundle. If the collected Scenario Result is valid, explicitly
`passed`, and configured Validators pass against the collected outputs, `run`
accepts the Scenario as completed and exits 0 even though the broker return path
failed. Missing, malformed, failed, incomplete, or Validator-failing collected
results remain failed and exit non-zero.
When Scenario screenshot evidence is enabled and the selected broker supports
screenshot Visual Evidence, `run` also best-effort captures
`screenshots/failure-desktop.png` for unrecovered failures.
`manifest.json` and `evidence.json` record the resolved runtime profile, host
adapter, broker adapter, broker config source, and live-proof eligibility.
Every Visual Evidence artifact in `evidence.json` records Visual Evidence
Provenance: an `origin` (`broker-capture`, `fake-broker-capture`, or
`discovered`) plus a `sha256` content hash, and Session Broker captures append
`*.capture-requested` and `*.captured` provenance events to `events.jsonl`.
Live-proof-eligible runs fail closed if the Evidence Bundle would contain
Visual Evidence that was not captured through the Session Broker.
Scenario normalization owns Run Payload paths, Expected Outputs, evidence
policy, and Validator config, so local run validation, Doctor, SSH output
downloads, and Evidence Bundle output discovery use the same paths.

## Run Workspace Layout

For each run, Maya Stall derives local and remote paths from one Run Workspace:

- local state: `.maya-stall/state/runs/<run-id>/`
- embedded Run Ledger: `.maya-stall/state/ledger/runs/<run-id>/`
- local staged payload mirror: `.maya-stall/state/runs/<run-id>/payload/`
- local workspace: `.maya-stall/state/runs/<run-id>/workspace/`
- local Evidence Bundle: `artifacts/maya-stall/<run-id>/`
- remote run root: `workRoot/runs/<run-id>/`
- remote staged payloads: `workRoot/runs/<run-id>/payload/`
- remote workspace: `workRoot/runs/<run-id>/workspace/`
- remote Scenario Result: `workRoot/runs/<run-id>/workspace/<expectedOutputs.scenarioResult>`
- optional trusted Plugin Artifact root: `trustedPluginArtifactsRoot/<repo-relative-plugin-path>`

## Trusted Plugin Artifacts

Autodesk Maya secure plug-in loading is location-based. Trusting each fresh
`workRoot/runs/<run-id>` path can trigger a new prompt every run, while trusting
all of `workRoot` or `workRoot/runs` would also trust arbitrary run payloads.

Use this split instead:

- Consuming repo: declare Plugin Artifacts in `payload.pluginArtifacts` and
  keep Scenario scripts responsible for loading and asserting the plug-in.
- Operator/host config: set `trustedPluginArtifactsRoot` to a stable directory
  using an absolute Windows drive or UNC path that is not inside or above
  `workRoot/runs`, then trust that root plus the declared destination and
  nested plug-in parent directories reported by `doctor --scenario <scenario>`
  for the Windows account that runs the interactive Maya UI.
- Scenario script: when `MAYA_STALL_TRUSTED_PLUGIN_ARTIFACTS_ROOT` is present,
  load the declared plug-in from that root using the same repo-relative path;
  otherwise load from the per-run `payload/pluginArtifacts` path.

Example Scenario-side path selection:

```python
import os
from pathlib import Path

trusted_root = os.environ.get("MAYA_STALL_TRUSTED_PLUGIN_ARTIFACTS_ROOT")
if trusted_root:
    plugin_path = Path(trusted_root) / "build" / "demo.mll"
else:
    plugin_path = Path.cwd().parent / "payload" / "pluginArtifacts" / "build" / "demo.mll"
```

Maya Stall removes each declared destination in the trusted root before copying
the current Plugin Artifact, so directory artifacts do not retain stale files.
It adds trusted locations to Maya preferences only through the explicit
`maya-stall doctor --scenario <scenario> --repair-trusted-plugin-allowlist`
operator action; ordinary run paths validate but do not mutate host security
policy.

Default output:

```text
artifacts/maya-stall/<run-id>/
```

## Host Locking

One active Fresh Run may use a Maya Host at a time. Real SSH hosts store the
authoritative lock at `workRoot/state/locks/host.lock`, so independent
controllers and repo checkouts contend on the same host-owned state. Each
active lock binds a unique token to its Run ID and renews a bounded lease while
the controller is alive. A kept lock has no active-controller lease, but its Run
Record has a 90-minute keep deadline by default. It remains locked until
`maya-stall stop <run-id>` verifies ownership and stops it or a later `run` or
`doctor` contact with that Maya Host expires it through the same broker path.

Configured Control Plane Host Locks additionally persist a 30-minute idle
deadline and 6-hour hard lifetime by default. Active progress and Agent
heartbeats refresh only the idle deadline. Any Control Plane request enforces
an elapsed deadline and directs the current or replacement Agent to
exact-session cleanup. A configured Kept Session can be extended within the hard cap
only through explicit authenticated `maya-stall extend --by <duration>`.

An expired lease is recoverable only after the configured Session Broker proves
that no Maya UI Session is active. Unreadable ownership, a live lease, or an
unavailable/active broker fails closed. The repo-local lock remains as a mirror
for local commands and migration, but it is not the authority for an SSH host.

Use `--host-lock-wait <duration>` to wait for a busy host or
`--host-lock-fail-fast` to fail immediately.

## Stop Policy

Fresh Runs stop their identified broker-owned Maya UI Session and clean hidden
run state plus the remote run workspace by default after writing the Evidence
Bundle. Use `--keep-on-failure` to retain a
failed Session Broker-backed Maya UI Session for debugging.

Explicit `--stop-after` values are:

- `success`: stop after successful runs.
- `failure`: stop after failed runs.
- `always`: always stop.
- `never`: keep the session until `maya-stall stop`.

Kept sessions write a hidden Run Record under `.maya-stall/state/runs/<run-id>/`
with the local paths, remote workspace, broker adapter, broker capabilities, and
remote session metadata needed by `status`, `attach`, and `stop`. The record also
stores `keepTTL` and an RFC3339Nano UTC `keepDeadline`. The built-in TTL is 90
minutes; `--keep-ttl <duration>` overrides it for that run. No Repo Run Config
default exists because the current config has no Stop Policy/kept-session
defaults section. Embedded mode has a fixed 6-hour Kept Session retention limit
and rejects a longer `--keep-ttl`; configured mode validates the requested TTL
against the Control Plane's active Host Lock `--host-lock-hard-lifetime` policy.

Kept sessions are visible through truth-seeking `status`, readable through
read-only `attach`, and cleaned with broker-backed `stop`. `run` sweeps only
Maya Stall Run Records for the candidate host before Host Lock acquisition. It
grace-stamps legacy records without a deadline on first contact, stops overdue
records through the explicit retained-session broker path, and reports cleanup
failures without failing an otherwise viable successor run.

Accepted runs remain discoverable with `maya-stall history` while their ledger
records are retained. Completed and failed records expire after the configured
ledger retention window (30 days by default); unresolved records do not expire.
Ledger retention is separate from Host/session cleanup and Evidence Bundle
retention.
