# PR And Merge Proof

Read when preparing, reviewing, or landing a PR.

## Proof Manifest

Every PR closeout should include a Proof Manifest from `scripts/proof/select-proof.mjs`.
The manifest records changed files, whether live Maya proof is required, why it
was required, and the status of local, docs, artifact, and live Maya gates.

The checked-in policy is `proof/live-maya-policy.json`. If a PR changes live
product behavior, the selector writes `live_maya_required=true`.

Live-required surfaces include:

- Target Profile, Host Pool, host config, SSH, SFTP, or Windows host execution;
- `gg_mayasessiond`, Session Broker, or interactive desktop checks;
- `doctor`, `run`, `stop`, `status`, `attach`, screenshot, or recording behavior;
- Scenario parsing or execution, Fresh Run lifecycle, run retention, and kept sessions;
- Evidence Bundle layout, manifest, logs, Visual Evidence, and Review Comments;
- consuming-repo smoke wiring and docs that change the live proof contract.

## Merge Rule

Fake/local tests are still required, but fake-first tests do not satisfy
real-product proof. For `live_maya_required=true`, the live Maya gate must pass
against a configured real Windows Maya Host. Skipped, missing, or fake-only live
proof is a failure.

The live gate runs desktop Visual Evidence and desktop control proof first,
then the older SSH smokes, the retained run-scoped desktop ops smoke, and the
shared Control Plane/Host Agent real-Maya path as a required subtest of the SSH
run smoke. That path includes a shared Kept Session subtest that extends its
deadline, advances a controlled clock, and proves exact-session expiry cleanup.
The SSH run smoke restores the broker after its retained-stop proof before
entering those shared-path subtests. Live smokes restore the documented
interactive sessiond UI scheduled task before proof starts and retained-stop
smokes restore it again before the next proof step:

```sh
go test -json ./internal/cli -run '^(TestOptInRealVisualEvidenceSmoke|TestOptInRealDesktopControlModalSmoke|TestOptInRealSSHDoctorSmoke|TestOptInRealPreRunReadinessSmoke|TestOptInRealSSHConsumingRepoSmoke|TestOptInRealSSHRunSmoke|TestOptInRealHostLockContentionAndRecoverySmoke|TestOptInRealRunScopedDesktopOpsSmoke)$' -count=1 -parallel=1 -timeout=20m
```

The single Go process compiles and initializes the package once. All eight named
tests must report individual passes; skips fail the live gate. Both shared Agent
subtests must also pass for `TestOptInRealSSHRunSmoke` to pass. `-parallel=1`
keeps the one interactive Windows desktop serialized, while `-timeout=20m`
leaves five minutes of the job budget for setup and proof publication.

That opt-in smoke runs `maya-stall doctor --scenario smoke`, then one real
`maya-stall run smoke` through `gg_mayasessiond`, and asserts the Evidence
Bundle, Scenario Result, logs, manifest, and real Visual Evidence bytes.
It also proves the pre-run readiness boundary by keeping SSH reachable while
pointing at a deliberately absent broker state, then asserting a
`session-broker` failure before staging with the Host Lock released.
It also runs one canonical Consuming Repo Scenario from a checked-out consuming
repo path, then publishes the Evidence Bundle to a temporary filesystem
Evidence Store and verifies review-ready artifact files. The live run smoke
uses a recording-enabled Scenario and validates its Evidence Bundle has a real
MP4 recording with duration/FPS and selected-host metadata. The live Visual
Evidence smoke additionally invokes the standalone `maya-stall record` command,
asserts `maya.exe` is in the interactive Windows `Console` session, and
validates the command's MP4 Evidence Bundle before publishing sanitized review
proof. The run-scoped desktop ops smoke keeps a failed run locked, proves
standalone screenshot fails closed while the Host Lock is held, captures a
run-scoped desktop screenshot, and clears a controlled modal with
`attach <run-id> control click`.
The shared Host Agent smoke submits without client Host config, waits for the
registered Agent to bind the shared Host Lock to the exact real broker session,
and verifies transferred evidence, inactive broker state, remote and Agent
workspace removal, and both shared and host-side lock release.
The Host Lock smoke runs separate controller processes against the same SSH
Maya Host, proves contention crosses checkout boundaries, leaves one lease
behind as if its controller crashed, proves the Session Broker is inactive,
and then recovers the expired lease through the real SSH/Session Broker path.

