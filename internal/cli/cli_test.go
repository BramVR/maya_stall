package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func init() {
	fakeSSHHostSideLockDirHook = func(host mayaHostConfig) (string, bool) {
		if !strings.HasPrefix(filepath.Base(host.SSH.Binary), "fake-ssh-") {
			return "", false
		}
		identity := sha256.Sum256([]byte(host.SSH.Host + "\x00" + host.WorkRoot))
		return filepath.Join(filepath.Dir(host.SSH.Binary), "fake-ssh-host", fmt.Sprintf("%x", identity[:8]), "state", "locks", "hosts"), true
	}
}

func TestHelpAndVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	code := Run([]string{"--help"}, &stdout, &stderr, "", "test-version")
	if code != 0 {
		t.Fatalf("help exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "maya-stall") || !strings.Contains(stdout.String(), "init") {
		t.Fatalf("help output missing command surface:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "\n\t") {
		t.Fatalf("help output contains tab-indented command:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"version"}, &stdout, &stderr, "", "test-version")
	if code != 0 {
		t.Fatalf("version exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "maya-stall test-version" {
		t.Fatalf("version output = %q", got)
	}
}

func TestInitWritesRepoOnlySmokeScenario(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"init"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("init exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	content := string(contentBytes)
	for _, want := range []string{
		"version: 1",
		"scenarios:",
		"smoke:",
		"requirements:",
		"minimum: \"2025\"",
		"payload:",
		"evidence:",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("generated config missing %q:\n%s", want, content)
		}
	}
	for _, forbidden := range []string{
		"host",
		"Host",
		"hostname",
		"Host Pool",
		"Host Credentials",
		"ssh",
		"credential",
		"password",
		"private",
	} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("generated config contains forbidden host/credential detail %q:\n%s", forbidden, content)
		}
	}
}

func TestInitConfigRunsFakeSmokeScenario(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"init"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("init exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("run output missing passed status:\n%s", stdout.String())
	}
}

func TestInitConfigPlansForPreparedPointReleaseHost(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"init"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("init exit code = %d; stderr: %s", code, stderr.String())
	}
	hostConfig := filepath.Join(dir, "hosts.yaml")
	mustWriteFile(t, hostConfig, `version: 1
targetProfiles:
  ci:
    hostPool: windows
hostPools:
  windows:
    hosts:
      - id: maya-win-01
        capabilities:
          mayaBuilds: ["2025.3"]
          sessionMayaBuild: "2025.3"
          python: "3.11"
          sessionBroker:
            version: "1"
            features: [script.execute]
          capture: [screenshot, visual-evidence]
          control: []
          renderers: [unknown]
          gpu: [unknown]
          display: [console]
          licensing: [unknown]
          trustedPluginArtifacts: false
`)
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"plan", "--host-config", hostConfig, "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("prepared point-release plan exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func TestDiscoverConfigRecognizesSupportedRepoFilenames(t *testing.T) {
	for _, name := range []string{".maya-stall.yaml", "maya-stall.yaml"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, name)
			if err := os.WriteFile(path, []byte("version: 1\nscenarios: {}\n"), 0o644); err != nil {
				t.Fatalf("write config fixture: %v", err)
			}

			got, err := DiscoverConfig(dir)
			if err != nil {
				t.Fatalf("DiscoverConfig returned error: %v", err)
			}
			if got != path {
				t.Fatalf("DiscoverConfig = %q, want %q", got, path)
			}
		})
	}
}

