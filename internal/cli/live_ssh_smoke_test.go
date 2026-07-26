package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const smokeHostConfigEnv = "MAYA_STALL_SMOKE_HOST_CONFIG"
const smokeTargetProfileEnv = "MAYA_STALL_SMOKE_TARGET_PROFILE"
const smokeHostEnv = "MAYA_STALL_SMOKE_HOST"
const consumingRepoSmokeDirEnv = "MAYA_STALL_CONSUMING_REPO_SMOKE_DIR"

func TestOptInRealSSHDoctorSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	dir := writeRunConfigFixture(t)
	restoreLiveSessionBrokerFixtures(t, options)
	report := runDoctor(dir, options.doctorOptions())
	assertLiveHostHealthProof(t, report)
	t.Logf("Host Health: %s", formatHostHealthReport(report))
	var stdout, stderr bytes.Buffer

	code := Run(options.doctorArgs(), &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("real SSH smoke doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func TestOptInRealPreRunReadinessSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	dir := writeRunConfigFixture(t)
	options.Host = liveSmokeHostForContention(t, options)
	selected := liveSmokeHostConfigByID(t, options, options.Host)
	probeHost := selected
	probeHost.Broker.StateDir = remoteJoin(selected.WorkRoot, "state", "proof", fmt.Sprintf("pre-run-readiness-missing-%d", time.Now().UnixNano()))
	runtime := defaultRunRuntime()
	runtime.ReadinessHost = realSSHHost{host: probeHost}
	runtime.ReadinessBroker = ggMayaSessiondBroker{host: probeHost}
	var stdout, stderr bytes.Buffer

	code := RunWithRuntime(options.runArgs("smoke"), &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("real pre-run readiness exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"pre-run readiness failed at session-broker layer for Maya Host " + selected.ID,
		"gg_mayasessiond is not running",
		"docs/setup/windows-maya-host.md#session-broker",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("real pre-run readiness stderr missing %q:\n%s", want, stderr.String())
		}
	}
	runID := outputValue(t, stdout.String(), "run")
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("real pre-run readiness Run State missing: %v", err)
	}
	bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", runID))
	if bundle.Version != evidenceSchemaVersion || bundle.Failure == nil || bundle.Failure.FailedLayer != "remote-check" || bundle.Failure.CaptureState != "not-started" || bundle.Failure.CleanupState != "completed" {
		t.Fatalf("real pre-run readiness minimal Evidence Bundle = %+v", bundle)
	}
	if bundle.ScenarioResult != "" || len(bundle.VisualEvidence) != 0 {
		t.Fatalf("real pre-run readiness unexpectedly reached Scenario evidence: %+v", bundle)
	}
	lockLayer := remoteHostLockLayer(selected)
	if lockLayer.Status != "ok" || lockLayer.State != "unlocked" {
		t.Fatalf("real pre-run readiness did not release Host Lock: %+v", lockLayer)
	}
	t.Log("real SSH reachability passed; real Session Broker status failed closed before staging; minimal evidence retained; Host Lock released")
}

func TestOptInRealSSHConsumingRepoSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	restoreLiveSessionBrokerFixtures(t, options)
	runKLVPushConsumingRepoSmoke(t, options)
}

func TestOptInRealSSHRunSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	dir := writeLiveRunConfigFixture(t)
	restoreLiveSessionBrokerFixtures(t, options)

	doctorOptions := options.doctorOptions()
	doctorOptions.ScenarioName = "smoke"
	report := runDoctor(dir, doctorOptions)
	assertLiveHostHealthProof(t, report)
	t.Logf("Host Health: %s", formatHostHealthReport(report))

	var runStdout, runStderr bytes.Buffer
	runCode := Run(options.runArgs("smoke"), &runStdout, &runStderr, dir, "test-version")
	if runCode != 0 {
		t.Fatalf("real SSH smoke run exit code = %d, want 0; stdout: %s stderr: %s", runCode, runStdout.String(), runStderr.String())
	}
	evidenceDir := smokeOutputValue(runStdout.String(), "evidence")
	if evidenceDir == "" {
		t.Fatalf("real SSH smoke run did not print Evidence Bundle path:\n%s", runStdout.String())
	}
	bundle := assertLiveSmokeEvidenceBundle(t, evidenceDir)
	if bundle.Scenario != "smoke" {
		t.Fatalf("Evidence Bundle scenario = %q, want smoke", bundle.Scenario)
	}
	options.Host = bundle.Host
	host := liveSmokeHostConfigByID(t, options, bundle.Host)
	requireLiveScenarioRecordingArtifact(t, evidenceDir, bundle)
	assertLiveFreshRunSessionStopped(t, host, bundle.BrokerSession)

	var secondRunStdout, secondRunStderr bytes.Buffer
	secondRunCode := Run(options.runArgs("smoke"), &secondRunStdout, &secondRunStderr, dir, "test-version")
	if secondRunCode != 0 {
		t.Fatalf("second real SSH smoke run exit code = %d, want 0; stdout: %s stderr: %s", secondRunCode, secondRunStdout.String(), secondRunStderr.String())
	}
	secondEvidenceDir := smokeOutputValue(secondRunStdout.String(), "evidence")
	if secondEvidenceDir == "" {
		t.Fatalf("second real SSH smoke run did not print Evidence Bundle path:\n%s", secondRunStdout.String())
	}
	secondBundle := assertLiveSmokeEvidenceBundle(t, secondEvidenceDir)
	if secondBundle.BrokerSession.SessionID == bundle.BrokerSession.SessionID {
		t.Fatalf("two consecutive Fresh Runs reused broker session %q", bundle.BrokerSession.SessionID)
	}
	assertLiveFreshRunSessionStopped(t, host, secondBundle.BrokerSession)

	var keptStdout, keptStderr bytes.Buffer
	keptCode := Run(options.runArgs("retention-failure", "--keep-on-failure"), &keptStdout, &keptStderr, dir, "test-version")
	if keptCode != 1 {
		t.Fatalf("real SSH retention run exit code = %d, want 1; stdout: %s stderr: %s", keptCode, keptStdout.String(), keptStderr.String())
	}
	runID := smokeOutputValue(keptStdout.String(), "run")
	if runID == "" || !strings.Contains(keptStdout.String(), "stopPolicy: kept") {
		t.Fatalf("real SSH retention run did not keep failed run:\nstdout: %s\nstderr: %s", keptStdout.String(), keptStderr.String())
	}

	var statusStdout, statusStderr bytes.Buffer
	statusCode := Run([]string{"status", "--run", runID}, &statusStdout, &statusStderr, dir, "test-version")
	if statusCode != 0 {
		t.Fatalf("real SSH retention status exit code = %d, want 0; stdout: %s stderr: %s", statusCode, statusStdout.String(), statusStderr.String())
	}
	if !strings.Contains(statusStdout.String(), "state: kept") || !strings.Contains(statusStdout.String(), "brokerSession:") {
		t.Fatalf("real SSH retention status missing kept broker session:\n%s", statusStdout.String())
	}

	var attachStdout, attachStderr bytes.Buffer
	attachCode := Run([]string{"attach", runID}, &attachStdout, &attachStderr, dir, "test-version")
	if attachCode != 0 {
		t.Fatalf("real SSH retention attach exit code = %d, want 0; stdout: %s stderr: %s", attachCode, attachStdout.String(), attachStderr.String())
	}
	if !strings.Contains(attachStdout.String(), "brokerReport:") || !strings.Contains(attachStdout.String(), "intentional retention smoke failure") {
		t.Fatalf("real SSH retention attach missing broker/local evidence:\n%s", attachStdout.String())
	}

	var stopStdout, stopStderr bytes.Buffer
	stopCode := Run([]string{"stop", runID}, &stopStdout, &stopStderr, dir, "test-version")
	if stopCode != 0 {
		t.Fatalf("real SSH retention stop exit code = %d, want 0; stdout: %s stderr: %s", stopCode, stopStdout.String(), stopStderr.String())
	}
	if !strings.Contains(stopStdout.String(), "stopped: "+runID) {
		t.Fatalf("real SSH retention stop missing run id:\n%s", stopStdout.String())
	}
	restoreLiveSessionBrokerFixture(t, host)
	t.Run("shared Host Agent path", runOptInRealSharedHostAgentRunSmoke)
	t.Run("shared Kept Session deadline path", runOptInRealSharedKeptSessionDeadlineSmoke)
}

func TestOptInRealHostLockContentionAndRecoverySmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	options.Host = liveSmokeHostForContention(t, options)
	host := liveSmokeHostConfigByID(t, options, options.Host)
	restoreLiveSessionBrokerFixture(t, host)
	t.Cleanup(func() { restoreLiveSessionBrokerFixture(t, host) })

	holderDir := t.TempDir()
	contenderDir := t.TempDir()
	readyPath := filepath.Join(t.TempDir(), "holder-ready")
	crashPath := filepath.Join(t.TempDir(), "holder-crash")
	holder := startHostLockController(t, options, holderDir, "hold-crash", "contention-holder", readyPath, crashPath)
	var holderOutput bytes.Buffer
	holder.Stdout = &holderOutput
	holder.Stderr = &holderOutput
	if err := holder.Start(); err != nil {
		t.Fatalf("start holder controller: %v", err)
	}
	holderWaited := false
	t.Cleanup(func() {
		_ = os.WriteFile(crashPath, []byte("cleanup\n"), 0o600)
		if holder.Process != nil && !holderWaited {
			_ = holder.Process.Kill()
			_ = holder.Wait()
			holderWaited = true
		}
		broker := ggMayaSessiondBroker{host: host}
		if status, err := broker.status(); err == nil && sessiondSessionLooksActive(status) {
			if err := broker.stopSessiondSession(); err != nil {
				t.Errorf("cleanup Host Lock smoke Session Broker: %v", err)
			}
		}
		if err := releaseHostSideLock(host, "contention-holder"); err != nil && !errors.Is(err, errHostLockOwnershipChanged) {
			t.Errorf("cleanup crashed holder Host Lock: %v", err)
		}
	})
	waitForFile(t, readyPath, 45*time.Second)

	runHostLockController(t, options, contenderDir, "expect-locked", "contention-contender", "", "")
	if err := os.WriteFile(crashPath, []byte("crash\n"), 0o600); err != nil {
		t.Fatalf("signal controller crash: %v", err)
	}
	if err := holder.Wait(); err != nil {
		holderWaited = true
		t.Fatalf("holder controller did not crash cleanly: %v: %s", err, strings.TrimSpace(holderOutput.String()))
	}
	holderWaited = true

	broker := ggMayaSessiondBroker{host: host}
	status, err := broker.status()
	if err != nil {
		t.Fatalf("inspect Session Broker before lease recovery: %v", err)
	}
	if sessiondSessionLooksActive(status) {
		if err := broker.stopSessiondSession(); err != nil {
			t.Fatalf("stop Session Broker before lease recovery: %v", err)
		}
	}
	status, err = broker.status()
	if err != nil || sessiondSessionLooksActive(status) {
		t.Fatalf("Session Broker was not inactive before lease recovery: active=%t err=%v", sessiondSessionLooksActive(status), err)
	}
	time.Sleep(3 * time.Second)
	runHostLockController(t, options, contenderDir, "acquire-release", "lease-recovery-successor", "", "")
}

