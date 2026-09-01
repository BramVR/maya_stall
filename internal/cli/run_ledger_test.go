package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func TestRetainedRunLedgerEventMetadataStreamsInput(t *testing.T) {
	events := strings.Join([]string{
		`{"sequence":1,"type":"scenario.started"}`,
		`{"sequence":2,"type":"run-ledger.events.truncated","details":{"omittedCount":3}}`,
	}, "\n") + "\n"

	count, omitted, truncated, err := retainedRunLedgerEventMetadata(iotest.OneByteReader(strings.NewReader(events)))
	if err != nil {
		t.Fatalf("read retained event metadata: %v", err)
	}
	if count != 2 || omitted != 3 || !truncated {
		t.Fatalf("retained event metadata = count %d, omitted %d, truncated %t", count, omitted, truncated)
	}
}

func TestFreshRunDoesNotReuseInterruptedLedgerRunID(t *testing.T) {
	dir := writeRunConfigFixture(t)
	runID := "20260720T120000.000000000Z"
	partialPath := filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName)
	if err := os.MkdirAll(filepath.Dir(partialPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partialPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, defaultRunRuntime()).(*freshRunLifecycle)

	err := run.accept()
	if err == nil || !strings.Contains(err.Error(), "assigned Run ID "+runID+" already exists") {
		t.Fatalf("accept with occupied Run ID error = %v", err)
	}
	if content, readErr := os.ReadFile(partialPath); readErr != nil || string(content) != "partial" {
		t.Fatalf("interrupted ledger changed: content %q, error %v", content, readErr)
	}
}

func TestRunLedgerStoreRejectsSymlinkedArtifactPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	runID := "20260720T120000.000000000Z"
	run := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, defaultRunRuntime()).(*freshRunLifecycle)
	if err := run.accept(); err != nil {
		t.Fatal(err)
	}
	store := newRunLedgerStore(dir)
	eventsPath := filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName)
	externalPath := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(externalPath, []byte("external\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(eventsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalPath, eventsPath); err != nil {
		t.Fatal(err)
	}

	for name, operation := range map[string]func() error{
		"snapshot": func() error {
			_, err := store.Snapshot(runID)
			return err
		},
		"update snapshot": func() error {
			return store.UpdateSnapshot(runID, func(*runLedgerSnapshot) error { return nil })
		},
		"replace events": func() error {
			return store.ReplaceEvents(runID, []byte("replacement\n"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlinked artifact error = %v", err)
			}
		})
	}
	if content, err := os.ReadFile(externalPath); err != nil || string(content) != "external\n" {
		t.Fatalf("external artifact changed: content %q, error %v", content, err)
	}
}

func TestRunLedgerStoreExistenceChecksRejectSymlinkedParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := t.TempDir()
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, ".maya-stall")); err != nil {
		t.Fatal(err)
	}
	store := newRunLedgerStore(dir)
	for name, operation := range map[string]func() error{
		"has record": func() error {
			_, err := store.HasRecord("20260720T120000.000000000Z")
			return err
		},
		"occupied": func() error {
			_, err := store.Occupied("20260720T120000.000000000Z")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(); err == nil || !strings.Contains(err.Error(), "symlink") {
				t.Fatalf("symlinked parent error = %v", err)
			}
		})
	}
}

