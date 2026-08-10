# Source Map

Read this when:

- checking whether docs still match implementation;
- changing behavior documented in more than one place;
- writing a release note from source instead of memory.

This page maps user-facing behavior to implementation files. Docs are
descriptive; code is the source-backed check when behavior claims disagree.

## CLI Surface

- Entrypoint: `cmd/maya-stall/main.go`
- Command dispatch, help text, and command output: `internal/cli/cli.go`
- Version command: `internal/cli/cli.go`
- Usage errors and command tests: `internal/cli/cli_test.go`

## Config And Scenarios

- Repo config loading, Scenario parsing, payload model, validators, Target
  Profiles, Host Pools, and host config fields: `internal/cli/config.go`
- `maya-stall init` sample config generation: `internal/cli/cli.go`,
  `internal/cli/config.go`
- Scenario Result model and optional Python helper behavior:
  `helpers/python/maya_stall/__init__.py`,
  `helpers/python/tests/test_maya_stall_result.py`
- Runtime Input normalization, binding, path safety, hashing, immutable
  snapshots, and Scenario environment mapping: `internal/cli/runtime_inputs.go`,
  `internal/cli/scenario.go`

## Host Selection And Transport

- Host capability records, Scenario requirements, shared compatibility reasons,
  and deterministic Agent candidate ranking: `internal/cli/capabilities.go`,
  `internal/cli/capabilities_test.go`
- Host Pool selection and Host Locks: `internal/cli/host_pool.go`
- Real SSH and SFTP transport, PowerShell wrapping, upload/download behavior,
  and fixed SSH/SFTP command timeouts: `internal/cli/ssh_transport.go`
- Local Windows filesystem transport and direct desktop integration:
  `internal/cli/local_windows_host.go`,
  `internal/cli/windows_desktop_capture.go`
- Windows Maya Host prepare script and fake/check-only fixture tests:
  `scripts/windows/prepare-maya-host.ps1`,
  `scripts/windows/prepare-maya-host.test.mjs`
- `gg_mayasessiond` Session Broker adapter, `script.execute` wrapper behavior,
  interactive session checks, and desktop Visual Evidence:
  `internal/cli/sessiond_broker.go`
- Platform process helpers: `internal/cli/process_posix.go`,
  `internal/cli/process_windows.go`
- Opt-in live SSH doctor and run smoke tests:
  `internal/cli/live_ssh_smoke_test.go`

## Run Lifecycle

- Fresh Run lifecycle interface and setup, execute, settle ordering:
  `internal/cli/fresh_run.go`
- Fresh Run data model, Stop Policy, payload staging, Scenario Result
  collection, Validator execution, and Evidence Bundle layout:
  `internal/cli/run.go`
- Kept run state, `status`, `attach`, and `stop`: `internal/cli/run_state.go`
- Embedded Run Ledger, `history`, retention, bounded events/logs, and terminal
  status/attach: `internal/cli/run_ledger.go`,
  `internal/cli/run_ledger_store.go`,
  `internal/cli/run_ledger_test.go`
- Run-scoped attach screenshot/control helpers:
  `internal/cli/run_scoped_ops.go`
- Crabbox-inspired defaults: `internal/cli/crabbox_defaults.go`
- Embedded/Configured Control Plane mode selection, authenticated API,
  detached submissions, sequenced bounded event streams, durable run reads,
  and Control Plane server:
  `internal/cli/control_plane.go`, `internal/cli/control_plane_test.go`
- Windows Host Agent enrollment, outbound assignment polling, bounded active
  Run Ledger checkpoints, durable Host Lock and Maya UI Session fencing,
  fake/real execution, result transfer, and restart recovery:
  `internal/cli/host_agent.go`,
  `internal/cli/host_agent_transition_store.go`,
  `internal/cli/host_agent_test.go`
- Durable compatible-Run queueing, FIFO scheduling, queue status, cancellation,
  and restart recovery: `internal/cli/run_queue.go`,
  `internal/cli/run_queue_test.go`
- Protected shared-path real Maya proof:
  `internal/cli/live_shared_agent_smoke_test.go`

## Doctor

- Local checks, Target Profile checks, Host Health layers, fake diagnostic
  fields, real SSH checks, and repair hints: `internal/cli/doctor.go`
- Windows Maya Host setup guidance: `docs/setup/windows-maya-host.md`

## Visual Evidence And Publishing

- Standalone screenshot/record commands and Visual Evidence planning:
  `internal/cli/visual_evidence.go`,
  `internal/cli/windows_desktop_capture.go`
- Evidence Bundle publishing, filesystem Evidence Store copying, URL generation,
  artifact manifest, and review comment markdown: `internal/cli/publish.go`
- GitHub and GitLab Review Comment rendering, marker handling, platform clients,
  dry-run behavior, and token env handling: `internal/cli/review_comment.go`
- Review comment tests: `internal/cli/review_comment_test.go`

## Docs And Decisions

- Domain language: `CONTEXT.md`
- V1 product shape: `docs/prd/0001-maya-stall-v1.md`
- Architectural decisions: `docs/adr/`
- Agent routing docs: `docs/agents/`
- Public host setup checklist: `docs/setup/windows-maya-host.md`

## PR Proof

- Changed-path live Maya proof policy: `proof/live-maya-policy.json`
- Proof Manifest selector and fail-closed live assertion:
  `scripts/proof/select-proof.mjs`, `scripts/proof/assert-live-proof.mjs`
- Restricted hosted workflow: `.github/workflows/ci-hosted.yml`
- Trusted classification, live proof, and required-result workflow: `.github/workflows/ci-required.yml`
- CI topology, timing method, and measured baseline: `docs/agents/pr-merge.md`,
  `docs/ci-performance.md`
- PR closeout and merge proof docs: `docs/agents/pr-merge.md`