func TestHostLockControllerHelper(t *testing.T) {
	if os.Getenv("MAYA_STALL_HOST_LOCK_HELPER") != "1" {
		t.Skip("controller subprocess only")
	}
	lease, err := time.ParseDuration(os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_LEASE"))
	if err != nil {
		t.Fatalf("parse helper lease: %v", err)
	}
	hostSideLockLeaseDuration = lease
	hostSideLockHeartbeatInterval = hostLockContentionHeartbeatInterval(lease)
	config, err := loadUserHostConfig(os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_CONFIG"))
	if err != nil {
		t.Fatalf("load helper Host Config: %v", err)
	}
	candidates, err := hostCandidates(config, os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_PROFILE"), os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_HOST"))
	if err != nil || len(candidates) != 1 {
		t.Fatalf("resolve helper Maya Host: candidates=%d err=%v", len(candidates), err)
	}
	lock, locked, err := acquireRunHostLock(os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_REPO"), candidates[0])
	action := os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_ACTION")
	if action == "expect-locked" {
		if err != nil || !locked {
			t.Fatalf("contender Host Lock result: locked=%t err=%v", locked, err)
		}
		return
	}
	if err != nil || locked {
		t.Fatalf("acquire helper Host Lock: locked=%t err=%v", locked, err)
	}
	if err := lock.markActive(os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_RUN")); err != nil {
		t.Fatalf("mark helper Host Lock active: %v", err)
	}
	if action == "acquire-release" {
		if err := lock.release(); err != nil {
			t.Fatalf("release recovered Host Lock: %v", err)
		}
		return
	}
	if action != "hold-crash" {
		t.Fatalf("unknown helper action %q", action)
	}
	stopHeartbeat, _ := startHostLockHeartbeat(lock.renew)
	if err := os.WriteFile(os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_READY"), []byte("ready\n"), 0o600); err != nil {
		t.Fatalf("write helper readiness: %v", err)
	}
	waitForFile(t, os.Getenv("MAYA_STALL_HOST_LOCK_HELPER_CRASH"), 2*time.Minute)
	if err := stopHeartbeat(); err != nil {
		t.Fatalf("renew holder Host Lock: %v", err)
	}
	os.Exit(0)
}

func hostLockContentionHeartbeatInterval(lease time.Duration) time.Duration {
	return lease / 4
}

func startHostLockController(t *testing.T, options realSSHSmokeOptions, repoDir string, action string, runID string, readyPath string, crashPath string) *exec.Cmd {
	t.Helper()
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, testBinary, "-test.run=^TestHostLockControllerHelper$", "-test.count=1")
	command.Env = append(os.Environ(),
		"MAYA_STALL_HOST_LOCK_HELPER=1",
		"MAYA_STALL_HOST_LOCK_HELPER_LEASE=2s",
		"MAYA_STALL_HOST_LOCK_HELPER_CONFIG="+options.HostConfig,
		"MAYA_STALL_HOST_LOCK_HELPER_PROFILE="+options.TargetProfile,
		"MAYA_STALL_HOST_LOCK_HELPER_HOST="+options.Host,
		"MAYA_STALL_HOST_LOCK_HELPER_REPO="+repoDir,
		"MAYA_STALL_HOST_LOCK_HELPER_ACTION="+action,
		"MAYA_STALL_HOST_LOCK_HELPER_RUN="+runID,
		"MAYA_STALL_HOST_LOCK_HELPER_READY="+readyPath,
		"MAYA_STALL_HOST_LOCK_HELPER_CRASH="+crashPath,
	)
	return command
}

func runHostLockController(t *testing.T, options realSSHSmokeOptions, repoDir string, action string, runID string, readyPath string, crashPath string) {
	t.Helper()
	command := startHostLockController(t, options, repoDir, action, runID, readyPath, crashPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Host Lock controller %s failed: %v: %s", action, err, strings.TrimSpace(string(output)))
	}
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", filepath.Base(path))
}

func liveSmokeHostForContention(t *testing.T, options realSSHSmokeOptions) string {
	t.Helper()
	config, err := loadUserHostConfig(options.HostConfig)
	if err != nil {
		t.Fatalf("load live Host Config: %v", err)
	}
	candidates, err := hostCandidates(config, options.TargetProfile, options.Host)
	if err != nil {
		t.Fatalf("resolve live Host Pool: %v", err)
	}
	for _, candidate := range candidates {
		resolved, resolveErr := resolveRuntimeForHost(candidate)
		if resolveErr == nil && isHealthyHost(candidate) && candidate.usesRealSSH() && resolved.Metadata.LiveProofEligible {
			return candidate.ID
		}
	}
	t.Fatalf("no healthy real SSH Maya Host available for Target Profile %q", options.TargetProfile)
	return ""
}

func runKLVPushConsumingRepoSmoke(t *testing.T, options realSSHSmokeOptions) {
	t.Helper()
	consumingRepoDir := os.Getenv(consumingRepoSmokeDirEnv)
	if consumingRepoDir == "" {
		t.Fatalf("%s is not set; live proof requires a real consuming repo checkout", consumingRepoSmokeDirEnv)
	}
	dir := writeKLVPushConsumingRepoSmokeFixture(t, consumingRepoDir)

	doctorOptions := options.doctorOptions()
	doctorOptions.ScenarioName = "klv-push-smoke"
	report := runDoctor(dir, doctorOptions)
	assertLiveHostHealthProof(t, report)
	t.Logf("Host Health: %s", formatHostHealthReport(report))

	var runStdout, runStderr bytes.Buffer
	runCode := Run(options.runArgs("klv-push-smoke"), &runStdout, &runStderr, dir, "test-version")
	if runCode != 0 {
		t.Fatalf("real consuming repo smoke run exit code = %d, want 0; stdout: %s stderr: %s", runCode, runStdout.String(), runStderr.String())
	}
	evidenceDir := smokeOutputValue(runStdout.String(), "evidence")
	if evidenceDir == "" {
		t.Fatalf("real consuming repo smoke did not print Evidence Bundle path:\n%s", runStdout.String())
	}
	bundle := assertLiveSmokeEvidenceBundle(t, evidenceDir)
	if bundle.Scenario != "klv-push-smoke" {
		t.Fatalf("Evidence Bundle scenario = %q, want klv-push-smoke", bundle.Scenario)
	}
	assertKLVPushScenarioResult(t, evidenceDir, bundle.ScenarioResult)

	storeDir := filepath.Join(t.TempDir(), "evidence-store")
	var publishStdout, publishStderr bytes.Buffer
	publishCode := Run([]string{
		"evidence", "publish",
		"--destination", storeDir,
		"--base-url", "https://evidence.example.invalid/maya-stall",
		evidenceDir,
	}, &publishStdout, &publishStderr, dir, "test-version")
	if publishCode != 0 {
		t.Fatalf("evidence publish exit code = %d, want 0; stdout: %s stderr: %s", publishCode, publishStdout.String(), publishStderr.String())
	}
	for _, name := range []string{"artifact-manifest.json", "review-comment.md"} {
		if _, err := os.Stat(filepath.Join(storeDir, bundle.RunID, name)); err != nil {
			t.Fatalf("published Evidence Store missing %s: %v", name, err)
		}
	}
	if !strings.Contains(publishStdout.String(), "url: https://evidence.example.invalid/maya-stall/") {
		t.Fatalf("evidence publish did not print review-ready URL:\n%s", publishStdout.String())
	}
}

func TestKLVPushConsumingRepoSmokeFixtureUsesBuiltPluginArtifact(t *testing.T) {
	source := t.TempDir()
	mustWriteFile(t, filepath.Join(source, "pyproject.toml"), "[project]\nname = \"gg-klv-push\"\n")
	mustWriteFile(t, filepath.Join(source, "packages", "klv_push", "README.md"), "# KLV Push\n")
	mustWriteFile(t, filepath.Join(source, "packages", "klv_push", "pyproject.toml"), "[project]\nname = \"klv-push\"\n")
	mustWriteFile(t, filepath.Join(source, "src", "klv_push", "__init__.py"), "\"\"\"test\"\"\"\n")
	mustWriteFile(t, filepath.Join(source, "src", "klv_push", "klvPush.py"), "NODE_NAME = 'klvPush'\n")

	dir := writeKLVPushConsumingRepoSmokeFixture(t, source)
	config, _, err := loadRepoRunConfig(dir)
	if err != nil {
		t.Fatalf("load generated Repo Run Config: %v", err)
	}
	scenario := config.Scenarios["klv-push-smoke"]
	if scenario.MayaVersion != "" || scenario.Requirements.Maya.Minimum != "2025" {
		t.Fatalf("Maya requirement = %+v with legacy version %q, want minimum 2025", scenario.Requirements.Maya, scenario.MayaVersion)
	}
	if len(scenario.Payload.PluginArtifacts) != 1 || scenario.Payload.PluginArtifacts[0] != "build/maya2025/Release/klv_push" {
		t.Fatalf("pluginArtifacts = %#v, want nested klv_push artifact", scenario.Payload.PluginArtifacts)
	}
	if len(scenario.Payload.IncludePaths) != 1 || scenario.Payload.IncludePaths[0] != "build/maya2025/Release/klv_push/scripts" {
		t.Fatalf("includePaths = %#v, want built package scripts", scenario.Payload.IncludePaths)
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "maya2025", "Release", "klv_push", "scripts", "klv_push", "klvPush.py")); err != nil {
		t.Fatalf("built Plugin Artifact missing klvPush.py: %v", err)
	}
	scriptBytes, err := os.ReadFile(filepath.Join(dir, "maya", "klv_push_smoke.py"))
	if err != nil {
		t.Fatalf("read Maya Script: %v", err)
	}
	for _, want := range []string{
		`os.environ["MAYA_STALL_TRUSTED_PLUGIN_ARTIFACTS_ROOT"]`,
		"cmds.unloadPlugin(loaded_plugin, force=True)",
		"cmds.loadPlugin(plugin_path, quiet=True)",
		"cmds.deformer(marker, type=plugin_node_type)",
	} {
		if !strings.Contains(string(scriptBytes), want) {
			t.Fatalf("Maya Script does not exercise real plug-in loading, missing %q", want)
		}
	}
}

