package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOptInRealLocalSessiondRuntimeInputSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	options.Host = liveSmokeHostForContention(t, options)
	controlHost := liveSmokeHostConfigByID(t, options, options.Host)
	restoreLiveSessionBrokerFixture(t, controlHost)
	controlBroker := ggMayaSessiondBroker{host: controlHost}
	if status, err := controlBroker.status(); err != nil {
		t.Fatalf("inspect configured live Sessiond before local proof: %v", err)
	} else if sessiondSessionLooksActive(status) {
		if err := controlBroker.stopSessiondSession(); err != nil {
			t.Fatalf("stop configured live Sessiond before isolated local proof: %v", err)
		}
	}
	t.Cleanup(func() { restoreLiveSessionBrokerFixture(t, controlHost) })

	paths := readLiveSessiondLaunchPaths(t, controlHost)
	suffix := strconv.FormatInt(time.Now().UTC().UnixNano(), 10)
	proofRoot := remoteJoin(controlHost.WorkRoot, "proofs", "local-sessiond-runtime-input-"+suffix)
	workRoot := remoteJoin(proofRoot, "host-work")
	stateDir := remoteJoin(proofRoot, "sessiond-state")
	consumerRoot := proofRoot
	port := 20000 + int(time.Now().UTC().UnixNano()%20000)
	assertLiveProofPortFree(t, controlHost, port)

	localRoot := t.TempDir()
	executable := buildLiveWindowsCandidate(t, localRoot)
	writeLocalSessiondLiveFixture(t, localRoot, proofRoot, controlHost.ID, workRoot, stateDir, controlHost.Broker.Python, controlHost.Broker.Repo, paths, port)
	stageLocalSessiondLiveFixture(t, controlHost, localRoot, proofRoot, executable)
	taskName := "MayaStallLocalRuntimeInput-" + suffix
	t.Cleanup(func() { cleanupLocalSessiondLiveProof(t, controlHost, proofRoot, taskName) })
	startLocalSessiondLiveTask(t, controlHost, proofRoot, taskName)
	waitForLocalSessiondLiveCompletion(t, controlHost, proofRoot, 8*time.Minute)

	runOutput := readRemoteText(t, controlHost, remoteJoin(proofRoot, "run-output.jsonl"))
	runID, evidenceDir := terminalLiveLocalRun(t, runOutput)
	if exit := strings.TrimSpace(readRemoteText(t, controlHost, remoteJoin(proofRoot, "exit-code.txt"))); exit != "0" {
		t.Fatalf("local Windows candidate exit code = %s; output:\n%s", exit, runOutput)
	}
	proof := inspectLiveLocalSessiondProof(t, controlHost, proofRoot, workRoot, stateDir, consumerRoot, evidenceDir, runID, port)
	if proof.Status != "passed" || proof.Runtime != "local-sessiond" || proof.HostAdapter != "local-windows" {
		t.Fatalf("local Sessiond Evidence runtime = %+v", proof)
	}
	if proof.BrokerSession == "" || proof.BrokerSession != proof.StoppedSession || proof.BrokerSession != proof.LockSession || proof.LockRun != runID {
		t.Fatalf("local Sessiond exact ownership proof = %+v", proof)
	}
	if proof.MayaSessionID == 0 {
		t.Fatalf("local Maya ran in Windows Services session 0: %+v", proof)
	}
	if proof.InputName != "scene" || proof.InputKind != "runtimeInput:file" || proof.InputSourcePresent || !proof.SourceUnchanged || !proof.StagedMutated {
		t.Fatalf("local Runtime Input proof = %+v", proof)
	}
	if !proof.ScreenshotPNG || proof.ScreenshotBytes == 0 || !proof.ScreenshotHashMatch || proof.ScreenshotOrigin != visualEvidenceOriginBrokerCapture {
		t.Fatalf("local Visual Evidence proof = %+v", proof)
	}
	if !proof.ValidatorsPassed || proof.SessiondStatus != "stopped" || proof.SessiondMayaAlive || proof.SessiondMCPAlive || proof.OwnedProcessAlive || proof.HostLockExists || proof.RunRootExists || proof.LocalRunStateExists || proof.PortListening {
		t.Fatalf("local Sessiond cleanup proof = %+v", proof)
	}

	cleanupLocalSessiondLiveProof(t, controlHost, proofRoot, taskName)
	if output, err := runSSHCommandOutput(controlHost, encodedPowerShellCommand(fmt.Sprintf("[bool](Test-Path -LiteralPath %s) | ConvertTo-Json -Compress", powerShellSingleQuoted(proofRoot))), sessiondCommandTimeout); err != nil || strings.TrimSpace(string(output)) != "false" {
		t.Fatalf("local proof control residue remains: output=%q err=%v", strings.TrimSpace(string(output)), err)
	}
	t.Logf("local Sessiond Runtime Input proof: run=%s session=%s screenshotBytes=%d sourceUnchanged=true zeroResidue=true", runID, proof.BrokerSession, proof.ScreenshotBytes)
}

