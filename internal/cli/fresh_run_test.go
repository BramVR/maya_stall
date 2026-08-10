package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshRunLifecycleOrdersSetupExecuteAndSettle(t *testing.T) {
	dir := writeRunConfigFixture(t)
	recorder := &freshRunLifecycleRecorder{}
	runtime := defaultRunRuntime()
	runtime.Host = recordingRunHost{recorder: recorder}
	runtime.Broker = recordingSessionBroker{recorder: recorder, result: ScenarioResult{Status: resultStatusPassed, Summary: "recorded"}}
	runtime.Now = func() time.Time {
		return time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC)
	}

	outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err != nil {
		t.Fatalf("Fresh Run returned error: %v", err)
	}
	if outcome.Result.Status != resultStatusPassed {
		t.Fatalf("Fresh Run status = %q, want passed", outcome.Result.Status)
	}

	recorder.requireOrder(t,
		"execute.start-session",
		"setup.stage-payload",
		"execute.run-scenario",
		"settle.collect-artifacts",
		"settle.stop-session",
	)
	readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", outcome.RunID))
	if _, _, err := readScenarioResultDocument(filepath.Join(dir, "artifacts", "maya-stall", outcome.RunID, "scenario-result.json")); err != nil {
		t.Fatalf("Fresh Run did not write Scenario Result during settle: %v", err)
	}
}

func TestFreshRunBindsSessionBeforeAssignedMayaBuildVerification(t *testing.T) {
	dir := writeRunConfigFixture(t)
	recorder := &freshRunLifecycleRecorder{}
	runtime := defaultRunRuntime()
	runtime.Host = recordingRunHost{recorder: recorder}
	runtime.Broker = recordingSessionBroker{recorder: recorder, result: ScenarioResult{Status: resultStatusPassed, Summary: "recorded"}}
	runtime.SessionStarted = func(brokerSessionIdentity) error {
		recorder.add("execute.bind-session")
		return nil
	}

	if _, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedMayaBuild: "2025"}, runtime).Run(); err != nil {
		t.Fatal(err)
	}
	recorder.requireOrder(t,
		"execute.start-session",
		"execute.bind-session",
		"execute.verify-maya-build",
		"setup.stage-payload",
		"execute.run-scenario",
		"settle.collect-artifacts",
		"settle.stop-session",
	)
}

func TestFreshRunLifecycleSelectsUniqueRunIDWithoutChangingExistingState(t *testing.T) {
	dir := writeRunConfigFixture(t)
	now := time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC)
	runID := now.Format("20060102T150405.000000000Z")
	sentinel := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "owned-by-other-run")
	mustWriteFile(t, sentinel, "keep\n")
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }

	outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err != nil {
		t.Fatalf("Fresh Run error = %v", err)
	}
	if outcome.RunID == runID {
		t.Fatalf("Fresh Run reused colliding Run ID %q", runID)
	}
	if content, err := os.ReadFile(sentinel); err != nil || string(content) != "keep\n" {
		t.Fatalf("Fresh Run changed colliding run state: content=%q err=%v", content, err)
	}
}

func TestFreshRunLifecycleSelectsUniqueRunIDWithoutChangingExistingEvidence(t *testing.T) {
	dir := writeRunConfigFixture(t)
	now := time.Date(2026, 7, 6, 12, 30, 0, 0, time.UTC)
	runID := now.Format("20060102T150405.000000000Z")
	blockingEvidencePath := filepath.Join(dir, "artifacts", "maya-stall", runID)
	mustWriteFile(t, blockingEvidencePath, "keep\n")
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }

	outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err != nil {
		t.Fatalf("Fresh Run error = %v", err)
	}
	if outcome.RunID == runID {
		t.Fatalf("Fresh Run reused colliding Run ID %q", runID)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !os.IsNotExist(statErr) {
		t.Fatalf("Fresh Run left owned state after partial setup: %v", statErr)
	}
	if content, readErr := os.ReadFile(blockingEvidencePath); readErr != nil || string(content) != "keep\n" {
		t.Fatalf("Fresh Run changed blocking Evidence path: content=%q err=%v", content, readErr)
	}
}

func TestFreshRunSessionBindingFailureStopsSessionAndReleasesHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	runtime := defaultRunRuntime()
	runtime.SessionStarted = func(brokerSessionIdentity) error {
		return fmt.Errorf("shared Host Lock binding rejected")
	}
	outcome, err := newFreshRun(dir, runOptions{
		ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways,
	}, runtime).Run()
	if err == nil || !strings.Contains(err.Error(), "shared Host Lock binding rejected") {
		t.Fatalf("Fresh Run session binding error = %v", err)
	}
	if outcome.Failure == nil || outcome.Failure.CleanupState != "completed" || outcome.StopPolicy != "stopped" {
		t.Fatalf("Fresh Run session binding failure outcome = %+v", outcome)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("session binding failure retained Host Lock: %v", statErr)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, ".maya-stall", "state", "runs", outcome.RunID)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("session binding failure retained run workspace: %v", statErr)
	}
}