type realSSHSmokeOptions struct {
	HostConfig    string
	TargetProfile string
	Host          string
}

func realSSHSmokeOptionsFromEnv(t *testing.T) (realSSHSmokeOptions, bool) {
	t.Helper()
	hostConfig, ok := os.LookupEnv(smokeHostConfigEnv)
	if !ok || hostConfig == "" {
		t.Skip(smokeHostConfigEnv + " is not set; skipping opt-in real SSH smoke")
		return realSSHSmokeOptions{}, false
	}
	options := realSSHSmokeOptions{HostConfig: hostConfig, TargetProfile: "default"}
	if value, ok := os.LookupEnv(smokeTargetProfileEnv); ok && value != "" {
		options.TargetProfile = value
	}
	if value, ok := os.LookupEnv(smokeHostEnv); ok && value != "" {
		options.Host = value
	}
	return options, true
}

func (options realSSHSmokeOptions) doctorArgs(extra ...string) []string {
	args := []string{"doctor", "--host-config", options.HostConfig, "--target-profile", options.TargetProfile}
	if options.Host != "" {
		args = append(args, "--host", options.Host)
	}
	return append(args, extra...)
}

func (options realSSHSmokeOptions) doctorOptions() doctorOptions {
	return doctorOptions{HostConfig: options.HostConfig, TargetProfile: options.TargetProfile, HostPin: options.Host}
}