type liveSessiondLaunchPaths struct {
	MayaExe   string `json:"maya_exe"`
	MCPSource string `json:"mcp_src"`
	MCPPython string `json:"mcp_python"`
}

func readLiveSessiondLaunchPaths(t *testing.T, host mayaHostConfig) liveSessiondLaunchPaths {
	t.Helper()
	configPath := remoteJoin(host.Broker.StateDir, "config.json")
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(fmt.Sprintf("Get-Content -LiteralPath %s -Raw", powerShellSingleQuoted(configPath))), sessiondCommandTimeout)
	if err != nil {
		t.Fatalf("read live Sessiond launch config: %v", err)
	}
	var paths liveSessiondLaunchPaths
	if err := json.Unmarshal(trimToJSON(raw), &paths); err != nil {
		t.Fatalf("parse live Sessiond launch config: %v", err)
	}
	if paths.MayaExe == "" || paths.MCPSource == "" {
		t.Fatalf("live Sessiond launch config lacks Maya/MCP paths")
	}
	if paths.MCPPython == "" {
		paths.MCPPython = host.Broker.Python
	}
	return paths
}

func buildLiveWindowsCandidate(t *testing.T, localRoot string) string {
	t.Helper()
	working, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Clean(filepath.Join(working, "..", ".."))
	output := filepath.Join(localRoot, "maya-stall.exe")
	command := exec.Command("go", "build", "-o", output, "./cmd/maya-stall")
	command.Dir = repoRoot
	command.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if raw, err := command.CombinedOutput(); err != nil {
		t.Fatalf("cross-build Windows live candidate: %v: %s", err, strings.TrimSpace(string(raw)))
	}
	return output
}

