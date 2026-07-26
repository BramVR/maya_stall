package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestQueuedRunCancellationIsDurableAndDoesNotTouchHost(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	var registered hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now()), runtime, http.StatusOK, &registered); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "locked")
	firstRunID := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01").RunID

	secondDone := make(chan int, 1)
	var secondOutput bytes.Buffer
	go func() {
		secondDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &secondOutput, io.Discard, repoDir, "test-version", runtime)
	}()
	queuedRunID := waitForQueuedRunID(t, handler)
	evidenceRequest, err := http.NewRequest(http.MethodGet, server.URL+"/v1/runs/"+queuedRunID+"/evidence", nil)
	if err != nil {
		t.Fatal(err)
	}
	evidenceRequest.Header.Set("Authorization", "Bearer operator-token")
	evidenceResponse, err := server.Client().Do(evidenceRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = evidenceResponse.Body.Close()
	if evidenceResponse.StatusCode != http.StatusConflict {
		t.Fatalf("queued Evidence HTTP status = %d, want 409", evidenceResponse.StatusCode)
	}
	var statusOutput bytes.Buffer
	if code := RunWithRuntime([]string{"status", "--control-plane", server.URL, "--run", queuedRunID}, &statusOutput, io.Discard, repoDir, "test-version", runtime); code != 0 || !bytes.Contains(statusOutput.Bytes(), []byte("queuePosition: 1\nwaitReason: compatible-hosts-busy\n")) {
		t.Fatalf("human queued status exit code = %d, output = %q", code, statusOutput.String())
	}
	if err := postControlPlaneJSON(server.URL, "", "/v1/runs/"+queuedRunID+"/cancel", controlPlaneQueueCancelRequest{Version: controlPlaneAPIVersion + 1}, runtime, http.StatusBadRequest, nil); err != nil {
		t.Fatalf("reject unsupported queue cancellation: %v", err)
	}
	var stopOutput bytes.Buffer
	if code := RunWithRuntime([]string{"stop", "--control-plane", server.URL, queuedRunID}, &stopOutput, io.Discard, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("cancel queued Run exit code = %d", code)
	}
	if status := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01"); status.RunID != firstRunID || status.State != "locked" {
		t.Fatalf("queued cancellation mutated active Host: %+v", status)
	}
	var status controlPlaneStatusResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+queuedRunID+"/status", runtime, &status); err != nil {
		t.Fatalf("read canceled Run status: %v", err)
	}
	if status.State != "canceled" || status.CleanupState != "not-required" || status.QueuePosition != 0 || status.Host != "" {
		t.Fatalf("canceled Run status = %+v", status)
	}
	if _, err := os.Lstat(filepath.Join(dataDir, "runs", queuedRunID, "repo", ".maya-stall", "state", "runs", queuedRunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled Run retained transient state: %v", err)
	}
	var events controlPlaneEventsResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+queuedRunID+"/events", runtime, &events); err != nil {
		t.Fatalf("read canceled Run events: %v", err)
	}
	if len(events.Events) != 3 || events.Events[0]["type"] != "run.accepted" || events.Events[1]["type"] != "run.queued" || events.Events[2]["type"] != "run.canceled" {
		t.Fatalf("canceled Run events = %+v", events.Events)
	}
	var terminal controlPlaneResultResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+queuedRunID+"/result", runtime, &terminal); err != nil {
		t.Fatalf("read canceled Run result: %v", err)
	}
	if !terminal.Final || terminal.Success || terminal.State != "canceled" || terminal.CleanupState != "not-required" {
		t.Fatalf("canceled Run result = %+v", terminal)
	}
	select {
	case code := <-secondDone:
		if code != 1 {
			t.Fatalf("canceled submitter exit code = %d, want 1", code)
		}
		results := decodeRunJSONLines(t, secondOutput.Bytes())
		if len(results) != 2 || results[1].FailedLayer != string(failureLayerHostSelection) || results[1].Diagnostic != errQueuedRunCanceled.Error() {
			t.Fatalf("canceled submitter result = %+v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled submitter did not finish")
	}

	simulateHostAgentProcessExit(t, handler, "windows-agent-01")
	var agentStderr bytes.Buffer
	if code := RunWithRuntime([]string{"host-agent", "run-once", "--control-plane", server.URL, "--agent-id", "windows-agent-01", "--host", "maya-win-01", "--work-root", agentWorkRoot, "--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL"}, io.Discard, &agentStderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("finish active Run exit code = %d; stderr: %s", code, agentStderr.String())
	}
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("active Run did not finish")
	}
}