func TestCompletedRunRemainsInEmbeddedHistoryAfterRunStateCleanup(t *testing.T) {
	dir := writeRunConfigFixture(t)
	now := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer
	runtime := runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "done"}},
		Now:    func() time.Time { return now },
	}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !os.IsNotExist(err) {
		t.Fatalf("completed Run State = %v, want cleaned", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithRuntime([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version", runRuntime{Now: func() time.Time { return now }})
	if code != 0 {
		t.Fatalf("history exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var history struct {
		Version int `json:"version"`
		Runs    []struct {
			RunID      string `json:"runId"`
			Scenario   string `json:"scenario"`
			Host       string `json:"host"`
			State      string `json:"state"`
			Status     string `json:"status"`
			AcceptedAt string `json:"acceptedAt"`
			Events     string `json:"events"`
			Log        string `json:"log"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
		t.Fatalf("parse history JSON: %v\n%s", err, stdout.String())
	}
	if history.Version != 1 || len(history.Runs) != 1 {
		t.Fatalf("history = %+v, want one versioned run", history)
	}
	got := history.Runs[0]
	if got.RunID != runID || got.Scenario != "smoke" || got.Host != defaultFakeHostID || got.State != "completed" || got.Status != resultStatusPassed {
		t.Fatalf("history run = %+v", got)
	}
	if got.AcceptedAt != now.Format(time.RFC3339Nano) || got.Events != "events.jsonl" || got.Log != "logs/session.log" {
		t.Fatalf("history durable fields = %+v", got)
	}
}

func TestCorruptHistoricalLedgerRecordDoesNotBlockNewRun(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("first run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	firstRunID := outputValue(t, stdout.String(), "run")
	if err := os.WriteFile(filepath.Join(runLedgerDir(dir, firstRunID), "run.json"), []byte("{truncated"), 0o600); err != nil {
		t.Fatalf("corrupt historical ledger record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("new run blocked by corrupt history; exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func TestEmbeddedRunLedgerClassifiesTerminalAndRetainedStates(t *testing.T) {
	base := time.Date(2026, time.July, 14, 1, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		args      []string
		at        time.Time
		broker    sessionBroker
		wantCode  int
		wantState string
	}{
		{name: "completed", args: []string{"run", "smoke"}, at: base, broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}, wantCode: 0, wantState: "completed"},
		{name: "submission failed", args: []string{"run", "smoke", "--unknown"}, at: base.Add(30 * time.Minute), broker: fakeSessionBroker{}, wantCode: 1, wantState: "failed"},
		{name: "failed", args: []string{"run", "missing"}, at: base.Add(time.Hour), broker: fakeSessionBroker{}, wantCode: 1, wantState: "failed"},
		{name: "cleanup failed", args: []string{"run", "smoke"}, at: base.Add(2 * time.Hour), broker: ledgerCleanupFailingBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}}, wantCode: 1, wantState: "cleanup-failed"},
		{name: "kept", args: []string{"run", "--stop-after", "never", "smoke"}, at: base.Add(3 * time.Hour), broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}, wantCode: 0, wantState: "kept"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			var stdout, stderr bytes.Buffer
			code := RunWithRuntime(test.args, &stdout, &stderr, dir, "test-version", runRuntime{
				Broker: test.broker,
				Now:    func() time.Time { return test.at },
			})
			if code != test.wantCode {
				t.Fatalf("run exit code = %d, want %d; stdout: %s stderr: %s", code, test.wantCode, stdout.String(), stderr.String())
			}
			runID := outputValue(t, stdout.String(), "run")
			record, err := readRunLedgerRecord(dir, runID)
			if err != nil {
				t.Fatalf("read ledger record: %v", err)
			}
			if record.State != test.wantState {
				t.Fatalf("ledger state = %q, want %q; record=%+v", record.State, test.wantState, record)
			}
			if test.wantState == "cleanup-failed" {
				stdout.Reset()
				stderr.Reset()
				if stopCode := Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version"); stopCode != 0 {
					t.Fatalf("stop cleanup-failed run exit code = %d; stdout: %s stderr: %s", stopCode, stdout.String(), stderr.String())
				}
			}
		})
	}
}

func TestStopRetriesCleanupFailedActiveRun(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: stopFailingSessionBroker{
			fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
			message:           "persistent stop failure",
		},
	})
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	if record.State != "cleanup-failed" {
		t.Fatalf("ledger state = %q, want cleanup-failed", record.State)
	}
	manifest, stateDir, found, err := readStopRunManifest(dir, runID)
	if err != nil {
		t.Fatalf("read run manifest: %v", err)
	}
	if !found {
		t.Fatal("cleanup-failed Run State missing")
	}
	retentionRecord, err := readRunRetentionRecord(dir, stateDir, manifest)
	if err != nil {
		t.Fatalf("read Run Record: %v", err)
	}
	if retentionRecord.Status != "running" || retentionRecord.RemoteSession.SessionID == "" {
		t.Fatalf("cleanup-failed Run Record = %+v, want running owned session", retentionRecord)
	}
	markHostLockControllerDead(t, filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", manifest.Host+".lock"))

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("retry stop exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry stop Run State = %v, want removed", err)
	}
}

func TestCleanupOwnershipVerificationAcceptsExpiredLeaseForSameRun(t *testing.T) {
	workRoot := t.TempDir()
	host := mayaHostConfig{ID: "alpha", WorkRoot: workRoot}
	lockDir := filepath.Join(workRoot, "state", "locks", "hosts")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		t.Fatalf("create host-side lock directory: %v", err)
	}
	runID := "20260714T120000.000000000Z"
	content := fmt.Sprintf("host: alpha\nlockToken: owned-token\nactiveRun: %s\nleaseExpiresAt: %s\n", runID, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(lockDir, "alpha.lock"), []byte(content), 0o600); err != nil {
		t.Fatalf("write expired host-side lock: %v", err)
	}
	if err := verifyHostSideLockForRun(host, runID); err == nil {
		t.Fatal("normal active verification accepted expired lease")
	}
	if err := verifyCleanupHostSideLockForRun(host, runID); err != nil {
		t.Fatalf("cleanup ownership verification rejected same expired lease: %v", err)
	}
	if err := verifyCleanupHostSideLockForRun(host, "other-run"); err == nil {
		t.Fatal("cleanup ownership verification accepted a different run")
	}
}

func TestSubmittedActiveRunStatusAndAttachUseLiveState(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	manifest, stateDir, found, err := readStopRunManifest(dir, runID)
	if err != nil || !found {
		t.Fatalf("read run state: found=%t err=%v", found, err)
	}
	record, err := readRunRetentionRecord(dir, stateDir, manifest)
	if err != nil {
		t.Fatalf("read Run Record: %v", err)
	}
	record.Status = "running"
	record.RetentionReason = ""
	if err := writeRunRetentionRecord(runContext{StateDir: stateDir}, record); err != nil {
		t.Fatalf("write active Run Record: %v", err)
	}
	if err := markHostLockActive(dir, defaultFakeHostID, runID); err != nil {
		t.Fatalf("mark active Host Lock: %v", err)
	}
	ledger, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	ledger.State = "submitted"
	if err := writeRunLedgerRecord(dir, ledger); err != nil {
		t.Fatalf("write submitted ledger: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, filepath.FromSlash(evidenceLogPath)), []byte("live active log\n"), 0o644); err != nil {
		t.Fatalf("write live log: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("active status exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"state: running", "remoteState: running", "stateDir: "} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("active status missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("active attach exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"live active log", "adapter: fake", "session: fake-" + runID} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("active attach missing %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 || !strings.Contains(stderr.String(), "still has a live controller") {
		t.Fatalf("live-controller stop exit code = %d, want refusal; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("live-controller refusal removed Run State: %v", err)
	}
}

func TestLedgerInitializationNormalizesLegacyEvents(t *testing.T) {
	dir := t.TempDir()
	runID := "20260714T120000.000000000Z"
	source := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatalf("create legacy Run State: %v", err)
	}
	if err := os.WriteFile(source, []byte("{\"event\":\"run.accepted\",\"detail\":\"smoke\"}\n"), 0o644); err != nil {
		t.Fatalf("write legacy events: %v", err)
	}
	acceptedAt := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	manifest := runManifest{RunID: runID, Scenario: "smoke", TargetProfile: "default", Host: defaultFakeHostID}
	if err := initializeRunLedger(dir, manifest, acceptedAt, source); err != nil {
		t.Fatalf("initialize ledger: %v", err)
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read initialized ledger: %v", err)
	}
	if err := syncRunLedgerArtifacts(dir, &record, defaultRunLedgerPolicy()); err != nil {
		t.Fatalf("sync legacy artifacts: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read normalized events: %v", err)
	}
	var event struct {
		Sequence  int            `json:"sequence"`
		Timestamp string         `json:"timestamp"`
		Phase     string         `json:"phase"`
		Type      string         `json:"type"`
		Stream    string         `json:"stream"`
		Details   map[string]any `json:"details"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(content), &event); err != nil {
		t.Fatalf("parse normalized event: %v", err)
	}
	if event.Sequence != 1 || event.Timestamp != acceptedAt.Format(time.RFC3339Nano) || event.Phase != "submission" || event.Type != "run.accepted" || event.Stream != "lifecycle" || event.Details["message"] != "smoke" {
		t.Fatalf("normalized event = %+v", event)
	}
}

type ledgerCleanupFailingBroker struct {
	fakeSessionBroker
}

func (ledgerCleanupFailingBroker) CleanupRun(runContext) error {
	return errors.New("cleanup unavailable")
}

func TestEmbeddedRunLedgerBoundsRetainedLogsWithExplicitMarker(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  maxLogBytes: 96\n"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	fullLog := strings.Repeat("0123456789", 40) + "TAIL\n"
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: ledgerLogBroker{log: fullLog},
	})
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	retained, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), filepath.FromSlash(runLedgerLogPath)))
	if err != nil {
		t.Fatalf("read retained log: %v", err)
	}
	if len(retained) > 96 || !bytes.HasPrefix(retained, []byte("[maya-stall: log truncated; omitted ")) || !bytes.HasSuffix(retained, []byte("TAIL\n")) {
		t.Fatalf("retained log bytes=%d:\n%s", len(retained), retained)
	}
	evidenceLog, err := os.ReadFile(filepath.Join(dir, "artifacts", "maya-stall", runID, filepath.FromSlash(evidenceLogPath)))
	if err != nil {
		t.Fatalf("read Evidence Bundle log: %v", err)
	}
	if string(evidenceLog) != fullLog {
		t.Fatalf("Evidence Bundle log was truncated: bytes=%d want=%d", len(evidenceLog), len(fullLog))
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	if !record.LogTruncated || record.LogBytes != len(retained) {
		t.Fatalf("retained log metadata = %+v", record)
	}
}

func TestAcceptedSubmissionFailureUsesConfiguredLedgerBounds(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  maxLogBytes: 96\n"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "smoke", "--" + strings.Repeat("unknown", 100)}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("submission failure exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	if !record.LogTruncated || record.LogBytes > 96 {
		t.Fatalf("submission failure ledger bounds = %+v", record)
	}
}

func TestEmbeddedRunLedgerBoundsEventsWithOrderedTruncationMarker(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  maxEvents: 4\n"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
	})
	if code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	content, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read durable events: %v", err)
	}
	lines := bytes.Split(bytes.TrimSpace(content), []byte{'\n'})
	if len(lines) != 4 {
		t.Fatalf("retained event count = %d, want 4:\n%s", len(lines), content)
	}
	previousSequence := 0
	foundMarker := false
	for index, line := range lines {
		var event struct {
			Sequence int            `json:"sequence"`
			Type     string         `json:"type"`
			Details  map[string]any `json:"details"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatalf("parse retained event %d: %v", index+1, err)
		}
		if event.Sequence <= previousSequence {
			t.Fatalf("retained event sequences not increasing: %s", content)
		}
		previousSequence = event.Sequence
		if event.Type == "run-ledger.events.truncated" {
			foundMarker = true
			if event.Details["omittedCount"] == nil {
				t.Fatalf("truncation marker missing omittedCount: %s", line)
			}
		}
	}
	if !foundMarker {
		t.Fatalf("retained events missing truncation marker:\n%s", content)
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	if !record.EventsTruncated || record.EventsOmitted < 1 || record.EventCount != 4 {
		t.Fatalf("retained event metadata = %+v", record)
	}
}

func TestEmbeddedRunLedgerRetentionRemovesExpiredHistoryOnly(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  retention: 1h\n"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	base := time.Date(2026, time.July, 14, 6, 0, 0, 0, time.UTC)
	runAt := func(at time.Time) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
			Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
			Now:    func() time.Time { return at },
		})
		if code != 0 {
			t.Fatalf("run at %s exit code = %d; stdout: %s stderr: %s", at, code, stdout.String(), stderr.String())
		}
		return outputValue(t, stdout.String(), "run")
	}
	oldRunID := runAt(base)
	oldEvidence := filepath.Join(dir, "artifacts", "maya-stall", oldRunID, evidenceBundleFileName)
	publishedRoot := filepath.Join(dir, "published")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"evidence", "publish", "--destination", publishedRoot, "--base-url", "https://evidence.example.test/maya", filepath.Dir(oldEvidence)}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("publish exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	newRunID := runAt(base.Add(2 * time.Hour))

	if _, err := os.Stat(runLedgerDir(dir, oldRunID)); !os.IsNotExist(err) {
		t.Fatalf("expired ledger run = %v, want removed", err)
	}
	for _, path := range []string{
		oldEvidence,
		filepath.Join(publishedRoot, oldRunID, "artifact-manifest.json"),
		filepath.Join(runLedgerDir(dir, newRunID), "run.json"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("retention removed required path %s: %v", path, err)
		}
	}
	stdout.Reset()
	stderr.Reset()
	code = RunWithRuntime([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version", runRuntime{Now: func() time.Time { return base.Add(2 * time.Hour) }})
	if code != 0 {
		t.Fatalf("history exit code = %d; stderr: %s", code, stderr.String())
	}
	var history runHistoryResponse
	if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
		t.Fatalf("parse history: %v", err)
	}
	if len(history.Runs) != 1 || history.Runs[0].RunID != newRunID {
		t.Fatalf("history after retention = %+v", history)
	}
}

func TestHistoryAppliesEmbeddedRunLedgerRetentionWithoutAnotherRun(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  retention: 1h\n"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	base := time.Date(2026, time.July, 14, 6, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
		Now:    func() time.Time { return base },
	})
	if code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")

	stdout.Reset()
	stderr.Reset()
	code = RunWithRuntime([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Now: func() time.Time { return base.Add(2 * time.Hour) },
	})
	if code != 0 {
		t.Fatalf("history exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var history runHistoryResponse
	if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
		t.Fatalf("parse history: %v", err)
	}
	if len(history.Runs) != 0 {
		t.Fatalf("expired history = %+v, want empty", history)
	}
	if _, err := os.Stat(runLedgerDir(dir, runID)); !os.IsNotExist(err) {
		t.Fatalf("expired ledger run = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "maya-stall", runID, evidenceBundleFileName)); err != nil {
		t.Fatalf("retention removed Evidence Bundle: %v", err)
	}
}

func TestRunLedgerRetentionRemovesCanceledRuns(t *testing.T) {
	dir := writeRunConfigFixture(t)
	base := time.Date(2026, time.July, 14, 6, 0, 0, 0, time.UTC)
	runID := "20260714T060000.000000000Z"
	run := newFreshRun(dir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, runRuntime{Now: func() time.Time { return base }}).(*freshRunLifecycle)
	if err := run.accept(); err != nil {
		t.Fatal(err)
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatal(err)
	}
	record.State = "canceled"
	record.Status = resultStatusFailed
	record.CompletedAt = base.Format(time.RFC3339Nano)
	if err := writeRunLedgerRecord(dir, record); err != nil {
		t.Fatal(err)
	}
	if err := pruneRunLedger(dir, runLedgerPolicy{Retention: time.Hour}, base.Add(2*time.Hour), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(runLedgerDir(dir, runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired canceled ledger remains: %v", err)
	}
}

func TestHistoryIgnoresIncompleteLedgerInitialization(t *testing.T) {
	dir := writeRunConfigFixture(t)
	orphanID := "20260714T050000.000000000Z"
	if err := os.MkdirAll(runLedgerDir(dir, orphanID), 0o755); err != nil {
		t.Fatalf("create interrupted ledger directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runLedgerDir(dir, orphanID), runLedgerEventsFileName), []byte("partial\n"), 0o644); err != nil {
		t.Fatalf("write interrupted ledger event: %v", err)
	}
	temporaryDir := filepath.Join(runLedgerRoot(dir), "."+orphanID+".tmp-crash")
	if err := os.MkdirAll(temporaryDir, 0o755); err != nil {
		t.Fatalf("create interrupted ledger staging directory: %v", err)
	}
	temporaryRecord := runLedgerRecord{
		Version:     runLedgerSchemaVersion,
		RunID:       orphanID,
		Scenario:    "smoke",
		State:       "submitted",
		AcceptedAt:  "2026-07-14T05:00:00Z",
		UpdatedAt:   "2026-07-14T05:00:00Z",
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", orphanID)),
		Events:      runLedgerEventsFileName,
		Log:         runLedgerLogPath,
	}
	content, err := json.Marshal(temporaryRecord)
	if err != nil {
		t.Fatalf("marshal interrupted ledger record: %v", err)
	}
	if err := os.WriteFile(filepath.Join(temporaryDir, "run.json"), content, 0o644); err != nil {
		t.Fatalf("write interrupted ledger record: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("history exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var history runHistoryResponse
	if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
		t.Fatalf("parse history: %v", err)
	}
	if len(history.Runs) != 0 {
		t.Fatalf("history includes incomplete initialization: %+v", history)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run after incomplete initialization exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func TestHistoryRejectsLedgerLockSymlinkWithoutCreatingTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	if err := os.MkdirAll(runLedgerRoot(dir), 0o755); err != nil {
		t.Fatalf("create ledger root: %v", err)
	}
	target := filepath.Join(t.TempDir(), "outside.lock")
	if err := os.Symlink(target, filepath.Join(runLedgerRoot(dir), ".ledger.lock")); err != nil {
		t.Skipf("create ledger lock symlink: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"history"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("history exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("ledger lock symlink target = %v, want missing", err)
	}
}

func TestHistoryDoesNotPruneWhenLedgerPolicyIsInvalid(t *testing.T) {
	dir := writeRunConfigFixture(t)
	base := time.Date(2026, time.July, 14, 6, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
		Now:    func() time.Time { return base },
	})
	if code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  retention: invalid\n"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write invalid policy: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithRuntime([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Now: func() time.Time { return base.Add(31 * 24 * time.Hour) },
	})
	if code != 0 {
		t.Fatalf("history exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(runLedgerDir(dir, runID), "run.json")); err != nil {
		t.Fatalf("invalid policy pruned durable history: %v", err)
	}
}

func TestInvalidHistorySinceDoesNotApplyRetention(t *testing.T) {
	dir := writeRunConfigFixture(t)
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  retention: 1h\n"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	base := time.Date(2026, time.July, 14, 6, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
		Now:    func() time.Time { return base },
	})
	if code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")

	stdout.Reset()
	stderr.Reset()
	code = RunWithRuntime([]string{"history", "--since", "invalid"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Now: func() time.Time { return base.Add(2 * time.Hour) },
	})
	if code != 2 {
		t.Fatalf("invalid history exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(runLedgerDir(dir, runID), "run.json")); err != nil {
		t.Fatalf("invalid history pruned durable record: %v", err)
	}
}

func TestHistoryAppliesDefaultRetentionWhenRepoConfigIsMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	base := time.Date(2026, time.July, 14, 6, 0, 0, 0, time.UTC)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
		Now:    func() time.Time { return base },
	})
	if code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if err := os.Remove(filepath.Join(dir, defaultConfigName)); err != nil {
		t.Fatalf("remove Repo Run Config: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithRuntime([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Now: func() time.Time { return base.Add(31 * 24 * time.Hour) },
	})
	if code != 0 {
		t.Fatalf("history exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(runLedgerDir(dir, runID)); !os.IsNotExist(err) {
		t.Fatalf("default retention left expired ledger record: %v", err)
	}
}

func TestRunLedgerPolicyRejectsUnboundedLimits(t *testing.T) {
	if _, err := resolveRunLedgerPolicy(runLedgerConfig{MaxEvents: maximumRunLedgerMaxEvents + 1}); err == nil {
		t.Fatal("maxEvents above safe maximum returned nil error")
	}
	if _, err := resolveRunLedgerPolicy(runLedgerConfig{MaxEventBytes: maximumRunLedgerMaxEventBytes + 1}); err == nil {
		t.Fatal("maxEventBytes above safe maximum returned nil error")
	}
	if _, err := resolveRunLedgerPolicy(runLedgerConfig{MaxLogBytes: maximumRunLedgerMaxLogBytes + 1}); err == nil {
		t.Fatal("maxLogBytes above safe maximum returned nil error")
	}
}

func TestEmbeddedRunLedgerReplacesOversizedEventWithExplicitMarker(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "events.jsonl")
	destination := filepath.Join(dir, "retained.jsonl")
	var events bytes.Buffer
	for sequence, detail := range []string{"accepted", strings.Repeat("x", 4096), "completed"} {
		event, err := json.Marshal(map[string]any{
			"event":     "test.event",
			"sequence":  sequence + 1,
			"timestamp": "2026-07-14T08:00:00Z",
			"phase":     "lifecycle",
			"type":      "test.event",
			"stream":    "lifecycle",
			"details":   map[string]any{"message": detail},
		})
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		events.Write(event)
		events.WriteByte('\n')
	}
	if err := os.WriteFile(source, events.Bytes(), 0o644); err != nil {
		t.Fatalf("write events: %v", err)
	}
	count, omitted, truncated, retainedBytes, err := copyBoundedLedgerEvents(source, destination, 10, 1024)
	if err != nil {
		t.Fatalf("copy bounded events: %v", err)
	}
	if count != 3 || omitted != 0 || !truncated || retainedBytes > 1024 {
		t.Fatalf("bounded event result count=%d omitted=%d truncated=%t bytes=%d", count, omitted, truncated, retainedBytes)
	}
	retained, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read bounded events: %v", err)
	}
	if !bytes.Contains(retained, []byte(`"type":"run-ledger.event.truncated"`)) {
		t.Fatalf("oversized event marker missing:\n%s", retained)
	}
}

func TestEmbeddedRunLedgerBoundsEventsExpandedByLegacyNormalization(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "events.jsonl")
	destination := filepath.Join(dir, "retained.jsonl")
	legacy := fmt.Sprintf("{\"event\":\"run.accepted\",\"detail\":%q}\n{\"event\":\"run.completed\",\"detail\":\"passed\"}\n", strings.Repeat("x", 400))
	if err := os.WriteFile(source, []byte(legacy), 0o644); err != nil {
		t.Fatalf("write legacy events: %v", err)
	}
	count, _, truncated, retainedBytes, err := copyBoundedLedgerEvents(source, destination, 10, 1024, "2026-07-14T08:00:00Z")
	if err != nil {
		t.Fatalf("copy normalized bounded events: %v", err)
	}
	if count != 2 || !truncated || retainedBytes > 1024 {
		t.Fatalf("normalized bounded result count=%d truncated=%t bytes=%d", count, truncated, retainedBytes)
	}
	retained, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read normalized bounded events: %v", err)
	}
	if !bytes.Contains(retained, []byte(`"type":"run-ledger.event.truncated"`)) || !bytes.Contains(retained, []byte(`"type":"run.completed"`)) {
		t.Fatalf("normalized bounded events missing marker or tail:\n%s", retained)
	}
}

func TestEmbeddedRunLedgerReboundsPreviouslyTruncatedEventsWithoutLosingOmittedCount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	var events bytes.Buffer
	for sequence := 1; sequence <= 20; sequence++ {
		encoded, err := json.Marshal(map[string]any{
			"event":     "test.event",
			"sequence":  sequence,
			"timestamp": "2026-07-14T08:00:00Z",
			"phase":     "lifecycle",
			"type":      "test.event",
			"stream":    "lifecycle",
		})
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		events.Write(encoded)
		events.WriteByte('\n')
	}
	if err := os.WriteFile(path, events.Bytes(), 0o600); err != nil {
		t.Fatalf("write events: %v", err)
	}
	if _, omitted, _, _, err := copyBoundedLedgerEvents(path, path, 5, 1<<20); err != nil {
		t.Fatalf("first event bound: %v", err)
	} else if omitted != 16 {
		t.Fatalf("first omitted count = %d, want 16", omitted)
	}
	if count, omitted, _, _, err := copyBoundedLedgerEvents(path, path, 3, 1<<20); err != nil {
		t.Fatalf("second event bound: %v", err)
	} else if count != 3 || omitted != 18 {
		t.Fatalf("second bound count=%d omitted=%d, want count=3 omitted=18", count, omitted)
	}
	retained, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read rebound events: %v", err)
	}
	if !bytes.Contains(retained, []byte(`"omittedCount":18`)) {
		t.Fatalf("rebound events lost logical omitted count:\n%s", retained)
	}
}

func TestEmbeddedRunLedgerReboundsPreviouslyTruncatedLogWithoutLosingSourceBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.log")
	if err := os.WriteFile(path, bytes.Repeat([]byte("x"), 1000), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if _, _, err := copyBoundedLedgerLog(path, path, 200); err != nil {
		t.Fatalf("first log bound: %v", err)
	}
	if _, _, err := copyBoundedLedgerLog(path, path, 100); err != nil {
		t.Fatalf("second log bound: %v", err)
	}
	sourceBytes, truncated, err := retainedLedgerLogSourceBytesAndTruncated(path)
	if err != nil {
		t.Fatalf("read rebound log metadata: %v", err)
	}
	if sourceBytes != 1000 || !truncated {
		t.Fatalf("rebound log source bytes=%d truncated=%t, want 1000 true", sourceBytes, truncated)
	}
	if _, truncated, err := copyBoundedLedgerLog(path, path, 300); err != nil {
		t.Fatalf("expanded log rebound: %v", err)
	} else if !truncated {
		t.Fatal("expanded log rebound forgot that source content was truncated")
	}
	sourceBytes, _, err = retainedLedgerLogSourceBytesAndTruncated(path)
	if err != nil || sourceBytes != 1000 {
		t.Fatalf("expanded rebound source bytes=%d err=%v, want 1000", sourceBytes, err)
	}
}

func TestStoppedCleanupPhaseUsesDurableStatusAndAttach(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	manifest, stateDir, found, err := readStopRunManifest(dir, runID)
	if err != nil || !found {
		t.Fatalf("read run state: found=%t err=%v", found, err)
	}
	record, err := readRunRetentionRecord(dir, stateDir, manifest)
	if err != nil {
		t.Fatalf("read Run Record: %v", err)
	}
	record.StopPhase = "session-stopped"
	if err := writeRunRetentionRecord(runContext{StateDir: stateDir}, record); err != nil {
		t.Fatalf("write stopped Run Record: %v", err)
	}
	ledger, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	ledger.State = "cleanup-failed"
	ledger.StopPhase = "session-stopped"
	if err := writeRunLedgerRecord(dir, ledger); err != nil {
		t.Fatalf("write stopped ledger: %v", err)
	}

	for _, args := range [][]string{{"status", "--run", runID}, {"attach", runID}} {
		stdout.Reset()
		stderr.Reset()
		code = Run(args, &stdout, &stderr, dir, "test-version")
		if code != 0 {
			t.Fatalf("%v exit code = %d; stdout: %s stderr: %s", args, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "state: cleanup-failed") || strings.Contains(stdout.String(), "adapter: fake") {
			t.Fatalf("%v used live stopped-session state:\n%s", args, stdout.String())
		}
	}
}

func TestStatusUsesDurableRunIdentityAfterTransientStateCleanup(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
	})
	if code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("status exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"run: " + runID,
		"state: completed",
		"scenario: smoke",
		"host: " + defaultFakeHostID,
		"status: passed",
		"evidence: " + filepath.Join(dir, "artifacts", "maya-stall", runID),
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("durable status missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestFailedRunStatusAndAttachUseLedgerWhileTransientStateRemains(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("failed run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("failed transient state missing: %v", err)
	}

	for _, args := range [][]string{{"status", "--run", runID}, {"attach", runID}, {"history", "--state", "failed"}} {
		stdout.Reset()
		stderr.Reset()
		code = Run(args, &stdout, &stderr, dir, "test-version")
		if code != 0 {
			t.Fatalf("%v exit code = %d; stdout: %s stderr: %s", args, code, stdout.String(), stderr.String())
		}
		for _, want := range []string{"run: " + runID, "state: failed"} {
			if !strings.Contains(stdout.String(), want) {
				t.Fatalf("%v missing %q:\n%s", args, want, stdout.String())
			}
		}
	}
}

func TestStoppingKeptRunUpdatesDurableLedgerIdentity(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
	})
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read stopped ledger record: %v", err)
	}
	if record.RunID != runID || record.State != "completed" || record.Status != resultStatusPassed || record.CompletedAt == "" {
		t.Fatalf("stopped ledger record = %+v", record)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); !os.IsNotExist(err) {
		t.Fatalf("stopped transient state = %v, want removed", err)
	}
}

func TestStoppingKeptRunPersistsTerminalStateWhenRepoConfigIsMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
	})
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if err := os.Remove(filepath.Join(dir, defaultConfigName)); err != nil {
		t.Fatalf("remove Repo Run Config: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read stopped ledger record: %v", err)
	}
	if record.State != "completed" || record.CompletedAt == "" {
		t.Fatalf("stopped ledger record = %+v", record)
	}
}

func TestStoppingKeptRunRecoversSubmittedLedgerAfterCrashWindow(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
	})
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read kept ledger record: %v", err)
	}
	record.State = "submitted"
	record.Status = ""
	record.CompletedAt = ""
	if err := writeRunLedgerRecord(dir, record); err != nil {
		t.Fatalf("restore submitted crash-window record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	record, err = readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read stopped ledger record: %v", err)
	}
	if record.State != "completed" || record.Status != resultStatusPassed || record.CompletedAt == "" {
		t.Fatalf("recovered ledger record = %+v", record)
	}
}

func TestStopRecoversSubmittedLedgerWithActiveOwnedSessionWithoutEvidence(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	manifest, stateDir, found, err := readStopRunManifest(dir, runID)
	if err != nil || !found {
		t.Fatalf("read run state: found=%t err=%v", found, err)
	}
	retentionRecord, err := readRunRetentionRecord(dir, stateDir, manifest)
	if err != nil {
		t.Fatalf("read Run Record: %v", err)
	}
	retentionRecord.Status = "running"
	retentionRecord.RetentionReason = ""
	if err := writeRunRetentionRecord(runContext{StateDir: stateDir}, retentionRecord); err != nil {
		t.Fatalf("write active Run Record: %v", err)
	}
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	if err := markHostLockActive(dir, defaultFakeHostID, runID); err != nil {
		t.Fatalf("mark active Host Lock: %v", err)
	}
	markHostLockControllerDead(t, lockPath)
	ledger, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	ledger.State = "submitted"
	ledger.Status = ""
	if err := writeRunLedgerRecord(dir, ledger); err != nil {
		t.Fatalf("write submitted ledger: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "artifacts", "maya-stall", runID, evidenceBundleFileName)); err != nil {
		t.Fatalf("remove unfinished Evidence Bundle: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("active crash-recovery stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active crash-recovery Run State = %v, want removed", err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active crash-recovery Host Lock = %v, want removed", err)
	}
}

func TestStopReconstructsCorruptLedgerFromVerifiedTransientState(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if err := os.WriteFile(filepath.Join(runLedgerDir(dir, runID), "run.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatalf("corrupt ledger record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("corrupt-ledger recovery stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read reconstructed terminal ledger: %v", err)
	}
	if record.State != "completed" {
		t.Fatalf("reconstructed terminal ledger state = %q, want completed", record.State)
	}
}

func TestStopPreservesDurableEventsWhenTransientTailIsMalformed(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	stateEvents := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "events.jsonl")
	file, err := os.OpenFile(stateEvents, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open transient events: %v", err)
	}
	_, writeErr := file.WriteString("{partial")
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append malformed transient tail: %v", errors.Join(writeErr, closeErr))
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("malformed-event recovery stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read preserved durable events: %v", err)
	}
	if !bytes.Contains(events, []byte(`"type":"run.completed"`)) {
		t.Fatalf("durable event prefix was not preserved:\n%s", events)
	}
}

func markHostLockControllerDead(t *testing.T, lockPath string) {
	t.Helper()
	content, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read Host Lock controller: %v", err)
	}
	lines := strings.Split(string(content), "\n")
	found := false
	for index, line := range lines {
		if strings.HasPrefix(line, "pid:") {
			lines[index] = "pid: 2147483647"
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Host Lock has no controller pid:\n%s", content)
	}
	if err := os.WriteFile(lockPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatalf("write dead Host Lock controller: %v", err)
	}
}

func TestRunLedgerAndRunRecordFilesRemainPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not portable to Windows")
	}
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	for _, path := range []string{
		filepath.Join(runLedgerDir(dir, runID), "run.json"),
		filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName),
		filepath.Join(runLedgerDir(dir, runID), filepath.FromSlash(runLedgerLogPath)),
		filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat private state file %s: %v", path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("private state file %s mode = %04o, want no group/other access", path, info.Mode().Perm())
		}
	}
}

func TestCleanupCheckpointRecoversResolvedLedgerMetadata(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read kept ledger record: %v", err)
	}
	record.Scenario = ""
	record.TargetProfile = ""
	record.Host = ""
	record.Status = ""
	record.State = "submitted"
	if err := writeRunLedgerRecord(dir, record); err != nil {
		t.Fatalf("write pre-resolution ledger record: %v", err)
	}
	if err := checkpointRunLedgerStopPhase(dir, runID, "broker-cleaned", time.Now()); err != nil {
		t.Fatalf("checkpoint cleanup ledger: %v", err)
	}
	record, err = readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read checkpointed ledger record: %v", err)
	}
	if record.Scenario != "smoke" || record.TargetProfile != "default" || record.Host != defaultFakeHostID || record.Status != resultStatusPassed {
		t.Fatalf("checkpointed ledger metadata = %+v", record)
	}
}

func TestSubmittedCrashWindowStillUsesRetainedStatusAndAttach(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
	})
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read kept ledger record: %v", err)
	}
	record.State = "submitted"
	record.Status = ""
	if err := writeRunLedgerRecord(dir, record); err != nil {
		t.Fatalf("restore submitted crash-window record: %v", err)
	}

	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"status", "--run", runID}, want: "remoteState: running"},
		{args: []string{"attach", runID}, want: "adapter: fake"},
	} {
		stdout.Reset()
		stderr.Reset()
		code = Run(test.args, &stdout, &stderr, dir, "test-version")
		if code != 0 {
			t.Fatalf("%v exit code = %d; stdout: %s stderr: %s", test.args, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), test.want) {
			t.Fatalf("%v output missing %q:\n%s", test.args, test.want, stdout.String())
		}
	}
}

func TestStopRecoversCleanupCompletedBeforeTerminalLedgerWrite(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
	})
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	recordPath := filepath.Join(runLedgerDir(dir, runID), "run.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(content, &raw); err != nil {
		t.Fatalf("parse ledger record: %v", err)
	}
	raw["stopPhase"] = "host-lock-released"
	raw["state"] = "cleanup-failed"
	content, err = json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatalf("marshal ledger recovery marker: %v", err)
	}
	if err := os.WriteFile(recordPath, append(content, '\n'), 0o644); err != nil {
		t.Fatalf("write ledger recovery marker: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("remove transient Run State: %v", err)
	}
	leftoverStateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	if err := os.MkdirAll(leftoverStateDir, 0o755); err != nil {
		t.Fatalf("recreate partially removed Run State: %v", err)
	}
	if err := os.WriteFile(filepath.Join(leftoverStateDir, "leftover.tmp"), []byte("partial cleanup\n"), 0o644); err != nil {
		t.Fatalf("write partial cleanup leftover: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")); err != nil {
		t.Fatalf("remove Host Lock: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("cleanup-pending status exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: cleanup-failed") || strings.Contains(stdout.String(), "state: kept") {
		t.Fatalf("cleanup-pending status misreported retained session:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("recovery stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read recovered ledger: %v", err)
	}
	if record.State != "completed" || record.CompletedAt == "" {
		t.Fatalf("recovered ledger record = %+v", record)
	}
	if _, err := os.Stat(leftoverStateDir); !os.IsNotExist(err) {
		t.Fatalf("partial Run State after recovery = %v, want removed", err)
	}
}

func TestReleasedStopRecoveryPreservesSuccessorHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	manifest, stateDir, found, err := readStopRunManifest(dir, runID)
	if err != nil || !found {
		t.Fatalf("read retained run manifest: found=%t err=%v", found, err)
	}
	record, err := readRunRetentionRecord(dir, stateDir, manifest)
	if err != nil {
		t.Fatalf("read retained Run Record: %v", err)
	}
	record.StopPhase = "host-lock-released"
	if err := writeRunRetentionRecord(runContext{StateDir: stateDir}, record); err != nil {
		t.Fatalf("write released Run Record: %v", err)
	}
	ledger, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	ledger.State = "cleanup-failed"
	ledger.StopPhase = "host-lock-released"
	if err := writeRunLedgerRecord(dir, ledger); err != nil {
		t.Fatalf("write released ledger record: %v", err)
	}
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", defaultFakeHostID+".lock")
	if err := os.WriteFile(lockPath, []byte("host: "+defaultFakeHostID+"\nkeptRun: successor-run\n"), 0o644); err != nil {
		t.Fatalf("write successor Host Lock: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("released recovery stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	content, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read successor Host Lock: %v", err)
	}
	if !strings.Contains(string(content), "keptRun: successor-run") {
		t.Fatalf("successor Host Lock changed:\n%s", content)
	}
}

func TestReleasedStopRecoveryUsesManifestHostWhenRunRecordHostIsStale(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	manifest, stateDir, found, err := readStopRunManifest(dir, runID)
	if err != nil || !found {
		t.Fatalf("read run state: found=%t err=%v", found, err)
	}
	record, err := readRunRetentionRecord(dir, stateDir, manifest)
	if err != nil {
		t.Fatalf("read Run Record: %v", err)
	}
	record.Host = "stale-host"
	record.StopPhase = "host-lock-released"
	if err := writeRunRetentionRecord(runContext{StateDir: stateDir}, record); err != nil {
		t.Fatalf("write stale-host Run Record: %v", err)
	}
	ledger, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	ledger.State = "cleanup-failed"
	ledger.StopPhase = "host-lock-released"
	if err := writeRunLedgerRecord(dir, ledger); err != nil {
		t.Fatalf("write released ledger: %v", err)
	}
	lockPath := filepath.Join(dir, ".maya-stall", "state", "locks", "hosts", manifest.Host+".lock")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("released recovery exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("manifest Host Lock after recovery = %v, want removed", err)
	}
}

func TestAutomaticCleanupPreservesRunStateWhenTerminalLedgerCheckpointFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file permissions do not reliably reject ledger staging writes")
	}
	dir := writeRunConfigFixture(t)
	now := time.Date(2026, time.July, 14, 14, 0, 0, 0, time.UTC)
	runID := now.Format("20060102T150405.000000000Z")
	broker := newBlockingBroker(ScenarioResult{Status: resultStatusPassed})
	done := make(chan runResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
			Broker: broker,
			Now:    func() time.Time { return now },
		})
		done <- runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()
	waitForBlockingBrokerStart(t, broker.started, done)
	ledgerDir := runLedgerDir(dir, runID)
	if err := os.Chmod(ledgerDir, 0o555); err != nil {
		t.Fatalf("make ledger record read-only: %v", err)
	}
	if directoryAcceptsTemporaryFile(ledgerDir) {
		if err := os.Chmod(ledgerDir, 0o755); err != nil {
			t.Fatalf("restore ledger permissions: %v", err)
		}
		close(broker.release)
		<-done
		t.Skip("current user can write through read-only directory permissions")
	}
	close(broker.release)
	result := <-done
	if err := os.Chmod(ledgerDir, 0o755); err != nil {
		t.Fatalf("restore ledger permissions: %v", err)
	}
	if result.code == 0 {
		t.Fatalf("run unexpectedly succeeded; stdout: %s stderr: %s", result.stdout, result.stderr)
	}
	bundleDir := filepath.Join(dir, "artifacts", "maya-stall", runID)
	if _, err := os.Stat(filepath.Join(bundleDir, evidenceReportFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("report remained authoritative after terminal ledger durability failure: %v", err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatalf("read ledger-failing Evidence Bundle: %v", err)
	}
	if bundle.Status != resultStatusFailed || bundle.Failure == nil || !strings.Contains(bundle.Failure.Diagnostic, "run ledger") {
		t.Fatalf("ledger-failing Evidence Bundle = %+v", bundle)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("Run State removed before terminal ledger durability: %v", err)
	}
	var stopStdout, stopStderr bytes.Buffer
	if code := Run([]string{"stop", runID}, &stopStdout, &stopStderr, dir, "test-version"); code != 0 {
		t.Fatalf("stop preserved cleanup-pending run exit code = %d; stdout: %s stderr: %s", code, stopStdout.String(), stopStderr.String())
	}
}

func TestDeferredFailureCleanupPreservesRunStateWhenTerminalLedgerCheckpointFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file permissions do not reliably reject ledger staging writes")
	}
	dir := writeRunConfigFixture(t)
	now := time.Date(2026, time.July, 14, 14, 30, 0, 0, time.UTC)
	runID := now.Format("20060102T150405.000000000Z")
	broker := newBlockingErrorBroker(errors.New("scenario transport failed"))
	done := make(chan runResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
			Broker: broker,
			Now:    func() time.Time { return now },
		})
		done <- runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()
	waitForBlockingBrokerStart(t, broker.started, done)
	ledgerDir := runLedgerDir(dir, runID)
	if err := os.Chmod(ledgerDir, 0o555); err != nil {
		t.Fatalf("make ledger record read-only: %v", err)
	}
	if directoryAcceptsTemporaryFile(ledgerDir) {
		if err := os.Chmod(ledgerDir, 0o755); err != nil {
			t.Fatalf("restore ledger permissions: %v", err)
		}
		close(broker.release)
		<-done
		t.Skip("current user can write through read-only directory permissions")
	}
	close(broker.release)
	result := <-done
	if err := os.Chmod(ledgerDir, 0o755); err != nil {
		t.Fatalf("restore ledger permissions: %v", err)
	}
	if result.code == 0 {
		t.Fatalf("run unexpectedly succeeded; stdout: %s stderr: %s", result.stdout, result.stderr)
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", runID)); err != nil {
		t.Fatalf("Run State removed before deferred terminal ledger durability: %v", err)
	}
	var stopStdout, stopStderr bytes.Buffer
	if code := Run([]string{"stop", runID}, &stopStdout, &stopStderr, dir, "test-version"); code != 0 {
		t.Fatalf("stop deferred cleanup-pending run exit code = %d; stdout: %s stderr: %s", code, stopStdout.String(), stopStderr.String())
	}
}

func directoryAcceptsTemporaryFile(dir string) bool {
	file, err := os.CreateTemp(dir, ".permission-probe-*")
	if err != nil {
		return false
	}
	path := file.Name()
	_ = file.Close()
	_ = os.Remove(path)
	return true
}

func TestAcceptedRunLedgerRecordExistsBeforeScenarioCompletes(t *testing.T) {
	dir := writeRunConfigFixture(t)
	now := time.Date(2026, time.July, 14, 13, 0, 0, 0, time.UTC)
	runID := now.Format("20060102T150405.000000000Z")
	broker := newBlockingBroker(ScenarioResult{Status: resultStatusPassed})
	done := make(chan runResult, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
			Broker: broker,
			Now:    func() time.Time { return now },
		})
		done <- runResult{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()
	released := false
	defer func() {
		if !released {
			close(broker.release)
			<-done
		}
	}()
	waitForBlockingBrokerStart(t, broker.started, done)

	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read active ledger record: %v", err)
	}
	if record.State != "submitted" || record.RunID != runID || record.Scenario != "smoke" {
		t.Fatalf("active ledger record = %+v", record)
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read active durable events: %v", err)
	}
	if record.EventBytes != len(events) || record.EventBytes == 0 {
		t.Fatalf("active ledger eventBytes = %d, retained bytes = %d", record.EventBytes, len(events))
	}
	var accepted struct {
		Sequence int    `json:"sequence"`
		Type     string `json:"type"`
	}
	first := bytes.Split(bytes.TrimSpace(events), []byte{'\n'})[0]
	if err := json.Unmarshal(first, &accepted); err != nil {
		t.Fatalf("parse accepted durable event: %v", err)
	}
	if accepted.Sequence != 1 || accepted.Type != "run.accepted" {
		t.Fatalf("accepted durable event = %+v", accepted)
	}

	close(broker.release)
	released = true
	result := <-done
	if result.code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", result.code, result.stdout, result.stderr)
	}
}

func waitForBlockingBrokerStart(t *testing.T, started <-chan struct{}, done chan runResult) {
	t.Helper()
	select {
	case <-started:
		return
	case result := <-done:
		done <- result
		t.Fatalf("run exited before blocking broker started; code=%d stdout=%s stderr=%s", result.code, result.stdout, result.stderr)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for blocking broker to start")
	}
}

type ledgerLogBroker struct {
	fakeSessionLifecycle
	log string
}

type blockingErrorBroker struct {
	fakeSessionLifecycle
	started chan struct{}
	release chan struct{}
	err     error
}

func newBlockingErrorBroker(err error) *blockingErrorBroker {
	return &blockingErrorBroker{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     err,
	}
}

func (broker *blockingErrorBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	close(broker.started)
	<-broker.release
	return ScenarioResult{}, broker.err
}

func (broker ledgerLogBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte(broker.log), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", resultStatusPassed); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusPassed}, nil
}

func TestCompletedRunRetainsStructuredEventsAndLogsForAttach(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	runtime := runRuntime{Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}}

	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	eventsPath := filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName)
	eventsFile, err := os.Open(eventsPath)
	if err != nil {
		t.Fatalf("open durable events: %v", err)
	}
	defer func() { _ = eventsFile.Close() }()
	scanner := bufio.NewScanner(eventsFile)
	eventCount := 0
	for scanner.Scan() {
		eventCount++
		var event struct {
			Sequence  int            `json:"sequence"`
			Timestamp string         `json:"timestamp"`
			Phase     string         `json:"phase"`
			Type      string         `json:"type"`
			Stream    string         `json:"stream"`
			Details   map[string]any `json:"details"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("parse durable event %d: %v", eventCount, err)
		}
		if event.Sequence != eventCount || event.Timestamp == "" || event.Phase == "" || event.Type == "" || event.Stream == "" || event.Details == nil {
			t.Fatalf("durable event %d = %+v", eventCount, event)
		}
		if _, err := time.Parse(time.RFC3339Nano, event.Timestamp); err != nil {
			t.Fatalf("durable event %d timestamp = %q: %v", eventCount, event.Timestamp, err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan durable events: %v", err)
	}
	if eventCount < 2 {
		t.Fatalf("durable event count = %d, want at least 2", eventCount)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("durable attach exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"run: " + runID, "events:", `"type":"run.accepted"`, "logs:", "fake Session Broker ran Scenario", "evidence: "} {
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Fatalf("durable attach missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestDurableAttachRejectsSymlinkedLedgerLogDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	logsDir := filepath.Join(runLedgerDir(dir, runID), "logs")
	if err := os.RemoveAll(logsDir); err != nil {
		t.Fatalf("remove ledger logs: %v", err)
	}
	externalDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(externalDir, "session.log"), []byte("external secret\n"), 0o644); err != nil {
		t.Fatalf("write external log: %v", err)
	}
	if err := os.Symlink(externalDir, logsDir); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID}, &stdout, &stderr, dir, "test-version")
	if code != 1 {
		t.Fatalf("attach exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "external secret") {
		t.Fatalf("attach followed symlinked ledger log directory:\n%s", stdout.String())
	}
}

func TestCleanupFailedRunWithTransientStateKeepsTruthSeekingStatus(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger record: %v", err)
	}
	record.State = "cleanup-failed"
	if err := writeRunLedgerRecord(dir, record); err != nil {
		t.Fatalf("write cleanup-failed ledger record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("status exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"stateDir: ", "runtime: fake-local", "retentionReason: stop-after-never"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("truth-seeking status missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestFreshRunCleanupFailureUsesDurableStatusWithoutKeptRouting(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
		Broker: ledgerCleanupFailingBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}},
	})
	if code != 1 {
		t.Fatalf("cleanup-failed run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("cleanup-failed status exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "state: cleanup-failed") || strings.Contains(stdout.String(), "stateDir:") {
		t.Fatalf("cleanup-failed durable status:\n%s", stdout.String())
	}
}

func TestRunScopedClickSucceedsAfterRepoConfigIsRemoved(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if err := os.Remove(filepath.Join(dir, defaultConfigName)); err != nil {
		t.Fatalf("remove Repo Run Config: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run-scoped click exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read durable events: %v", err)
	}
	if !bytes.Contains(events, []byte(`"type":"attach.control.click"`)) {
		t.Fatalf("durable events missing click:\n%s", events)
	}
}

func TestRunScopedClickReportsLedgerDurabilityWarningAfterAction(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	logsDir := filepath.Join(runLedgerDir(dir, runID), "logs")
	if err := os.RemoveAll(logsDir); err != nil {
		t.Fatalf("remove ledger logs: %v", err)
	}
	if err := os.Symlink(t.TempDir(), logsDir); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run-scoped click exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "durabilityWarning: click succeeded but refresh embedded run ledger:") {
		t.Fatalf("click output missing durability warning:\n%s", stdout.String())
	}
}