func TestRunScenarioStagesPayloadAndWritesStateAndEvidence(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "passed", Summary: "fake broker result"}}

	code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("run output missing passed status:\n%s", stdout.String())
	}

	runState := onlyRunDir(t, filepath.Join(dir, ".maya-stall", "state", "runs"))
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))

	manifestBytes, err := os.ReadFile(filepath.Join(runState, "manifest.json"))
	if err != nil {
		t.Fatalf("read run manifest: %v", err)
	}
	var manifest struct {
		Scenario string `json:"scenario"`
		Payload  []struct {
			Kind   string `json:"kind"`
			Source string `json:"source"`
			Staged string `json:"staged"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("parse run manifest: %v", err)
	}
	if manifest.Scenario != "smoke" {
		t.Fatalf("manifest scenario = %q, want smoke", manifest.Scenario)
	}
	wantPayload := map[string]string{
		"mayaScripts:maya/smoke.py":      filepath.Join("payload", "mayaScripts", "maya", "smoke.py"),
		"scenes:scenes/start.ma":         filepath.Join("payload", "scenes", "scenes", "start.ma"),
		"pluginArtifacts:build/demo.mll": filepath.Join("payload", "pluginArtifacts", "build", "demo.mll"),
	}
	for _, item := range manifest.Payload {
		key := item.Kind + ":" + item.Source
		want, ok := wantPayload[key]
		if !ok {
			t.Fatalf("unexpected manifest payload item: %+v", item)
		}
		if item.Staged != want {
			t.Fatalf("manifest staged path for %s = %q, want %q", key, item.Staged, want)
		}
		if _, err := os.Stat(filepath.Join(runState, item.Staged)); err != nil {
			t.Fatalf("staged payload %s: %v", item.Staged, err)
		}
		delete(wantPayload, key)
	}
	if len(wantPayload) != 0 {
		t.Fatalf("manifest missing payload items: %#v", wantPayload)
	}

	for _, path := range []string{
		filepath.Join(runState, "events.jsonl"),
		filepath.Join(runState, "logs", "session.log"),
		filepath.Join(runState, "workspace", "outputs", "smoke-result.json"),
		filepath.Join(evidence, "evidence.json"),
		filepath.Join(evidence, "events.jsonl"),
		filepath.Join(evidence, "logs", "session.log"),
		filepath.Join(evidence, "manifest.json"),
		filepath.Join(evidence, "scenario-result.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected run artifact %s: %v", path, err)
		}
	}
}

func TestRunScenarioCapturesRecordingVisualEvidenceArtifact(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    evidence:
      recording:
        enabled: true
    validators:
      - type: visualEvidence
        required: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("run output missing passed status:\n%s", stdout.String())
	}

	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	recordingPath := filepath.Join(evidence, "recordings", "smoke.mp4")
	recordingBytes, err := os.ReadFile(recordingPath)
	if err != nil {
		t.Fatalf("read recording artifact: %v", err)
	}
	if !looksLikeMP4Bytes(recordingBytes) {
		t.Fatalf("recording artifact does not look like MP4 bytes: %q", string(recordingBytes))
	}
	bundle := readEvidenceBundle(t, evidence)
	if bundle.TargetProfile != "default" || bundle.Host != defaultFakeHostID {
		t.Fatalf("Evidence Bundle selected target metadata = target %q host %q", bundle.TargetProfile, bundle.Host)
	}
	if len(bundle.VisualEvidence) != 1 {
		t.Fatalf("visual evidence metadata = %+v", bundle.VisualEvidence)
	}
	got := bundle.VisualEvidence[0]
	if got.Kind != "recording" || got.Path != "recordings/smoke.mp4" || got.MediaType != "video/mp4" {
		t.Fatalf("recording metadata = %+v", got)
	}
	if got.DurationSeconds != defaultRecordingDuration.Seconds() || got.FPS != defaultRecordingFPS {
		t.Fatalf("recording timing metadata = %+v", got)
	}
	if got.TargetProfile != bundle.TargetProfile || got.Host != bundle.Host {
		t.Fatalf("recording target metadata = %+v, want Target Profile %q and Maya Host %q", got, bundle.TargetProfile, bundle.Host)
	}
	if len(bundle.Validators) != 1 || bundle.Validators[0].Type != "visualEvidence" || bundle.Validators[0].Status != resultStatusPassed {
		t.Fatalf("validator metadata = %+v", bundle.Validators)
	}
}

func TestScreenshotCapturesVisualEvidenceArtifact(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("screenshot exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "artifact: screenshots/screenshot.png") {
		t.Fatalf("screenshot output missing artifact path:\n%s", stdout.String())
	}
	if strings.Contains(stdout.String(), "accepted:") {
		t.Fatalf("standalone screenshot output contains Scenario acceptance state:\n%s", stdout.String())
	}

	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	if _, err := os.Stat(filepath.Join(evidence, "screenshots", "screenshot.png")); err != nil {
		t.Fatalf("expected screenshot artifact: %v", err)
	}
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.VisualEvidence) != 1 {
		t.Fatalf("visual evidence count = %d, want 1", len(bundle.VisualEvidence))
	}
	got := bundle.VisualEvidence[0]
	if got.Kind != "screenshot" || got.Path != filepath.ToSlash(filepath.Join("screenshots", "screenshot.png")) {
		t.Fatalf("visual evidence metadata = %+v", got)
	}
	if got.TargetProfile != "default" || got.Host != defaultFakeHostID {
		t.Fatalf("visual evidence target metadata = %+v", got)
	}
	if got.Origin != visualEvidenceOriginFakeBrokerCapture {
		t.Fatalf("visual evidence origin = %q, want %q", got.Origin, visualEvidenceOriginFakeBrokerCapture)
	}
	if got.SHA256 != "e9a524f5fbb36de6c8725271e42cce2f536219c0f05f5b10163668b548cf63d7" {
		t.Fatalf("visual evidence sha256 = %q, want fake screenshot digest", got.SHA256)
	}
}

func TestRecordCapturesVisualEvidenceArtifactWithCrabboxDefaults(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"record"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("record exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "artifact: recordings/recording.mp4") {
		t.Fatalf("record output missing artifact path:\n%s", stdout.String())
	}

	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	if _, err := os.Stat(filepath.Join(evidence, "recordings", "recording.mp4")); err != nil {
		t.Fatalf("expected recording artifact: %v", err)
	}
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.VisualEvidence) != 1 {
		t.Fatalf("visual evidence count = %d, want 1", len(bundle.VisualEvidence))
	}
	got := bundle.VisualEvidence[0]
	if got.Kind != "recording" || got.Path != filepath.ToSlash(filepath.Join("recordings", "recording.mp4")) {
		t.Fatalf("visual evidence metadata = %+v", got)
	}
	if got.MediaType != "video/mp4" || got.DurationSeconds != defaultRecordingDuration.Seconds() || got.FPS != defaultRecordingFPS {
		t.Fatalf("recording timing metadata = %+v", got)
	}
	if got.TargetProfile != "default" || got.Host != defaultFakeHostID {
		t.Fatalf("visual evidence target metadata = %+v", got)
	}
}

func TestControlClickRunsDesktopClick(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("control click exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"action: click",
		"targetProfile: default",
		"host: " + defaultFakeHostID,
		"x: 12",
		"y: 34",
		"dryRun: false",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("control click output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestControlClickDryRunDoesNotRequireDesktopControlSupport(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = invalidSessionBroker{err: errors.New("control unavailable")}

	code := RunWithRuntime([]string{"control", "click", "--x", "12", "--y", "34", "--dry-run"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("control click dry-run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "control unavailable") {
		t.Fatalf("dry-run should not execute desktop control: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "dryRun: true") {
		t.Fatalf("dry-run output missing flag:\n%s", stdout.String())
	}
}

func TestControlClickReturnsUsageCodeForInvalidCoordinates(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"control", "click", "--x", "-1", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 2 {
		t.Fatalf("control click exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "coordinates must be non-negative") {
		t.Fatalf("control click error missing coordinate detail: %s", stderr.String())
	}
}

func TestAttachRunScreenshotCapturesThroughOwnedHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("attach screenshot exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "artifact: screenshots/run-scoped-screenshot.png") {
		t.Fatalf("attach screenshot output missing artifact path:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "maya-stall", runID, "screenshots", "run-scoped-screenshot.png")); err != nil {
		t.Fatalf("expected run-scoped screenshot artifact: %v", err)
	}
	evidenceDir := filepath.Join(dir, "artifacts", "maya-stall", runID)
	bundle := readEvidenceBundle(t, evidenceDir)
	found := false
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Kind == "screenshot" && artifact.Path == "screenshots/run-scoped-screenshot.png" && artifact.TargetProfile == "default" && artifact.Host == defaultFakeHostID {
			if artifact.Origin != visualEvidenceOriginFakeBrokerCapture || artifact.SHA256 != sha256HexOfBytes([]byte("fake screenshot\n")) {
				t.Fatalf("run-scoped screenshot provenance = %+v", artifact)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("Evidence Bundle missing run-scoped screenshot metadata: %+v", bundle.VisualEvidence)
	}
	if err := requireBrokerCaptureProvenanceEvents(evidenceDir, bundle); err != nil {
		t.Fatalf("run-scoped screenshot bundle provenance events: %v", err)
	}
	requireSequencedRunEvents(t, filepath.Join(evidenceDir, evidenceEventsFileName))
}

func TestAttachRunScreenshotUsesAuthoritativeHostLockWithoutLocalMirror(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, dir, hostRoot)
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "keep for inspection"}}
	var stdout, stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	localMirror := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	if err := os.Remove(localMirror); err != nil {
		t.Fatalf("remove compatibility Host Lock mirror: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("attach screenshot exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestAttachRunScreenshotRejectsExpiredAuthoritativeActiveLease(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, dir, hostRoot)
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "keep for inspection"}}
	var stdout, stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	hostLockPath := filepath.Join(hostRoot, "state", "locks", "hosts", "alpha.lock")
	mustWriteFile(t, hostLockPath, fmt.Sprintf("host: alpha\nlockToken: expired-owner\nactiveRun: %s\nleaseExpiresAt: %s\nleaseDurationSeconds: 30\n", runID, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)))
	if err := os.Remove(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); err != nil {
		t.Fatalf("remove compatibility Host Lock mirror: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("expired active attach screenshot exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestAttachRunControlClickRequiresOwnedHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("attach control click exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"run: " + runID,
		"action: click",
		"host: " + defaultFakeHostID,
		"x: 12",
		"y: 34",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("attach control click output missing %q:\n%s", want, stdout.String())
		}
	}

	mustWriteFile(t, filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock"), "host: "+defaultFakeHostID+"\nkeptRun: other-run\n")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("wrong-owner attach control click exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "belongs to kept run other-run, not "+runID) {
		t.Fatalf("wrong-owner error missing Host Lock owner detail: %s", stderr.String())
	}
}

func TestActiveRunRecordsRunScopedHostLockOwner(t *testing.T) {
	dir := writeRunConfigFixture(t)
	broker := newBlockingBroker(ScenarioResult{Status: resultStatusPassed, Summary: "active run completed"})
	runtimeConfig := defaultRunRuntime()
	runtimeConfig.Broker = broker
	var runOut, runErr bytes.Buffer
	done := make(chan runResult, 1)

	go func() {
		code := RunWithRuntime([]string{"run", "smoke"}, &runOut, &runErr, dir, "test-version", runtimeConfig)
		done <- runResult{code: code, stdout: runOut.String(), stderr: runErr.String()}
	}()
	<-broker.started
	runState := onlyRunDir(t, filepath.Join(dir, ".maya-stall", "state", "runs"))
	runID := filepath.Base(runState)
	lockBytes, err := os.ReadFile(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock"))
	if err != nil {
		t.Fatalf("read active Host Lock: %v", err)
	}
	if !strings.Contains(string(lockBytes), "activeRun: "+runID) {
		t.Fatalf("active Fresh Run Host Lock content = %q, want activeRun owner", string(lockBytes))
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("active attach screenshot exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "maya-stall", runID, "screenshots", "run-scoped-screenshot.png")); err != nil {
		t.Fatalf("expected active run-scoped screenshot artifact: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "56", "--y", "78"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("active attach control click exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "x: 56") || !strings.Contains(stdout.String(), "y: 78") {
		t.Fatalf("active attach control click output missing coordinates:\n%s", stdout.String())
	}
	close(broker.release)
	result := <-done
	if result.code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", result.code, result.stdout, result.stderr)
	}
	bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", runID))
	found := false
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Kind == "screenshot" && artifact.Path == "screenshots/run-scoped-screenshot.png" {
			found = true
		}
	}
	if !found {
		t.Fatalf("completed active run Evidence Bundle missing run-scoped screenshot: %+v", bundle.VisualEvidence)
	}
}

func TestAttachRunScreenshotFailsClosedForWrongActiveOwner(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock"), fmt.Sprintf("host: %s\npid: %d\nactiveRun: other-run\n", defaultFakeHostID, os.Getpid()))

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("wrong-active-owner attach screenshot exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "belongs to active run other-run, not "+runID) {
		t.Fatalf("wrong-active-owner error missing Host Lock owner detail: %s", stderr.String())
	}
}

func TestAttachRunScreenshotRejectsSymlinkArtifactLeaf(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	screenshotDir := filepath.Join(dir, "artifacts", "maya-stall", runID, "screenshots")
	if err := os.MkdirAll(screenshotDir, 0o755); err != nil {
		t.Fatalf("create screenshot dir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.png")
	mustWriteFile(t, outside, "outside\n")
	if err := os.Symlink(outside, filepath.Join(screenshotDir, "run-scoped-screenshot.png")); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("symlink leaf attach screenshot exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "must not be a symlink") {
		t.Fatalf("symlink leaf error missing detail: %s", stderr.String())
	}
	content, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read outside target: %v", err)
	}
	if string(content) != "outside\n" {
		t.Fatalf("outside symlink target was overwritten: %q", string(content))
	}
}

func TestAttachRunScreenshotRejectsSymlinkStateLeavesBeforeCapture(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	for _, leaf := range []string{"events.jsonl", "run-scoped-visual-evidence.json"} {
		t.Run(leaf, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			var stdout, stderr bytes.Buffer

			code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
			if code != 0 {
				t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			runID := outputValue(t, stdout.String(), "run")
			outside := filepath.Join(t.TempDir(), "outside-state")
			mustWriteFile(t, outside, "outside\n")
			stateLeaf := filepath.Join(dir, ".maya-stall", "state", "runs", runID, leaf)
			if err := os.Remove(stateLeaf); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("remove state leaf: %v", err)
			}
			if err := os.Symlink(outside, stateLeaf); err != nil {
				t.Skipf("create symlink fixture: %v", err)
			}

			stdout.Reset()
			stderr.Reset()
			code = Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
			if code != 1 {
				t.Fatalf("symlink state leaf attach screenshot exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "must not be a symlink") {
				t.Fatalf("symlink state leaf error missing detail: %s", stderr.String())
			}
			if _, err := os.Stat(filepath.Join(dir, "artifacts", "maya-stall", runID, "screenshots", "run-scoped-screenshot.png")); !os.IsNotExist(err) {
				t.Fatalf("run-scoped screenshot should not be captured before state leaf validation, stat err = %v", err)
			}
		})
	}
}

func TestAttachRunControlValidatesRetainedRemoteRunRoot(t *testing.T) {
	dir := t.TempDir()
	runID := "20260708T000000.000000000Z"
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	mustWriteFile(t, filepath.Join(stateDir, "manifest.json"), `{
  "runId": "20260708T000000.000000000Z",
  "scenario": "smoke",
  "targetProfile": "ci",
  "host": "alpha",
  "runtime": {
    "profile": "ssh-sessiond",
    "hostAdapter": "ssh",
    "brokerAdapter": "gg-mayasessiond",
    "liveProofEligible": true
  }
}
`)
	mustWriteFile(t, filepath.Join(stateDir, "run-record.json"), `{
  "runId": "20260708T000000.000000000Z",
  "scenario": "smoke",
  "targetProfile": "ci",
  "host": "alpha",
  "runtime": {
    "profile": "ssh-sessiond",
    "hostAdapter": "ssh",
    "brokerAdapter": "gg-mayasessiond",
    "liveProofEligible": true
  },
  "status": "kept",
  "localStateDir": "",
  "localEvidenceDir": "",
  "localWorkspace": "",
  "remoteRunRoot": "C:/unexpected/runs/20260708T000000.000000000Z",
  "hostConfig": {
    "id": "alpha",
    "transport": "ssh",
    "ssh": {
      "host": "maya-win-01"
    },
    "workRoot": "C:/maya-stall",
    "broker": {
      "type": "gg-mayasessiond",
      "stateDir": "C:/maya-stall/sessiond-ui",
      "python": "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
      "repo": "C:/maya-stall/tools/GG_MayaSessiond"
    }
  }
}
`)
	mustWriteFile(t, filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock"), "host: alpha\nkeptRun: "+runID+"\n")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("tampered remote root attach control exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "remote cleanup path") || !strings.Contains(stderr.String(), "does not match expected") {
		t.Fatalf("tampered remote root error missing validation detail: %s", stderr.String())
	}
}

func TestStandaloneVisualEvidenceCommandsReturnUsageCodeForUnknownTargetProfile(t *testing.T) {
	for _, command := range []string{"screenshot", "record"} {
		t.Run(command, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			var stdout, stderr bytes.Buffer

			code := Run([]string{command, "--target-profile", "missing"}, &stdout, &stderr, dir, "test-version")
			if code != 2 {
				t.Fatalf("%s exit code = %d, want 2; stdout: %s stderr: %s", command, code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "unknown Target Profile") {
				t.Fatalf("%s error missing usage detail: %s", command, stderr.String())
			}
		})
	}
}

func TestEvidenceCollectWritesManifestAndExpectedVisualFiles(t *testing.T) {
	dir := writeRunConfigFixture(t)
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    mayaVersion: "2025"
    payload:
      scripts:
        - "maya/smoke.py"
      scenes:
        - "scenes/start.ma"
      pluginArtifacts:
        - "build/demo.mll"
    expectedOutputs:
      scenarioResult: "outputs/smoke-result.json"
    evidence:
      screenshots:
        enabled: true
      recording:
        enabled: true
    validators:
      - type: visualEvidence
        required: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("evidence collect output missing status:\n%s", stdout.String())
	}

	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	for _, path := range []string{
		filepath.Join(evidence, "evidence.json"),
		filepath.Join(evidence, "manifest.json"),
		filepath.Join(evidence, "events.jsonl"),
		filepath.Join(evidence, "logs", "session.log"),
		filepath.Join(evidence, "scenario-result.json"),
		filepath.Join(evidence, "screenshots", "smoke.png"),
		filepath.Join(evidence, "recordings", "smoke.mp4"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected evidence collect file %s: %v", path, err)
		}
	}
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Status != resultStatusPassed {
		t.Fatalf("evidence status = %q, want passed", bundle.Status)
	}
	if len(bundle.VisualEvidence) != 2 {
		t.Fatalf("visual evidence metadata = %+v", bundle.VisualEvidence)
	}
	foundScreenshot := false
	foundRecording := false
	for _, artifact := range bundle.VisualEvidence {
		switch artifact.Kind {
		case "screenshot":
			foundScreenshot = artifact.Path == "screenshots/smoke.png" && artifact.TargetProfile == "default" && artifact.Host == defaultFakeHostID
		case "recording":
			foundRecording = artifact.Path == "recordings/smoke.mp4" && artifact.MediaType == "video/mp4" && artifact.DurationSeconds == defaultRecordingDuration.Seconds() && artifact.FPS == defaultRecordingFPS
		}
	}
	if !foundScreenshot || !foundRecording {
		t.Fatalf("visual evidence metadata missing screenshot/recording: %+v", bundle.VisualEvidence)
	}
	if len(bundle.Validators) != 1 || bundle.Validators[0].Type != "visualEvidence" || bundle.Validators[0].Status != resultStatusPassed {
		t.Fatalf("validator metadata = %+v", bundle.Validators)
	}
}

func TestEvidenceCollectFailsClearlyWhenRequiredVisualEvidenceMissing(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
      - type: visualEvidence
        required: true
`)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = outputWritingBroker{}

	code := RunWithRuntime([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("evidence collect exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: failed") {
		t.Fatalf("evidence collect output missing failed status:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "validator: visualEvidence failed - Visual Evidence is missing") {
		t.Fatalf("evidence collect output missing clear Visual Evidence failure:\n%s", stdout.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Status != resultStatusFailed {
		t.Fatalf("evidence status = %q, want failed", bundle.Status)
	}
	if len(bundle.Validators) != 1 || bundle.Validators[0].Status != resultStatusFailed || !strings.Contains(bundle.Validators[0].Message, "Visual Evidence is missing") {
		t.Fatalf("missing Visual Evidence failure = %+v", bundle.Validators)
	}
}

func TestDoctorAcceptsRecordingEvidenceOnDefaultHost(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    evidence:
      recording:
        enabled: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "visual-evidence: ok") {
		t.Fatalf("doctor did not accept default-host recording scenario:\n%s", stdout.String())
	}
}

func TestEvidenceCollectSanitizesScenarioNameBeforeWritingVisualEvidence(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  "../escape":
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    evidence:
      screenshots:
        enabled: true
    validators:
      - type: visualEvidence
        required: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "collect", "../escape"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	if _, err := os.Stat(filepath.Join(evidence, "escape.png")); !os.IsNotExist(err) {
		t.Fatalf("unexpected escaped screenshot path stat err = %v", err)
	}
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.VisualEvidence) != 1 {
		t.Fatalf("visual evidence metadata = %+v", bundle.VisualEvidence)
	}
	if strings.Contains(bundle.VisualEvidence[0].Path, "..") || strings.Contains(bundle.VisualEvidence[0].Path, "/../") {
		t.Fatalf("visual evidence path was not sanitized: %+v", bundle.VisualEvidence[0])
	}
	if _, err := os.Stat(filepath.Join(evidence, bundle.VisualEvidence[0].Path)); err != nil {
		t.Fatalf("expected sanitized visual evidence file: %v", err)
	}
}

func TestEvidencePublishCopiesBundleAndWritesReviewLinks(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = outputWritingBroker{
		files:          map[string]string{"outputs/report.json": `{"ok":true}` + "\n"},
		visualEvidence: true,
	}

	code := RunWithRuntime([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runID := filepath.Base(evidence)
	store := filepath.Join(t.TempDir(), "evidence-store")
	baseURL := "https://evidence.example.test/maya/"

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", baseURL, evidence}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence publish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	published := filepath.Join(store, runID)
	for _, path := range []string{
		filepath.Join(published, "evidence.json"),
		filepath.Join(published, "manifest.json"),
		filepath.Join(published, "artifact-manifest.json"),
		filepath.Join(published, "review-comment.md"),
		filepath.Join(published, "screenshots", "smoke.png"),
		filepath.Join(published, "outputs", "report.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected published artifact %s: %v", path, err)
		}
	}

	artifactManifestBytes, err := os.ReadFile(filepath.Join(published, "artifact-manifest.json"))
	if err != nil {
		t.Fatalf("read artifact manifest: %v", err)
	}
	var artifactManifest publishedArtifactManifest
	if err := json.Unmarshal(artifactManifestBytes, &artifactManifest); err != nil {
		t.Fatalf("parse artifact manifest: %v", err)
	}
	wantScreenshotURL := "https://evidence.example.test/maya/" + runID + "/screenshots/smoke.png"
	wantLogURL := "https://evidence.example.test/maya/" + runID + "/logs/session.log"
	wantOutputURL := "https://evidence.example.test/maya/" + runID + "/outputs/report.json"
	if artifactManifest.RunID != runID || artifactManifest.BaseURL != strings.TrimRight(baseURL, "/") {
		t.Fatalf("artifact manifest identity = %+v, want run/base URL", artifactManifest)
	}
	if !publishedManifestHasURL(artifactManifest, "Visual Evidence", wantScreenshotURL) {
		t.Fatalf("artifact manifest missing screenshot URL %q: %+v", wantScreenshotURL, artifactManifest.Artifacts)
	}
	if !publishedManifestHasURL(artifactManifest, "logs", wantLogURL) {
		t.Fatalf("artifact manifest missing log URL %q: %+v", wantLogURL, artifactManifest.Artifacts)
	}
	if !publishedManifestHasURL(artifactManifest, "outputs", wantOutputURL) {
		t.Fatalf("artifact manifest missing output URL %q: %+v", wantOutputURL, artifactManifest.Artifacts)
	}

	reviewMarkdownBytes, err := os.ReadFile(filepath.Join(published, "review-comment.md"))
	if err != nil {
		t.Fatalf("read review markdown: %v", err)
	}
	reviewMarkdown := string(reviewMarkdownBytes)
	for _, want := range []string{
		"status: passed",
		wantScreenshotURL,
		wantLogURL,
		wantOutputURL,
		"https://evidence.example.test/maya/" + runID + "/evidence.json",
	} {
		if !strings.Contains(reviewMarkdown, want) {
			t.Fatalf("review markdown missing %q:\n%s", want, reviewMarkdown)
		}
	}

	mustWriteFile(t, filepath.Join(published, "stale.txt"), "stale\n")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", baseURL, evidence}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("republish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(published, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("republish did not remove stale file, stat err = %v", err)
	}
}

func TestEvidencePublishRejectsDestinationInsideBundle(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	nestedStore := filepath.Join(evidence, "store")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", nestedStore, "--base-url", "https://evidence.example.test/maya", evidence}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("nested publish exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "must not overlap") {
		t.Fatalf("nested publish error missing clear detail: %s", stderr.String())
	}
	if _, err := os.Stat(nestedStore); !os.IsNotExist(err) {
		t.Fatalf("nested publish created destination, stat err = %v", err)
	}
}

func TestEvidencePublishRejectsBundleInsideDestinationRun(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runID := filepath.Base(evidence)
	store := filepath.Join(t.TempDir(), "store")
	bundleInsidePublishedRun := filepath.Join(store, runID, "bundle")
	if err := copyPath(evidence, bundleInsidePublishedRun); err != nil {
		t.Fatalf("prepare nested source bundle: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", bundleInsidePublishedRun}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("overlap publish exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "must not overlap") {
		t.Fatalf("overlap publish error missing clear detail: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(bundleInsidePublishedRun, "evidence.json")); err != nil {
		t.Fatalf("overlap publish deleted source bundle: %v", err)
	}
}

func TestEvidencePublishDoesNotDeleteSourceAtScratchLikePath(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runID := filepath.Base(evidence)
	store := filepath.Join(t.TempDir(), "store")
	scratchLikeSource := filepath.Join(store, "."+runID+".tmp")
	if err := copyPath(evidence, scratchLikeSource); err != nil {
		t.Fatalf("prepare scratch-like source bundle: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", scratchLikeSource}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("publish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(scratchLikeSource, "evidence.json")); err != nil {
		t.Fatalf("publish deleted scratch-like source bundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, runID, "review-comment.md")); err != nil {
		t.Fatalf("publish did not create final review comment: %v", err)
	}
}

func TestEvidencePublishInvalidBundleRunIDReturnsUsageCode(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "bundle")
	mustWriteFile(t, filepath.Join(bundleDir, "evidence.json"), `{
  "runId": "../bad",
  "scenario": "smoke",
  "status": "passed",
  "targetProfile": "default",
  "host": "fake-local",
  "manifest": "manifest.json",
  "events": "events.jsonl",
  "log": "logs/session.log",
  "scenarioResult": "scenario-result.json",
  "payload": []
}
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "publish", "--destination", filepath.Join(dir, "store"), "--base-url", "https://evidence.example.test/maya", bundleDir}, &stdout, &stderr, dir, "test-version")
	if code != 2 {
		t.Fatalf("publish exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "run id") {
		t.Fatalf("publish error missing run id usage detail: %s", stderr.String())
	}
}

func TestEvidencePublishEscapesReviewMarkdownLabels(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = outputWritingBroker{
		files: map[string]string{"outputs/report](https://evil.test).json": "{}\n"},
	}

	code := RunWithRuntime([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	store := filepath.Join(t.TempDir(), "store")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", evidence}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence publish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := filepath.Base(evidence)
	reviewMarkdownBytes, err := os.ReadFile(filepath.Join(store, runID, "review-comment.md"))
	if err != nil {
		t.Fatalf("read review markdown: %v", err)
	}
	reviewMarkdown := string(reviewMarkdownBytes)
	if strings.Contains(reviewMarkdown, "](https://evil.test)") || strings.Contains(reviewMarkdown, "](https:/evil.test)") {
		t.Fatalf("review markdown contains unescaped forged link:\n%s", reviewMarkdown)
	}
	if !strings.Contains(reviewMarkdown, `outputs/report\]\(https:/evil.test\).json`) {
		t.Fatalf("review markdown missing escaped output label:\n%s", reviewMarkdown)
	}
	if !strings.Contains(reviewMarkdown, `](<https://evidence.example.test/maya/`) {
		t.Fatalf("review markdown missing angle-bracket link destination:\n%s", reviewMarkdown)
	}
}

func TestEvidencePublishFailedRepublishPreservesExistingRun(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    evidence:
      screenshots:
        enabled: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runID := filepath.Base(evidence)
	store := filepath.Join(t.TempDir(), "store")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", evidence}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("initial publish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	published := filepath.Join(store, runID)
	mustWriteFile(t, filepath.Join(published, "existing-marker.txt"), "keep me\n")
	bundle := readEvidenceBundle(t, evidence)
	bundle.VisualEvidence = []visualEvidenceArtifact{{Kind: "screenshot", Path: "screenshots/missing.png", MediaType: "image/png"}}
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	if err := writeJSONFile(filepath.Join(evidence, "evidence.json"), bundle); err != nil {
		t.Fatalf("corrupt source evidence bundle: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", evidence}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("failed republish exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(published, "existing-marker.txt")); err != nil {
		t.Fatalf("failed republish did not preserve existing published run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(published, "review-comment.md")); err != nil {
		t.Fatalf("failed republish removed existing review comment: %v", err)
	}
}

func TestEvidencePublishLinksDeclaredOutputsOutsideOutputsDir(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
      - type: outputExists
        path: "reports/report.json"
`)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = outputWritingBroker{
		files: map[string]string{"reports/report.json": "{}\n"},
	}

	code := RunWithRuntime([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	if _, err := os.Stat(filepath.Join(evidence, "reports", "report.json")); err != nil {
		t.Fatalf("expected declared output in Evidence Bundle: %v", err)
	}
	store := filepath.Join(t.TempDir(), "store")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", evidence}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence publish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	published := filepath.Join(store, filepath.Base(evidence))
	wantOutputURL := "https://evidence.example.test/maya/" + filepath.Base(evidence) + "/reports/report.json"
	artifactManifestBytes, err := os.ReadFile(filepath.Join(published, "artifact-manifest.json"))
	if err != nil {
		t.Fatalf("read artifact manifest: %v", err)
	}
	var artifactManifest publishedArtifactManifest
	if err := json.Unmarshal(artifactManifestBytes, &artifactManifest); err != nil {
		t.Fatalf("parse artifact manifest: %v", err)
	}
	if !publishedManifestHasURL(artifactManifest, "outputs", wantOutputURL) {
		t.Fatalf("artifact manifest missing declared output URL %q: %+v", wantOutputURL, artifactManifest.Artifacts)
	}
	reviewMarkdownBytes, err := os.ReadFile(filepath.Join(published, "review-comment.md"))
	if err != nil {
		t.Fatalf("read review markdown: %v", err)
	}
	if !strings.Contains(string(reviewMarkdownBytes), wantOutputURL) {
		t.Fatalf("review markdown missing declared output URL %q:\n%s", wantOutputURL, string(reviewMarkdownBytes))
	}
}

func TestEvidencePublishUsesEvidenceBundleArtifactCatalog(t *testing.T) {
	dir := t.TempDir()
	bundleDir := filepath.Join(dir, "bundle")
	for _, path := range []string{
		"evidence.json",
		"manifest.json",
		"events.jsonl",
		filepath.Join("logs", "session.log"),
		"scenario-result.json",
		filepath.Join("screenshots", "smoke.jpg"),
		filepath.Join("outputs", "declared.json"),
		filepath.Join("outputs", "stray.json"),
	} {
		mustWriteFile(t, filepath.Join(bundleDir, path), "{}\n")
	}
	bundle := evidenceBundle{
		RunID:          "20260704T120000.000000000Z",
		Scenario:       "smoke",
		Status:         resultStatusPassed,
		TargetProfile:  "ci",
		Host:           "maya-win-01",
		Manifest:       "manifest.json",
		Events:         "events.jsonl",
		Log:            filepath.ToSlash(filepath.Join("logs", "session.log")),
		ScenarioResult: "scenario-result.json",
		Artifacts: []evidenceArtifact{
			{Label: "metadata", Kind: "metadata", Path: "evidence.json", MediaType: "application/json"},
			{Label: "metadata", Kind: "metadata", Path: "manifest.json", MediaType: "application/json"},
			{Label: "metadata", Kind: "metadata", Path: "scenario-result.json", MediaType: "application/json"},
			{Label: "logs", Kind: "events", Path: "events.jsonl", MediaType: "application/x-ndjson"},
			{Label: "logs", Kind: "log", Path: filepath.ToSlash(filepath.Join("logs", "session.log")), MediaType: "text/plain"},
			{Label: "Visual Evidence", Kind: "screenshot", Path: filepath.ToSlash(filepath.Join("screenshots", "smoke.jpg")), MediaType: "image/jpeg"},
			{Label: "outputs", Kind: "output", Path: filepath.ToSlash(filepath.Join("outputs", "declared.json")), MediaType: "application/json"},
		},
	}
	if err := writeJSONFile(filepath.Join(bundleDir, "evidence.json"), bundle); err != nil {
		t.Fatalf("write bundle catalog: %v", err)
	}
	store := filepath.Join(dir, "store")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", bundleDir}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence publish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	published := filepath.Join(store, bundle.RunID)
	artifactManifestBytes, err := os.ReadFile(filepath.Join(published, "artifact-manifest.json"))
	if err != nil {
		t.Fatalf("read artifact manifest: %v", err)
	}
	var artifactManifest publishedArtifactManifest
	if err := json.Unmarshal(artifactManifestBytes, &artifactManifest); err != nil {
		t.Fatalf("parse artifact manifest: %v", err)
	}
	if publishedManifestHasURL(artifactManifest, "outputs", "https://evidence.example.test/maya/"+bundle.RunID+"/outputs/stray.json") {
		t.Fatalf("artifact manifest included stray output outside catalog: %+v", artifactManifest.Artifacts)
	}
	if !publishedManifestHasURL(artifactManifest, "outputs", "https://evidence.example.test/maya/"+bundle.RunID+"/outputs/declared.json") {
		t.Fatalf("artifact manifest missing declared output from catalog: %+v", artifactManifest.Artifacts)
	}
	reviewMarkdownBytes, err := os.ReadFile(filepath.Join(published, "review-comment.md"))
	if err != nil {
		t.Fatalf("read review markdown: %v", err)
	}
	reviewMarkdown := string(reviewMarkdownBytes)
	if strings.Contains(reviewMarkdown, "outputs/stray.json") {
		t.Fatalf("review markdown included stray output outside catalog:\n%s", reviewMarkdown)
	}
	if !strings.Contains(reviewMarkdown, "outputs/declared.json") {
		t.Fatalf("review markdown missing declared catalog output:\n%s", reviewMarkdown)
	}
}

func TestEvidencePublishEscapesReviewMarkdownMetadata(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"evidence", "collect", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence collect exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	bundle.Status = "passed\n- forged: [x](https://evil.test)"
	bundle.Scenario = "smoke [scenario](https://evil.test)"
	if err := writeJSONFile(filepath.Join(evidence, "evidence.json"), bundle); err != nil {
		t.Fatalf("write malicious evidence metadata: %v", err)
	}
	store := filepath.Join(t.TempDir(), "store")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"evidence", "publish", "--destination", store, "--base-url", "https://evidence.example.test/maya", evidence}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("evidence publish exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	reviewMarkdownBytes, err := os.ReadFile(filepath.Join(store, filepath.Base(evidence), "review-comment.md"))
	if err != nil {
		t.Fatalf("read review markdown: %v", err)
	}
	reviewMarkdown := string(reviewMarkdownBytes)
	if strings.Contains(reviewMarkdown, "\n- forged:") || strings.Contains(reviewMarkdown, "](https://evil.test)") {
		t.Fatalf("review markdown contains unescaped metadata:\n%s", reviewMarkdown)
	}
	if !strings.Contains(reviewMarkdown, `status: passed - forged: \[x\]\(https://evil.test\)`) {
		t.Fatalf("review markdown missing escaped metadata:\n%s", reviewMarkdown)
	}
}

func TestScreenshotReportsHostLockReleaseFailure(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = lockCorruptingVisualBroker{}

	code := RunWithRuntime([]string{"screenshot"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("screenshot exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "release Host Lock for fake-local") {
		t.Fatalf("screenshot error did not report Host Lock release failure: %s", stderr.String())
	}
}

func TestRunScenarioProvidesScenarioResultPathInRunEnvironment(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	broker := &environmentCheckingBroker{}
	runtime := defaultRunRuntime()
	runtime.Broker = broker

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if broker.resultPath == "" {
		t.Fatal("broker did not receive Scenario Result path")
	}
	wantSuffix := filepath.Join(".maya-stall", "state", "runs", broker.runID, "workspace", "outputs", "smoke-result.json")
	if !strings.HasSuffix(broker.resultPath, wantSuffix) {
		t.Fatalf("Scenario Result path = %q, want suffix %q", broker.resultPath, wantSuffix)
	}
	if broker.resultPath != broker.environment[scenarioResultEnvVar] {
		t.Fatalf("run environment %s = %q, want %q", scenarioResultEnvVar, broker.environment[scenarioResultEnvVar], broker.resultPath)
	}
}

func TestRunScenarioUsesScenarioAuthoredResultFile(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = scenarioResultFileBroker{}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: failed") {
		t.Fatalf("run output missing file-authored failed status:\n%s", stdout.String())
	}

	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	scenarioResultBytes, err := os.ReadFile(filepath.Join(evidence, "scenario-result.json"))
	if err != nil {
		t.Fatalf("read evidence Scenario Result: %v", err)
	}
	var scenarioResult map[string]any
	if err := json.Unmarshal(scenarioResultBytes, &scenarioResult); err != nil {
		t.Fatalf("parse evidence Scenario Result: %v", err)
	}
	if scenarioResult["status"] != "failed" || scenarioResult["summary"] != "script wrote result" {
		t.Fatalf("evidence Scenario Result status/summary = %#v", scenarioResult)
	}
	if !strings.Contains(string(scenarioResultBytes), `"largeNumber": 9007199254740993`) {
		t.Fatalf("evidence Scenario Result rounded large number:\n%s", string(scenarioResultBytes))
	}
	assertions, ok := scenarioResult["assertions"].([]any)
	if !ok || len(assertions) != 1 {
		t.Fatalf("evidence Scenario Result lost assertions: %#v", scenarioResult)
	}
}

func TestRunScenarioAuthoredResultWithoutStatusKeepsBrokerFailure(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = partialScenarioResultFileBroker{}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: failed") {
		t.Fatalf("run output missing broker failure status:\n%s", stdout.String())
	}

	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	scenarioResultBytes, err := os.ReadFile(filepath.Join(evidence, "scenario-result.json"))
	if err != nil {
		t.Fatalf("read evidence Scenario Result: %v", err)
	}
	if !strings.Contains(string(scenarioResultBytes), `"status": "failed"`) {
		t.Fatalf("evidence Scenario Result did not preserve broker failure:\n%s", string(scenarioResultBytes))
	}
	if !strings.Contains(string(scenarioResultBytes), `"summary": "script partial result"`) {
		t.Fatalf("evidence Scenario Result lost authored summary:\n%s", string(scenarioResultBytes))
	}
}

func TestRunScenarioRejectsScenarioResultWithTrailingJSON(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = trailingScenarioResultBroker{}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "parse Scenario Result") {
		t.Fatalf("run error did not reject trailing JSON: %s", stderr.String())
	}
}

func TestRunScenarioRejectsSymlinkedScenarioResult(t *testing.T) {
	dir := writeRunConfigFixture(t)
	outside := filepath.Join(t.TempDir(), "result.json")
	mustWriteFile(t, outside, `{"status":"passed","summary":"outside"}`+"\n")
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = symlinkOutputBroker{linkPath: "outputs/smoke-result.json", target: outside}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "Scenario Result path") || !strings.Contains(stderr.String(), "must not be or contain a symlink") {
		t.Fatalf("run error did not reject symlinked Scenario Result: %s", stderr.String())
	}
}

func TestRunScenarioStagesTypedPayloadIncludingExplicitIgnoredOutputs(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".gitignore"), "build/\n")
	mustWriteFile(t, filepath.Join(dir, "maya", "smoke.py"), "print('smoke')\n")
	mustWriteFile(t, filepath.Join(dir, "scenes", "start.ma"), "// fake maya scene\n")
	mustWriteFile(t, filepath.Join(dir, "build", "demo.mll"), "fake ignored plugin artifact\n")
	mustWriteFile(t, filepath.Join(dir, "golden", "expected.json"), `{"ok":true}`+"\n")
	mustWriteFile(t, filepath.Join(dir, "includes", "helpers.py"), "def helper(): pass\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    mayaVersion: "2025"
    payload:
      mayaScripts:
        - "maya/smoke.py"
      scenes:
        - "scenes/start.ma"
      pluginArtifacts:
        - "build/demo.mll"
      expectedOutputs:
        - "golden/expected.json"
      includePaths:
        - "includes"
    expectedOutputs:
      scenarioResult: "outputs/smoke-result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	runState := onlyRunDir(t, filepath.Join(dir, ".maya-stall", "state", "runs"))
	manifestBytes, err := os.ReadFile(filepath.Join(runState, "manifest.json"))
	if err != nil {
		t.Fatalf("read run manifest: %v", err)
	}
	var manifest struct {
		Payload []manifestPayload `json:"payload"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("parse run manifest: %v", err)
	}
	wantPayload := map[string]string{
		"mayaScripts:maya/smoke.py":            filepath.Join("payload", "mayaScripts", "maya", "smoke.py"),
		"scenes:scenes/start.ma":               filepath.Join("payload", "scenes", "scenes", "start.ma"),
		"pluginArtifacts:build/demo.mll":       filepath.Join("payload", "pluginArtifacts", "build", "demo.mll"),
		"expectedOutputs:golden/expected.json": filepath.Join("payload", "expectedOutputs", "golden", "expected.json"),
		"includePaths:includes":                filepath.Join("payload", "includePaths", "includes"),
	}
	for _, item := range manifest.Payload {
		key := item.Kind + ":" + item.Source
		want, ok := wantPayload[key]
		if !ok {
			t.Fatalf("unexpected payload item: %+v", item)
		}
		if item.Staged != want {
			t.Fatalf("staged path for %s = %q, want %q", key, item.Staged, want)
		}
		if _, err := os.Stat(filepath.Join(runState, item.Staged)); err != nil {
			t.Fatalf("staged payload %s: %v", item.Staged, err)
		}
		delete(wantPayload, key)
	}
	if len(wantPayload) != 0 {
		t.Fatalf("manifest missing payload items: %#v", wantPayload)
	}
}

func TestRunScenarioSnapshotsOverlappingPayloadDeclarations(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "package", "plugin.py"), "# plug-in\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      pluginArtifacts:
        - "package/plugin.py"
        - "package"
    expectedOutputs:
      scenarioResult: "outputs/smoke-result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	runState := onlyRunDir(t, filepath.Join(dir, ".maya-stall", "state", "runs"))
	content, err := os.ReadFile(filepath.Join(runState, "payload", "pluginArtifacts", "package", "plugin.py"))
	if err != nil {
		t.Fatalf("read overlapping payload snapshot: %v", err)
	}
	if string(content) != "# plug-in\n" {
		t.Fatalf("overlapping payload snapshot = %q", content)
	}
}

func TestCopyFileRejectsExistingDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.txt")
	destination := filepath.Join(dir, "destination.txt")
	mustWriteFile(t, source, "new artifact\n")
	mustWriteFile(t, destination, "existing artifact\n")

	err := copyFile(source, destination)
	if err == nil || !errors.Is(err, os.ErrExist) {
		t.Fatalf("copy existing destination error = %v, want os.ErrExist", err)
	}
	content, readErr := os.ReadFile(destination)
	if readErr != nil {
		t.Fatalf("read existing destination: %v", readErr)
	}
	if string(content) != "existing artifact\n" {
		t.Fatalf("existing destination changed to %q", content)
	}
}

func TestRunScenarioReportsMissingTypedPayloadPath(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      includePaths:
        - "missing/includes"
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "stage includePaths payload missing/includes") {
		t.Fatalf("missing typed payload error = %q", stderr.String())
	}
}

func TestDoctorReportsHealthyLocalConfigAndTargetProfile(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{
		"local-config: ok - .maya-stall.yaml",
		"target-profile: ok - default",
		"host-pool: ok - default",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorLocalConfigFailureIncludesRepairHint(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{
		"local-config: fail",
		"no Maya Stall repo config found",
		"hint: Run maya-stall init or fix the repo config schema.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorReportsHostSpecificHealthLayers(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeLayeredHostConfig(t, dir)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{
		"host: ok - alpha",
		"fake-ssh: ok - reachable",
		"work-root: ok - writable",
		"session-broker: ok - reachable",
		"maya-version: ok - 2025",
		"visual-evidence: ok - available",
		"desktop-control: ok - available",
		"host-lock: ok - unlocked",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorReportsTrustedPluginAllowlistForConfiguredRoot(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`maya-stall-ssh-ok`,
		`writable`,
		`{"ok":true,"checks":[]}`,
		sessiondStatusFixture("session-alpha"),
		`{"ProcessId":123,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts";
`),
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		`removed`,
	})
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "trusted-plugin-allowlist: ok - Maya 2025 SafeModeAllowedlistPaths contains trustedPluginArtifactsRoot") {
		t.Fatalf("doctor output missing trusted-plugin-allowlist ok:\n%s", stdout.String())
	}
}

func TestDoctorFailsWhenTrustedPluginAllowlistMissesConfiguredRoot(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`maya-stall-ssh-ok`,
		`writable`,
		`{"ok":true,"checks":[]}`,
		sessiondStatusFixture("session-alpha"),
		`{"ProcessId":123,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/elsewhere/plugins";
`),
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		`removed`,
	})
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"trusted-plugin-allowlist: fail - maya 2025 SafeModeAllowedlistPaths does not contain trustedPluginArtifactsRoot",
		"hint: Add trustedPluginArtifactsRoot to Maya's trusted plug-in locations",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorRejectsUnsafeTrustedPluginAllowlistRoot(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        workRoot: C:/maya-stall
        trustedPluginArtifactsRoot: C:/maya-stall/runs/trusted-plugin-artifacts
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "trusted-plugin-allowlist: fail - trustedPluginArtifactsRoot must be outside workRoot/runs") {
		t.Fatalf("doctor output missing unsafe trusted root failure:\n%s", stdout.String())
	}
}

func TestDoctorRepairTrustedPluginAllowlistRequiresMayaStopped(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`maya-stall-ssh-ok`,
		`writable`,
		`maya-version:2025`,
		`{"ProcessId":123,"SessionId":1,"Name":"maya.exe"}`,
	})
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--repair-trusted-plugin-allowlist"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "trusted-plugin-allowlist: fail - TrustCenter repair requires Maya to be stopped first") {
		t.Fatalf("doctor output missing stopped-Maya repair guard:\n%s", stdout.String())
	}
}

func TestDoctorRepairTrustedPluginAllowlistStopsAfterScenarioValidationFailure(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	mustWriteFile(t, configPath, strings.Replace(string(configBytes), "build/demo.mll", "build/missing-demo.mll", 1))
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, nil)
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, "")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke", "--repair-trusted-plugin-allowlist"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "scenario-inputs: fail") {
		t.Fatalf("doctor output missing scenario validation failure:\n%s", stdout.String())
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Fatalf("TrustCenter repair touched the host after Scenario validation failed, stat err = %v", err)
	}
}

func TestDoctorRepairTrustedPluginAllowlistValidatesNestedDestinationsBeforeHostAccess(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	mustWriteFile(t, configPath, strings.Replace(string(configBytes), "build/demo.mll", "build/package", 1))
	mustWriteFile(t, filepath.Join(dir, "build", "package", "scripts\nbad", "plugin.py"), "def initializePlugin(plugin):\n    pass\n")
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, nil)
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, "")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke", "--repair-trusted-plugin-allowlist"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Fatalf("TrustCenter repair touched the host before validating nested destinations, stat err = %v", err)
	}
}

func TestDoctorRepairTrustedPluginAllowlistValidatesArtifactLeafBeforeHostAccess(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	mustWriteFile(t, configPath, strings.Replace(string(configBytes), "build/demo.mll", "build/demo?.mll", 1))
	mustWriteFile(t, filepath.Join(dir, "build", "demo?.mll"), "fake plug-in\n")
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, nil)
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, "")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke", "--repair-trusted-plugin-allowlist"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Fatalf("TrustCenter repair touched the host before validating the artifact leaf, stat err = %v", err)
	}
}

func TestDoctorRepairTrustedPluginAllowlistReusesPrevalidatedDestinations(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	mustWriteFile(t, configPath, strings.Replace(string(configBytes), "build/demo.mll", "build/package", 1))
	mustWriteFile(t, filepath.Join(dir, "build", "package", "initial", "plugin.py"), "# Initial Python plug-in\n")

	root := "C:/maya-stall/trusted-plugin-artifacts"
	repairedPrefs := `// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts/build/package"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts/build/package/initial";
`
	sshPath := filepath.Join(dir, "fake-ssh-prevalidated-trust-paths")
	countPath := filepath.Join(dir, "fake-ssh-prevalidated-trust-paths.count")
	inputPath := filepath.Join(dir, "repair-input.txt")
	latePluginPath := filepath.Join(dir, "build", "package", "late", "plugin.py")
	sshScript := fmt.Sprintf(`#!/bin/sh
count=0
if [ -f %[1]s ]; then
  count=$(cat %[1]s)
fi
count=$((count + 1))
printf '%%s\n' "$count" > %[1]s
case "$count" in
1)
  mkdir -p %[2]s
  printf '%%s\n' '# Late Python plug-in' > %[3]s
  printf '%%s\n' 'maya-stall-ssh-ok'
  ;;
2)
  printf '%%s\n' 'writable'
  ;;
3)
  printf '%%s\n' 'maya-version:2025'
  ;;
4)
  ;;
5)
  cat > %[4]s
  cat <<'JSON'
%[5]s
JSON
  ;;
*)
  printf 'unexpected SSH call %%s\n' "$count" >&2
  exit 1
  ;;
esac
`, shellQuote(countPath), shellQuote(filepath.Dir(latePluginPath)), shellQuote(latePluginPath), shellQuote(inputPath), trustedPluginPrefsProbeOutputChanged(repairedPrefs, true))
	if err := os.WriteFile(sshPath, []byte(sshScript), 0o755); err != nil {
		t.Fatalf("write prevalidated-path fake SSH command: %v", err)
	}
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, "")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke", "--repair-trusted-plugin-allowlist"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	streamedInput, err := os.ReadFile(inputPath)
	if err != nil {
		t.Fatalf("read repair input: %v", err)
	}
	var envelope struct {
		RequiredPaths string `json:"requiredPaths"`
	}
	if err := json.Unmarshal(streamedInput, &envelope); err != nil {
		t.Fatalf("parse repair input envelope: %v", err)
	}
	inputJSON, err := base64.StdEncoding.DecodeString(envelope.RequiredPaths)
	if err != nil {
		t.Fatalf("decode repair input: %v", err)
	}
	var requiredPaths []string
	if err := json.Unmarshal(inputJSON, &requiredPaths); err != nil {
		t.Fatalf("parse repair input: %v", err)
	}
	want := []string{root, root + "/build/package", root + "/build/package/initial"}
	if !reflect.DeepEqual(requiredPaths, want) {
		t.Fatalf("repair paths = %#v, want prevalidated snapshot %#v", requiredPaths, want)
	}
}

func TestDoctorRepairTrustedPluginAllowlistSkipsLiveBrokerProbes(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshLog := filepath.Join(dir, "ssh.log")
	repairedPrefs := `// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts";
`
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		`maya-stall-ssh-ok`,
		`writable`,
		`maya-version:2025`,
		``,
		trustedPluginPrefsProbeOutputChanged(repairedPrefs, true),
	})
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--repair-trusted-plugin-allowlist"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"maya-version: ok - 2025",
		"trusted-plugin-allowlist: ok - Maya 2025 SafeModeAllowedlistPaths contains trustedPluginArtifactsRoot after repair",
		"session-broker: ok - skipped during TrustCenter repair",
		"visual-evidence: ok - skipped during TrustCenter repair",
		"desktop-control: ok - skipped during TrustCenter repair",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
	logBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read fake ssh log: %v", err)
	}
	if strings.Contains(string(logBytes), "gg_mayasessiond") || strings.Contains(string(logBytes), "viewport.capture") {
		t.Fatalf("repair doctor should not run live broker probes:\n%s", string(logBytes))
	}
}

func TestTrustedPluginAllowlistRepairPreservesExistingEntriesWhenMayaStopped(t *testing.T) {
	dir := t.TempDir()
	repairedPrefs := `// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/elsewhere/plugins"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts";
`
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		``,
		trustedPluginPrefsProbeOutputChanged(repairedPrefs, true),
	})
	host := mayaHostConfig{
		Transport:                  "ssh",
		SSH:                        sshConfig{Host: "maya-win-01", Binary: sshPath},
		TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts",
	}

	requiredPaths := []string{"C:/maya-stall/trusted-plugin-artifacts"}
	changed, err := ensureTrustedPluginAllowlist(host, []string{"2025"}, requiredPaths, true)
	if err != nil {
		t.Fatalf("repair allowlist error: %v", err)
	}
	if !changed {
		t.Fatalf("repair allowlist changed = false, want true")
	}
	if paths := parseSafeModeAllowedlistPaths(repairedPrefs); !reflect.DeepEqual(paths, []string{"C:/elsewhere/plugins", "C:/maya-stall/trusted-plugin-artifacts"}) {
		t.Fatalf("repaired allowlist entries = %#v", paths)
	}
}

func TestTrustedPluginAllowlistRejectsUnsafeMayaVersionPathSegment(t *testing.T) {
	host := mayaHostConfig{
		Transport:                  "ssh",
		TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts",
	}

	_, err := ensureTrustedPluginAllowlist(host, []string{`..\outside`}, []string{"C:/maya-stall/trusted-plugin-artifacts"}, false)
	if err == nil || !strings.Contains(err.Error(), "not a safe preferences path segment") {
		t.Fatalf("unsafe Maya version error = %v", err)
	}
}

func TestTrustedPluginAllowlistRequiresDeclaredMayaVersion(t *testing.T) {
	host := mayaHostConfig{
		Transport:                  "ssh",
		TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts",
		MayaVersions:               []string{""},
	}

	_, err := ensureTrustedPluginAllowlist(host, trustedPluginAllowlistMayaVersions(host, scenarioConfig{}), []string{"C:/maya-stall/trusted-plugin-artifacts"}, false)
	if err == nil || !strings.Contains(err.Error(), "maya version is required") {
		t.Fatalf("missing Maya version error = %v", err)
	}
}

func TestTrustedPluginAllowlistAcceptsPointReleaseMayaVersion(t *testing.T) {
	if err := validateMayaPrefsVersion("2016.5"); err != nil {
		t.Fatalf("point-release Maya version rejected: %v", err)
	}
}

func TestTrustedPluginAllowlistRequiredPathsIncludeNestedPluginParent(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "build", "maya2025", "Release", "demo.mll"), "fake nested plugin artifact\n")
	host := mayaHostConfig{
		TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts",
	}
	payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "build/maya2025/Release/demo.mll"}}

	paths, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
	if err != nil {
		t.Fatalf("required allowlist paths error: %v", err)
	}
	want := []string{
		"C:/maya-stall/trusted-plugin-artifacts",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("required allowlist paths = %#v, want %#v", paths, want)
	}
}

func TestTrustedPluginAllowlistRequiredPathsIncludeNestedPluginParents(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "build", "maya2025", "Release", "package", "scripts", "plugin", "tool.py"), "def initializePlugin(plugin):\n    pass\n\ndef uninitializePlugin(plugin):\n    pass\n")
	mustWriteFile(t, filepath.Join(dir, "build", "maya2025", "Release", "package", "scripts", "delegated", "loader.py"), "try:\n    from .implementation import initializePlugin, uninitializePlugin\nexcept ImportError:\n    from implementation import initializePlugin, uninitializePlugin\n")
	mustWriteFile(t, filepath.Join(dir, "build", "maya2025", "Release", "package", "scripts", "star", "loader.py"), "from package.implementation import *\n")
	mustWriteFile(t, filepath.Join(dir, "build", "maya2025", "Release", "package", "scripts", "tuple", "loader.py"), "initializePlugin, uninitializePlugin = make_callbacks()\n")
	mustWriteFile(t, filepath.Join(dir, "build", "maya2025", "Release", "package", "scripts", "support", "helper.py"), "# Documents initializePlugin and uninitializePlugin without exporting them.\ndef initializePluginForTest():\n    pass\n\ndef uninitializePluginForTest():\n    pass\n")
	host := mayaHostConfig{
		TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts",
	}
	payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "build/maya2025/Release/package"}}

	paths, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
	if err != nil {
		t.Fatalf("required allowlist paths error: %v", err)
	}
	want := []string{
		"C:/maya-stall/trusted-plugin-artifacts",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release/package",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release/package/scripts/delegated",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release/package/scripts/plugin",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release/package/scripts/star",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release/package/scripts/support",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release/package/scripts/tuple",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("required allowlist paths = %#v, want %#v", paths, want)
	}
}

func TestTrustedPluginAllowlistUsesFrozenRunPayloadSnapshot(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "package", "plugin.py"), "# original plug-in\n")
	payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "package", Staged: filepath.Join("payload", "pluginArtifacts", "package")}}
	workspace, err := newRunWorkspace(dir, "run-1", "C:/maya-stall", "scenario-result.json")
	if err != nil {
		t.Fatalf("create run workspace: %v", err)
	}
	context := runContext{
		RepoDir:      dir,
		RunWorkspace: workspace,
		StateDir:     workspace.StateDir(),
		EvidenceDir:  workspace.EvidenceDir(),
		Workspace:    workspace.LocalWorkspace(),
		EventsPath:   workspace.EventsPath(),
		LogPath:      workspace.LogPath(),
	}
	if err := createCleanRunDirs(context); err != nil {
		t.Fatalf("create run dirs: %v", err)
	}
	if err := snapshotRunPayload(context, payload); err != nil {
		t.Fatalf("snapshot run payload: %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, "package", "late", "plugin.py"), "# late plug-in\n")

	host := mayaHostConfig{TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts"}
	paths, err := trustedPluginAllowlistRequiredPathsFromSnapshot(context, host, payload)
	if err != nil {
		t.Fatalf("inspect snapshot allowlist paths: %v", err)
	}
	want := []string{
		"C:/maya-stall/trusted-plugin-artifacts",
		"C:/maya-stall/trusted-plugin-artifacts/package",
	}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("snapshot allowlist paths = %#v, want %#v", paths, want)
	}
	if _, err := os.Stat(filepath.Join(workspace.LocalPayloadPath(payload[0]), "late", "plugin.py")); !os.IsNotExist(err) {
		t.Fatalf("payload snapshot changed after repo mutation: %v", err)
	}
}

func TestTrustedPluginAllowlistRequiredPathsRejectTransportControlCharacters(t *testing.T) {
	for index, nestedDir := range []string{"scripts\nbad", "bad\n", "\nbad"} {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, "package", nestedDir, "plugin.py"), "# Python payload\n")
			host := mayaHostConfig{TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts"}
			payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "package"}}

			_, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
			if err == nil || !strings.Contains(err.Error(), "control characters") {
				t.Fatalf("required allowlist paths error = %v, want transport control-character rejection", err)
			}
		})
	}
}

func TestTrustedPluginAllowlistRequiredPathsRejectWin32Aliases(t *testing.T) {
	for _, test := range []struct {
		name      string
		root      string
		nestedDir string
	}{
		{name: "drive trailing period", root: "C:/maya-stall/trusted-plugin-artifacts", nestedDir: "scripts."},
		{name: "drive trailing space", root: "C:/maya-stall/trusted-plugin-artifacts", nestedDir: "scripts "},
		{name: "UNC trailing period", root: "//server/share/trusted-plugin-artifacts", nestedDir: "scripts."},
		{name: "UNC trailing space", root: "//server/share/trusted-plugin-artifacts", nestedDir: "scripts "},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, "package", test.nestedDir, "plugin.py"), "# Python payload\n")
			host := mayaHostConfig{TrustedPluginArtifactsRoot: test.root}
			payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "package"}}

			_, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
			if err == nil || !strings.Contains(err.Error(), "Windows path components ending in a space or period") {
				t.Fatalf("required allowlist paths error = %v, want Win32 alias rejection", err)
			}
		})
	}
}

func TestTrustedPluginAllowlistRequiredPathsRejectInvalidWin32Components(t *testing.T) {
	for _, nestedDir := range []string{"scripts?", "bad:name", "CON", "aux.txt", "COM¹", "lpt³.txt"} {
		t.Run(nestedDir, func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, "package", nestedDir, "plugin.py"), "# Python payload\n")
			host := mayaHostConfig{TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts"}
			payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "package"}}

			_, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
			if err == nil || !strings.Contains(err.Error(), "invalid Windows path component") {
				t.Fatalf("required allowlist paths error = %v, want invalid Win32 component rejection", err)
			}
		})
	}
}

func TestTrustedPluginAllowlistValidationErrorsRedactConfiguredRoot(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "package", "COM¹", "plugin.py"), "# Python payload\n")
	root := "D:/private-live-host/trusted-plugin-artifacts"
	host := mayaHostConfig{TrustedPluginArtifactsRoot: root}
	payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "package"}}

	_, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
	if err == nil {
		t.Fatal("required allowlist paths accepted invalid Win32 component")
	}
	if strings.Contains(err.Error(), root) {
		t.Fatalf("validation error disclosed configured trusted root: %v", err)
	}
	if !strings.Contains(err.Error(), "package/COM¹") {
		t.Fatalf("validation error missing repo-relative artifact path: %v", err)
	}
}

func TestTrustedPluginAllowlistRequiredPathsRejectInvalidWin32ArtifactLeaves(t *testing.T) {
	for _, test := range []struct {
		name    string
		source  string
		isDir   bool
		content string
	}{
		{name: "file artifact", source: "build/demo?.mll", content: "fake plug-in\n"},
		{name: "reserved file artifact", source: "build/CON.mll", content: "fake plug-in\n"},
		{name: "nested plugin leaf", source: "package/plugin?.py", isDir: true, content: "# Python plug-in\n"},
		{name: "nested support leaf", source: "package/notes?.txt", isDir: true, content: "support\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, filepath.FromSlash(test.source)), test.content)
			payloadSource := test.source
			if test.isDir {
				payloadSource = "package"
			}
			host := mayaHostConfig{TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts"}
			payload := []manifestPayload{{Kind: "pluginArtifacts", Source: payloadSource}}

			_, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
			if err == nil || !strings.Contains(err.Error(), "invalid Windows path component") {
				t.Fatalf("required allowlist paths error = %v, want invalid artifact leaf rejection", err)
			}
		})
	}
}

func TestTrustedPluginAllowlistRequiredPathsRejectUnsafeRawArtifactLeaves(t *testing.T) {
	for _, test := range []struct {
		name      string
		entry     string
		emptyDir  bool
		wantError string
	}{
		{name: "file terminal space", entry: "build/demo.mll ", wantError: "ending in a space or period"},
		{name: "file control character", entry: "build/demo.mll\n", wantError: "control characters"},
		{name: "support terminal space", entry: "package/notes.txt ", wantError: "ending in a space or period"},
		{name: "empty directory terminal period", entry: "package/cache.", emptyDir: true, wantError: "ending in a space or period"},
		{name: "nested backslash", entry: `package/scripts\bad/plugin.py`, wantError: "forward slashes"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			entryPath := filepath.Join(dir, filepath.FromSlash(test.entry))
			if test.emptyDir {
				if err := os.MkdirAll(entryPath, 0o755); err != nil {
					t.Fatalf("create artifact directory: %v", err)
				}
			} else {
				mustWriteFile(t, entryPath, "artifact\n")
			}
			payloadSource := test.entry
			if strings.HasPrefix(test.entry, "package/") {
				payloadSource = "package"
			}
			host := mayaHostConfig{TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts"}
			payload := []manifestPayload{{Kind: "pluginArtifacts", Source: payloadSource}}

			_, err := trustedPluginAllowlistRequiredPaths(dir, host, payload)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("required allowlist paths error = %v, want %q", err, test.wantError)
			}
		})
	}
}

func TestRunTrustedPluginAllowlistChecksAllHostMayaVersionsWhenScenarioUnpinned(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "build", "demo.mll"), "fake plugin artifact\n")
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts/build";
`),
		trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts/build";
`),
	})
	host := mayaHostConfig{
		Transport:                  "ssh",
		SSH:                        sshConfig{Host: "maya-win-01", Binary: sshPath},
		TrustedPluginArtifactsRoot: "C:/maya-stall/trusted-plugin-artifacts",
		MayaVersions:               []string{"2024", "2025"},
	}
	payload := []manifestPayload{{Kind: "pluginArtifacts", Source: "build/demo.mll", Staged: filepath.Join("payload", "pluginArtifacts", "build", "demo.mll")}}
	scenario := scenarioContract{Config: scenarioConfig{}, Payload: payload}
	workspace, err := newRunWorkspace(dir, "run-1", "C:/maya-stall", "scenario-result.json")
	if err != nil {
		t.Fatalf("create run workspace: %v", err)
	}
	context := runContext{
		RepoDir:      dir,
		RunWorkspace: workspace,
		StateDir:     workspace.StateDir(),
		EvidenceDir:  workspace.EvidenceDir(),
		Workspace:    workspace.LocalWorkspace(),
		EventsPath:   workspace.EventsPath(),
		LogPath:      workspace.LogPath(),
	}
	if err := createCleanRunDirs(context); err != nil {
		t.Fatalf("create run dirs: %v", err)
	}
	if err := snapshotRunPayload(context, payload); err != nil {
		t.Fatalf("snapshot run payload: %v", err)
	}

	if err := ensureTrustedPluginArtifactSnapshotAllowlistedForRun(context, host, scenario); err != nil {
		t.Fatalf("run allowlist preflight error: %v", err)
	}
	countBytes, err := os.ReadFile(filepath.Join(dir, "fake-ssh-sequenced.count"))
	if err != nil {
		t.Fatalf("read fake ssh count: %v", err)
	}
	if strings.TrimSpace(string(countBytes)) != "2" {
		t.Fatalf("allowlist preflight did not check every declared Maya version, fake SSH count = %s", string(countBytes))
	}
}

func TestTrustedPluginPrefsRepairScriptDocumentsMutationSafety(t *testing.T) {
	script, err := trustedPluginPrefsRepairScript(mayaHostConfig{}, "2025")
	if err != nil {
		t.Fatalf("build trusted plug-in prefs repair script: %v", err)
	}
	for _, want := range []string{
		"$env:MAYA_APP_DIR",
		"$MayaStallRequiredPathsInput",
		"ConvertFrom-Json",
		"ConvertFrom-Json | ForEach-Object { $_ }",
		"[Environment]::GetFolderPath('MyDocuments')",
		"[System.IO.Path]::GetFullPath",
		"Copy-Item -LiteralPath $prefs",
		"[regex]::Matches($content",
		"System.Collections.Generic.HashSet[string]",
		"System.Text.StringBuilder",
		"$normalizedPaths.Add($wanted)",
		"$paths.Add($requiredPath)",
		"Add-Content -LiteralPath $prefs",
		"SafeModeAllowedlistPaths",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("repair script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "New-Item -ItemType Directory") {
		t.Fatalf("repair script should not fabricate missing Maya prefs:\n%s", script)
	}
	if strings.Contains(script, "C:/maya-stall/trusted-plugin-artifacts") {
		t.Fatalf("repair script must receive required paths over stdin, not the command line:\n%s", script)
	}
	for _, quadratic := range []string{"$paths.Contains($entry)", "$block +="} {
		if strings.Contains(script, quadratic) {
			t.Fatalf("repair script contains quadratic operation %q:\n%s", quadratic, script)
		}
	}
	requiredLoop := strings.Index(script, "foreach ($requiredPath in $requiredPaths)")
	if requiredLoop < 0 {
		t.Fatalf("repair script missing required-path loop:\n%s", script)
	}
	writeBlock := strings.Index(script[requiredLoop:], "if ($changed)")
	if writeBlock < 0 || strings.Contains(script[requiredLoop:requiredLoop+writeBlock], "foreach ($entry in $paths)") {
		t.Fatalf("repair script must not rescan every path for each requirement:\n%s", script)
	}
}

func TestTrustedPluginPrefsUseSessionBrokerMayaAppDir(t *testing.T) {
	host := mayaHostConfig{Broker: brokerConfig{
		Structured: true,
		Type:       "gg-mayasessiond",
		StateDir:   "C:/maya-stall/sessiond-ui",
		Repo:       "C:/maya-stall/tools/GG_MayaSessiond",
	}}
	mayaAppDir, err := trustedPluginMayaAppDir(host)
	if err != nil {
		t.Fatalf("resolve trusted plug-in Maya app dir: %v", err)
	}
	if mayaAppDir != "C:/maya-stall/sessiond-ui/maya_app" {
		t.Fatalf("trusted plug-in Maya app dir = %q, want Session Broker Maya app dir", mayaAppDir)
	}
	preamble, err := trustedPluginPrefsScriptPreamble(host)
	if err != nil {
		t.Fatalf("build trusted plug-in prefs preamble: %v", err)
	}
	if !strings.Contains(preamble, "$mayaAppDir = 'C:/maya-stall/sessiond-ui/maya_app'") {
		t.Fatalf("trusted plug-in prefs script does not target Session Broker Maya app dir:\n%s", preamble)
	}
	host.Broker.StateDir = "sessiond-ui"
	host.Broker.Repo = "C:/maya-stall/tools/GG_MayaSessiond"
	mayaAppDir, err = trustedPluginMayaAppDir(host)
	if err != nil {
		t.Fatalf("resolve relative trusted plug-in Maya app dir: %v", err)
	}
	if mayaAppDir != "C:/maya-stall/tools/GG_MayaSessiond/sessiond-ui/maya_app" {
		t.Fatalf("relative broker state Maya app dir = %q, want path resolved from broker repo", mayaAppDir)
	}
}

func TestTrustedPluginPrefsRejectAmbiguousSessionBrokerStateDir(t *testing.T) {
	for _, stateDir := range []string{`\sessiond-ui`, `/sessiond-ui`, `C:sessiond-ui`} {
		t.Run(stateDir, func(t *testing.T) {
			host := mayaHostConfig{
				Transport: "ssh",
				SSH:       sshConfig{Host: "maya-win-01"},
				WorkRoot:  "C:/maya-stall",
				Broker: brokerConfig{
					Structured: true,
					Type:       "gg-mayasessiond",
					StateDir:   stateDir,
					Python:     "C:/Python/python.exe",
					Repo:       "C:/maya-stall/tools/GG_MayaSessiond",
				}}
			if _, err := trustedPluginPrefsReadScript(host, "2025"); err == nil {
				t.Fatalf("trusted plug-in prefs accepted ambiguous broker stateDir %q", stateDir)
			}
			if err := (ggMayaSessiondBroker{host: host}).validate(); err == nil {
				t.Fatalf("Session Broker accepted ambiguous stateDir %q", stateDir)
			}
		})
	}
}

func TestTrustedPluginPrefsRejectWhitespaceAmbiguousSessionBrokerPaths(t *testing.T) {
	for name, mutate := range map[string]func(*mayaHostConfig){
		"state-dir-leading-space":  func(host *mayaHostConfig) { host.Broker.StateDir = " sessiond-ui" },
		"state-dir-trailing-space": func(host *mayaHostConfig) { host.Broker.StateDir = "sessiond-ui " },
		"repo-leading-space":       func(host *mayaHostConfig) { host.Broker.Repo = " C:/maya-stall/tools/GG_MayaSessiond" },
		"repo-trailing-space":      func(host *mayaHostConfig) { host.Broker.Repo = "C:/maya-stall/tools/GG_MayaSessiond " },
	} {
		t.Run(name, func(t *testing.T) {
			host := mayaHostConfig{
				Transport: "ssh",
				SSH:       sshConfig{Host: "maya-win-01"},
				WorkRoot:  "C:/maya-stall",
				Broker: brokerConfig{
					Structured: true,
					Type:       "gg-mayasessiond",
					StateDir:   "sessiond-ui",
					Python:     "C:/Python/python.exe",
					Repo:       "C:/maya-stall/tools/GG_MayaSessiond",
				}}
			mutate(&host)
			if _, err := trustedPluginPrefsReadScript(host, "2025"); err == nil {
				t.Fatalf("trusted plug-in prefs accepted whitespace-ambiguous broker paths: %#v", host.Broker)
			}
			if err := (ggMayaSessiondBroker{host: host}).validate(); err == nil {
				t.Fatalf("Session Broker accepted whitespace-ambiguous paths: %#v", host.Broker)
			}
		})
	}
}

func TestTrustedPluginPrefsRejectAmbiguousSessionBrokerRepo(t *testing.T) {
	for _, repo := range []string{`\tools`, `/tools`, `C:tools`, `\\?\C:\maya-stall\tools`, `C:/../tools`, `C:/maya-stall/CON`} {
		t.Run(repo, func(t *testing.T) {
			host := mayaHostConfig{
				Transport: "ssh",
				SSH:       sshConfig{Host: "maya-win-01"},
				WorkRoot:  "C:/maya-stall",
				Broker: brokerConfig{
					Structured: true,
					Type:       "gg-mayasessiond",
					StateDir:   "sessiond-ui",
					Python:     "C:/Python/python.exe",
					Repo:       repo,
				}}
			if _, err := trustedPluginPrefsReadScript(host, "2025"); err == nil {
				t.Fatalf("trusted plug-in prefs accepted ambiguous broker repo %q", repo)
			}
			if err := (ggMayaSessiondBroker{host: host}).validate(); err == nil {
				t.Fatalf("Session Broker accepted ambiguous repo %q", repo)
			}
		})
	}
}

func TestTrustedPluginPrefsRepairInputKeepsLargePathSetsOutOfCommandLine(t *testing.T) {
	paths := make([]string, 0, 1_000)
	for index := range 1_000 {
		paths = append(paths, fmt.Sprintf("C:/maya-stall/trusted-plugin-artifacts/package-%04d/scripts", index))
	}

	input, err := trustedPluginPrefsRepairInput(paths)
	if err != nil {
		t.Fatalf("build repair input: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(input)
	if err != nil {
		t.Fatalf("decode repair input: %v", err)
	}
	var got []string
	if err := json.Unmarshal(decoded, &got); err != nil {
		t.Fatalf("parse repair input JSON: %v", err)
	}
	if !reflect.DeepEqual(got, compactTrustedPluginAllowlistPaths(paths)) {
		t.Fatalf("repair input paths differ: got %d paths, want %d", len(got), len(paths))
	}
	script, err := trustedPluginPrefsRepairScript(mayaHostConfig{}, "2025")
	if err != nil {
		t.Fatalf("build trusted plug-in prefs repair script: %v", err)
	}
	if strings.Contains(script, paths[len(paths)-1]) {
		t.Fatalf("repair command embeds a required path")
	}
}

func TestTrustedPluginPrefsRepairStreamsPowerShellProgram(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args.txt")
	stdinPath := filepath.Join(dir, "stdin.txt")
	sshPath := filepath.Join(dir, "fake-ssh-repair-stream")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s' \"$*\" > %s\ncat > %s\nprintf '%%s\\n' '%s{\"exists\":true,\"content\":\"\",\"changed\":true}'\n", shellQuote(argsPath), shellQuote(stdinPath), trustedPluginPrefsJSONPrefix)
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-win-01", Binary: sshPath}}
	paths := []string{
		"C:/maya-stall/trusted-plugin-artifacts",
		"C:/maya-stall/trusted-plugin-artifacts/build/maya2025/Release/klv_push/scripts/klv_push",
	}

	if _, err := trustedPluginPrefs(host, "2025", paths, true); err != nil {
		t.Fatalf("repair trusted plug-in prefs: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read SSH args: %v", err)
	}
	if !strings.Contains(string(args), "-EncodedCommand") || strings.Contains(string(args), "Copy-Item") {
		t.Fatalf("repair SSH args must use a short encoded bootstrap, got %q", string(args))
	}
	if len(args) > 2_048 {
		t.Fatalf("repair SSH args length = %d, want <= 2048", len(args))
	}
	stdin, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read SSH stdin: %v", err)
	}
	var envelope struct {
		Program       string `json:"program"`
		RequiredPaths string `json:"requiredPaths"`
	}
	if err := json.Unmarshal(stdin, &envelope); err != nil {
		t.Fatalf("parse repair stdin envelope: %v", err)
	}
	program, err := base64.StdEncoding.DecodeString(envelope.Program)
	if err != nil {
		t.Fatalf("decode repair PowerShell program: %v", err)
	}
	if !strings.Contains(string(program), "Copy-Item -LiteralPath $prefs") {
		t.Fatalf("repair PowerShell program was not streamed over stdin")
	}
	if strings.Contains(string(args), paths[len(paths)-1]) || strings.Contains(string(stdin), paths[len(paths)-1]) {
		t.Fatalf("repair transport exposed a raw required path")
	}
}

func TestRunSSHCommandOutputWithInputStreamsStdin(t *testing.T) {
	dir := t.TempDir()
	stdinPath := filepath.Join(dir, "stdin.txt")
	sshPath := filepath.Join(dir, "fake-ssh-stdin")
	script := fmt.Sprintf("#!/bin/sh\ncat > %s\nprintf 'ok'\n", shellQuote(stdinPath))
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-win-01", Binary: sshPath}}

	raw, err := runSSHCommandOutputWithInput(host, []string{"ignored"}, "large-input-payload", sshCommandTimeout)
	if err != nil {
		t.Fatalf("run SSH with stdin: %v", err)
	}
	if string(raw) != "ok" {
		t.Fatalf("SSH output = %q, want ok", string(raw))
	}
	got, err := os.ReadFile(stdinPath)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	if string(got) != "large-input-payload" {
		t.Fatalf("SSH stdin = %q", string(got))
	}
}

func TestRunSSHCommandOutputRedactsConfiguredEndpointFromFailure(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-private-endpoint")
	script := "#!/bin/sh\nprintf 'runner-private: ssh: connect to host maya-private.example port 22: Operation timed out\\n' >&2\nexit 255\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{
		Host: "maya-private.example", User: "runner-private", IdentityFile: "/private/identity", Binary: sshPath,
	}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("failed SSH command returned no error")
	}
	for _, private := range []string{host.SSH.Host, host.SSH.User, host.SSH.IdentityFile} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("SSH error exposed configured endpoint detail %q: %v", private, err)
		}
	}
	if !strings.Contains(err.Error(), "SSH operation reported an error") {
		t.Fatalf("SSH error missing public-safe diagnostic: %v", err)
	}
}

func TestRunSSHCommandOutputRedactsBeforeTruncatingFailure(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-long-private-endpoint")
	hostName := strings.Repeat("private-endpoint-", 20) + ".example"
	script := fmt.Sprintf("#!/bin/sh\nprintf %%s\\n %s >&2\nexit 255\n", shellQuote("ssh: connect to host "+hostName+" port 22: Operation timed out"))
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: hostName, Binary: sshPath}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("failed SSH command returned no error")
	}
	if strings.Contains(err.Error(), "private-endpoint-") || !strings.Contains(err.Error(), "SSH operation reported an error") {
		t.Fatalf("SSH error did not redact before truncation: %v", err)
	}
}

func TestRunSSHCommandOutputRedactsConfiguredExecutableStartFailure(t *testing.T) {
	privateBinary := filepath.Join(t.TempDir(), "private-ssh-binary")
	host := mayaHostConfig{SSH: sshConfig{Host: "maya.example", Binary: privateBinary}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("missing SSH executable returned no error")
	}
	if strings.Contains(err.Error(), privateBinary) || !strings.Contains(err.Error(), "[SSH executable]") {
		t.Fatalf("SSH start error exposed configured executable: %v", err)
	}
}

func TestRunSSHCommandOutputDoesNotForwardResolvedEndpoint(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-resolved-endpoint")
	script := "#!/bin/sh\nprintf 'ssh: connect to host 203.0.113.10 port 2222: Operation timed out\\n' >&2\nexit 255\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-alias", Port: 2222, Binary: sshPath}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("failed SSH command returned no error")
	}
	for _, private := range []string{"203.0.113.10", "2222"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("SSH error forwarded effective endpoint detail %q: %v", private, err)
		}
	}
}

func TestRunSSHCommandOutputKeepsPublicLabelWithShortUsername(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-short-user")
	script := "#!/bin/sh\nprintf 'ssh: connect to host maya.example port 22: Operation timed out\\n' >&2\nexit 255\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya.example", User: "a", Binary: sshPath}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("failed SSH command returned no error")
	}
	if !strings.Contains(err.Error(), "SSH operation reported an error") {
		t.Fatalf("short SSH username corrupted public-safe diagnostic: %v", err)
	}
}

func TestRunSSHCommandOutputKeepsRemoteFailurePrivateForRecoveryClassification(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-command-port-failure")
	script := "#!/bin/sh\nprintf 'Cannot connect to Maya commandPort at localhost:7001\\n' >&2\nexit 1\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-win-01", Binary: sshPath}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("failed remote command returned no error")
	}
	if strings.Contains(strings.ToLower(err.Error()), "commandport") || strings.Contains(err.Error(), "localhost:7001") {
		t.Fatalf("public SSH error exposed private remote failure detail: %v", err)
	}
	if !isCommandPortError(err) {
		t.Fatalf("private remote failure was unavailable for recovery classification: %v", err)
	}
}

func TestRunSSHCommandOutputDoesNotMisclassifyRemoteFailureAsTransportFailure(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-remote-permission-failure")
	script := "#!/bin/sh\nprintf 'remote tool: permission denied for private resource\\n' >&2\nexit 1\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-win-01", Binary: sshPath}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("failed remote command returned no error")
	}
	if strings.Contains(err.Error(), "authentication failed") || !strings.Contains(err.Error(), "SSH operation reported an error") {
		t.Fatalf("remote failure was misclassified as an SSH transport failure: %v", err)
	}
	if strings.Contains(err.Error(), "private resource") {
		t.Fatalf("public SSH error exposed private remote failure detail: %v", err)
	}
}

func TestRunSSHCommandOutputKeepsAmbiguousExit255Generic(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-remote-exit-255")
	script := "#!/bin/sh\nprintf 'remote tool: permission denied for private resource\\n' >&2\nexit 255\n"
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-win-01", Binary: sshPath}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil {
		t.Fatal("failed remote command returned no error")
	}
	if strings.Contains(err.Error(), "authentication failed") || !strings.Contains(err.Error(), "SSH operation reported an error") {
		t.Fatalf("ambiguous exit 255 was misclassified as an SSH transport failure: %v", err)
	}
	if strings.Contains(err.Error(), "private resource") {
		t.Fatalf("public SSH error exposed private remote failure detail: %v", err)
	}
}

func TestRunSSHCommandOutputPreservesCanonicalDesktopDiagnostics(t *testing.T) {
	tests := []string{
		"schtasks.exe is required for interactive desktop control",
		"interactive desktop session is unavailable for screenshot capture",
	}
	for _, diagnostic := range tests {
		t.Run(diagnostic, func(t *testing.T) {
			dir := t.TempDir()
			sshPath := filepath.Join(dir, "fake-ssh-canonical-diagnostic")
			script := fmt.Sprintf("#!/bin/sh\nprintf %%s\\n %s >&2\nexit 1\n", shellQuote(diagnostic))
			if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
				t.Fatalf("write fake SSH: %v", err)
			}
			host := mayaHostConfig{SSH: sshConfig{Host: "maya-win-01", Binary: sshPath}}

			_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
			if err == nil || !strings.Contains(err.Error(), diagnostic) {
				t.Fatalf("SSH error did not preserve canonical product diagnostic %q: %v", diagnostic, err)
			}
		})
	}
}

func TestRunSSHCommandOutputFindsCanonicalDiagnosticAfterPowerShellPreamble(t *testing.T) {
	dir := t.TempDir()
	sshPath := filepath.Join(dir, "fake-ssh-powershell-preamble")
	diagnostic := "interactive desktop session is unavailable for recording capture"
	script := fmt.Sprintf("#!/bin/sh\nprintf 'OpenSSH warning\\n#< CLIXML\\n<Objs>%%s</Objs>\\n' %s >&2\nexit 1\n", shellQuote(diagnostic))
	if err := os.WriteFile(sshPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SSH: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-win-01", Binary: sshPath}}

	_, err := runSSHCommandOutput(host, []string{"ignored"}, sshCommandTimeout)
	if err == nil || !strings.Contains(err.Error(), diagnostic) {
		t.Fatalf("SSH error did not preserve canonical diagnostic after PowerShell preamble: %v", err)
	}
}

func TestRunSFTPBatchUsesPublicSafeSFTPDiagnostic(t *testing.T) {
	dir := t.TempDir()
	sftpPath := filepath.Join(dir, "fake-sftp-private-endpoint")
	script := "#!/bin/sh\nprintf 'ssh: connect to host 203.0.113.10 port 2222: Operation timed out\\n' >&2\nexit 255\n"
	if err := os.WriteFile(sftpPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake SFTP: %v", err)
	}
	host := mayaHostConfig{SSH: sshConfig{Host: "maya-alias", Port: 2222, SFTPBinary: sftpPath}}

	err := runSFTPBatch(host, "quit\n")
	if err == nil {
		t.Fatal("failed SFTP command returned no error")
	}
	if strings.Contains(err.Error(), "203.0.113.10") || strings.Contains(err.Error(), "2222") {
		t.Fatalf("SFTP error exposed effective endpoint: %v", err)
	}
	if strings.Contains(err.Error(), "SSH operation") || !strings.Contains(err.Error(), "SFTP operation reported an error") {
		t.Fatalf("SFTP error used an incorrect public-safe diagnostic: %v", err)
	}

	privateBinary := filepath.Join(dir, "private-missing-sftp")
	host.SSH.SFTPBinary = privateBinary
	err = runSFTPBatch(host, "quit\n")
	if err == nil {
		t.Fatal("missing SFTP executable returned no error")
	}
	if strings.Contains(err.Error(), privateBinary) || strings.Contains(err.Error(), "[SSH executable]") || !strings.Contains(err.Error(), "[SFTP executable]") {
		t.Fatalf("SFTP start error used an incorrect public-safe executable label: %v", err)
	}
}

func TestRunSFTPBatchReconnectsAfterExit255(t *testing.T) {
	oldDelay := sftpReconnectDelay
	sftpReconnectDelay = 0
	t.Cleanup(func() { sftpReconnectDelay = oldDelay })
	exit255 := commandExitError(t, 255)
	calls := 0
	err := runSFTPBatchWithReconnectUsing(func() error {
		calls++
		if calls == 1 {
			return fmt.Errorf("sftp command failed: %w", exit255)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("SFTP collection did not reconnect after exit 255: %v", err)
	}
	if calls != 2 {
		t.Fatalf("SFTP calls = %d; want one reconnect", calls)
	}
}

func TestRunSFTPBatchDoesNotRetryOrdinaryCollectionFailure(t *testing.T) {
	oldDelay := sftpReconnectDelay
	sftpReconnectDelay = 0
	t.Cleanup(func() { sftpReconnectDelay = oldDelay })
	calls := 0
	err := runSFTPBatchWithReconnectUsing(func() error {
		calls++
		return errors.New("file not found")
	})
	if err == nil {
		t.Fatal("missing SFTP output unexpectedly succeeded")
	}
	if calls != 1 {
		t.Fatalf("SFTP calls = %d; ordinary failure must not reconnect", calls)
	}
}

func commandExitError(t *testing.T, code int) error {
	t.Helper()
	var command *exec.Cmd
	if runtime.GOOS == "windows" {
		command = exec.Command("cmd.exe", "/c", "exit", strconv.Itoa(code))
	} else {
		command = exec.Command("sh", "-c", "exit "+strconv.Itoa(code))
	}
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != code {
		t.Fatalf("create exit error %d: %v", code, err)
	}
	return err
}

func TestTrustedPluginAllowlistParsesAndNormalizesMayaPrefs(t *testing.T) {
	prefs := `// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:\\maya-stall\\trusted-plugin-artifacts\\"
 -sva "SafeModeAllowedlistPaths" "C:/other/path";
`
	paths := parseSafeModeAllowedlistPaths(prefs)
	if !reflect.DeepEqual(paths, []string{`C:\maya-stall\trusted-plugin-artifacts\`, "C:/other/path"}) {
		t.Fatalf("parsed allowlist paths = %#v", paths)
	}
	if !prefsAllowlistContainsRoot(prefs, "c:/maya-stall/trusted-plugin-artifacts") {
		t.Fatalf("allowlist did not normalize configured trusted root")
	}
}

func TestTrustedPluginAllowlistPreservesUNCIdentity(t *testing.T) {
	required := "//server/share/plugins"
	content := `// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "/server/share/plugins";
`

	missing := prefsAllowlistMissingPaths(content, []string{required})
	if !reflect.DeepEqual(missing, []string{required}) {
		t.Fatalf("UNC missing paths = %#v, want %#v", missing, []string{required})
	}
	if normalizeTrustedPluginPath(`\\server\share\plugins`) != normalizeTrustedPluginPath(required) {
		t.Fatalf("backslash and slash UNC forms did not normalize equally")
	}
}

func TestTrustedPluginAllowlistUsesEffectiveLatestMayaPrefsArray(t *testing.T) {
	prefs := `// Old Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts";
// Later Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/other/path";
`
	paths := parseSafeModeAllowedlistPaths(prefs)
	if !reflect.DeepEqual(paths, []string{"C:/other/path"}) {
		t.Fatalf("effective allowlist paths = %#v", paths)
	}
	if prefsAllowlistContainsRoot(prefs, "c:/maya-stall/trusted-plugin-artifacts") {
		t.Fatalf("allowlist accepted stale root overwritten by later optionVar reset")
	}
}

func TestDoctorReturnsStableHostHealthReportBeforeRendering(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeLayeredHostConfig(t, dir)

	report := runDoctor(dir, doctorOptions{HostConfig: hostConfigPath, TargetProfile: "ci", HostPin: "alpha"})

	if !report.Healthy {
		t.Fatalf("Host Health healthy = false, checks: %+v", report.Layers)
	}
	if report.TargetProfile != "ci" || report.HostPool != "windows-maya" || report.HostID != "alpha" {
		t.Fatalf("Host Health selection = target %q pool %q host %q", report.TargetProfile, report.HostPool, report.HostID)
	}
	if report.Runtime.Profile != "fake-local" || report.Runtime.HostAdapter != "fake" || report.Runtime.BrokerAdapter != "fake" {
		t.Fatalf("Host Health runtime = %+v, want fake-local", report.Runtime)
	}
	for _, want := range []struct {
		id     string
		status string
		state  string
		source string
	}{
		{id: "local-config", status: "ok"},
		{id: "target-profile", status: "ok"},
		{id: "host-pool", status: "ok"},
		{id: "host", status: "ok"},
		{id: "fake-ssh", status: "ok"},
		{id: "work-root", status: "ok"},
		{id: "runtime", status: "ok", source: "fake-local"},
		{id: "session-broker", status: "ok", source: "fake"},
		{id: "maya-version", status: "ok"},
		{id: "visual-evidence", status: "ok", source: "fake"},
		{id: "desktop-control", status: "ok", source: "fake"},
		{id: "host-lock", status: "ok", state: "unlocked"},
	} {
		check := requireHostHealthLayer(t, report, want.id)
		if check.Status != want.status {
			t.Fatalf("%s status = %q, want %q; layer: %+v", want.id, check.Status, want.status, check)
		}
		if want.state != "" && check.State != want.state {
			t.Fatalf("%s state = %q, want %q; layer: %+v", want.id, check.State, want.state, check)
		}
		if want.source != "" && check.Source != want.source {
			t.Fatalf("%s source = %q, want %q; layer: %+v", want.id, check.Source, want.source, check)
		}
	}
}

func TestDoctorRealSSHVerifiesConnectivityAndWritableWorkRoot(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		`maya-stall-ssh-ok`,
		`writable`,
		`{"ok":true,"checks":[]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":123,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		`removed`,
	})
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: fake-alpha
        health: healthy
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          user: maya-runner
          port: 2222
          identityFile: ~/.ssh/maya-stall-ci
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
        mayaVersions: ["2025"]
        visualEvidence: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"ssh: ok - reachable",
		"work-root: ok - writable",
		"runtime: ok - ssh-sessiond",
		"session-broker: ok - gg_mayasessiond",
		"maya-version: ok - 2025",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
	logBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{"-p", "2222", "-i", filepath.Join(os.Getenv("HOME"), ".ssh", "maya-stall-ci"), "maya-runner@maya-win-01", "powershell"} {
		if !strings.Contains(log, want) {
			t.Fatalf("ssh command log missing %q:\n%s", want, log)
		}
	}
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=10"} {
		if !strings.Contains(log, want) {
			t.Fatalf("ssh command log missing noninteractive option %q:\n%s", want, log)
		}
	}
}

