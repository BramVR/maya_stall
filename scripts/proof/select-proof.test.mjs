import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import test from "node:test";
import assert from "node:assert/strict";

const root = path.resolve(new URL("../..", import.meta.url).pathname);
const selectScript = path.join(root, "scripts", "proof", "select-proof.mjs");
const assertScript = path.join(root, "scripts", "proof", "assert-live-proof.mjs");
const confidentialityScript = path.join(root, "scripts", "proof", "assert-public-artifact-confidentiality.mjs");

test("selector requires live Maya proof for live product behavior paths", () => {
  const dir = tempDir();
  const changed = path.join(dir, "changed.txt");
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(changed, "internal/cli/sessiond_broker.go\nREADME.md\n");

  execFileSync("node", [
    selectScript,
    "--changed-files",
    changed,
    "--output",
    manifestPath,
  ], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.live_maya_required, true);
  assert.deepEqual(manifest.changed_files, [
    "internal/cli/sessiond_broker.go",
    "README.md",
  ]);
  assert.equal(manifest.live_maya_reasons[0].rule, "session-broker");
  assert.equal(manifest.gates.live_maya.status, "required");
  assert.equal(manifest.gates.live_maya.command, "go test -json ./internal/cli -run '^(TestOptInRealVisualEvidenceSmoke|TestOptInRealDesktopControlModalSmoke|TestOptInRealSSHDoctorSmoke|TestOptInRealPreRunReadinessSmoke|TestOptInRealSSHConsumingRepoSmoke|TestOptInRealSSHRunSmoke|TestOptInRealHostLockContentionAndRecoverySmoke|TestOptInRealRunScopedDesktopOpsSmoke|TestOptInRealLocalSessiondRuntimeInputSmoke|TestOptInRealSSHTransportSurvivesLeaseRenewalAndSFTPCollection|TestOptInRealSSHTransportMayaAcceptance)$' -count=1 -parallel=1 -timeout=45m");
  assert.equal(manifest.gates.local.status, "pending");
  assert.equal(manifest.gates.docs.status, "pending");
  assert.equal(manifest.gates.artifacts.status, "pending");
});

test("selector allows docs-only changes with manifest saying live is not required", () => {
  const dir = tempDir();
  const changed = path.join(dir, "changed.txt");
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(changed, "docs/commands/version.md\n");

  execFileSync("node", [
    selectScript,
    "--changed-files",
    changed,
    "--output",
    manifestPath,
  ], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.live_maya_required, false);
  assert.deepEqual(manifest.live_maya_reasons, []);
  assert.equal(manifest.gates.live_maya.status, "not_required");
});

test("selector requires live Maya proof for deleted watched paths", () => {
  const dir = tempDir();
  const changed = path.join(dir, "changed.txt");
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(changed, "D\tinternal/cli/sessiond_broker.go\n");

  execFileSync("node", [
    selectScript,
    "--changed-files",
    changed,
    "--output",
    manifestPath,
  ], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.live_maya_required, true);
  assert.equal(manifest.changed_files[0], "internal/cli/sessiond_broker.go");
  assert.equal(manifest.live_maya_reasons[0].rule, "session-broker");
});

test("selector checks both old and new rename paths", () => {
  const dir = tempDir();
  const changed = path.join(dir, "changed.txt");
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(changed, "R100\tinternal/cli/sessiond_broker.go\tinternal/cli/broker_renamed.go\n");

  execFileSync("node", [
    selectScript,
    "--changed-files",
    changed,
    "--output",
    manifestPath,
  ], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.live_maya_required, true);
  assert.deepEqual(manifest.changed_files, [
    "internal/cli/sessiond_broker.go",
    "internal/cli/broker_renamed.go",
  ]);
  assert.equal(manifest.live_maya_reasons[0].path, "internal/cli/sessiond_broker.go");
});

test("selector requires live Maya proof for new files under live source prefixes", () => {
  const dir = tempDir();
  const changed = path.join(dir, "changed.txt");
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(changed, "A\tinternal/cli/new_live_surface.go\n");

  execFileSync("node", [
    selectScript,
    "--changed-files",
    changed,
    "--output",
    manifestPath,
  ], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.live_maya_required, true);
  assert.equal(manifest.live_maya_reasons[0].rule, "live-product-source");
});

test("selector cannot weaken the trusted base policy in the changed head", () => {
  const dir = tempDir();
  const changed = path.join(dir, "changed.txt");
  const manifestPath = path.join(dir, "proof-manifest.json");
  const headPolicy = path.join(dir, "head-policy.json");
  const basePolicy = path.join(dir, "base-policy.json");
  fs.writeFileSync(changed, "internal/cli/sessiond_broker.go\n");
  fs.writeFileSync(headPolicy, JSON.stringify({ schema_version: 1, rules: [] }));
  fs.writeFileSync(basePolicy, JSON.stringify({
    schema_version: 1,
    rules: [{ id: "trusted", reason: "trusted base policy", paths: ["internal/cli/sessiond_broker.go"] }],
  }));

  execFileSync("node", [
    selectScript,
    "--changed-files", changed,
    "--policy", headPolicy,
    "--additional-policy", basePolicy,
    "--output", manifestPath,
  ], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.live_maya_required, true);
  assert.equal(manifest.live_maya_reasons[0].rule, "trusted");
});