func TestRunScopedScreenshotWarnsWhenLedgerRecordIsMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if err := os.RemoveAll(runLedgerDir(dir, runID)); err != nil {
		t.Fatalf("remove ledger record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "screenshot"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run-scoped screenshot exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "durabilityWarning: screenshot succeeded but refresh embedded run ledger: run") {
		t.Fatalf("screenshot output missing missing-ledger warning:\n%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "maya-stall", runID, "screenshots", "run-scoped-screenshot.png")); err != nil {
		t.Fatalf("screenshot side effect missing: %v", err)
	}
}

func TestRunScopedClickWarnsWhenLedgerRecordIsMissing(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	if err := os.RemoveAll(runLedgerDir(dir, runID)); err != nil {
		t.Fatalf("remove ledger record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run-scoped click exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "durabilityWarning: click succeeded but refresh embedded run ledger: run") {
		t.Fatalf("click output missing missing-ledger warning:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop with reconstructed ledger exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read reconstructed ledger: %v", err)
	}
	if record.State != "completed" {
		t.Fatalf("reconstructed ledger state = %q, want completed", record.State)
	}
}

func TestStopCheckpointsTransientEventsBeforeCleanup(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	evidenceEventsPath := filepath.Join(dir, "artifacts", "maya-stall", runID, evidenceEventsFileName)
	staleEvidence, err := os.ReadFile(evidenceEventsPath)
	if err != nil {
		t.Fatalf("read initial Evidence events: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run-scoped click exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if err := os.WriteFile(evidenceEventsPath, staleEvidence, 0o644); err != nil {
		t.Fatalf("restore stale Evidence events: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"stop", runID}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("stop exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read stopped ledger events: %v", err)
	}
	if !bytes.Contains(events, []byte(`"type":"attach.control.click"`)) {
		t.Fatalf("stopped ledger lost transient click event:\n%s", events)
	}
}

func TestLedgerRefreshPreservesNewerDurableEventsWhenTransientEventsAreGone(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run-scoped click exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	ledgerEventsPath := filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName)
	ledgerInfo, err := os.Stat(ledgerEventsPath)
	if err != nil {
		t.Fatalf("stat durable events: %v", err)
	}
	evidenceEventsPath := filepath.Join(dir, "artifacts", "maya-stall", runID, evidenceEventsFileName)
	older := ledgerInfo.ModTime().Add(-time.Hour)
	if err := os.Chtimes(evidenceEventsPath, older, older); err != nil {
		t.Fatalf("age Evidence events: %v", err)
	}
	transientEventsPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "events.jsonl")
	if err := os.Remove(transientEventsPath); err != nil {
		t.Fatalf("remove transient events: %v", err)
	}

	if err := refreshRunLedgerArtifacts(dir, runID, time.Now()); err != nil {
		t.Fatalf("refresh ledger: %v", err)
	}
	events, err := os.ReadFile(ledgerEventsPath)
	if err != nil {
		t.Fatalf("read refreshed durable events: %v", err)
	}
	if !bytes.Contains(events, []byte(`"type":"attach.control.click"`)) {
		t.Fatalf("refresh replaced newer durable events with stale Evidence:\n%s", events)
	}
}

func TestLedgerRefreshPreservesNewerDurableEventsThanTransientPrefix(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")

	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"attach", runID, "control", "click", "--x", "12", "--y", "34"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run-scoped click exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	transientEventsPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "events.jsonl")
	transientEvents, err := os.ReadFile(transientEventsPath)
	if err != nil {
		t.Fatalf("read transient events: %v", err)
	}
	firstEvent := bytes.SplitN(transientEvents, []byte{'\n'}, 2)[0]
	if err := os.WriteFile(transientEventsPath, append(firstEvent, '\n'), 0o600); err != nil {
		t.Fatalf("restore shorter transient prefix: %v", err)
	}

	if err := refreshRunLedgerArtifacts(dir, runID, time.Now()); err != nil {
		t.Fatalf("refresh ledger: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read durable events: %v", err)
	}
	if !bytes.Contains(events, []byte(`"type":"attach.control.click"`)) {
		t.Fatalf("shorter transient prefix replaced newer durable events:\n%s", events)
	}
}

func TestLedgerRefreshCopiesNewerEvidenceWithEqualModificationTime(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	if err := os.Remove(filepath.Join(stateDir, "events.jsonl")); err != nil {
		t.Fatalf("remove transient events: %v", err)
	}
	if err := os.Remove(filepath.Join(stateDir, filepath.FromSlash(evidenceLogPath))); err != nil {
		t.Fatalf("remove transient log: %v", err)
	}
	evidenceDir := filepath.Join(dir, "artifacts", "maya-stall", runID)
	evidenceEventsPath := filepath.Join(evidenceDir, evidenceEventsFileName)
	eventsFile, err := os.OpenFile(evidenceEventsPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open Evidence events: %v", err)
	}
	_, writeErr := eventsFile.WriteString("{\"sequence\":999,\"timestamp\":\"2026-07-14T12:00:00Z\",\"phase\":\"test\",\"type\":\"terminal.equal-mtime\",\"stream\":\"lifecycle\",\"details\":{}}\n")
	closeErr := eventsFile.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append Evidence event: %v", errors.Join(writeErr, closeErr))
	}
	evidenceLogPathname := filepath.Join(evidenceDir, filepath.FromSlash(evidenceLogPath))
	logFile, err := os.OpenFile(evidenceLogPathname, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open Evidence log: %v", err)
	}
	_, writeErr = logFile.WriteString("terminal equal-mtime log\n")
	closeErr = logFile.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append Evidence log: %v", errors.Join(writeErr, closeErr))
	}
	equalTime := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	for _, path := range []string{
		evidenceEventsPath,
		filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName),
		evidenceLogPathname,
		filepath.Join(runLedgerDir(dir, runID), filepath.FromSlash(runLedgerLogPath)),
	} {
		if err := os.Chtimes(path, equalTime, equalTime); err != nil {
			t.Fatalf("set equal artifact time for %s: %v", path, err)
		}
	}

	if err := refreshRunLedgerArtifacts(dir, runID, equalTime.Add(time.Minute)); err != nil {
		t.Fatalf("refresh ledger: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read durable events: %v", err)
	}
	if !bytes.Contains(events, []byte(`"type":"terminal.equal-mtime"`)) {
		t.Fatalf("equal-mtime refresh omitted terminal event:\n%s", events)
	}
	log, err := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), filepath.FromSlash(runLedgerLogPath)))
	if err != nil {
		t.Fatalf("read durable log: %v", err)
	}
	if !bytes.Contains(log, []byte("terminal equal-mtime log")) {
		t.Fatalf("equal-mtime refresh omitted terminal log:\n%s", log)
	}
}

