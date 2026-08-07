# CI Performance

Read this when:

- comparing hosted CI feedback with the live Maya proof path;
- checking whether CI topology or runner capacity regressed;
- collecting representative timing evidence after a workflow change.

## Baseline

Measured on 2026-07-11 after the CI redesign:

- hosted feedback: 38 seconds;
- live-runner queue after trusted classification: 56 seconds;
- live execution: 422 seconds;
- latest-head behavior: an obsolete running live proof was cancelled when the newer head began.

Hosted feedback is the developer-latency target and should remain under 90
seconds. Live queue and execution are reported separately because they measure
protected-runner capacity and real Maya proof, not hosted feedback.

The representative successful run is
<https://github.com/BramVR/maya_stall/actions/runs/29153711052>. The
latest-head cancellation proof is
<https://github.com/BramVR/maya_stall/actions/runs/29153790625>.

Use `scripts/proof/report-ci-timing.mjs` as documented in
`docs/agents/pr-merge.md` to refresh these measurements.