func TestDoctorRealSSHProbesInstalledMayaVersion(t *testing.T) {
	tests := []struct {
		name         string
		probeOutput  string
		mayaVersions string
		wantCode     int
		wantOutput   string
	}{
		{
			name:        "matching version",
			probeOutput: "PowerShell noise\nmaya-version:2025",
			wantCode:    0,
			wantOutput:  "maya-version: ok - 2025 satisfies Scenario smoke",
		},
		{
			name:        "mismatched version",
			probeOutput: "maya-version:2024",
			wantCode:    1,
			wantOutput:  "maya-version: fail - Scenario smoke needs 2025; host has 2024",
		},
		{
			name:        "point release version",
			probeOutput: "maya-version:2016.5",
			wantCode:    1,
			wantOutput:  "maya-version: fail - Scenario smoke needs 2025; host has 2016.5",
		},
		{
			name:        "no installed version",
			probeOutput: "",
			wantCode:    1,
			wantOutput:  "maya-version: fail - no Autodesk Maya installation found on host",
		},
		{
			name:         "config drift",
			probeOutput:  "maya-version:2025",
			mayaVersions: `        mayaVersions: ["2024"]` + "\n",
			wantCode:     0,
			wantOutput:   "maya-version: ok - 2025 satisfies Scenario smoke; config declares 2024 (drift)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			useFakeFFmpegLookPath(t, dir)
			sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
				`maya-stall-ssh-ok`,
				`writable`,
				`{"ok":true,"checks":[]}`,
				`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
				`{"ProcessId":123,"SessionId":1,"Name":"maya.exe"}`,
				`{"ok":true,"tool":"script.execute"}`,
				`removed`,
				tt.probeOutput,
				`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
				``,
				``,
				writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
				`removed`,
			})
			sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
			hostConfigPath := writeRealSSHDoctorHostConfig(t, dir, sshPath, sftpPath, tt.mayaVersions)
			var stdout, stderr bytes.Buffer

			code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version")
			if code != tt.wantCode {
				t.Fatalf("doctor exit code = %d, want %d; stdout: %s stderr: %s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Fatalf("doctor output missing %q:\n%s", tt.wantOutput, stdout.String())
			}
		})
	}
}