func TestLedgerRefreshRecomputesMetadataFromCurrentDurableArtifacts(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	record.EventCount = 0
	record.EventBytes = 0
	record.EventsOmitted = 99
	record.EventsTruncated = true
	record.LogBytes = 0
	record.LogTruncated = true
	if err := writeRunLedgerRecord(dir, record); err != nil {
		t.Fatalf("write stale ledger metadata: %v", err)
	}
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	if err := os.Remove(filepath.Join(stateDir, "events.jsonl")); err != nil {
		t.Fatalf("remove transient events: %v", err)
	}
	if err := os.Remove(filepath.Join(stateDir, filepath.FromSlash(evidenceLogPath))); err != nil {
		t.Fatalf("remove transient log: %v", err)
	}

	if err := refreshRunLedgerArtifacts(dir, runID, time.Now()); err != nil {
		t.Fatalf("refresh current durable artifacts: %v", err)
	}
	record, err = readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read refreshed ledger: %v", err)
	}
	if record.EventCount == 0 || record.EventBytes == 0 || record.EventsOmitted != 0 || record.EventsTruncated {
		t.Fatalf("refreshed event metadata = %+v", record)
	}
	if record.LogBytes == 0 || record.LogTruncated {
		t.Fatalf("refreshed log metadata = %+v", record)
	}
}