Production-ready parallel-execution claims additionally require the opt-in
two-Host smoke against a Target Profile whose Host Pool exposes at least two
compatible, isolated, live-proof-eligible Maya Hosts. It proves two bound Host
Locks and real Maya UI Sessions overlap, a third Run queues until one Host
frees, and all three Evidence Bundles and cleanup paths remain isolated by Run
ID:

```sh
env -u MAYA_STALL_SMOKE_HOST \
  MAYA_STALL_SMOKE_HOST_CONFIG=/path/to/two-hosts.yaml \
  MAYA_STALL_SMOKE_TARGET_PROFILE=ci \
  go test ./internal/cli -run '^TestOptInRealTwoHostParallelRunsSmoke$' -count=1 -timeout=30m
```

The smoke skips with an explicit reason when the config is absent, the Host
Pool is pinned, or fewer than two compatible live Hosts are configured. It is
not part of the required eight-test live gate because the current protected CI
fixture provides one Windows Maya Host.

Non-live-only changes may merge with local gates plus a manifest saying
`live_maya_required=false`.

## CI Topology

`.github/workflows/ci-hosted.yml` runs candidate code under the restricted
`pull_request` token: four parallel jobs cover race-enabled Go tests, lint,
documentation, and proof-policy/Windows-helper tests. It persists no checkout
credentials, exposes no secrets or self-hosted runner, and disables tool caches.

After that run completes, `.github/workflows/ci-required.yml` runs from its
trusted default-branch copy on `workflow_run`. It verifies the exact hosted job
set (rejecting missing, duplicate, or extra job names), classifies the immutable
PR diff, and owns one ref concurrency group with
`cancel-in-progress: true`. A new push cancels obsolete classification and live
work. Before secrets are decoded and again immediately before the Maya smoke,
the live job asks GitHub whether its exact SHA is still the current PR or `main`
head. A stale SHA fails without touching the Maya Host.

A non-live-sensitive diff never schedules the self-hosted runner. A trusted
live-sensitive diff reaches the serialized `Live Maya Proof` job only after all
hosted gates pass. A live-sensitive fork gets neither secrets nor self-hosted
runner access; `CI / Required` reports action required until a maintainer
promotes the commit to a same-repository ref.

`CI / Required` is the only stable branch-protection status. A dedicated
repository-scoped GitHub App publishes it, so candidate-controlled GitHub
Actions jobs cannot spoof the required identity. It always runs and
accepts a non-live change only after every hosted result succeeds; a live change
also needs successful real-Maya proof. Classification failure, cancellation,
skip, fork promotion, or missing required live proof all fail the aggregate.

## Branch Protection

Required configuration:

- Required status check on `main`: `CI / Required`, pinned to the dedicated CI Required GitHub App.
- `enforce_admins` enabled.
- Force pushes and branch deletion disabled.

The CI Required GitHub App must be installed only on this repository, with
metadata read and Checks read/write permissions, and no active webhook. Store
its app ID as repository variable `CI_REQUIRED_APP_ID` and its private
key as repository secret `CI_REQUIRED_APP_PRIVATE_KEY`. The hosted candidate
workflow receives neither value.

Audit the live repository without printing secrets or configuration values:

```sh
node scripts/proof/audit-main-protection.mjs --repository <owner>/<repo> --app-id <ci-required-app-id>
```

The script reads the existing branch protection and verifies the contract. It
does not mutate or replace review requirements, push restrictions, or other
rules.

## Timing CI

Fetch the linked hosted and trusted runs, then report hosted feedback,
live-runner queue, and live execution separately:

