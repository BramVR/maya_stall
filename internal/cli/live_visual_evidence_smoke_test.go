package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOptInRealVisualEvidenceSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	dir := writeLiveRunConfigFixture(t)
	restoreLiveSessionBrokerFixtures(t, options)

	doctorOptions := options.doctorOptions()
	doctorOptions.ScenarioName = "smoke"
	report := runDoctor(dir, doctorOptions)
	assertLiveVisualEvidenceHostProof(t, report)
	t.Logf("Host Health: %s", formatHostHealthReport(report))

	recordEvidenceDir := captureLiveRecordCommandProof(t, dir, options)
	recordBundle := assertLiveRecordCommandProofBundle(t, recordEvidenceDir, options)
	recording := requireLiveRecordCommandArtifact(t, recordEvidenceDir, recordBundle)
	t.Logf("Live record command proof: run=%s recording=%s bytes=%d",
		recordBundle.RunID,
		recording.Path,
		artifactSize(t, recordEvidenceDir, recording),
	)
	evidenceDir := recordEvidenceDir
	addLiveDesktopScreenshotForProofArtifact(t, dir, evidenceDir, options)
	bundle := assertLiveVisualEvidenceProofBundle(t, evidenceDir)
	screenshot, recording := requireLiveDesktopVisualArtifacts(t, evidenceDir, bundle)
	t.Logf("Live Visual Evidence proof artifact source: run=%s screenshot=%s bytes=%d recording=%s bytes=%d",
		bundle.RunID,
		screenshot.Path,
		artifactSize(t, evidenceDir, screenshot),
		recording.Path,
		artifactSize(t, evidenceDir, recording),
	)
	publishOptionalLiveVisualEvidenceProofArtifact(t, evidenceDir)
}

func TestOptInRealDesktopControlModalSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	dir := writeLiveRunConfigFixture(t)
	restoreLiveSessionBrokerFixtures(t, options)

	hostID := selectLiveDesktopControlSmokeHost(t, dir, options)
	options.Host = hostID
	host := liveSmokeHostConfigByID(t, options, hostID)
	processes, err := mayaTasklistSessions(host)
	if err != nil {
		t.Fatalf("query maya.exe tasklist sessions: %v", err)
	}
	if err := requireConsoleMayaProcess(processes); err != nil {
		t.Fatal(err)
	}

	fixture := launchLiveDesktopControlModalFixture(t, host)
	defer func() {
		cleanupLiveDesktopControlModalFixture(t, host, fixture)
	}()

	evidenceDir := captureLiveScreenshotCommandProof(t, dir, options)
	bundle := assertLiveDesktopControlScreenshotProofBundle(t, evidenceDir, options)
	screenshot := requireLiveDesktopControlScreenshotArtifact(t, evidenceDir, bundle)
	assertLiveDesktopControlScreenshotShowsModal(t, evidenceDir, screenshot)

	var stdout, stderr bytes.Buffer
	code := Run(options.controlClickArgs(fixture.ClickX, fixture.ClickY), &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("live desktop control click exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "dryRun: false") {
		t.Fatalf("control click output missing execution detail:\n%s", stdout.String())
	}
	waitForLiveDesktopControlModalClosed(t, host, fixture)
	cleanupLiveDesktopControlModalFixture(t, host, fixture)
	fixture.RemoteRoot = ""
	waitForLiveSessionBrokerCallReady(t, host)
	t.Logf("Live desktop control modal proof: screenshotRun=%s screenshot=%s bytes=%d click=(%d,%d)",
		bundle.RunID,
		screenshot.Path,
		artifactSize(t, evidenceDir, screenshot),
		fixture.ClickX,
		fixture.ClickY,
	)
}

func TestOptInRealRunScopedDesktopOpsSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	dir := writeLiveRunConfigFixture(t)
	restoreLiveSessionBrokerFixtures(t, options)

	hostID := selectLiveDesktopControlSmokeHost(t, dir, options)
	options.Host = hostID
	host := liveSmokeHostConfigByID(t, options, hostID)
	processes, err := mayaTasklistSessions(host)
	if err != nil {
		t.Fatalf("query maya.exe tasklist sessions: %v", err)
	}
	if err := requireConsoleMayaProcess(processes); err != nil {
		t.Fatal(err)
	}

	var keptStdout, keptStderr bytes.Buffer
	keptCode := Run(options.runArgs("retention-failure", "--keep-on-failure"), &keptStdout, &keptStderr, dir, "test-version")
	if keptCode != 1 {
		t.Fatalf("live run-scoped retained run exit code = %d, want 1; stdout: %s stderr: %s", keptCode, keptStdout.String(), keptStderr.String())
	}
	runID := smokeOutputValue(keptStdout.String(), "run")
	if runID == "" || !strings.Contains(keptStdout.String(), "stopPolicy: kept") {
		t.Fatalf("live run-scoped proof did not keep failed run:\nstdout: %s\nstderr: %s", keptStdout.String(), keptStderr.String())
	}
	defer func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
		if code != 0 && !strings.Contains(stderr.String(), "not found") {
			t.Fatalf("cleanup retained run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
		}
		restoreLiveSessionBrokerFixture(t, host)
	}()

	var standaloneStdout, standaloneStderr bytes.Buffer
	standaloneCode := Run(options.screenshotArgs(), &standaloneStdout, &standaloneStderr, dir, "test-version")
	if standaloneCode != 1 || !strings.Contains(standaloneStderr.String(), "no healthy unlocked Maya Host") {
		t.Fatalf("standalone screenshot while Host Lock held exit code = %d, want fail-closed locked host; stdout: %s stderr: %s", standaloneCode, standaloneStdout.String(), standaloneStderr.String())
	}

	fixture := launchLiveDesktopControlModalFixture(t, host)
	defer func() {
		cleanupLiveDesktopControlModalFixture(t, host, fixture)
	}()

	var screenshotStdout, screenshotStderr bytes.Buffer
	screenshotCode := Run([]string{"attach", runID, "screenshot"}, &screenshotStdout, &screenshotStderr, dir, "test-version")
	if screenshotCode != 0 {
		t.Fatalf("live run-scoped screenshot exit code = %d, want 0; stdout: %s stderr: %s", screenshotCode, screenshotStdout.String(), screenshotStderr.String())
	}
	evidenceDir := smokeOutputValue(screenshotStdout.String(), "evidence")
	if evidenceDir == "" {
		t.Fatalf("live run-scoped screenshot did not print Evidence Bundle path:\n%s", screenshotStdout.String())
	}
	bundle := readEvidenceBundle(t, evidenceDir)
	screenshot := requireLiveRunScopedScreenshotArtifact(t, evidenceDir, bundle)
	assertLiveDesktopControlScreenshotShowsModal(t, evidenceDir, screenshot)

	var controlStdout, controlStderr bytes.Buffer
	controlCode := Run([]string{"attach", runID, "control", "click", "--x", strconv.Itoa(fixture.ClickX), "--y", strconv.Itoa(fixture.ClickY)}, &controlStdout, &controlStderr, dir, "test-version")
	if controlCode != 0 {
		t.Fatalf("live run-scoped control click exit code = %d, want 0; stdout: %s stderr: %s", controlCode, controlStdout.String(), controlStderr.String())
	}
	if !strings.Contains(controlStdout.String(), "run: "+runID) || !strings.Contains(controlStdout.String(), "dryRun: false") {
		t.Fatalf("live run-scoped control output missing execution detail:\n%s", controlStdout.String())
	}
	waitForLiveDesktopControlModalClosed(t, host, fixture)
	cleanupLiveDesktopControlModalFixture(t, host, fixture)
	fixture.RemoteRoot = ""
	waitForLiveSessionBrokerCallReady(t, host)
	t.Logf("Live run-scoped desktop ops proof: run=%s screenshot=%s bytes=%d click=(%d,%d)",
		runID,
		screenshot.Path,
		artifactSize(t, evidenceDir, screenshot),
		fixture.ClickX,
		fixture.ClickY,
	)
}

