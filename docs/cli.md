# CLI

This is the command-surface overview for the `maya-stall` binary. It maps the
command tree, shared flags, config files, output rules, and exit behavior.

For per-command examples, see [commands/](commands/README.md). For vocabulary,
see [Concepts and glossary](concepts.md).

## Name

`maya-stall` - run Autodesk Maya UI Scenarios from repo-owned config and collect
review-ready evidence.

## Usage

```text
maya-stall [--help]
maya-stall version
maya-stall <command> [args]
```

Primary results go to stdout. Progress, diagnostics, and errors go to stderr.
Commands that mutate external systems, such as Review Comment publishing, also
support dry-run paths where available.

## Command Map

### Setup And Diagnostics

```text
maya-stall version
maya-stall init
maya-stall doctor [--host-config <path>] [--target-profile <name>] [--host <id>] [--scenario <name>] [--repair-trusted-plugin-allowlist]
maya-stall plan [--json] [--host-config <path>] <scenario>
```

See [version](commands/version.md), [init](commands/init.md),
[doctor](commands/doctor.md), and [plan](commands/plan.md).

### Run Lifecycle

```text
maya-stall run [--json] [control-plane flags] [host flags] [lock flags] [stop flags] <scenario>
maya-stall history [--json] [control-plane flags] [--before-run <run-id>] [--scenario <name>] [--host <id>] [--state <state>] [--since <duration-or-rfc3339>]
maya-stall status [--json] [control-plane flags] --run <run-id>
maya-stall events [--json] [control-plane flags] [--from-sequence <number>] <run-id>
maya-stall logs [--json] [control-plane flags] <run-id>
maya-stall result [--json] [control-plane flags] <run-id>
maya-stall control-plane serve --data-dir <path> --tls-cert <path> --tls-key <path> [--host-lock-idle-timeout <duration>] [--host-lock-hard-lifetime <duration>]
maya-stall control-plane enroll-agent --control-plane <url> --agent-id <id> --host <id> --credential-env <name>
maya-stall host-agent run-once --control-plane <url> --agent-id <id> --host <id> --work-root <path> [--host-config <path>] --credential-env <name>
maya-stall attach <run-id> [control-plane flags] [--from-sequence <number>]
maya-stall attach <run-id> screenshot
maya-stall attach <run-id> control click --x <pixels> --y <pixels>
maya-stall stop [--control-plane <https-url>] [--control-plane-token-env <name>] <run-id>
maya-stall extend [--control-plane <https-url>] [--control-plane-token-env <name>] --by <duration> <run-id>
```

Control Plane flags:

```text
--control-plane <origin-only-https-url>
--control-plane-token-env <environment-variable-name>
```

Omitting `--control-plane` selects Embedded Mode. Providing it selects
Configured Control Plane Mode without changing Repo Run Config. The default
token environment variable is `MAYA_STALL_CONTROL_PLANE_TOKEN`; token values
are never command arguments or config fields. Configured mode owns Maya Host
selection, so client host, lock, and pin flags are rejected. Target Profile and
Stop Policy remain part of the submitted Scenario request.

Common host flags:

```text
--host-config <path>
--target-profile <name>
--host <id>
```

Lock and stop flags:

```text
--host-lock-wait <duration>
--host-lock-fail-fast
--keep-ttl <duration>
--keep-on-failure
--stop-after success|failure|always|never
```

Kept Sessions default to a 90-minute TTL. `--keep-ttl` sets a positive Go
duration for the submitted run and is preserved in Configured Control Plane
Mode. The current Repo Run Config has no Stop Policy/kept-session default;
omitting the flag uses the built-in value.

See [run](commands/run.md), [history](commands/history.md),
[status](commands/status.md), [events](commands/events.md),
[logs](commands/logs.md), [result](commands/result.md),
[control-plane](commands/control-plane.md), [host-agent](commands/host-agent.md),
[attach](commands/attach.md), [stop](commands/stop.md), and
[extend](commands/extend.md).

An identified Scenario submission receives a Run ID before validation, host
selection, or remote checks. `--json` emits newline-delimited acceptance and
terminal records; syntax that never identifies a Scenario emits one usage-error
record and creates no run.

Accepted runs are retained in the embedded or Control Plane Run Ledger after
transient state cleanup. `history --json` returns a stable versioned object and supports exact
Scenario, Maya Host, state, and recent-time filters.

`status`, `events`, `logs`, and `result` render the same versioned response
contracts in Embedded and Configured Control Plane modes. Configured reads use
the Run ID returned by submission. The Control Plane persists its own Run
Ledger and Evidence. When an Agent is enrolled, it routes each Scenario through
one registered outbound Windows Host Agent and a durable shared Host Lock. An
Agent-local `--host-config` selects real Maya execution; omitting it selects the
explicit fake development path. Without an enrollment the Control Plane retains
the in-process fake path.