func TestQueueRecoveryReplaysWriteAheadRecordExactlyOnce(t *testing.T) {
	dataDir := privateTempDir(t)
	if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err != nil {
		t.Fatalf("create Control Plane layout: %v", err)
	}
	runID := "20260717T120000.000000000Z"
	repoDir := filepath.Join(dataDir, "runs", runID, "repo")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture := writeRunConfigFixture(t)
	config, err := os.ReadFile(filepath.Join(fixture, ".maya-stall.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".maya-stall.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	submission := controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}
	run := newFreshRun(repoDir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, defaultRunRuntime()).(*freshRunLifecycle)
	if err := run.accept(); err != nil {
		t.Fatalf("accept recovery fixture: %v", err)
	}
	record := controlPlaneQueueRecord{Version: controlPlaneQueueVersion, RunID: runID, State: "admitting", Submission: submission, HostPool: "default"}
	if err := writePrivateJSON(filepath.Join(dataDir, "queued-runs", runID+".json"), record); err != nil {
		t.Fatalf("write queue intent: %v", err)
	}
	if err := os.RemoveAll(run.context.StateDir); err != nil {
		t.Fatalf("simulate lost transient state: %v", err)
	}

	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("recover Control Plane queue: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	ledger, err := readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "queued" {
		t.Fatalf("recovered ledger = %+v, error = %v", ledger, err)
	}
	if _, _, found, err := readStopRunManifest(repoDir, runID); err != nil || !found {
		t.Fatalf("reconstructed queued manifest: found=%t error=%v", found, err)
	}
	events, err := readControlPlaneEvents(repoDir, ledger)
	if err != nil || len(events.Events) != 2 || events.Events[1]["type"] != "run.queued" {
		t.Fatalf("recovered queue events = %+v, error = %v", events.Events, err)
	}
	handler.mu.Lock()
	if len(handler.queuedRuns) != 1 || handler.queuedRuns[runID] == nil {
		handler.mu.Unlock()
		t.Fatalf("recovered queue = %+v", handler.queuedRuns)
	}
	queued := handler.queuedRuns[runID]
	queued.canceled = true
	queued.record.State = "canceling"
	if err := writePrivateJSON(handler.queuePath(runID), queued.record); err != nil {
		handler.mu.Unlock()
		t.Fatalf("persist cancellation intent fixture: %v", err)
	}
	handler.mu.Unlock()
	restartedValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("recover queued cancellation intent: %v", err)
	}
	handler.mu.Lock()
	delete(handler.queuedRuns, runID)
	close(queued.done)
	handler.mu.Unlock()
	restarted := restartedValue.(*controlPlaneHandler)
	if len(restarted.queuedRuns) != 0 {
		t.Fatalf("recovered canceled queue = %+v", restarted.queuedRuns)
	}
	ledger, err = readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "canceled" {
		t.Fatalf("recovered cancellation ledger = %+v, error = %v", ledger, err)
	}
	events, err = readControlPlaneEvents(repoDir, ledger)
	if err != nil || len(events.Events) != 3 || events.Events[2]["type"] != "run.canceled" {
		t.Fatalf("recovered cancellation events = %+v, error = %v", events.Events, err)
	}
}

func TestQueueRecoveryCleansRecognizedAtomicWriteTemporary(t *testing.T) {
	dataDir := privateTempDir(t)
	if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err != nil {
		t.Fatal(err)
	}
	temporary := filepath.Join(dataDir, "queued-runs", ".20260717T120000.000000000Z.json.tmp-crash")
	if err := os.WriteFile(temporary, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err != nil {
		t.Fatalf("restart with atomic queue temporary: %v", err)
	}
	if _, err := os.Lstat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("atomic queue temporary remains: %v", err)
	}
}