func TestInstalledMayaVersionsProbeRequiresMayaExecutable(t *testing.T) {
	script := installedMayaVersionsProbeScript()
	for _, want := range []string{
		`^Maya(\d{4}(?:\.\d+)?)$`,
		`^\d{4}(?:\.\d+)?$`,
		`Join-Path $_.FullName 'bin\maya.exe'`,
		`Join-Path ([string]$_.Value) 'bin\maya.exe'`,
		`Test-Path -LiteralPath $mayaExe -PathType Leaf`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("installed Maya probe script missing %q:\n%s", want, script)
		}
	}
}

func TestNormalizeMayaVersions(t *testing.T) {
	got := normalizeMayaVersions([]string{" 2025 ", "2016.5", "invalid", "2025", "2016.5"})
	want := []string{"2016.5", "2025"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized Maya versions = %#v, want %#v", got, want)
	}
}

func TestDoctorFakeHostUsesFakeInstalledMayaVersions(t *testing.T) {
	tests := []struct {
		name           string
		hostInventory  string
		wantCode       int
		wantOutput     string
		wantAbsent     string
		configVersions string
	}{
		{
			name:          "found matching install",
			hostInventory: `        fakeInstalledMayaVersions: ["2025"]` + "\n",
			wantCode:      0,
			wantOutput:    "maya-version: ok - 2025 satisfies Scenario smoke",
		},
		{
			name:          "missing install",
			hostInventory: `        fakeInstalledMayaVersions: []` + "\n",
			wantCode:      1,
			wantOutput:    "maya-version: fail - no Autodesk Maya installation found on host",
		},
		{
			name:          "mismatched install",
			hostInventory: `        fakeInstalledMayaVersions: ["2024"]` + "\n",
			wantCode:      1,
			wantOutput:    "maya-version: fail - Scenario smoke needs 2025; host has 2024",
		},
		{
			name:           "config drift",
			configVersions: `        mayaVersions: ["2025"]` + "\n",
			hostInventory:  `        fakeInstalledMayaVersions: ["2024"]` + "\n",
			wantCode:       1,
			wantOutput:     "maya-version: fail - Scenario smoke needs 2025; host has 2024; config declares 2025 (drift)",
		},
		{
			name:           "config versions are inventory fallback",
			configVersions: `        mayaVersions: ["2024"]` + "\n",
			wantCode:       1,
			wantOutput:     "maya-version: fail - Scenario smoke needs 2025; host has 2024",
		},
		{
			name:           "config fallback satisfies scenario",
			configVersions: `        mayaVersions: ["2025"]` + "\n",
			wantCode:       0,
			wantOutput:     "maya-version: ok - 2025 satisfies Scenario smoke",
		},
		{
			name:          "fake inventory is normalized",
			hostInventory: `        fakeInstalledMayaVersions: [" 2025 ", "invalid", "2025"]` + "\n",
			wantCode:      0,
			wantOutput:    "maya-version: ok - 2025 satisfies Scenario smoke",
		},
		{
			name:           "config fallback is normalized",
			configVersions: `        mayaVersions: [" 2025 ", "invalid", "2025"]` + "\n",
			wantCode:       0,
			wantOutput:     "maya-version: ok - 2025 satisfies Scenario smoke",
		},
		{
			name:           "normalized config avoids false drift",
			configVersions: `        mayaVersions: [" 2025 ", "invalid", "2025"]` + "\n",
			hostInventory:  `        fakeInstalledMayaVersions: ["2025"]` + "\n",
			wantCode:       0,
			wantOutput:     "maya-version: ok - 2025 satisfies Scenario smoke",
			wantAbsent:     "(drift)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
			mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        ssh: ok
        workRoot: writable
        broker: ok
`+tt.configVersions+tt.hostInventory)
			var stdout, stderr bytes.Buffer

			code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version")
			if code != tt.wantCode {
				t.Fatalf("doctor exit code = %d, want %d; stdout: %s stderr: %s", code, tt.wantCode, stdout.String(), stderr.String())
			}
			if !strings.Contains(stdout.String(), tt.wantOutput) {
				t.Fatalf("doctor output missing %q:\n%s", tt.wantOutput, stdout.String())
			}
			if tt.wantAbsent != "" && strings.Contains(stdout.String(), tt.wantAbsent) {
				t.Fatalf("doctor output unexpectedly contains %q:\n%s", tt.wantAbsent, stdout.String())
			}
		})
	}
}

func TestDoctorHostHealthTiesRealVisualEvidenceToSessionBroker(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		`maya-stall-ssh-ok`,
		`writable`,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		`removed`,
	})
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
          mcpSource: C:/maya-stall/tools/GG_MayaMCP
        visualEvidence: true
`)

	report := runDoctor(dir, doctorOptions{HostConfig: hostConfigPath, TargetProfile: "ci", HostPin: "alpha"})

	if !report.Healthy {
		t.Fatalf("Host Health healthy = false, checks: %+v", report.Layers)
	}
	broker := requireHostHealthLayer(t, report, "session-broker")
	if broker.Status != "ok" || broker.Source != "gg-mayasessiond" || !broker.InteractiveDesktop {
		t.Fatalf("session-broker Host Health = %+v, want interactive gg_mayasessiond", broker)
	}
	visual := requireHostHealthLayer(t, report, "visual-evidence")
	if visual.Status != "ok" || visual.Source != "session-broker" || visual.BlockedBy != "" {
		t.Fatalf("visual-evidence Host Health = %+v, want broker-backed ok", visual)
	}
	for _, want := range []string{"viewport.capture", "desktop recording"} {
		if !strings.Contains(visual.Detail, want) {
			t.Fatalf("visual-evidence detail = %q, want %s proof", visual.Detail, want)
		}
	}
	control := requireHostHealthLayer(t, report, "desktop-control")
	if control.Status != "ok" || control.Source != "session-broker" || control.BlockedBy != "" {
		t.Fatalf("desktop-control Host Health = %+v, want broker-backed ok", control)
	}
	if !strings.Contains(control.Detail, "desktop click") {
		t.Fatalf("desktop-control detail = %q, want desktop click support", control.Detail)
	}
}

func TestDoctorGGMayaSessiondRejectsMayaInWindowsServicesSession(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		``,
		``,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":0,"Name":"maya.exe"}`,
		`maya-version:2025`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
          mcpSource: C:/maya-stall/tools/GG_MayaMCP
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"session-broker: fail - maya.exe is running in Windows Services session 0",
		"hint: Restart gg_mayasessiond from the interactive Windows desktop.",
		"visual-evidence: fail - skipped because session-broker is not healthy",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
	logBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(logBytes), "CALL "); got != 6 {
		t.Fatalf("doctor should only probe Maya versions after session-broker failure, got %d SSH calls:\n%s", got, string(logBytes))
	}
}

func TestDoctorGGMayaSessiondVerifiesScriptExecuteProbe(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sshLog := filepath.Join(dir, "ssh.log")
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		``,
		``,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		``,
		`maya-version:2025`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		``,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
          mcpSource: C:/maya-stall/tools/GG_MayaMCP
        visualEvidence: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "session-broker: ok - gg_mayasessiond reachable; Maya UI is interactive") {
		t.Fatalf("doctor output missing broker ok:\n%s", stdout.String())
	}
	logBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if !strings.Contains(string(logBytes), "maya-win-01") {
		t.Fatalf("ssh log missing host target:\n%s", string(logBytes))
	}
}

func TestDoctorGGMayaSessiondRecoversCommandPortWithScheduledTask(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sshLog := filepath.Join(dir, "ssh.log")
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		``,
		``,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":false,"maya_alive":true,"mcp_alive":true}}`,
		`{"ok":true,"task":"MayaStallSessiondUI","reason":"command-port-not-ready"}`,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		`removed`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
          recoveryTask: MayaStallSessiondUI
        visualEvidence: true
`)

	report := runDoctor(dir, doctorOptions{HostConfig: hostConfigPath, TargetProfile: "ci", HostPin: "alpha"})

	if !report.Healthy {
		t.Fatalf("Host Health healthy = false, checks: %+v", report.Layers)
	}
	broker := requireHostHealthLayer(t, report, "session-broker")
	if broker.Status != "ok" || broker.State != "recovered" || !broker.InteractiveDesktop {
		t.Fatalf("session-broker Host Health = %+v, want recovered interactive broker", broker)
	}
	logBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if strings.Count(string(logBytes), "CALL ") < 10 {
		t.Fatalf("doctor did not run scheduled task recovery and re-probe:\n%s", string(logBytes))
	}
}

func TestDoctorGGMayaSessiondRecoversMalformedScriptExecuteResponse(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	sshLog := filepath.Join(dir, "ssh.log")
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		``,
		``,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":false,"tool":"script.execute","error":"Error calling tool 'script.execute': 'int' object has no attribute 'get'"}`,
		`removed`,
		`{"ok":true,"task":"MayaStallSessiondUI","reason":"script-execute-invalid-response"}`,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		`removed`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
          recoveryTask: MayaStallSessiondUI
        visualEvidence: true
`)

	report := runDoctor(dir, doctorOptions{HostConfig: hostConfigPath, TargetProfile: "ci", HostPin: "alpha"})

	if !report.Healthy {
		t.Fatalf("Host Health healthy = false, checks: %+v", report.Layers)
	}
	broker := requireHostHealthLayer(t, report, "session-broker")
	if broker.Status != "ok" || broker.State != "recovered" || !broker.InteractiveDesktop {
		t.Fatalf("session-broker Host Health = %+v, want recovered interactive broker", broker)
	}
}

func TestDoctorGGMayaSessiondReportsDaemonDoctorCheckIDs(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		``,
		``,
		`{"ok":false,"checks":[{"id":"state_dir","ok":false},{"id":"mcp_source","ok":true}]}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "session-broker: fail - gg_mayasessiond doctor failed: state_dir") {
		t.Fatalf("doctor did not include failing daemon check id:\n%s", stdout.String())
	}
}

func TestDoctorGGMayaSessiondAcceptsRecordingScenario(t *testing.T) {
	dir := writeRunConfigFixture(t)
	useFakeFFmpegLookPath(t, dir)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "screenshots:\n        enabled: false", "screenshots:\n        enabled: false\n      recording:\n        enabled: true", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		``,
		``,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}],"output":{"mime_type":"image/jpeg"}}`,
		``,
		``,
		writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
		`removed`,
	})
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "visual-evidence: ok") {
		t.Fatalf("doctor did not accept recording scenario:\n%s", stdout.String())
	}
}

func TestDoctorGGMayaSessiondRequiresDesktopRecordingReadiness(t *testing.T) {
	dir := writeRunConfigFixture(t)
	previousLookPath := lookPath
	lookPath = func(string) (string, error) {
		return "", fmt.Errorf("executable file not found in PATH")
	}
	defer func() {
		lookPath = previousLookPath
	}()
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		``,
		``,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
		`{"ok":true,"tool":"script.execute"}`,
		`removed`,
		`maya-version:2025`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}],"output":{"mime_type":"image/jpeg"}}`,
	})
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
        visualEvidence: true
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"visual-evidence: fail",
		"local ffmpeg is required for Windows desktop recording capture",
		"viewport.capture",
		"desktop recording",
		"hint: Install ffmpeg locally and repair Windows desktop recording prerequisites. See docs/setup/windows-maya-host.md#visual-evidence.",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorGGMayaSessiondReportsDesktopRecordingPrerequisiteFailures(t *testing.T) {
	tests := []struct {
		name       string
		failIndex  int
		failDetail string
	}{
		{
			name:       "remote capture root",
			failIndex:  10,
			failDetail: "remote capture root is not writable",
		},
		{
			name:       "interactive scheduled task",
			failIndex:  12,
			failDetail: "schtasks.exe is required for interactive desktop capture",
		},
		{
			name:       "desktop assemblies",
			failIndex:  12,
			failDetail: "Windows PowerShell desktop assemblies System.Windows.Forms and System.Drawing are required",
		},
		{
			name:       "compress archive",
			failIndex:  12,
			failDetail: "Compress-Archive is not recognized",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			useFakeFFmpegLookPath(t, dir)
			outputs := []string{
				``,
				``,
				`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
				`{"derived_status":"running","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
				`{"ProcessId":1234,"SessionId":1,"Name":"maya.exe"}`,
				`{"ok":true,"tool":"script.execute"}`,
				`removed`,
				`maya-version:2025`,
				`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}],"output":{"mime_type":"image/jpeg"}}`,
				``,
				``,
				writeFakeSSHBinaryOutput(t, dir, "recording-frames.zip", zipFrameArchive(t)),
				`removed`,
			}
			outputs[tt.failIndex-1] = "@fail:" + tt.failDetail
			sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), outputs)
			sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
			hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
			mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
        visualEvidence: true
`)
			var stdout, stderr bytes.Buffer

			code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
			if code != 1 {
				t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			for _, want := range []string{
				"visual-evidence: fail",
				"desktop recording unavailable",
				tt.failDetail,
				"hint: Install ffmpeg locally and repair Windows desktop recording prerequisites. See docs/setup/windows-maya-host.md#visual-evidence.",
			} {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestDoctorGGMayaSessiondRejectsStaleDerivedStatus(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		``,
		``,
		`{"ok":true,"checks":[{"id":"state_dir","ok":true}]}`,
		`{"derived_status":"stale","state":{"status":"running","call_server_ready":true,"maya_alive":true,"mcp_alive":true}}`,
		`[]`,
		`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"` + base64.StdEncoding.EncodeToString([]byte("jpeg proof")) + `","mimeType":"image/jpeg"}]}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "session-broker: fail - gg_mayasessiond is not running") {
		t.Fatalf("doctor did not reject stale derived status:\n%s", stdout.String())
	}
}

func TestDoctorRejectsUnknownStructuredBrokerType(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        broker:
          type: ok
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `session-broker: fail - unknown broker.type "ok"`) {
		t.Fatalf("doctor did not reject unknown broker type:\n%s", stdout.String())
	}
}

func TestDoctorRejectsStructuredBrokerWithoutType(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        broker:
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "session-broker: fail - broker.type is required for structured broker config") {
		t.Fatalf("doctor did not reject structured broker without type:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "visual-evidence: fail - unavailable: broker.type is required for structured broker config") {
		t.Fatalf("doctor reported Visual Evidence as available for invalid broker config:\n%s", stdout.String())
	}
}

func TestDoctorRejectsScalarBrokerStatusForVisualEvidence(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        broker: gg-mayasessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `session-broker: fail - broker status "gg-mayasessiond" is not usable for runs`) {
		t.Fatalf("doctor did not report scalar broker status failure:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `visual-evidence: fail - unavailable: broker status "gg-mayasessiond" is not usable for runs`) {
		t.Fatalf("doctor reported Visual Evidence as available for invalid scalar broker:\n%s", stdout.String())
	}
}

func TestDoctorGGMayaSessiondSkipsLiveProbesWhenHostLocked(t *testing.T) {
	dir := writeRunConfigFixture(t)
	release, locked, err := acquireHostLock(dir, "alpha")
	if err != nil {
		t.Fatalf("pre-lock host: %v", err)
	}
	if locked {
		t.Fatal("host was already locked")
	}
	defer release()
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{``, ``})
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"host-lock: fail - alpha locked",
		"session-broker: fail - skipped because Host Lock is not clear",
		"maya-version: fail - skipped because Host Lock is not clear",
		"visual-evidence: fail - skipped because Host Lock is not clear",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
	sshBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(sshBytes), "CALL "); got != 2 {
		t.Fatalf("doctor should only check ssh/work-root when host is locked, got %d SSH calls:\n%s", got, string(sshBytes))
	}
	if _, err := os.Stat(sftpLog); !os.IsNotExist(err) {
		t.Fatalf("locked doctor should not stage script.execute probe, stat err = %v", err)
	}
}

func TestDoctorHostHealthRepresentsHostLockState(t *testing.T) {
	t.Run("host-side lock is authoritative across checkouts", func(t *testing.T) {
		secondDir := writeRunConfigFixture(t)
		hostRoot := t.TempDir()
		hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, secondDir, hostRoot)
		host := mayaHostConfig{ID: "alpha", WorkRoot: hostRoot}
		lock, locked, err := acquireHostSideLock(host)
		if err != nil || locked {
			t.Fatalf("acquire host-side Host Lock: locked=%t err=%v", locked, err)
		}
		if err := lock.markActive(host.ID, "run-from-first-checkout"); err != nil {
			t.Fatalf("mark host-side Host Lock active: %v", err)
		}
		t.Cleanup(func() { _ = lock.release() })

		report := runDoctor(secondDir, doctorOptions{HostConfig: hostConfigPath, TargetProfile: "ci", HostPin: "alpha"})
		lockLayer := requireHostHealthLayer(t, report, "host-lock")
		if lockLayer.Status != "fail" || lockLayer.State != "active" || lockLayer.Source != "maya-host" || lockLayer.ActiveRun != "run-from-first-checkout" {
			t.Fatalf("host-side Host Lock layer = %+v", lockLayer)
		}
		if formatted := formatHostHealthReport(report); !strings.Contains(formatted, "activeRun:run-from-first-checkout") {
			t.Fatalf("formatted Host Health omitted active Run ID: %s", formatted)
		}
	})

	t.Run("kept run blocks with kept run id", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		hostConfigPath := writeSingleHealthyHostConfig(t, dir)
		lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
		mustWriteFile(t, lockPath, "host: alpha\nkeptRun: run-kept\n")

		report := runDoctor(dir, doctorOptions{HostConfig: hostConfigPath, TargetProfile: "ci", HostPin: "alpha"})

		lock := requireHostHealthLayer(t, report, "host-lock")
		if lock.Status != "fail" || lock.State != "kept" || lock.KeptRun != "run-kept" {
			t.Fatalf("kept Host Lock layer = %+v", lock)
		}
	})

	t.Run("stale lock is represented as stale but not blocking", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		hostConfigPath := writeSingleHealthyHostConfig(t, dir)
		lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
		mustWriteFile(t, lockPath, "host: alpha\npid: -1\n")

		report := runDoctor(dir, doctorOptions{HostConfig: hostConfigPath, TargetProfile: "ci", HostPin: "alpha"})

		lock := requireHostHealthLayer(t, report, "host-lock")
		if lock.Status != "ok" || lock.State != "stale" {
			t.Fatalf("stale Host Lock layer = %+v", lock)
		}
	})
}

func TestDoctorGGMayaSessiondValidatesConfigWhenHostLocked(t *testing.T) {
	dir := writeRunConfigFixture(t)
	release, locked, err := acquireHostLock(dir, "alpha")
	if err != nil {
		t.Fatalf("pre-lock host: %v", err)
	}
	if locked {
		t.Fatal("host was already locked")
	}
	defer release()
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{``, ``})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "session-broker: fail - gg_mayasessiond broker requires broker.python") {
		t.Fatalf("doctor hid static broker config error behind Host Lock:\n%s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "visual-evidence: fail - unavailable: gg_mayasessiond broker requires broker.python") {
		t.Fatalf("doctor Visual Evidence did not report static broker config error:\n%s", stdout.String())
	}
}