func (options realSSHSmokeOptions) runArgs(scenario string, extra ...string) []string {
	args := []string{"run", "--host-config", options.HostConfig, "--target-profile", options.TargetProfile}
	if options.Host != "" {
		args = append(args, "--host", options.Host)
	}
	args = append(args, extra...)
	return append(args, scenario)
}

func (options realSSHSmokeOptions) recordArgs(extra ...string) []string {
	args := []string{"record", "--host-config", options.HostConfig, "--target-profile", options.TargetProfile}
	if options.Host != "" {
		args = append(args, "--host", options.Host)
	}
	return append(args, extra...)
}

func (options realSSHSmokeOptions) screenshotArgs(extra ...string) []string {
	args := []string{"screenshot", "--host-config", options.HostConfig, "--target-profile", options.TargetProfile}
	if options.Host != "" {
		args = append(args, "--host", options.Host)
	}
	return append(args, extra...)
}

func (options realSSHSmokeOptions) controlClickArgs(x int, y int, extra ...string) []string {
	args := []string{
		"control", "click",
		"--host-config", options.HostConfig,
		"--target-profile", options.TargetProfile,
	}
	if options.Host != "" {
		args = append(args, "--host", options.Host)
	}
	args = append(args, "--x", strconv.Itoa(x), "--y", strconv.Itoa(y))
	return append(args, extra...)
}

func assertLiveHostHealthProof(t *testing.T, report hostHealthReport) {
	t.Helper()
	if !report.Healthy {
		t.Fatalf("Host Health is not healthy: %s", formatHostHealthReport(report))
	}
	if report.Runtime.Profile != "ssh-sessiond" || report.Runtime.HostAdapter != "ssh" || report.Runtime.BrokerAdapter != "gg-mayasessiond" || !report.Runtime.LiveProofEligible {
		t.Fatalf("Host Health runtime = %+v, want live-proof-eligible ssh-sessiond", report.Runtime)
	}
	broker := requireHostHealthLayer(t, report, "session-broker")
	if broker.Status != "ok" || broker.Source != "gg-mayasessiond" || !broker.InteractiveDesktop {
		t.Fatalf("session-broker Host Health = %+v, want interactive gg_mayasessiond", broker)
	}
	visual := requireHostHealthLayer(t, report, "visual-evidence")
	if visual.Status != "ok" || visual.Source != "session-broker" || !strings.Contains(visual.Detail, "viewport.capture") {
		t.Fatalf("visual-evidence Host Health = %+v, want broker-backed viewport.capture", visual)
	}
}

func writeLiveRunConfigFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "maya", "smoke.py"), "print('maya-stall live smoke')\n")
	mustWriteFile(t, filepath.Join(dir, "maya", "retention_failure.py"), "raise RuntimeError('intentional retention smoke failure')\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    description: "Live Maya Host smoke Scenario."
    requirements:
      maya:
        minimum: "2025"
    payload:
      scripts:
        - "maya/smoke.py"
    expectedOutputs:
      files: []
      scenarioResult: "outputs/smoke-result.json"
    evidence:
      screenshots:
        enabled: true
      recording:
        enabled: true
    validators:
      - type: scenarioResultStatus
        status: passed
      - type: visualEvidence
  retention-failure:
    description: "Live Maya Host retention failure Scenario."
    requirements:
      maya:
        minimum: "2025"
    payload:
      scripts:
        - "maya/retention_failure.py"
    expectedOutputs:
      files: []
      scenarioResult: "outputs/retention-result.json"
    evidence:
      screenshots:
        enabled: false
    validators:
      - type: scenarioResultStatus
        status: passed
`)
	return dir
}

func smokeOutputValue(output string, key string) string {
	prefix := key + ": "
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, prefix); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func assertLiveSmokeEvidenceBundle(t *testing.T, evidenceDir string) evidenceBundle {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(evidenceDir, "evidence.json"))
	if err != nil {
		t.Fatalf("read Evidence Bundle: %v", err)
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		t.Fatalf("parse Evidence Bundle: %v", err)
	}
	if bundle.Status != resultStatusPassed {
		t.Fatalf("Evidence Bundle status = %q, want %q", bundle.Status, resultStatusPassed)
	}
	if bundle.Runtime.Profile != "ssh-sessiond" || bundle.Runtime.HostAdapter != "ssh" || bundle.Runtime.BrokerAdapter != "gg-mayasessiond" || !bundle.Runtime.LiveProofEligible {
		t.Fatalf("Evidence Bundle runtime = %+v, want live-proof-eligible ssh-sessiond", bundle.Runtime)
	}
	if bundle.BrokerSession == nil || bundle.BrokerSession.BrokerAdapter != "gg-mayasessiond" || bundle.BrokerSession.SessionID == "" {
		t.Fatalf("Evidence Bundle broker session = %+v, want identified gg-mayasessiond Maya UI Session", bundle.BrokerSession)
	}
	if len(bundle.VisualEvidence) == 0 {
		t.Fatalf("Evidence Bundle missing Visual Evidence")
	}
	for _, relative := range []string{bundle.Manifest, bundle.Events, bundle.Log, bundle.ScenarioResult} {
		if relative == "" {
			t.Fatalf("Evidence Bundle has empty required path")
		}
		if _, err := os.Stat(filepath.Join(evidenceDir, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("Evidence Bundle missing %s: %v", relative, err)
		}
	}
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Kind == "" || artifact.Path == "" || artifact.MediaType == "" {
			t.Fatalf("Visual Evidence artifact incomplete: %+v", artifact)
		}
		if artifact.TargetProfile != bundle.TargetProfile || artifact.Host != bundle.Host {
			t.Fatalf("Visual Evidence target metadata = %+v, want Target Profile %q and Maya Host %q", artifact, bundle.TargetProfile, bundle.Host)
		}
		if artifact.Origin != visualEvidenceOriginBrokerCapture {
			t.Fatalf("Visual Evidence artifact origin = %q, want Session Broker capture provenance: %+v", artifact.Origin, artifact)
		}
		visualPath := filepath.Join(evidenceDir, filepath.FromSlash(artifact.Path))
		content, err := os.ReadFile(visualPath)
		if err != nil {
			t.Fatalf("Visual Evidence artifact missing %s: %v", artifact.Path, err)
		}
		if len(content) == 0 {
			t.Fatalf("Visual Evidence artifact %s is empty", artifact.Path)
		}
		if artifact.SHA256 != sha256HexOfBytes(content) {
			t.Fatalf("Visual Evidence artifact %s sha256 provenance = %q, want %q", artifact.Path, artifact.SHA256, sha256HexOfBytes(content))
		}
		switch artifact.Kind {
		case "recording":
			if artifact.MediaType != "video/mp4" {
				t.Fatalf("recording Visual Evidence media type = %q, want video/mp4", artifact.MediaType)
			}
			if artifact.DurationSeconds <= 0 || artifact.FPS <= 0 {
				t.Fatalf("recording Visual Evidence missing duration/FPS metadata: %+v", artifact)
			}
			if !looksLikeMP4Bytes(content) {
				t.Fatalf("recording Visual Evidence artifact %s does not look like an MP4", artifact.Path)
			}
		default:
			if !looksLikeImageBytes(artifact.MediaType, content) {
				t.Fatalf("Visual Evidence artifact %s does not match media type %s", artifact.Path, artifact.MediaType)
			}
		}
	}
	if err := requireBrokerCaptureProvenanceEvents(evidenceDir, bundle); err != nil {
		t.Fatalf("Visual Evidence provenance events: %v", err)
	}
	return bundle
}

func assertLiveFreshRunSessionStopped(t *testing.T, host mayaHostConfig, session *brokerSessionIdentity) {
	t.Helper()
	if session == nil || session.SessionID == "" {
		t.Fatal("cannot verify stopped Fresh Run without broker session identity")
	}
	status, err := (ggMayaSessiondBroker{host: host}).status()
	if err != nil {
		t.Fatalf("query gg_mayasessiond after stopped Fresh Run %q: %v", session.SessionID, err)
	}
	if err := freshRunStoppedProofError(status, session); err != nil {
		t.Fatalf("stopped Fresh Run %q failed post-stop proof: %v", session.SessionID, err)
	}
}

func freshRunStoppedProofError(status sessiondStatusResult, session *brokerSessionIdentity) error {
	if session == nil || session.SessionID == "" {
		return fmt.Errorf("missing broker session identity")
	}
	if status.State.SessionID == "" {
		missingState := !status.HasState && strings.EqualFold(sessiondEffectiveStatus(status), "missing")
		stoppedTombstone := status.HasState && strings.EqualFold(status.DerivedStatus, "stopped") && strings.EqualFold(status.State.Status, "stopped")
		if !missingState && !stoppedTombstone {
			return fmt.Errorf("session identity disappeared without an inactive broker state")
		}
	} else if status.State.SessionID != session.SessionID {
		return fmt.Errorf("evidence session %q does not match status session %q", session.SessionID, status.State.SessionID)
	}
	if sessiondSessionLooksActive(status) {
		return fmt.Errorf("broker reports active session %q with status %q", status.State.SessionID, status.DerivedStatus)
	}
	for process, alive := range status.ProcessAlive {
		if alive {
			return fmt.Errorf("broker process %q remains alive", process)
		}
	}
	return nil
}

func TestFreshRunStoppedProof(t *testing.T) {
	tests := []struct {
		name      string
		status    sessiondStatusResult
		wantError bool
	}{
		{
			name:   "matching stopped tombstone",
			status: stoppedSessiondStatus("session-owned"),
		},
		{
			name:   "anonymous stopped tombstone",
			status: stoppedSessiondStatus(""),
		},
		{
			name:   "state removed after stop",
			status: missingSessiondStatus(),
		},
		{
			name:      "different stopped session",
			status:    stoppedSessiondStatus("session-other"),
			wantError: true,
		},
		{
			name:      "missing state with live process",
			status:    missingSessiondStatusWithLiveProcess(),
			wantError: true,
		},
		{
			name:      "matching active session",
			status:    activeSessiondStatus("session-owned"),
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := freshRunStoppedProofError(test.status, &brokerSessionIdentity{SessionID: "session-owned"})
			if (err != nil) != test.wantError {
				t.Fatalf("freshRunStoppedProofError() error = %v, wantError %t", err, test.wantError)
			}
		})
	}
}

func stoppedSessiondStatus(sessionID string) sessiondStatusResult {
	status := sessiondStatusResult{HasState: true, DerivedStatus: "stopped", ProcessAlive: map[string]bool{"daemon": false, "maya": false, "mcp": false}}
	status.State.Status = "stopped"
	status.State.SessionID = sessionID
	return status
}

func missingSessiondStatus() sessiondStatusResult {
	return sessiondStatusResult{DerivedStatus: "missing", ProcessAlive: map[string]bool{}}
}

func missingSessiondStatusWithLiveProcess() sessiondStatusResult {
	status := missingSessiondStatus()
	status.ProcessAlive["maya"] = true
	return status
}

func activeSessiondStatus(sessionID string) sessiondStatusResult {
	status := sessiondStatusResult{HasState: true, DerivedStatus: "running", ProcessAlive: map[string]bool{"daemon": true, "maya": true, "mcp": true}}
	status.State.Status = "running"
	status.State.SessionID = sessionID
	status.State.MayaAlive = true
	status.State.MCPAlive = true
	return status
}

func requireLiveScenarioRecordingArtifact(t *testing.T, evidenceDir string, bundle evidenceBundle) visualEvidenceArtifact {
	t.Helper()
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Kind != "recording" {
			continue
		}
		if artifact.MediaType != "video/mp4" || !strings.HasPrefix(cleanEvidenceArtifactPath(artifact.Path), evidenceRecordingsDir+"/") {
			t.Fatalf("Scenario recording artifact = %+v, want video/mp4 under recordings/", artifact)
		}
		if artifact.DurationSeconds <= 0 || artifact.FPS <= 0 {
			t.Fatalf("Scenario recording missing duration/FPS metadata: %+v", artifact)
		}
		if artifact.TargetProfile != bundle.TargetProfile || artifact.Host != bundle.Host {
			t.Fatalf("Scenario recording target metadata = %+v, want Target Profile %q and Maya Host %q", artifact, bundle.TargetProfile, bundle.Host)
		}
		content, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(artifact.Path)))
		if err != nil {
			t.Fatalf("read Scenario recording %s: %v", artifact.Path, err)
		}
		if !looksLikeMP4Bytes(content) {
			t.Fatalf("Scenario recording %s does not look like an MP4", artifact.Path)
		}
		return artifact
	}
	t.Fatalf("Evidence Bundle missing Scenario recording Visual Evidence: %+v", bundle.VisualEvidence)
	return visualEvidenceArtifact{}
}

func writeKLVPushConsumingRepoSmokeFixture(t *testing.T, sourceRepoDir string) string {
	t.Helper()
	sourceRepoDir, err := filepath.Abs(sourceRepoDir)
	if err != nil {
		t.Fatalf("resolve consuming repo path: %v", err)
	}
	for _, required := range []string{
		"pyproject.toml",
		"packages/klv_push",
		"src/klv_push/__init__.py",
		"src/klv_push/klvPush.py",
	} {
		if _, err := os.Stat(filepath.Join(sourceRepoDir, filepath.FromSlash(required))); err != nil {
			t.Fatalf("consuming repo missing %s: %v", required, err)
		}
	}

	dir := t.TempDir()
	artifactDir := filepath.Join(dir, "build", "maya2025", "Release", "klv_push")
	if err := copyPath(filepath.Join(sourceRepoDir, "packages", "klv_push"), artifactDir); err != nil {
		t.Fatalf("copy klv_push package artifact shell: %v", err)
	}
	if err := copyPath(filepath.Join(sourceRepoDir, "src", "klv_push"), filepath.Join(artifactDir, "scripts", "klv_push")); err != nil {
		t.Fatalf("build klv_push Plugin Artifact: %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, "pyproject.toml"), "[project]\nname = \"klv-push-consuming-smoke\"\n")
	mustWriteFile(t, filepath.Join(dir, "maya", "klv_push_smoke.py"), klvPushConsumingRepoSmokeScript())
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  klv-push-smoke:
    description: "KLV Push consuming repo smoke Scenario."
    requirements:
      maya:
        minimum: "2025"
    payload:
      scripts:
        - "maya/klv_push_smoke.py"
      pluginArtifacts:
        - "build/maya2025/Release/klv_push"
      includePaths:
        - "build/maya2025/Release/klv_push/scripts"
    expectedOutputs:
      files:
        - "outputs/klv-push-smoke-result.json"
      scenarioResult: "outputs/klv-push-smoke-result.json"
    evidence:
      screenshots:
        enabled: true
    validators:
      - type: scenarioResultStatus
        status: passed
      - type: visualEvidence
      - type: jsonEquals
        path: "outputs/klv-push-smoke-result.json"
        jsonPath: "$.pluginName"
        equals: "klv_push"
      - type: jsonEquals
        path: "outputs/klv-push-smoke-result.json"
        jsonPath: "$.import.ok"
        equals: true
      - type: jsonEquals
        path: "outputs/klv-push-smoke-result.json"
        jsonPath: "$.artifact.fromTrustedRoot"
        equals: true
      - type: jsonEquals
        path: "outputs/klv-push-smoke-result.json"
        jsonPath: "$.action.ok"
        equals: true
`)
	return dir
}