func TestLedgerRefreshReappliesTightenedArtifactBounds(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	stateLog := filepath.Join(dir, ".maya-stall", "state", "runs", runID, filepath.FromSlash(evidenceLogPath))
	if err := os.WriteFile(stateLog, []byte(strings.Repeat("log-data-", 100)), 0o600); err != nil {
		t.Fatalf("expand transient log: %v", err)
	}
	if err := refreshRunLedgerArtifacts(dir, runID, time.Now()); err != nil {
		t.Fatalf("refresh expanded artifacts: %v", err)
	}
	configPath := filepath.Join(dir, defaultConfigName)
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read Repo Run Config: %v", err)
	}
	config = bytes.Replace(config, []byte("version: 1\n"), []byte("version: 1\nrunLedger:\n  maxEvents: 3\n  maxEventBytes: 1024\n  maxLogBytes: 96\n"), 1)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatalf("tighten Run Ledger config: %v", err)
	}

	if err := refreshRunLedgerArtifacts(dir, runID, time.Now()); err != nil {
		t.Fatalf("refresh tightened artifacts: %v", err)
	}
	record, err := readRunLedgerRecord(dir, runID)
	if err != nil {
		t.Fatalf("read tightened ledger: %v", err)
	}
	if record.EventCount > 3 || record.EventBytes > 1024 || !record.EventsTruncated {
		t.Fatalf("tightened event metadata = %+v", record)
	}
	if record.LogBytes > 96 || !record.LogTruncated {
		t.Fatalf("tightened log metadata = %+v", record)
	}
}