func TestLiveVisualEvidenceProofRejectsInvalidProofShapes(t *testing.T) {
	liveRuntime := runtimeMetadata{Profile: "ssh-sessiond", HostAdapter: "ssh", BrokerAdapter: "gg-mayasessiond", LiveProofEligible: true}
	console := []windowsProcessSession{{ProcessID: 42, SessionID: 1, SessionName: "Console", Name: "maya.exe"}}
	cases := []struct {
		name      string
		runtime   runtimeMetadata
		processes []windowsProcessSession
		visual    []visualEvidenceArtifact
		files     map[string][]byte
		wantErr   string
		wantValid bool
	}{
		{
			name:      "valid desktop screenshot and recording",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes(pngHeaderBytes())},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes(mp4HeaderBytes())},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": pngHeaderBytes(),
				"recordings/desktop-recording.mp4":   mp4HeaderBytes(),
			},
			wantValid: true,
		},
		{
			name:      "fake runtime",
			runtime:   runtimeMetadata{Profile: "fake-local", HostAdapter: "fake", BrokerAdapter: "fake"},
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png"},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4"},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": pngHeaderBytes(),
				"recordings/desktop-recording.mp4":   mp4HeaderBytes(),
			},
			wantErr: "live-proof-eligible ssh-sessiond",
		},
		{
			name:      "discovered visual artifact",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes(pngHeaderBytes())},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginDiscovered, SHA256: sha256HexOfBytes(mp4HeaderBytes())},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": pngHeaderBytes(),
				"recordings/desktop-recording.mp4":   mp4HeaderBytes(),
			},
			wantErr: `origin "discovered"`,
		},
		{
			name:      "fake broker capture origin",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png", Origin: visualEvidenceOriginFakeBrokerCapture, SHA256: sha256HexOfBytes(pngHeaderBytes())},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes(mp4HeaderBytes())},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": pngHeaderBytes(),
				"recordings/desktop-recording.mp4":   mp4HeaderBytes(),
			},
			wantErr: `origin "fake-broker-capture"`,
		},
		{
			name:      "missing provenance hash",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes(pngHeaderBytes())},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginBrokerCapture},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": pngHeaderBytes(),
				"recordings/desktop-recording.mp4":   mp4HeaderBytes(),
			},
			wantErr: "sha256 provenance hash",
		},
		{
			name:      "mismatched provenance hash",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes(pngHeaderBytes())},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes([]byte("replaced after capture"))},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": pngHeaderBytes(),
				"recordings/desktop-recording.mp4":   mp4HeaderBytes(),
			},
			wantErr: "does not match recorded provenance hash",
		},
		{
			name:      "viewport screenshot only",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/smoke.jpg", MediaType: "image/jpeg"},
			},
			files:   map[string][]byte{"screenshots/smoke.jpg": jpegHeaderBytes()},
			wantErr: "desktop screenshot and desktop recording",
		},
		{
			name:      "non console maya",
			runtime:   liveRuntime,
			processes: []windowsProcessSession{{ProcessID: 42, SessionID: 0, SessionName: "Services", Name: "maya.exe"}},
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png"},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4"},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": pngHeaderBytes(),
				"recordings/desktop-recording.mp4":   mp4HeaderBytes(),
			},
			wantErr: "interactive Console session",
		},
		{
			name:      "fake bytes",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "screenshot", Path: "screenshots/desktop-screenshot.png", MediaType: "image/png", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes([]byte("fake screenshot"))},
				{Kind: "recording", Path: "recordings/desktop-recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes([]byte("fake recording"))},
			},
			files: map[string][]byte{
				"screenshots/desktop-screenshot.png": []byte("fake screenshot"),
				"recordings/desktop-recording.mp4":   []byte("fake recording"),
			},
			wantErr: "does not look like a PNG",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			evidenceDir := writeLiveVisualEvidenceProofBundle(t, tt.runtime, tt.visual, tt.files)
			bundle, err := validateLiveVisualEvidenceProofBundle(evidenceDir, tt.processes)
			if tt.wantValid {
				if err != nil {
					t.Fatalf("validateLiveVisualEvidenceProofBundle returned error: %v", err)
				}
				if len(bundle.VisualEvidence) != 2 {
					t.Fatalf("Visual Evidence count = %d, want 2", len(bundle.VisualEvidence))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLiveVisualEvidenceProofBundle error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLiveVisualEvidenceProofWorkflowRequiresSmokePass(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci-required.yml"))
	if err != nil {
		t.Fatalf("read proof workflow: %v", err)
	}
	text := string(content)
	for _, want := range []string{
		"TestOptInRealVisualEvidenceSmoke",
		"TestOptInRealDesktopControlModalSmoke",
		"TestOptInRealSSHDoctorSmoke",
		"TestOptInRealSSHConsumingRepoSmoke",
		"TestOptInRealSSHRunSmoke",
		"TestOptInRealHostLockContentionAndRecoverySmoke",
		"TestOptInRealRunScopedDesktopOpsSmoke",
		"-count=1 -parallel=1 -timeout=20m",
		"MAYA_STALL_LIVE_PROOF_ARTIFACT_ENABLED",
		"live-visual-evidence-proof",
		"assert-public-artifact-confidentiality.mjs",
		"all eight individual live smoke tests must pass without skips",
		"failed_missing_visual_evidence_proof_artifact",
		"failed_visual_evidence_proof_confidentiality",
		"failed_visual_evidence_proof_upload",
		"failed_missing_host_config",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("proof workflow missing %q", want)
		}
	}
	if strings.Contains(text, "MAYA_STALL_LIVE_PROOF_MEDIA_REVIEWED") {
		t.Fatal("proof workflow must not self-attest public media review")
	}
	if got := strings.Count(text, "go test -json ./internal/cli -run"); got != 1 {
		t.Fatalf("live smoke Go test process count = %d, want 1", got)
	}
	sshSmoke, err := os.ReadFile("live_ssh_smoke_test.go")
	if err != nil {
		t.Fatalf("read live SSH smoke: %v", err)
	}
	if !strings.Contains(string(sshSmoke), `t.Run("shared Host Agent path", runOptInRealSharedHostAgentRunSmoke)`) {
		t.Fatal("protected live SSH smoke does not require the shared Host Agent path")
	}
	if !strings.Contains(string(sshSmoke), `t.Run("shared Kept Session deadline path", runOptInRealSharedKeptSessionDeadlineSmoke)`) {
		t.Fatal("protected live SSH smoke does not require shared Kept Session deadline proof")
	}
}

func TestRealSSHSmokeScreenshotAndControlArgs(t *testing.T) {
	options := realSSHSmokeOptions{
		HostConfig:    "/tmp/hosts.yaml",
		TargetProfile: "ci",
		Host:          "maya-win-01",
	}

	gotScreenshot := options.screenshotArgs()
	wantScreenshot := []string{"screenshot", "--host-config", "/tmp/hosts.yaml", "--target-profile", "ci", "--host", "maya-win-01"}
	if strings.Join(gotScreenshot, "\n") != strings.Join(wantScreenshot, "\n") {
		t.Fatalf("screenshot args = %#v, want %#v", gotScreenshot, wantScreenshot)
	}

	gotControl := options.controlClickArgs(123, 456)
	wantControl := []string{"control", "click", "--host-config", "/tmp/hosts.yaml", "--target-profile", "ci", "--host", "maya-win-01", "--x", "123", "--y", "456"}
	if strings.Join(gotControl, "\n") != strings.Join(wantControl, "\n") {
		t.Fatalf("control click args = %#v, want %#v", gotControl, wantControl)
	}
}

func TestRealSSHSmokeRecordArgsUseStandaloneCommand(t *testing.T) {
	options := realSSHSmokeOptions{
		HostConfig:    "/tmp/hosts.yaml",
		TargetProfile: "ci",
		Host:          "maya-win-01",
	}
	got := options.recordArgs()
	want := []string{"record", "--host-config", "/tmp/hosts.yaml", "--target-profile", "ci", "--host", "maya-win-01"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("record args = %#v, want %#v", got, want)
	}
}

func TestLiveRecordCommandProofRejectsInvalidProofShapes(t *testing.T) {
	liveRuntime := runtimeMetadata{Profile: "ssh-sessiond", HostAdapter: "ssh", BrokerAdapter: "gg-mayasessiond", LiveProofEligible: true}
	console := []windowsProcessSession{{ProcessID: 42, SessionID: 1, SessionName: "Console", Name: "maya.exe"}}
	cases := []struct {
		name      string
		runtime   runtimeMetadata
		processes []windowsProcessSession
		visual    []visualEvidenceArtifact
		files     map[string][]byte
		wantErr   string
		wantValid bool
	}{
		{
			name:      "valid standalone record command bundle",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes(mp4HeaderBytes()), DurationSeconds: defaultRecordingDuration.Seconds(), FPS: defaultRecordingFPS, TargetProfile: "ci", Host: "maya-win-01"},
			},
			files:     map[string][]byte{"recordings/recording.mp4": mp4HeaderBytes()},
			wantValid: true,
		},
		{
			name:      "discovered recording origin",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginDiscovered, SHA256: sha256HexOfBytes(mp4HeaderBytes()), DurationSeconds: defaultRecordingDuration.Seconds(), FPS: defaultRecordingFPS, TargetProfile: "ci", Host: "maya-win-01"},
			},
			files:   map[string][]byte{"recordings/recording.mp4": mp4HeaderBytes()},
			wantErr: `origin "discovered"`,
		},
		{
			name:      "missing recording metadata",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/recording.mp4", MediaType: "video/mp4", TargetProfile: "ci", Host: "maya-win-01"},
			},
			files:   map[string][]byte{"recordings/recording.mp4": mp4HeaderBytes()},
			wantErr: "duration/FPS metadata",
		},
		{
			name:      "missing selected host metadata",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/recording.mp4", MediaType: "video/mp4", DurationSeconds: defaultRecordingDuration.Seconds(), FPS: defaultRecordingFPS, TargetProfile: "ci"},
			},
			files:   map[string][]byte{"recordings/recording.mp4": mp4HeaderBytes()},
			wantErr: "selected Maya Host metadata",
		},
		{
			name:      "fake bytes",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/recording.mp4", MediaType: "video/mp4", Origin: visualEvidenceOriginBrokerCapture, SHA256: sha256HexOfBytes([]byte("fake recording")), DurationSeconds: defaultRecordingDuration.Seconds(), FPS: defaultRecordingFPS, TargetProfile: "ci", Host: "maya-win-01"},
			},
			files:   map[string][]byte{"recordings/recording.mp4": []byte("fake recording")},
			wantErr: "does not look like an MP4",
		},
		{
			name:      "fake runtime",
			runtime:   runtimeMetadata{Profile: "fake-local", HostAdapter: "fake", BrokerAdapter: "fake"},
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/recording.mp4", MediaType: "video/mp4", DurationSeconds: defaultRecordingDuration.Seconds(), FPS: defaultRecordingFPS, TargetProfile: "ci", Host: "maya-win-01"},
			},
			files:   map[string][]byte{"recordings/recording.mp4": mp4HeaderBytes()},
			wantErr: "live-proof-eligible ssh-sessiond",
		},
		{
			name:      "traversal recording path",
			runtime:   liveRuntime,
			processes: console,
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/../logs/session.log", MediaType: "video/mp4", DurationSeconds: defaultRecordingDuration.Seconds(), FPS: defaultRecordingFPS, TargetProfile: "ci", Host: "maya-win-01"},
			},
			files:   map[string][]byte{"logs/session.log": mp4HeaderBytes()},
			wantErr: "under recordings/",
		},
		{
			name:      "non console maya",
			runtime:   liveRuntime,
			processes: []windowsProcessSession{{ProcessID: 42, SessionID: 0, SessionName: "Services", Name: "maya.exe"}},
			visual: []visualEvidenceArtifact{
				{Kind: "recording", Path: "recordings/recording.mp4", MediaType: "video/mp4", DurationSeconds: defaultRecordingDuration.Seconds(), FPS: defaultRecordingFPS, TargetProfile: "ci", Host: "maya-win-01"},
			},
			files:   map[string][]byte{"recordings/recording.mp4": mp4HeaderBytes()},
			wantErr: "interactive Console session",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			evidenceDir := writeLiveRecordCommandProofBundle(t, tt.runtime, tt.visual, tt.files)
			bundle, err := validateLiveRecordCommandProofBundle(evidenceDir, tt.processes, "ci", "maya-win-01")
			if tt.wantValid {
				if err != nil {
					t.Fatalf("validateLiveRecordCommandProofBundle returned error: %v", err)
				}
				if len(bundle.VisualEvidence) != 1 {
					t.Fatalf("Visual Evidence count = %d, want 1", len(bundle.VisualEvidence))
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateLiveRecordCommandProofBundle error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLiveVisualEvidenceHostProofDoesNotDependOnViewportCapture(t *testing.T) {
	report := hostHealthReport{
		Runtime: runtimeMetadata{Profile: "ssh-sessiond", HostAdapter: "ssh", BrokerAdapter: "gg-mayasessiond", LiveProofEligible: true},
		Layers: []hostHealthLayer{
			okCheck("local-config", ".maya-stall.yaml"),
			okCheck("scenario-inputs", "smoke"),
			okCheck("target-profile", "default"),
			okCheck("host-pool", "windows-maya"),
			okCheck("host", "maya-win-01"),
			okCheck("ssh", "reachable"),
			okCheck("work-root", "writable"),
			withState(okCheck("host-lock", "unlocked"), "unlocked"),
			withSource(okCheck("session-broker", "gg_mayasessiond reachable; Maya UI is interactive"), "gg-mayasessiond"),
			okCheck("maya-version", "2025 satisfies Scenario smoke"),
			withSource(failedCheck("visual-evidence", "Error calling tool 'viewport.capture': Maya did not return an output path.", "Repair viewport.capture in gg_mayasessiond."), "session-broker"),
		},
	}
	report.Layers[8].InteractiveDesktop = true
	if err := validateLiveVisualEvidenceHostProof(report); err != nil {
		t.Fatalf("validateLiveVisualEvidenceHostProof returned error: %v", err)
	}

	report.Layers[8].InteractiveDesktop = false
	if err := validateLiveVisualEvidenceHostProof(report); err == nil || !strings.Contains(err.Error(), "interactive gg_mayasessiond") {
		t.Fatalf("validateLiveVisualEvidenceHostProof error = %v, want interactive broker failure", err)
	}
}

func TestWindowsDesktopCaptureCommandsUseInteractiveDesktop(t *testing.T) {
	screenshot := windowsDesktopScreenshotPowerShell("C:/maya-stall/artifacts/proof")
	for _, want := range []string{"System.Windows.Forms", "ImageFormat]::Png", "schtasks.exe", "/IT", "MayaStallVisualEvidenceScreenshot"} {
		if !strings.Contains(screenshot, want) {
			t.Fatalf("screenshot command missing %q:\n%s", want, screenshot)
		}
	}
	if strings.Contains(screenshot, "viewport.capture") {
		t.Fatalf("screenshot command must not use viewport.capture:\n%s", screenshot)
	}

	recording := windowsDesktopRecordingPowerShell("C:/maya-stall/artifacts/proof", 3, 500)
	for _, want := range []string{"System.Windows.Forms", "ImageFormat]::Jpeg", "Compress-Archive", "frame-*.jpg", "schtasks.exe", "/IT", "MayaStallVisualEvidenceRecording"} {
		if !strings.Contains(recording, want) {
			t.Fatalf("recording command missing %q:\n%s", want, recording)
		}
	}
	if strings.Contains(recording, "viewport.capture") {
		t.Fatalf("recording command must not use viewport.capture:\n%s", recording)
	}
}

func TestLiveDesktopControlModalFixtureUsesInteractiveTask(t *testing.T) {
	fixture := liveDesktopControlModalFixture{
		RemoteRoot: "C:/maya-stall/artifacts/modal-proof",
		TaskName:   "MayaStallDesktopControlModal-proof",
		ClickX:     300,
		ClickY:     251,
	}
	launch := liveDesktopControlModalFixturePowerShell(fixture)
	for _, want := range []string{
		"System.Windows.Forms",
		"ShowDialog",
		"schtasks.exe",
		"/IT",
		"desktop-control-modal.shown",
		"desktop-control-modal.closed",
		"FormBorderStyle = \"None\"",
		"FromArgb(255, 0, 255)",
	} {
		if !strings.Contains(launch, want) {
			t.Fatalf("modal fixture launch missing %q:\n%s", want, launch)
		}
	}

	cleanup := liveDesktopControlModalCleanupPowerShell(fixture)
	for _, want := range []string{"schtasks.exe /Delete", "Stop-Process", "Remove-Item -Recurse -Force"} {
		if !strings.Contains(cleanup, want) {
			t.Fatalf("modal fixture cleanup missing %q:\n%s", want, cleanup)
		}
	}
}

func TestLiveDesktopControlModalMarkerDetection(t *testing.T) {
	dir := t.TempDir()
	markerPath := filepath.Join(dir, "marker.png")
	marker := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			marker.Set(x, y, color.RGBA{R: 255, B: 255, A: 255})
		}
	}
	writePNGForTest(t, markerPath, marker)
	if err := validateLiveDesktopControlModalMarker(markerPath); err != nil {
		t.Fatalf("validateLiveDesktopControlModalMarker returned error: %v", err)
	}

	plainPath := filepath.Join(dir, "plain.png")
	plain := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			plain.Set(x, y, color.RGBA{R: 255, G: 255, B: 255, A: 255})
		}
	}
	writePNGForTest(t, plainPath, plain)
	if err := validateLiveDesktopControlModalMarker(plainPath); err == nil || !strings.Contains(err.Error(), "modal marker") {
		t.Fatalf("validateLiveDesktopControlModalMarker error = %v, want modal marker failure", err)
	}
}

