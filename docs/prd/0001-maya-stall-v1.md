# Maya Stall V1 PRD

## Problem Statement

Maya plugin repositories need reliable real Autodesk Maya UI end-to-end testing on owned Windows machines. Existing generic CI and batch/headless Maya checks do not prove that a plugin works in an interactive Maya desktop, and failures are hard to debug without screenshots, logs, scenes, and structured output.

Crabbox already proves useful patterns for remote execution, static SSH, stop policy, doctor checks, visual evidence, artifacts, and publishing, but Maya Stall needs a Maya-specific product boundary: owned Maya Hosts, `gg_mayasessiond`, typed Run Payloads, Scenario Results, Maya version compatibility, and review-ready Evidence Bundles.

## Solution

Build `maya-stall` as a Go CLI for running typed Maya Scenarios on owned Windows Maya Hosts over SSH. The tool stages declared payload paths into a clean per-run workspace, asks the Session Broker to launch or attach to a real Maya UI Session, runs repo-owned Maya Scripts, collects Visual Evidence and outputs into an Evidence Bundle, validates generic results, and publishes review-ready evidence to GitHub or GitLab through a filesystem Evidence Store.

Maya Stall uses Crabbox as a reference and may selectively vendor MIT-licensed Crabbox code, but it is not a Crabbox fork and does not depend on Crabbox at runtime.

## User Stories

1. As a plugin maintainer, I want to run a named Maya Scenario from my repo, so that I can prove the plugin works in real Maya UI.

2. As a CI maintainer, I want Maya Stall to use owned Windows Maya Hosts over SSH, so that I do not pay for cloud compute or inference-like rented capacity.

3. As a repo owner, I want `.maya-stall.yaml` to contain only non-secret Scenario and payload configuration, so that private hostnames, SSH keys, and credentials stay outside the repo.

4. As a developer, I want to declare Plugin Artifacts, Maya Scripts, scenes, Expected Outputs, and evidence policy, so that each Scenario is explicit and reusable.

5. As a developer, I want a Fresh Run to start from a clean Maya UI Session and clean per-run workspace, so that stale state does not affect test results.

6. As a developer debugging locally, I want `--keep-on-failure` and Debug Attach, so that I can inspect a failed Maya UI Session before cleanup.

7. As a CI operator, I want one active Fresh Run per Maya Host with Host Locks, so that concurrent Maya UI runs do not corrupt each other.

8. As a CI operator, I want Target Profiles to choose from a Host Pool, so that multiple owned machines can provide concurrency across hosts.

9. As a developer, I want layered `doctor` checks, so that failures point to SSH, work root, Session Broker, Maya version, visual capture, Host Lock, or Scenario inputs instead of one vague readiness error.

10. As a reviewer, I want screenshots attached to each run, so that UI failures can be understood without reproducing them.

11. As a reviewer, I want Evidence Bundles published through Review Comments on GitHub PRs or GitLab MRs, so that visual proof is part of the normal code review workflow.

12. As a studio operator, I want Evidence Bundles copied to a filesystem or network Evidence Store with a configured base URL, so that artifacts live on owned infrastructure.

13. As a Scenario author, I want to write a structured Scenario Result JSON file, so that assertions, measurements, plugin load details, and produced outputs are machine-readable.

14. As a Scenario author, I want an optional tiny Maya Python helper, so that writing Scenario Results is less repetitive.

15. As a maintainer, I want generic Validators for files, JSON values, numeric arrays, hashes, and visual evidence presence, so that common checks are reusable without putting plugin-domain logic into Maya Stall.

16. As a maintainer, I want `maya-stall init`, so that new consuming repos can generate a safe repo-only `.maya-stall.yaml` example.

17. As a host admin, I want a Windows host setup checklist, so that OpenSSH, interactive desktop, Autodesk Maya, `gg_mayasessiond`, work roots, visual capture, and Evidence Store access are prepared consistently.

18. As a maintainer, I want the default test suite to run without real Maya, private hosts, or secrets, so that the project remains easy to develop.

## Implementation Decisions

- Build a Go CLI named `maya-stall`; the repository is `maya_stall`.

- Use `.maya-stall.yaml` and `maya-stall.yaml` as repo config filenames.

- Keep Repo Run Config non-secret. Host Pools, Host Credentials, SSH keys, broker endpoints, Windows credentials, and license-related values live in user config, CI variables, or runner credentials.

- Target Windows Maya Hosts in v1. Keep terms such as Maya Host and Target Profile future-neutral, but do not implement macOS or Linux Maya Hosts yet.

- Require an interactive logged-in Windows desktop. Service-only or raw SSH-only Maya UI runs are not supported in v1.

- Treat Maya launched in Windows Services session `0` as unhealthy for UI runs, even when MCP calls or viewport capture appear to work.

- Diagnose host prerequisites instead of installing them. Host setup remains a runbook/manual step in v1.

- Define a Session Broker interface and implement `gg_mayasessiond` as the only v1 broker.

- Capture Visual Evidence through the Session Broker. The broker may use Crabbox-style Windows helpers such as interactive scheduled tasks internally. Recordings use desktop frame capture with local MP4 encoding.

