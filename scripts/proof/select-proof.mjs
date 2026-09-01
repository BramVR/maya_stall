import fs from "node:fs";
import path from "node:path";
import { execFileSync } from "node:child_process";

const root = path.resolve(new URL("../..", import.meta.url).pathname);
const defaultPolicyPath = path.join(root, "proof", "live-maya-policy.json");
const args = parseArgs(process.argv.slice(2));
const policyPath = path.resolve(root, args.policy ?? defaultPolicyPath);
const outputPath = path.resolve(root, args.output ?? path.join("artifacts", "proof", "proof-manifest.json"));
const policies = [readJSON(policyPath)];
if (args.additional_policy) policies.push(readJSON(path.resolve(root, args.additional_policy)));
const rawChangedFiles = readChangedFiles(args);
const changedFiles = args.changed_files_json || args.diff_mode === "exact" ? uniqueStructuredPaths(rawChangedFiles) : uniqueChangedFiles(rawChangedFiles);
const liveReasons = uniqueLiveReasons(policies.flatMap((policy) => selectLiveReasons(policy, changedFiles)));
const liveRequired = liveReasons.length > 0;

const manifest = {
  schema_version: 1,
  generated_at: new Date().toISOString(),
  policy: path.relative(root, policyPath).replaceAll(path.sep, "/"),
  base: args.base ?? "",
  head: args.head ?? "",
  changed_files: changedFiles,
  live_maya_required: liveRequired,
  live_maya_reasons: liveReasons,
  gates: {
    local: { status: "pending", command: "go test ./..." },
    docs: { status: "pending", command: "scripts/check-docs.sh" },
    artifacts: { status: "pending", required: true },
    live_maya: {
      status: liveRequired ? "required" : "not_required",
      command: "go test -json ./internal/cli -run '^(TestOptInRealVisualEvidenceSmoke|TestOptInRealDesktopControlModalSmoke|TestOptInRealSSHDoctorSmoke|TestOptInRealPreRunReadinessSmoke|TestOptInRealSSHConsumingRepoSmoke|TestOptInRealSSHRunSmoke|TestOptInRealHostLockContentionAndRecoverySmoke|TestOptInRealRunScopedDesktopOpsSmoke|TestOptInRealLocalSessiondRuntimeInputSmoke|TestOptInRealSSHTransportSurvivesLeaseRenewalAndSFTPCollection|TestOptInRealSSHTransportMayaAcceptance)$' -count=1 -parallel=1 -timeout=45m",
      fail_closed: liveRequired,
    },
  },
};

fs.mkdirSync(path.dirname(outputPath), { recursive: true });
fs.writeFileSync(outputPath, `${JSON.stringify(manifest, null, 2)}\n`);
writeGitHubOutput({
  manifest: path.relative(root, outputPath).replaceAll(path.sep, "/"),
  live_maya_required: liveRequired ? "true" : "false",
});
console.log(`proof_manifest: ${path.relative(root, outputPath).replaceAll(path.sep, "/")}`);
console.log(`live_maya_required: ${liveRequired ? "true" : "false"}`);
if (liveReasons.length > 0) {
  for (const reason of liveReasons) {
    console.log(`live_maya_reason: ${reason.rule} ${reason.path}`);
  }
}

function parseArgs(argv) {
  const parsed = {};
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i];
    switch (arg) {
      case "--policy":
      case "--output":
      case "--changed-files":
      case "--changed-files-json":
      case "--additional-policy":
      case "--diff-mode":
      case "--base":
      case "--head":
        i++;
        if (i >= argv.length || argv[i] === "") {
          fail(`${arg} needs a value`);
        }
        parsed[arg.slice(2).replaceAll("-", "_")] = argv[i];
        break;
      default:
        fail(`unknown option ${arg}`);
    }
  }
  parsed.changedFiles = parsed.changed_files;
  parsed.base = parsed.base ?? process.env.MAYA_STALL_PROOF_BASE ?? "";
  parsed.head = parsed.head ?? process.env.MAYA_STALL_PROOF_HEAD ?? "HEAD";
  return parsed;
}

