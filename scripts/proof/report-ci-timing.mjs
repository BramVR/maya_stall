import fs from "node:fs";

const args = parseArgs(process.argv.slice(2));
const hostedJobs = jobs(args.hosted_input);
const trustedJobs = jobs(args.trusted_input);
const runCreated = timestamp(args.hosted_run_created_at, "hosted run creation");
const hostedNames = new Set(["Go Tests", "Go Tests (Windows)", "Lint", "Documentation", "Proof Policy And Helpers"]);
const hosted = hostedJobs.filter((job) => hostedNames.has(job.name));
if (hosted.length !== hostedNames.size) fail("timing input is missing one or more hosted jobs");
const hostedComplete = Math.max(...hosted.map((job) => timestamp(job.completed_at, `${job.name} completion`)));
const classification = trustedJobs.find((job) => job.name === "Classify Proof");
if (!classification) fail("timing input is missing trusted classification");
const classificationComplete = timestamp(classification.completed_at, "classification completion");
const live = trustedJobs.find((job) => job.name === "Live Maya Proof" && job.conclusion !== "skipped");

console.log(`hosted_feedback_seconds: ${seconds(hostedComplete - runCreated)}`);
if (!live) {
  console.log("live_runner_queue_seconds: not_scheduled");
  console.log("live_execution_seconds: not_scheduled");
} else {
  const liveStart = timestamp(live.started_at, "live start");
  const liveComplete = timestamp(live.completed_at, "live completion");
  console.log(`live_runner_queue_seconds: ${seconds(Math.max(0, liveStart - classificationComplete))}`);
  console.log(`live_execution_seconds: ${seconds(liveComplete - liveStart)}`);
}

function jobs(input) {
  const payload = JSON.parse(fs.readFileSync(input, "utf8"));
  return payload.jobs ?? payload;
}

function seconds(milliseconds) {
  return Math.round(milliseconds / 1000);
}

function timestamp(value, label) {
  const parsed = Date.parse(value ?? "");
  if (!Number.isFinite(parsed)) fail(`${label} timestamp is missing or invalid`);
  return parsed;
}

function parseArgs(argv) {
  const parsed = {};
  for (let i = 0; i < argv.length; i += 2) {
    const option = argv[i];
    const value = argv[i + 1];
    if (!["--hosted-input", "--trusted-input", "--hosted-run-created-at"].includes(option) || !value) fail(`invalid option ${option ?? ""}`);
    parsed[option.slice(2).replaceAll("-", "_")] = value;
  }
  if (!parsed.hosted_input || !parsed.trusted_input || !parsed.hosted_run_created_at) fail("all timing inputs are required");
  return parsed;
}

function fail(message) {
  console.error(`report-ci-timing: ${message}`);
  process.exit(2);
}
