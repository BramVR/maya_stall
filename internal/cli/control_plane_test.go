package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunCompletesFakeScenarioThroughConfiguredControlPlane(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("shared run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 2 || results[0].Kind != "run-accepted" || results[1].Kind != "run" || results[1].Status != resultStatusPassed {
		t.Fatalf("shared run output = %+v", results)
	}
	runID := results[0].RunID
	if results[0].EvidenceDir != "/v1/runs/"+runID+"/evidence" || results[1].EvidenceDir != results[0].EvidenceDir {
		t.Fatalf("shared run evidence links = accepted %q terminal %q", results[0].EvidenceDir, results[1].EvidenceDir)
	}
	serverRepo := filepath.Join(dataDir, "runs", runID, "repo")
	if _, err := os.Stat(filepath.Join(serverRepo, ".maya-stall", "state", "ledger", "runs", runID, "run.json")); err != nil {
		t.Fatalf("Control Plane Run Ledger: %v", err)
	}
	if _, err := os.Stat(filepath.Join(serverRepo, "artifacts", "maya-stall", runID, evidenceBundleFileName)); err != nil {
		t.Fatalf("Control Plane Evidence Bundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".maya-stall")); !os.IsNotExist(err) {
		t.Fatalf("shared run client state = %v, want absent", err)
	}
	var evidence controlPlaneEvidenceResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/evidence", runtime, &evidence); err != nil {
		t.Fatalf("read Control Plane evidence: %v", err)
	}
	if evidence.Version != 1 || evidence.Kind != "evidence" || evidence.RunID != runID || evidence.Bundle.RunID != runID || evidence.Bundle.Status != resultStatusPassed {
		t.Fatalf("shared evidence response = %+v", evidence)
	}
	evidence.Bundle.RunID = "20260101T000000.000000000Z"
	corrupt, err := json.Marshal(evidence.Bundle)
	if err != nil {
		t.Fatalf("marshal mismatched evidence: %v", err)
	}
	if err := os.WriteFile(filepath.Join(serverRepo, "artifacts", "maya-stall", runID, evidenceBundleFileName), corrupt, 0o600); err != nil {
		t.Fatalf("write mismatched evidence: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"result", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime); code != 1 {
		t.Fatalf("mismatched evidence result exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func TestConfiguredControlPlaneReturnsRunIDBeforeFakeExecutionCompletes(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	serverRuntime := defaultRunRuntime()
	serverRuntime.Broker = blockingFakeBroker{
		fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "fake passed"}},
		started:           started,
		release:           release,
	}
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", serverRuntime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	accepted := make(chan runOutcome, 1)
	clientRuntime := defaultRunRuntime()
	clientRuntime.ControlPlaneHTTPClient = server.Client()
	clientRuntime.Accepted = func(outcome runOutcome) { accepted <- outcome }
	done := make(chan int, 1)
	go func() {
		done <- RunWithRuntime([]string{"run", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", clientRuntime)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("fake execution did not start")
	}
	var acceptedRun runOutcome
	select {
	case acceptedRun = <-accepted:
		if acceptedRun.RunID == "" || !acceptedRun.Accepted {
			t.Fatalf("early accepted outcome = %+v", acceptedRun)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run ID was not returned before fake execution completed")
	}
	var resultOut bytes.Buffer
	var resultErr bytes.Buffer
	if code := RunWithRuntime([]string{"result", "--json", "--control-plane", server.URL, acceptedRun.RunID}, &resultOut, &resultErr, repoDir, "test-version", clientRuntime); code != 0 {
		t.Fatalf("active shared result exit code = %d; stdout: %s stderr: %s", code, resultOut.String(), resultErr.String())
	}
	var pending controlPlaneResultResponse
	if err := json.Unmarshal(resultOut.Bytes(), &pending); err != nil || pending.Final || pending.Success || pending.State == "" {
		t.Fatalf("active shared result = %+v err %v", pending, err)
	}
	secondDone := make(chan int, 1)
	go func() {
		secondDone <- RunWithRuntime([]string{"run", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", clientRuntime)
	}()
	select {
	case code := <-secondDone:
		if code != 1 {
			t.Fatalf("concurrent shared admission exit code = %d, want 1", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent shared admission waited behind active execution")
	}
	close(release)
	released = true
	if code := <-done; code != 0 {
		t.Fatalf("shared blocking run exit code = %d, want 0", code)
	}
}

func TestConfiguredControlPlaneReadsEventsDuringExecution(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	serverRuntime := defaultRunRuntime()
	serverRuntime.Broker = blockingFakeBroker{
		fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
		started:           started,
		release:           release,
	}
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", serverRuntime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	accepted := make(chan runOutcome, 1)
	runtime.Accepted = func(outcome runOutcome) { accepted <- outcome }
	done := make(chan int, 1)
	go func() {
		done <- RunWithRuntime([]string{"run", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	<-started
	runID := (<-accepted).RunID

	var response controlPlaneEventsResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/events?fromSequence=2", runtime, &response); err != nil {
		t.Fatalf("read active events: %v", err)
	}
	var found bool
	for _, event := range response.Events {
		if event["type"] == "run.started" {
			found = true
		}
	}
	if !found {
		t.Fatalf("active events = %+v, want run.started", response.Events)
	}
	close(release)
	released = true
	if code := <-done; code != 0 {
		t.Fatalf("shared run exit code = %d", code)
	}
}

func TestConfiguredControlPlaneStreamsEventsFromSequenceThroughCompletion(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	serverRuntime := defaultRunRuntime()
	serverRuntime.Broker = blockingFakeBroker{
		fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
		started:           started,
		release:           release,
	}
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", serverRuntime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	accepted := make(chan runOutcome, 1)
	runtime.Accepted = func(outcome runOutcome) { accepted <- outcome }
	runDone := make(chan int, 1)
	go func() {
		runDone <- RunWithRuntime([]string{"run", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	<-started
	runID := (<-accepted).RunID
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/runs/"+runID+"/events?fromSequence=2&follow=true", nil)
	if err != nil {
		t.Fatalf("create event stream request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer test-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("event stream response = HTTP %d %q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	decoder := json.NewDecoder(response.Body)
	sequences := make([]int, 0)
	var first struct {
		Kind     string         `json:"kind"`
		Sequence int            `json:"sequence"`
		Event    map[string]any `json:"event"`
	}
	if err := decoder.Decode(&first); err != nil {
		t.Fatalf("read live event: %v", err)
	}
	if first.Kind != "event" || first.Sequence < 2 || ledgerEventSequence(first.Event) != first.Sequence {
		t.Fatalf("first streamed event = %+v", first)
	}
	sequences = append(sequences, first.Sequence)
	close(release)
	released = true
	for {
		var record struct {
			Kind     string         `json:"kind"`
			Sequence int            `json:"sequence"`
			Event    map[string]any `json:"event"`
		}
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("read event stream: %v", err)
		}
		if record.Kind == "stream-end" {
			break
		}
		if record.Kind == "event" {
			sequences = append(sequences, record.Sequence)
		}
	}
	for index := 1; index < len(sequences); index++ {
		if sequences[index] <= sequences[index-1] {
			t.Fatalf("streamed sequences = %v", sequences)
		}
	}
	if code := <-runDone; code != 0 {
		t.Fatalf("shared run exit code = %d", code)
	}
}

func TestConfiguredEventStreamAndLogsExposeBoundedTruncation(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	configPath := filepath.Join(repoDir, ".maya-stall.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read Repo Run Config: %v", err)
	}
	config = append(config, []byte("\nrunLedger:\n  maxEvents: 3\n  maxEventBytes: 1024\n  maxLogBytes: 96\n")...)
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatalf("write bounded Repo Run Config: %v", err)
	}
	serverRuntime := defaultRunRuntime()
	serverRuntime.Broker = longLogFakeBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}}
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", serverRuntime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("bounded run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/runs/"+runID+"/events?fromSequence=2&follow=true", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("open bounded event stream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	decoder := json.NewDecoder(response.Body)
	var marker controlPlaneEventStreamRecord
	if err := decoder.Decode(&marker); err != nil {
		t.Fatalf("decode event truncation marker: %v", err)
	}
	if marker.Kind != "events-truncated" || !marker.EventsTruncated || marker.EventsOmitted < 1 || marker.Sequence != 0 {
		t.Fatalf("event truncation marker = %+v", marker)
	}
	if marker.NextSequence < 3 {
		t.Fatalf("event truncation cursor = %d, want first retained sequence after the gap", marker.NextSequence)
	}
	var resumed controlPlaneEventsResponse
	if err := getControlPlaneJSON(server.URL, "", fmt.Sprintf("/v1/runs/%s/events?fromSequence=%d", runID, marker.NextSequence), runtime, &resumed); err != nil {
		t.Fatalf("resume after retained event gap: %v", err)
	}
	if resumed.EventsTruncated || resumed.EventsOmitted != 0 || resumed.FirstAvailableSequence < marker.NextSequence {
		t.Fatalf("resumed event metadata repeats retained gap: %+v", resumed)
	}
	var beyond controlPlaneEventsResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/events?fromSequence=999999", runtime, &beyond); err != nil {
		t.Fatalf("read beyond terminal sequence: %v", err)
	}
	if beyond.EventsTruncated || beyond.FirstAvailableSequence != 0 || beyond.NextSequence != 999999 {
		t.Fatalf("beyond-terminal event metadata = %+v", beyond)
	}
	var logs controlPlaneLogsResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/logs", runtime, &logs); err != nil {
		t.Fatalf("read bounded logs: %v", err)
	}
	if !logs.Truncated || logs.Bytes > 96 || !strings.HasPrefix(logs.Content, "[maya-stall: log truncated; omitted ") {
		t.Fatalf("bounded logs = %+v", logs)
	}
}

func TestControlPlaneTerminalRejectsPassedOutcomeWhenFinalizationFails(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "server-run")
	outcome := runOutcome{
		RunID: "20260101T000000.000000000Z", Scenario: "smoke", Accepted: true,
		Result: ScenarioResult{Status: resultStatusPassed},
	}

	terminal := controlPlaneTerminalResponse(outcome, errors.New("finalize "+repoDir+" ledger: disk full"), repoDir)

	if terminal.Status != resultStatusFailed || terminal.FailedLayer != string(failureLayerRunState) || !strings.Contains(terminal.Diagnostic, "disk full") || strings.Contains(terminal.Diagnostic, repoDir) {
		t.Fatalf("Control Plane terminal after finalization failure = %+v", terminal)
	}
}

func TestControlPlaneSanitizerRedactsSharedDataRoot(t *testing.T) {
	dataDir := privateTempDir(t)
	repoDir := filepath.Join(dataDir, "runs", "20260101T000000.000000000Z", "repo")
	privatePath := filepath.Join(dataDir, "fake-host", "state", "locks", "hosts", "fake-local.lock")

	sanitized := sanitizeControlPlaneText("lock failure at "+privatePath, repoDir)

	if strings.Contains(sanitized, dataDir) || !strings.Contains(sanitized, "<control-plane-data>") {
		t.Fatalf("sanitized shared data path = %q", sanitized)
	}
}

func TestControlPlaneSanitizerUsesModeNeutralRunRepositoryLabel(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "embedded-repo")

	sanitized := sanitizeControlPlaneText("failure in "+repoDir, repoDir)

	if strings.Contains(sanitized, repoDir) || !strings.Contains(sanitized, "<run-repository>") || strings.Contains(sanitized, "<control-plane-run>") {
		t.Fatalf("sanitized run repository path = %q", sanitized)
	}
}

func TestControlPlaneEvidenceReadsRejectSymlinkedEvidenceDirectory(t *testing.T) {
	repoDir := t.TempDir()
	runID := "20260101T000000.000000000Z"
	evidenceRelative := filepath.Join("artifacts", "maya-stall", runID)
	evidenceParent := filepath.Join(repoDir, "artifacts", "maya-stall")
	if err := os.MkdirAll(evidenceParent, 0o700); err != nil {
		t.Fatalf("create evidence parent: %v", err)
	}
	outside := t.TempDir()
	bundle, err := json.Marshal(evidenceBundle{RunID: runID, Status: resultStatusPassed})
	if err != nil {
		t.Fatalf("marshal outside evidence bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, evidenceBundleFileName), bundle, 0o600); err != nil {
		t.Fatalf("write outside evidence bundle: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(evidenceParent, runID)); err != nil {
		t.Fatalf("symlink evidence directory: %v", err)
	}
	record := runLedgerRecord{
		RunID: runID, State: "completed", Status: resultStatusPassed,
		EvidenceDir: filepath.ToSlash(evidenceRelative),
	}

	if _, err := readControlPlaneEvidence(repoDir, record); err == nil {
		t.Fatal("Control Plane evidence read followed a symlinked evidence directory")
	}
	if cleanupState := controlPlaneCleanupState(repoDir, record); cleanupState != "unresolved" {
		t.Fatalf("cleanup state through symlinked evidence directory = %q, want unresolved", cleanupState)
	}
}

func TestControlPlaneTerminalSurfacesFinalizationFailureAfterScenarioFailure(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "server-run")
	outcome := runOutcome{
		RunID: "20260101T000000.000000000Z", Scenario: "smoke", Accepted: true,
		Result:  ScenarioResult{Status: resultStatusFailed},
		Failure: &runFailureEvidence{FailedLayer: string(failureLayerExecution), Diagnostic: "scenario failed"},
	}
	runErr := errors.Join(errors.New("scenario failed"), errors.New("finalize ledger: disk full"))

	terminal := controlPlaneTerminalResponse(outcome, runErr, repoDir)

	if terminal.FailedLayer != string(failureLayerRunState) || !strings.Contains(terminal.Diagnostic, "scenario failed") || !strings.Contains(terminal.Diagnostic, "disk full") {
		t.Fatalf("Control Plane failed terminal after finalization failure = %+v", terminal)
	}
}

func TestStatusReadsConfiguredControlPlaneRunAsStableJSON(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"status", "--json", "--control-plane", server.URL, "--run", runID}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("shared status exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var status struct {
		Version      int    `json:"version"`
		Kind         string `json:"kind"`
		RunID        string `json:"runId"`
		State        string `json:"state"`
		Status       string `json:"status"`
		CleanupState string `json:"cleanupState"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("decode shared status JSON: %v; output: %s", err, stdout.String())
	}
	if status.Version != 1 || status.Kind != "status" || status.RunID != runID || status.State != "completed" || status.Status != resultStatusPassed || status.CleanupState != "completed" {
		t.Fatalf("shared status = %+v", status)
	}
}

func TestHistoryFindsCompletedConfiguredControlPlaneRun(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"history", "--json", "--control-plane", server.URL}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("configured history exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var history runHistoryResponse
	if err := json.Unmarshal(stdout.Bytes(), &history); err != nil {
		t.Fatalf("decode configured history: %v", err)
	}
	if len(history.Runs) != 1 || history.Runs[0].RunID != runID || history.Runs[0].State != "completed" || history.Runs[0].Events == "" || history.Runs[0].Log == "" {
		t.Fatalf("configured history = %+v", history)
	}
}

func TestControlPlaneHistorySkipsReservedRunWithoutDurableLedger(t *testing.T) {
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	reservedRunID := "20260716T120000.000000000Z"
	if err := os.MkdirAll(filepath.Join(dataDir, "runs", reservedRunID, "repo"), 0o700); err != nil {
		t.Fatalf("reserve run directory: %v", err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://maya-stall.example.com/v1/runs", nil)
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("history HTTP status = %d, body %s", response.Code, response.Body.String())
	}
	var history runHistoryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &history); err != nil || len(history.Runs) != 0 {
		t.Fatalf("history with incomplete reservation = %+v err %v", history, err)
	}
}

func TestControlPlaneHistoryProductionSortAndCap(t *testing.T) {
	runIDs := []string{"20260716T120000.000000000Z-9", "20260716T120000.000000000Z-10"}
	sortControlPlaneRunIDsNewest(runIDs)
	if runIDs[0] != "20260716T120000.000000000Z-10" {
		t.Fatalf("collision Run ID order = %v", runIDs)
	}
	records := make([]runLedgerRecord, 0, maximumControlPlaneHistoryRuns)
	for index := 0; index < maximumControlPlaneHistoryRuns; index++ {
		if !appendBoundedControlPlaneHistory(&records, runLedgerRecord{RunID: fmt.Sprintf("run-%d", index)}) {
			t.Fatalf("history capped before record %d", index)
		}
	}
	if appendBoundedControlPlaneHistory(&records, runLedgerRecord{RunID: "overflow"}) || len(records) != maximumControlPlaneHistoryRuns {
		t.Fatalf("history cap accepted overflow: %d records", len(records))
	}
}

func TestControlPlaneHistoryWindowBoundsScanAndExposesCursor(t *testing.T) {
	runIDs := make([]string, maximumControlPlaneHistoryScanRuns+1)
	for index := range runIDs {
		runIDs[index] = fmt.Sprintf("20260716T120000.000000000Z-%d", maximumControlPlaneHistoryScanRuns-index)
	}
	window, nextBeforeRunID, omitted, err := boundedControlPlaneHistoryWindow(runIDs, "")
	if err != nil {
		t.Fatalf("bound history window: %v", err)
	}
	if len(window) != maximumControlPlaneHistoryScanRuns || nextBeforeRunID != window[len(window)-1] || omitted != 1 {
		t.Fatalf("history window = %d runs, cursor %q, omitted %d", len(window), nextBeforeRunID, omitted)
	}
	next, nextCursor, nextOmitted, err := boundedControlPlaneHistoryWindow(runIDs, nextBeforeRunID)
	if err != nil {
		t.Fatalf("read next history window: %v", err)
	}
	if len(next) != 1 || next[0] != runIDs[len(runIDs)-1] || nextCursor != "" || nextOmitted != 0 {
		t.Fatalf("next history window = %v, cursor %q, omitted %d", next, nextCursor, nextOmitted)
	}
}

func TestConfiguredHistoryParsesContinuationCursor(t *testing.T) {
	runID := "20260716T120000.000000000Z"
	options, err := parseHistoryArgs([]string{"--control-plane", "https://maya-stall.example.com", "--before-run", runID})
	if err != nil || options.BeforeRunID != runID {
		t.Fatalf("configured history cursor = %q, err %v", options.BeforeRunID, err)
	}
}

func TestConfiguredHistoryHumanOutputPreservesEmptyPageCursor(t *testing.T) {
	runID := "20260716T120000.000000000Z"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(runHistoryResponse{
			Version: controlPlaneAPIVersion, Kind: "history", Runs: []runLedgerRecord{},
			RunsTruncated: true, RunsOmittedAtLeast: 1, NextBeforeRunID: runID,
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var output bytes.Buffer

	err := printRunHistoryThroughMode("", historyOptions{ControlPlane: server.URL}, time.Now(), &output, runtime)

	if err != nil {
		t.Fatalf("print configured history: %v", err)
	}
	for _, want := range []string{"state: no runs", "historyTruncated: true", "runsOmittedAtLeast: 1", "nextBeforeRunId: " + runID} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("configured history output missing %q: %s", want, output.String())
		}
	}
}

func TestEventsReadConfiguredControlPlaneRunAsStableJSON(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"events", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("shared events exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var events struct {
		Version int    `json:"version"`
		Kind    string `json:"kind"`
		RunID   string `json:"runId"`
		Events  []struct {
			Sequence  int            `json:"sequence"`
			Timestamp string         `json:"timestamp"`
			Phase     string         `json:"phase"`
			Type      string         `json:"type"`
			Stream    string         `json:"stream"`
			Details   map[string]any `json:"details"`
		} `json:"events"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &events); err != nil {
		t.Fatalf("decode shared events JSON: %v; output: %s", err, stdout.String())
	}
	if events.Version != 1 || events.Kind != "events" || events.RunID != runID || len(events.Events) < 3 {
		t.Fatalf("shared events = %+v", events)
	}
	for index, event := range events.Events {
		if event.Sequence != index+1 || event.Timestamp == "" || event.Phase == "" || event.Type == "" || event.Stream == "" || event.Details == nil {
			t.Fatalf("shared event %d = %+v", index, event)
		}
	}
}

func TestEventsReconnectFromRequestedSequence(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"events", "--json", "--from-sequence", "3", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("reconnected events exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var events controlPlaneEventsResponse
	if err := json.Unmarshal(stdout.Bytes(), &events); err != nil {
		t.Fatalf("decode reconnected events: %v", err)
	}
	if len(events.Events) == 0 {
		t.Fatal("reconnected events are empty")
	}
	for _, event := range events.Events {
		if sequence := ledgerEventSequence(event); sequence < 3 {
			t.Fatalf("reconnected event sequence = %d, want >= 3", sequence)
		}
	}
}

func TestEventsReconnectAfterOversizedMarkerDoesNotRepeatTruncation(t *testing.T) {
	repoDir := privateTempDir(t)
	runID := "20260716T120000.000000000Z"
	eventsPath := filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName)
	if err := writeRunLedgerBytes(eventsPath, []byte("{\"sequence\":1,\"type\":\"run-ledger.event.truncated\"}\n{\"sequence\":2,\"type\":\"run.completed\"}\n")); err != nil {
		t.Fatalf("write retained events: %v", err)
	}
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, AcceptedAt: "2026-07-16T12:00:00Z",
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)),
		Events:      runLedgerEventsFileName, Log: runLedgerLogPath, EventCount: 2,
	}
	if err := newRunLedgerStore(repoDir).Replace(record); err != nil {
		t.Fatalf("write retained Run Ledger: %v", err)
	}

	response, err := readControlPlaneEvents(repoDir, record, 2)

	if err != nil {
		t.Fatalf("read reconnected events: %v", err)
	}
	if response.EventsTruncated || response.EventsOmitted != 0 || len(response.Events) != 1 || ledgerEventSequence(response.Events[0]) != 2 {
		t.Fatalf("reconnected events = %+v", response)
	}
}

func TestEventStreamAdvancesCursorAcrossRetainedGapWithoutTail(t *testing.T) {
	dataDir := privateTempDir(t)
	runID := "20260716T120000.000000000Z"
	repoDir := filepath.Join(dataDir, "runs", runID, "repo")
	if err := os.MkdirAll(runLedgerDir(repoDir, runID), 0o700); err != nil {
		t.Fatalf("create run ledger: %v", err)
	}
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, Scenario: "smoke", TargetProfile: "default", Host: "maya-win-01",
		State: "completed", Status: resultStatusPassed, AcceptedAt: "2026-07-16T12:00:00Z", CompletedAt: "2026-07-16T12:01:00Z",
		UpdatedAt: "2026-07-16T12:01:00Z", EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)),
		Events: runLedgerEventsFileName, Log: runLedgerLogPath, EventCount: 2, EventsOmitted: 2, EventsTruncated: true,
	}
	if err := writeRunLedgerRecord(repoDir, record); err != nil {
		t.Fatalf("write run ledger: %v", err)
	}
	events := "{\"sequence\":1,\"type\":\"run.accepted\"}\n" +
		"{\"details\":{\"firstOmittedSequence\":2,\"lastOmittedSequence\":3,\"omittedCount\":2},\"sequence\":2,\"type\":\"run-ledger.events.truncated\"}\n"
	if err := writeRunLedgerBytes(filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName), []byte(events)); err != nil {
		t.Fatalf("write retained events: %v", err)
	}
	handler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	request, err := http.NewRequest(http.MethodGet, server.URL+"/v1/runs/"+runID+"/events?fromSequence=1&follow=true", nil)
	if err != nil {
		t.Fatalf("create event stream request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer test-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("read event stream: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	decoder := json.NewDecoder(response.Body)
	var records []controlPlaneEventStreamRecord
	for {
		var streamRecord controlPlaneEventStreamRecord
		if err := decoder.Decode(&streamRecord); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode event stream: %v", err)
		}
		records = append(records, streamRecord)
	}
	if len(records) != 3 || records[1].Kind != "events-truncated" || records[1].NextSequence != 4 || records[2].Kind != "stream-end" || records[2].NextSequence != 4 {
		t.Fatalf("gap-only event stream = %+v", records)
	}
}