func klvPushConsumingRepoSmokeScript() string {
	return `import json
import os
import sys
import traceback

import maya.cmds as cmds

result_path = os.environ["MAYA_STALL_SCENARIO_RESULT"]
trusted_root = os.environ["MAYA_STALL_TRUSTED_PLUGIN_ARTIFACTS_ROOT"]
artifact_root = os.path.abspath(os.path.join(trusted_root, "build", "maya2025", "Release", "klv_push"))
package_scripts = os.path.join(artifact_root, "scripts")
plugin_path = os.path.join(package_scripts, "klv_push", "klvPush.py")

def write_result(status, summary, **fields):
    payload = {"status": status, "summary": summary}
    payload.update(fields)
    parent = os.path.dirname(result_path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    with open(result_path, "w", encoding="utf-8") as handle:
        json.dump(payload, handle, indent=2, sort_keys=True)
        handle.write("\n")

try:
    cmds.file(new=True, force=True)
    target_plugin_path = os.path.normcase(os.path.abspath(plugin_path))
    for loaded_plugin in cmds.pluginInfo(query=True, listPlugins=True) or []:
        loaded_plugin_path = cmds.pluginInfo(loaded_plugin, query=True, path=True)
        if loaded_plugin_path and os.path.normcase(os.path.abspath(loaded_plugin_path)) == target_plugin_path:
            cmds.unloadPlugin(loaded_plugin, force=True)

    for module_name in list(sys.modules):
        if module_name == "klv_push" or module_name.startswith("klv_push."):
            sys.modules.pop(module_name, None)
    sys.path.insert(0, package_scripts)
    import klv_push.klvPush as klv_push_plugin

    imported_path = os.path.normcase(os.path.abspath(getattr(klv_push_plugin, "__file__", "")))
    package_root = os.path.normcase(os.path.abspath(package_scripts))
    if not imported_path.startswith(package_root + os.sep):
        raise RuntimeError("klv_push imported outside staged Plugin Artifact")

    loaded_plugins = cmds.loadPlugin(plugin_path, quiet=True)
    plugin_name = loaded_plugins[0] if loaded_plugins else os.path.basename(plugin_path)
    loaded_path = os.path.normcase(os.path.abspath(cmds.pluginInfo(plugin_name, query=True, path=True)))
    if loaded_path != os.path.normcase(os.path.abspath(plugin_path)):
        raise RuntimeError("klv_push loaded outside staged Plugin Artifact")

    marker = cmds.polyCube(name="mayaStallKlvPushImportMarker", width=1, height=1, depth=1)[0]
    plugin_node_type = getattr(klv_push_plugin, "NODE_NAME", "klvPush")
    deformer = cmds.deformer(marker, type=plugin_node_type)[0]
    cmds.select(marker, replace=True)
    cmds.refresh(force=True)
    write_result(
        "passed",
        "klv_push loaded from the staged artifact and registered its Maya deformer node.",
        pluginName="klv_push",
        action={"ok": True, "createdNodes": [marker, deformer], "nodeType": plugin_node_type, "pluginLoaded": True},
        mayaVersion=str(cmds.about(version=True)),
        artifact={"name": "build/maya2025/Release/klv_push", "builtScriptsPackage": True, "fromTrustedRoot": bool(trusted_root)},
        **{"import": {"ok": True, "module": getattr(klv_push_plugin, "__name__", "klv_push.klvPush"), "fromStagedArtifact": True, "fromTrustedRoot": bool(trusted_root)}},
    )
except Exception as exc:
    write_result(
        "failed",
        str(exc),
        pluginName="klv_push",
        action={"ok": False, "createdNodes": []},
        mayaVersion=str(cmds.about(version=True)) if "cmds" in globals() else "",
        traceback=traceback.format_exc(),
        **{"import": {"ok": False}},
    )
    raise
`
}