Configured Host Locks default to a 30-minute idle timeout and 6-hour hard
lifetime. Active Run progress and Agent heartbeats refresh the idle deadline.
Kept Sessions remain under the same hard cap; `extend --by <duration>` is the
explicit authenticated action for moving their keep deadline within policy.

After acceptance, a configured run survives submitter disconnect. Configured
`attach` follows from an inclusive durable sequence cursor; live and historical
reads preserve identical event identities and report retention or per-stream
truncation explicitly.

### Visual Evidence

```text
maya-stall screenshot [host flags]
maya-stall record [host flags]
maya-stall control click --x <pixels> --y <pixels> [host flags] [--dry-run]
```

See [screenshot](commands/screenshot.md), [record](commands/record.md), and
[control](commands/control.md).
Scenarios can also enable screenshot and recording Visual Evidence in repo
config; `maya-stall run` and `maya-stall evidence collect` write those artifacts
into the Scenario Evidence Bundle.

When a Fresh Run or Kept Session already owns a Host Lock, use the run-scoped
`attach <run-id> screenshot` and `attach <run-id> control click` forms. They
verify the current Host Lock owner before touching the desktop.

### Evidence And Review Publishing

```text
maya-stall evidence collect [--json] [control-plane flags] [host flags] <scenario>
maya-stall evidence publish --destination <path> --base-url <url> <evidence-bundle-dir>
maya-stall review-comment github --repo <owner/name> --pr <number> [--token-env <name>] [--api-url <url>] [--dry-run] <published-evidence-dir>
maya-stall review-comment gitlab --project <path-or-id> --merge-request <iid> [--token-env <name>] [--base-url <url>] [--dry-run] <published-evidence-dir>
```

See [evidence](commands/evidence.md) and
[review-comment](commands/review-comment.md).

## Config Files

Repo config is YAML and must be non-secret:

```text
.maya-stall.yaml
maya-stall.yaml
```

Repo config contains Scenarios, Run Payload paths, Expected Outputs, Visual
Evidence policy, and Validators. It must not contain Host Credentials, private
hostnames, SSH keys, Windows users, license details, Host Pools, or Evidence
Store secrets.

Optional embedded Run Ledger policy also belongs in Repo Run Config:

```yaml
runLedger:
  retention: 720h
  maxEvents: 10000
  maxEventBytes: 8388608
  maxLogBytes: 1048576
```

Defaults are 30 days, 10,000 events, 8 MiB of retained event data, and 1 MiB
of retained log data per run. `retention` must be a positive Go duration.
`maxEvents` accepts 3 through 100,000, `maxEventBytes` accepts 1 KiB through
64 MiB, and `maxLogBytes` accepts 96 bytes through 64 MiB.
Automatic retention removes only expired `completed` and `failed` ledger
records. It does not delete Evidence Bundles or expire unresolved records.

Host config is passed explicitly:

```sh
maya-stall run --host-config ci-hosts.yaml --target-profile ci smoke
```

Host config may contain Target Profiles, Host Pools, fake diagnostic fields, or
opt-in real SSH transport. Keep it in user config, CI secrets, or runner-owned
files rather than in the consuming repo.
For real Maya plug-in runs, host config may also set
`trustedPluginArtifactsRoot` to a stable Maya Host directory that the operator
has added to Maya's trusted plug-in locations. `maya-stall doctor` validates
that Maya's durable `SafeModeAllowedlistPaths` preference contains that root,
Scenario-declared Plugin Artifact destinations, and parent directories for
nested `.mll` and `.py` files under directory artifacts. Python files are
treated conservatively because Maya plug-in callbacks can be published dynamically.
`maya-stall run` fails before staging Plugin Artifacts when the baseline is
missing. `maya-stall run` copies only declared `pluginArtifacts` there and
exposes the root as `MAYA_STALL_TRUSTED_PLUGIN_ARTIFACTS_ROOT`.

## Exit Behavior

`maya-stall` returns `0` on success and non-zero on usage errors, failed checks,
failed Scenarios, transport errors, validator failures, or publishing failures.

Scripts should branch on success versus non-zero and inspect stderr plus the
Evidence Bundle for details. Scenario failures are also recorded in
`evidence.json`.

## Output Artifacts

Default run artifacts are written under:

```text
artifacts/maya-stall/<run-id>/
```

Internal run state and Host Locks live under hidden repo-local state:

```text
.maya-stall/state/
```

The durable embedded ledger is stored below
`.maya-stall/state/ledger/runs/<run-id>/`; transient operational state remains
under `.maya-stall/state/runs/<run-id>/`.

Published bundles contain:

```text
artifact-manifest.json
review-comment.md
```