func TestDoctorScenarioValidatesInputsAndMayaVersion(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeLayeredHostConfig(t, dir)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{
		"scenario-inputs: ok - smoke",
		"host: ok - alpha",
		"maya-version: ok - 2025 satisfies Scenario smoke",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDoctorAndRunReportSameScenarioInputFailures(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(string) string
		detail string
	}{
		{
			name: "missing Scenario Result",
			edit: func(content string) string {
				return strings.Replace(content, `scenarioResult: "outputs/smoke-result.json"`, `scenarioResult: ""`, 1)
			},
			detail: `Scenario "smoke" missing expectedOutputs.scenarioResult`,
		},
		{
			name: "bad payload path",
			edit: func(content string) string {
				return strings.Replace(content, `maya/smoke.py`, `maya/missing.py`, 1)
			},
			detail: `stage mayaScripts payload maya/missing.py`,
		},
		{
			name: "bad Validator path",
			edit: func(content string) string {
				return content + `    validators:
      - type: outputExists
        path: "../outside.json"
`
			},
			detail: `repo path "../outside.json" must be repo-relative`,
		},
		{
			name: "bad optional Validator path",
			edit: func(content string) string {
				return content + `    validators:
      - type: visualEvidence
        path: "../outside.json"
`
			},
			detail: `repo path "../outside.json" must be repo-relative`,
		},
		{
			name: "unknown Validator",
			edit: func(content string) string {
				return content + `    validators:
      - type: pluginDomainAssertion
`
			},
			detail: `unknown Validator type "pluginDomainAssertion"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			configPath := filepath.Join(dir, ".maya-stall.yaml")
			contentBytes, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("read config fixture: %v", err)
			}
			mustWriteFile(t, configPath, tt.edit(string(contentBytes)))
			hostConfigPath := writeSingleHealthyHostConfig(t, dir)
			var doctorOut, doctorErr, runOut, runErr bytes.Buffer

			doctorCode := Run([]string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--scenario", "smoke"}, &doctorOut, &doctorErr, dir, "test-version")
			if doctorCode != 1 {
				t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", doctorCode, doctorOut.String(), doctorErr.String())
			}
			runCode := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "smoke"}, &runOut, &runErr, dir, "test-version")
			if runCode != 1 {
				t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", runCode, runOut.String(), runErr.String())
			}
			if !strings.Contains(doctorOut.String(), tt.detail) {
				t.Fatalf("doctor output missing %q:\n%s", tt.detail, doctorOut.String())
			}
			if !strings.Contains(runErr.String(), tt.detail) {
				t.Fatalf("run error missing %q:\n%s", tt.detail, runErr.String())
			}
		})
	}
}

func TestScenarioContractNormalizesSSHOutputPlan(t *testing.T) {
	contract, err := resolveScenarioContract(repoRunConfig{
		Version: 1,
		Scenarios: map[string]scenarioConfig{
			"smoke": {
				Payload: runPayload{
					Scripts: []string{"maya/../maya/smoke.py"},
				},
				ExpectedOutputs: expectedOutputs{
					ScenarioResult: "outputs/../results/smoke.json",
					Files: []string{
						"outputs/../reports/report.json",
						"reports/report.json",
					},
				},
				Validators: []validatorConfig{
					{Type: "outputExists", Path: "outputs/../reports/report.json"},
					{Type: "jsonEquals", Path: "outputs/../reports/summary.json", JSONPath: "$.status", Equals: "passed"},
				},
			},
		},
	}, "smoke")
	if err != nil {
		t.Fatalf("resolve Scenario contract: %v", err)
	}

	var got []scenarioOutputPath
	got = append(got, contract.Outputs...)
	want := []scenarioOutputPath{
		{Path: "results/smoke.json", Optional: false},
		{Path: "reports/report.json", Optional: true},
		{Path: "reports/summary.json", Optional: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output plan = %#v, want %#v", got, want)
	}
	if len(contract.Payload) != 1 || contract.Payload[0].Source != "maya/smoke.py" {
		t.Fatalf("normalized payload = %#v", contract.Payload)
	}
}

func TestDoctorFailureLayersIncludeRepairHints(t *testing.T) {
	tests := []struct {
		name       string
		config     string
		args       []string
		lockHost   string
		wantLayer  string
		wantDetail string
		wantHint   string
	}{
		{
			name: "target profile",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
`,
			args:       []string{"--target-profile", "missing"},
			wantLayer:  "target-profile: fail",
			wantDetail: `unknown Target Profile "missing"`,
			wantHint:   "Choose a configured Target Profile or add it to the host config. See docs/setup/windows-maya-host.md#target-profile-and-host-pool.",
		},
		{
			name: "host pool",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts: []
`,
			wantLayer:  "host-pool: fail",
			wantDetail: "has no Maya Hosts",
			wantHint:   "Add at least one Maya Host to the Host Pool. See docs/setup/windows-maya-host.md#target-profile-and-host-pool.",
		},
		{
			name: "fake ssh",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        ssh: down
`,
			wantLayer:  "fake-ssh: fail",
			wantDetail: "down",
			wantHint:   "Fix SSH reachability for this Maya Host. See docs/setup/windows-maya-host.md#openssh-reachability.",
		},
		{
			name: "work root",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        workRoot: unwritable
`,
			wantLayer:  "work-root: fail",
			wantDetail: "unwritable",
			wantHint:   "Fix the host work root path or permissions. See docs/setup/windows-maya-host.md#work-root.",
		},
		{
			name: "session broker",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        broker: down
`,
			wantLayer:  "session-broker: fail",
			wantDetail: "down",
			wantHint:   "Use broker.type: gg-mayasessiond or a legacy fake broker status. See docs/setup/windows-maya-host.md#session-broker.",
		},
		{
			name: "maya version",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        mayaVersions: ["2024"]
`,
			args:       []string{"--scenario", "smoke"},
			wantLayer:  "maya-version: fail",
			wantDetail: "needs 2025",
			wantHint:   "Install Autodesk Maya 2025 or choose a Maya Host with 2025 installed; detected 2024. See docs/setup/windows-maya-host.md#autodesk-maya.",
		},
		{
			name: "visual evidence",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        visualEvidence: false
`,
			wantLayer:  "visual-evidence: fail",
			wantDetail: "unavailable",
			wantHint:   "Enable screenshot capture through the Session Broker. See docs/setup/windows-maya-host.md#visual-evidence.",
		},
		{
			name: "host lock",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
`,
			lockHost:   "alpha",
			wantLayer:  "host-lock: fail",
			wantDetail: "locked",
			wantHint:   "Wait for the active Fresh Run or clear the stale Host Lock after verifying no run is active. See docs/setup/windows-maya-host.md#host-lock-and-retention.",
		},
		{
			name: "scenario inputs",
			config: `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
`,
			args:       []string{"--scenario", "smoke"},
			wantLayer:  "scenario-inputs: fail",
			wantDetail: "missing.py",
			wantHint:   "Fix the Scenario payload paths, expectedOutputs, and Validators in repo config. See docs/setup/windows-maya-host.md#scenario-inputs.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			if tt.name == "scenario inputs" {
				configPath := filepath.Join(dir, ".maya-stall.yaml")
				contentBytes, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatalf("read config fixture: %v", err)
				}
				content := strings.Replace(string(contentBytes), "maya/smoke.py", "maya/missing.py", 1)
				if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
					t.Fatalf("write missing payload config fixture: %v", err)
				}
			}
			hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
			mustWriteFile(t, hostConfigPath, tt.config)
			if tt.lockHost != "" {
				release, locked, err := acquireHostLock(dir, tt.lockHost)
				if err != nil {
					t.Fatalf("pre-lock host: %v", err)
				}
				if locked {
					t.Fatal("host was already locked")
				}
				defer release()
			}
			args := []string{"doctor", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}
			args = append(args, tt.args...)
			var stdout, stderr bytes.Buffer

			code := Run(args, &stdout, &stderr, dir, "test-version")
			if code != 1 {
				t.Fatalf("doctor exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			for _, want := range []string{tt.wantLayer, tt.wantDetail, "hint: " + tt.wantHint} {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("doctor output missing %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}

func TestRunScenarioSelectsFirstHealthyUnlockedHostFromExternalConfig(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        health: unhealthy
      - id: beta
        health: healthy
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: beta") {
		t.Fatalf("run output missing selected host:\n%s", stdout.String())
	}

	runState := onlyRunDir(t, filepath.Join(dir, ".maya-stall", "state", "runs"))
	manifestBytes, err := os.ReadFile(filepath.Join(runState, "manifest.json"))
	if err != nil {
		t.Fatalf("read run manifest: %v", err)
	}
	var manifest struct {
		TargetProfile string `json:"targetProfile"`
		Host          string `json:"host"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("parse run manifest: %v", err)
	}
	if manifest.TargetProfile != "ci" || manifest.Host != "beta" {
		t.Fatalf("manifest selected target/host = %q/%q, want ci/beta", manifest.TargetProfile, manifest.Host)
	}
}

func TestRunScenarioRealSSHUploadsPayloadAndDownloadsDeclaredOutputs(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "files: []", "files:\n        - \"outputs/report.json\"", 1)
	content = strings.Replace(content, "evidence:\n      screenshots:\n        enabled: false", "evidence:\n      screenshots:\n        enabled: false\n    validators:\n      - type: outputExists\n        path: \"outputs/report.json\"", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: alpha") || !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("run output missing selected real host/pass status:\n%s", stdout.String())
	}
	logBytes, err := os.ReadFile(sftpLog)
	if err != nil {
		t.Fatalf("read sftp log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{
		`-mkdir "/C:/maya-stall"`,
		`-mkdir "/C:/maya-stall/runs"`,
		`put -r `,
		`maya/smoke.py`,
		`/C:/maya-stall/runs/`,
		`payload/mayaScripts/maya/smoke.py`,
		`-get -r `,
		`workspace/outputs/report.json`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("sftp batch missing %q:\n%s", want, log)
		}
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	if _, err := os.Stat(filepath.Join(evidence, "outputs", "report.json")); err != nil {
		t.Fatalf("expected downloaded output in Evidence Bundle: %v", err)
	}
}

func TestSFTPBatchMkdirAllPreservesUNCVolume(t *testing.T) {
	batch := newSFTPBatch()
	batch.mkdirAll("//server/share/trusted/plugins")
	got := batch.String()
	for _, want := range []string{
		`-mkdir "//server"`,
		`-mkdir "//server/share"`,
		`-mkdir "//server/share/trusted"`,
		`-mkdir "//server/share/trusted/plugins"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("UNC mkdir batch missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `-mkdir "/server`) {
		t.Fatalf("UNC mkdir batch collapsed the UNC prefix:\n%s", got)
	}
}

func TestRunScenarioRealSSHUploadsPluginArtifactsToTrustedRoot(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts/build";
`),
		`{"ok":true,"tool":"trusted-plugin-cleanup"}`,
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        trustedPluginArtifactsRoot: C:/maya-stall/trusted-plugin-artifacts
        mayaVersions: ["2025"]
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	logBytes, err := os.ReadFile(sftpLog)
	if err != nil {
		t.Fatalf("read sftp log: %v", err)
	}
	log := string(logBytes)
	for _, want := range []string{
		`put -r `,
		`build/demo.mll`,
		`"/C:/maya-stall/trusted-plugin-artifacts/build/demo.mll"`,
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("trusted Plugin Artifact upload missing %q:\n%s", want, log)
		}
	}
	for _, forbidden := range []string{
		`"/C:/maya-stall/trusted-plugin-artifacts/maya/smoke.py"`,
		`"/C:/maya-stall/trusted-plugin-artifacts/scenes/start.ma"`,
	} {
		if strings.Contains(log, forbidden) {
			t.Fatalf("trusted root upload included non-plugin payload %q:\n%s", forbidden, log)
		}
	}
}

func TestRunScenarioRealSSHToleratesMissingTrustedPluginDestination(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeMissingTrustedPluginDestinationFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), "C:/maya-stall/trusted-plugin-artifacts/build/demo.mll")
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	logBytes, err := os.ReadFile(sftpLog)
	if err != nil {
		t.Fatalf("read sftp log: %v", err)
	}
	log := string(logBytes)
	if !strings.Contains(log, `"/C:/maya-stall/trusted-plugin-artifacts/build/demo.mll"`) {
		t.Fatalf("missing-destination prepare did not continue to trusted Plugin Artifact upload:\n%s", log)
	}
}

func TestRunScenarioRealSSHStopsWhenTrustedPluginDestinationRemovalFails(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeFailingTrustedPluginDestinationRemovalFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), "C:/maya-stall/trusted-plugin-artifacts/build/demo.mll")
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"prepare trusted Plugin Artifact destination",
		"access denied removing trusted Plugin Artifact",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("run stderr missing %q:\n%s", want, stderr.String())
		}
	}
	if _, err := os.Stat(sftpLog); !os.IsNotExist(err) {
		t.Fatalf("trusted Plugin Artifact upload continued after removal failure; sftp log stat = %v", err)
	}
}

func TestRunScenarioRejectsTrustedPluginRootUnderRunWorkspaces(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
        workRoot: C:/maya-stall
        trustedPluginArtifactsRoot: C:/maya-stall/runs/trusted-plugin-artifacts
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "trustedPluginArtifactsRoot must be outside workRoot/runs") {
		t.Fatalf("run stderr missing trusted root rejection:\n%s", stderr.String())
	}
}

func TestTrustedPluginArtifactsRootRejectsFilesystemRoots(t *testing.T) {
	for _, root := range []string{
		".", "/", "C:/", `C:\`,
		`//server/share`, `\\server\share`, `//server/share/.`, `//server/share/plugins/..`,
	} {
		t.Run(root, func(t *testing.T) {
			host := mayaHostConfig{
				WorkRoot:                   "C:/maya-stall",
				TrustedPluginArtifactsRoot: root,
			}

			err := validateTrustedPluginArtifactsRoot(host)
			if err == nil || !strings.Contains(err.Error(), "must resolve to a non-root directory") {
				t.Fatalf("validate trusted Plugin Artifact root %q error = %v", root, err)
			}
		})
	}
}

func TestTrustedPluginArtifactsRootRejectsWindowsDeviceNamespaces(t *testing.T) {
	for _, root := range []string{
		`\\?\UNC\server\share\plugins`, `\\.\UNC\server\share\plugins`,
		`\\?\C:\plugins`, `\\.\C:\plugins`,
		`\\?\Volume{01234567-89ab-cdef-0123-456789abcdef}\plugins`,
		`\\?\GLOBALROOT\Device\HarddiskVolume1\plugins`,
	} {
		t.Run(root, func(t *testing.T) {
			host := mayaHostConfig{
				WorkRoot:                   "C:/maya-stall",
				TrustedPluginArtifactsRoot: root,
			}

			err := validateTrustedPluginArtifactsRoot(host)
			if err == nil || !strings.Contains(err.Error(), "must not use a Windows device namespace") {
				t.Fatalf("validate trusted Plugin Artifact root %q error = %v", root, err)
			}
		})
	}
}

func TestTrustedPluginArtifactsRootRejectsRelativeWindowsPaths(t *testing.T) {
	for _, root := range []string{
		"plugins", "maya-stall/runs", `C:plugins`, `C:maya-stall\runs`,
	} {
		t.Run(root, func(t *testing.T) {
			host := mayaHostConfig{
				WorkRoot:                   "C:/Users/maya/maya-stall",
				TrustedPluginArtifactsRoot: root,
			}

			err := validateTrustedPluginArtifactsRoot(host)
			if err == nil || !strings.Contains(err.Error(), "must be an absolute Windows path") {
				t.Fatalf("validate trusted Plugin Artifact root %q error = %v", root, err)
			}
		})
	}
}

func TestTrustedPluginArtifactsRootRejectsTraversalAboveWindowsVolume(t *testing.T) {
	for _, root := range []string{
		"C:/plugins/../../..",
		"C:/../../maya-stall/runs/trusted",
		"//server/share/../../plugins",
		"//server/share/plugins/../../../other",
	} {
		t.Run(root, func(t *testing.T) {
			host := mayaHostConfig{
				WorkRoot:                   "C:/maya-stall",
				TrustedPluginArtifactsRoot: root,
			}

			err := validateTrustedPluginArtifactsRoot(host)
			if err == nil || !strings.Contains(err.Error(), "must not traverse above its Windows volume root") {
				t.Fatalf("validate trusted Plugin Artifact root %q error = %v", root, err)
			}
		})
	}
}

func TestTrustedPluginArtifactsRootRejectsWin32TrimmedComponents(t *testing.T) {
	for _, test := range []struct {
		name     string
		workRoot string
		root     string
	}{
		{name: "drive trailing period", workRoot: "C:/maya-stall", root: "C:/maya-stall/runs./trusted"},
		{name: "drive trailing space", workRoot: "C:/maya-stall", root: "C:/maya-stall/runs /trusted"},
		{name: "drive terminal space", workRoot: "C:/maya-stall", root: "C:/trusted/plugins "},
		{name: "UNC trailing period", workRoot: "//server/share/maya-stall", root: "//server/share/maya-stall/runs./trusted"},
		{name: "UNC trailing space", workRoot: "//server/share/maya-stall", root: "//server/share/maya-stall/runs /trusted"},
		{name: "UNC terminal space", workRoot: "//server/share/maya-stall", root: "//server/share/trusted/plugins "},
	} {
		t.Run(test.name, func(t *testing.T) {
			host := mayaHostConfig{
				WorkRoot:                   test.workRoot,
				TrustedPluginArtifactsRoot: test.root,
			}

			err := validateTrustedPluginArtifactsRoot(host)
			if err == nil || !strings.Contains(err.Error(), "must not contain Windows path components ending in a space or period") {
				t.Fatalf("validate trusted Plugin Artifact root %q error = %v", test.root, err)
			}
		})
	}
}

func TestTrustedPluginArtifactsRootRejectsWin32TrimmedWorkRoot(t *testing.T) {
	for _, workRoot := range []string{
		"C:/maya-stall./work",
		"C:/maya-stall ",
		"//server/share/maya-stall /work",
		"//server/share/maya-stall ",
	} {
		t.Run(workRoot, func(t *testing.T) {
			host := mayaHostConfig{
				WorkRoot:                   workRoot,
				TrustedPluginArtifactsRoot: "C:/trusted/plugins",
			}

			err := validateTrustedPluginArtifactsRoot(host)
			if err == nil || !strings.Contains(err.Error(), "workRoot must not contain Windows path components ending in a space or period") {
				t.Fatalf("validate work root %q error = %v", workRoot, err)
			}
		})
	}
}

func TestTrustedPluginArtifactsRootRejectsInvalidWin32Components(t *testing.T) {
	for _, root := range []string{
		"C:/trusted/CON",
		"C:/trusted/bad?name",
		"C:/trusted/COM¹",
		"//server/share/trusted/aux.txt",
		"//server/share/trusted/bad:name",
		"//server/share/trusted/lpt².log",
	} {
		t.Run(root, func(t *testing.T) {
			host := mayaHostConfig{
				WorkRoot:                   "C:/maya-stall",
				TrustedPluginArtifactsRoot: root,
			}

			err := validateTrustedPluginArtifactsRoot(host)
			if err == nil || !strings.Contains(err.Error(), "invalid Windows path component") {
				t.Fatalf("validate trusted Plugin Artifact root %q error = %v", root, err)
			}
		})
	}
}

func TestRunScenarioRejectsTrustedPluginRootThatNormalizesUnderRunWorkspaces(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
        workRoot: C:/maya-stall
        trustedPluginArtifactsRoot: C:/maya-stall/runs2/../runs/trusted-plugin-artifacts
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "trustedPluginArtifactsRoot must be outside workRoot/runs") {
		t.Fatalf("run stderr missing trusted root rejection:\n%s", stderr.String())
	}
}

func TestRunScenarioRejectsTrustedPluginRootThatContainsRunWorkspaces(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
        workRoot: C:/maya-stall/work
        trustedPluginArtifactsRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "trustedPluginArtifactsRoot must not contain workRoot/runs") {
		t.Fatalf("run stderr missing broad trusted root rejection:\n%s", stderr.String())
	}
}

func TestRunScenarioDoesNotCleanTrustedPluginRootWhenPayloadIsInvalid(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), `      scenes:
        - "scenes/start.ma"`, `      scenes:
        - "scenes/missing.ma"`, 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		`{"ok":true,"tool":"trusted-plugin-cleanup"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
        workRoot: C:/maya-stall
        trustedPluginArtifactsRoot: C:/maya-stall/trusted-plugin-artifacts
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "scenes/missing.ma") {
		t.Fatalf("run stderr missing invalid payload path:\n%s", stderr.String())
	}
	if _, err := os.Stat(sshLog); !os.IsNotExist(err) {
		t.Fatalf("trusted Plugin Artifact cleanup ran before payload validation; ssh log stat = %v", err)
	}
}

func TestSFTPBatchUsesWindowsAbsoluteDrivePaths(t *testing.T) {
	batch := newSFTPBatch()

	batch.mkdirAll("C:/maya-stall/runs/run-1/workspace")
	batch.put("/tmp/probe.py", "C:/maya-stall/runs/run-1/workspace/probe.py")
	batch.get("C:/maya-stall/runs/run-1/workspace/outputs/result.json", "/tmp/result.json", true)

	got := batch.String()
	for _, want := range []string{
		`-mkdir "/C:/maya-stall"`,
		`-mkdir "/C:/maya-stall/runs/run-1/workspace"`,
		`put -r "/tmp/probe.py" "/C:/maya-stall/runs/run-1/workspace/probe.py"`,
		`-get -r "/C:/maya-stall/runs/run-1/workspace/outputs/result.json" "/tmp/result.json"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("SFTP batch missing %q:\n%s", want, got)
		}
	}
}

func TestSFTPBatchTimeoutConfig(t *testing.T) {
	timeout, err := sftpBatchTimeout(mayaHostConfig{})
	if err != nil {
		t.Fatalf("default sftp timeout: %v", err)
	}
	if timeout != defaultSFTPBatchTimeout {
		t.Fatalf("default sftp timeout = %s, want %s", timeout, defaultSFTPBatchTimeout)
	}

	timeout, err = sftpBatchTimeout(mayaHostConfig{SSH: sshConfig{SFTPTimeout: "0"}})
	if err != nil {
		t.Fatalf("disabled sftp timeout: %v", err)
	}
	if timeout != 0 {
		t.Fatalf("disabled sftp timeout = %s, want 0", timeout)
	}

	if _, err := sftpBatchTimeout(mayaHostConfig{SSH: sshConfig{SFTPTimeout: "-1s"}}); err == nil {
		t.Fatal("negative sftp timeout succeeded")
	}
}

func TestRunScenarioGGMayaSessiondBrokerExecutesRemoteScenarioAndCapturesScreenshot(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "screenshots:\n        enabled: false", "screenshots:\n        enabled: true", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshLog := filepath.Join(dir, "ssh.log")
	jpegPath := filepath.Join(dir, "desktop-screenshot.jpg")
	if err := os.WriteFile(jpegPath, validJPEGBytes(t), 0o644); err != nil {
		t.Fatalf("write desktop screenshot fixture: %v", err)
	}
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		``,
		``,
		"@file:" + jpegPath,
		``,
		sessiondStatusFixture("session-fresh"),
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
          mcpSource: C:/maya-stall/tools/GG_MayaMCP
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: alpha") || !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("run output missing selected real broker host/pass status:\n%s", stdout.String())
	}
	sshBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	sshLogContent := string(sshBytes)
	for _, want := range []string{"powershell", "maya-win-01"} {
		if !strings.Contains(sshLogContent, want) {
			t.Fatalf("ssh log missing %q:\n%s", want, sshLogContent)
		}
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	screenshotPath := filepath.Join(evidence, "screenshots", "smoke.png")
	screenshotBytes, err := os.ReadFile(screenshotPath)
	if err != nil {
		t.Fatalf("read sessiond screenshot: %v", err)
	}
	if !looksLikeImageBytes("image/png", screenshotBytes) {
		t.Fatalf("sessiond screenshot bytes do not contain the transcoded PNG: %v", screenshotBytes)
	}
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.VisualEvidence) != 1 || bundle.VisualEvidence[0].MediaType != "image/png" {
		t.Fatalf("sessiond screenshot metadata = %+v, want image/png", bundle.VisualEvidence)
	}
	sftpBytes, err := os.ReadFile(sftpLog)
	if err != nil {
		t.Fatalf("read sftp log: %v", err)
	}
	if !strings.Contains(string(sftpBytes), ".maya-stall-scenario.py") {
		t.Fatalf("sftp log missing staged scenario wrapper:\n%s", string(sftpBytes))
	}
}

func TestRunScenarioGGMayaSessiondDownloadedFailedResultFailsRun(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeScenarioResultSFTPCommand(t, dir, sftpLog, resultStatusFailed, "remote script failed")
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-alpha"),
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundleBytes, err := os.ReadFile(filepath.Join(evidence, "evidence.json"))
	if err != nil {
		t.Fatalf("read evidence bundle: %v; stdout: %s stderr: %s", err, stdout.String(), stderr.String())
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		t.Fatalf("parse evidence bundle: %v", err)
	}
	if bundle.Status != resultStatusFailed {
		t.Fatalf("Evidence Bundle status = %q, want failed; stdout: %s stderr: %s", bundle.Status, stdout.String(), stderr.String())
	}
}

func TestRunRetentionCommandsUseSessiondBrokerForStatusAttachAndStop(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshLog := filepath.Join(dir, "ssh.log")
	sshPath := writeSequencedFakeSSHCommand(t, dir, sshLog, []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		sessiondStatusFixture("session-fresh"),
		sessiondStatusFixture("session-fresh"),
		`{"events":[{"kind":"session","message":"retained run log"}]}`,
		sessiondStatusFixture("session-fresh"),
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("status exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"state: kept", "remoteState: running", "brokerSession: session-fresh", "remoteWorkspace: C:/maya-stall/runs/" + runID + "/workspace"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("attach exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"broker:", "adapter: gg-mayasessiond", "brokerReport:", "retained run log"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("attach output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("Host Lock after stop = %v, want missing", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !os.IsNotExist(err) {
		t.Fatalf("run state after stop = %v, want missing", err)
	}
	sshBytes, err := os.ReadFile(sshLog)
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(sshBytes), "CALL "); got < 8 {
		t.Fatalf("retention commands made %d SSH calls, want at least 8:\n%s", got, string(sshBytes))
	}
}

func TestRunRetentionFailureKeepsHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		`{"ok":false,"error":"status unavailable"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "status unavailable") {
		t.Fatalf("run error missing retain status failure: %s", stderr.String())
	}
	lockBytes, err := os.ReadFile(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock"))
	if err != nil {
		t.Fatalf("Host Lock missing after retention failure: %v", err)
	}
	runID := filepath.Base(onlyRunDir(t, filepath.Join(dir, ".maya-stall", "state", "runs")))
	if !strings.Contains(string(lockBytes), "keptRun: "+runID) {
		t.Fatalf("Host Lock after retention failure was not kept:\n%s", string(lockBytes))
	}
}

func TestRunRetentionStopWithMissingRecordedSessionIDCleansWithoutStoppingCurrentSession(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		`{"ok":false,"error":"retain status unavailable"}`,
		sessiondStatusFixture("session-beta"),
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := filepath.Base(onlyRunDir(t, filepath.Join(dir, ".maya-stall", "state", "runs")))

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); !os.IsNotExist(err) {
		t.Fatalf("Host Lock after missing-session-id cleanup = %v, want missing", err)
	}
	sshBytes, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(sshBytes), "CALL "); got != 6 {
		t.Fatalf("missing-session-id stop made %d SSH calls, want Fresh Run lifecycle plus status and cleanup but no retained broker stop:\n%s", got, string(sshBytes))
	}
}

func TestRunRetentionStatusReportsStaleWhenSessiondSessionChanged(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		sessiondStatusFixture("session-beta"),
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("status exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"state: stale", "brokerSession: session-beta", "orphaned"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stale status output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunRetentionStopFailureKeepsRunStateAndHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		sessiondStatusFixture("session-fresh"),
		`{"ok":false,"error":"cannot stop retained session"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("stop exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "cannot stop retained session") {
		t.Fatalf("stop error missing broker failure: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("run state missing after failed stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); err != nil {
		t.Fatalf("Host Lock missing after failed stop: %v", err)
	}
}

func TestRunRetentionStopStatusFailureKeepsRunStateAndHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		`{"ok":false,"error":"status unavailable"}`,
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("stop exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "status unavailable") {
		t.Fatalf("stop error missing status failure: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("run state missing after status failed stop: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); err != nil {
		t.Fatalf("Host Lock missing after status failed stop: %v", err)
	}
	sshBytes, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(sshBytes), "CALL "); got != 4 {
		t.Fatalf("status-failed stop made %d SSH calls, want no stop call after failed status:\n%s", got, string(sshBytes))
	}
}

func TestRunRetentionStopRequiresConfirmedBrokerStop(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		sessiondStatusFixture("session-fresh"),
		`{"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("stop exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "gg_mayasessiond stop failed") {
		t.Fatalf("stop error missing unconfirmed stop failure: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); err != nil {
		t.Fatalf("Host Lock missing after unconfirmed stop: %v", err)
	}
}

func TestRunRetentionStopRejectsTamperedRemoteCleanupPath(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")
	recordBytes, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(recordBytes, &record); err != nil {
		t.Fatalf("parse run record: %v", err)
	}
	record["remoteRunRoot"] = "C:/maya-stall/not-this-run"
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("write tampered run record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("stop exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "does not match expected") {
		t.Fatalf("stop error missing cleanup path validation: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); err != nil {
		t.Fatalf("Host Lock missing after rejected cleanup path: %v", err)
	}
	sshBytes, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(sshBytes), "CALL "); got != 3 {
		t.Fatalf("tampered cleanup made %d SSH calls, want no stop/cleanup after run retention:\n%s", got, string(sshBytes))
	}
}

func TestRunRetentionStopRefusesStaleSessiondSession(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		sessiondStatusFixture("session-beta"),
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); !os.IsNotExist(err) {
		t.Fatalf("Host Lock after stale stop cleanup = %v, want missing", err)
	}
	sshBytes, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(sshBytes), "CALL "); got != 5 {
		t.Fatalf("stale stop made %d SSH calls, want status plus cleanup but no stop call:\n%s", got, string(sshBytes))
	}
}

func TestRunRetentionStopTreatsMissingCurrentSessionIDAsStale(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		`{"has_state":true,"derived_status":"running","state":{"status":"running","maya_alive":true,"mcp_alive":true,"call_server_ready":true}}`,
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	sshBytes, err := os.ReadFile(filepath.Join(dir, "ssh.log"))
	if err != nil {
		t.Fatalf("read ssh log: %v", err)
	}
	if got := strings.Count(string(sshBytes), "CALL "); got != 5 {
		t.Fatalf("missing-current-session stop made %d SSH calls, want status plus cleanup but no broker stop:\n%s", got, string(sshBytes))
	}
}

func TestRunRetentionLegacySessiondRecordCanReleaseLocalLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	runID := "20260704T112600.000000000Z"
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	mustWriteFile(t, filepath.Join(stateDir, "manifest.json"), `{
  "runId": "`+runID+`",
  "scenario": "smoke",
  "targetProfile": "ci",
  "host": "alpha",
  "runtime": {
    "profile": "ssh-sessiond",
    "hostAdapter": "ssh",
    "brokerAdapter": "gg-mayasessiond",
    "brokerConfigSource": "legacy",
    "liveProofEligible": true
  }
}
`)
	mustWriteFile(t, filepath.Join(stateDir, "events.jsonl"), "{}\n")
	mustWriteFile(t, filepath.Join(stateDir, "logs", "session.log"), "legacy\n")
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	mustWriteFile(t, lockPath, "host: alpha\nkeptRun: "+runID+"\n")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("legacy status exit code = %d, want evidence-missing failure before bundle exists; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("legacy stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("legacy Host Lock after stop = %v, want missing", err)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("legacy run state after stop = %v, want missing", err)
	}
}

func TestRunScenarioRejectsUnknownStructuredBrokerType(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        broker:
          type: gg_mayasessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `unknown broker.type "gg_mayasessiond"`) {
		t.Fatalf("run error did not reject unknown broker type: %s", stderr.String())
	}
}

func TestRunScenarioRejectsStructuredBrokerWithoutType(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "broker.type is required for structured broker config") {
		t.Fatalf("run error did not reject structured broker without type: %s", stderr.String())
	}
	if _, err := os.Stat(sftpLog); !os.IsNotExist(err) {
		t.Fatalf("invalid broker config should fail before SFTP staging, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); !os.IsNotExist(err) {
		t.Fatalf("host lock was not released after broker fail-fast, stat err = %v", err)
	}
}

func TestRunScenarioRejectsIncompleteSessiondBrokerBeforeRemoteStaging(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "gg_mayasessiond broker requires broker.python") {
		t.Fatalf("run error did not reject incomplete sessiond broker config: %s", stderr.String())
	}
	if _, err := os.Stat(sftpLog); !os.IsNotExist(err) {
		t.Fatalf("incomplete broker config should fail before SFTP staging, stat err = %v", err)
	}
}

func TestRunScenarioRealSSHPreflightFailsBeforeStagingWhenCommandPortRecoveryUnavailable(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`{"derived_status":"running","state":{"status":"running","call_server_ready":false,"maya_alive":true,"mcp_alive":true}}`,
		`@fresh-fail:Cannot find scheduled task MayaStallSessiondUI`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"restart gg_mayasessiond for a fresh Maya UI Session",
		"restart scheduled task",
		"MayaStallSessiondUI",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("run preflight error missing %q: %s", want, stderr.String())
		}
	}
	if _, err := os.Stat(sftpLog); !os.IsNotExist(err) {
		t.Fatalf("commandPort preflight failure should happen before SFTP staging, stat err = %v", err)
	}
}

func TestRunScenarioRealSSHFailsBeforeStagingWhenTrustedPluginAllowlistMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/elsewhere/plugins";
`),
	})
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "trusted Plugin Artifact allowlist preflight failed: maya 2025 SafeModeAllowedlistPaths does not contain trustedPluginArtifactsRoot") {
		t.Fatalf("run stderr missing trusted allowlist preflight:\n%s", stderr.String())
	}
	if _, err := os.Stat(sftpLog); !os.IsNotExist(err) {
		t.Fatalf("trusted allowlist preflight should happen before SFTP staging, stat err = %v", err)
	}
	runID := outputValue(t, stdout.String(), "run")
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("failed allowlist preflight Run State missing: %v", err)
	}
	bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", runID))
	if bundle.Failure == nil || bundle.Failure.FailedLayer != "remote-check" || bundle.Failure.CleanupState != "completed" {
		t.Fatalf("failed allowlist preflight minimal evidence = %+v", bundle.Failure)
	}
}