func TestFreshRunLifecycleSettlesStopPolicyAndFailures(t *testing.T) {
	t.Run("success cleans state and releases Host Lock", func(t *testing.T) {
		dir := writeRunConfigFixture(t)

		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, defaultRunRuntime()).Run()
		if err != nil {
			t.Fatalf("Fresh Run returned error: %v", err)
		}
		if outcome.StopPolicy != "stopped" || outcome.Result.Status != resultStatusPassed {
			t.Fatalf("Fresh Run outcome = %+v, want stopped passed", outcome)
		}
		if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", outcome.RunID)); !os.IsNotExist(err) {
			t.Fatalf("stopped Fresh Run state = %v, want missing", err)
		}
		if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !os.IsNotExist(err) {
			t.Fatalf("stopped Fresh Run Host Lock = %v, want missing", err)
		}
		readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", outcome.RunID))
	})

	t.Run("broker error releases Host Lock", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = failingSessionBroker{message: "broker unavailable"}

		_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "broker unavailable") {
			t.Fatalf("Fresh Run error = %v, want broker unavailable", err)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !os.IsNotExist(statErr) {
			t.Fatalf("broker-error Host Lock = %v, want missing", statErr)
		}
		evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
		events, readErr := os.ReadFile(filepath.Join(evidence, "events.jsonl"))
		if readErr != nil {
			t.Fatalf("read broker-error events: %v", readErr)
		}
		if !strings.Contains(string(events), `"event":"broker.session.stopped"`) {
			t.Fatalf("broker-error events missing stopped Maya UI Session:\n%s", string(events))
		}
	})

	t.Run("broker error honors keep on failure", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = failingRetainableSessionBroker{
			fakeSessionBroker: fakeSessionBroker{},
			message:           "broker disconnected before result",
		}

		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterSuccess}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "broker disconnected before result") {
			t.Fatalf("Fresh Run error = %v, want broker disconnect", err)
		}
		if outcome.StopPolicy != "kept" || outcome.RunID == "" || len(outcome.FollowUpCommands) != 3 {
			t.Fatalf("Fresh Run outcome = %+v, want kept failed run", outcome)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); statErr != nil {
			t.Fatalf("kept broker-error Host Lock missing: %v", statErr)
		}
		record := readRunRetentionRecordFile(t, filepath.Join(outcome.StateDir, "run-record.json"))
		if record.Status != "kept" || record.RemoteSession.SessionID != "fake-"+outcome.RunID {
			t.Fatalf("broker-error retention record = %+v", record)
		}
	})

	t.Run("broker error does not report kept when retention fails", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = failingExecutionRetentionBroker{
			failingRetainableSessionBroker: failingRetainableSessionBroker{
				fakeSessionBroker: fakeSessionBroker{},
				message:           "broker disconnected before result",
			},
		}

		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterSuccess}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "retention failed") {
			t.Fatalf("Fresh Run error = %v, want retention failure", err)
		}
		if outcome.RunID == "" || outcome.StopPolicy != "stopped" || outcome.Result.Status != resultStatusFailed || len(outcome.FollowUpCommands) != 0 {
			t.Fatalf("retention-failure outcome = %+v, want terminal stopped failure", outcome)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !os.IsNotExist(statErr) {
			t.Fatalf("retention-failure Host Lock = %v, want released after session stop", statErr)
		}
	})

	t.Run("payload staging error honors keep on failure", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Host = stageFailingRunHost{message: "payload staging failed"}
		runtime.Broker = fakeSessionBroker{}

		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterSuccess}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "payload staging failed") {
			t.Fatalf("Fresh Run error = %v, want staging failure", err)
		}
		if outcome.StopPolicy != "kept" || outcome.RunID == "" {
			t.Fatalf("staging-failure outcome = %+v, want kept run", outcome)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); statErr != nil {
			t.Fatalf("staging-failure Host Lock missing: %v", statErr)
		}
	})

	t.Run("broker success requires an owned session identity", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = emptyIdentitySessionBroker{fakeSessionLifecycle: fakeSessionLifecycle{}}

		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "without an owned session identity") {
			t.Fatalf("Fresh Run error = %v, want missing owned session identity", err)
		}
		if outcome.StopPolicy != "unresolved" || len(outcome.FollowUpCommands) != 0 {
			t.Fatalf("ambiguous broker session outcome = %+v, want unresolved without an unsafe stop follow-up", outcome)
		}
		bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", outcome.RunID))
		if bundle.Failure == nil || bundle.Failure.CleanupState != "failed" {
			t.Fatalf("ambiguous broker session evidence = %+v", bundle.Failure)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); statErr != nil {
			t.Fatalf("ambiguous broker session did not retain Host Lock: %v", statErr)
		}
	})

	t.Run("broker error captures failure desktop evidence", func(t *testing.T) {
		dir := writeFailureScreenshotRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = failingScreenshotSessionBroker{message: "script.execute failed before Scenario Result"}

		_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "script.execute failed before Scenario Result") {
			t.Fatalf("Fresh Run error = %v, want script.execute failure", err)
		}
		evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
		bundle := readEvidenceBundle(t, evidence)
		if bundle.Status != resultStatusFailed {
			t.Fatalf("failure Evidence Bundle status = %q, want failed", bundle.Status)
		}
		if bundle.Failure == nil || bundle.Failure.CaptureState != "completed" {
			t.Fatalf("failure screenshot capture state = %+v", bundle.Failure)
		}
		if len(bundle.VisualEvidence) != 1 {
			t.Fatalf("failure Visual Evidence = %+v, want one desktop screenshot", bundle.VisualEvidence)
		}
		artifact := bundle.VisualEvidence[0]
		if artifact.Kind != "screenshot" || artifact.MediaType != "image/png" || artifact.Path != "screenshots/failure-desktop.png" {
			t.Fatalf("failure screenshot artifact = %+v", artifact)
		}
		if _, statErr := os.Stat(filepath.Join(evidence, artifact.Path)); statErr != nil {
			t.Fatalf("failure screenshot missing: %v", statErr)
		}
		if artifact.TargetProfile != "default" || artifact.Host != defaultFakeHostID {
			t.Fatalf("failure screenshot target metadata = %+v", artifact)
		}
		scenarioResult, found, readErr := readScenarioResultDocument(filepath.Join(evidence, "scenario-result.json"))
		if readErr != nil || !found {
			t.Fatalf("failure Scenario Result = found %v err %v", found, readErr)
		}
		if scenarioResult.Result.Status != resultStatusFailed || !strings.Contains(scenarioResult.Result.Summary, "script.execute failed before Scenario Result") {
			t.Fatalf("failure Scenario Result = %+v", scenarioResult.Result)
		}
	})

	t.Run("broker error honors screenshot opt out", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = failingScreenshotSessionBroker{message: "script.execute failed before Scenario Result"}

		_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "script.execute failed before Scenario Result") {
			t.Fatalf("Fresh Run error = %v, want script.execute failure", err)
		}
		evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
		bundle := readEvidenceBundle(t, evidence)
		if len(bundle.VisualEvidence) != 0 {
			t.Fatalf("failure Visual Evidence = %+v, want screenshot opt-out honored", bundle.VisualEvidence)
		}
		if _, statErr := os.Stat(filepath.Join(evidence, "screenshots", "failure-desktop.png")); !os.IsNotExist(statErr) {
			t.Fatalf("failure screenshot opt-out stat = %v, want missing", statErr)
		}
	})

	t.Run("validator failure drives failed status", func(t *testing.T) {
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
		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, defaultRunRuntime()).Run()
		if err != nil {
			t.Fatalf("Fresh Run returned error: %v", err)
		}
		if outcome.Result.Status != resultStatusFailed {
			t.Fatalf("Fresh Run status = %q, want failed", outcome.Result.Status)
		}
		bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", outcome.RunID))
		if bundle.Status != resultStatusFailed || len(bundle.Validators) != 1 || bundle.Validators[0].Status != resultStatusFailed {
			t.Fatalf("Fresh Run evidence validators = %+v status %q", bundle.Validators, bundle.Status)
		}
	})

	t.Run("keep on failure retains Host Lock and run state", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: resultStatusFailed, Summary: "fake failure"}}

		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterSuccess}, runtime).Run()
		if err != nil {
			t.Fatalf("Fresh Run returned error: %v", err)
		}
		if outcome.StopPolicy != "kept" || len(outcome.FollowUpCommands) != 3 {
			t.Fatalf("Fresh Run Stop Policy = %q follow-ups %#v, want kept with follow-ups", outcome.StopPolicy, outcome.FollowUpCommands)
		}
		if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", outcome.RunID)); err != nil {
			t.Fatalf("kept Fresh Run state missing: %v", err)
		}
		lockBytes, err := os.ReadFile(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock"))
		if err != nil {
			t.Fatalf("kept Fresh Run Host Lock missing: %v", err)
		}
		if !strings.Contains(string(lockBytes), "keptRun: "+outcome.RunID) {
			t.Fatalf("kept Fresh Run Host Lock content = %q", string(lockBytes))
		}
	})
}