func TestLedgerRefreshRejectsSymlinkedTransientArtifactSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is not consistently available on Windows test runners")
	}
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := outputValue(t, stdout.String(), "run")
	transientLogPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, filepath.FromSlash(evidenceLogPath))
	if err := os.Remove(transientLogPath); err != nil {
		t.Fatalf("remove transient log: %v", err)
	}
	externalLogPath := filepath.Join(t.TempDir(), "secret.log")
	if err := os.WriteFile(externalLogPath, []byte("external secret\n"), 0o644); err != nil {
		t.Fatalf("write external log: %v", err)
	}
	if err := os.Symlink(externalLogPath, transientLogPath); err != nil {
		t.Fatalf("symlink transient log: %v", err)
	}

	err := refreshRunLedgerArtifacts(dir, runID, time.Now())
	if err == nil || !strings.Contains(err.Error(), "must not be or contain a symlink") {
		t.Fatalf("refresh error = %v, want symlink rejection", err)
	}
	durableLog, readErr := os.ReadFile(filepath.Join(runLedgerDir(dir, runID), filepath.FromSlash(runLedgerLogPath)))
	if readErr != nil {
		t.Fatalf("read durable log: %v", readErr)
	}
	if bytes.Contains(durableLog, []byte("external secret")) {
		t.Fatalf("refresh copied symlink target into durable log:\n%s", durableLog)
	}
}