func TestControlPlaneEventReadWaitsForLedgerTransaction(t *testing.T) {
	repoDir := privateTempDir(t)
	runID := "20260716T120000.000000000Z"
	eventsPath := filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName)
	if err := writeRunLedgerBytes(eventsPath, []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n")); err != nil {
		t.Fatalf("write retained events: %v", err)
	}
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, AcceptedAt: "2026-07-16T12:00:00Z",
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)),
		Events:      runLedgerEventsFileName, Log: runLedgerLogPath, EventCount: 1,
	}
	if err := newRunLedgerStore(repoDir).Replace(record); err != nil {
		t.Fatalf("write retained Run Ledger: %v", err)
	}
	locked := make(chan struct{})
	release := make(chan struct{})
	transactionDone := make(chan error, 1)
	go func() {
		transactionDone <- withRunLedgerLock(repoDir, runID, func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	readDone := make(chan error, 1)
	go func() {
		_, err := readControlPlaneEvents(repoDir, record)
		readDone <- err
	}()
	select {
	case err := <-readDone:
		t.Fatalf("event read escaped ledger transaction: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-transactionDone; err != nil {
		t.Fatalf("ledger transaction: %v", err)
	}
	if err := <-readDone; err != nil {
		t.Fatalf("read events after transaction: %v", err)
	}
}

func TestQuarantinedAssignmentStreamIsNeverTransientRefreshCandidate(t *testing.T) {
	record := runLedgerRecord{State: "failed", Status: resultStatusFailed}
	if shouldRefreshControlPlaneEventStream(true, record) {
		t.Fatal("assignment-backed terminal stream entered transient refresh path")
	}
	if shouldRefreshControlPlaneEventStream(false, record) {
		t.Fatal("terminal embedded stream entered transient refresh path")
	}
	if !shouldRefreshControlPlaneEventStream(false, runLedgerRecord{State: "submitted"}) {
		t.Fatal("active embedded stream stopped refreshing transient events")
	}
}

func TestControlPlaneLogsReadContentAndTruncationMetadataAtomically(t *testing.T) {
	repoDir := privateTempDir(t)
	runID := "20260716T120000.000000000Z"
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, Scenario: "smoke", TargetProfile: "default", Host: "maya-win-01",
		State: "submitted", AcceptedAt: "2026-07-16T12:00:00Z", UpdatedAt: "2026-07-16T12:00:01Z",
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)), Events: runLedgerEventsFileName, Log: runLedgerLogPath,
		LogTruncated: true,
	}
	if err := writeRunLedgerRecord(repoDir, record); err != nil {
		t.Fatalf("write run ledger: %v", err)
	}
	if err := writeRunLedgerBytes(filepath.Join(runLedgerDir(repoDir, runID), filepath.FromSlash(runLedgerLogPath)), []byte("[maya-stall: log truncated; omitted 10 bytes]\ntail\n")); err != nil {
		t.Fatalf("write retained log: %v", err)
	}
	stale := record
	stale.LogTruncated = false

	logs, err := readControlPlaneLogs(repoDir, stale)

	if err != nil {
		t.Fatalf("read Control Plane logs: %v", err)
	}
	if !logs.Truncated || !strings.Contains(logs.Content, "log truncated") {
		t.Fatalf("Control Plane logs = %+v", logs)
	}
}