func assertKLVPushScenarioResult(t *testing.T, evidenceDir string, relativeResultPath string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(relativeResultPath)))
	if err != nil {
		t.Fatalf("read KLV Push Scenario Result: %v", err)
	}
	var result struct {
		Status     string `json:"status"`
		PluginName string `json:"pluginName"`
		Import     struct {
			OK bool `json:"ok"`
		} `json:"import"`
		Action struct {
			OK           bool     `json:"ok"`
			CreatedNodes []string `json:"createdNodes"`
		} `json:"action"`
		MayaVersion string `json:"mayaVersion"`
	}
	if err := json.Unmarshal(content, &result); err != nil {
		t.Fatalf("parse KLV Push Scenario Result: %v", err)
	}
	if result.Status != resultStatusPassed || result.PluginName != "klv_push" || !result.Import.OK || !result.Action.OK || result.MayaVersion == "" || len(result.Action.CreatedNodes) == 0 {
		t.Fatalf("KLV Push Scenario Result missing product proof: %+v", result)
	}
}

func looksLikeImageBytes(mediaType string, content []byte) bool {
	switch mediaType {
	case "image/jpeg":
		return len(content) >= 3 && content[0] == 0xff && content[1] == 0xd8 && content[2] == 0xff
	case "image/png":
		return len(content) >= 8 &&
			content[0] == 0x89 &&
			content[1] == 'P' &&
			content[2] == 'N' &&
			content[3] == 'G' &&
			content[4] == '\r' &&
			content[5] == '\n' &&
			content[6] == 0x1a &&
			content[7] == '\n'
	default:
		return len(content) > 0
	}
}