func TestRunHistoryFiltersByScenarioHostStateAndRecentTime(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfig := writeSingleHealthyHostConfig(t, dir)
	base := time.Date(2026, time.July, 14, 8, 0, 0, 0, time.UTC)
	run := func(at time.Time, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := RunWithRuntime(args, &stdout, &stderr, dir, "test-version", runRuntime{
			Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
			Now:    func() time.Time { return at },
		})
		if code != 0 && args[len(args)-1] != "missing" {
			t.Fatalf("run %v exit code = %d; stdout: %s stderr: %s", args, code, stdout.String(), stderr.String())
		}
		if args[len(args)-1] == "missing" && code != 1 {
			t.Fatalf("missing Scenario exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
		}
		return outputValue(t, stdout.String(), "run")
	}
	firstID := run(base, "run", "smoke")
	failedID := run(base.Add(time.Hour), "run", "missing")
	alphaID := run(base.Add(2*time.Hour), "run", "--host-config", hostConfig, "--target-profile", "ci", "smoke")

	tests := []struct {
		name   string
		args   []string
		wantID string
	}{
		{name: "Scenario", args: []string{"--scenario", "missing"}, wantID: failedID},
		{name: "Maya Host", args: []string{"--host", "alpha"}, wantID: alphaID},
		{name: "state", args: []string{"--state", "failed"}, wantID: failedID},
		{name: "recent time", args: []string{"--since", "30m"}, wantID: alphaID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"history", "--json"}, test.args...)
			var stdout, stderr bytes.Buffer
			code := RunWithRuntime(args, &stdout, &stderr, dir, "test-version", runRuntime{Now: func() time.Time { return base.Add(2 * time.Hour) }})
			if code != 0 {
				t.Fatalf("history %v exit code = %d; stdout: %s stderr: %s", test.args, code, stdout.String(), stderr.String())
			}
			var history runHistoryResponse
			if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
				t.Fatalf("parse history JSON: %v", err)
			}
			if history.Version != runLedgerSchemaVersion || len(history.Runs) != 1 || history.Runs[0].RunID != test.wantID {
				t.Fatalf("filtered history = %+v, want %s", history, test.wantID)
			}
		})
	}

	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version", runRuntime{Now: func() time.Time { return base.Add(2 * time.Hour) }})
	if code != 0 {
		t.Fatalf("unfiltered history exit code = %d; stderr: %s", code, stderr.String())
	}
	var history runHistoryResponse
	if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
		t.Fatalf("parse unfiltered history: %v", err)
	}
	wantOrder := []string{alphaID, failedID, firstID}
	if len(history.Runs) != len(wantOrder) {
		t.Fatalf("unfiltered history count = %d, want %d", len(history.Runs), len(wantOrder))
	}
	for index, want := range wantOrder {
		if history.Runs[index].RunID != want {
			t.Fatalf("history order = %+v, want %v", history.Runs, wantOrder)
		}
	}
}