func TestAttachReconnectsConfiguredRunFromRequestedSequence(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"attach", runID, "--control-plane", server.URL, "--from-sequence", "3"}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("configured attach exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	output := stdout.String()
	if strings.Contains(output, `"sequence":1`) || strings.Contains(output, `"sequence":2`) {
		t.Fatalf("configured attach replayed events before cursor: %s", output)
	}
	for _, want := range []string{`"sequence":3`, "fake Session Broker ran Scenario", "cleanupState: completed", "evidence: /v1/runs/" + runID + "/evidence"} {
		if !strings.Contains(output, want) {
			t.Fatalf("configured attach output missing %q: %s", want, output)
		}
	}
}

func TestAttachRejectsUnexpectedControlPlaneStreamContentType(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{"attach", "run-1", "--control-plane", server.URL}, &stdout, &stderr, privateTempDir(t), "test-version", runtime)

	if code != 1 {
		t.Fatalf("configured attach exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `unexpected Content-Type "application/json"`) {
		t.Fatalf("configured attach error missing observed Content-Type: %s", stderr.String())
	}
}

func TestLogsReadConfiguredControlPlaneRunAsStableJSON(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"logs", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("shared logs exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var logs struct {
		Version   int    `json:"version"`
		Kind      string `json:"kind"`
		RunID     string `json:"runId"`
		Content   string `json:"content"`
		Bytes     int    `json:"bytes"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &logs); err != nil {
		t.Fatalf("decode shared logs JSON: %v; output: %s", err, stdout.String())
	}
	if logs.Version != 1 || logs.Kind != "logs" || logs.RunID != runID || !strings.Contains(logs.Content, "fake Session Broker ran Scenario") || logs.Bytes != len(logs.Content) || logs.Truncated {
		t.Fatalf("shared logs = %+v", logs)
	}
}

func TestConfiguredControlPlaneReadsLogsAboveDefaultEventLimit(t *testing.T) {
	const runID = "20260101T000000.000000000Z"
	content := strings.Repeat("x", 8*1024*1024)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/runs/"+runID+"/logs" {
			http.NotFound(response, request)
			return
		}
		_ = json.NewEncoder(response).Encode(controlPlaneLogsResponse{
			Version: controlPlaneAPIVersion,
			Kind:    "logs",
			RunID:   runID,
			Content: content,
			Bytes:   len(content),
		})
	}))
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{"logs", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, t.TempDir(), "test-version", runtime)

	if code != 0 {
		t.Fatalf("large shared logs exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	var logs controlPlaneLogsResponse
	if err := json.Unmarshal(stdout.Bytes(), &logs); err != nil || logs.Content != content || logs.Bytes != len(content) {
		t.Fatalf("large shared logs bytes = %d content bytes = %d err = %v", logs.Bytes, len(logs.Content), err)
	}
}

func TestResultReadsConfiguredControlPlaneRunAsFinalSuccess(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"result", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 0 {
		t.Fatalf("shared result exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var result struct {
		Version      int            `json:"version"`
		Kind         string         `json:"kind"`
		RunID        string         `json:"runId"`
		State        string         `json:"state"`
		Status       string         `json:"status"`
		CleanupState string         `json:"cleanupState"`
		Final        bool           `json:"final"`
		Success      bool           `json:"success"`
		Result       ScenarioResult `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode shared result JSON: %v; output: %s", err, stdout.String())
	}
	if result.Version != 1 || result.Kind != "result" || result.RunID != runID || result.State != "completed" || result.Status != resultStatusPassed || result.CleanupState != "completed" || !result.Final || !result.Success || result.Result.Status != resultStatusPassed {
		t.Fatalf("shared result = %+v", result)
	}
}