func captureLiveScreenshotCommandProof(t *testing.T, repoDir string, options realSSHSmokeOptions) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(options.screenshotArgs(), &stdout, &stderr, repoDir, "test-version")
	if code != 0 {
		t.Fatalf("live screenshot command exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidenceDir := smokeOutputValue(stdout.String(), "evidence")
	if evidenceDir == "" {
		t.Fatalf("live screenshot command did not print Evidence Bundle path:\n%s", stdout.String())
	}
	return evidenceDir
}

func captureLiveRecordCommandProof(t *testing.T, repoDir string, options realSSHSmokeOptions) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(options.recordArgs(), &stdout, &stderr, repoDir, "test-version")
	if code != 0 {
		t.Fatalf("live record command exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	evidenceDir := smokeOutputValue(stdout.String(), "evidence")
	if evidenceDir == "" {
		t.Fatalf("live record command did not print Evidence Bundle path:\n%s", stdout.String())
	}
	return evidenceDir
}

func selectLiveDesktopControlSmokeHost(t *testing.T, repoDir string, options realSSHSmokeOptions) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(options.controlClickArgs(liveDesktopControlModalClickX, liveDesktopControlModalClickY, "--dry-run"), &stdout, &stderr, repoDir, "test-version")
	if code != 0 {
		t.Fatalf("live desktop control dry-run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	hostID := smokeOutputValue(stdout.String(), "host")
	if hostID == "" {
		t.Fatalf("live desktop control dry-run did not print selected host:\n%s", stdout.String())
	}
	return hostID
}

func assertLiveDesktopControlScreenshotProofBundle(t *testing.T, evidenceDir string, options realSSHSmokeOptions) evidenceBundle {
	t.Helper()
	selected := readEvidenceBundle(t, evidenceDir)
	host := liveSmokeHostConfigByID(t, options, selected.Host)
	processes, err := mayaTasklistSessions(host)
	if err != nil {
		t.Fatalf("query maya.exe tasklist sessions: %v", err)
	}
	bundle, err := validateLiveDesktopControlScreenshotProofBundle(evidenceDir, processes, options.TargetProfile, options.Host)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func validateLiveDesktopControlScreenshotProofBundle(evidenceDir string, processes []windowsProcessSession, targetProfile string, hostPin string) (evidenceBundle, error) {
	content, err := os.ReadFile(filepath.Join(evidenceDir, evidenceBundleFileName))
	if err != nil {
		return evidenceBundle{}, err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireLiveRuntime(bundle.Runtime); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireConsoleMayaProcess(processes); err != nil {
		return evidenceBundle{}, err
	}
	if bundle.Scenario != evidenceStandaloneScenarioPrefix+"screenshot" {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle scenario = %q, want standalone screenshot command scenario", bundle.Scenario)
	}
	if bundle.TargetProfile != targetProfile {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle Target Profile = %q, want %q", bundle.TargetProfile, targetProfile)
	}
	if bundle.Host == "" {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle missing selected Maya Host metadata")
	}
	if hostPin != "" && bundle.Host != hostPin {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle selected Maya Host = %q, want pinned host %q", bundle.Host, hostPin)
	}
	screenshot, err := liveDesktopControlScreenshotArtifact(bundle)
	if err != nil {
		return evidenceBundle{}, err
	}
	screenshotBytes, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(screenshot.Path)))
	if err != nil {
		return evidenceBundle{}, err
	}
	if !looksLikeImageBytes("image/png", screenshotBytes) {
		return evidenceBundle{}, fmt.Errorf("desktop control screenshot %s does not look like a PNG", screenshot.Path)
	}
	return bundle, nil
}

func requireLiveDesktopControlScreenshotArtifact(t *testing.T, evidenceDir string, bundle evidenceBundle) visualEvidenceArtifact {
	t.Helper()
	screenshot, err := liveDesktopControlScreenshotArtifact(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir, filepath.FromSlash(screenshot.Path))); err != nil {
		t.Fatalf("missing Visual Evidence artifact %s: %v", screenshot.Path, err)
	}
	return screenshot
}

func requireLiveRunScopedScreenshotArtifact(t *testing.T, evidenceDir string, bundle evidenceBundle) visualEvidenceArtifact {
	t.Helper()
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Kind == "screenshot" && artifact.MediaType == "image/png" && artifact.Path == "screenshots/"+runScopedScreenshotName {
			content, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(artifact.Path)))
			if err != nil {
				t.Fatalf("missing run-scoped Visual Evidence artifact %s: %v", artifact.Path, err)
			}
			if artifact.TargetProfile != bundle.TargetProfile || artifact.Host != bundle.Host {
				t.Fatalf("run-scoped screenshot target metadata = %+v, want bundle target %q host %q", artifact, bundle.TargetProfile, bundle.Host)
			}
			if artifact.Origin != visualEvidenceOriginBrokerCapture || artifact.SHA256 != sha256HexOfBytes(content) {
				t.Fatalf("run-scoped screenshot provenance = %+v, want broker capture with byte-matched sha256", artifact)
			}
			if err := requireBrokerCaptureProvenanceEvents(evidenceDir, bundle); err != nil {
				t.Fatalf("run-scoped screenshot provenance events: %v", err)
			}
			return artifact
		}
	}
	t.Fatalf("live run-scoped proof requires run-scoped screenshot artifact, got %+v", bundle.VisualEvidence)
	return visualEvidenceArtifact{}
}