func writeLocalSessiondLiveFixture(t *testing.T, root, proofRoot, hostID, workRoot, stateDir, sessiondPython, sessiondRepo string, paths liveSessiondLaunchPaths, port int) {
	t.Helper()
	mustWriteFile(t, filepath.Join(root, ".maya-stall.yaml"), `version: 1
scenarios:
  local-runtime-input:
    mayaVersion: "2025"
    payload:
      mayaScripts: [scenario.py]
      runtimeInputs:
        scene:
          kind: file
          extensions: [.ma]
          destination: scenes/input.ma
    expectedOutputs:
      scenarioResult: scenario-result.json
      files: [outputs/runtime-input-proof.json]
    evidence:
      screenshots: {enabled: true}
    validators:
      - {type: outputExists, path: outputs/runtime-input-proof.json}
      - {type: visualEvidence, required: true}
`)
	mustWriteFile(t, filepath.Join(root, "hosts.yaml"), fmt.Sprintf(`version: 1
targetProfiles:
  local-live: {hostPool: local-workstation}
hostPools:
  local-workstation:
    hosts:
      - id: %s
        transport: local
        workRoot: '%s'
        mayaVersions: ["2025"]
        visualEvidence: true
        broker:
          type: gg-mayasessiond
          stateDir: '%s'
          python: '%s'
          repo: '%s'
          mcpSource: '%s'
          mcpPython: '%s'
          mayaExe: '%s'
          port: %d
`, hostID, workRoot, stateDir, strings.ReplaceAll(sessiondPython, "'", "''"), strings.ReplaceAll(sessiondRepo, "'", "''"), strings.ReplaceAll(paths.MCPSource, "'", "''"), strings.ReplaceAll(paths.MCPPython, "'", "''"), strings.ReplaceAll(paths.MayaExe, "'", "''"), port))
	mustWriteFile(t, filepath.Join(root, "scenario.py"), `import hashlib, json, os
from pathlib import Path
from maya import cmds
staged = Path(json.loads(os.environ["MAYA_STALL_RUNTIME_INPUTS"])["scene"])
before = hashlib.sha256(staged.read_bytes()).hexdigest()
cmds.file(str(staged), open=True, force=True, prompt=False)
cmds.createNode("transform", name="mayaStallLocalRuntimeInputProof")
cmds.file(rename=str(staged))
cmds.file(save=True, type="mayaAscii", force=True)
after = hashlib.sha256(staged.read_bytes()).hexdigest()
if before == after: raise RuntimeError("staged Runtime Input did not change")
root = Path(os.environ["MAYA_STALL_SCENARIO_RESULT"]).parent
(root / "outputs").mkdir(parents=True, exist_ok=True)
(root / "outputs" / "runtime-input-proof.json").write_text(json.dumps({"before": before, "after": after}), encoding="utf-8")
Path(os.environ["MAYA_STALL_SCENARIO_RESULT"]).write_text(json.dumps({"status": "passed", "summary": "local Runtime Input proof"}), encoding="utf-8")
`)
	mustWriteFile(t, filepath.Join(root, "input.ma"), "//Maya ASCII 2025 scene\nrequires maya \"2025\";\ncreateNode transform -n \"runtimeInputOriginal\";\n")
	mustWriteFile(t, filepath.Join(root, "monitor.ps1"), fmt.Sprintf(`$ErrorActionPreference = "Stop"
$state = %s
$lock = %s
$out = %s
$deadline = (Get-Date).AddMinutes(6)
while ((Get-Date) -lt $deadline) {
  if ((Test-Path -LiteralPath $state) -and (Test-Path -LiteralPath $lock)) {
    $s = Get-Content -LiteralPath $state -Raw | ConvertFrom-Json
    $l = Get-Content -LiteralPath $lock -Raw
    if ($s.status -eq "running" -and $l -match "brokerSessionId:\s*(\S+)") {
      $maya = Get-Process -Id $s.maya_pid -ErrorAction Stop
      [pscustomobject]@{session=[string]$s.session_id; lockSession=[string]$Matches[1]; lockRun=[regex]::Match($l,"activeRun:\s*(\S+)").Groups[1].Value; mayaPid=[int]$s.maya_pid; mayaSessionId=[int]$maya.SessionId} | ConvertTo-Json -Compress | Set-Content -Encoding UTF8 -LiteralPath $out
      exit 0
    }
  }
  Start-Sleep -Milliseconds 250
}
throw "timed out waiting for local Sessiond ownership"
`, powerShellSingleQuoted(remoteJoin(stateDir, "state.json")), powerShellSingleQuoted(remoteJoin(workRoot, "state", "locks", "hosts", "host.lock")), powerShellSingleQuoted(remoteJoin(proofRoot, "ownership.json"))))
}