function uniqueLiveReasons(reasons) {
  const seen = new Set();
  return reasons.filter((reason) => {
    const key = `${reason.path}\0${reason.rule}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

function readChangedFiles(options) {
  if (options.changed_files_json) {
    const value = readJSON(path.resolve(root, options.changed_files_json));
    if (!Array.isArray(value) || value.some((file) => typeof file !== "string")) fail("--changed-files-json must contain an array of strings");
    return value;
  }
  if (options.changedFiles) {
    return fs.readFileSync(path.resolve(root, options.changedFiles), "utf8").split(/\r?\n/);
  }
  if (!options.base) {
    return execFileSync("git", ["diff", "--name-status", "--diff-filter=ACDMRT", "HEAD"], {
      cwd: root,
      encoding: "utf8",
    }).split(/\r?\n/);
  }
  if (options.diff_mode && options.diff_mode !== "exact") fail("--diff-mode must be exact");
  if (options.diff_mode === "exact") {
    const output = execFileSync("git", ["diff", "--name-status", "-z", "--diff-filter=ACDMRT", options.base, options.head], { cwd: root });
    return changedFilesFromNulNameStatus(output);
  }
  return execFileSync("git", ["diff", "--name-status", "--diff-filter=ACDMRT", `${options.base}...${options.head}`], {
    cwd: root,
    encoding: "utf8",
  }).split(/\r?\n/);
}

function changedFilesFromNulNameStatus(output) {
  const fields = output.toString("utf8").split("\0");
  const files = [];
  for (let i = 0; i < fields.length && fields[i];) {
    const status = fields[i++];
    if (/^[RC]\d*/.test(status)) files.push(fields[i++], fields[i++]);
    else files.push(fields[i++]);
  }
  return files;
}

function uniqueChangedFiles(lines) {
  const files = [];
  const seen = new Set();
  for (const line of lines) {
    for (const file of changedFilesFromLine(line)) {
      if (!seen.has(file)) {
        seen.add(file);
        files.push(file);
      }
    }
  }
  return files;
}

function uniqueStructuredPaths(paths) {
  const seen = new Set();
  const files = [];
  for (const file of paths) {
    const normalized = file.replaceAll("\\", "/").replace(/^\.\/+/, "");
    if (!seen.has(normalized)) {
      seen.add(normalized);
      files.push(normalized);
    }
  }
  return files;
}

function changedFilesFromLine(line) {
  const trimmed = line.trim();
  if (!trimmed) {
    return [];
  }
  const fields = trimmed.split(/\t+/);
  if (fields.length === 1) {
    return [normalizePath(fields[0])].filter(Boolean);
  }
  const status = fields[0];
  if (/^[RC]\d*/.test(status)) {
    return fields.slice(1, 3).map(normalizePath).filter(Boolean);
  }
  return [normalizePath(fields[1])].filter(Boolean);
}

function selectLiveReasons(policy, changedFiles) {
  const reasons = [];
  for (const file of changedFiles) {
    for (const rule of policy.rules ?? []) {
      if (matchesRule(file, rule)) {
        reasons.push({ path: file, rule: rule.id, reason: rule.reason });
        break;
      }
    }
  }
  return reasons;
}

function matchesRule(file, rule) {
  return (rule.paths ?? []).includes(file) ||
    (rule.prefixes ?? []).some((prefix) => file.startsWith(prefix));
}

function readJSON(file) {
  return JSON.parse(fs.readFileSync(file, "utf8"));
}

function normalizePath(file) {
  return file.trim().replaceAll("\\", "/").replace(/^\.\/+/, "");
}

function writeGitHubOutput(values) {
  const githubOutput = process.env.GITHUB_OUTPUT;
  if (!githubOutput) {
    return;
  }
  const lines = Object.entries(values).map(([key, value]) => `${key}=${value}`);
  fs.appendFileSync(githubOutput, `${lines.join("\n")}\n`);
}

function fail(message) {
  console.error(`select-proof: ${message}`);
  process.exit(2);
}
