import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const root = path.resolve(new URL("../..", import.meta.url).pathname);
const hostedPath = path.join(root, ".github", "workflows", "ci-hosted.yml");
const requiredPath = path.join(root, ".github", "workflows", "ci-required.yml");
const hosted = fs.readFileSync(hostedPath, "utf8");
const required = fs.readFileSync(requiredPath, "utf8");

test("candidate code runs only in the restricted pull-request workflow", () => {
  assert.match(hosted, /^  pull_request:/m);
  assert.match(hosted, /^permissions:\n  contents: read$/m);
  assert.doesNotMatch(hosted, /pull_request_target|workflow_run|allow-unsafe-pr-checkout|self-hosted|checks: write/);
  for (const command of ["go test -race ./...", "golangci-lint-action", "scripts/check-docs.sh", "node --test scripts/proof/"]) {
    assert.match(hosted, new RegExp(command.replaceAll("/", "\\/")));
  }
  for (const job of ["go_tests", "go_tests_windows", "lint", "docs", "proof_scripts"]) assert.doesNotMatch(required, new RegExp(`^  ${job}:`, "m"));
  assert.doesNotMatch(required, /allow-unsafe-pr-checkout/);
});

test("five hosted jobs are parallel, credentialless, cacheless, and cancelable", () => {
  for (const job of ["go_tests", "go_tests_windows", "lint", "docs", "proof_scripts"]) {
    const start = hosted.indexOf(`  ${job}:`);
    const nextMatch = hosted.slice(start + 3).match(/^  [a-z_]+:/m);
    const next = nextMatch ? start + 3 + nextMatch.index : -1;
    const body = hosted.slice(start, next === -1 ? undefined : next);
    assert.ok(start > 0);
    assert.doesNotMatch(body, /needs:/);
  }
  assert.match(hosted, /cancel-in-progress: true/);
  assert.doesNotMatch(hosted, /persist-credentials: true/);
  assert.doesNotMatch(hosted, /(?:^|[\s,{])cache: true/m);
  assert.match(hosted, /skip-cache: true/);
  assert.match(hosted, /--new-from-rev=\$\{\{ steps\.lint_base\.outputs\.base_sha \}\}/);
});

test("trusted default-branch workflow verifies hosted results and current head", () => {
  assert.match(required, /^  workflow_run:/m);
  assert.match(required, /workflows: \[CI Hosted\]/);
  assert.doesNotMatch(required, /^  pull_request(?:_target)?:/m);
  assert.match(required, /run\.path !== "\.github\/workflows\/ci-hosted\.yml"/);
  for (const name of ["Go Tests", "Go Tests (Windows)", "Lint", "Documentation", "Proof Policy And Helpers"]) assert.ok(required.includes(`"${name}"`));
  assert.match(required, /run\.conclusion === "success"/);
  assert.match(required, /github\.rest\.pulls\.list/);
  assert.match(required, /matches\.length !== 1/);
  assert.match(required, /jobs\.length === expectedJobs\.size && actualNames\.size === expectedJobs\.size/);
  assert.match(required, /currentHead = pr\.state === "open" && pr\.head\.sha === headSha && pr\.base\.sha === baseSha/);
  assert.match(required, /cancel-in-progress: true/);
});

test("one same-repository live job is serialized behind verified hosted gates", () => {
  assert.equal((required.match(/runs-on: \[self-hosted, maya-live-proof\]/g) ?? []).length, 1);
  assert.match(required, /environment: maya-live-proof/);
  assert.match(required, /hosted_ok == 'true'/);
  assert.match(required, /trusted_head == 'true'/);
  assert.equal((required.match(/name: (?:Guard|Recheck) current PR or main head/g) ?? []).length, 2);
});

test("all nine live smokes run serially in one bounded Go test process", () => {
  assert.equal((required.match(/go test -json \.\/internal\/cli -run/g) ?? []).length, 1);
	assert.match(required, /-parallel=1 -timeout=30m/);
	assert.match(required, /live_maya:[\s\S]*?timeout-minutes: 40/);
  assert.match(required, /go clean -cache -testcache/);
  assert.doesNotMatch(required, /extra_policy\[@\]/);
  for (const name of [
    "TestOptInRealVisualEvidenceSmoke",
    "TestOptInRealDesktopControlModalSmoke",
    "TestOptInRealSSHDoctorSmoke",
    "TestOptInRealPreRunReadinessSmoke",
    "TestOptInRealSSHConsumingRepoSmoke",
    "TestOptInRealSSHRunSmoke",
    "TestOptInRealHostLockContentionAndRecoverySmoke",
    "TestOptInRealRunScopedDesktopOpsSmoke",
    "TestOptInRealLocalSessiondRuntimeInputSmoke",
  ]) assert.match(required, new RegExp(name));
});

test("trusted aggregator publishes the only stable required Check Run", () => {
  assert.equal((required.match(/name: "CI \/ Required"/g) ?? []).length, 1);
  assert.equal((hosted.match(/CI \/ Required/g) ?? []).length, 0);
  assert.match(required, /actions\/create-github-app-token@v3/);
  assert.match(required, /app-id:.*CI_REQUIRED_APP_ID/);
  assert.match(required, /private-key:.*CI_REQUIRED_APP_PRIVATE_KEY/);
  assert.match(required, /permission-checks: write/);
  assert.match(required, /github-token:.*required_app\.outputs\.token/);
  assert.doesNotMatch(required, /^\s+checks: write$/m);
  assert.match(required, /HOSTED_OK/);
  assert.match(required, /CURRENT_HEAD/);
  assert.match(required, /LIVE_REQUIRED/);
});

test("public proof is exact-allowlisted before upload", () => {
  const confidentiality = required.indexOf("      - name: Public artifact confidentiality gate");
  const upload = required.indexOf("      - name: Upload live Visual Evidence proof");
  assert.ok(confidentiality > 0 && upload > confidentiality);
  for (const file of ["evidence-metadata.json", "proof-artifact-manifest.json"]) {
    assert.match(required.slice(confidentiality, upload), new RegExp(file.replace(".", "\\.")));
  }
  for (const file of ["media-review.json", "recording.mp4", "desktop-screenshot.png"]) {
    assert.doesNotMatch(required.slice(confidentiality, upload), new RegExp(file.replace(".", "\\.")));
  }
});

test("obsolete workflows are removed", () => {
  for (const name of ["proof.yml", "golangci-lint.yml", "ci.yml"]) {
    assert.equal(fs.existsSync(path.join(root, ".github", "workflows", name)), false);
  }
});