func stageLocalSessiondLiveFixture(t *testing.T, host mayaHostConfig, localRoot, proofRoot, executable string) {
	t.Helper()
	configPath := filepath.Join(localRoot, "hosts.yaml")
	wrapper := fmt.Sprintf(`$ErrorActionPreference = "Stop"
$root = %s
$monitor = Start-Process powershell.exe -WindowStyle Hidden -PassThru -ArgumentList @("-NoProfile","-ExecutionPolicy","Bypass","-File",(Join-Path $root "monitor.ps1"))
try {
  Set-Location -LiteralPath $root
  & (Join-Path $root "maya-stall.exe") run --json --host-config (Join-Path $root "hosts.yaml") --target-profile local-live --input ("scene=" + (Join-Path $root "input.ma")) local-runtime-input 2>&1 | Set-Content -Encoding UTF8 -LiteralPath (Join-Path $root "run-output.jsonl")
  $code = $LASTEXITCODE
} finally {
  if (-not $monitor.HasExited) { $monitor.WaitForExit(30000) }
  [string]$code | Set-Content -Encoding ASCII -LiteralPath (Join-Path $root "exit-code.txt")
}
`, powerShellSingleQuoted(proofRoot))
	wrapperPath := filepath.Join(localRoot, "run.ps1")
	mustWriteFile(t, wrapperPath, wrapper)
	batch := newSFTPBatch()
	batch.mkdirAll(proofRoot)
	for _, item := range []struct{ local, remote string }{
		{executable, remoteJoin(proofRoot, "maya-stall.exe")},
		{filepath.Join(localRoot, ".maya-stall.yaml"), remoteJoin(proofRoot, ".maya-stall.yaml")},
		{configPath, remoteJoin(proofRoot, "hosts.yaml")},
		{filepath.Join(localRoot, "scenario.py"), remoteJoin(proofRoot, "scenario.py")},
		{filepath.Join(localRoot, "input.ma"), remoteJoin(proofRoot, "input.ma")},
		{filepath.Join(localRoot, "monitor.ps1"), remoteJoin(proofRoot, "monitor.ps1")},
		{wrapperPath, remoteJoin(proofRoot, "run.ps1")},
	} {
		batch.put(item.local, item.remote)
	}
	if err := runSFTPBatch(host, batch.String()); err != nil {
		t.Fatalf("stage local Sessiond live candidate: %v", err)
	}
}

func assertLiveProofPortFree(t *testing.T, host mayaHostConfig, port int) {
	t.Helper()
	script := fmt.Sprintf("if (Get-NetTCPConnection -State Listen -LocalPort %d -ErrorAction SilentlyContinue) { throw 'selected local proof port is in use' }", port)
	if _, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sessiondCommandTimeout); err != nil {
		t.Fatal(err)
	}
}

func startLocalSessiondLiveTask(t *testing.T, host mayaHostConfig, proofRoot, taskName string) {
	t.Helper()
	wrapper := remoteJoin(proofRoot, "run.ps1")
	script := fmt.Sprintf(`$ErrorActionPreference = "Stop"
$task = %s
cmd.exe /c "schtasks.exe /Delete /TN $task /F 2>NUL" | Out-Null
$start = (Get-Date).AddMinutes(1).ToString("HH:mm")
$command = 'powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + %s + '"'
& schtasks.exe /Create /TN $task /SC ONCE /ST $start /TR $command /RL LIMITED /IT /F | Out-Null
if ($LASTEXITCODE -ne 0) { throw "create interactive local proof task failed" }
& schtasks.exe /Run /TN $task | Out-Null
if ($LASTEXITCODE -ne 0) { throw "start interactive local proof task failed" }
`, powerShellSingleQuoted(taskName), powerShellSingleQuoted(wrapper))
	if _, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sessiondCommandTimeout); err != nil {
		t.Fatalf("start local Sessiond live task: %v", err)
	}
}