func assertLiveDesktopControlScreenshotShowsModal(t *testing.T, evidenceDir string, screenshot visualEvidenceArtifact) {
	t.Helper()
	path := filepath.Join(evidenceDir, filepath.FromSlash(screenshot.Path))
	if err := validateLiveDesktopControlModalMarker(path); err != nil {
		t.Fatal(err)
	}
}

func validateLiveDesktopControlModalMarker(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	img, err := png.Decode(file)
	if err != nil {
		return fmt.Errorf("decode desktop control screenshot PNG: %w", err)
	}
	markerPixels := 0
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if r>>8 >= 240 && g>>8 <= 20 && b>>8 >= 240 {
				markerPixels++
			}
		}
	}
	if markerPixels < 500 {
		return fmt.Errorf("desktop control screenshot is missing modal marker; found %d marker pixels", markerPixels)
	}
	return nil
}

func writePNGForTest(t *testing.T, path string, img image.Image) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create png fixture: %v", err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Fatalf("close png fixture: %v", err)
		}
	}()
	if err := png.Encode(file, img); err != nil {
		t.Fatalf("encode png fixture: %v", err)
	}
}

func liveDesktopControlScreenshotArtifact(bundle evidenceBundle) (visualEvidenceArtifact, error) {
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Kind == "screenshot" && artifact.MediaType == "image/png" && artifact.Path == "screenshots/screenshot.png" {
			return artifact, nil
		}
	}
	return visualEvidenceArtifact{}, fmt.Errorf("live desktop control proof requires standalone desktop screenshot artifact, got %+v", bundle.VisualEvidence)
}

const (
	liveDesktopControlModalLeft   = 120
	liveDesktopControlModalTop    = 120
	liveDesktopControlModalButton = 130
	liveDesktopControlModalClickX = 300
	liveDesktopControlModalClickY = 251
	defaultLiveSessiondUITaskName = "MayaStallSessiondUI"
	smokeSessiondUITaskEnv        = "MAYA_STALL_SMOKE_SESSIOND_UI_TASK"
)

type liveDesktopControlModalFixture struct {
	RemoteRoot string
	TaskName   string
	ClickX     int
	ClickY     int
}