func TestQueueRecoveryRemovesAbandonedPreLedgerAdmission(t *testing.T) {
	dataDir := privateTempDir(t)
	if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err != nil {
		t.Fatal(err)
	}
	runID := "20260717T120000.000000000Z"
	runRoot := filepath.Join(dataDir, "runs", runID)
	repoDir := filepath.Join(runRoot, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "submitted.txt"), []byte("snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := controlPlaneQueueRecord{Version: controlPlaneQueueVersion, RunID: runID, State: "admitting", Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways}, HostPool: "windows-maya"}
	if err := writePrivateJSON(filepath.Join(dataDir, "queued-runs", runID+".json"), record); err != nil {
		t.Fatal(err)
	}
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("recover pre-ledger admission: %v", err)
	}
	if _, err := os.Lstat(runRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned Run root remains: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	for _, indexedRunID := range handler.runIDs {
		if indexedRunID == runID {
			t.Fatalf("abandoned Run remains indexed: %+v", handler.runIDs)
		}
	}
}

func TestQueueRecoveryPreservesInterruptedLedgerInitialization(t *testing.T) {
	dataDir := privateTempDir(t)
	if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err != nil {
		t.Fatal(err)
	}
	runID := "20260717T120000.000000000Z"
	repoDir := filepath.Join(dataDir, "runs", runID, "repo")
	partialPath := filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName)
	if err := os.MkdirAll(filepath.Dir(partialPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partialPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := controlPlaneQueueRecord{Version: controlPlaneQueueVersion, RunID: runID, State: "admitting", Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways}, HostPool: "windows-maya"}
	queuePath := filepath.Join(dataDir, "queued-runs", runID+".json")
	if err := writePrivateJSON(queuePath, record); err != nil {
		t.Fatal(err)
	}

	if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err == nil || !strings.Contains(err.Error(), "has no recoverable durable ledger") {
		t.Fatalf("recover interrupted ledger error = %v", err)
	}
	if content, err := os.ReadFile(partialPath); err != nil || string(content) != "partial" {
		t.Fatalf("interrupted ledger changed: content %q, error %v", content, err)
	}
	if _, err := os.Stat(queuePath); err != nil {
		t.Fatalf("queue intent removed: %v", err)
	}
}

func TestConfiguredStopRequiresExactCanceledStatus(t *testing.T) {
	runID := "20260717T120000.000000000Z"
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	tests := []struct {
		name       string
		status     controlPlaneStatusResponse
		wantResult string
	}{
		{name: "empty", status: controlPlaneStatusResponse{}},
		{name: "wrong run", status: controlPlaneStatusResponse{Version: controlPlaneAPIVersion, Kind: "status", RunID: "20260717T120001.000000000Z", State: "canceled"}},
		{name: "not canceled", status: controlPlaneStatusResponse{Version: controlPlaneAPIVersion, Kind: "status", RunID: runID, State: "queued"}},
		{name: "canceled", status: controlPlaneStatusResponse{Version: controlPlaneAPIVersion, Kind: "status", RunID: runID, State: "canceled"}, wantResult: "stopped"},
		{name: "cleanup requested", status: controlPlaneStatusResponse{Version: controlPlaneAPIVersion, Kind: "status", RunID: runID, State: "expiring"}, wantResult: "stop-requested"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(response).Encode(test.status)
			}))
			defer server.Close()
			runtime := defaultRunRuntime()
			runtime.ControlPlaneHTTPClient = server.Client()
			result, err := stopRunThroughMode(t.TempDir(), stopOptions{RunID: runID, ControlPlane: server.URL}, runtime)
			if test.wantResult == "" {
				if err == nil {
					t.Fatalf("configured stop result = %q, want error", result)
				}
			} else if err != nil || result != test.wantResult {
				t.Fatalf("configured stop = %q, %v; want %q, nil", result, err, test.wantResult)
			}
		})
	}
}

