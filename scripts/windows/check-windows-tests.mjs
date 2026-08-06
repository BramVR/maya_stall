// Fail-closed gate for the Windows CI job.
//
// The job runs the full `go test ./... -json` suite and masks its exit status,
// because expected failures make go test exit non-zero. This script is the real
// gate: it compares the run against scripts/windows/known-failures.txt and fails
// on anything that is not exactly the recorded state, so the list can only
// shrink and a broken run can never read as success.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";

const TERMINAL_ACTIONS = ["pass", "fail", "skip"];

const root = path.resolve(fileURLToPath(new URL("../..", import.meta.url)));
const [resultsPath, knownPath = path.join(root, "scripts", "windows", "known-failures.txt"), ...extraArgs] = process.argv.slice(2);

if (!resultsPath || extraArgs.length > 0) {
  fail("usage: node scripts/windows/check-windows-tests.mjs <go-test-json-file> [known-failures-file]");
}

let knownFailures;
try {
  knownFailures = readKnownFailures(knownPath);
} catch (error) {
  fail(`cannot read known failures: ${error.message}`);
}

let input;
try {
  input = fs.readFileSync(resultsPath, "utf8");
} catch (error) {
  fail(`cannot read test results: ${error.message}`);
}

const run = readRun(input);
printSummary(run, knownFailures.length);

if (run.eventCount === 0) {
  fail("no test results -- the build or the run itself failed");
}

// A test that emitted events but never reached pass/fail/skip did not complete:
// a panic aborting the binary, -timeout firing, or a killed process. Checked
// before the known-failures comparison, which would otherwise misreport the
// aborted tail as stale entries.
if (run.incomplete.length > 0) {
  fail(`${run.incomplete.length} test(s) started but never completed -- the suite did not finish`, [], [], run.incomplete);
}

// Every package that ran tests must report a terminal result of its own,
// otherwise the stream was truncated between packages.
const unfinishedPackages = [...run.packagesWithTests].filter((pkg) => !run.packageResults.has(pkg));
if (unfinishedPackages.length > 0) {
  fail("package(s) never reported a terminal result -- the run was truncated", [], [], unfinishedPackages);
}

// A package can fail without any of its tests failing: a build error, a TestMain
// failure, or a panic in package initialisation. Per-test allowlisting cannot
// express that, so it is always a hard failure.
const packagesFailedOutsideTests = [...run.packageResults]
  .filter(([pkg, action]) => action === "fail" && !run.failingPackages.has(pkg))
  .map(([pkg]) => pkg);
if (packagesFailedOutsideTests.length > 0) {
  fail("package(s) failed with no failing test -- failure outside the tests themselves", [], [], packagesFailedOutsideTests);
}

// known-failures.txt identifies tests by bare name. That is unambiguous only
// while a single package has tests: today only internal/cli has any. Rather
// than silently aliasing two same-named tests, stop and require this script to
// grow package-qualified identities first -- do not tell the operator to write
// a qualified entry here, because nothing would parse it.
const aliased = [...run.testPackages].filter(([, packages]) => packages.size > 1).map(([testName]) => testName);
if (aliased.length > 0) {
  fail("test name(s) exist in more than one package -- this gate needs package-qualified identities before it can be trusted; see readRun()", [], [], aliased);
}

const knownSet = new Set(knownFailures);
const additions = [...run.statuses]
  .filter(([testName, status]) => status === "fail" && !knownSet.has(testName))
  .map(([testName]) => testName);

// Every listed entry must actually fail. A listed test that now passes, that was
// turned into a skip, or that no longer exists is stale -- otherwise a failure
// could be silenced with t.Skip while the entry sits there unchanged.
const deletions = knownFailures.filter((testName) => run.statuses.get(testName) !== "fail");

if (additions.length > 0 || deletions.length > 0) {
  fail("known-failures mismatch", additions, deletions);
}

function readKnownFailures(filePath) {
  return fs.readFileSync(filePath, "utf8")
    .split(/\r?\n/)
    .map((line) => line.split("#", 1)[0].trim())
    .filter(Boolean);
}

function readRun(jsonLines) {
  const statuses = new Map();
  const started = new Set();
  const testPackages = new Map();
  const packageResults = new Map();
  const packagesWithTests = new Set();
  const failingPackages = new Set();
  let eventCount = 0;

  for (const line of jsonLines.split(/\r?\n/)) {
    let event;
    try {
      event = JSON.parse(line);
    } catch {
      continue;
    }
    if (typeof event?.Package !== "string" || event.Package === "") {
      continue;
    }
    if (typeof event.Test !== "string" || event.Test === "") {
      if (TERMINAL_ACTIONS.includes(event.Action)) {
        packageResults.set(event.Package, event.Action);
      }
      continue;
    }

    eventCount += 1;
    const testName = event.Test.split("/", 1)[0];
    started.add(testName);
    packagesWithTests.add(event.Package);
    if (!testPackages.has(testName)) {
      testPackages.set(testName, new Set());
    }
    testPackages.get(testName).add(event.Package);

    const current = statuses.get(testName);
    if (event.Action === "fail") {
      statuses.set(testName, "fail");
      failingPackages.add(event.Package);
    } else if (event.Action === "pass" && current !== "fail") {
      statuses.set(testName, "pass");
    } else if (event.Action === "skip" && current === undefined) {
      statuses.set(testName, "skip");
    }
  }

  const incomplete = [...started].filter((testName) => !statuses.has(testName)).sort();
  return { eventCount, statuses, incomplete, testPackages, packageResults, packagesWithTests, failingPackages };
}

function printSummary(run, knownCount) {
  const counts = { pass: 0, fail: 0, skip: 0 };
  for (const status of run.statuses.values()) {
    counts[status] += 1;
  }
  console.log(`windows-test-gate: pass=${counts.pass} fail=${counts.fail} skip=${counts.skip} incomplete=${run.incomplete.length} known=${knownCount}`);
}

function fail(message, additions = [], deletions = [], details = []) {
  console.error(`windows-test-gate: ${message}`);
  for (const detail of details) {
    console.error(detail);
  }
  if (details.length === 0) {
    console.error("add these:");
    for (const testName of additions) {
      console.error(testName);
    }
    console.error("delete these:");
    for (const testName of deletions) {
      console.error(testName);
    }
  }
  process.exit(1);
}
