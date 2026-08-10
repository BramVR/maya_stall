# evidence

`maya-stall evidence` collects and publishes Evidence Bundles.

## collect

`maya-stall evidence collect` runs a Scenario and writes a complete Evidence
Bundle.

```sh
maya-stall evidence collect smoke
maya-stall evidence collect --json smoke
maya-stall evidence collect --host-config ci-hosts.yaml --target-profile ci smoke
maya-stall evidence collect --host-config ci-hosts.yaml --target-profile ci --host maya-win-01 smoke
maya-stall evidence collect --control-plane https://maya-stall.example.com smoke
```

`--control-plane <origin-only-https-url>` selects Configured Control Plane Mode
without changing Repo Run Config. The bearer token defaults to
`MAYA_STALL_CONTROL_PLANE_TOKEN`; `--control-plane-token-env <name>` selects a
different environment variable. The first configured path executes fake
Scenarios only and returns API evidence by Run ID. Omitting the flag preserves
Embedded Mode.

The bundle includes:

- `evidence.json`
- `manifest.json`
- Scenario Result JSON
- events and logs
- Visual Evidence artifacts
- declared output files

Validator failures are recorded in `evidence.json` and mark the run failed.
Like `maya-stall run`, collection accepts an identified Scenario before config,
host, or remote validation and preserves early failures as minimal Evidence
Bundles. `--json` emits the same `run-accepted`, terminal `run`, and
pre-acceptance `usage-error` record shapes.

Every Visual Evidence artifact carries Visual Evidence Provenance in
`evidence.json`: an `origin` value plus a `sha256` content hash computed at
capture or collection time. Origin values are:

- `broker-capture`: captured through a real Session Broker.
- `fake-broker-capture`: captured by the fake Session Broker.
- `discovered`: a file found under `screenshots/` or `recordings/` that was not
  registered by a Session Broker capture.

Session Broker captures also append `broker.screenshot.capture-requested`,
`broker.screenshot.captured`, `broker.recording.capture-requested`, and
`broker.recording.captured` provenance events (with origin and sha256) to
`events.jsonl`. Live-proof-eligible bundles fail on `discovered` Visual
Evidence; `fake-broker-capture` artifacts are only accepted in the fake
runtime.

The fake broker supports configured Visual Evidence. With
`broker.type: gg-mayasessiond`, screenshot and recording Visual Evidence use an
interactive Windows scheduled task to capture the visible desktop session that
owns Maya. On real Windows Maya Hosts, both screenshots and recording frames
cover the full Windows virtual desktop across attached monitors rather than
only the primary screen. SSH screenshot capture streams its PowerShell program
over stdin so it does not depend on the Windows default-shell command-line
limit, uses a transient JPEG on the Windows Host, then transcodes it on the
controller so the Evidence artifact remains PNG without doing lossless
compression beside Maya. Recording uses 10 seconds at 15 fps by default and is
encoded locally with `ffmpeg`.

## publish

`maya-stall evidence publish` copies one Evidence Bundle to a filesystem
Evidence Store and writes the published manifest plus Review Comment markdown.

```sh
maya-stall evidence publish \
  --destination /mnt/evidence/maya-stall \
  --base-url https://evidence.example.com/maya-stall \
  artifacts/maya-stall/<run-id>
```

Publishing writes:

```text
<destination>/<run-id>/artifact-manifest.json
<destination>/<run-id>/review-comment.md
```

Publishing the same run again replaces the previous published run directory so
stale files do not survive.

`artifact-manifest.json` carries each Visual Evidence artifact's `origin` and
`sha256` through from the Evidence Bundle, so published manifests match bundle
manifests.

## live proof artifact

The protected GitHub Actions live proof workflow can also publish a sanitized
downloadable artifact named `live-visual-evidence-proof`. It is disabled by
default and enabled only in the live workflow through
`MAYA_STALL_LIVE_PROOF_ARTIFACT_ENABLED=true`.

The public artifact contains only reviewer-facing metadata:

- `proof-artifact-manifest.json`
- `evidence-metadata.json`

The protected runner still captures and validates a real broker-backed PNG and
the standalone `maya-stall record` MP4. It also validates a recording-enabled
Scenario through the paired live run gate before accepting the PR. Those pixel
files stay runner-local and are never uploaded.

`evidence-metadata.json` records `mediaPublished: false` and the verified source
hashes. The publisher only accepts bundles whose Visual Evidence carries
`broker-capture` provenance with a `sha256` that matches the runner-local
artifact bytes; `discovered` or `fake-broker-capture` origins fail the publish.

Retention is short and configurable with
`MAYA_STALL_LIVE_PROOF_RETENTION_DAYS` or the matching workflow variable.
Private host names should be replaced with
`MAYA_STALL_LIVE_PROOF_PUBLIC_HOST_ALIAS` before upload.