test("selector preserves untrusted filenames through JSON input", () => {
  const dir = tempDir();
  const changed = path.join(dir, "changed.json");
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(changed, JSON.stringify(["internal/cli/\tlive.go"]));

  execFileSync("node", [selectScript, "--changed-files-json", changed, "--output", manifestPath], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.live_maya_required, true);
  assert.deepEqual(manifest.changed_files, ["internal/cli/\tlive.go"]);
});

test("assert-live-proof fails closed when required live proof has no host config", () => {
  const dir = tempDir();
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(manifestPath, JSON.stringify({
    schema_version: 1,
    live_maya_required: true,
    gates: {
      live_maya: { status: "required" },
    },
  }));

  assert.throws(() => {
    execFileSync("node", [
      assertScript,
      "--manifest",
      manifestPath,
      "--live-status",
      "skipped",
      "--host-config-state",
      "missing",
    ], { cwd: root, stdio: "pipe" });
  }, /live Maya proof is required/);
});

test("assert-live-proof accepts non-live changes without host config", () => {
  const dir = tempDir();
  const manifestPath = path.join(dir, "proof-manifest.json");
  fs.writeFileSync(manifestPath, JSON.stringify({
    schema_version: 1,
    live_maya_required: false,
    gates: {
      live_maya: { status: "not_required" },
    },
  }));

  execFileSync("node", [
    assertScript,
    "--manifest",
    manifestPath,
    "--live-status",
    "skipped",
    "--host-config-state",
    "missing",
  ], { cwd: root });

  const manifest = readJSON(manifestPath);
  assert.equal(manifest.gates.live_maya.status, "not_required");
});

test("public artifact confidentiality gate accepts sanitized proof text", () => {
  const dir = tempDir();
  fs.mkdirSync(path.join(dir, "screenshots"), { recursive: true });
  fs.writeFileSync(path.join(dir, "screenshots", "desktop-screenshot.png"), "png");
  fs.writeFileSync(path.join(dir, "media-review.json"), JSON.stringify({
    reviewed: true,
    paths: ["screenshots/desktop-screenshot.png"],
  }));
  fs.writeFileSync(path.join(dir, "proof-artifact-manifest.json"), JSON.stringify({
    selectedHostAlias: "maya-live-proof-host",
    artifacts: [
      { path: "screenshots/desktop-screenshot.png", mediaType: "image/png", bytes: 12, sha256: "a".repeat(64) },
    ],
  }));

  execFileSync("node", [
    confidentialityScript,
    "--path",
    dir,
  ], { cwd: root });
});

test("public artifact confidentiality gate rejects private proof text", () => {
  const dir = tempDir();
  fs.writeFileSync(path.join(dir, "proof-artifact-manifest.json"), JSON.stringify({
    host: "desktop-private",
    token: "abc",
  }));

  assert.throws(() => {
    execFileSync("node", [
      confidentialityScript,
      "--path",
      dir,
    ], { cwd: root, stdio: "pipe" });
  }, /confidentiality gate failed/);
});

test("public artifact confidentiality gate rejects unreviewed media files", () => {
  const dir = tempDir();
  fs.mkdirSync(path.join(dir, "recordings"), { recursive: true });
  fs.writeFileSync(path.join(dir, "recordings", "desktop-recording.mp4"), "mp4");
  fs.writeFileSync(path.join(dir, "proof-artifact-manifest.json"), JSON.stringify({
    selectedHostAlias: "maya-live-proof-host",
  }));

  assert.throws(() => {
    execFileSync("node", [
      confidentialityScript,
      "--path",
      dir,
    ], { cwd: root, stdio: "pipe" });
  }, /confidentiality gate failed/);
});

test("public artifact confidentiality gate rejects quoted JSON secret keys", () => {
  const dir = tempDir();
  fs.writeFileSync(path.join(dir, "proof-artifact-manifest.json"), JSON.stringify({
    selectedHostAlias: "maya-live-proof-host",
    token: "abc",
  }));

  assert.throws(() => {
    execFileSync("node", [
      confidentialityScript,
      "--path",
      dir,
    ], { cwd: root, stdio: "pipe" });
  }, /confidentiality gate failed/);
});

function tempDir() {
  return fs.mkdtempSync(path.join(os.tmpdir(), "maya-stall-proof-"));
}

function readJSON(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}