func waitForLocalSessiondLiveCompletion(t *testing.T, host mayaHostConfig, proofRoot string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	exitPath := remoteJoin(proofRoot, "exit-code.txt")
	for time.Now().Before(deadline) {
		output, err := runSSHCommandOutput(host, encodedPowerShellCommand(fmt.Sprintf("[bool](Test-Path -LiteralPath %s) | ConvertTo-Json -Compress", powerShellSingleQuoted(exitPath))), sessiondCommandTimeout)
		if err == nil && strings.TrimSpace(string(output)) == "true" {
			return
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("timed out waiting for local Sessiond live candidate")
}

func readRemoteText(t *testing.T, host mayaHostConfig, path string) string {
	t.Helper()
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(fmt.Sprintf("Get-Content -LiteralPath %s -Raw", powerShellSingleQuoted(path))), sessiondCommandTimeout)
	if err != nil {
		t.Fatalf("read remote proof file %s: %v", filepath.Base(path), err)
	}
	return strings.TrimPrefix(string(raw), "\ufeff")
}

func terminalLiveLocalRun(t *testing.T, output string) (string, string) {
	t.Helper()
	var runID, evidenceDir string
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &record) != nil || record["kind"] != "run" {
			continue
		}
		runID, _ = record["runId"].(string)
		evidenceDir, _ = record["evidenceDir"].(string)
	}
	if runID == "" || evidenceDir == "" {
		t.Fatalf("local Windows candidate produced no terminal Run JSON:\n%s", output)
	}
	return runID, filepath.ToSlash(evidenceDir)
}

type liveLocalSessiondProof struct {
	Status, Runtime, HostAdapter, BrokerSession, StoppedSession, LockSession, LockRun string
	InputName, InputKind, ScreenshotOrigin, SessiondStatus                            string
	InputSourcePresent, SourceUnchanged, StagedMutated                                bool
	ScreenshotPNG, ScreenshotHashMatch, ValidatorsPassed                              bool
	SessiondMayaAlive, SessiondMCPAlive, OwnedProcessAlive                            bool
	HostLockExists, RunRootExists, LocalRunStateExists, PortListening                 bool
	ScreenshotBytes, MayaSessionID                                                    int
}