func TestQueueOrderingAndCompatibilityAreReevaluatedDeterministically(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	host := &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-1", HostID: "maya-win-01", State: "ready", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	handler.hostAgents["agent-1"] = host
	earlier := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}
	later := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-b", State: "queued", AcceptedAt: now.Add(time.Second).Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}
	handler.queuedRuns["run-a"] = earlier
	handler.queuedRuns["run-b"] = later
	queueStatus := controlPlaneStatusResponse{RunID: "run-a", TargetProfile: "default"}
	handler.addControlPlaneQueueStatus(&queueStatus)
	if queueStatus.HostPool != "windows-maya" || queueStatus.TargetProfile == queueStatus.HostPool {
		t.Fatalf("queued Host Pool identity = %+v", queueStatus)
	}
	host.status.Capabilities.Maintenance = true
	if selected, _, _, _, _ := handler.selectQueuedRun("run-a"); selected != nil || handler.queueWaitReasonLocked(earlier) != "waiting-for-compatible-host" {
		t.Fatal("maintenance Host qualified for queued Run")
	}
	host.status.Capabilities.Maintenance = false
	host.status.Capabilities.TargetProfileHostPools["default"] = "other-pool"
	if selected, _, _, _, _ := handler.selectQueuedRun("run-a"); selected != nil {
		t.Fatal("Host outside admitted Host Pool qualified for queued Run")
	}
	if reason := handler.queueWaitReasonLocked(earlier); reason != "waiting-for-compatible-host" {
		t.Fatalf("remapped Host Pool wait reason = %q", reason)
	}
	host.status.Capabilities.TargetProfileHostPools["default"] = "windows-maya"
	host.status.State = "offline"
	if selected, _, _, _, _ := handler.selectQueuedRun("run-a"); selected != nil {
		t.Fatal("offline Host qualified for queued Run")
	}
	host.status.State = "ready"
	report2 := report
	host2 := &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-2", HostID: "maya-win-02", State: "ready", Slots: 1, SessionID: "session-2", SessionBinding: true, DeadlineActions: true, Capabilities: report2}, sessionExpiresAt: now.Add(time.Minute)}
	handler.hostAgents["agent-2"] = host2
	selected2, _, _, release2, _ := handler.selectQueuedRun("run-b")
	if selected2 != host2 || release2 == nil {
		t.Fatal("second compatible Host did not advance second queued Run")
	}
	selected, _, _, release, _ := handler.selectQueuedRun("run-a")
	if selected != host || release == nil {
		t.Fatal("compatible Host did not advance earliest queued Run")
	}
	var assignment hostAgentAssignmentRecord
	selected.status.Capabilities.Maintenance = true
	if err := handler.refreshQueuedReservationCapabilitiesLocked("run-a", "default", scenarioRequirements{}, selected, &assignment); err == nil {
		t.Fatal("maintained Host passed final reservation compatibility check")
	}
	selected.status.Capabilities.Maintenance = false
	if err := handler.refreshQueuedReservationCapabilitiesLocked("run-a", "default", scenarioRequirements{}, selected, &assignment); err != nil || assignment.Capabilities.Version != mayaHostCapabilityRecordVersion {
		t.Fatalf("fresh final reservation compatibility = %+v, error %v", assignment.Capabilities, err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if err := release2(); err != nil {
		t.Fatal(err)
	}
	canceling := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-c", State: "canceling"}, canceled: true, cancellationActive: true}
	handler.queuedRuns["run-c"] = canceling
	if _, err := handler.cancelQueuedRun("run-c"); !errors.Is(err, errQueuedRunCanceling) {
		t.Fatalf("duplicate cancellation error = %v", err)
	}
	if ordered := handler.orderedQueuedRunsLocked(); len(ordered) != 2 {
		t.Fatalf("public queue order includes cancellation intent: %+v", ordered)
	}
	chronological := &controlPlaneHandler{queuedRuns: map[string]*controlPlaneQueuedRun{
		"earlier-z": {record: controlPlaneQueueRecord{RunID: "earlier-z", AcceptedAt: "2026-07-17T12:00:00Z"}},
		"later-a":   {record: controlPlaneQueueRecord{RunID: "later-a", AcceptedAt: "2026-07-17T12:00:00.1Z"}},
	}}
	ordered := chronological.orderedQueuedRunsLocked()
	if ordered[0].record.RunID != "earlier-z" {
		t.Fatalf("variable-width timestamp order = %s before %s", ordered[0].record.RunID, ordered[1].record.RunID)
	}
}

func TestQueuedCancellationCleanupFailureIsDurableAndRetryable(t *testing.T) {
	fixture := writeRunConfigFixture(t)
	submission, err := buildControlPlaneSubmission(fixture, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := privateTempDir(t)
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	runID := "20260717T120000.000000000Z"
	repoDir := filepath.Join(dataDir, "runs", runID, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := materializeControlPlaneSubmission(repoDir, submission); err != nil {
		t.Fatal(err)
	}
	run := newFreshRun(repoDir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, defaultRunRuntime()).(*freshRunLifecycle)
	if err := run.accept(); err != nil {
		t.Fatal(err)
	}
	if err := markControlPlaneRunQueued(repoDir, runID, run.runtime.Now()); err != nil {
		t.Fatal(err)
	}
	record := controlPlaneQueueRecord{Version: controlPlaneQueueVersion, RunID: runID, State: "queued", AcceptedAt: run.acceptedAt.Format(time.RFC3339Nano), Submission: submission, HostPool: "default"}
	handler.queuedRuns[runID] = &controlPlaneQueuedRun{record: record, run: run, done: make(chan struct{})}
	stateRuns := filepath.Join(repoDir, ".maya-stall", "state", "runs")
	if err := os.Chmod(stateRuns, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateRuns, 0o755) })
	if _, err := handler.cancelQueuedRun(runID); err == nil {
		t.Fatal("read-only transient state cleanup succeeded")
	}
	ledger, err := readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "cleanup-failed" {
		t.Fatalf("cancellation cleanup failure ledger = %+v, error = %v", ledger, err)
	}
	if queued := handler.queuedRuns[runID]; queued == nil || !queued.canceled || queued.cancellationActive {
		t.Fatalf("failed cancellation was not left retryable: %+v", queued)
	}
	select {
	case <-handler.queuedRuns[runID].done:
	default:
		t.Fatal("terminal cleanup-failed cancellation did not release submitter")
	}
	restartedValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("restart with retryable cancellation failure: %v", err)
	}
	restarted := restartedValue.(*controlPlaneHandler)
	if retry := restarted.queuedRuns[runID]; retry == nil || !retry.canceled || retry.record.State != "canceling" {
		t.Fatalf("recovered retryable cancellation = %+v", retry)
	}
	status := controlPlaneStatusFromRecord(repoDir, ledger, "/v1/runs/"+runID+"/evidence")
	handler.addControlPlaneQueueStatus(&status)
	if status.State != "cleanup-failed" || status.QueuePosition != 0 || status.CleanupState != "failed" {
		t.Fatalf("retryable cancellation status = %+v", status)
	}
	if err := os.Chmod(stateRuns, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.cancelQueuedRun(runID); err != nil {
		t.Fatalf("retry cancellation cleanup: %v", err)
	}
	ledger, err = readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "canceled" {
		t.Fatalf("retried cleanup ledger = %+v, error = %v", ledger, err)
	}
	if _, err := os.Lstat(filepath.Join(stateRuns, runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retried cleanup retained transient state: %v", err)
	}
}

func TestQueuedCancellationRemainsCancelingUntilCleanup(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	runID := "20260717T120000.000000000Z"
	run := newFreshRun(repoDir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, defaultRunRuntime()).(*freshRunLifecycle)
	if err := run.accept(); err != nil {
		t.Fatal(err)
	}
	if err := markControlPlaneRunQueued(repoDir, runID, run.runtime.Now()); err != nil {
		t.Fatal(err)
	}
	if err := prepareQueuedRunCancellation(run); err != nil {
		t.Fatal(err)
	}
	ledger, err := readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "canceling" || ledger.CompletedAt != "" {
		t.Fatalf("prepared cancellation ledger = %+v, error = %v", ledger, err)
	}
	if _, err := os.Lstat(run.context.StateDir); err != nil {
		t.Fatalf("prepared cancellation removed transient state early: %v", err)
	}
	if err := cleanupQueuedRunCancellation(repoDir, runID, run.runtime.Now()); err != nil {
		t.Fatal(err)
	}
	ledger, err = readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "canceled" || ledger.CompletedAt == "" {
		t.Fatalf("completed cancellation ledger = %+v, error = %v", ledger, err)
	}
}

func TestQueuedCancellationIntentRemovalFailureReleasesSubmitter(t *testing.T) {
	fixture := writeRunConfigFixture(t)
	submission, err := buildControlPlaneSubmission(fixture, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := privateTempDir(t)
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	runID := "20260717T120000.000000000Z"
	repoDir := filepath.Join(dataDir, "runs", runID, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := materializeControlPlaneSubmission(repoDir, submission); err != nil {
		t.Fatal(err)
	}
	run := newFreshRun(repoDir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, defaultRunRuntime()).(*freshRunLifecycle)
	if err := run.accept(); err != nil {
		t.Fatal(err)
	}
	if err := markControlPlaneRunQueued(repoDir, runID, run.runtime.Now()); err != nil {
		t.Fatal(err)
	}
	record := controlPlaneQueueRecord{Version: controlPlaneQueueVersion, RunID: runID, State: "canceling", AcceptedAt: run.acceptedAt.Format(time.RFC3339Nano), Submission: submission, HostPool: "default"}
	queued := &controlPlaneQueuedRun{record: record, run: run, canceled: true, done: make(chan struct{})}
	handler.queuedRuns[runID] = queued
	if err := writePrivateJSON(handler.queuePath(runID), record); err != nil {
		t.Fatal(err)
	}
	queueDir := filepath.Join(dataDir, "queued-runs")
	if err := os.Chmod(queueDir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(queueDir, 0o700) })
	if _, err := handler.cancelQueuedRun(runID); err == nil {
		t.Fatal("read-only queue intent removal succeeded")
	}
	ledger, err := readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "canceled" {
		t.Fatalf("terminal cancellation ledger = %+v, error = %v", ledger, err)
	}
	select {
	case <-queued.done:
	default:
		t.Fatal("terminal cancellation did not release submitter after intent cleanup failure")
	}
	if !queued.canceled || queued.cancellationActive {
		t.Fatalf("intent cleanup failure was not left inert and retryable: %+v", queued)
	}
	if err := os.Chmod(queueDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.cancelQueuedRun(runID); err != nil {
		t.Fatalf("retry intent cleanup: %v", err)
	}
}

func TestQueueWaitReasonPrefersReadyCompatibleHost(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	handler.hostAgents["busy"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "busy", HostID: "maya-win-01", State: "locked", Slots: 1, RunID: "active", SessionID: "busy-session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	handler.hostAgents["ready"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "ready", HostID: "maya-win-02", State: "ready", Slots: 1, SessionID: "ready-session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	queued := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}}
	for range 100 {
		if reason := handler.queueWaitReasonLocked(queued); reason != "awaiting-host-assignment" {
			t.Fatalf("mixed compatible Host wait reason = %q", reason)
		}
	}
}

func TestQueueWaitReasonExcludesSessionUnboundHost(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	handler.hostAgents["legacy"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "legacy", HostID: "maya-win-01", State: "ready", Slots: 1, SessionID: "legacy-session", SessionBinding: false, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	queued := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}}
	if reason := handler.queueWaitReasonLocked(queued); reason != "waiting-for-compatible-host" {
		t.Fatalf("session-unbound Host wait reason = %q", reason)
	}
}

func TestQueuedCancellationCleanupFailureIsDurable(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	runID := "20260717T120000.000000000Z"
	run := newFreshRun(repoDir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, defaultRunRuntime()).(*freshRunLifecycle)
	if err := run.accept(); err != nil {
		t.Fatal(err)
	}
	if err := markControlPlaneRunQueued(repoDir, runID, run.runtime.Now()); err != nil {
		t.Fatal(err)
	}
	stateRuns := filepath.Join(repoDir, ".maya-stall", "state", "runs")
	realStateRuns := stateRuns + "-real"
	if err := os.Rename(stateRuns, realStateRuns); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realStateRuns, stateRuns); err != nil {
		t.Fatal(err)
	}
	if err := cleanupQueuedRunCancellation(repoDir, runID, run.runtime.Now()); err == nil {
		t.Fatal("symlinked cancellation cleanup succeeded")
	}
	ledger, err := readRunLedgerRecord(repoDir, runID)
	if err != nil || ledger.State != "cleanup-failed" {
		t.Fatalf("cleanup failure ledger = %+v, error = %v", ledger, err)
	}
}

func TestQueueAdmissionIsBoundedBeforeRunAcceptance(t *testing.T) {
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	for index := 0; index < maximumControlPlaneQueuedRuns; index++ {
		runID := fmt.Sprintf("queued-%04d", index)
		handler.queuedRuns[runID] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: runID}}
	}
	repoDir := writeRunConfigFixture(t)
	run, _, _, _, _, err := handler.queueHostAgentRun(repoDir, controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways}, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways}, scenarioRequirements{}, defaultRunRuntime())
	if !errors.Is(err, errControlPlaneQueueFull) || run != nil {
		t.Fatalf("full queue admission run = %+v, error = %v", run, err)
	}
	if _, err := os.Lstat(filepath.Join(repoDir, ".maya-stall", "state", "ledger")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("full queue created Run Ledger: %v", err)
	}
	for _, queued := range handler.queuedRuns {
		queued.canceled = true
	}
	run, _, _, _, _, err = handler.queueHostAgentRun(repoDir, controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways}, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways}, scenarioRequirements{}, defaultRunRuntime())
	if errors.Is(err, errControlPlaneQueueFull) || run != nil {
		t.Fatalf("cancellation intents consumed queue capacity: run=%+v error=%v", run, err)
	}
}