func TestConfiguredControlPlaneKeptRunIsNotFinalSuccess(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "--stop-after", "never", "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runOutput := decodeRunJSONLines(t, stdout.Bytes())
	runID := runOutput[0].RunID
	if len(runOutput) != 2 || len(runOutput[1].FollowUpCommands) != 0 {
		t.Fatalf("shared kept run follow-up commands = %#v, want none until configured cleanup exists", runOutput)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"result", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared kept result exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var result controlPlaneResultResponse
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode shared kept result: %v", err)
	}
	if result.State != "kept" || result.Status != resultStatusPassed || result.CleanupState != "retained" || result.Final || result.Success {
		t.Fatalf("shared kept result = %+v", result)
	}
}

func TestConfiguredControlPlaneKeptRunBlocksTheSharedFakeHost(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "--stop-after", "never", "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared kept run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()

	code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 1 {
		t.Fatalf("run behind shared kept lock exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 2 || results[1].Status != resultStatusFailed || results[1].FailedLayer != string(failureLayerHostSelection) {
		t.Fatalf("run behind shared kept lock = %+v", results)
	}
}

func TestConfiguredControlPlaneCleanupFailedRunIsNotFinalSuccess(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	serverRuntime := defaultRunRuntime()
	serverRuntime.Broker = cleanupFailingFakeBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "Scenario passed before cleanup"}}}
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", serverRuntime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	clientRuntime := defaultRunRuntime()
	clientRuntime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", clientRuntime); code != 1 {
		t.Fatalf("shared cleanup-failed run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"result", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", clientRuntime); code != 0 {
		t.Fatalf("shared cleanup-failed result exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var result controlPlaneResultResponse
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode shared cleanup-failed result: %v", err)
	}
	if result.State != "cleanup-failed" || result.CleanupState != "failed" || !result.Final || result.Success {
		t.Fatalf("shared cleanup-failed result = %+v", result)
	}
}