func TestFreshRunLifecycleAcceptsCompletedScenarioAfterBrokerFailure(t *testing.T) {
	dir := t.TempDir()
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload: {}
    expectedOutputs:
      files:
        - "outputs/parity.json"
        - "outputs/smoke.ma"
      scenarioResult: "outputs/result.json"
    validators:
      - type: scenarioResultStatus
        status: passed
      - type: outputExists
        path: "outputs/parity.json"
      - type: outputExists
        path: "outputs/smoke.ma"
      - type: numericApprox
        path: "outputs/parity.json"
        jsonPath: "$.maxAbsDiff"
        equals: 0
        tolerance: 0.001
    evidence:
      screenshots:
        enabled: true
`)
	runtime := defaultRunRuntime()
	runtime.Broker = brokerFailureAfterScenarioCompletion{
		result: `{"status":"passed","summary":"Scenario completed before broker timeout"}` + "\n",
		files: map[string]string{
			"outputs/parity.json": `{"maxAbsDiff":0.000000035}` + "\n",
			"outputs/smoke.ma":    "// saved scene\n",
		},
		message: "ssh command timed out after 10m30s",
	}

	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "status: passed") {
		t.Fatalf("run output missing passed status:\n%s", stdout.String())
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Status != resultStatusPassed {
		t.Fatalf("recovered Evidence Bundle status = %q, want passed", bundle.Status)
	}
	requireOutputArtifact(t, bundle, "outputs/parity.json")
	requireOutputArtifact(t, bundle, "outputs/smoke.ma")
	if len(bundle.VisualEvidence) != 0 {
		t.Fatalf("recovered Evidence Bundle Visual Evidence = %+v, want none", bundle.VisualEvidence)
	}
	if len(bundle.Validators) != 4 {
		t.Fatalf("validator count = %d, want 4: %+v", len(bundle.Validators), bundle.Validators)
	}
	for _, result := range bundle.Validators {
		if result.Status != resultStatusPassed {
			t.Fatalf("validator = %+v, want passed", result)
		}
	}
	events, err := os.ReadFile(filepath.Join(evidence, "events.jsonl"))
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if !strings.Contains(string(events), "run.recovered-after-broker-failure") {
		t.Fatalf("events missing broker recovery marker:\n%s", string(events))
	}
}

func TestFreshRunLifecycleCapturesVisualEvidenceAfterKnownBrokerResponseFailure(t *testing.T) {
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
    validators:
      - type: scenarioResultStatus
        status: passed
      - type: visualEvidence
`)
	runtime := defaultRunRuntime()
	runtime.Broker = brokerFailureAfterScenarioCompletion{
		result:                             `{"status":"passed","summary":"Scenario completed before broker response failed"}` + "\n",
		message:                            "adapter-approved response failure",
		captureVisualEvidenceAfterRecovery: true,
	}

	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	bundle := readEvidenceBundle(t, onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall")))
	if len(bundle.VisualEvidence) != 1 || bundle.VisualEvidence[0].Kind != "screenshot" {
		t.Fatalf("recovered Visual Evidence = %+v, want one screenshot", bundle.VisualEvidence)
	}
	for _, result := range bundle.Validators {
		if result.Status != resultStatusPassed {
			t.Fatalf("validator = %+v, want passed", result)
		}
	}
}