func TestRunScenarioRealSSHFailsBeforeStagingWhenNestedTrustedPluginDestinationMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	mustWriteFile(t, filepath.Join(dir, "build", "maya2025", "Release", "demo.mll"), "fake nested plugin artifact\n")
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	mustWriteFile(t, configPath, strings.Replace(string(configBytes), "build/demo.mll", "build/maya2025/Release/demo.mll", 1))
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts";
`),
	})
	hostConfigPath := writeTrustedPluginHostConfig(t, dir, sshPath, sftpPath)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "trusted Plugin Artifact allowlist preflight failed: maya 2025 SafeModeAllowedlistPaths does not contain trusted Plugin Artifact destination directories; run doctor with the same Scenario and --repair-trusted-plugin-allowlist") {
		t.Fatalf("run stderr missing nested trusted allowlist preflight:\n%s", stderr.String())
	}
	if _, err := os.Stat(sftpLog); !os.IsNotExist(err) {
		t.Fatalf("trusted allowlist preflight should happen before SFTP staging, stat err = %v", err)
	}
}

func TestRunScenarioRejectsUnknownScalarBrokerStatus(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        broker: gg-mayasessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `broker status "gg-mayasessiond" is not usable for runs`) {
		t.Fatalf("run error did not reject unknown scalar broker status: %s", stderr.String())
	}
}

func TestStandaloneSessiondScreenshotLabelsRealBrokerEvidence(t *testing.T) {
	dir := writeRunConfigFixture(t)
	jpegPath := filepath.Join(dir, "desktop-screenshot.jpg")
	if err := os.WriteFile(jpegPath, validJPEGBytes(t), 0o644); err != nil {
		t.Fatalf("write desktop screenshot fixture: %v", err)
	}
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		``,
		``,
		"@file:" + jpegPath,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"screenshot", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("screenshot exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.VisualEvidence) != 1 || bundle.VisualEvidence[0].MediaType != "image/png" {
		t.Fatalf("standalone screenshot metadata = %+v, want one PNG", bundle.VisualEvidence)
	}
	screenshotBytes, err := os.ReadFile(filepath.Join(evidence, filepath.FromSlash(bundle.VisualEvidence[0].Path)))
	if err != nil || !looksLikeImageBytes("image/png", screenshotBytes) {
		t.Fatalf("standalone screenshot is not a transcoded PNG: bytes=%v err=%v", screenshotBytes, err)
	}
	logBytes, err := os.ReadFile(filepath.Join(evidence, "logs", "session.log"))
	if err != nil {
		t.Fatalf("read evidence log: %v", err)
	}
	if strings.Contains(string(logBytes), "fake") || !strings.Contains(string(logBytes), "gg_mayasessiond") {
		t.Fatalf("standalone log mislabeled broker:\n%s", string(logBytes))
	}
	resultBytes, err := os.ReadFile(filepath.Join(evidence, "scenario-result.json"))
	if err != nil {
		t.Fatalf("read scenario result: %v", err)
	}
	if strings.Contains(string(resultBytes), "fake") || !strings.Contains(string(resultBytes), "gg_mayasessiond") {
		t.Fatalf("standalone result mislabeled broker:\n%s", string(resultBytes))
	}
}

func TestStandaloneSessiondScreenshotRequiresSSHHost(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"screenshot", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("screenshot exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "ssh.host is required") {
		t.Fatalf("screenshot error did not validate ssh.host: %s", stderr.String())
	}
	if entries, err := os.ReadDir(filepath.Join(dir, "artifacts", "maya-stall")); err == nil && len(entries) > 0 {
		t.Fatalf("failed standalone screenshot left Evidence Bundle dirs: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")); !os.IsNotExist(err) {
		t.Fatalf("host lock was not released after standalone screenshot fail-fast, stat err = %v", err)
	}
}

func TestGGMayaSessiondScreenshotNameFollowsDaemonMediaType(t *testing.T) {
	if got := visualEvidenceNameForMediaType("smoke.png", "image/jpeg"); got != "smoke.jpg" {
		t.Fatalf("jpeg screenshot name = %q, want smoke.jpg", got)
	}
	if got := visualEvidenceNameForMediaType("smoke.jpg", "image/png"); got != "smoke.png" {
		t.Fatalf("png screenshot name = %q, want smoke.png", got)
	}
	if got := visualEvidenceNameForMediaType("smoke.png", "image/webp"); got != "smoke.bin" {
		t.Fatalf("unknown media type screenshot name = %q, want smoke.bin", got)
	}
}

func TestGGMayaSessiondCaptureImageDataRequiresImageContent(t *testing.T) {
	var result sessiondCaptureResult
	result.Content = append(result.Content, struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}{
		Type:     "text",
		Data:     base64.StdEncoding.EncodeToString([]byte("not image")),
		MimeType: "text/plain",
	}, struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}{
		Type:     "image",
		Data:     base64.StdEncoding.EncodeToString([]byte("image bytes")),
		MimeType: "image/png",
	})

	data, mediaType, err := captureImageData(result)
	if err != nil {
		t.Fatalf("captureImageData returned error: %v", err)
	}
	if string(data) != "image bytes" || mediaType != "image/png" {
		t.Fatalf("captureImageData = %q/%q, want image bytes/image/png", string(data), mediaType)
	}
}

func TestGGMayaSessiondRecoveryScriptWaitsForRunningTaskToStop(t *testing.T) {
	script := sessiondRecoveryScript(mayaHostConfig{Broker: brokerConfig{
		StateDir: "C:/maya-stall/sessiond-ui",
		Python:   "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
		Repo:     "C:/maya-stall/tools/GG_MayaSessiond",
	}}, "MayaStallSessiondUI", "command-port-not-ready")

	for _, want := range []string{
		"gg_maya_sessiond.cli @('stop','--state-dir'",
		"'--wait-timeout-seconds','120'",
		"Stop-ScheduledTask",
		"for ($attempt = 0; $attempt -lt 90; $attempt++)",
		`if ($task.State -ne "Running")`,
		`throw "scheduled task $taskName did not stop before restart"`,
		"Start-ScheduledTask",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("recovery script missing %q:\n%s", want, script)
		}
	}
}

func TestVisualEvidenceBundlePrefersCapturedMediaType(t *testing.T) {
	got := mergeVisualEvidence(
		[]visualEvidenceArtifact{{Kind: "screenshot", Path: "screenshots/smoke.bin", MediaType: "image/webp"}},
		[]visualEvidenceArtifact{{Kind: "screenshot", Path: "screenshots/smoke.bin", MediaType: "application/octet-stream"}},
	)
	if len(got) != 1 {
		t.Fatalf("merged Visual Evidence count = %d, want 1", len(got))
	}
	if got[0].MediaType != "image/webp" {
		t.Fatalf("merged Visual Evidence media type = %q, want image/webp", got[0].MediaType)
	}
}

func TestGGMayaSessiondScenarioWrapperPreservesFailingScenarioResult(t *testing.T) {
	dir := writeRunConfigFixture(t)
	broker := ggMayaSessiondBroker{host: mayaHostConfig{
		WorkRoot: "C:/maya-stall",
		Broker: brokerConfig{
			Type:     "gg-mayasessiond",
			StateDir: "C:/maya-stall/sessiond-ui",
			Python:   "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
			Repo:     "C:/maya-stall/tools/GG_MayaSessiond",
		},
	}}
	config, _, err := loadRepoRunConfig(dir)
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	wrapper, err := broker.scenarioWrapper(runContext{RunWorkspace: mustRunWorkspace(t, dir, "run-1", "C:/maya-stall", config.Scenarios["smoke"].ExpectedOutputs.ScenarioResult)}, config.Scenarios["smoke"])
	if err != nil {
		t.Fatalf("build wrapper: %v", err)
	}
	for _, want := range []string{
		"def _maya_stall_should_overwrite_failure():",
		"return payload.get('status') in (None, '', 'passed')",
		"previous_cwd = os.getcwd()",
		"previous_sys_path = list(sys.path)",
		"def _maya_stall_clear_run_modules():",
		"class _MayaStallStopScenario(Exception):",
		"maya_stall_environment = ",
		"previous_environment = {}",
		"for include_path in reversed(",
		"Scenario exited before running all scripts",
		"sys.path[:] = previous_sys_path",
		"os.chdir(previous_cwd)",
		"_maya_stall_write_result('failed', str(exc), traceback.format_exc(), overwrite=_maya_stall_should_overwrite_failure())",
		"except SystemExit as exc:",
		"_maya_stall_write_result('failed', 'Scenario exited with code %s' % code, overwrite=_maya_stall_should_overwrite_failure())",
		"run_name='__main__'",
	} {
		if !strings.Contains(wrapper, want) {
			t.Fatalf("wrapper missing failure result field %q:\n%s", want, wrapper)
		}
	}
	if strings.Contains(wrapper, "\n    raise\n") {
		t.Fatalf("wrapper re-raises after writing failed Scenario Result:\n%s", wrapper)
	}
	assertPythonParses(t, wrapper)
}

func TestGGMayaSessiondScenarioWrapperProvidesTrustedPluginRoot(t *testing.T) {
	dir := writeRunConfigFixture(t)
	broker := ggMayaSessiondBroker{host: mayaHostConfig{
		WorkRoot: "C:/maya-stall",
		Broker: brokerConfig{
			Type:     "gg-mayasessiond",
			StateDir: "C:/maya-stall/sessiond-ui",
			Python:   "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
			Repo:     "C:/maya-stall/tools/GG_MayaSessiond",
		},
	}}
	config, _, err := loadRepoRunConfig(dir)
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	context := runContext{
		RunWorkspace: mustRunWorkspace(t, dir, "run-1", "C:/maya-stall", config.Scenarios["smoke"].ExpectedOutputs.ScenarioResult),
		Environment: map[string]string{
			scenarioResultEnvVar:             "C:/maya-stall/runs/run-1/workspace/outputs/smoke-result.json",
			trustedPluginArtifactsRootEnvVar: "C:/maya-stall/trusted-plugin-artifacts",
		},
	}

	wrapper, err := broker.scenarioWrapper(context, config.Scenarios["smoke"])
	if err != nil {
		t.Fatalf("build wrapper: %v", err)
	}
	for _, want := range []string{
		trustedPluginArtifactsRootEnvVar,
		"C:/maya-stall/trusted-plugin-artifacts",
		"previous_environment = {}",
		"for key, value in maya_stall_environment.items():",
		"os.environ[key] = value",
		"for key, value in previous_environment.items():",
	} {
		if !strings.Contains(wrapper, want) {
			t.Fatalf("wrapper missing trusted Plugin Artifact environment %q:\n%s", want, wrapper)
		}
	}
	assertPythonParses(t, wrapper)
}

func TestGGMayaSessiondScenarioWrapperFailsEarlyZeroSystemExit(t *testing.T) {
	python := pythonInterpreter(t)
	root := filepath.ToSlash(t.TempDir())
	remoteRunRoot := root + "/runs/run-1"
	remoteWorkspace := remoteRunRoot + "/workspace"
	scriptDir := filepath.FromSlash(remoteRunRoot + "/payload/mayaScripts/maya")
	if err := os.MkdirAll(scriptDir, 0o755); err != nil {
		t.Fatalf("create staged script dir: %v", err)
	}
	mustWriteFile(t, filepath.Join(scriptDir, "first.py"), "import sys\nsys.exit(0)\n")
	mustWriteFile(t, filepath.Join(scriptDir, "second.py"), "from pathlib import Path\nPath('second-ran.txt').write_text('ran', encoding='utf-8')\n")
	broker := ggMayaSessiondBroker{host: mayaHostConfig{
		WorkRoot: root,
		Broker: brokerConfig{
			Type:     "gg-mayasessiond",
			StateDir: root + "/state",
			Python:   python,
			Repo:     root + "/repo",
		},
	}}
	scenario := scenarioConfig{
		Payload: runPayload{Scripts: []string{"maya/first.py", "maya/second.py"}},
		ExpectedOutputs: expectedOutputs{
			ScenarioResult: "outputs/result.json",
		},
	}
	wrapper, err := broker.scenarioWrapper(runContext{RunWorkspace: mustRunWorkspace(t, root, "run-1", root, scenario.ExpectedOutputs.ScenarioResult)}, scenario)
	if err != nil {
		t.Fatalf("build wrapper: %v", err)
	}
	command := exec.Command(python, "-c", wrapper)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run wrapper: %v\n%s\n%s", err, string(output), wrapper)
	}
	resultBytes, err := os.ReadFile(filepath.FromSlash(remoteWorkspace + "/outputs/result.json"))
	if err != nil {
		t.Fatalf("read wrapper Scenario Result: %v", err)
	}
	var result ScenarioResult
	if err := json.Unmarshal(resultBytes, &result); err != nil {
		t.Fatalf("parse wrapper Scenario Result: %v", err)
	}
	if result.Status != resultStatusFailed || !strings.Contains(result.Summary, "before running all scripts") {
		t.Fatalf("wrapper Scenario Result = %+v, want failed early-exit result", result)
	}
	if _, err := os.Stat(filepath.FromSlash(remoteWorkspace + "/second-ran.txt")); !os.IsNotExist(err) {
		t.Fatalf("second script should not run after early SystemExit, stat err = %v", err)
	}
}

func assertPythonParses(t *testing.T, source string) {
	t.Helper()
	python := pythonInterpreter(t)
	command := exec.Command(python, "-c", "import ast, sys\nast.parse(sys.stdin.read())\n")
	command.Stdin = strings.NewReader(source)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("generated wrapper did not parse as Python: %v\n%s\n%s", err, string(output), source)
	}
}

func pythonInterpreter(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		python, err = exec.LookPath("python")
	}
	if err != nil {
		t.Skip("python interpreter not available for wrapper check")
	}
	return python
}

func TestGGMayaSessiondScenarioWrapperRejectsBackslashScenarioResultPath(t *testing.T) {
	broker := ggMayaSessiondBroker{}
	scenario := scenarioConfig{ExpectedOutputs: expectedOutputs{ScenarioResult: `..\..\Users\maya-runner\result.json`}}

	_, err := broker.scenarioWrapper(runContext{RunWorkspace: mustRunWorkspace(t, t.TempDir(), "run-1", "C:/maya-stall", "outputs/result.json")}, scenario)
	if err == nil {
		t.Fatal("scenarioWrapper returned nil error for backslash Scenario Result path")
	}
	if !strings.Contains(err.Error(), "forward slashes") {
		t.Fatalf("backslash Scenario Result error = %v", err)
	}
}

func TestGGMayaSessiondScenarioWrapperRejectsBackslashPayloadPaths(t *testing.T) {
	broker := ggMayaSessiondBroker{}
	for _, scenario := range []scenarioConfig{
		{
			Payload:         runPayload{Scripts: []string{`maya\..\..\evil.py`}},
			ExpectedOutputs: expectedOutputs{ScenarioResult: "outputs/result.json"},
		},
		{
			Payload:         runPayload{IncludePaths: []string{`includes\..\..\evil`}},
			ExpectedOutputs: expectedOutputs{ScenarioResult: "outputs/result.json"},
		},
	} {
		_, err := broker.scenarioWrapper(runContext{RunWorkspace: mustRunWorkspace(t, t.TempDir(), "run-1", "C:/maya-stall", scenario.ExpectedOutputs.ScenarioResult)}, scenario)
		if err == nil {
			t.Fatal("scenarioWrapper returned nil error for backslash payload path")
		}
		if !strings.Contains(err.Error(), "forward slashes") {
			t.Fatalf("backslash payload path error = %v, want forward slashes", err)
		}
	}
}

func TestGGMayaSessiondStageRemoteFileRejectsSFTPControlCharacters(t *testing.T) {
	broker := ggMayaSessiondBroker{host: mayaHostConfig{
		Transport: "ssh",
		SSH:       sshConfig{Host: "maya-win-01"},
		WorkRoot:  "C:/maya-stall",
		Broker: brokerConfig{
			Type:     "gg-mayasessiond",
			StateDir: "C:/maya-stall/sessiond-ui",
			Python:   "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
			Repo:     "C:/maya-stall/tools/GG_MayaSessiond",
		},
	}}

	err := broker.stageRemoteFile("C:/maya-stall/runs/probe\nput /tmp/leak C:/leak.py", []byte("probe\n"))
	if err == nil {
		t.Fatal("stageRemoteFile returned nil error for SFTP control characters")
	}
	if !strings.Contains(err.Error(), "control characters") {
		t.Fatalf("stageRemoteFile error = %v, want control characters", err)
	}
}

func TestSessiondJSONFromFailedOutputRequiresCompleteCLIJSON(t *testing.T) {
	valid, ok := sessiondJSONFromFailedOutput([]byte("traceback noise\n{\"ok\":false,\"error\":\"tool failed\"}\n"))
	if !ok {
		t.Fatal("sessiondJSONFromFailedOutput rejected complete sessiond JSON")
	}
	if !strings.Contains(string(valid), `"ok":false`) {
		t.Fatalf("sessiond JSON = %s", string(valid))
	}

	for _, raw := range [][]byte{
		[]byte("ssh failed before JSON"),
		[]byte("traceback {'not': 'json'}"),
		[]byte(`{"error":"missing ok"}`),
		[]byte(`{"ok":true,"tool":"script.execute"}`),
		[]byte(`{"level":"error","ok":false,"msg":"connection refused"}`),
	} {
		if got, ok := sessiondJSONFromFailedOutput(raw); ok {
			t.Fatalf("sessiondJSONFromFailedOutput accepted %q as %s", string(raw), string(got))
		}
	}
}

func TestTrimToJSONReturnsFirstDocumentWithTrailingNoise(t *testing.T) {
	got := trimToJSON([]byte("startup noise\n{\"ok\":true,\"tool\":\"status\"}\nshutdown noise\n"))
	if string(got) != `{"ok":true,"tool":"status"}` {
		t.Fatalf("trimToJSON = %q, want first JSON document", string(got))
	}
}

func TestTrimToJSONSkipsBracketedLogNoise(t *testing.T) {
	got := trimToJSON([]byte("[INFO] loading {state}\n{\"ok\":true,\"tool\":\"status\"}\n"))
	if string(got) != `{"ok":true,"tool":"status"}` {
		t.Fatalf("trimToJSON = %q, want JSON after bracketed log noise", string(got))
	}
}

func TestTrimToJSONSkipsStructuredJSONLogNoise(t *testing.T) {
	got := trimToJSON([]byte("{\"level\":\"info\",\"msg\":\"connecting\"}\n{\"ok\":true,\"tool\":\"status\"}\n"))
	if string(got) != `{"ok":true,"tool":"status"}` {
		t.Fatalf("trimToJSON = %q, want sessiond JSON after structured log noise", string(got))
	}
}

func TestTrimToJSONSkipsNestedStructuredJSONLogNoise(t *testing.T) {
	got := trimToJSON([]byte("{\"level\":\"info\",\"payload\":{\"ok\":true}}\n{\"ok\":false,\"error\":\"tool failed\"}\n"))
	if string(got) != `{"ok":false,"error":"tool failed"}` {
		t.Fatalf("trimToJSON = %q, want sessiond JSON after nested structured log noise", string(got))
	}
}

func TestTrimToJSONSkipsStructuredLogWithResultLikeKeys(t *testing.T) {
	got := trimToJSON([]byte("{\"level\":\"info\",\"state\":\"connecting\",\"ok\":\"soon\"}\n{\"ok\":false,\"error\":\"tool failed\"}\n"))
	if string(got) != `{"ok":false,"error":"tool failed"}` {
		t.Fatalf("trimToJSON = %q, want sessiond JSON after result-like structured log", string(got))
	}
}

func TestTrimToJSONSkipsStructuredLogWithBooleanOK(t *testing.T) {
	got := trimToJSON([]byte("{\"level\":\"info\",\"ok\":true,\"msg\":\"connected\"}\n{\"ok\":true,\"tool\":\"status\"}\n"))
	if string(got) != `{"ok":true,"tool":"status"}` {
		t.Fatalf("trimToJSON = %q, want sessiond JSON after boolean ok structured log", string(got))
	}
}

func TestTrimToJSONSkipsOkShapedLogNoise(t *testing.T) {
	got := trimToJSON([]byte("{\"ok\":true,\"msg\":\"starting\"}\n{\"ok\":true,\"tool\":\"script.execute\"}\n"))
	if string(got) != `{"ok":true,"tool":"script.execute"}` {
		t.Fatalf("trimToJSON = %q, want protocol JSON after ok-shaped log", string(got))
	}
}

func TestTrimToJSONAcceptsSessiondResultWithMessageField(t *testing.T) {
	got := trimToJSON([]byte("{\"level\":\"info\",\"ok\":true,\"msg\":\"connected\"}\n{\"ok\":false,\"error\":\"tool failed\",\"msg\":\"details\"}\n"))
	if string(got) != `{"ok":false,"error":"tool failed","msg":"details"}` {
		t.Fatalf("trimToJSON = %q, want sessiond JSON with message field", string(got))
	}
}

func TestTrimToJSONSkipsStructuredErrorLog(t *testing.T) {
	got := trimToJSON([]byte("{\"level\":\"error\",\"ok\":false,\"msg\":\"retrying\",\"error\":\"temporary\"}\n{\"ok\":true,\"tool\":\"status\"}\n"))
	if string(got) != `{"ok":true,"tool":"status"}` {
		t.Fatalf("trimToJSON = %q, want sessiond JSON after structured error log", string(got))
	}
}

func TestTrimToJSONSkipsStructuredLogWithObjectState(t *testing.T) {
	got := trimToJSON([]byte("{\"level\":\"info\",\"msg\":\"connecting\",\"state\":{\"phase\":\"startup\"}}\n{\"derived_status\":\"running\",\"state\":{\"status\":\"running\"}}\n"))
	if string(got) != `{"derived_status":"running","state":{"status":"running"}}` {
		t.Fatalf("trimToJSON = %q, want sessiond status JSON after object state structured log", string(got))
	}
}

func TestTrimToJSONPreservesArrayDocuments(t *testing.T) {
	got := trimToJSON([]byte(`[{"ok":true,"tool":"status"}]`))
	if string(got) != `[{"ok":true,"tool":"status"}]` {
		t.Fatalf("trimToJSON = %q, want full array document", string(got))
	}
}

func TestTrimToJSONSkipsArrayLogNoiseBeforeSessiondResult(t *testing.T) {
	got := trimToJSON([]byte("[{\"level\":\"info\"}]\n{\"ok\":true,\"tool\":\"status\"}\n"))
	if string(got) != `{"ok":true,"tool":"status"}` {
		t.Fatalf("trimToJSON = %q, want sessiond JSON after array log noise", string(got))
	}
}

func TestSessiondJSONFromFailedOutputSkipsBracketedLogNoise(t *testing.T) {
	got, ok := sessiondJSONFromFailedOutput([]byte("[ERROR] loading {state}\n{\"ok\":false,\"error\":\"tool failed\"}\n"))
	if !ok {
		t.Fatal("sessiondJSONFromFailedOutput rejected JSON after bracketed log noise")
	}
	if !strings.Contains(string(got), `"ok":false`) {
		t.Fatalf("sessiond JSON = %s", string(got))
	}
}

func TestSessiondJSONFromFailedOutputSkipsStructuredJSONLogNoise(t *testing.T) {
	got, ok := sessiondJSONFromFailedOutput([]byte("{\"level\":\"error\",\"msg\":\"tool failed\"}\n{\"ok\":false,\"error\":\"tool failed\"}\n"))
	if !ok {
		t.Fatal("sessiondJSONFromFailedOutput rejected sessiond JSON after structured log noise")
	}
	if !strings.Contains(string(got), `"ok":false`) {
		t.Fatalf("sessiond JSON = %s", string(got))
	}
}

func TestRunSessiondCLIRejectsStructuredLogWithoutResult(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`{"level":"error","ok":false,"msg":"connection refused","error":"temporary"}`,
	})
	broker := ggMayaSessiondBroker{host: mayaHostConfig{
		Transport: "ssh",
		SSH:       sshConfig{Host: "maya-win-01", Binary: sshPath},
		WorkRoot:  "C:/maya-stall",
		Broker: brokerConfig{
			Type:     "gg-mayasessiond",
			StateDir: "C:/maya-stall/sessiond-ui",
			Python:   "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
			Repo:     "C:/maya-stall/tools/GG_MayaSessiond",
		},
	}}

	_, err := broker.runSessiondCLI([]string{"status", "--json"}, sessiondCommandTimeout)
	if err == nil {
		t.Fatal("runSessiondCLI returned nil error for structured log without sessiond result")
	}
	if !strings.Contains(err.Error(), "returned no sessiond JSON result") {
		t.Fatalf("runSessiondCLI error = %v, want no sessiond JSON result", err)
	}
}

func TestSessiondStatusJSONFromNonzeroExit(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		wantError   bool
		wantDerived string
		wantSession string
	}{
		{
			name:        "stopped",
			output:      `@stdout-fail:{"state_dir":"C:/maya-stall/sessiond-ui","has_state":true,"derived_status":"stopped","state":{"status":"stopped","session_id":"session-stopped"},"process_alive":{"daemon":false,"maya":false,"mcp":false}}`,
			wantDerived: "stopped",
			wantSession: "session-stopped",
		},
		{
			name:        "missing",
			output:      `@stdout-fail:{"state_dir":"C:/maya-stall/sessiond-ui","has_state":false,"derived_status":"missing","state":{},"process_alive":{}}`,
			wantDerived: "missing",
		},
		{
			name:      "running",
			output:    `@stdout-fail:{"state_dir":"C:/maya-stall/sessiond-ui","has_state":true,"derived_status":"running","state":{"status":"running","session_id":"session-running"},"process_alive":{"daemon":true,"maya":true,"mcp":true}}`,
			wantError: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{test.output})
			broker := ggMayaSessiondBroker{host: mayaHostConfig{
				Transport: "ssh",
				SSH:       sshConfig{Host: "maya-win-01", Binary: sshPath},
				WorkRoot:  "C:/maya-stall",
				Broker: brokerConfig{
					Type:     "gg-mayasessiond",
					StateDir: "C:/maya-stall/sessiond-ui",
					Python:   "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
					Repo:     "C:/maya-stall/tools/GG_MayaSessiond",
				},
			}}

			status, err := broker.status()
			if test.wantError {
				if err == nil {
					t.Fatal("status returned nil error for non-stopped JSON from nonzero exit")
				}
				return
			}
			if err != nil {
				t.Fatalf("status returned error for inactive JSON: %v", err)
			}
			if status.DerivedStatus != test.wantDerived || status.State.SessionID != test.wantSession {
				t.Fatalf("status = %+v, want derived status %q and session %q", status, test.wantDerived, test.wantSession)
			}
		})
	}
}

func TestRunScenarioRealSSHRequiresDownloadedScenarioResult(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sftpPath := writeFailingGetSFTPCommand(t, dir)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "download declared outputs") {
		t.Fatalf("run error did not report required Scenario Result download failure: %s", stderr.String())
	}
}

func TestRunScenarioRealSSHAcceptsCollectedCompletionAfterBrokerDisconnect(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "files: []", "files:\n        - \"outputs/product-ui-e2e-result.json\"\n        - \"outputs/parity.json\"", 1)
	content = strings.Replace(content, "    evidence:", `    validators:
      - type: scenarioResultStatus
        status: passed
      - type: jsonEquals
        path: "outputs/product-ui-e2e-result.json"
        jsonPath: "$.product"
        equals: ok
      - type: numericApprox
        path: "outputs/parity.json"
        jsonPath: "$.maxAbsDiff"
        equals: 0
        tolerance: 0.001
    evidence:`, 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeScenarioOutputSFTPCommand(t, dir, sftpLog, map[string]string{
		"outputs/smoke-result.json":          `{"status":"passed","summary":"product Scenario passed","assertions":[{"name":"plugin loaded","passed":true}]}` + "\n",
		"outputs/product-ui-e2e-result.json": `{"status":"passed","product":"ok"}` + "\n",
		"outputs/parity.json":                `{"maxAbsDiff":3.55e-08,"mismatchCount":0}` + "\n",
	})
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":false,"tool":"script.execute","error":"Cannot connect to Maya commandPort at localhost:7001"}`,
		sessiondStatusFixture("session-fresh"),
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("run output missing passed status:\n%s", stdout.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Status != resultStatusPassed {
		t.Fatalf("Evidence Bundle status = %q, want passed", bundle.Status)
	}
	if len(bundle.Validators) != 3 {
		t.Fatalf("validator count = %d, want 3: %+v", len(bundle.Validators), bundle.Validators)
	}
	for _, result := range bundle.Validators {
		if result.Status != resultStatusPassed {
			t.Fatalf("validator = %+v, want passed", result)
		}
	}
	scenarioResultBytes, err := os.ReadFile(filepath.Join(evidence, "scenario-result.json"))
	if err != nil {
		t.Fatalf("read collected Scenario Result: %v", err)
	}
	var scenarioResult ScenarioResult
	if err := json.Unmarshal(scenarioResultBytes, &scenarioResult); err != nil {
		t.Fatalf("parse collected Scenario Result: %v", err)
	}
	if scenarioResult.Status != resultStatusPassed || scenarioResult.Summary != "product Scenario passed" {
		t.Fatalf("collected Scenario Result = %+v, want product pass", scenarioResult)
	}
	for _, path := range []string{
		filepath.Join(evidence, "outputs", "product-ui-e2e-result.json"),
		filepath.Join(evidence, "outputs", "parity.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("expected collected output %s: %v", path, err)
		}
	}
	eventsBytes, err := os.ReadFile(filepath.Join(evidence, "events.jsonl"))
	if err != nil {
		t.Fatalf("read failure events: %v", err)
	}
	if !strings.Contains(string(eventsBytes), "run.recovered-after-broker-failure") || !strings.Contains(string(eventsBytes), "Cannot connect to Maya commandPort") {
		t.Fatalf("Evidence Bundle events missing broker recovery:\n%s", string(eventsBytes))
	}
}

func TestRunScenarioRealSSHCollectsDeclaredOutputsWhenBrokerDisconnectsBeforeResult(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "files: []", "files:\n        - \"outputs/product-ui-e2e-result.json\"", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeMissingScenarioResultSFTPCommand(t, dir, sftpLog, "outputs/product-ui-e2e-result.json", `{"status":"passed","product":"ok"}`+"\n")
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":false,"tool":"script.execute","error":"Cannot connect to Maya commandPort at localhost:7001"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	if _, err := os.Stat(filepath.Join(evidence, "outputs", "product-ui-e2e-result.json")); err != nil {
		t.Fatalf("expected declared output despite missing Scenario Result: %v", err)
	}
	scenarioResultBytes, err := os.ReadFile(filepath.Join(evidence, "scenario-result.json"))
	if err != nil {
		t.Fatalf("read synthesized failure Scenario Result: %v", err)
	}
	if !strings.Contains(string(scenarioResultBytes), `"status": "failed"`) || !strings.Contains(string(scenarioResultBytes), "Cannot connect to Maya commandPort") {
		t.Fatalf("synthesized failure Scenario Result missing broker failure:\n%s", string(scenarioResultBytes))
	}
}

func TestRunScenarioRealSSHWritesFailureBundleWhenCollectedScenarioResultIsMalformed(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "files: []", "files:\n        - \"outputs/product-ui-e2e-result.json\"", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeScenarioOutputSFTPCommand(t, dir, sftpLog, map[string]string{
		"outputs/smoke-result.json":          `{"status":`,
		"outputs/product-ui-e2e-result.json": `{"status":"passed","product":"ok"}` + "\n",
	})
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":false,"tool":"script.execute","error":"Cannot connect to Maya commandPort at localhost:7001"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	if _, err := os.Stat(filepath.Join(evidence, "outputs", "product-ui-e2e-result.json")); err != nil {
		t.Fatalf("expected declared output despite malformed Scenario Result: %v", err)
	}
	scenarioResultBytes, err := os.ReadFile(filepath.Join(evidence, "scenario-result.json"))
	if err != nil {
		t.Fatalf("read synthesized failure Scenario Result: %v", err)
	}
	if !strings.Contains(string(scenarioResultBytes), `"status": "failed"`) || !strings.Contains(string(scenarioResultBytes), "Cannot connect to Maya commandPort") {
		t.Fatalf("synthesized failure Scenario Result missing broker failure:\n%s", string(scenarioResultBytes))
	}
	eventsBytes, err := os.ReadFile(filepath.Join(evidence, "events.jsonl"))
	if err != nil {
		t.Fatalf("read failure events: %v", err)
	}
	if !strings.Contains(string(eventsBytes), "scenario-result-unreadable") {
		t.Fatalf("failure events missing malformed Scenario Result note:\n%s", string(eventsBytes))
	}
}