func TestConfiguredControlPlanePersistsValidatorFailureAsTerminalResult(t *testing.T) {
	repoDir := t.TempDir()
	mustWriteFile(t, filepath.Join(repoDir, defaultConfigName), `version: 1
scenarios:
  smoke:
    mayaVersion: "2025"
    payload: {}
    expectedOutputs:
      scenarioResult: outputs/result.json
    validators:
      - type: scenarioResultStatus
        status: failed
`)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 1 {
		t.Fatalf("shared validator-failed run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"result", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared validator-failed result exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var result controlPlaneResultResponse
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode validator-failed result: %v", err)
	}
	if result.State != "failed" || result.Status != resultStatusFailed || result.CleanupState != "completed" || !result.Final || result.Success || len(result.Validators) != 1 || result.Validators[0].Status != resultStatusFailed {
		t.Fatalf("shared validator-failed result = %+v", result)
	}
}

func TestConfiguredControlPlaneAcceptsRunBeforeRepoConfigValidation(t *testing.T) {
	repoDir := t.TempDir()
	mustWriteFile(t, filepath.Join(repoDir, defaultConfigName), "version: 1\nscenarios: [invalid\n")
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 1 {
		t.Fatalf("shared invalid-config run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 2 || results[0].Kind != "run-accepted" || results[0].RunID == "" || results[1].FailedLayer != string(failureLayerRepoConfig) {
		t.Fatalf("shared invalid-config output = %+v", results)
	}
	runID := results[0].RunID
	evidenceDir := filepath.Join(dataDir, "runs", runID, "repo", "artifacts", "maya-stall", runID)
	bundle := readEvidenceBundle(t, evidenceDir)
	if bundle.RunID != runID || bundle.Status != resultStatusFailed || bundle.Failure == nil || bundle.Failure.FailedLayer != string(failureLayerRepoConfig) {
		t.Fatalf("shared invalid-config Evidence Bundle = %+v", bundle)
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"logs", "--json", "--control-plane", server.URL, runID}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared invalid-config logs exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var logs controlPlaneLogsResponse
	if err := json.Unmarshal(stdout.Bytes(), &logs); err != nil {
		t.Fatalf("decode shared invalid-config logs: %v", err)
	}
	if strings.Contains(logs.Content, dataDir) || logs.Bytes != len(logs.Content) {
		t.Fatalf("shared invalid-config logs expose Control Plane data path: %q", logs.Content)
	}
}

func TestConfiguredControlPlaneOwnsIdentifiedSubmissionSyntaxFailures(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--json", "smoke", "--stop-after", "invalid", "--control-plane", server.URL}, &stdout, &stderr, repoDir, "test-version", runtime)

	if code != 1 {
		t.Fatalf("shared syntax-failed run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 2 || results[0].RunID == "" || results[1].FailedLayer != string(failureLayerSubmission) {
		t.Fatalf("shared syntax-failed output = %+v", results)
	}
	runID := results[0].RunID
	if _, err := os.Stat(filepath.Join(dataDir, "runs", runID, "repo", ".maya-stall", "state", "ledger", "runs", runID, "run.json")); err != nil {
		t.Fatalf("shared syntax-failed Run Ledger: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".maya-stall")); !os.IsNotExist(err) {
		t.Fatalf("shared syntax failure client state = %v, want absent", err)
	}
}

func TestMalformedControlPlaneSelectionDoesNotFallBackToEmbeddedMode(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--json", "smoke", "--control-plane"}, &stdout, &stderr, repoDir, "test-version", defaultRunRuntime())

	if code != 2 {
		t.Fatalf("malformed Control Plane selection exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 1 || results[0].Kind != "usage-error" || results[0].Accepted || results[0].RunID != "" {
		t.Fatalf("malformed Control Plane selection output = %+v", results)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".maya-stall")); !os.IsNotExist(err) {
		t.Fatalf("malformed Control Plane selection embedded state = %v, want absent", err)
	}
}

func TestControlPlaneTokenEnvWithoutURLDoesNotFallBackToEmbeddedMode(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--json", "smoke", "--control-plane-token-env", "TEST_TOKEN", "--stop-after", "invalid"}, &stdout, &stderr, repoDir, "test-version", defaultRunRuntime())

	if code != 2 {
		t.Fatalf("token-env-only selection exit code = %d, want 2; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 1 || results[0].Kind != "usage-error" || results[0].Accepted {
		t.Fatalf("token-env-only selection output = %+v", results)
	}
	if _, err := os.Stat(filepath.Join(repoDir, ".maya-stall")); !os.IsNotExist(err) {
		t.Fatalf("token-env-only selection embedded state = %v, want absent", err)
	}
}

func TestControlPlaneSubmissionRejectsSymlinkedRepoConfig(t *testing.T) {
	repoDir := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.yaml")
	mustWriteFile(t, target, "version: 1\nscenarios: {}\n")
	if err := os.Symlink(target, filepath.Join(repoDir, defaultConfigName)); err != nil {
		t.Fatalf("symlink Repo Run Config: %v", err)
	}

	if _, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke"}); err == nil {
		t.Fatal("symlinked Repo Run Config was accepted for Control Plane upload")
	}
}

func TestControlPlaneSubmissionRejectsPayloadThroughSymlinkedAncestor(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	outside := t.TempDir()
	mustWriteFile(t, filepath.Join(outside, "start.ma"), "private outside scene")
	if err := os.Remove(filepath.Join(repoDir, "scenes", "start.ma")); err != nil {
		t.Fatalf("remove fixture scene: %v", err)
	}
	if err := os.Remove(filepath.Join(repoDir, "scenes")); err != nil {
		t.Fatalf("remove fixture scene directory: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(repoDir, "scenes")); err != nil {
		t.Fatalf("symlink payload ancestor: %v", err)
	}

	if _, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke"}); err == nil {
		t.Fatal("payload through symlinked ancestor was accepted for Control Plane upload")
	}
}

func TestControlPlaneSubmissionDeduplicatesDeclaredRepoConfig(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	configPath := filepath.Join(repoDir, defaultConfigName)
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read fixture config: %v", err)
	}
	content = bytes.Replace(content, []byte("scripts:\n        - \"maya/smoke.py\""), []byte("scripts:\n        - \"maya/smoke.py\"\n        - \".maya-stall.yaml\""), 1)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("add config to declared payload: %v", err)
	}

	submission, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke"})
	if err != nil {
		t.Fatalf("build submission with declared config: %v", err)
	}
	if err := validateControlPlaneSubmissionFiles(submission); err != nil {
		t.Fatalf("validate submission with declared config: %v", err)
	}
	for _, file := range submission.Files {
		if file.Path == defaultConfigName {
			t.Fatalf("Repo Run Config duplicated in submission files: %+v", submission.Files)
		}
	}
}

func TestControlPlaneSubmissionRejectsOversizedPayloadBeforeUpload(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	if err := os.Truncate(filepath.Join(repoDir, "scenes", "start.ma"), maximumControlPlaneSubmissionBytes); err != nil {
		t.Fatalf("create oversized payload: %v", err)
	}

	if _, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke"}); err == nil {
		t.Fatal("oversized Control Plane payload was accepted for upload")
	}
}

func TestStatusContractMatchesEmbeddedAndConfiguredModes(t *testing.T) {
	for _, mode := range []string{"embedded", "configured"} {
		t.Run(mode, func(t *testing.T) {
			repoDir := writeRunConfigFixture(t)
			runtime := defaultRunRuntime()
			var modeArgs []string
			if mode == "configured" {
				handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
				if err != nil {
					t.Fatalf("create Control Plane handler: %v", err)
				}
				server := httptest.NewTLSServer(handler)
				t.Cleanup(server.Close)
				t.Setenv(defaultControlPlaneTokenEnv, "test-token")
				runtime.ControlPlaneHTTPClient = server.Client()
				modeArgs = []string{"--control-plane", server.URL}
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			runArgs := append([]string{"run", "--json"}, modeArgs...)
			runArgs = append(runArgs, "smoke")
			if code := RunWithRuntime(runArgs, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
				t.Fatalf("%s run exit code = %d; stdout: %s stderr: %s", mode, code, stdout.String(), stderr.String())
			}
			runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
			stdout.Reset()
			stderr.Reset()
			statusArgs := append([]string{"status", "--json", "--run", runID}, modeArgs...)
			if code := RunWithRuntime(statusArgs, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
				t.Fatalf("%s status exit code = %d; stdout: %s stderr: %s", mode, code, stdout.String(), stderr.String())
			}
			var status controlPlaneStatusResponse
			if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
				t.Fatalf("decode %s status: %v", mode, err)
			}
			if status.Version != 1 || status.Kind != "status" || status.RunID != runID || status.State != "completed" || status.Status != resultStatusPassed || status.CleanupState != "completed" {
				t.Fatalf("%s status = %+v", mode, status)
			}
		})
	}
}

func TestRunReadContractsExerciseEmbeddedAndConfiguredModesThroughOneSeam(t *testing.T) {
	for _, mode := range []string{"embedded", "configured"} {
		t.Run(mode, func(t *testing.T) {
			repoDir := writeRunConfigFixture(t)
			runtime := defaultRunRuntime()
			var modeArgs []string
			if mode == "configured" {
				handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
				if err != nil {
					t.Fatalf("create Control Plane handler: %v", err)
				}
				server := httptest.NewTLSServer(handler)
				t.Cleanup(server.Close)
				t.Setenv(defaultControlPlaneTokenEnv, "test-token")
				runtime.ControlPlaneHTTPClient = server.Client()
				modeArgs = []string{"--control-plane", server.URL}
			}
			var stdout bytes.Buffer
			var stderr bytes.Buffer
			runArgs := append([]string{"run", "--json"}, modeArgs...)
			runArgs = append(runArgs, "smoke")
			if code := RunWithRuntime(runArgs, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
				t.Fatalf("%s run exit code = %d; stdout: %s stderr: %s", mode, code, stdout.String(), stderr.String())
			}
			runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID

			for _, resource := range []string{"status", "events", "logs", "result"} {
				stdout.Reset()
				stderr.Reset()
				var args []string
				if resource == "status" {
					args = []string{"status", "--json", "--run", runID}
				} else {
					args = []string{resource, "--json", runID}
				}
				args = append(args, modeArgs...)
				if code := RunWithRuntime(args, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
					t.Fatalf("%s %s exit code = %d; stdout: %s stderr: %s", mode, resource, code, stdout.String(), stderr.String())
				}
				var envelope struct {
					Version int    `json:"version"`
					Kind    string `json:"kind"`
					RunID   string `json:"runId"`
				}
				if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
					t.Fatalf("decode %s %s contract: %v", mode, resource, err)
				}
				if envelope.Version != 1 || envelope.Kind != resource || envelope.RunID != runID {
					t.Fatalf("%s %s envelope = %+v", mode, resource, envelope)
				}
			}
		})
	}
}