- Use typed Run Payloads, not generic folder-plus-command execution.

- Stage Plugin Artifacts into a predictable run workspace, but let Scenario Maya Scripts load plugins and assert load success.

- Consume prebuilt Plugin Artifacts. Compilation and packaging belong to the consuming repo's CI or local build.

- Sync only Scenario-declared payload paths, including ignored build outputs when explicitly declared.

- Use clean per-run remote workspaces for Fresh Runs.

- Support multiple named Scenarios per repo config.

- Let Scenarios declare Maya Version Requirements; Host Health verifies compatibility.

- Use Scenario Result JSON as the structured result contract.

- Ship an optional `maya_stall` Python helper for Maya scripts.

- Keep v1 Validators generic: Scenario Result status, required output existence, JSON path equality, numeric array approximate equality, file hashes, and Visual Evidence presence.

- Use Crabbox-style Stop Policy: default cleanup after Fresh Runs, `--keep-on-failure`, explicit stop-after behavior, and visible Kept Sessions.

- Serialize Fresh Runs per Maya Host with Host Locks.

- Let Target Profiles reference Host Pools. Select the first healthy unlocked host, allow host pinning, and support wait or fail-fast behavior.

- Use layered Host Health checks and ship `doctor` as a core v1 command.

- Ship a compact command surface: `init`, `doctor`, `run`, `status`, `stop`, `attach`, `screenshot`, `evidence collect`, and `evidence publish`.

- Use `attach` for run/session event and log following, not UI viewing. Keep UI viewer out of v1.

- Store hidden internal run state separately from user-facing Evidence Bundles.

- Publish Evidence Bundles in v1. Keep collection and publishing separate.

- Support GitHub and GitLab Review Comments through thin adapters over shared markdown rendering.

- Support a filesystem Evidence Store in v1: copy bundles to local or network paths and generate URLs from `baseUrl`.

- Leave S3/R2/MinIO and brokered upload backends for later unless a first consumer needs them immediately.

- Use Crabbox-like timeout defaults where they map: kept-session TTL around 90 minutes, idle timeout around 30 minutes, and screenshot settle around 2 seconds. Define Maya launch and Scenario timeouts separately.

- Include a Windows Maya Host setup document.

- Keep examples generic. Do not bake `gg_klv_push` behavior into Maya Stall.

- Selectively copy or vendor useful MIT-licensed Crabbox code with attribution. Do not fork Crabbox wholesale.

## Testing Decisions

- Default tests must not require Autodesk Maya, private hosts, SSH secrets, GitHub/GitLab credentials, or an Evidence Store.

- Test config parsing, config precedence, and schema validation with local fixtures.

- Test Scenario selection and Run Payload manifest building with fake repo layouts.

- Test Host Pool selection, Host Lock behavior, and Host Health layering with fake hosts.

- Test SSH/sync planning without real SSH; use fake transport boundaries for default tests.

- Test the Session Broker adapter against a fake daemon protocol.

- Test run state, Stop Policy, Fresh Run, Debug Attach, and Kept Session behavior with fakes.

- Test Evidence Bundle layout, artifact manifests, screenshot metadata, and missing-evidence failures.

- Test Review Comment markdown rendering once, then test GitHub and GitLab adapters as thin command/API shaping layers.

- Test filesystem Evidence Store publishing with temp directories.

- Test Validators for files, JSON values, numeric arrays, hashes, and visual evidence presence.

- Test `doctor` output by asserting failing layers and repair hints.

- Test `init` output to ensure it writes repo-only config and no host credentials.

- Add opt-in live smoke tests gated by explicit environment/configuration for real SSH Maya Host, `gg_mayasessiond`, screenshot capture, and one smoke Scenario.

- Live smoke tests must assert the Maya process is in the interactive Windows desktop session before accepting screenshot proof.

- PRs that change live product behavior must use a checked-in proof selector and Proof Manifest. Fake-first tests remain required, but they do not satisfy real-product proof; the live Maya proof gate must fail closed when required proof is skipped, missing, or fake-only.

## Out of Scope

- Paid cloud provisioning, brokered cloud leases, and Crabbox coordinator integration.

- Running through the Crabbox binary as a runtime dependency.

- Whole-repo sync.

- Building Plugin Artifacts.

- Maya batch or headless testing.

- macOS or Linux Maya Hosts.

- Built-in UI viewer, VNC portal, or remote desktop control.

- Service-only Windows sessions.

- Installing/configuring host prerequisites from the CLI.

- Plugin-domain validators such as deformer-specific checks.

- Visual diff/image comparison.

- S3/R2/MinIO publishing in the first cut unless needed by the first workflow.

- Publishing issues to GitHub or GitLab from this PRD.

## Further Notes

- `gg_klv_push` is an expected future consuming repo, but its Scenario config and plugin-specific payloads belong in that repo.

- The first implementation should start by scaffolding the Go CLI, config schema, fake broker, Evidence Bundle layout, and doctor output before wiring real Windows/Maya behavior.

- Crabbox source should be mined selectively for implementation patterns and copied modules only where it saves real time without importing the full provider/coordinator surface.