func TestRunScenarioRealSSHDownloadsValidatorOnlyOutputs(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "evidence:\n      screenshots:\n        enabled: false", "evidence:\n      screenshots:\n        enabled: false\n    validators:\n      - type: jsonEquals\n        path: \"outputs/metrics.json\"\n        jsonPath: \"$.status\"\n        equals: passed", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	sftpLog := filepath.Join(dir, "sftp.log")
	sftpPath := writeFakeSFTPCommand(t, dir, sftpLog)
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-alpha"),
		`{"ok":true,"tool":"script.execute"}`,
		sessiondStatusFixture("session-fresh"),
		`{"ok":true,"status":"stopped"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	logBytes, err := os.ReadFile(sftpLog)
	if err != nil {
		t.Fatalf("read sftp log: %v", err)
	}
	if !strings.Contains(string(logBytes), "workspace/outputs/metrics.json") {
		t.Fatalf("sftp batch did not download validator-only output:\n%s", string(logBytes))
	}
}

func TestRunScenarioRealSSHRejectsNestedSymlinkPayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "leak.py"), "print('outside')\n")
	if err := os.MkdirAll(filepath.Join(dir, "includes"), 0o755); err != nil {
		t.Fatalf("create includes fixture: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "leak.py"), filepath.Join(dir, "includes", "leak.py")); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      includePaths:
        - "includes"
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Fatalf("run error did not reject nested symlink payload: %s", stderr.String())
	}
}

func TestRunScenarioRealSSHRejectsSFTPBatchControlCharacters(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "maya", "smoke.py"), "print('smoke')\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), "version: 1\nscenarios:\n  smoke:\n    payload:\n      scripts:\n        - \"maya/smoke.py\"\n    expectedOutputs:\n      files:\n        - \"outputs/report.json\\nput /tmp/leak C:/maya-stall/leak\"\n      scenarioResult: \"outputs/result.json\"\n")
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`{"ok":true,"tool":"script.execute"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "control characters") {
		t.Fatalf("run error did not reject SFTP batch control characters: %s", stderr.String())
	}
}

func TestRunScenarioRealSSHRejectsBackslashRemoteArtifactPath(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "maya", "smoke.py"), "print('smoke')\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), "version: 1\nscenarios:\n  smoke:\n    payload:\n      scripts:\n        - \"maya/smoke.py\"\n    expectedOutputs:\n      files:\n        - \"..\\\\..\\\\Users\\\\maya-runner\\\\secret.json\"\n      scenarioResult: \"outputs/result.json\"\n")
	sftpPath := writeFakeSFTPCommand(t, dir, filepath.Join(dir, "sftp.log"))
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`{"ok":true,"tool":"script.execute"}`,
	})
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:\maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "backslashes") {
		t.Fatalf("run error did not reject backslash artifact path: %s", stderr.String())
	}
}

func TestRunScenarioRejectsSymlinkedDownloadDestination(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	runtimeConfig := defaultRunRuntime()
	runtimeConfig.Broker = symlinkOutputBroker{linkPath: "outputs", target: t.TempDir()}
	var stdout, stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtimeConfig)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Fatalf("run error did not reject symlinked download destination: %s", stderr.String())
	}
}

func TestRunScenarioSkipsLockedHostForNextHealthyHost(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        health: healthy
      - id: beta
        health: healthy
`)
	release, locked, err := acquireHostLock(dir, "alpha")
	if err != nil {
		t.Fatalf("pre-lock alpha: %v", err)
	}
	if locked {
		t.Fatal("alpha was already locked")
	}
	defer release()
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: beta") {
		t.Fatalf("run output missing next unlocked host:\n%s", stdout.String())
	}
}

func TestRunScenarioRemovesStaleHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeSingleHealthyHostConfig(t, dir)
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	mustWriteFile(t, lockPath, "host: alpha\npid: -1\n")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host-lock-fail-fast", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: alpha") {
		t.Fatalf("run output missing host after stale lock removal:\n%s", stdout.String())
	}
}

func TestRunScenarioRemovesMalformedHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeSingleHealthyHostConfig(t, dir)
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	mustWriteFile(t, lockPath, "host: alpha\n")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host-lock-fail-fast", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: alpha") {
		t.Fatalf("run output missing host after malformed lock removal:\n%s", stdout.String())
	}
}

func TestRunScenarioHostPinSelectsRequestedHostOrFailsClearly(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        health: healthy
      - id: beta
        health: healthy
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "beta", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("pinned run exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: beta") {
		t.Fatalf("pinned run output missing requested host:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "gamma", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("missing pinned host exit code = %d, want 1; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `pinned Maya Host "gamma" is not in Target Profile "ci"`) {
		t.Fatalf("missing pinned host error = %q", stderr.String())
	}
}

func TestRunScenarioHostLockPreventsConcurrentFailFastRun(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeSingleHealthyHostConfig(t, dir)
	firstBroker := newBlockingBroker(ScenarioResult{Status: "passed", Summary: "first done"})
	firstDone := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		runtime := defaultRunRuntime()
		runtime.Broker = firstBroker
		firstDone <- RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	}()
	<-firstBroker.started
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("active run did not hold host lock: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host-lock-fail-fast", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("concurrent fail-fast run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `no healthy unlocked Maya Host available in Target Profile "ci"`) {
		t.Fatalf("concurrent lock error = %q", stderr.String())
	}

	close(firstBroker.release)
	select {
	case code := <-firstDone:
		if code != 0 {
			t.Fatalf("first run exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not finish after release")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("host lock was not released: %v", err)
	}
}

func TestRunScenarioHostSideLockPreventsConcurrentRunAcrossCheckouts(t *testing.T) {
	firstDir := writeRunConfigFixture(t)
	secondDir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	firstHostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, firstDir, hostRoot)
	secondHostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, secondDir, hostRoot)
	firstBroker := newBlockingBroker(ScenarioResult{Status: "passed", Summary: "first done"})
	firstDone := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		runtime := defaultRunRuntime()
		runtime.Broker = firstBroker
		firstDone <- RunWithRuntime([]string{"run", "--host-config", firstHostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, firstDir, "test-version", runtime)
	}()
	<-firstBroker.started
	hostLockPath := filepath.Join(hostRoot, "state", "locks", "hosts", "alpha.lock")
	hostLockBytes, err := os.ReadFile(hostLockPath)
	if err != nil {
		t.Fatalf("active run did not hold host-side Host Lock: %v", err)
	}
	hostLockOwner := parseHostLockOwner(string(hostLockBytes))
	if hostLockOwner.ActiveRun == "" || hostLockOwner.LockToken == "" {
		t.Fatalf("active host-side Host Lock did not bind Run ID and token: %q", hostLockBytes)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--host-config", secondHostConfigPath, "--target-profile", "ci", "--host-lock-fail-fast", "smoke"}, &stdout, &stderr, secondDir, "test-version")
	if code != 1 {
		t.Fatalf("concurrent cross-checkout run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `no healthy unlocked Maya Host available in Target Profile "ci"`) {
		t.Fatalf("cross-checkout lock error = %q", stderr.String())
	}

	close(firstBroker.release)
	select {
	case code := <-firstDone:
		if code != 0 {
			t.Fatalf("first run exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first run did not finish after release")
	}
	if _, err := os.Stat(hostLockPath); !os.IsNotExist(err) {
		t.Fatalf("host-side Host Lock was not released: %v", err)
	}
}

func TestRunScenarioRenewsHostSideLockAcrossLeaseBoundary(t *testing.T) {
	oldLease := hostSideLockLeaseDuration
	oldHeartbeat := hostSideLockHeartbeatInterval
	hostSideLockLeaseDuration = 150 * time.Millisecond
	hostSideLockHeartbeatInterval = 25 * time.Millisecond
	t.Cleanup(func() {
		hostSideLockLeaseDuration = oldLease
		hostSideLockHeartbeatInterval = oldHeartbeat
	})

	firstDir := writeRunConfigFixture(t)
	secondDir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	firstConfig := writeSingleHealthyHostConfigWithWorkRoot(t, firstDir, hostRoot)
	secondConfig := writeSingleHealthyHostConfigWithWorkRoot(t, secondDir, hostRoot)
	firstBroker := newBlockingBroker(ScenarioResult{Status: "passed"})
	firstDone := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		runtime := defaultRunRuntime()
		runtime.Broker = firstBroker
		firstDone <- RunWithRuntime([]string{"run", "--host-config", firstConfig, "--target-profile", "ci", "smoke"}, &stdout, &stderr, firstDir, "test-version", runtime)
	}()
	<-firstBroker.started
	time.Sleep(2 * hostSideLockLeaseDuration)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--host-config", secondConfig, "--target-profile", "ci", "--host-lock-fail-fast", "smoke"}, &stdout, &stderr, secondDir, "test-version")
	if code != 1 || !strings.Contains(stderr.String(), "no healthy unlocked Maya Host available") {
		t.Fatalf("successor crossed renewed lease: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	close(firstBroker.release)
	if code := <-firstDone; code != 0 {
		t.Fatalf("first run exit code = %d, want 0", code)
	}
}

func TestRunScenarioCleansUpFreshRunStateAndReleasesHostLockByDefault(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeSingleHealthyHostConfig(t, dir)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopPolicy: stopped") {
		t.Fatalf("run output missing stopped Stop Policy:\n%s", stdout.String())
	}
	onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runsDir := filepath.Join(dir, ".maya-stall", "state", "runs")
	if entries, err := os.ReadDir(runsDir); err == nil && len(entries) != 0 {
		t.Fatalf("run state entries after default cleanup = %d, want 0", len(entries))
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read run state after cleanup: %v", err)
	}
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("host lock after default cleanup = %v, want missing", err)
	}
}

func TestKeepOnFailureLeavesSessionForStatusAttachAndStop(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "fake failure"}}

	code := RunWithRuntime([]string{"run", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	for _, want := range []string{
		"status: failed",
		"stopPolicy: kept",
		"next: maya-stall status --run " + runID,
		"next: maya-stall attach " + runID,
		"next: maya-stall stop " + runID,
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("run output missing %q:\n%s", want, stdout.String())
		}
	}
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("kept Host Lock missing: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("status exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{"run: " + runID, "state: kept", "host: " + defaultFakeHostID, "status: failed"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("attach exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, want := range []string{"events:", "run.started", "broker.session.finished", "logs:", "fake Session Broker ran Scenario"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("attach output missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "stopped: "+runID) {
		t.Fatalf("stop output missing run id:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !os.IsNotExist(err) {
		t.Fatalf("kept run state after stop = %v, want missing", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("kept Host Lock after stop = %v, want missing", err)
	}
}

func TestStopReleasesKeptHostSideLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, dir, hostRoot)
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "keep for inspection"}}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	hostLockPath := filepath.Join(hostRoot, "state", "locks", "hosts", "alpha.lock")
	content, err := os.ReadFile(hostLockPath)
	if err != nil {
		t.Fatalf("read kept host-side Host Lock: %v", err)
	}
	if owner := parseHostLockOwner(string(content)); owner.KeptRun != runID {
		t.Fatalf("host-side Host Lock owner = %+v, want kept run %s", owner, runID)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(hostLockPath); !os.IsNotExist(err) {
		t.Fatalf("host-side Host Lock after stop = %v, want missing", err)
	}
}

func TestStopRejectsAuthoritativeActiveHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, dir, hostRoot)
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "keep for inspection"}}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	hostLockPath := filepath.Join(hostRoot, "state", "locks", "hosts", "alpha.lock")
	mustWriteFile(t, hostLockPath, fmt.Sprintf("host: alpha\nlockToken: active-owner\nactiveRun: %s\nleaseExpiresAt: %s\nleaseDurationSeconds: 30\n", runID, time.Now().UTC().Add(time.Minute).Format(time.RFC3339Nano)))

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("active authoritative stop exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(hostLockPath); err != nil {
		t.Fatalf("active authoritative Host Lock changed: %v", err)
	}
}

func TestStopReconcilesAlreadyReleasedHostSideLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, dir, hostRoot)
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "keep for retry"}}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	hostLockPath := filepath.Join(hostRoot, "state", "locks", "hosts", "alpha.lock")
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	manifest := readRunManifestFile(t, filepath.Join(stateDir, "manifest.json"))
	record, err := readRunRetentionRecord(dir, stateDir, manifest)
	if err != nil {
		t.Fatalf("read kept run record: %v", err)
	}
	record.StopPhase = "broker-stopped"
	if err := writeRunRetentionRecord(runContext{StateDir: stateDir}, record); err != nil {
		t.Fatalf("record completed broker stop phase: %v", err)
	}
	if err := os.Remove(hostLockPath); err != nil {
		t.Fatalf("simulate prior authoritative release: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("retry stop exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("run state after retry stop = %v, want missing", err)
	}
}

func TestStopRefusesToReleaseOnlyAuthoritativeLocalMirrorWithoutRunRecord(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, dir, hostRoot)
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "keep without record"}}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	if err := os.RemoveAll(stateDir); err != nil {
		t.Fatalf("simulate missing run record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 || !strings.Contains(stderr.String(), "refusing to release only the repo-local Host Lock mirror") {
		t.Fatalf("recordless stop result: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	for _, lockPath := range []string{
		filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", "alpha.lock"),
		filepath.Join(hostRoot, "state", "locks", "hosts", "alpha.lock"),
	} {
		if _, err := os.Stat(lockPath); err != nil {
			t.Fatalf("recordless stop removed lock %s: %v", lockPath, err)
		}
	}
}

func TestStatusRejectsStaleLocalOwnerAfterHostSideLockChanges(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostRoot := t.TempDir()
	hostConfigPath := writeSingleHealthyHostConfigWithWorkRoot(t, dir, hostRoot)
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed"}}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	hostLockPath := filepath.Join(hostRoot, "state", "locks", "hosts", "alpha.lock")
	mustWriteFile(t, hostLockPath, "host: alpha\nlockToken: successor-token\nkeptRun: successor-run\ncreatedAt: 2026-07-13T00:00:00Z\n")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 || !strings.Contains(stderr.String(), "Host Lock ownership changed") {
		t.Fatalf("status trusted stale local ownership: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestKeepOnFailurePrintsRetainedRunWhenBrokerExecutionErrors(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = failingRetainableSessionBroker{
		fakeSessionBroker: fakeSessionBroker{},
		message:           "broker disconnected before result",
	}

	code := RunWithRuntime([]string{"run", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"status: failed", "stopPolicy: kept", "next: maya-stall stop"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("run output missing %q:\n%s", want, stdout.String())
		}
	}
	if !strings.Contains(stderr.String(), "broker disconnected before result") {
		t.Fatalf("run stderr missing broker error: %s", stderr.String())
	}
}

func TestEvidenceCollectPrintsRetainedRunWhenBrokerExecutionErrors(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = failingRetainableSessionBroker{
		fakeSessionBroker: fakeSessionBroker{},
		message:           "broker disconnected before result",
	}

	code := RunWithRuntime([]string{"evidence", "collect", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("evidence collect exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"status: failed", "stopPolicy: kept", "next: maya-stall stop"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("evidence collect output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestStopReleasesKeptHostLockWhenEvidenceIsMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("kept Host Lock missing: %v", err)
	}
	if err := os.Rename(filepath.Join(dir, "artifacts", "maya-stall", runID, "evidence.json"), filepath.Join(dir, "artifacts", "maya-stall", runID, "evidence.json.moved")); err != nil {
		t.Fatalf("move evidence fixture: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("kept Host Lock after stop = %v, want missing", err)
	}
}

func TestAttachWorksWhenEvidenceIsMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	if err := os.Rename(filepath.Join(dir, "artifacts", "maya-stall", runID, "evidence.json"), filepath.Join(dir, "artifacts", "maya-stall", runID, "evidence.json.moved")); err != nil {
		t.Fatalf("move evidence fixture: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("attach exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"events:", "run.started", "logs:", "fake Session Broker ran Scenario"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("attach output missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunStateCommandsRejectDotRunIDs(t *testing.T) {
	for _, command := range []string{"status", "attach", "stop"} {
		for _, runID := range []string{".", ".."} {
			t.Run(command+" "+runID, func(t *testing.T) {
				dir := writeRunConfigFixture(t)
				var stdout, stderr bytes.Buffer
				args := []string{command}
				if command == "status" {
					args = append(args, "--run")
				}
				args = append(args, runID)

				code := Run(args, &stdout, &stderr, dir, "test-version")
				if code != 2 {
					t.Fatalf("%s exit code = %d, want 2; stdout: %s stderr: %s", command, code, stdout.String(), stderr.String())
				}
				if !strings.Contains(stderr.String(), "run id") {
					t.Fatalf("%s error missing run id detail: %s", command, stderr.String())
				}
			})
		}
	}
}

func TestStatusIgnoresPartialNonKeptRunState(t *testing.T) {
	dir := writeRunConfigFixture(t)
	if err := os.MkdirAll(filepath.Join(dir, ".maya-stall", "state", "runs", "partial"), 0o755); err != nil {
		t.Fatalf("create partial run state: %v", err)
	}
	var stdout, stderr bytes.Buffer

	code := Run([]string{"status"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("status exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: no kept sessions") {
		t.Fatalf("status output did not ignore partial state:\n%s", stdout.String())
	}
}

func TestStatusRejectsSymlinkedLockDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".maya-stall", "state", "locks"), 0o755); err != nil {
		t.Fatalf("create lock parent: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".maya-stall", "state", "locks", "hosts")); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}
	var stdout, stderr bytes.Buffer

	code := Run([]string{"status"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("status exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func TestTargetedStatusAndAttachRequireKeptHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	if err := os.Rename(lockPath, lockPath+".moved"); err != nil {
		t.Fatalf("move Host Lock fixture: %v", err)
	}

	for _, args := range [][]string{
		{"status", "--run", runID},
		{"attach", runID},
	} {
		stdout.Reset()
		stderr.Reset()
		code := Run(args, &stdout, &stderr, dir, "test-version")
		if code == 0 {
			t.Fatalf("%v exit code = 0, want failure; stdout: %s stderr: %s", args, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String(), "state: kept") {
			t.Fatalf("%v misreported stale state as kept:\n%s", args, stdout.String())
		}
	}
}

func TestStatusRejectsSymlinkedEvidenceParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "maya-stall", runID, "evidence.json"), `{"runId":"`+runID+`","status":"passed"}`+"\n")
	artifactsPath := filepath.Join(dir, "artifacts")
	if err := os.Rename(artifactsPath, artifactsPath+".real"); err != nil {
		t.Fatalf("move artifacts fixture: %v", err)
	}
	if err := os.Symlink(outside, artifactsPath); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("status exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func TestStopDoesNotReleaseHostLockForDifferentKeptRun(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	mustWriteFile(t, lockPath, "host: "+defaultFakeHostID+"\nkeptRun: different-run\n")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("stop exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	content, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read Host Lock after mismatched stop: %v", err)
	}
	if !strings.Contains(string(content), "keptRun: different-run") {
		t.Fatalf("Host Lock was changed by mismatched stop:\n%s", string(content))
	}
}

func TestStopReleasesMatchingKeptHostLockWhenRunStateIsMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	runID := "20260704T112500.000000000Z"
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	mustWriteFile(t, lockPath, "host: "+defaultFakeHostID+"\nkeptRun: "+runID+"\n")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("kept Host Lock after orphan stop = %v, want missing", err)
	}
}

func TestStopRejectsSymlinkedRunStateParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	runID := "20260704T112300.000000000Z"
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, runID, "manifest.json"), `{"runId":"`+runID+`","scenario":"smoke","targetProfile":"default","host":"`+defaultFakeHostID+`"}`+"\n")
	if err := os.MkdirAll(filepath.Join(dir, ".maya-stall", "state"), 0o755); err != nil {
		t.Fatalf("create state parent: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".maya-stall", "state", "runs")); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	mustWriteFile(t, lockPath, "host: "+defaultFakeHostID+"\nkeptRun: "+runID+"\n")
	var stdout, stderr bytes.Buffer

	code := Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("stop exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(outside, runID, "manifest.json")); err != nil {
		t.Fatalf("outside run state was removed or changed: %v", err)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("Host Lock was released after failed stop: %v", err)
	}
}

func TestCleanupRunStateRejectsSymlinkedRunStateParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := t.TempDir()
	runID := "20260704T112400.000000000Z"
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, runID, "sentinel.txt"), "keep me\n")
	if err := os.MkdirAll(filepath.Join(dir, ".maya-stall", "state"), 0o755); err != nil {
		t.Fatalf("create state parent: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, ".maya-stall", "state", "runs")); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}

	err := cleanupRunState(dir, runID)
	if err == nil {
		t.Fatal("cleanupRunState returned nil error for symlinked run state parent")
	}
	if _, statErr := os.Stat(filepath.Join(outside, runID, "sentinel.txt")); statErr != nil {
		t.Fatalf("outside run state was removed or changed: %v", statErr)
	}
}

func TestRunRejectsConflictingStopPolicyFlags(t *testing.T) {
	for _, args := range [][]string{
		{"run", "--keep-on-failure", "--stop-after", "always", "smoke"},
		{"run", "--stop-after", "never", "--keep-on-failure", "smoke"},
	} {
		t.Run(strings.Join(args[1:len(args)-1], " "), func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			var stdout, stderr bytes.Buffer

			code := Run(args, &stdout, &stderr, dir, "test-version")
			if code != 2 {
				t.Fatalf("run exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "Stop Policy") {
				t.Fatalf("conflicting Stop Policy error = %q", stderr.String())
			}
		})
	}
}

func TestAttachRejectsSymlinkedEventOrLogFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	tests := []struct {
		name       string
		path       string
		linkParent bool
	}{
		{name: "events", path: "events.jsonl"},
		{name: "log", path: filepath.Join("logs", "session.log")},
		{name: "log parent", path: "logs", linkParent: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			var stdout, stderr bytes.Buffer
			code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
			if code != 0 {
				t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			runID := runIDFromOutput(t, stdout.String())
			target := filepath.Join(t.TempDir(), "secret.txt")
			if tt.linkParent {
				target = t.TempDir()
				mustWriteFile(t, filepath.Join(target, "session.log"), "do not print\n")
			} else {
				mustWriteFile(t, target, "do not print\n")
			}
			linkPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, tt.path)
			if err := os.Rename(linkPath, linkPath+".real"); err != nil {
				t.Fatalf("move %s fixture: %v", tt.path, err)
			}
			if err := os.Symlink(target, linkPath); err != nil {
				t.Skipf("create symlink fixture: %v", err)
			}

			stdout.Reset()
			stderr.Reset()
			code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
			if code != 1 {
				t.Fatalf("attach exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			if strings.Contains(stdout.String(), "do not print") {
				t.Fatalf("attach printed symlink target:\n%s", stdout.String())
			}
		})
	}
}

func TestAttachRejectsScenarioResultPathOutsideRunState(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")
	recordBytes, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(recordBytes, &record); err != nil {
		t.Fatalf("parse run record: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	mustWriteFile(t, outside, "do not print\n")
	record["scenarioResultPath"] = outside
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("write tampered run record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("attach exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "do not print") {
		t.Fatalf("attach printed outside scenario result:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "must stay under kept run workspace") {
		t.Fatalf("attach error missing path validation: %s", stderr.String())
	}
}

func TestRunScenarioHonorsExplicitStopAfterPolicy(t *testing.T) {
	tests := []struct {
		name       string
		stopAfter  string
		result     string
		wantPolicy string
	}{
		{name: "success", stopAfter: "success", result: "passed", wantPolicy: "stopped"},
		{name: "success keeps failure", stopAfter: "success", result: "failed", wantPolicy: "kept"},
		{name: "failure", stopAfter: "failure", result: "failed", wantPolicy: "stopped"},
		{name: "failure keeps success", stopAfter: "failure", result: "passed", wantPolicy: "kept"},
		{name: "always", stopAfter: "always", result: "passed", wantPolicy: "stopped"},
		{name: "never", stopAfter: "never", result: "passed", wantPolicy: "kept"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			var stdout, stderr bytes.Buffer
			runtime := defaultRunRuntime()
			runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: tt.result, Summary: "fake result"}}

			code := RunWithRuntime([]string{"run", "--stop-after", tt.stopAfter, "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
			wantCode := 0
			if tt.result != resultStatusPassed {
				wantCode = 1
			}
			if code != wantCode {
				t.Fatalf("run exit code = %d, want %d; stdout: %s stderr: %s", code, wantCode, stdout.String(), stderr.String())
			}
			runID := runIDFromOutput(t, stdout.String())
			if !strings.Contains(stdout.String(), "stopPolicy: "+tt.wantPolicy) {
				t.Fatalf("run output missing Stop Policy %q:\n%s", tt.wantPolicy, stdout.String())
			}
			_, stateErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID))
			_, lockErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock"))
			if tt.wantPolicy == "kept" {
				if stateErr != nil {
					t.Fatalf("kept run state missing: %v", stateErr)
				}
				if lockErr != nil {
					t.Fatalf("kept Host Lock missing: %v", lockErr)
				}
				return
			}
			if !os.IsNotExist(stateErr) {
				t.Fatalf("stopped run state = %v, want missing", stateErr)
			}
			if !os.IsNotExist(lockErr) {
				t.Fatalf("stopped Host Lock = %v, want missing", lockErr)
			}
		})
	}
}

func TestRunScenarioHostLockWaitsForRelease(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeSingleHealthyHostConfig(t, dir)
	firstBroker := newBlockingBroker(ScenarioResult{Status: "passed", Summary: "first done"})
	firstDone := make(chan int, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		runtime := defaultRunRuntime()
		runtime.Broker = firstBroker
		firstDone <- RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	}()
	<-firstBroker.started

	secondDone := make(chan runResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host-lock-wait", "2s", "smoke"}, &stdout, &stderr, dir, "test-version")
		secondDone <- runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()
	select {
	case result := <-secondDone:
		t.Fatalf("waiting run finished before lock release: %+v", result)
	case <-time.After(100 * time.Millisecond):
	}

	close(firstBroker.release)
	select {
	case code := <-firstDone:
		if code != 0 {
			t.Fatalf("first run exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first run did not finish after release")
	}
	select {
	case result := <-secondDone:
		if result.code != 0 {
			t.Fatalf("waiting run exit code = %d, want 0; stdout: %s stderr: %s", result.code, result.stdout, result.stderr)
		}
		if !strings.Contains(result.stdout, "host: alpha") {
			t.Fatalf("waiting run output missing host:\n%s", result.stdout)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiting run did not finish after lock release")
	}
}

func TestRunScenarioReportsHostLockReleaseFailure(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := writeSingleHealthyHostConfig(t, dir)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = lockCorruptingBroker{}

	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "release Host Lock for alpha") {
		t.Fatalf("release failure error = %q", stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Status != resultStatusFailed || bundle.Failure == nil || bundle.Failure.CleanupState != "failed" {
		t.Fatalf("Host Lock release failure evidence = %+v", bundle)
	}
	events, err := os.ReadFile(filepath.Join(evidence, evidenceEventsFileName))
	if err != nil || !strings.Contains(string(events), `"event":"run.failed"`) {
		t.Fatalf("Host Lock release terminal event = %q err %v", string(events), err)
	}
	requireSequencedRunEvents(t, filepath.Join(evidence, evidenceEventsFileName))
}

func TestRunScenarioResultFailureDrivesExitCode(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := defaultRunRuntime()
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: "failed", Summary: "fake broker result"}}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: failed") {
		t.Fatalf("run output missing failed status:\n%s", stdout.String())
	}
}

func TestRunScenarioResultStatusValidatorFailureDrivesRunStatusAndEvidenceMetadata(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
      - type: scenarioResultStatus
        status: failed
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: failed") {
		t.Fatalf("run output missing failed status:\n%s", stdout.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Status != "failed" {
		t.Fatalf("evidence status = %q, want failed", bundle.Status)
	}
	scenarioResultBytes, err := os.ReadFile(filepath.Join(evidence, bundle.ScenarioResult))
	if err != nil {
		t.Fatalf("read evidence Scenario Result: %v", err)
	}
	var scenarioResult ScenarioResult
	if err := json.Unmarshal(scenarioResultBytes, &scenarioResult); err != nil {
		t.Fatalf("parse evidence Scenario Result: %v", err)
	}
	if scenarioResult.Status != "failed" {
		t.Fatalf("evidence Scenario Result status = %q, want failed", scenarioResult.Status)
	}
	if len(bundle.Validators) != 1 {
		t.Fatalf("validator result count = %d, want 1", len(bundle.Validators))
	}
	got := bundle.Validators[0]
	if got.Type != "scenarioResultStatus" || got.Status != "failed" || !strings.Contains(got.Message, `status "passed"`) {
		t.Fatalf("validator metadata = %+v", got)
	}
}

func TestRunScenarioValidatorsCanInspectScenarioResultOutput(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
      - type: jsonEquals
        path: "outputs/result.json"
        jsonPath: "$.status"
        equals: passed
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.Validators) != 1 || bundle.Validators[0].Status != "passed" {
		t.Fatalf("validator metadata = %+v", bundle.Validators)
	}
}

func TestRunScenarioValidatorsCoverOutputsJSONHashesAndVisualEvidence(t *testing.T) {
	reportHash := sha256.Sum256([]byte("hash me\n"))
	tests := []struct {
		name      string
		validator string
		broker    sessionBroker
	}{
		{
			name: "required output existence",
			validator: `      - type: outputExists
        path: "outputs/report.txt"
`,
			broker: outputWritingBroker{files: map[string]string{"outputs/report.txt": "exists\n"}},
		},
		{
			name: "json path equality",
			validator: `      - type: jsonEquals
        path: "outputs/metrics.json"
        jsonPath: "$.plugin.loaded"
        equals: true
`,
			broker: outputWritingBroker{files: map[string]string{"outputs/metrics.json": `{"plugin":{"loaded":true}}` + "\n"}},
		},
		{
			name: "json path equality normalizes nested numbers",
			validator: `      - type: jsonEquals
        path: "outputs/metrics.json"
        jsonPath: "$.mesh"
        equals:
          bounds: [1, 2, 3]
          vertexCount: 4
`,
			broker: outputWritingBroker{files: map[string]string{"outputs/metrics.json": `{"mesh":{"bounds":[1,2,3],"vertexCount":4}}` + "\n"}},
		},
		{
			name: "numeric approximate equality",
			validator: `      - type: numericApprox
        path: "outputs/metrics.json"
        jsonPath: "$.timings.solveMs"
        equals: 12.5
        tolerance: 0.1
`,
			broker: outputWritingBroker{files: map[string]string{"outputs/metrics.json": `{"timings":{"solveMs":12.45}}` + "\n"}},
		},
		{
			name: "numeric array approximate equality",
			validator: `      - type: numericApprox
        path: "outputs/metrics.json"
        jsonPath: "$.mesh.bounds"
        equals: [1.0, 2.0, 3.0]
        tolerance: 0.1
`,
			broker: outputWritingBroker{files: map[string]string{"outputs/metrics.json": `{"mesh":{"bounds":[1.01,1.99,3.05]}}` + "\n"}},
		},
		{
			name: "file hash",
			validator: fmt.Sprintf(`      - type: fileHash
        path: "outputs/report.txt"
        sha256: "%x"
`, reportHash),
			broker: outputWritingBroker{files: map[string]string{"outputs/report.txt": "hash me\n"}},
		},
		{
			name: "visual evidence",
			validator: `      - type: visualEvidence
        required: true
`,
			broker: outputWritingBroker{visualEvidence: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
`+tt.validator)
			var stdout, stderr bytes.Buffer
			runtime := defaultRunRuntime()
			runtime.Broker = tt.broker

			code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
			if code != 0 {
				t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
			bundle := readEvidenceBundle(t, evidence)
			if bundle.Status != "passed" {
				t.Fatalf("evidence status = %q, want passed", bundle.Status)
			}
			if len(bundle.Validators) != 1 || bundle.Validators[0].Status != "passed" {
				t.Fatalf("validator metadata = %+v", bundle.Validators)
			}
		})
	}
}