func TestControlPlaneServeCommandBuildsAuthenticatedTLSServer(t *testing.T) {
	workDir := t.TempDir()
	dataDir := filepath.Join(workDir, "control-plane-data")
	certPath := filepath.Join(workDir, "tls.crt")
	keyPath := filepath.Join(workDir, "tls.key")
	mustWriteFile(t, certPath, "test certificate")
	mustWriteFile(t, keyPath, "test key")
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	called := false
	runtime.ControlPlaneServe = func(options controlPlaneServeOptions, handler http.Handler) error {
		called = true
		if options.Listen != "127.0.0.1:9443" || options.DataDir != dataDir || options.TLSCert != certPath || options.TLSKey != keyPath || options.HostLockIdle != 20*time.Minute || options.HostLockLifetime != 4*time.Hour || handler == nil {
			t.Fatalf("Control Plane serve options = %+v handler nil = %t", options, handler == nil)
		}
		return nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{
		"control-plane", "serve", "--listen", "127.0.0.1:9443", "--data-dir", dataDir,
		"--tls-cert", certPath, "--tls-key", keyPath,
		"--host-lock-idle-timeout", "20m", "--host-lock-hard-lifetime", "4h",
	}, &stdout, &stderr, workDir, "test-version", runtime)

	if code != 0 || !called || !strings.Contains(stdout.String(), "controlPlane: https://127.0.0.1:9443") {
		t.Fatalf("serve command = code %d called %t stdout %q stderr %q", code, called, stdout.String(), stderr.String())
	}
}