func launchLiveDesktopControlModalFixture(t *testing.T, host mayaHostConfig) liveDesktopControlModalFixture {
	t.Helper()
	suffix := time.Now().UTC().Format("20060102T150405000000000")
	fixture := liveDesktopControlModalFixture{
		RemoteRoot: remoteJoin(host.WorkRoot, "artifacts", "desktop-control-modal-"+suffix),
		TaskName:   "MayaStallDesktopControlModal-" + suffix,
		ClickX:     liveDesktopControlModalClickX,
		ClickY:     liveDesktopControlModalClickY,
	}
	transport := sshWindowsDesktopTransport(host)
	if _, err := transport.RunPowerShell(fmt.Sprintf("New-Item -ItemType Directory -Force -Path %s | Out-Null", powerShellSingleQuoted(fixture.RemoteRoot)), sessiondCommandTimeout); err != nil {
		t.Fatalf("create live desktop control modal fixture root: %v", err)
	}
	launchPath := remoteJoin(fixture.RemoteRoot, "launch-desktop-control-modal.ps1")
	if err := transport.WritePowerShellScript(launchPath, liveDesktopControlModalFixturePowerShell(fixture), sessiondCommandTimeout); err != nil {
		_, _ = runSSHCommandOutput(host, encodedPowerShellCommand(liveDesktopControlModalCleanupPowerShell(fixture)), sessiondCommandTimeout)
		t.Fatalf("stage live desktop control modal fixture: %v", err)
	}
	raw, err := transport.RunPowerShell(fmt.Sprintf("Set-ExecutionPolicy -Scope Process -ExecutionPolicy Bypass -Force; & %s", powerShellSingleQuoted(launchPath)), sessiondCommandTimeout)
	if err != nil {
		_, _ = runSSHCommandOutput(host, encodedPowerShellCommand(liveDesktopControlModalCleanupPowerShell(fixture)), sessiondCommandTimeout)
		t.Fatalf("launch live desktop control modal fixture: %v: %s", err, strings.TrimSpace(string(raw)))
	}
	if !strings.Contains(string(raw), "shown") {
		_, _ = runSSHCommandOutput(host, encodedPowerShellCommand(liveDesktopControlModalCleanupPowerShell(fixture)), sessiondCommandTimeout)
		t.Fatalf("live desktop control modal fixture did not report shown: %s", strings.TrimSpace(string(raw)))
	}
	return fixture
}

func waitForLiveDesktopControlModalClosed(t *testing.T, host mayaHostConfig, fixture liveDesktopControlModalFixture) {
	t.Helper()
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(liveDesktopControlModalClosedPowerShell(fixture)), 10*time.Second)
	if err != nil {
		t.Fatalf("wait for live desktop control modal fixture to close: %v: %s", err, strings.TrimSpace(string(raw)))
	}
}

func cleanupLiveDesktopControlModalFixture(t *testing.T, host mayaHostConfig, fixture liveDesktopControlModalFixture) {
	t.Helper()
	if fixture.RemoteRoot == "" {
		return
	}
	if _, err := runSSHCommandOutput(host, encodedPowerShellCommand(liveDesktopControlModalCleanupPowerShell(fixture)), sessiondCommandTimeout); err != nil {
		t.Fatalf("cleanup live desktop control modal fixture: %v", err)
	}
}

func waitForLiveSessionBrokerCallReady(t *testing.T, host mayaHostConfig) {
	t.Helper()
	var lastErr error
	for i := 0; i < 10; i++ {
		if err := liveSessionBrokerCallReady(host); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("session broker call server did not settle after desktop control modal proof: %v", lastErr)
}

func liveSessionBrokerCallReady(host mayaHostConfig) error {
	stateDir := strings.TrimSpace(host.Broker.StateDir)
	if stateDir == "" {
		stateDir = "C:/maya-stall/sessiond-ui"
	}
	python := strings.TrimSpace(host.Broker.Python)
	if python == "" {
		python = "python"
	}
	repo := strings.TrimSpace(host.Broker.Repo)
	if repo == "" {
		repo = "."
	}
	script := fmt.Sprintf(`$ErrorActionPreference = "Stop"
cd %s
& %s -m gg_maya_sessiond.cli call --state-dir %s --list --json`,
		powerShellSingleQuoted(repo),
		powerShellSingleQuoted(python),
		powerShellSingleQuoted(stateDir),
	)
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), 10*time.Second)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(raw)))
	}
	if !strings.Contains(string(raw), `"ok": true`) {
		return fmt.Errorf("session broker call list did not report ok: %s", strings.TrimSpace(string(raw)))
	}
	return nil
}

func liveSessionBrokerFixtureReady(host mayaHostConfig) error {
	status, err := ggMayaSessiondBroker{host: host}.status()
	if err != nil {
		return err
	}
	effectiveStatus := status.DerivedStatus
	if effectiveStatus == "" {
		effectiveStatus = status.State.Status
	}
	if effectiveStatus != "running" {
		return fmt.Errorf("gg_mayasessiond status = %q, want running", effectiveStatus)
	}
	if !status.State.CallServerReady {
		return fmt.Errorf("gg_mayasessiond call server is not ready")
	}
	processes, err := mayaTasklistSessions(host)
	if err != nil {
		return err
	}
	if err := requireConsoleMayaProcess(processes); err != nil {
		return err
	}
	return nil
}

func restoreLiveSessionBrokerFixtures(t *testing.T, options realSSHSmokeOptions) {
	t.Helper()
	config, err := loadUserHostConfig(options.HostConfig)
	if err != nil {
		t.Fatalf("load live host config for session broker restore: %v", err)
	}
	candidates, err := hostCandidates(config, options.TargetProfile, options.Host)
	if err != nil {
		t.Fatalf("select live hosts for session broker restore: %v", err)
	}
	if len(candidates) == 0 {
		t.Fatalf("no live hosts available for target profile %q", options.TargetProfile)
	}
	restored := false
	for _, host := range candidates {
		if isHealthyHost(host) && host.Broker.isGGMayaSessiond() {
			restoreLiveSessionBrokerFixture(t, host)
			restored = true
		}
	}
	if !restored {
		t.Fatalf("no healthy gg_mayasessiond live hosts available for target profile %q", options.TargetProfile)
	}
}

func restoreLiveSessionBrokerFixture(t *testing.T, host mayaHostConfig) {
	t.Helper()
	if err := liveSessionBrokerFixtureReady(host); err == nil {
		return
	}
	taskName := strings.TrimSpace(os.Getenv(smokeSessiondUITaskEnv))
	if taskName == "" {
		taskName = defaultLiveSessiondUITaskName
	}
	script := fmt.Sprintf(`$ErrorActionPreference = "Stop"
$taskName = %s
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
if ($task.State -eq "Running") {
  Write-Output "running"
} else {
  Start-ScheduledTask -InputObject $task
  Write-Output "started"
}`,
		powerShellSingleQuoted(taskName),
	)
	if raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sessiondCommandTimeout); err != nil {
		t.Fatalf("restore live session broker fixture with scheduled task %q: %v: %s", taskName, err, strings.TrimSpace(string(raw)))
	}
	var lastErr error
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if err := liveSessionBrokerFixtureReady(host); err == nil {
			return
		} else {
			lastErr = err
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("restore live session broker fixture did not return Maya and session broker to the interactive Console session: %v", lastErr)
}