func inspectLiveLocalSessiondProof(t *testing.T, host mayaHostConfig, proofRoot, workRoot, stateDir, consumerRoot, evidenceDir, runID string, port int) liveLocalSessiondProof {
	t.Helper()
	script := fmt.Sprintf(`$ErrorActionPreference = "Stop"
$e = %s
$bundle = Get-Content -LiteralPath (Join-Path $e "evidence.json") -Raw | ConvertFrom-Json
$manifest = Get-Content -LiteralPath (Join-Path $e "manifest.json") -Raw | ConvertFrom-Json
$input = $manifest.payload | Where-Object {$_.name -eq "scene"} | Select-Object -First 1
$output = Get-Content -LiteralPath (Join-Path $e "outputs/runtime-input-proof.json") -Raw | ConvertFrom-Json
$shot = $bundle.visualEvidence | Select-Object -First 1
$shotPath = Join-Path $e $shot.path
$bytes = [IO.File]::ReadAllBytes($shotPath)
Push-Location -LiteralPath %s
try { $status = & %s -m gg_maya_sessiond.cli status --state-dir %s --json | Out-String | ConvertFrom-Json } finally { Pop-Location }
$owner = Get-Content -LiteralPath %s -Raw | ConvertFrom-Json
$ownedAlive = $false
foreach ($pidValue in @($status.state.daemon_pid,$status.state.maya_pid,$status.state.mcp_pid)) { if ($pidValue -and (Get-Process -Id $pidValue -ErrorAction SilentlyContinue)) { $ownedAlive = $true } }
$validatorsPassed = @($bundle.validators | Where-Object {$_.status -ne "passed"}).Count -eq 0
[pscustomobject]@{
 status=$bundle.status; runtime=$bundle.runtime.profile; hostAdapter=$bundle.runtime.hostAdapter; brokerSession=$bundle.brokerSession.sessionId; stoppedSession=$status.state.session_id
 lockSession=$owner.lockSession; lockRun=$owner.lockRun; mayaSessionId=$owner.mayaSessionId
 inputName=$input.name; inputKind=$input.kind; inputSourcePresent=($input.PSObject.Properties.Name -contains "source")
 sourceUnchanged=((Get-FileHash -Algorithm SHA256 -LiteralPath %s).Hash.ToLower() -eq $input.sha256); stagedMutated=($output.before -ne $output.after)
 screenshotOrigin=$shot.origin; screenshotBytes=$bytes.Length; screenshotPNG=([BitConverter]::ToString($bytes[0..7]) -eq "89-50-4E-47-0D-0A-1A-0A"); screenshotHashMatch=((Get-FileHash -Algorithm SHA256 -LiteralPath $shotPath).Hash.ToLower() -eq $shot.sha256)
 validatorsPassed=$validatorsPassed; sessiondStatus=$status.derived_status; sessiondMayaAlive=[bool]$status.process_alive.maya; sessiondMCPAlive=[bool]$status.process_alive.mcp; ownedProcessAlive=$ownedAlive
 hostLockExists=(Test-Path -LiteralPath %s); runRootExists=(Test-Path -LiteralPath %s); localRunStateExists=(Test-Path -LiteralPath %s); portListening=[bool](Get-NetTCPConnection -State Listen -LocalPort %d -ErrorAction SilentlyContinue)
} | ConvertTo-Json -Compress
`, powerShellSingleQuoted(evidenceDir), powerShellSingleQuoted(host.Broker.Repo), powerShellSingleQuoted(host.Broker.Python), powerShellSingleQuoted(stateDir), powerShellSingleQuoted(remoteJoin(proofRoot, "ownership.json")), powerShellSingleQuoted(remoteJoin(proofRoot, "input.ma")), powerShellSingleQuoted(remoteJoin(workRoot, "state", "locks", "hosts", "host.lock")), powerShellSingleQuoted(remoteJoin(workRoot, "runs", runID)), powerShellSingleQuoted(remoteJoin(consumerRoot, ".maya-stall", "state", "runs", runID)), port)
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sessiondCommandTimeout)
	if err != nil {
		t.Fatalf("inspect local Sessiond live proof: %v", err)
	}
	var proof liveLocalSessiondProof
	if err := json.Unmarshal(trimToJSON(raw), &proof); err != nil {
		t.Fatalf("parse local Sessiond live proof: %v: %s", err, strings.TrimSpace(string(raw)))
	}
	return proof
}

func cleanupLocalSessiondLiveProof(t *testing.T, host mayaHostConfig, proofRoot, taskName string) {
	t.Helper()
	script := fmt.Sprintf(`$ErrorActionPreference = "Continue"
$task = %s
$proofRoot = %s
$stateDir = Join-Path $proofRoot "sessiond-state"
cmd.exe /c "schtasks.exe /End /TN $task 2>NUL" | Out-Null
Push-Location -LiteralPath %s
try { & %s -m gg_maya_sessiond.cli stop --state-dir $stateDir --wait-timeout-seconds 120 --json | Out-Null } finally { Pop-Location }
$candidatePath = Join-Path $proofRoot "maya-stall.exe"
Get-CimInstance Win32_Process | Where-Object { $_.ExecutablePath -eq $candidatePath } | ForEach-Object { Invoke-CimMethod -InputObject $_ -MethodName Terminate | Out-Null }
cmd.exe /c "schtasks.exe /Delete /TN $task /F 2>NUL" | Out-Null
if (Test-Path -LiteralPath %s) { Remove-Item -LiteralPath %s -Recurse -Force }
`, powerShellSingleQuoted(taskName), powerShellSingleQuoted(proofRoot), powerShellSingleQuoted(host.Broker.Repo), powerShellSingleQuoted(host.Broker.Python), powerShellSingleQuoted(proofRoot), powerShellSingleQuoted(proofRoot))
	if _, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sessiondCommandTimeout); err != nil {
		t.Errorf("cleanup local Sessiond live proof: %v", err)
	}
}