func TestRunHistoryOrdersCollisionSuffixesNumerically(t *testing.T) {
	dir := t.TempDir()
	acceptedAt := time.Date(2026, time.July, 14, 12, 0, 0, 0, time.UTC)
	source := filepath.Join(dir, "events.jsonl")
	if err := appendEvent(source, "run.accepted", "smoke"); err != nil {
		t.Fatalf("write source event: %v", err)
	}
	base := acceptedAt.Format("20060102T150405.000000000Z")
	for _, suffix := range []string{"-9", "-10"} {
		runID := base + suffix
		manifest := runManifest{RunID: runID, Scenario: "smoke", TargetProfile: "default", Host: defaultFakeHostID}
		if err := initializeRunLedger(dir, manifest, acceptedAt, source); err != nil {
			t.Fatalf("initialize collision ledger %s: %v", runID, err)
		}
	}
	records, err := listRunLedgerRecords(dir)
	if err != nil {
		t.Fatalf("list collision ledgers: %v", err)
	}
	if len(records) != 2 || records[0].RunID != base+"-10" || records[1].RunID != base+"-9" {
		t.Fatalf("collision history order = %+v", records)
	}
}

func TestRunHistorySortsVariablePrecisionTimestampsChronologically(t *testing.T) {
	dir := writeRunConfigFixture(t)
	base := time.Date(2026, time.July, 14, 8, 0, 0, 0, time.UTC)
	runAt := func(at time.Time) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runRuntime{
			Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
			Now:    func() time.Time { return at },
		})
		if code != 0 {
			t.Fatalf("run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
		}
		return outputValue(t, stdout.String(), "run")
	}
	olderID := runAt(base)
	newerID := runAt(base.Add(500 * time.Millisecond))

	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"history", "--json"}, &stdout, &stderr, dir, "test-version", runRuntime{Now: func() time.Time { return base.Add(time.Second) }})
	if code != 0 {
		t.Fatalf("history exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var history runHistoryResponse
	if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
		t.Fatalf("parse history: %v", err)
	}
	if len(history.Runs) != 2 || history.Runs[0].RunID != newerID || history.Runs[1].RunID != olderID {
		t.Fatalf("history order = %+v, want [%s %s]", history.Runs, newerID, olderID)
	}
}
