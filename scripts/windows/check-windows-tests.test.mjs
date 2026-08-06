import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { execFileSync } from "node:child_process";
import { fileURLToPath } from "node:url";
import test from "node:test";
import assert from "node:assert/strict";

const root = path.resolve(fileURLToPath(new URL("../..", import.meta.url)));
const checker = path.join(root, "scripts", "windows", "check-windows-tests.mjs");
const PKG = "example.org/pkg";

test("passes when every known failure fails and every other test passes", () => {
  const result = runChecker([
    "compiler noise",
    testEvent("TestKnownFailure", "run"),
    testEvent("TestKnownFailure", "fail"),
    testEvent("TestPassing", "pass"),
    packageEvent(PKG, "fail"),
  ], "TestKnownFailure # documented cause\n");

  assert.equal(result.code, 0, result.stderr);
  assert.equal(result.stderr, "");
  assert.equal(result.stdout, "windows-test-gate: pass=1 fail=1 skip=0 incomplete=0 known=1\n");
});

test("reports an unexpected failure under add these", () => {
  const result = runChecker([
    testEvent("TestUnexpected", "fail"),
    packageEvent(PKG, "fail"),
  ], "");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /add these:\nTestUnexpected\n/);
});

test("reports a known failure that now passes under delete these", () => {
  const result = runChecker([
    testEvent("TestKnownFailure", "pass"),
    packageEvent(PKG, "pass"),
  ], "TestKnownFailure\n");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /delete these:\nTestKnownFailure\n/);
});

test("reports a known failure that was turned into a skip under delete these", () => {
  // Otherwise a failing test could be silenced with t.Skip while its entry sits
  // in the list unchanged, and the gate would stay green.
  const result = runChecker([
    testEvent("TestKnownFailure", "skip"),
    packageEvent(PKG, "pass"),
  ], "TestKnownFailure\n");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /delete these:\nTestKnownFailure\n/);
});

test("reports a known failure that matched no test under delete these", () => {
  const result = runChecker([
    testEvent("TestPassing", "pass"),
    packageEvent(PKG, "pass"),
  ], "TestMissing\n");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /delete these:\nTestMissing\n/);
});

test("fails closed on empty or garbage input", () => {
  for (const lines of [[], ["build failed", JSON.stringify({ Action: "fail", Package: PKG })]]) {
    const result = runChecker(lines, "");
    assert.equal(result.code, 1);
    assert.match(result.stderr, /no test results/);
  }
});

test("fails closed when a test starts but never completes", () => {
  const result = runChecker([
    testEvent("TestCompleted", "pass"),
    testEvent("TestCrashedMidway", "run"),
    packageEvent(PKG, "fail"),
  ], "");

  assert.equal(result.code, 1);
  assert.match(result.stdout, /incomplete=1/);
  assert.match(result.stderr, /started but never completed/);
  assert.match(result.stderr, /TestCrashedMidway/);
});

test("an incomplete known failure is not reported as a stale entry", () => {
  const result = runChecker([
    testEvent("TestKnownFailure", "run"),
    packageEvent(PKG, "fail"),
  ], "TestKnownFailure\n");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /started but never completed/);
  assert.doesNotMatch(result.stderr, /delete these/);
});

test("fails closed when a package never reports a terminal result", () => {
  const result = runChecker([
    testEvent("TestPassing", "pass"),
  ], "");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /never reported a terminal result/);
  assert.match(result.stderr, new RegExp(PKG.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
});

test("fails closed when a package fails with no failing test", () => {
  // A build error, a TestMain failure, or a panic in package init. Per-test
  // allowlisting cannot express those.
  const result = runChecker([
    testEvent("TestPassing", "pass"),
    packageEvent(PKG, "fail"),
  ], "");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /failed with no failing test/);
});

test("fails closed when one test name exists in two packages", () => {
  const result = runChecker([
    testEvent("TestShared", "pass", "example.org/one"),
    testEvent("TestShared", "fail", "example.org/two"),
    packageEvent("example.org/one", "pass"),
    packageEvent("example.org/two", "fail"),
  ], "TestShared\n");

  assert.equal(result.code, 1);
  assert.match(result.stderr, /more than one package/);
  assert.match(result.stderr, /TestShared/);
});

test("rolls a failing subtest up to its top-level parent", () => {
  const result = runChecker([
    testEvent("TestParent", "pass"),
    testEvent("TestParent/subtest", "fail"),
    packageEvent(PKG, "fail"),
  ], "TestParent\n");

  assert.equal(result.code, 0, result.stderr);
  assert.match(result.stdout, /pass=0 fail=1 skip=0 incomplete=0 known=1/);
});

function testEvent(testName, action, pkg = PKG) {
  return JSON.stringify({ Action: action, Package: pkg, Test: testName });
}

function packageEvent(pkg, action) {
  return JSON.stringify({ Action: action, Package: pkg });
}

function runChecker(lines, knownFailures) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "windows-test-gate-"));
  const resultsPath = path.join(dir, "windows-tests.json");
  const knownPath = path.join(dir, "known-failures.txt");
  fs.writeFileSync(resultsPath, lines.length === 0 ? "" : `${lines.join("\n")}\n`);
  fs.writeFileSync(knownPath, knownFailures);

  try {
    const stdout = execFileSync(process.execPath, [checker, resultsPath, knownPath], {
      cwd: dir,
      encoding: "utf8",
      stdio: "pipe",
    });
    return { code: 0, stdout, stderr: "" };
  } catch (error) {
    return {
      code: error.status,
      stdout: error.stdout ?? "",
      stderr: error.stderr ?? "",
    };
  } finally {
    fs.rmSync(dir, { recursive: true, force: true });
  }
}