func TestFullQueueRejectsSubmissionWithoutOrphanedRunState(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	submission, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := privateTempDir(t)
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	handler.hostAgents["agent-1"] = &controlPlaneHostAgent{}
	for index := 0; index < maximumControlPlaneQueuedRuns; index++ {
		runID := fmt.Sprintf("queued-%04d", index)
		handler.queuedRuns[runID] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: runID}}
	}
	request := httptest.NewRequest(http.MethodPost, "https://maya-stall.example.com/v1/runs", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusTooManyRequests || !strings.Contains(response.Body.String(), "run queue is full") {
		t.Fatalf("full queue response = %d %s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("full queue run directories = %+v, error = %v", entries, err)
	}
}

func TestFullQueueCleanupFailureKeepsRejectedRunVisible(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	submission, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(submission)
	if err != nil {
		t.Fatal(err)
	}
	dataDir := privateTempDir(t)
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	handler.hostAgents["agent-1"] = &controlPlaneHostAgent{}
	for index := 0; index < maximumControlPlaneQueuedRuns; index++ {
		runID := fmt.Sprintf("queued-%04d", index)
		handler.queuedRuns[runID] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: runID}}
	}
	cleanupErr := errors.New("injected rejected-run cleanup failure")
	handler.removeRejectedRunRoot = func(string) error { return cleanupErr }
	request := httptest.NewRequest(http.MethodPost, "https://maya-stall.example.com/v1/runs", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer operator-token")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), "clean up rejected run") {
		t.Fatalf("failed overflow cleanup response = %d %s", response.Code, response.Body.String())
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil || len(entries) != 1 || len(handler.runIDs) != 1 || handler.runIDs[0] != entries[0].Name() {
		t.Fatalf("failed overflow cleanup visibility: entries=%+v runIDs=%+v error=%v", entries, handler.runIDs, err)
	}
}