func liveDesktopControlModalFixturePowerShell(fixture liveDesktopControlModalFixture) string {
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"
$root = %s
$taskName = %s
New-Item -ItemType Directory -Force -Path $root | Out-Null
if (-not (Get-Command schtasks.exe -ErrorAction SilentlyContinue)) { throw "schtasks.exe is required for interactive desktop control modal proof" }
$shown = Join-Path $root "desktop-control-modal.shown"
$closed = Join-Path $root "desktop-control-modal.closed"
$pidPath = Join-Path $root "desktop-control-modal.pid"
$script = Join-Path $root "desktop-control-modal.ps1"
$template = @'
$ErrorActionPreference = "Stop"
Set-Content -LiteralPath "__MAYA_STALL_MODAL_PID__" -Value $PID
Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
[System.Windows.Forms.Application]::EnableVisualStyles()
$form = New-Object System.Windows.Forms.Form
$form.FormBorderStyle = "None"
$form.TopMost = $true
$form.StartPosition = "Manual"
$form.Location = New-Object System.Drawing.Point(%d, %d)
$form.Size = New-Object System.Drawing.Size(420, 220)
$form.BackColor = [System.Drawing.Color]::FromArgb(255, 0, 255)
$label = New-Object System.Windows.Forms.Label
$label.AutoSize = $false
$label.Location = New-Object System.Drawing.Point(24, 26)
$label.Size = New-Object System.Drawing.Size(372, 72)
$label.BackColor = [System.Drawing.Color]::White
$label.Text = "Maya Stall desktop control smoke prompt"
$label.Font = New-Object System.Drawing.Font("Segoe UI", 14)
$form.Controls.Add($label)
$marker = New-Object System.Windows.Forms.Panel
$marker.Location = New-Object System.Drawing.Point(300, 24)
$marker.Size = New-Object System.Drawing.Size(80, 50)
$marker.BackColor = [System.Drawing.Color]::FromArgb(255, 0, 255)
$form.Controls.Add($marker)
$button = New-Object System.Windows.Forms.Button
$button.Text = "OK"
$button.Location = New-Object System.Drawing.Point(%d, 110)
$button.Size = New-Object System.Drawing.Size(100, 42)
$button.Add_Click({
  Set-Content -LiteralPath "__MAYA_STALL_MODAL_CLOSED__" -Value "clicked"
  $form.Close()
})
$form.Controls.Add($button)
$form.Add_Shown({
  Set-Content -LiteralPath "__MAYA_STALL_MODAL_SHOWN__" -Value "shown"
})
[void]$form.ShowDialog()
'@
$content = $template.Replace("__MAYA_STALL_MODAL_PID__", $pidPath.Replace("\", "\\")).Replace("__MAYA_STALL_MODAL_SHOWN__", $shown.Replace("\", "\\")).Replace("__MAYA_STALL_MODAL_CLOSED__", $closed.Replace("\", "\\"))
Set-Content -Encoding ASCII -LiteralPath $script -Value $content
cmd.exe /c "schtasks.exe /Delete /TN $taskName /F 2>NUL" | Out-Null
$startTime = (Get-Date).AddMinutes(1).ToString("HH:mm")
$taskRun = 'powershell.exe -NoProfile -WindowStyle Hidden -ExecutionPolicy Bypass -File "' + $script + '"'
$createArgs = @("/Create", "/TN", $taskName, "/SC", "ONCE", "/ST", $startTime, "/TR", $taskRun, "/RL", "HIGHEST", "/IT", "/F")
& schtasks.exe @createArgs | Out-Null
if ($LASTEXITCODE -ne 0) { throw "failed to create interactive desktop control modal task with schtasks.exe /IT; ensure an interactive desktop session is logged in" }
schtasks.exe /Run /TN $taskName | Out-Null
for ($i = 0; $i -lt 40; $i++) {
  if (Test-Path -LiteralPath $shown) {
    [pscustomobject]@{ status = "shown"; clickX = %d; clickY = %d } | ConvertTo-Json -Compress
    exit 0
  }
  Start-Sleep -Milliseconds 250
}
throw "scheduled interactive desktop control modal did not appear; ensure an interactive desktop session is logged in"`,
		powerShellSingleQuoted(fixture.RemoteRoot),
		powerShellSingleQuoted(fixture.TaskName),
		liveDesktopControlModalLeft,
		liveDesktopControlModalTop,
		liveDesktopControlModalButton,
		fixture.ClickX,
		fixture.ClickY,
	)
}

func liveDesktopControlModalClosedPowerShell(fixture liveDesktopControlModalFixture) string {
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$closed = Join-Path %s "desktop-control-modal.closed"
for ($i = 0; $i -lt 40; $i++) {
  if (Test-Path -LiteralPath $closed) {
    Write-Output "closed"
    exit 0
  }
  Start-Sleep -Milliseconds 250
}
throw "desktop control modal fixture did not close after click"`, powerShellSingleQuoted(fixture.RemoteRoot))
}

func liveDesktopControlModalCleanupPowerShell(fixture liveDesktopControlModalFixture) string {
	return fmt.Sprintf(`$ErrorActionPreference = "Continue"
$root = %s
$taskName = %s
schtasks.exe /Delete /TN $taskName /F 2>$null | Out-Null
$pidPath = Join-Path $root "desktop-control-modal.pid"
if (Test-Path -LiteralPath $pidPath) {
  $processID = [int](Get-Content -LiteralPath $pidPath -Raw)
  Stop-Process -Id $processID -Force -ErrorAction SilentlyContinue
}
Remove-Item -Recurse -Force -LiteralPath $root -ErrorAction SilentlyContinue`,
		powerShellSingleQuoted(fixture.RemoteRoot),
		powerShellSingleQuoted(fixture.TaskName),
	)
}

func addLiveDesktopScreenshotForProofArtifact(t *testing.T, repoDir string, evidenceDir string, options realSSHSmokeOptions) {
	t.Helper()
	bundle := readEvidenceBundle(t, evidenceDir)
	release, locked, err := acquireHostLock(repoDir, bundle.Host)
	if err != nil {
		t.Fatalf("acquire Host Lock for proof artifact screenshot on %s: %v", bundle.Host, err)
	}
	if locked {
		t.Fatalf("selected Maya Host %s became locked before proof artifact screenshot", bundle.Host)
	}
	defer func() {
		if err := release(); err != nil {
			t.Fatalf("release Host Lock for proof artifact screenshot on %s: %v", bundle.Host, err)
		}
	}()
	host := liveSmokeHostConfigByID(t, options, bundle.Host)
	processes, err := mayaTasklistSessions(host)
	if err != nil {
		t.Fatalf("query maya.exe tasklist sessions: %v", err)
	}
	if err := requireConsoleMayaProcess(processes); err != nil {
		t.Fatal(err)
	}
	context := runContext{
		RunWorkspace: runWorkspace{
			runID:          bundle.RunID,
			remoteWorkRoot: remotePath(host.WorkRoot),
		},
		EvidenceDir: evidenceDir,
		EventsPath:  filepath.Join(evidenceDir, evidenceEventsFileName),
	}
	screenshot, err := (ggMayaSessiondBroker{host: host}).CaptureScreenshot(context, screenshotRequest{Name: "desktop-screenshot.png"})
	if err != nil {
		t.Fatalf("capture desktop screenshot through Session Broker for proof artifact: %v", err)
	}
	screenshot.TargetProfile = bundle.TargetProfile
	screenshot.Host = bundle.Host
	bundle.VisualEvidence = append([]visualEvidenceArtifact{screenshot}, bundle.VisualEvidence...)
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	if err := writeJSONFile(filepath.Join(evidenceDir, evidenceBundleFileName), bundle); err != nil {
		t.Fatalf("update live proof Evidence Bundle with desktop screenshot: %v", err)
	}
}

func liveSmokeHostConfigByID(t *testing.T, options realSSHSmokeOptions, hostID string) mayaHostConfig {
	t.Helper()
	if hostID == "" {
		t.Fatalf("live Evidence Bundle did not record a selected Maya Host")
	}
	config, err := loadUserHostConfig(options.HostConfig)
	if err != nil {
		t.Fatalf("load live host config: %v", err)
	}
	candidates, err := hostCandidates(config, options.TargetProfile, hostID)
	if err != nil {
		t.Fatalf("select live host config: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != hostID {
		t.Fatalf("selected live host config = %+v, want host %q", candidates, hostID)
	}
	if !isHealthyHost(candidates[0]) {
		t.Fatalf("selected live host %q is not healthy in Target Profile %q", hostID, options.TargetProfile)
	}
	return candidates[0]
}

func publishOptionalLiveVisualEvidenceProofArtifact(t *testing.T, evidenceDir string) {
	t.Helper()
	options, err := liveVisualEvidenceProofArtifactOptionsFromEnv(os.LookupEnv)
	if err != nil {
		t.Fatalf("parse live Visual Evidence proof artifact config: %v", err)
	}
	if !options.Enabled {
		t.Logf("Live Visual Evidence proof artifact upload disabled; set %s=true to publish public-safe metadata", liveProofArtifactEnabledEnv)
		return
	}
	published, err := publishLiveVisualEvidenceProofArtifact(evidenceDir, options)
	if err != nil {
		t.Fatalf("publish live Visual Evidence proof artifact: %v", err)
	}
	legacyCompatibility, err := legacyTrustedWorkflowMetadataCompatibilityRequested(os.LookupEnv)
	if err != nil {
		t.Fatalf("parse legacy trusted-workflow compatibility: %v", err)
	}
	if legacyCompatibility {
		if err := addLegacyTrustedWorkflowMetadataCompatibility(published); err != nil {
			t.Fatalf("add legacy trusted-workflow metadata compatibility: %v", err)
		}
	}
	t.Logf("Live Visual Evidence proof artifact: live-visual-evidence-proof path=%s retentionDays=%d", filepath.Base(published), options.RetentionDays)
}

const legacyLiveProofMediaReviewedEnv = "MAYA_STALL_LIVE_PROOF_MEDIA_REVIEWED"

func legacyTrustedWorkflowMetadataCompatibilityRequested(lookup func(string) (string, bool)) (bool, error) {
	raw, ok := lookup(legacyLiveProofMediaReviewedEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return false, nil
	}
	return parseBoolConfig(raw)
}

func addLegacyTrustedWorkflowMetadataCompatibility(destination string) error {
	// workflow_run executes the trusted workflow from main. During this one-step
	// transition, its retired five-path allowlist still runs before the new
	// metadata-only workflow lands. Satisfy those names with explicit JSON
	// metadata markers; never copy or synthesize desktop pixels.
	type marker struct {
		Kind           string `json:"kind"`
		MediaPublished bool   `json:"mediaPublished"`
		SourceMetadata string `json:"sourceMetadata"`
	}
	type mediaReview struct {
		Reviewed       bool     `json:"reviewed"`
		Scope          string   `json:"scope"`
		Paths          []string `json:"paths"`
		MediaPublished bool     `json:"mediaPublished"`
	}
	markerPaths := []string{
		"recordings/recording.mp4",
		"screenshots/desktop-screenshot.png",
	}
	for _, relative := range markerPaths {
		if err := writePublicSafeProofJSON(destination, relative, marker{
			Kind:           "metadata-only-compatibility-marker",
			MediaPublished: false,
			SourceMetadata: liveProofEvidenceMetadataName,
		}); err != nil {
			return err
		}
	}
	if err := writePublicSafeProofJSON(destination, "media-review.json", mediaReview{
		Reviewed:       true,
		Scope:          "metadata-only-legacy-workflow-compatibility",
		Paths:          markerPaths,
		MediaPublished: false,
	}); err != nil {
		return err
	}

	manifestPath := filepath.Join(destination, liveProofArtifactManifestName)
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest liveVisualEvidenceProofArtifactManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return err
	}
	for _, relative := range append([]string{"media-review.json"}, markerPaths...) {
		entry, err := proofArtifactFileEntry(destination, relative, "compatibility-metadata", "application/json")
		if err != nil {
			return err
		}
		manifest.Artifacts = append(manifest.Artifacts, entry)
	}
	sort.SliceStable(manifest.Artifacts, func(i, j int) bool {
		return manifest.Artifacts[i].Path < manifest.Artifacts[j].Path
	})
	return writePublicSafeProofJSON(destination, liveProofArtifactManifestName, manifest)
}

func writePublicSafeProofJSON(root string, relative string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	content = append(content, '\n')
	if err := rejectConfidentialProofText(relative, string(content)); err != nil {
		return err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, content, 0o644)
}

func mayaTasklistSessions(host mayaHostConfig) ([]windowsProcessSession, error) {
	script := `$ErrorActionPreference = 'Stop'
$rows = @(tasklist.exe /v /fi "imagename eq maya.exe" /fo csv | ConvertFrom-Csv | Where-Object { $_.'Image Name' -ieq 'maya.exe' } | ForEach-Object {
  [pscustomobject]@{
    ProcessId = [int]$_.PID
    SessionId = [int]$_.'Session#'
    SessionName = $_.'Session Name'
    UserName = $_.'User Name'
    Name = $_.'Image Name'
  }
})
if ($rows.Count -eq 0) { Write-Output '[]' } else { $rows | ConvertTo-Json -Compress }`
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sessiondCommandTimeout)
	if err != nil {
		return nil, err
	}
	return parseWindowsProcessSessions(raw)
}