func TestFreshRunLifecycleRejectsInvalidScenarioCompletionAfterBrokerFailure(t *testing.T) {
	tests := []struct {
		name       string
		result     string
		files      map[string]string
		wantStatus string
		wantError  bool
	}{
		{
			name:       "missing Scenario Result",
			files:      map[string]string{"outputs/parity.json": `{"maxAbsDiff":0}` + "\n"},
			wantStatus: resultStatusFailed,
			wantError:  true,
		},
		{
			name:       "malformed Scenario Result",
			result:     `{"status":"passed"`,
			files:      map[string]string{"outputs/parity.json": `{"maxAbsDiff":0}` + "\n"},
			wantStatus: resultStatusFailed,
			wantError:  true,
		},
		{
			name:       "failed Scenario Result",
			result:     `{"status":"failed","summary":"Scenario assertion failed"}` + "\n",
			files:      map[string]string{"outputs/parity.json": `{"maxAbsDiff":0}` + "\n"},
			wantStatus: resultStatusFailed,
			wantError:  true,
		},
		{
			name:       "Validator failure",
			result:     `{"status":"passed","summary":"Scenario completed"}` + "\n",
			files:      map[string]string{"outputs/parity.json": `{"maxAbsDiff":1}` + "\n"},
			wantStatus: resultStatusFailed,
			wantError:  false,
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
      files:
        - "outputs/parity.json"
      scenarioResult: "outputs/result.json"
    validators:
      - type: scenarioResultStatus
        status: passed
      - type: numericApprox
        path: "outputs/parity.json"
        jsonPath: "$.maxAbsDiff"
        equals: 0
        tolerance: 0.001
    evidence:
      screenshots:
        enabled: true
`)
			runtime := defaultRunRuntime()
			runtime.Broker = brokerFailureAfterScenarioCompletion{
				result:  tt.result,
				files:   tt.files,
				message: "ssh command timed out after 10m30s",
			}

			outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "ssh command timed out") {
					t.Fatalf("Fresh Run error = %v, want broker timeout", err)
				}
			} else if err != nil {
				t.Fatalf("Fresh Run returned error: %v", err)
			}
			if !tt.wantError && outcome.Result.Status != tt.wantStatus {
				t.Fatalf("Fresh Run status = %q, want %q", outcome.Result.Status, tt.wantStatus)
			}
			evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
			bundle := readEvidenceBundle(t, evidence)
			if bundle.Status != tt.wantStatus {
				t.Fatalf("Evidence Bundle status = %q, want %q", bundle.Status, tt.wantStatus)
			}
			if tt.name == "Validator failure" {
				if len(bundle.Validators) != 2 || bundle.Validators[1].Status != resultStatusFailed {
					t.Fatalf("validator metadata = %+v, want numericApprox failure", bundle.Validators)
				}
				requireVisualEvidenceArtifact(t, bundle, "screenshots/failure-desktop.png")
			}
		})
	}
}

func TestFreshRunSessionLifecycleRecordsIdentityAndStopPolicy(t *testing.T) {
	tests := []struct {
		name             string
		stopAfter        string
		brokerResult     ScenarioResult
		wantStopPolicy   string
		wantStoppedEvent bool
	}{
		{
			name:             "stopped run stops the Maya UI Session",
			stopAfter:        stopAfterAlways,
			brokerResult:     ScenarioResult{Status: resultStatusPassed, Summary: "fake Scenario completed"},
			wantStopPolicy:   "stopped",
			wantStoppedEvent: true,
		},
		{
			name:             "kept run retains the Maya UI Session",
			stopAfter:        stopAfterNever,
			brokerResult:     ScenarioResult{Status: resultStatusPassed, Summary: "fake Scenario completed"},
			wantStopPolicy:   "kept",
			wantStoppedEvent: false,
		},
		{
			name:             "keep on failure retains the Maya UI Session",
			stopAfter:        stopAfterSuccess,
			brokerResult:     ScenarioResult{Status: resultStatusFailed, Summary: "fake failure"},
			wantStopPolicy:   "kept",
			wantStoppedEvent: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			runtime := defaultRunRuntime()
			runtime.Broker = fakeSessionBroker{Result: tt.brokerResult}

			outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: tt.stopAfter}, runtime).Run()
			if err != nil {
				t.Fatalf("Fresh Run returned error: %v", err)
			}
			if outcome.StopPolicy != tt.wantStopPolicy {
				t.Fatalf("Fresh Run Stop Policy = %q, want %q", outcome.StopPolicy, tt.wantStopPolicy)
			}
			evidence := filepath.Join(dir, "artifacts", "maya-stall", outcome.RunID)
			bundle := readEvidenceBundle(t, evidence)
			wantSessionID := "fake-" + outcome.RunID
			if bundle.BrokerSession == nil || bundle.BrokerSession.BrokerAdapter != "fake" || bundle.BrokerSession.SessionID != wantSessionID {
				t.Fatalf("Evidence Bundle broker session = %+v, want fake %q", bundle.BrokerSession, wantSessionID)
			}
			manifest := readRunManifestFile(t, filepath.Join(evidence, "manifest.json"))
			if manifest.BrokerSession == nil || manifest.BrokerSession.SessionID != wantSessionID {
				t.Fatalf("manifest broker session = %+v, want %q", manifest.BrokerSession, wantSessionID)
			}
			events, err := os.ReadFile(filepath.Join(evidence, "events.jsonl"))
			if err != nil {
				t.Fatalf("read evidence events: %v", err)
			}
			if !eventRecordsContain(readEventRecords(t, filepath.Join(evidence, "events.jsonl")), "broker.session.fresh", wantSessionID) {
				t.Fatalf("evidence events missing fresh session identity:\n%s", string(events))
			}
			hasStopped := strings.Contains(string(events), `"broker.session.stopped"`)
			if hasStopped != tt.wantStoppedEvent {
				t.Fatalf("evidence events stopped marker = %v, want %v:\n%s", hasStopped, tt.wantStoppedEvent, string(events))
			}
			if tt.wantStopPolicy == "kept" {
				record := readRunRetentionRecordFile(t, filepath.Join(dir, ".maya-stall", "state", "runs", outcome.RunID, "run-record.json"))
				if record.RemoteSession.SessionID != wantSessionID {
					t.Fatalf("kept run retained session = %q, want the Fresh Run session %q", record.RemoteSession.SessionID, wantSessionID)
				}
			}
		})
	}
}

func TestFreshRunDoesNotReuseSessionAcrossConsecutiveRuns(t *testing.T) {
	dir := writeRunConfigFixture(t)
	sessionIDs := make(map[string]bool)
	for attempt := 0; attempt < 2; attempt++ {
		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, defaultRunRuntime()).Run()
		if err != nil {
			t.Fatalf("Fresh Run %d returned error: %v", attempt+1, err)
		}
		bundle := readEvidenceBundle(t, filepath.Join(dir, "artifacts", "maya-stall", outcome.RunID))
		if bundle.BrokerSession == nil || bundle.BrokerSession.SessionID == "" {
			t.Fatalf("Fresh Run %d Evidence Bundle missing broker session identity: %+v", attempt+1, bundle.BrokerSession)
		}
		if sessionIDs[bundle.BrokerSession.SessionID] {
			t.Fatalf("Fresh Run reused Maya UI Session %q across consecutive runs", bundle.BrokerSession.SessionID)
		}
		sessionIDs[bundle.BrokerSession.SessionID] = true
	}
}

func TestFreshRunFailsWhenSessionLifecycleFails(t *testing.T) {
	t.Run("start fresh session failure fails the run with evidence", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = startFailingSessionBroker{message: "no interactive Maya UI Session available"}

		_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "no interactive Maya UI Session available") {
			t.Fatalf("Fresh Run error = %v, want session start failure", err)
		}
		evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
		bundle := readEvidenceBundle(t, evidence)
		if bundle.Status != resultStatusFailed || bundle.BrokerSession != nil {
			t.Fatalf("failure Evidence Bundle = status %q session %+v, want failed without session", bundle.Status, bundle.BrokerSession)
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !os.IsNotExist(statErr) {
			t.Fatalf("session-start-failure Host Lock = %v, want missing", statErr)
		}
	})

	t.Run("stop session failure fails the run", func(t *testing.T) {
		dir := writeRunConfigFixture(t)
		runtime := defaultRunRuntime()
		runtime.Broker = stopFailingSessionBroker{
			fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "fake Scenario completed"}},
			message:           "gg_mayasessiond stop failed",
		}

		outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
		if err == nil || !strings.Contains(err.Error(), "stop Maya UI Session") || !strings.Contains(err.Error(), "gg_mayasessiond stop failed") {
			t.Fatalf("Fresh Run error = %v, want stop session failure", err)
		}
		evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
		bundle := readEvidenceBundle(t, evidence)
		if bundle.Status != resultStatusFailed || bundle.BrokerSession == nil || bundle.Failure == nil || bundle.Failure.CleanupState != "failed" {
			t.Fatalf("stop-failure Evidence Bundle = %+v, want unresolved terminal failure", bundle)
		}
		if outcome.StopPolicy != "unresolved" || len(outcome.FollowUpCommands) != 0 {
			t.Fatalf("stop-failure outcome = %+v, want unresolved without an unsafe stop follow-up", outcome)
		}
		events, readErr := os.ReadFile(filepath.Join(evidence, evidenceEventsFileName))
		if readErr != nil || !strings.Contains(string(events), `"event":"run.failed"`) {
			t.Fatalf("stop-failure terminal event = %q err %v", string(events), readErr)
		}
		requireSequencedRunEvents(t, filepath.Join(evidence, evidenceEventsFileName))
	})
}

func TestFreshRunStopsPartiallyStartedSession(t *testing.T) {
	dir := writeRunConfigFixture(t)
	runtime := defaultRunRuntime()
	runtime.Broker = partialStartFailingSessionBroker{message: "fresh session readiness failed"}

	_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err == nil || !strings.Contains(err.Error(), "fresh session readiness failed") {
		t.Fatalf("Fresh Run error = %v, want partial session start failure", err)
	}
	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.BrokerSession == nil || bundle.BrokerSession.SessionID != "fake-partial-session" {
		t.Fatalf("partial-start Evidence Bundle broker session = %+v", bundle.BrokerSession)
	}
	events, readErr := os.ReadFile(filepath.Join(evidence, "events.jsonl"))
	if readErr != nil {
		t.Fatalf("read partial-start events: %v", readErr)
	}
	if !eventRecordsContain(readEventRecords(t, filepath.Join(evidence, "events.jsonl")), "broker.session.stopped", "fake-partial-session") {
		t.Fatalf("partial-start events missing stopped session:\n%s", string(events))
	}
}

func eventRecordsContain(records []map[string]any, eventName string, detail string) bool {
	for _, record := range records {
		if record["event"] == eventName && record["detail"] == detail {
			return true
		}
	}
	return false
}

func TestFreshRunRejectsRetainedSessionIdentityChange(t *testing.T) {
	dir := writeRunConfigFixture(t)
	runtime := defaultRunRuntime()
	runtime.Broker = identityChangingRetentionBroker{
		fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "fake Scenario completed"}},
	}

	outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterNever}, runtime).Run()
	if err == nil || !strings.Contains(err.Error(), "retained a different Maya UI Session") {
		t.Fatalf("Fresh Run error = %v, want retained session identity mismatch", err)
	}
	if outcome.RunID == "" || outcome.StopPolicy != "stopped" || outcome.Result.Status != resultStatusFailed || len(outcome.FollowUpCommands) != 0 {
		t.Fatalf("identity-mismatch outcome = %+v, want terminal stopped failure", outcome)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !os.IsNotExist(statErr) {
		t.Fatalf("identity-mismatch Host Lock = %v, want released", statErr)
	}
}

func TestFreshRunDeferredStopSuccessCleansRunState(t *testing.T) {
	dir := writeRunConfigFixture(t)
	broker := &retryingStopSessionBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "passed"}}}
	runtime := defaultRunRuntime()
	runtime.Broker = broker

	_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err == nil || !strings.Contains(err.Error(), "transient stop failure") {
		t.Fatalf("Fresh Run error = %v, want primary stop failure", err)
	}
	if broker.stopCalls != 2 || broker.cleanupCalls != 1 {
		t.Fatalf("deferred cleanup calls = stop %d cleanup %d, want 2/1", broker.stopCalls, broker.cleanupCalls)
	}
	evidenceDir := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runID := filepath.Base(evidenceDir)
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !os.IsNotExist(statErr) {
		t.Fatalf("deferred-stop run state = %v, want removed", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !os.IsNotExist(statErr) {
		t.Fatalf("deferred-stop Host Lock = %v, want released", statErr)
	}
}

func TestFreshRunDeferredStopRetainsStateWhenRemoteCleanupFails(t *testing.T) {
	dir := writeRunConfigFixture(t)
	broker := &retryingStopSessionBroker{
		fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "passed"}},
		cleanupErr:        fmt.Errorf("remote cleanup failed"),
	}
	runtime := defaultRunRuntime()
	runtime.Broker = broker

	outcome, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err == nil || !strings.Contains(err.Error(), "remote cleanup failed") {
		t.Fatalf("Fresh Run error = %v, want remote cleanup failure", err)
	}
	evidenceDir := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidenceDir)
	if outcome.StopPolicy != "unresolved" || bundle.Failure == nil || bundle.Failure.CleanupState != "failed" {
		t.Fatalf("cleanup-failure terminal state = outcome %+v failure %+v", outcome, bundle.Failure)
	}
	runID := filepath.Base(evidenceDir)
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")); statErr != nil {
		t.Fatalf("cleanup-failure Run Record missing: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); statErr != nil {
		t.Fatalf("cleanup-failure Host Lock missing: %v", statErr)
	}
}

func TestFreshRunStopPreservesRecoverableStateWhenEvidenceSyncFails(t *testing.T) {
	dir := writeRunConfigFixture(t)
	broker := &syncFailingStopSessionBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "passed"}}}
	runtime := defaultRunRuntime()
	runtime.Broker = broker

	_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err == nil || !strings.Contains(err.Error(), "read run events after stopping run") {
		t.Fatalf("Fresh Run error = %v, want evidence sync failure", err)
	}
	if broker.cleanupCalls != 0 {
		t.Fatalf("cleanup calls = %d, want 0 before durable recovery", broker.cleanupCalls)
	}
	evidenceDir := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runID := filepath.Base(evidenceDir)
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); statErr != nil {
		t.Fatalf("sync-failure Run State missing: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); statErr != nil {
		t.Fatalf("sync-failure Host Lock missing: %v", statErr)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("stop sync-failure recovery exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !os.IsNotExist(statErr) {
		t.Fatalf("sync-failure Run State after stop = %v, want removed", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); !os.IsNotExist(statErr) {
		t.Fatalf("sync-failure Host Lock after stop = %v, want removed", statErr)
	}
}

func TestFreshRunSettleRetainsStateWhenRemoteCleanupFails(t *testing.T) {
	dir := writeRunConfigFixture(t)
	broker := &settleCleanupFailingBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "passed"}}}
	runtime := defaultRunRuntime()
	runtime.Broker = broker

	_, err := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime).Run()
	if err == nil || !strings.Contains(err.Error(), "settle cleanup failed") {
		t.Fatalf("Fresh Run error = %v, want settle cleanup failure", err)
	}
	evidenceDir := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	runID := filepath.Base(evidenceDir)
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")); statErr != nil {
		t.Fatalf("settle cleanup-failure Run Record missing: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); statErr != nil {
		t.Fatalf("settle cleanup-failure Host Lock missing: %v", statErr)
	}
}

func TestGGMayaSessiondFreshRunFailsClosedWhenInheritedSessionStopFails(t *testing.T) {
	dir := t.TempDir()
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		sessiondStatusFixture("session-inherited"),
		`@inherited-stop-fail:inherited session stop failed`,
	})
	eventsPath := filepath.Join(dir, "events.jsonl")
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

	_, err := broker.StartFreshSession(runContext{EventsPath: eventsPath}, scenarioConfig{})
	if err == nil || !strings.Contains(err.Error(), "stop inherited gg_mayasessiond Maya UI Session") || !strings.Contains(err.Error(), "inherited session stop failed") {
		t.Fatalf("StartFreshSession error = %v, want inherited session stop failure", err)
	}
}

func TestGGMayaSessiondStopSessionFailsClosedWithoutCurrentIdentity(t *testing.T) {
	dir := t.TempDir()
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`{"has_state":true,"derived_status":"running","state":{"status":"running","maya_alive":true,"mcp_alive":true,"call_server_ready":true}}`,
	})
	eventsPath := filepath.Join(dir, "events.jsonl")
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

	err := broker.StopSession(runContext{EventsPath: eventsPath}, brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond", SessionID: "session-owned"})
	if err == nil || !strings.Contains(err.Error(), "did not report a session id") {
		t.Fatalf("StopSession error = %v, want missing current session identity", err)
	}
}

func TestGGMayaSessiondRefusesToRetainInactiveOwnedSession(t *testing.T) {
	dir := t.TempDir()
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`{"has_state":true,"derived_status":"stopped","state":{"status":"stopped","session_id":"session-owned","maya_alive":false,"mcp_alive":false}}`,
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
	manifest := runManifest{RunID: "run-owned", BrokerSession: &brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond", SessionID: "session-owned"}}

	_, err := broker.RetainRun(runContext{}, manifest, "keep-on-failure")
	if err == nil || !strings.Contains(err.Error(), "is not active") {
		t.Fatalf("RetainRun error = %v, want inactive session rejection", err)
	}
}

func TestGGMayaSessiondFreshRunReturnsPartialIdentityWhenTaskRestartIsAmbiguous(t *testing.T) {
	dir := t.TempDir()
	sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{
		`{"has_state":false,"derived_status":"stopped","state":{"status":"stopped"}}`,
		`@fresh-fail:ssh connection lost after task start`,
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

	identity, err := broker.StartFreshSession(runContext{EventsPath: filepath.Join(dir, "events.jsonl")}, scenarioConfig{})
	if err == nil || !strings.Contains(err.Error(), "ssh connection lost after task start") {
		t.Fatalf("StartFreshSession error = %v, want ambiguous task restart failure", err)
	}
	if identity.BrokerAdapter != "gg-mayasessiond" {
		t.Fatalf("partial identity = %+v, want gg-mayasessiond ownership marker", identity)
	}
}

func TestGGMayaSessiondFreshSessionReadinessRequiresRunningStatus(t *testing.T) {
	status := sessiondStatusResult{DerivedStatus: "starting", HasState: true}
	status.State.Status = "starting"
	status.State.SessionID = "session-fresh"
	status.State.MayaAlive = true
	status.State.MCPAlive = true
	status.State.CallServerReady = true
	if sessiondFreshSessionReady(status) {
		t.Fatal("starting gg_mayasessiond session reported ready")
	}
	status.DerivedStatus = "failed"
	status.State.Status = "failed"
	if sessiondFreshSessionReady(status) {
		t.Fatal("failed gg_mayasessiond session reported ready")
	}
	status.DerivedStatus = "running"
	status.State.Status = "running"
	if !sessiondFreshSessionReady(status) {
		t.Fatal("running gg_mayasessiond session did not report ready")
	}
}

func TestFreshRunAwareFakeSSHPreludeHandlesSceneInfoReadinessProbe(t *testing.T) {
	prelude := freshRunAwareFakeSSHPrelude(t.TempDir(), "fake-ssh", "", "", true)
	for _, expected := range []string{"scene.info", `{"ok":true,"tool":"scene.info"}`, "viewport.capture", `"mimeType":"image/jpeg"`} {
		if !strings.Contains(prelude, expected) {
			t.Fatalf("fake SSH readiness prelude missing %q", expected)
		}
	}
}

func TestKnownSessiondScriptExecuteResponseErrors(t *testing.T) {
	for _, message := range []string{
		"Error calling tool 'script.execute': 'int' object has no attribute 'get'",
		"Error calling tool 'script.execute': Expecting value: line 1 column 1 (char 0)",
	} {
		if !isKnownSessiondScriptExecuteResponseError(fmt.Errorf("%s", message)) {
			t.Fatalf("known stale broker error was not recoverable: %s", message)
		}
	}
	if isKnownSessiondScriptExecuteResponseError(fmt.Errorf("script.execute path is not allowed")) {
		t.Fatal("unrelated script.execute failure was classified as recoverable")
	}
	if isKnownSessiondScriptExecuteResponseError(fmt.Errorf("Expecting value: line 1 column 1 (char 5)")) {
		t.Fatal("non-empty malformed response was classified as recoverable")
	}
}

func readRunManifestFile(t *testing.T, path string) runManifest {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read run manifest: %v", err)
	}
	var manifest runManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		t.Fatalf("parse run manifest: %v", err)
	}
	return manifest
}

func readRunRetentionRecordFile(t *testing.T, path string) runRetentionRecord {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read run retention record: %v", err)
	}
	var record runRetentionRecord
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse run retention record: %v", err)
	}
	return record
}

type startFailingSessionBroker struct {
	fakeSessionLifecycle
	message string
}

type emptyIdentitySessionBroker struct {
	fakeSessionLifecycle
}

func (broker emptyIdentitySessionBroker) StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error) {
	return brokerSessionIdentity{}, nil
}

func (broker emptyIdentitySessionBroker) RunScenario(runContext, scenarioConfig) (ScenarioResult, error) {
	return ScenarioResult{}, fmt.Errorf("RunScenario must not run after empty session identity")
}

func (broker startFailingSessionBroker) StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error) {
	return brokerSessionIdentity{}, fmt.Errorf("%s", broker.message)
}

func (broker startFailingSessionBroker) RunScenario(runContext, scenarioConfig) (ScenarioResult, error) {
	return ScenarioResult{}, fmt.Errorf("RunScenario must not run when the fresh session did not start")
}

type stopFailingSessionBroker struct {
	fakeSessionBroker
	message string
}

type partialStartFailingSessionBroker struct {
	fakeSessionLifecycle
	message string
}

func (broker partialStartFailingSessionBroker) StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error) {
	return brokerSessionIdentity{BrokerAdapter: "fake", SessionID: "fake-partial-session"}, fmt.Errorf("%s", broker.message)
}

func (broker partialStartFailingSessionBroker) RunScenario(runContext, scenarioConfig) (ScenarioResult, error) {
	return ScenarioResult{}, fmt.Errorf("RunScenario must not run after partial session start failure")
}

type identityChangingRetentionBroker struct {
	fakeSessionBroker
}

func (broker identityChangingRetentionBroker) RetainRun(runContext, runManifest, string) (retainedSessionRecord, error) {
	return retainedSessionRecord{BrokerAdapter: "fake", SessionID: "fake-other-session", Status: "running"}, nil
}

func (broker stopFailingSessionBroker) StopSession(runContext, brokerSessionIdentity) error {
	return fmt.Errorf("%s", broker.message)
}

type freshRunLifecycleRecorder struct {
	events []string
}

func (recorder *freshRunLifecycleRecorder) add(event string) {
	recorder.events = append(recorder.events, event)
}

func (recorder *freshRunLifecycleRecorder) requireOrder(t *testing.T, want ...string) {
	t.Helper()
	if len(recorder.events) != len(want) {
		t.Fatalf("lifecycle events = %#v, want %#v", recorder.events, want)
	}
	for index, event := range want {
		if recorder.events[index] != event {
			t.Fatalf("lifecycle events = %#v, want %#v", recorder.events, want)
		}
	}
}

type recordingRunHost struct {
	recorder *freshRunLifecycleRecorder
}

type stageFailingRunHost struct {
	message string
}

func (host stageFailingRunHost) StagePayload(runContext, []manifestPayload) error {
	return fmt.Errorf("%s", host.message)
}

func (host recordingRunHost) StagePayload(runContext, []manifestPayload) error {
	host.recorder.add("setup.stage-payload")
	return nil
}

func (host recordingRunHost) CollectArtifacts(runContext, scenarioContract) error {
	host.recorder.add("settle.collect-artifacts")
	return nil
}

type recordingSessionBroker struct {
	recorder *freshRunLifecycleRecorder
	result   ScenarioResult
}

func (broker recordingSessionBroker) VerifyMayaBuild(runContext, brokerSessionIdentity, string) error {
	broker.recorder.add("execute.verify-maya-build")
	return nil
}

func (broker recordingSessionBroker) StartFreshSession(context runContext, scenario scenarioConfig) (brokerSessionIdentity, error) {
	broker.recorder.add("execute.start-session")
	return fakeSessionLifecycle{}.StartFreshSession(context, scenario)
}

func (broker recordingSessionBroker) StopSession(context runContext, session brokerSessionIdentity) error {
	broker.recorder.add("settle.stop-session")
	return fakeSessionLifecycle{}.StopSession(context, session)
}

func (broker recordingSessionBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	broker.recorder.add("execute.run-scenario")
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("recording broker ran Scenario\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", broker.result.Status); err != nil {
		return ScenarioResult{}, err
	}
	return broker.result, nil
}

func writeFailureScreenshotRunConfigFixture(t *testing.T) string {
	t.Helper()
	dir := writeRunConfigFixture(t)
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
        enabled: true
`)
	return dir
}

type failingSessionBroker struct {
	fakeSessionLifecycle
	message string
}

type failingRetainableSessionBroker struct {
	fakeSessionBroker
	message string
}

type failingExecutionRetentionBroker struct {
	failingRetainableSessionBroker
}

type retryingStopSessionBroker struct {
	fakeSessionBroker
	stopCalls    int
	cleanupCalls int
	cleanupErr   error
}

type syncFailingStopSessionBroker struct {
	fakeSessionBroker
	cleanupCalls int
}

type settleCleanupFailingBroker struct {
	fakeSessionBroker
}

func (broker *settleCleanupFailingBroker) CleanupRun(runContext) error {
	return fmt.Errorf("settle cleanup failed")
}

func (broker *syncFailingStopSessionBroker) StopSession(context runContext, session brokerSessionIdentity) error {
	if err := broker.fakeSessionBroker.StopSession(context, session); err != nil {
		return err
	}
	return os.Remove(context.EventsPath)
}

func (broker *syncFailingStopSessionBroker) CleanupRun(runContext) error {
	broker.cleanupCalls++
	return nil
}

func (broker *retryingStopSessionBroker) StopSession(context runContext, session brokerSessionIdentity) error {
	broker.stopCalls++
	if broker.stopCalls == 1 {
		return fmt.Errorf("transient stop failure")
	}
	return broker.fakeSessionBroker.StopSession(context, session)
}

func (broker *retryingStopSessionBroker) CleanupRun(runContext) error {
	broker.cleanupCalls++
	return broker.cleanupErr
}

func (broker failingExecutionRetentionBroker) RetainRun(runContext, runManifest, string) (retainedSessionRecord, error) {
	return retainedSessionRecord{}, fmt.Errorf("retention failed")
}

func (broker failingRetainableSessionBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{}, fmt.Errorf("%s", broker.message)
}

func (broker failingSessionBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{}, fmt.Errorf("%s", broker.message)
}

type failingScreenshotSessionBroker struct {
	fakeSessionLifecycle
	message string
}

func (broker failingScreenshotSessionBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	return failingSessionBroker(broker).RunScenario(context, scenario)
}

func (failingScreenshotSessionBroker) CaptureScreenshot(context runContext, request screenshotRequest) (visualEvidenceArtifact, error) {
	return fakeSessionBroker{}.CaptureScreenshot(context, request)
}

type brokerFailureAfterScenarioCompletion struct {
	fakeSessionLifecycle
	result                             string
	files                              map[string]string
	message                            string
	captureVisualEvidenceAfterRecovery bool
}

func (broker brokerFailureAfterScenarioCompletion) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("Scenario wrote outputs before broker failure\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if broker.result != "" {
		if err := os.MkdirAll(filepath.Dir(context.ScenarioResultPath), 0o755); err != nil {
			return ScenarioResult{}, err
		}
		if err := os.WriteFile(context.ScenarioResultPath, []byte(broker.result), 0o644); err != nil {
			return ScenarioResult{}, err
		}
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
	return ScenarioResult{}, fmt.Errorf("%s", broker.message)
}

func (brokerFailureAfterScenarioCompletion) CaptureScreenshot(context runContext, request screenshotRequest) (visualEvidenceArtifact, error) {
	return fakeSessionBroker{}.CaptureScreenshot(context, request)
}

func (broker brokerFailureAfterScenarioCompletion) CaptureVisualEvidenceAfterRecoveredScenario(error) bool {
	return broker.captureVisualEvidenceAfterRecovery
}

func requireOutputArtifact(t *testing.T, bundle evidenceBundle, path string) {
	t.Helper()
	for _, output := range bundle.Outputs {
		if output.Path == path {
			return
		}
	}
	t.Fatalf("Evidence Bundle missing output %q: %+v", path, bundle.Outputs)
}

func requireVisualEvidenceArtifact(t *testing.T, bundle evidenceBundle, path string) {
	t.Helper()
	for _, artifact := range bundle.VisualEvidence {
		if artifact.Path == path {
			return
		}
	}
	t.Fatalf("Evidence Bundle missing Visual Evidence %q: %+v", path, bundle.VisualEvidence)
}