func TestQueueSchedulerScansCentrallyAtCapacity(t *testing.T) {
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	handler.mu.Lock()
	for index := 0; index < maximumControlPlaneQueuedRuns; index++ {
		runID := fmt.Sprintf("queued-%04d", index)
		handler.queuedRuns[runID] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: runID}, ready: make(chan struct{}, 1)}
	}
	handler.startQueueSchedulerLocked()
	handler.mu.Unlock()
	time.Sleep(130 * time.Millisecond)
	handler.mu.Lock()
	cycles := handler.queueDispatchCycles
	for _, queued := range handler.queuedRuns {
		queued.canceled = true
	}
	handler.mu.Unlock()
	if cycles == 0 || cycles > 10 {
		t.Fatalf("central queue dispatch cycles in 130ms = %d", cycles)
	}
}

func TestQueueAdmissionSerializesAcceptanceThroughDurableInsertion(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	handler.hostAgents["agent-1"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-1", HostID: "maya-win-01", State: "locked", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})
	firstRuntime := defaultRunRuntime()
	secondRuntime := defaultRunRuntime()
	var firstOnce sync.Once
	var secondOnce sync.Once
	firstRuntime.Now = func() time.Time {
		firstOnce.Do(func() {
			close(firstEntered)
			<-releaseFirst
		})
		return now
	}
	secondRuntime.Now = func() time.Time {
		secondOnce.Do(func() { close(secondEntered) })
		return now.Add(time.Second)
	}
	type queueResult struct {
		run *freshRunLifecycle
		err error
	}
	results := make(chan queueResult, 2)
	queue := func(repoDir string, runID string, runtime runRuntime) {
		run, _, _, _, _, err := handler.queueHostAgentRun(repoDir, controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, scenarioRequirements{}, runtime)
		results <- queueResult{run: run, err: err}
	}
	firstRepoDir := writeRunConfigFixture(t)
	secondRepoDir := writeRunConfigFixture(t)
	go queue(firstRepoDir, "20260717T120000.000000000Z", firstRuntime)
	<-firstEntered
	go queue(secondRepoDir, "20260717T120001.000000000Z", secondRuntime)
	select {
	case <-secondEntered:
		t.Fatal("later Run entered acceptance before earlier durable insertion")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	<-secondEntered
	deadline := time.Now().Add(2 * time.Second)
	queuedBoth := false
	for time.Now().Before(deadline) {
		handler.mu.Lock()
		if len(handler.queuedRuns) == 2 {
			ordered := handler.orderedQueuedRunsLocked()
			for _, queued := range ordered {
				close(queued.done)
			}
			handler.mu.Unlock()
			if ordered[0].record.RunID != "20260717T120000.000000000Z" {
				t.Fatalf("serialized FIFO order = %+v", ordered)
			}
			queuedBoth = true
			break
		}
		handler.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	if !queuedBoth {
		t.Fatal("both serialized admissions did not become durable")
	}
	for index := 0; index < 2; index++ {
		result := <-results
		if result.run == nil || !errors.Is(result.err, errQueuedRunCanceled) {
			t.Fatalf("released queue result = %+v", result)
		}
	}
}

func TestQueueAcceptanceFailureCleansUnacceptedOwnership(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	handler.hostAgents["agent-1"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-1", HostID: "maya-win-01", State: "locked", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	repoDir := writeRunConfigFixture(t)
	runID := "20260717T120000.000000000Z"
	run, _, _, _, _, err := handler.queueHostAgentRun(repoDir, controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID, AssignedEventPrefix: []byte("invalid")}, scenarioRequirements{}, defaultRunRuntime())
	if err == nil || run == nil || run.accepted {
		t.Fatalf("invalid prefix acceptance = run %+v, error %v", run, err)
	}
	for _, path := range []string{filepath.Join(repoDir, ".maya-stall", "state", "runs", runID), filepath.Join(repoDir, "artifacts", "maya-stall", runID)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("failed acceptance retained ownership %s: %v", path, err)
		}
	}
}

func TestQueueUsesLaterCompatibleHostWhenFirstHostLockIsBusy(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	for index, hostID := range []string{"maya-win-01", "maya-win-02"} {
		agentID := fmt.Sprintf("agent-%d", index+1)
		handler.hostAgents[agentID] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: agentID, HostID: hostID, State: "ready", Slots: 1, SessionID: "session-" + agentID, SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	}
	handler.queuedRuns["run-a"] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}
	externalRelease, locked, err := acquireHostLock(filepath.Join(handler.dataDir, "fake-host"), "maya-win-01")
	if err != nil || locked {
		t.Fatalf("acquire external Host Lock: locked=%t error=%v", locked, err)
	}
	defer func() { _ = externalRelease() }()
	selected, _, _, release, _ := handler.selectQueuedRun("run-a")
	if selected == nil || selected.status.HostID != "maya-win-02" || release == nil {
		t.Fatalf("selected Host after first lock contention = %+v", selected)
	}
	if handler.queuedRuns["run-a"].lockContended {
		t.Fatal("successful later Host left stale lock-contention state")
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestQueueUsesLaterCompatibleHostWhenFirstHostLockErrors(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	for index, hostID := range []string{"maya-win-01", "maya-win-02"} {
		agentID := fmt.Sprintf("agent-%d", index+1)
		handler.hostAgents[agentID] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: agentID, HostID: hostID, State: "ready", Slots: 1, SessionID: "session-" + agentID, SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	}
	handler.queuedRuns["run-a"] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}
	badLock := filepath.Join(handler.dataDir, "fake-host", ".maya-stall", "state", "locks", "hosts", "maya-win-01.lock")
	if err := os.MkdirAll(badLock, 0o700); err != nil {
		t.Fatal(err)
	}
	selected, _, _, release, selectionErr := handler.selectQueuedRun("run-a")
	if selected == nil || selected.status.HostID != "maya-win-02" || release == nil || selectionErr != nil {
		t.Fatalf("selected Host after first Host Lock error = %+v, error %v", selected, selectionErr)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestQueueWaitReasonReportsExternalHostLockContention(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	handler.hostAgents["agent-1"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-1", HostID: "maya-win-01", State: "ready", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	queued := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}
	handler.queuedRuns["run-a"] = queued
	externalRelease, locked, err := acquireHostLock(filepath.Join(handler.dataDir, "fake-host"), "maya-win-01")
	if err != nil || locked {
		t.Fatalf("acquire external Host Lock: locked=%t error=%v", locked, err)
	}
	defer func() { _ = externalRelease() }()
	if selected, _, _, _, selectionErr := handler.selectQueuedRun("run-a"); selected != nil || selectionErr != nil {
		t.Fatalf("externally locked Host selection = %+v, error %v", selected, selectionErr)
	}
	if reason := handler.queueWaitReasonLocked(queued); reason != "compatible-hosts-busy" {
		t.Fatalf("external Host Lock wait reason = %q", reason)
	}
}

func TestQueueSurfacesHostLockIOErrors(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	handler.hostAgents["agent-1"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-1", HostID: "maya-win-01", State: "ready", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	handler.queuedRuns["run-a"] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}
	fakeRoot := filepath.Join(handler.dataDir, "fake-host")
	if err := os.MkdirAll(fakeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(fakeRoot, ".maya-stall")); err != nil {
		t.Fatal(err)
	}
	selected, _, _, _, selectionErr := handler.selectQueuedRun("run-a")
	if selected != nil || selectionErr == nil || !strings.Contains(selectionErr.Error(), "acquire Host Lock") || !handler.queuedRuns["run-a"].dispatching {
		t.Fatalf("Host Lock I/O selection = selected %+v, error %v", selected, selectionErr)
	}
}

func TestQueueRetainsRunWhenHostLockErrorsBesideCompatibleBusyHost(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	handler.hostAgents["broken"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "broken", HostID: "maya-win-01", State: "ready", Slots: 1, SessionID: "broken-session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	handler.hostAgents["busy"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "busy", HostID: "maya-win-02", State: "locked", Slots: 1, RunID: "active", SessionID: "busy-session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	queued := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}
	handler.queuedRuns["run-a"] = queued
	fakeRoot := filepath.Join(handler.dataDir, "fake-host")
	if err := os.MkdirAll(fakeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(fakeRoot, ".maya-stall")); err != nil {
		t.Fatal(err)
	}
	selected, _, _, _, selectionErr := handler.selectQueuedRun("run-a")
	if selected != nil || selectionErr != nil || queued.dispatching {
		t.Fatalf("busy compatible Host did not preserve queued Run: selected=%+v error=%v queued=%+v", selected, selectionErr, queued)
	}
	delete(handler.hostAgents, "busy")
	_, _, _, _, selectionErr = handler.selectQueuedRun("run-a")
	if selectionErr == nil || !queued.dispatching {
		t.Fatalf("sole broken Host Lock was not surfaced: error=%v queued=%+v", selectionErr, queued)
	}
}

func TestQueueReloadFailureCannotReserveHostWithoutWaiter(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	runID := "20260717T120000.000000000Z"
	queued := &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: runID, State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, HostPool: "windows-maya"}, done: make(chan struct{}), ready: make(chan struct{}, 1)}
	handler.queuedRuns[runID] = queued
	_, _, _, _, _, err = handler.queueHostAgentRun(filepath.Join(handler.dataDir, "runs", runID, "repo"), queued.record.Submission, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, scenarioRequirements{}, defaultRunRuntime())
	if err == nil || !queued.dispatching {
		t.Fatalf("queued reload failure = dispatching %t, error %v", queued.dispatching, err)
	}
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	host := &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-1", HostID: "maya-win-01", State: "ready", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	handler.hostAgents["agent-1"] = host
	if selected, _, _, _, _ := handler.selectQueuedRun(runID); selected != nil || host.status.State != "ready" || queued.releaseHostLock != nil {
		t.Fatalf("ownerless queued reload reserved Host: selected=%+v host=%+v", selected, host.status)
	}
}

func TestQueueHostLockIODoesNotBlockControlPlaneState(t *testing.T) {
	now := time.Date(2026, time.July, 17, 12, 0, 0, 0, time.UTC)
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runRuntime{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	handler.hostAgents["agent-1"] = &controlPlaneHostAgent{status: hostAgentStatusResponse{AgentID: "agent-1", HostID: "maya-win-01", State: "ready", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report}, sessionExpiresAt: now.Add(time.Minute)}
	handler.queuedRuns["run-a"] = &controlPlaneQueuedRun{record: controlPlaneQueueRecord{RunID: "run-a", State: "queued", AcceptedAt: now.Format(time.RFC3339Nano), Submission: controlPlaneSubmission{TargetProfile: "default"}, HostPool: "windows-maya"}, done: make(chan struct{})}

	lockStarted := make(chan struct{})
	allowLock := make(chan struct{})
	handler.queueAcquireHostLock = func(string, string) (func() error, bool, error) {
		close(lockStarted)
		<-allowLock
		return func() error { return nil }, false, nil
	}
	dispatchDone := make(chan struct{})
	go func() {
		handler.dispatchQueuedRuns()
		close(dispatchDone)
	}()
	select {
	case <-lockStarted:
	case <-time.After(time.Second):
		t.Fatal("Host Lock acquisition did not start")
	}
	stateAvailable := make(chan struct{})
	go func() {
		handler.mu.Lock()
		close(stateAvailable)
		handler.mu.Unlock()
	}()
	select {
	case <-stateAvailable:
	case <-time.After(time.Second):
		t.Fatal("Host Lock filesystem I/O held the Control Plane state mutex")
	}
	close(allowLock)
	select {
	case <-dispatchDone:
	case <-time.After(time.Second):
		t.Fatal("queue dispatch did not finish")
	}
	if release := handler.queuedRuns["run-a"].releaseHostLock; release == nil {
		t.Fatal("queued Run was not selected after Host Lock acquisition")
	} else if err := release(); err != nil {
		t.Fatal(err)
	}
}

func waitForQueuedRunID(t *testing.T, handler *controlPlaneHandler) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		handler.mu.Lock()
		for runID := range handler.queuedRuns {
			handler.mu.Unlock()
			return runID
		}
		handler.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Run did not enter queue")
	return ""
}