```sh
hosted_run_id=<ci-hosted-run-id>
trusted_run_id=<ci-required-run-id>
gh api "repos/<owner>/<repo>/actions/runs/$hosted_run_id" > /tmp/maya-stall-hosted-run.json
gh api "repos/<owner>/<repo>/actions/runs/$hosted_run_id/jobs?filter=latest" > /tmp/maya-stall-hosted-jobs.json
gh api "repos/<owner>/<repo>/actions/runs/$trusted_run_id/jobs?filter=latest" > /tmp/maya-stall-trusted-jobs.json
node scripts/proof/report-ci-timing.mjs \
  --hosted-input /tmp/maya-stall-hosted-jobs.json \
  --trusted-input /tmp/maya-stall-trusted-jobs.json \
  --hosted-run-created-at "$(jq -r .created_at /tmp/maya-stall-hosted-run.json)"
```

Hosted feedback should remain under 90 seconds. Treat Maya runner queue and live
execution as separate capacity/proof measurements rather than hosted latency.

Policy path coverage is audited automatically: `scripts/proof/audit-live-policy.mjs`
verifies every `proof/live-maya-policy.json` rule path still exists, every prefix
still matches tracked files, and every file under `cmd/`, `internal/`,
`scripts/proof/`, and `scripts/windows/` is covered by some rule. The audit runs
in the `Proof Script Tests` step on every PR; run it locally with
`node scripts/proof/audit-live-policy.mjs`. File drift it reports as follow-up
issues instead of weakening the policy.

## Repository Setup

Automation expects a protected GitHub Environment named `maya-live-proof`.
Configure required reviewers on that environment before adding live host
credentials, because the live job checks out and tests PR code after approval.

Add the live host config as an environment secret named
`MAYA_STALL_LIVE_HOST_CONFIG_B64`, containing base64-encoded host config YAML.
Add the canonical consuming repo checkout path as an environment secret named
`MAYA_STALL_LIVE_CONSUMING_REPO_SMOKE_DIR`. A variable with the same name is
accepted as fallback, but the secret keeps local runner paths masked in logs.
The self-hosted `maya-live-proof` runner must provide `python3 >= 3.10` with
`venv` and `ensurepip`; the live job checks this before running the consuming
repo local gate.
Optional environment or repository variables:

- `MAYA_STALL_LIVE_TARGET_PROFILE`
- `MAYA_STALL_LIVE_HOST`
- `MAYA_STALL_LIVE_PROOF_RETENTION_DAYS`, default `3`, max `14`
- `MAYA_STALL_LIVE_PROOF_PUBLIC_HOST_ALIAS`, default `maya-live-proof-host`

Do not commit hostnames, SSH identities, credentials, generated live evidence,
or local machine paths. Public proof should report command classes, pass/fail
status, manifest status, and redacted live-config state only.

## Live Visual Evidence Artifact

When the live gate passes, the workflow uploads a GitHub Actions artifact named
`live-visual-evidence-proof`. Reviewers can open the exact workflow run, choose
`Artifacts`, download `live-visual-evidence-proof`, and inspect:

- `proof-artifact-manifest.json`
- `evidence-metadata.json`

The proof artifact is generated only after the local Evidence Bundle exists and
only when the live workflow sets
`MAYA_STALL_LIVE_PROOF_ARTIFACT_ENABLED=true`. The protected runner still
captures and validates the real broker-backed PNG and MP4, including hashes and
provenance events, but public artifacts contain metadata only. Desktop pixel
media stays runner-local and is never uploaded. `evidence-metadata.json`
records `mediaPublished: false` plus the verified source hashes; the manifest
lists only the uploaded metadata file with its byte size and SHA256 alongside
run id, Scenario, Target Profile, public-safe selected host alias, and retention
days. The public artifact confidentiality gate rejects unexpected files and
proof text containing private host aliases, user paths, SSH material,
token-like fields, license variables, or symlinks before upload.

S3/R2 publishing is deferred. A future backend should use a separate
`evidence store` config block with bucket endpoint, short lifecycle retention,
scoped write credentials from CI/user secrets only, and signed or otherwise
review-scoped reads. Any future binary-media publication must add a deterministic
pixel-redaction boundary and reuse the same proof-artifact manifest and
confidentiality gate before upload.

For live-required fork PRs, automation withholds live host credentials and fails
closed. A maintainer must review and promote the change to a trusted
same-repository branch or ref before the protected live Maya environment can run.
