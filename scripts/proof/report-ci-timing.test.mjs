import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const root = path.resolve(new URL("../..", import.meta.url).pathname);
const script = path.join(root, "scripts", "proof", "report-ci-timing.mjs");

test("timing report separates hosted feedback, trusted queue, and live execution", () => {
  const hosted = [
    job("Go Tests", 0, 40), job("Go Tests (Windows)", 0, 55), job("Lint", 0, 25), job("Documentation", 0, 15), job("Proof Policy And Helpers", 0, 30),
  ];
  const trusted = [job("Classify Proof", 45, 55), job("Live Maya Proof", 70, 190)];
  const output = run(hosted, trusted);
  assert.match(output, /hosted_feedback_seconds: 55/);
  assert.match(output, /live_runner_queue_seconds: 15/);
  assert.match(output, /live_execution_seconds: 120/);
});

test("timing report treats a skipped live job as not scheduled", () => {
  const hosted = [
    job("Go Tests", 0, 40), job("Go Tests (Windows)", 0, 55), job("Lint", 0, 25), job("Documentation", 0, 15), job("Proof Policy And Helpers", 0, 30),
  ];
  const trusted = [job("Classify Proof", 45, 55), { name: "Live Maya Proof", conclusion: "skipped" }];
  const output = run(hosted, trusted);
  assert.match(output, /live_runner_queue_seconds: not_scheduled/);
  assert.match(output, /live_execution_seconds: not_scheduled/);
});

function job(name, startedSeconds, completedSeconds) {
  const epoch = Date.parse("2026-01-01T00:00:00Z");
  return {
    name,
    conclusion: "success",
    started_at: new Date(epoch + startedSeconds * 1000).toISOString(),
    completed_at: new Date(epoch + completedSeconds * 1000).toISOString(),
  };
}

function run(hostedJobs, trustedJobs) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "maya-stall-timing-"));
  const hosted = path.join(dir, "hosted.json");
  const trusted = path.join(dir, "trusted.json");
  fs.writeFileSync(hosted, JSON.stringify({ jobs: hostedJobs }));
  fs.writeFileSync(trusted, JSON.stringify({ jobs: trustedJobs }));
  return execFileSync("node", [script, "--hosted-input", hosted, "--trusted-input", trusted, "--hosted-run-created-at", "2026-01-01T00:00:00Z"], { cwd: root, encoding: "utf8" });
}