func parseWindowsProcessSessions(raw []byte) ([]windowsProcessSession, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var processes []windowsProcessSession
		if err := json.Unmarshal([]byte(trimmed), &processes); err != nil {
			return nil, fmt.Errorf("parse maya.exe process JSON: %w", err)
		}
		return processes, nil
	}
	var process windowsProcessSession
	if err := json.Unmarshal([]byte(trimmed), &process); err != nil {
		return nil, fmt.Errorf("parse maya.exe process JSON: %w", err)
	}
	return []windowsProcessSession{process}, nil
}

func assertLiveVisualEvidenceHostProof(t *testing.T, report hostHealthReport) {
	t.Helper()
	if err := validateLiveVisualEvidenceHostProof(report); err != nil {
		t.Fatalf("Host Health is not ready for live desktop Visual Evidence proof: %v: %s", err, formatHostHealthReport(report))
	}
	if visual, ok := hostHealthLayerByID(report, "visual-evidence"); ok && visual.Status != "ok" {
		t.Logf("viewport Visual Evidence health is not used as live desktop proof: %+v", visual)
	}
}

func validateLiveVisualEvidenceHostProof(report hostHealthReport) error {
	if err := requireLiveRuntime(report.Runtime); err != nil {
		return err
	}
	for _, id := range []string{"local-config", "scenario-inputs", "target-profile", "host-pool", "host", "ssh", "work-root", "host-lock", "maya-version"} {
		layer, ok := hostHealthLayerByID(report, id)
		if !ok {
			return fmt.Errorf("Host Health missing %s layer", id)
		}
		if layer.Status != "ok" {
			return fmt.Errorf("Host Health %s layer = %+v, want ok", id, layer)
		}
	}
	broker, ok := hostHealthLayerByID(report, "session-broker")
	if !ok {
		return fmt.Errorf("Host Health missing session-broker layer")
	}
	if broker.Status != "ok" || broker.Source != "gg-mayasessiond" || !broker.InteractiveDesktop {
		return fmt.Errorf("session-broker Host Health = %+v, want interactive gg_mayasessiond", broker)
	}
	return nil
}

func assertLiveRecordCommandProofBundle(t *testing.T, evidenceDir string, options realSSHSmokeOptions) evidenceBundle {
	t.Helper()
	selected := readEvidenceBundle(t, evidenceDir)
	host := liveSmokeHostConfigByID(t, options, selected.Host)
	processes, err := mayaTasklistSessions(host)
	if err != nil {
		t.Fatalf("query maya.exe tasklist sessions: %v", err)
	}
	bundle, err := validateLiveRecordCommandProofBundle(evidenceDir, processes, options.TargetProfile, options.Host)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func validateLiveRecordCommandProofBundle(evidenceDir string, processes []windowsProcessSession, targetProfile string, hostPin string) (evidenceBundle, error) {
	content, err := os.ReadFile(filepath.Join(evidenceDir, evidenceBundleFileName))
	if err != nil {
		return evidenceBundle{}, err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireLiveRuntime(bundle.Runtime); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireConsoleMayaProcess(processes); err != nil {
		return evidenceBundle{}, err
	}
	if bundle.Scenario != evidenceStandaloneScenarioPrefix+"recording" {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle scenario = %q, want standalone record command scenario", bundle.Scenario)
	}
	if bundle.TargetProfile != targetProfile {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle Target Profile = %q, want %q", bundle.TargetProfile, targetProfile)
	}
	if bundle.Host == "" {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle missing selected Maya Host metadata")
	}
	if hostPin != "" && bundle.Host != hostPin {
		return evidenceBundle{}, fmt.Errorf("Evidence Bundle selected Maya Host = %q, want pinned host %q", bundle.Host, hostPin)
	}
	recording, err := liveRecordCommandArtifact(bundle)
	if err != nil {
		return evidenceBundle{}, err
	}
	if recording.DurationSeconds <= 0 || recording.FPS <= 0 {
		return evidenceBundle{}, fmt.Errorf("recording %s missing duration/FPS metadata: %+v", recording.Path, recording)
	}
	if recording.TargetProfile != bundle.TargetProfile || recording.Host != bundle.Host {
		return evidenceBundle{}, fmt.Errorf("recording target metadata = %+v, want Target Profile %q and Maya Host %q", recording, bundle.TargetProfile, bundle.Host)
	}
	if err := requireBrokerCapturedVisualEvidence(evidenceDir, bundle); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireBrokerCaptureProvenanceEvents(evidenceDir, bundle); err != nil {
		return evidenceBundle{}, err
	}
	recordingBytes, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(recording.Path)))
	if err != nil {
		return evidenceBundle{}, err
	}
	if !looksLikeMP4Bytes(recordingBytes) {
		return evidenceBundle{}, fmt.Errorf("recording %s does not look like an MP4", recording.Path)
	}
	return bundle, nil
}