func TestControlPlaneRejectsIdleTimeoutBelowAgentHeartbeatSafetyMargin(t *testing.T) {
	_, err := parseControlPlaneServeArgs([]string{
		"--data-dir", "control-plane-data", "--host-lock-idle-timeout", (minimumHostLockIdleTimeout - time.Second).String(),
	}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "at least 30s") {
		t.Fatalf("short Host Lock idle timeout error = %v", err)
	}
}

func TestControlPlaneRejectsNonPrivateExistingDataDirWithoutChangingMode(t *testing.T) {
	dataDir := privateTempDir(t)
	mustWriteFile(t, filepath.Join(dataDir, "unrelated.txt"), "not Control Plane data")
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatalf("make shared data directory: %v", err)
	}

	if _, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime()); err == nil {
		t.Fatal("non-private existing Control Plane data directory was accepted")
	}
	info, err := os.Stat(dataDir)
	if err != nil {
		t.Fatalf("stat rejected data directory: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("rejected data directory mode = %04o, want unchanged 0755", info.Mode().Perm())
	}
}

func TestControlPlaneAPIRejectsMissingAuthenticationAndUnsupportedVersionWithoutMutation(t *testing.T) {
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)

	for _, test := range []struct {
		name          string
		token         string
		rawToken      bool
		version       int
		wantHTTPState int
	}{
		{name: "missing authentication", version: 1, wantHTTPState: http.StatusUnauthorized},
		{name: "token without bearer scheme", token: "test-token", rawToken: true, version: 1, wantHTTPState: http.StatusUnauthorized},
		{name: "unsupported version", token: "test-token", version: 2, wantHTTPState: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := json.Marshal(controlPlaneSubmission{Version: test.version, Scenario: "smoke", StopAfter: stopAfterAlways})
			if err != nil {
				t.Fatalf("marshal submission: %v", err)
			}
			request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/runs", bytes.NewReader(body))
			if err != nil {
				t.Fatalf("create submission request: %v", err)
			}
			if test.token != "" {
				if test.rawToken {
					request.Header.Set("Authorization", test.token)
				} else {
					request.Header.Set("Authorization", "Bearer "+test.token)
				}
			}
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("submit request: %v", err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != test.wantHTTPState {
				t.Fatalf("HTTP status = %d, want %d", response.StatusCode, test.wantHTTPState)
			}
		})
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		t.Fatalf("read Control Plane runs: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected API submissions created run state: %+v", entries)
	}
}