func TestRunScenarioRequiredOutputValidatorReportsMissingPath(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
      - type: outputExists
        path: "outputs/missing.txt"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Status != "failed" {
		t.Fatalf("evidence status = %q, want failed", bundle.Status)
	}
	if len(bundle.Validators) != 1 || bundle.Validators[0].Status != "failed" || !strings.Contains(bundle.Validators[0].Message, "missing") {
		t.Fatalf("validator metadata = %+v", bundle.Validators)
	}
}

func TestRunScenarioValidatorsRejectSymlinkedOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.json")
	mustWriteFile(t, outside, `{"token":"outside"}`+"\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
      - type: jsonEquals
        path: "outputs/secret.json"
        jsonPath: "$.token"
        equals: outside
`)
	var stdout, stderr bytes.Buffer
	runtimeConfig := defaultRunRuntime()
	runtimeConfig.Broker = symlinkOutputBroker{linkPath: "outputs/secret.json", target: outside}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtimeConfig)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.Validators) != 1 || bundle.Validators[0].Status != "failed" || !strings.Contains(bundle.Validators[0].Message, "symlink") {
		t.Fatalf("validator metadata = %+v", bundle.Validators)
	}
	if _, err := os.Stat(filepath.Join(evidence, "outputs", "secret.json")); !os.IsNotExist(err) {
		t.Fatalf("symlinked output was copied into Evidence Bundle, stat err = %v", err)
	}
}

func TestRunScenarioSkipsOutputPathThatCollidesWithEvidenceMetadata(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "scenario-result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if len(bundle.Outputs) != 0 {
		t.Fatalf("metadata-colliding Scenario Result was recorded as output: %+v", bundle.Outputs)
	}
	if _, err := os.Stat(filepath.Join(evidence, "scenario-result.json")); err != nil {
		t.Fatalf("Scenario Result metadata missing: %v", err)
	}
}

func TestRunScenarioInvalidValidatorPathFailsBeforeEvidence(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      scenarioResult: "outputs/result.json"
    validators:
      - type: outputExists
        path: "../secret.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `repo path "../secret.json" must be repo-relative`) {
		t.Fatalf("run error did not report invalid Validator path: %s", stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", runID))
	if bundle.Failure == nil || bundle.Failure.FailedLayer != "scenario" {
		t.Fatalf("invalid Validator config minimal evidence = %+v", bundle.Failure)
	}
}

func TestRunScenarioRequiresKnownScenario(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "missing"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "unknown Scenario") {
		t.Fatalf("missing scenario error = %q", stderr.String())
	}
}

func TestRunScenarioValidatesScenarioResultBeforeRunState(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      scripts: []
    expectedOutputs:
      scenarioResult: "../result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "repo-relative") {
		t.Fatalf("scenario result validation error = %q", stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("run state missing after invalid scenario result: %v", err)
	}
	bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", runID))
	if bundle.Failure == nil || bundle.Failure.FailedLayer != "scenario" {
		t.Fatalf("invalid Scenario Result path minimal evidence = %+v", bundle.Failure)
	}
}

func TestRunScenarioRejectsReservedPayloadPath(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall", "state", "previous.txt"), "state should not stage\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      scripts:
        - ".maya-stall"
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "reserved for Maya Stall run state") {
		t.Fatalf("reserved path error = %q", stderr.String())
	}
}

func TestPayloadPathValidationRejectsReservedPathCaseVariants(t *testing.T) {
	for _, path := range []string{".MAYA-STALL", ".Maya-Stall/state", "artifacts", "Artifacts/Maya-Stall", "ARTIFACTS/MAYA-STALL/run"} {
		t.Run(path, func(t *testing.T) {
			_, err := cleanRepoRelativePath(path)
			if err == nil {
				t.Fatalf("cleanRepoRelativePath(%q) returned nil error", path)
			}
			if !strings.Contains(err.Error(), "reserved for Maya Stall run state") {
				t.Fatalf("reserved path error = %q", err.Error())
			}
		})
	}
}

func TestPayloadPathValidationRejectsBackslashesBeforeStaging(t *testing.T) {
	_, err := cleanRepoRelativePath(`maya\..\..\evil.py`)
	if err == nil {
		t.Fatal("cleanRepoRelativePath returned nil error for backslash path")
	}
	if !strings.Contains(err.Error(), "forward slashes") {
		t.Fatalf("backslash path error = %q", err.Error())
	}
}

func TestRunScenarioRejectsSymlinkedOutputParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, ".maya-stall")); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      scripts: []
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "must not be a symlink") {
		t.Fatalf("symlinked output parent error = %q", stderr.String())
	}
}

func TestRunScenarioRejectsSymlinkPayload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	outside := filepath.Join(t.TempDir(), "outside.py")
	mustWriteFile(t, outside, "print('outside')\n")
	linkPath := filepath.Join(dir, "maya", "leaks.py")
	if err := os.Symlink(outside, linkPath); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "maya/smoke.py", "maya/leaks.py", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write symlink config fixture: %v", err)
	}
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Fatalf("symlink path error = %q", stderr.String())
	}
}

func TestRunScenarioRejectsSymlinkPayloadAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := t.TempDir()
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "leaks.py"), "print('outside')\n")
	if err := os.Symlink(outside, filepath.Join(dir, "maya")); err != nil {
		t.Skipf("create symlink fixture: %v", err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      scripts:
        - "maya/leaks.py"
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "symlink") {
		t.Fatalf("symlink ancestor error = %q", stderr.String())
	}
}

func TestRunScenarioRejectsSpecialPayloadFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO fixtures are not available on Windows")
	}
	dir := writeRunConfigFixture(t)
	fifoPath := filepath.Join(dir, "maya", "fifo.py")
	if err := exec.Command("mkfifo", fifoPath).Run(); err != nil {
		t.Skipf("create FIFO fixture: %v", err)
	}
	configPath := filepath.Join(dir, ".maya-stall.yaml")
	contentBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config fixture: %v", err)
	}
	content := strings.Replace(string(contentBytes), "maya/smoke.py", "maya/fifo.py", 1)
	if err := os.WriteFile(configPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write FIFO config fixture: %v", err)
	}
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "regular file") {
		t.Fatalf("special payload error = %q", stderr.String())
	}
}

func writeRunConfigFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, "maya", "smoke.py"), "print('smoke')\n")
	mustWriteFile(t, filepath.Join(dir, "scenes", "start.ma"), "// fake maya scene\n")
	mustWriteFile(t, filepath.Join(dir, "build", "demo.mll"), "fake plugin artifact\n")
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    description: "Fake smoke Scenario."
    mayaVersion: "2025"
    payload:
      scripts:
        - "maya/smoke.py"
      scenes:
        - "scenes/start.ma"
      pluginArtifacts:
        - "build/demo.mll"
    expectedOutputs:
      files: []
      scenarioResult: "outputs/smoke-result.json"
    evidence:
      screenshots:
        enabled: false
`)
	return dir
}

func outputValue(t *testing.T, output string, key string) string {
	t.Helper()
	prefix := key + ": "
	for _, line := range strings.Split(output, "\n") {
		value, ok := strings.CutPrefix(line, prefix)
		if ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("output missing %q line:\n%s", key, output)
	return ""
}

func writeSingleHealthyHostConfig(t *testing.T, dir string) string {
	t.Helper()
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        health: healthy
`)
	return hostConfigPath
}

func writeSingleHealthyHostConfigWithWorkRoot(t *testing.T, dir string, workRoot string) string {
	t.Helper()
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        health: healthy
        workRoot: `+strconv.Quote(workRoot)+`
`)
	return hostConfigPath
}

func writeLayeredHostConfig(t *testing.T, dir string) string {
	t.Helper()
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        ssh: ok
        workRoot: writable
        broker: ok
        mayaVersions: ["2025"]
        fakeInstalledMayaVersions: ["2025"]
        visualEvidence: true
`)
	return hostConfigPath
}

func writeRealSSHDoctorHostConfig(t *testing.T, dir string, sshPath string, sftpPath string, mayaVersions string) string {
	t.Helper()
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+`
          sftpBinary: `+strconv.Quote(sftpPath)+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`+mayaVersions+`        visualEvidence: true
`)
	return hostConfigPath
}

type blockingBroker struct {
	fakeSessionLifecycle
	started chan struct{}
	release chan struct{}
	result  ScenarioResult
}

type runResult struct {
	code   int
	stdout string
	stderr string
}

func newBlockingBroker(result ScenarioResult) *blockingBroker {
	return &blockingBroker{
		started: make(chan struct{}),
		release: make(chan struct{}),
		result:  result,
	}
}

func (broker *blockingBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	close(broker.started)
	<-broker.release
	if err := os.WriteFile(context.LogPath, []byte("blocking fake Session Broker completed\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", broker.result.Status); err != nil {
		return ScenarioResult{}, err
	}
	return broker.result, nil
}

type lockCorruptingBroker struct{ fakeSessionLifecycle }

func (lockCorruptingBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	lockPath := filepath.Join(context.RepoDir, ".maya-stall", "state", "locks", "hosts", "alpha.lock")
	if err := os.Remove(lockPath); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.Mkdir(lockPath, 0o755); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(filepath.Join(lockPath, "child"), []byte("blocks remove\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("corrupted lock path\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: "passed", Summary: "lock release should fail"}, nil
}

type lockCorruptingVisualBroker struct{ fakeSessionLifecycle }

func (lockCorruptingVisualBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	return ScenarioResult{Status: resultStatusPassed}, nil
}

func (lockCorruptingVisualBroker) CaptureScreenshot(context runContext, request screenshotRequest) (visualEvidenceArtifact, error) {
	lockPath := filepath.Join(context.RepoDir, ".maya-stall", "state", "locks", "hosts", "fake-local.lock")
	if err := os.Remove(lockPath); err != nil {
		return visualEvidenceArtifact{}, err
	}
	if err := os.Mkdir(lockPath, 0o755); err != nil {
		return visualEvidenceArtifact{}, err
	}
	if err := os.WriteFile(filepath.Join(lockPath, "child"), []byte("blocks remove\n"), 0o644); err != nil {
		return visualEvidenceArtifact{}, err
	}
	return fakeSessionBroker{}.CaptureScreenshot(context, request)
}

type outputWritingBroker struct {
	fakeSessionLifecycle
	files          map[string]string
	visualEvidence bool
}

type environmentCheckingBroker struct {
	fakeSessionLifecycle
	runID       string
	resultPath  string
	environment map[string]string
}

func (broker *environmentCheckingBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	broker.runID = filepath.Base(context.StateDir)
	broker.resultPath = context.ScenarioResultPath
	broker.environment = context.Environment
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("checked run environment\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", resultStatusPassed); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusPassed, Summary: "checked run environment"}, nil
}

type scenarioResultFileBroker struct{ fakeSessionLifecycle }

func (scenarioResultFileBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("script wrote scenario result\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	result := map[string]any{
		"status":      resultStatusFailed,
		"summary":     "script wrote result",
		"largeNumber": json.Number("9007199254740993"),
		"assertions": []map[string]any{
			{"name": "plugin loaded", "passed": false},
		},
	}
	if err := writeJSONFile(context.ScenarioResultPath, result); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", resultStatusFailed); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusPassed, Summary: "broker fallback"}, nil
}

type partialScenarioResultFileBroker struct{ fakeSessionLifecycle }

func (partialScenarioResultFileBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("script wrote partial scenario result\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := writeJSONFile(context.ScenarioResultPath, map[string]any{"summary": "script partial result"}); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", resultStatusFailed); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusFailed, Summary: "broker failure"}, nil
}

type trailingScenarioResultBroker struct{ fakeSessionLifecycle }

func (trailingScenarioResultBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("script wrote malformed scenario result\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	content := []byte(`{"status":"passed"}{"status":"failed"}`)
	if err := os.MkdirAll(filepath.Dir(context.ScenarioResultPath), 0o755); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.ScenarioResultPath, content, 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", resultStatusPassed); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusPassed, Summary: "broker fallback"}, nil
}

func (broker outputWritingBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("output-writing fake Session Broker completed\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	for relativePath, content := range broker.files {
		path := filepath.Join(context.Workspace, relativePath)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return ScenarioResult{}, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return ScenarioResult{}, err
		}
	}
	if broker.visualEvidence {
		path := filepath.Join(context.EvidenceDir, "screenshots", "smoke.png")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return ScenarioResult{}, err
		}
		if err := os.WriteFile(path, []byte("fake png\n"), 0o644); err != nil {
			return ScenarioResult{}, err
		}
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", resultStatusPassed); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusPassed, Summary: "outputs written"}, nil
}

type symlinkOutputBroker struct {
	fakeSessionLifecycle
	linkPath string
	target   string
}

func (broker symlinkOutputBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("symlink fake Session Broker completed\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	linkPath := filepath.Join(context.Workspace, broker.linkPath)
	if err := os.MkdirAll(filepath.Dir(linkPath), 0o755); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.Symlink(broker.target, linkPath); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", resultStatusPassed); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusPassed, Summary: "symlink output written"}, nil
}

func mustWriteFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write fixture file: %v", err)
	}
}

func requireHostHealthLayer(t *testing.T, report hostHealthReport, id string) hostHealthLayer {
	t.Helper()
	for _, layer := range report.Layers {
		if layer.ID == id {
			return layer
		}
	}
	t.Fatalf("Host Health missing layer %q: %+v", id, report.Layers)
	return hostHealthLayer{}
}

func writeFakeCommand(t *testing.T, dir string, name string, logPath string, exitCode int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	content := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$0 $*\" >> %s\nexit %d\n", shellQuote(logPath), exitCode)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake command: %v", err)
	}
	return path
}

func freshRunAwareFakeSSHPrelude(dir string, prefix string, freshRunFailure string, inheritedStopFailure string, defaultReadiness bool) string {
	lifecyclePath := filepath.Join(dir, prefix+".lifecycle")
	decodedCommandPath := filepath.Join(dir, prefix+".command")
	seenFreshRunPath := filepath.Join(dir, prefix+".fresh-seen")
	return fmt.Sprintf(`#!/bin/sh
encoded=""
for argument in "$@"; do
  encoded="$argument"
done
if ! printf '%%s' "$encoded" | base64 --decode > %[1]s 2>/dev/null; then
  printf '%%s' "$encoded" | base64 -D > %[1]s 2>/dev/null || true
fi
decoded=$(iconv -f UTF-16LE -t UTF-8 %[1]s 2>/dev/null || true)
default_readiness=%[6]s
if [ "$default_readiness" = "1" ] && printf '%%s' "$decoded" | grep -q "maya-stall-prerun-probe-ok"; then
  printf '%%s\n' 'maya-stall-prerun-probe-ok'
  exit 0
fi
if [ "$default_readiness" = "1" ] && printf '%%s' "$decoded" | grep -q "maya-stall-prerun-session-broker-probe"; then
  printf '%%s\n' '{"state_dir":"C:/maya-stall/sessiond-ui","has_state":true,"derived_status":"stopped","state":{"status":"stopped","session_id":"session-previous"}}'
  exit 0
fi
if [ "$default_readiness" = "1" ] && printf '%%s' "$decoded" | grep -q "'call'" && printf '%%s' "$decoded" | grep -q "scene.info"; then
  printf '%%s\n' '{"ok":true,"tool":"scene.info"}'
  exit 0
fi
if [ "$default_readiness" = "1" ] && printf '%%s' "$decoded" | grep -q "viewport.capture" && printf '%%s' "$decoded" | grep -q "width=64" && printf '%%s' "$decoded" | grep -q "height=64"; then
  printf '%%s\n' '{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"AA==","mimeType":"image/jpeg"}]}'
  exit 0
fi
if [ ! -f %[2]s ] && printf '%%s' "$decoded" | grep -q "'stop'" && ! printf '%%s' "$decoded" | grep -q "Get-ScheduledTask"; then
  inherited_stop_failure=%[3]s
  if [ -n "$inherited_stop_failure" ]; then
    printf '%%s\n' '{"ok":false,"error":"inherited session stop failed"}'
    exit 0
  fi
  printf '%%s\n' '{"ok":true,"status":"stopped"}'
  exit 0
fi
if printf '%%s' "$decoded" | grep -q "fresh-run"; then
  fresh_failure=%[4]s
  if [ -n "$fresh_failure" ]; then
    printf '%%s\n' "$fresh_failure" >&2
    exit 1
  fi
  printf 'seen\n' > %[2]s
  printf '1\n' > %[5]s
  printf '%%s\n' '{"ok":true,"task":"MayaStallSessiondUI","reason":"fresh-run"}'
  exit 0
fi
if [ -f %[5]s ] && printf '%%s' "$decoded" | grep -q "'status'"; then
  rm -f %[5]s
  printf '%%s\n' '{"state_dir":"C:/maya-stall/sessiond-ui","has_state":true,"derived_status":"running","state":{"status":"running","session_id":"session-fresh","call_server_ready":true}}'
  exit 0
fi
`, shellQuote(decodedCommandPath), shellQuote(seenFreshRunPath), shellQuote(inheritedStopFailure), shellQuote(freshRunFailure), shellQuote(lifecyclePath), map[bool]string{true: "1", false: "0"}[defaultReadiness])
}

func writeSequencedFakeSSHCommand(t *testing.T, dir string, logPath string, outputs []string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ssh-sequenced")
	countPath := filepath.Join(dir, "fake-ssh-sequenced.count")
	var cases strings.Builder
	freshRunFailure := ""
	inheritedStopFailure := ""
	defaultReadiness := true
	callIndex := 0
	for _, output := range outputs {
		if output == "@prerun-explicit" {
			defaultReadiness = false
			continue
		}
		callIndex++
		if output == "" {
			continue
		}
		if message, ok := strings.CutPrefix(output, "@fresh-fail:"); ok {
			freshRunFailure = message
			continue
		}
		if message, ok := strings.CutPrefix(output, "@inherited-stop-fail:"); ok {
			inheritedStopFailure = message
			continue
		}
		if path, ok := strings.CutPrefix(output, "@file:"); ok {
			fmt.Fprintf(&cases, "%d) /bin/cat %s\n;;\n", callIndex, shellQuote(path))
			continue
		}
		if message, ok := strings.CutPrefix(output, "@fail:"); ok {
			fmt.Fprintf(&cases, "%d) printf '%%s\\n' %s >&2; exit 1\n;;\n", callIndex, shellQuote(message))
			continue
		}
		if message, ok := strings.CutPrefix(output, "@stdout-fail:"); ok {
			fmt.Fprintf(&cases, "%d) printf '%%s\\n' %s; exit 1\n;;\n", callIndex, shellQuote(message))
			continue
		}
		fmt.Fprintf(&cases, "%d) cat <<'JSON'\n%s\nJSON\n;;\n", callIndex, output)
	}
	content := freshRunAwareFakeSSHPrelude(dir, "fake-ssh-sequenced", freshRunFailure, inheritedStopFailure, defaultReadiness) + fmt.Sprintf(`count=0
if [ -f %[1]s ]; then
  count=$(cat %[1]s)
fi
count=$((count + 1))
printf '%%s\n' "$count" > %[1]s
printf 'CALL %%s %%s\n' "$count" "$*" >> %[2]s
case "$count" in
%[3]s
esac
exit 0
`, shellQuote(countPath), shellQuote(logPath), cases.String())
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write sequenced fake ssh command: %v", err)
	}
	return path
}

func writeMissingTrustedPluginDestinationFakeSSHCommand(t *testing.T, dir string, logPath string, missingDestination string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ssh-missing-trusted-plugin")
	countPath := filepath.Join(dir, "fake-ssh-missing-trusted-plugin.count")
	expectedScript := "$ErrorActionPreference = 'Stop'\n" +
		"foreach ($path in @(" + powerShellSingleQuoted(missingDestination) + ")) {\n" +
		"  if (Test-Path -LiteralPath $path) {\n" +
		"    Remove-Item -LiteralPath $path -Recurse -Force -ErrorAction Stop\n" +
		"  }\n" +
		"}\n" +
		"exit 0\n"
	expectedCommand := strings.Join(encodedPowerShellCommand(expectedScript), " ")
	content := freshRunAwareFakeSSHPrelude(dir, "fake-ssh-missing-trusted-plugin", "", "", true) + fmt.Sprintf(`count=0
if [ -f %[1]s ]; then
  count=$(cat %[1]s)
fi
count=$((count + 1))
printf '%%s\n' "$count" > %[1]s
printf 'CALL %%s %%s\n' "$count" "$*" >> %[2]s
case "$count" in
1)
	cat <<'JSON'
%[5]s
JSON
	;;
2)
	cat <<'JSON'
%[4]s
JSON
	;;
3)
  expected=%[3]s
  case "$*" in
    *"$expected"*)
      printf '%%s\n' '{"ok":true,"tool":"trusted-plugin-cleanup"}'
      ;;
    *)
      printf 'missing trusted Plugin Artifact destination should be a successful prepare\n' >&2
      exit 1
      ;;
  esac
  ;;
4)
  cat <<'JSON'
{"ok":true,"tool":"script.execute"}
JSON
  ;;
5)
  cat <<'JSON'
{"has_state":true,"derived_status":"running","state":{"status":"running","session_id":"session-fresh","call_server_ready":true}}
JSON
  ;;
esac
exit 0
`, shellQuote(countPath), shellQuote(logPath), shellQuote(expectedCommand), sessiondStatusFixture("session-alpha"), trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts/build";
`))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write missing trusted Plugin Artifact destination fake ssh command: %v", err)
	}
	return path
}

func writeFailingTrustedPluginDestinationRemovalFakeSSHCommand(t *testing.T, dir string, logPath string, destination string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-ssh-failing-trusted-plugin-removal")
	countPath := filepath.Join(dir, "fake-ssh-failing-trusted-plugin-removal.count")
	expectedScript := "$ErrorActionPreference = 'Stop'\n" +
		"foreach ($path in @(" + powerShellSingleQuoted(destination) + ")) {\n" +
		"  if (Test-Path -LiteralPath $path) {\n" +
		"    Remove-Item -LiteralPath $path -Recurse -Force -ErrorAction Stop\n" +
		"  }\n" +
		"}\n" +
		"exit 0\n"
	expectedCommand := strings.Join(encodedPowerShellCommand(expectedScript), " ")
	content := freshRunAwareFakeSSHPrelude(dir, "fake-ssh-failing-trusted-plugin-removal", "", "", true) + fmt.Sprintf(`count=0
if [ -f %[1]s ]; then
  count=$(cat %[1]s)
fi
count=$((count + 1))
printf '%%s\n' "$count" > %[1]s
printf 'CALL %%s %%s\n' "$count" "$*" >> %[2]s
case "$count" in
1)
	cat <<'JSON'
%[5]s
JSON
	;;
2)
	cat <<'JSON'
%[4]s
JSON
	;;
3)
    expected=%[3]s
    case "$*" in
      *"$expected"*)
    printf 'access denied removing trusted Plugin Artifact\n' >&2
    exit 1
        ;;
      *)
        printf 'cleanup command did not fail closed on existing destination removal\n' >&2
        exit 1
        ;;
    esac
    ;;
*)
    printf 'unexpected SSH call before trusted Plugin Artifact removal failure\n' >&2
    exit 1
    ;;
esac
`, shellQuote(countPath), shellQuote(logPath), shellQuote(expectedCommand), sessiondStatusFixture("session-alpha"), trustedPluginPrefsProbeOutput(`// Security
optionVar -cat "Security"
 -sa "SafeModeAllowedlistPaths"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts"
 -sva "SafeModeAllowedlistPaths" "C:/maya-stall/trusted-plugin-artifacts/build";
`))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write failing trusted Plugin Artifact removal fake ssh command: %v", err)
	}
	return path
}

func trustedPluginPrefsProbeOutput(content string) string {
	return trustedPluginPrefsProbeOutputChanged(content, false)
}

func trustedPluginPrefsProbeOutputChanged(content string, changed bool) string {
	encoded, err := json.Marshal(trustedPluginPrefsProbe{Exists: true, Content: content, Changed: changed})
	if err != nil {
		panic(err)
	}
	return trustedPluginPrefsJSONPrefix + string(encoded)
}

func writeTrustedPluginHostConfig(t *testing.T, dir string, sshPath string, sftpPath string) string {
	t.Helper()
	sftpConfig := ""
	if sftpPath != "" {
		sftpConfig = "\n          sftpBinary: " + strconv.Quote(sftpPath)
	}
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+strconv.Quote(sshPath)+sftpConfig+`
        workRoot: C:/maya-stall
        trustedPluginArtifactsRoot: C:/maya-stall/trusted-plugin-artifacts
        mayaVersions: ["2025"]
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	return hostConfigPath
}

func writeFakeSSHBinaryOutput(t *testing.T, dir string, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("write fake SSH binary output: %v", err)
	}
	return "@file:" + path
}

func useFakeFFmpegLookPath(t *testing.T, dir string) {
	t.Helper()
	ffmpeg := writeFakeFFmpeg(t, dir)
	previousLookPath := lookPath
	lookPath = func(name string) (string, error) {
		if name == "ffmpeg" {
			return ffmpeg, nil
		}
		return previousLookPath(name)
	}
	t.Cleanup(func() {
		lookPath = previousLookPath
	})
}

func sessiondStatusFixture(sessionID string) string {
	return `{"has_state":true,"derived_status":"running","state":{"status":"running","session_id":` + strconv.Quote(sessionID) + `,"daemon_pid":101,"maya_pid":202,"mcp_pid":303,"maya_alive":true,"mcp_alive":true,"call_server_ready":true,"daemon_log":"C:/maya-stall/sessiond-ui/daemon.log","heartbeat_at":"2026-07-06T08:00:00Z"},"process_alive":{"daemon":true,"maya":true,"mcp":true}}`
}

func writeFakeSFTPCommand(t *testing.T, dir string, logPath string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-sftp")
	content := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  printf '%%s\n' "$line" >> %s
  case "$line" in
    *get*)
      local_path=${line##*\" \"}
      local_path=${local_path%%\"}
      local_path=${local_path##\"}
      mkdir -p "$(dirname "$local_path")"
      printf '{"status":"passed","summary":"downloaded by fake sftp"}\n' > "$local_path"
      ;;
  esac
done
exit 0
`, shellQuote(logPath))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write fake sftp command: %v", err)
	}
	return path
}

func writeScenarioResultSFTPCommand(t *testing.T, dir string, logPath string, status string, summary string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-sftp-scenario-result")
	result := fmt.Sprintf("{\"status\":%q,\"summary\":%q}\n", status, summary)
	content := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  printf '%%s\n' "$line" >> %s
  case "$line" in
    *get*)
      local_path=${line##*\" \"}
      local_path=${local_path%%\"}
      local_path=${local_path##\"}
      mkdir -p "$(dirname "$local_path")"
      printf %%s %s > "$local_path"
      ;;
  esac
done
exit 0
`, shellQuote(logPath), shellQuote(result))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write scenario result fake sftp command: %v", err)
	}
	return path
}

func writeScenarioOutputSFTPCommand(t *testing.T, dir string, logPath string, outputs map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-sftp-scenario-outputs")
	var cases strings.Builder
	keys := make([]string, 0, len(outputs))
	for key := range outputs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(&cases, "        *%s) printf %%s %s > \"$local_path\" ;;\n", key, shellQuote(outputs[key]))
	}
	content := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  printf '%%s\n' "$line" >> %s
  case "$line" in
    *get*)
      local_path=${line##*\" \"}
      local_path=${local_path%%\"}
      local_path=${local_path##\"}
      mkdir -p "$(dirname "$local_path")"
      case "$local_path" in
%s        *) printf '{"status":"passed","summary":"downloaded by fake sftp"}\n' > "$local_path" ;;
      esac
      ;;
  esac
done
exit 0
`, shellQuote(logPath), cases.String())
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write scenario output fake sftp command: %v", err)
	}
	return path
}

func writeMissingScenarioResultSFTPCommand(t *testing.T, dir string, logPath string, outputPath string, outputContent string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-sftp-missing-scenario-result")
	content := fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  printf '%%s\n' "$line" >> %s
  case "$line" in
    *get*)
      local_path=${line##*\" \"}
      local_path=${local_path%%\"}
      local_path=${local_path##\"}
      mkdir -p "$(dirname "$local_path")"
      case "$line" in
        *%s*) printf %%s %s > "$local_path" ;;
        get*) printf 'missing scenario result\n' >&2; exit 1 ;;
      esac
      ;;
  esac
done
exit 0
`, shellQuote(logPath), outputPath, shellQuote(outputContent))
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write missing Scenario Result fake sftp command: %v", err)
	}
	return path
}

func writeFailingGetSFTPCommand(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-sftp-failing-get")
	content := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    get*) exit 1 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write failing fake sftp command: %v", err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func mustRunWorkspace(t *testing.T, repoDir string, runID string, remoteWorkRoot string, scenarioResult string) runWorkspace {
	t.Helper()
	workspace, err := newRunWorkspace(repoDir, runID, remoteWorkRoot, scenarioResult)
	if err != nil {
		t.Fatalf("create Run Workspace: %v", err)
	}
	return workspace
}

func onlyRunDir(t *testing.T, parent string) string {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read run dir parent %s: %v", parent, err)
	}
	if len(entries) != 1 {
		t.Fatalf("run dir count in %s = %d, want 1", parent, len(entries))
	}
	if !entries[0].IsDir() {
		t.Fatalf("run entry %s is not a directory", entries[0].Name())
	}
	return filepath.Join(parent, entries[0].Name())
}

func readEvidenceBundle(t *testing.T, evidenceDir string) evidenceBundle {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(evidenceDir, "evidence.json"))
	if err != nil {
		t.Fatalf("read evidence bundle: %v", err)
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		t.Fatalf("parse evidence bundle: %v", err)
	}
	return bundle
}

func publishedManifestHasURL(manifest publishedArtifactManifest, label string, url string) bool {
	for _, artifact := range manifest.Artifacts {
		if artifact.Label == label && artifact.URL == url {
			return true
		}
	}
	return false
}

func runIDFromOutput(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		value, ok := strings.CutPrefix(line, "run: ")
		if ok {
			return strings.TrimSpace(value)
		}
	}
	t.Fatalf("run output missing run id:\n%s", output)
	return ""
}