func liveRecordCommandArtifact(bundle evidenceBundle) (visualEvidenceArtifact, error) {
	var recordings []visualEvidenceArtifact
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Kind == "recording" {
			cleanPath := cleanEvidenceArtifactPath(artifact.Path)
			if !strings.HasPrefix(cleanPath, evidenceRecordingsDir+"/") {
				return visualEvidenceArtifact{}, fmt.Errorf("recording artifact = %+v, want path under recordings/", artifact)
			}
			artifact.Path = cleanPath
			recordings = append(recordings, artifact)
		}
	}
	if len(recordings) != 1 {
		return visualEvidenceArtifact{}, fmt.Errorf("Evidence Bundle has %d recording artifacts, want 1", len(recordings))
	}
	recording := recordings[0]
	if recording.MediaType != "video/mp4" || !strings.HasPrefix(recording.Path, evidenceRecordingsDir+"/") {
		return visualEvidenceArtifact{}, fmt.Errorf("recording artifact = %+v, want video/mp4 under recordings/", recording)
	}
	return recording, nil
}

func requireLiveRecordCommandArtifact(t *testing.T, evidenceDir string, bundle evidenceBundle) visualEvidenceArtifact {
	t.Helper()
	recording, err := liveRecordCommandArtifact(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(evidenceDir, filepath.FromSlash(recording.Path))); err != nil {
		t.Fatalf("missing record command artifact %s: %v", recording.Path, err)
	}
	return recording
}

func hostHealthLayerByID(report hostHealthReport, id string) (hostHealthLayer, bool) {
	for _, layer := range report.Layers {
		if layer.ID == id {
			return layer, true
		}
	}
	return hostHealthLayer{}, false
}

func assertLiveVisualEvidenceProofBundle(t *testing.T, evidenceDir string) evidenceBundle {
	t.Helper()
	processes := []windowsProcessSession{{ProcessID: 1, SessionID: 1, SessionName: "Console", Name: "maya.exe"}}
	bundle, err := validateLiveVisualEvidenceProofBundle(evidenceDir, processes)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func validateLiveVisualEvidenceProofBundle(evidenceDir string, processes []windowsProcessSession) (evidenceBundle, error) {
	content, err := os.ReadFile(filepath.Join(evidenceDir, evidenceBundleFileName))
	if err != nil {
		return evidenceBundle{}, err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireLiveRuntime(bundle.Runtime); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireConsoleMayaProcess(processes); err != nil {
		return evidenceBundle{}, err
	}
	screenshot, recording, err := liveDesktopVisualArtifacts(bundle)
	if err != nil {
		return evidenceBundle{}, err
	}
	if err := requireBrokerCapturedVisualEvidence(evidenceDir, bundle); err != nil {
		return evidenceBundle{}, err
	}
	if err := requireBrokerCaptureProvenanceEvents(evidenceDir, bundle); err != nil {
		return evidenceBundle{}, err
	}
	screenshotBytes, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(screenshot.Path)))
	if err != nil {
		return evidenceBundle{}, err
	}
	if !looksLikeImageBytes("image/png", screenshotBytes) {
		return evidenceBundle{}, fmt.Errorf("desktop screenshot %s does not look like a PNG", screenshot.Path)
	}
	recordingBytes, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(recording.Path)))
	if err != nil {
		return evidenceBundle{}, err
	}
	if !looksLikeMP4Bytes(recordingBytes) {
		return evidenceBundle{}, fmt.Errorf("desktop recording %s does not look like an MP4", recording.Path)
	}
	return bundle, nil
}

func requireConsoleMayaProcess(processes []windowsProcessSession) error {
	if len(processes) == 0 {
		return fmt.Errorf("maya.exe is not running in the interactive Console session")
	}
	for _, process := range processes {
		if strings.EqualFold(process.Name, "maya.exe") && strings.EqualFold(process.SessionName, "Console") && process.SessionID != 0 {
			return nil
		}
	}
	return fmt.Errorf("maya.exe is not running in the interactive Console session: %+v", processes)
}

func requireLiveDesktopVisualArtifacts(t *testing.T, evidenceDir string, bundle evidenceBundle) (visualEvidenceArtifact, visualEvidenceArtifact) {
	t.Helper()
	screenshot, recording, err := liveDesktopVisualArtifacts(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []visualEvidenceArtifact{screenshot, recording} {
		if _, err := os.Stat(filepath.Join(evidenceDir, filepath.FromSlash(artifact.Path))); err != nil {
			t.Fatalf("missing Visual Evidence artifact %s: %v", artifact.Path, err)
		}
	}
	return screenshot, recording
}

func looksLikeMP4Bytes(content []byte) bool {
	return len(content) >= 12 && string(content[4:8]) == "ftyp"
}

func artifactSize(t *testing.T, evidenceDir string, artifact visualEvidenceArtifact) int64 {
	t.Helper()
	info, err := os.Stat(filepath.Join(evidenceDir, filepath.FromSlash(artifact.Path)))
	if err != nil {
		t.Fatalf("stat %s: %v", artifact.Path, err)
	}
	return info.Size()
}

func writeLiveVisualEvidenceProofBundle(t *testing.T, runtime runtimeMetadata, visual []visualEvidenceArtifact, files map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, evidenceManifestFileName), "{}\n")
	var events strings.Builder
	for _, artifact := range visual {
		path, requestedEvent, capturedEvent := visualEvidenceEventDetails(artifact.Kind, artifact.Path)
		fmt.Fprintf(&events, "{\"event\":%q,\"detail\":%q,\"origin\":%q}\n", requestedEvent, path, artifact.Origin)
		fmt.Fprintf(&events, "{\"event\":%q,\"detail\":%q,\"origin\":%q,\"sha256\":%q}\n", capturedEvent, path, artifact.Origin, artifact.SHA256)
	}
	mustWriteFile(t, filepath.Join(dir, evidenceEventsFileName), events.String())
	mustWriteFile(t, filepath.Join(dir, evidenceLogPath), "log\n")
	mustWriteFile(t, filepath.Join(dir, evidenceScenarioResultFileName), `{"status":"passed"}`+"\n")
	for relative, content := range files {
		mustWriteFile(t, filepath.Join(dir, filepath.FromSlash(relative)), string(content))
	}
	bundle := evidenceBundle{
		RunID:          "20260706T120000.000000000Z",
		Scenario:       "live-visual-evidence-proof",
		Status:         resultStatusPassed,
		TargetProfile:  "ci",
		Host:           "alpha",
		Runtime:        runtime,
		Manifest:       evidenceManifestFileName,
		Events:         evidenceEventsFileName,
		Log:            evidenceLogPath,
		ScenarioResult: evidenceScenarioResultFileName,
		VisualEvidence: visual,
	}
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	if err := writeJSONFile(filepath.Join(dir, evidenceBundleFileName), bundle); err != nil {
		t.Fatalf("write evidence bundle: %v", err)
	}
	return dir
}

func writeLiveRecordCommandProofBundle(t *testing.T, runtime runtimeMetadata, visual []visualEvidenceArtifact, files map[string][]byte) string {
	t.Helper()
	dir := writeLiveVisualEvidenceProofBundle(t, runtime, visual, files)
	bundle := readEvidenceBundle(t, dir)
	bundle.Scenario = evidenceStandaloneScenarioPrefix + "recording"
	if len(visual) > 0 {
		bundle.TargetProfile = visual[0].TargetProfile
		bundle.Host = visual[0].Host
	}
	bundle.VisualEvidence = visual
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	if err := writeJSONFile(filepath.Join(dir, evidenceBundleFileName), bundle); err != nil {
		t.Fatalf("write record command proof bundle: %v", err)
	}
	return dir
}

func pngHeaderBytes() []byte {
	return []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0}
}

func jpegHeaderBytes() []byte {
	return []byte{0xff, 0xd8, 0xff, 0xdb}
}

func mp4HeaderBytes() []byte {
	return []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'}
}