func TestControlPlaneContinuesAcceptedRunWhenSubmitterDisconnects(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	submission, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatalf("build Control Plane submission: %v", err)
	}
	body, err := json.Marshal(submission)
	if err != nil {
		t.Fatalf("marshal Control Plane submission: %v", err)
	}
	ran := false
	runtime := defaultRunRuntime()
	runtime.Broker = recordingFakeBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}, ran: &ran}
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://maya-stall.example.com/v1/runs", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := &failingControlPlaneResponseWriter{header: make(http.Header)}

	handler.ServeHTTP(response, request)

	if !ran {
		t.Fatal("Control Plane cancelled accepted Scenario after submitter disconnected")
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("accepted run directories = %d, err %v", len(entries), err)
	}
	runID := entries[0].Name()
	record, err := readRunLedgerRecord(filepath.Join(dataDir, "runs", runID, "repo"), runID)
	if err != nil {
		t.Fatalf("read accepted run after disconnect: %v", err)
	}
	if record.State != "completed" || record.Status != resultStatusPassed {
		t.Fatalf("accepted run after disconnect = %+v", record)
	}
	restartedHandler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("restart Control Plane: %v", err)
	}
	restarted := httptest.NewTLSServer(restartedHandler)
	t.Cleanup(restarted.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	clientRuntime := defaultRunRuntime()
	clientRuntime.ControlPlaneHTTPClient = restarted.Client()
	for _, resource := range []string{"status", "events", "logs", "result", "evidence"} {
		var response map[string]any
		if err := getControlPlaneJSON(restarted.URL, "", "/v1/runs/"+runID+"/"+resource, clientRuntime, &response); err != nil {
			t.Fatalf("read completed %s after disconnect: %v", resource, err)
		}
		if response["runId"] != runID {
			t.Fatalf("completed %s Run ID = %v, want %s", resource, response["runId"], runID)
		}
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"history", "--json", "--control-plane", restarted.URL}, &stdout, &stderr, repoDir, "test-version", clientRuntime); code != 0 || !strings.Contains(stdout.String(), runID) {
		t.Fatalf("completed history after disconnect = code %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"attach", runID, "--control-plane", restarted.URL, "--from-sequence", "1"}, &stdout, &stderr, repoDir, "test-version", clientRuntime); code != 0 || !strings.Contains(stdout.String(), "cleanupState: completed") {
		t.Fatalf("completed attach after disconnect = code %d stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
}

func TestControlPlaneContinuesAcceptedRunAfterDisconnectDuringExecution(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	submission, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatalf("build Control Plane submission: %v", err)
	}
	body, _ := json.Marshal(submission)
	started := make(chan struct{})
	release := make(chan struct{})
	runtime := defaultRunRuntime()
	runtime.Broker = blockingFakeBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}}, started: started, release: release}
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://maya-stall.example.com/v1/runs", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	<-started
	cancel()
	close(release)
	<-done
	assertOnlyControlPlaneRunState(t, dataDir, "completed")
}

func TestControlPlaneContinuesAcceptedRunAfterDisconnectDuringCleanup(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	submission, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatalf("build Control Plane submission: %v", err)
	}
	body, _ := json.Marshal(submission)
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	runtime := defaultRunRuntime()
	runtime.Broker = cleanupBlockingFakeBroker{
		fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed}},
		started:           cleanupStarted, release: releaseCleanup,
	}
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://maya-stall.example.com/v1/runs", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), request)
		close(done)
	}()
	<-cleanupStarted
	cancel()
	close(releaseCleanup)
	<-done
	assertOnlyControlPlaneRunState(t, dataDir, "completed")
}

func assertOnlyControlPlaneRunState(t *testing.T, dataDir string, want string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("Control Plane run directories = %d, err %v", len(entries), err)
	}
	runID := entries[0].Name()
	record, err := readRunLedgerRecord(filepath.Join(dataDir, "runs", runID, "repo"), runID)
	if err != nil || record.State != want {
		t.Fatalf("Control Plane run after disconnect = %+v, err %v, want state %s", record, err, want)
	}
}

func TestConfiguredControlPlaneReadsPersistAcrossServerRestart(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	server.Close()

	restartedHandler, err := newControlPlaneHandler(dataDir, "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("restart Control Plane handler: %v", err)
	}
	restarted := httptest.NewTLSServer(restartedHandler)
	t.Cleanup(restarted.Close)
	runtime.ControlPlaneHTTPClient = restarted.Client()
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"status", "--json", "--control-plane", restarted.URL, "--run", runID}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("restarted status exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var status controlPlaneStatusResponse
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil || status.RunID != runID || status.State != "completed" {
		t.Fatalf("restarted status = %+v err %v", status, err)
	}
}

func TestConfiguredControlPlaneRunReadsHaveHumanOutput(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	handler, err := newControlPlaneHandler(privateTempDir(t), "test-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "test-token")
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("shared run exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runID := decodeRunJSONLines(t, stdout.Bytes())[0].RunID
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"status", "--control-plane", server.URL, "--run", runID}, want: "cleanupState: completed"},
		{args: []string{"events", "--control-plane", server.URL, runID}, want: "events:"},
		{args: []string{"logs", "--control-plane", server.URL, runID}, want: "fake Session Broker ran Scenario"},
		{args: []string{"result", "--control-plane", server.URL, runID}, want: "success: true"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := RunWithRuntime(test.args, &stdout, &stderr, repoDir, "test-version", runtime); code != 0 || !strings.Contains(stdout.String(), test.want) {
			t.Fatalf("human command %v = code %d stdout %q stderr %q", test.args, code, stdout.String(), stderr.String())
		}
	}
}

type cleanupFailingFakeBroker struct {
	fakeSessionBroker
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("make private temp directory: %v", err)
	}
	return dir
}

type blockingFakeBroker struct {
	fakeSessionBroker
	started chan struct{}
	release chan struct{}
}

type cleanupBlockingFakeBroker struct {
	fakeSessionBroker
	started chan struct{}
	release chan struct{}
}

type longLogFakeBroker struct{ fakeSessionBroker }

type recordingFakeBroker struct {
	fakeSessionBroker
	ran *bool
}

func (broker recordingFakeBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	*broker.ran = true
	return broker.fakeSessionBroker.RunScenario(context, scenario)
}

type failingControlPlaneResponseWriter struct {
	header http.Header
}

func (writer *failingControlPlaneResponseWriter) Header() http.Header { return writer.header }
func (*failingControlPlaneResponseWriter) WriteHeader(int)            {}
func (*failingControlPlaneResponseWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected")
}

func (broker blockingFakeBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	close(broker.started)
	<-broker.release
	return broker.fakeSessionBroker.RunScenario(context, scenario)
}

func (broker cleanupBlockingFakeBroker) StopSession(context runContext, session brokerSessionIdentity) error {
	close(broker.started)
	<-broker.release
	return broker.fakeSessionBroker.StopSession(context, session)
}

func (broker longLogFakeBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	result, err := broker.fakeSessionBroker.RunScenario(context, scenario)
	if writeErr := os.WriteFile(context.LogPath, []byte(strings.Repeat("bounded log payload ", 32)), 0o644); writeErr != nil {
		return ScenarioResult{}, writeErr
	}
	return result, err
}

func (cleanupFailingFakeBroker) StopSession(runContext, brokerSessionIdentity) error {
	return errors.New("fake cleanup failed")
}
