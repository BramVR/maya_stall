package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"sync/atomic"
	"testing"
	"time"
)

const testHostAgentCredential = "agent-token-0123456789abcdef0123456789abcdef"

type heartbeatCancellableBroker struct {
	started chan struct{}
	stopped chan struct{}
	once    sync.Once
}

type nonStoppingBroker struct {
	started chan struct{}
	release chan struct{}
}

type hungStopBroker struct {
	release chan struct{}
}

func (broker *hungStopBroker) StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error) {
	return brokerSessionIdentity{}, nil
}

func (broker *hungStopBroker) RunScenario(runContext, scenarioConfig) (ScenarioResult, error) {
	return ScenarioResult{}, nil
}

func (broker *hungStopBroker) StopSession(runContext, brokerSessionIdentity) error {
	<-broker.release
	return nil
}

func (broker *nonStoppingBroker) StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error) {
	return brokerSessionIdentity{}, nil
}

func (broker *nonStoppingBroker) RunScenario(runContext, scenarioConfig) (ScenarioResult, error) {
	close(broker.started)
	<-broker.release
	return ScenarioResult{}, errors.New("broker eventually returned")
}

func (broker *nonStoppingBroker) StopSession(runContext, brokerSessionIdentity) error {
	return errors.New("broker stop failed")
}

func (broker *heartbeatCancellableBroker) StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error) {
	return brokerSessionIdentity{}, nil
}

func (broker *heartbeatCancellableBroker) RunScenario(runContext, scenarioConfig) (ScenarioResult, error) {
	close(broker.started)
	<-broker.stopped
	return ScenarioResult{}, errors.New("broker stopped")
}

func (broker *heartbeatCancellableBroker) StopSession(runContext, brokerSessionIdentity) error {
	broker.once.Do(func() { close(broker.stopped) })
	return nil
}

func TestHeartbeatLossStopsActiveHostAgentExecution(t *testing.T) {
	broker := &heartbeatCancellableBroker{started: make(chan struct{}), stopped: make(chan struct{})}
	cancel := make(chan error, 1)
	run := &freshRunLifecycle{runtime: runRuntime{Broker: broker, Cancel: cancel}}
	done := make(chan error, 1)
	go func() {
		_, err := run.runBrokerScenario()
		done <- err
	}()
	<-broker.started
	cancel <- errors.New("Host Agent session fence lost")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "session fence lost") {
			t.Fatalf("canceled Host Agent execution error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Host Agent execution did not stop after heartbeat loss")
	}
	if !run.sessionSettled {
		t.Fatal("Host Agent session was not marked settled after heartbeat loss")
	}
}

func TestHeartbeatLossReturnsWhenBrokerDoesNotStop(t *testing.T) {
	broker := &nonStoppingBroker{started: make(chan struct{}), release: make(chan struct{})}
	cancel := make(chan error, 1)
	run := &freshRunLifecycle{runtime: runRuntime{Broker: broker, Cancel: cancel, CancelWait: 20 * time.Millisecond}, cancellationObserved: true}
	done := make(chan error, 1)
	go func() {
		_, err := run.runBrokerScenario()
		done <- err
	}()
	<-broker.started
	cancel <- errors.New("Host Agent session fence lost")
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "did not stop within") {
			t.Fatalf("non-stopping broker cancellation error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Host Agent cancellation waited indefinitely for broker")
	}
	if !run.sessionStopAttempted || run.sessionSettled {
		t.Fatalf("timed-out broker stop state: attempted=%t settled=%t", run.sessionStopAttempted, run.sessionSettled)
	}
	close(broker.release)
}

func TestHealthySettlementIsNotLimitedByCancellationWait(t *testing.T) {
	broker := &hungStopBroker{release: make(chan struct{})}
	cancel := make(chan error)
	run := &freshRunLifecycle{runtime: runRuntime{Broker: broker, Cancel: cancel, CancelWait: 10 * time.Millisecond}}
	go func() {
		time.Sleep(40 * time.Millisecond)
		close(broker.release)
	}()
	if err := run.stopSessionDuringSettlement(); err != nil {
		t.Fatalf("healthy settlement stop: %v", err)
	}
	if !run.sessionSettled || run.cancellationObserved {
		t.Fatalf("healthy settlement state: settled=%t cancellation=%t", run.sessionSettled, run.cancellationObserved)
	}
}

func TestCancellationBoundsDeferredBrokerStop(t *testing.T) {
	broker := &hungStopBroker{release: make(chan struct{})}
	cancel := make(chan error)
	run := &freshRunLifecycle{runtime: runRuntime{Broker: broker, Cancel: cancel, CancelWait: 20 * time.Millisecond}, cancellationObserved: true}
	started := time.Now()
	err := run.stopSessionAfterFailure()
	if err == nil || !strings.Contains(err.Error(), "did not stop within") || time.Since(started) > time.Second {
		t.Fatalf("bounded deferred stop error = %v elapsed=%s", err, time.Since(started))
	}
	if !run.sessionStopAttempted || run.sessionSettled {
		t.Fatalf("bounded deferred stop state: attempted=%t settled=%t", run.sessionStopAttempted, run.sessionSettled)
	}
	close(broker.release)
}

func TestRunCompletesFakeScenarioThroughRegisteredWindowsHostAgent(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer

	code := RunWithRuntime([]string{
		"control-plane", "enroll-agent",
		"--control-plane", server.URL,
		"--agent-id", "windows-agent-01",
		"--host", "maya-win-01",
		"--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, &stdout, &stderr, repoDir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("enroll Windows Host Agent exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	enrollmentBytes, err := os.ReadFile(filepath.Join(dataDir, "host-agents", "windows-agent-01", "enrollment.json"))
	if err != nil {
		t.Fatalf("read durable Host Agent enrollment: %v", err)
	}
	if bytes.Contains(enrollmentBytes, []byte(testHostAgentCredential)) {
		t.Fatalf("durable Host Agent enrollment stored the plaintext credential: %s", enrollmentBytes)
	}

	agentDone := make(chan int, 1)
	var agentStdout bytes.Buffer
	var agentStderr bytes.Buffer
	go func() {
		agentDone <- RunWithRuntime([]string{
			"host-agent", "run-once",
			"--control-plane", server.URL,
			"--agent-id", "windows-agent-01",
			"--host", "maya-win-01",
			"--work-root", agentWorkRoot,
			"--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
		}, &agentStdout, &agentStderr, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "ready")

	stdout.Reset()
	stderr.Reset()
	runDone := make(chan int, 1)
	agentFinished := false
	go func() {
		runDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime)
	}()
	select {
	case code = <-runDone:
	case agentCode := <-agentDone:
		if agentCode != 0 {
			t.Fatalf("Windows Host Agent exited before the run completed with code %d; stdout: %s stderr: %s", agentCode, agentStdout.String(), agentStderr.String())
		}
		agentFinished = true
		select {
		case code = <-runDone:
		case <-time.After(2 * time.Second):
			t.Fatal("Control Plane did not return the completed Host Agent Scenario")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Windows Host Agent Scenario did not complete")
	}
	if code != 0 {
		t.Fatalf("Host Agent Scenario exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 2 || results[0].Kind != "run-accepted" || results[1].Status != resultStatusPassed || results[1].Host != "maya-win-01" || results[1].TargetProfile != "default" {
		t.Fatalf("Host Agent Scenario output = %+v", results)
	}
	runID := results[0].RunID
	if !agentFinished {
		select {
		case agentCode := <-agentDone:
			if agentCode != 0 {
				t.Fatalf("Windows Host Agent exit code = %d, want 0", agentCode)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Windows Host Agent did not finish after Scenario completion")
		}
	}

	var result controlPlaneResultResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/result", runtime, &result); err != nil {
		t.Fatalf("read Host Agent result: %v", err)
	}
	if !result.Final || !result.Success || result.CleanupState != "completed" {
		t.Fatalf("Host Agent result = %+v", result)
	}
	var events controlPlaneEventsResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/events", runtime, &events); err != nil {
		t.Fatalf("read immediate Host Agent events: %v", err)
	}
	if len(events.Events) < 2 || events.Events[1]["type"] != "run.queued" || events.Events[1]["detail"] != "awaiting-host-assignment" {
		t.Fatalf("immediate Host Agent queue event = %+v", events.Events)
	}
	serverRepo := filepath.Join(dataDir, "runs", runID, "repo")
	if _, err := os.Stat(filepath.Join(serverRepo, ".maya-stall", "state", "ledger", "runs", runID, "run.json")); err != nil {
		t.Fatalf("transferred Run Ledger: %v", err)
	}
	if _, err := os.Stat(filepath.Join(serverRepo, "artifacts", "maya-stall", runID, evidenceBundleFileName)); err != nil {
		t.Fatalf("transferred Evidence Bundle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(agentWorkRoot, "runs", runID)); !os.IsNotExist(err) {
		t.Fatalf("Host Agent run workspace = %v, want removed", err)
	}
	if _, err := os.Stat(filepath.Join(agentWorkRoot, "host")); !os.IsNotExist(err) {
		t.Fatalf("unused top-level Host Agent workspace = %v, want absent", err)
	}
	concrete := handler.(*controlPlaneHandler)
	concrete.mu.Lock()
	assignmentCount := len(concrete.assignments)
	concrete.mu.Unlock()
	if assignmentCount != 0 {
		t.Fatalf("completed in-memory assignments = %d, want 0", assignmentCount)
	}
	sharedRelease, locked, err := acquireHostLock(filepath.Join(dataDir, "fake-host"), "maya-win-01")
	if err != nil || locked {
		t.Fatalf("shared fake Host Lock after completion: locked=%t err=%v", locked, err)
	}
	if err := sharedRelease(); err != nil {
		t.Fatalf("release shared fake Host Lock probe: %v", err)
	}
	var completed hostAgentAssignmentRecord
	if err := readPrivateJSON(filepath.Join(dataDir, "assignments", runID+".json"), &completed); err != nil {
		t.Fatalf("read completed assignment: %v", err)
	}
	if completed.BrokerSession == nil || completed.BrokerSession.BrokerAdapter != "fake" || completed.BrokerSession.SessionID == "" {
		t.Fatalf("completed shared Host Lock session binding = %+v", completed.BrokerSession)
	}
	var bundle evidenceBundle
	readJSONFile(t, filepath.Join(serverRepo, "artifacts", "maya-stall", runID, evidenceBundleFileName), &bundle)
	if bundle.BrokerSession == nil || *bundle.BrokerSession != *completed.BrokerSession {
		t.Fatalf("shared Host Lock binding = %+v, Evidence Bundle session = %+v", completed.BrokerSession, bundle.BrokerSession)
	}
	var repeated runCommandJSON
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+runID+"/complete", hostAgentCompletionRequest{
		Version: hostAgentAPIVersion, RunID: runID, LockToken: completed.LockToken, SessionID: "finished-session",
	}, runtime, http.StatusOK, &repeated); err != nil {
		t.Fatalf("repeat completed Host Agent upload: %v", err)
	}
	if repeated.RunID != runID || repeated.Status != resultStatusPassed {
		t.Fatalf("repeated completion terminal = %+v", repeated)
	}
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "offline")
}

func TestRegisteredHostAgentCheckpointsEventsDuringExecution(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	if code := RunWithRuntime([]string{
		"control-plane", "enroll-agent", "--control-plane", server.URL,
		"--agent-id", "windows-agent-01", "--host", "maya-win-01",
		"--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, io.Discard, io.Discard, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("enroll Windows Host Agent exit code = %d", code)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	agentRuntime := runtime
	agentRuntime.SessionStarted = func(brokerSessionIdentity) error {
		close(started)
		<-release
		return nil
	}
	agentDone := make(chan int, 1)
	go func() {
		agentDone <- RunWithRuntime([]string{
			"host-agent", "run-once", "--control-plane", server.URL,
			"--agent-id", "windows-agent-01", "--host", "maya-win-01",
			"--work-root", agentWorkRoot, "--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
		}, io.Discard, io.Discard, repoDir, "test-version", agentRuntime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "ready")
	accepted := make(chan runOutcome, 1)
	runtime.Accepted = func(outcome runOutcome) { accepted <- outcome }
	runDone := make(chan int, 1)
	go func() {
		runDone <- RunWithRuntime([]string{"run", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	<-started
	runID := (<-accepted).RunID

	deadline := time.Now().Add(5 * time.Second)
	var active controlPlaneEventsResponse
	for {
		err = getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/events?fromSequence=1", runtime, &active)
		if err == nil {
			for _, event := range active.Events {
				if event["type"] == "run.started" {
					goto checkpointed
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("active Host Agent events = %+v err %v", active, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

checkpointed:
	activeEvents := make(map[int][]byte, len(active.Events))
	for _, event := range active.Events {
		encoded, err := json.Marshal(event)
		if err != nil {
			t.Fatalf("marshal active event: %v", err)
		}
		activeEvents[ledgerEventSequence(event)] = encoded
	}
	close(release)
	released = true
	if code := <-agentDone; code != 0 {
		t.Fatalf("Windows Host Agent exit code = %d", code)
	}
	if code := <-runDone; code != 0 {
		t.Fatalf("Control Plane run exit code = %d", code)
	}
	var terminal controlPlaneEventsResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/events?fromSequence=1", runtime, &terminal); err != nil {
		t.Fatalf("read terminal Host Agent events: %v", err)
	}
	for _, event := range terminal.Events {
		sequence := ledgerEventSequence(event)
		if active, ok := activeEvents[sequence]; ok {
			encoded, err := json.Marshal(event)
			if err != nil || !bytes.Equal(active, encoded) {
				t.Fatalf("event sequence %d changed from live %s to terminal %s", sequence, active, encoded)
			}
		}
	}
}

func TestHostAgentProgressDoesNotHoldControlPlaneMutexDuringLedgerIO(t *testing.T) {
	repoDir := privateTempDir(t)
	dataDir := privateTempDir(t)
	runID := "20260716T120000.000000000Z"
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, Scenario: "smoke", TargetProfile: "default", Host: "maya-win-01",
		State: "submitted", AcceptedAt: now.Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)), Events: runLedgerEventsFileName, Log: runLedgerLogPath,
	}
	if err := writeRunLedgerRecord(repoDir, record); err != nil {
		t.Fatalf("write active Run Ledger: %v", err)
	}
	eventsPath := filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName)
	if err := writeRunLedgerBytes(eventsPath, []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n")); err != nil {
		t.Fatalf("write active events: %v", err)
	}
	if err := writeRunLedgerBytes(filepath.Join(runLedgerDir(repoDir, runID), filepath.FromSlash(runLedgerLogPath)), nil); err != nil {
		t.Fatalf("write active log: %v", err)
	}

	validated := make(chan struct{})
	var observeValidation atomic.Bool
	var nowCalls atomic.Int32
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time {
		if observeValidation.Load() && nowCalls.Add(1) == 2 {
			close(validated)
		}
		return now
	}
	httpHandler, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	handler := httpHandler.(*controlPlaneHandler)
	credentialDigest := sha256.Sum256([]byte(testHostAgentCredential))
	agent := &controlPlaneHostAgent{
		enrollment: hostAgentEnrollmentRecord{Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01", CredentialSHA256: fmt.Sprintf("%x", credentialDigest)},
		status:     hostAgentStatusResponse{Version: hostAgentAPIVersion, Kind: "host-agent", AgentID: "windows-agent-01", HostID: "maya-win-01", State: "running", RunID: runID, SessionID: "session-1", SessionBinding: true},
		notify:     make(chan struct{}), sessionExpiresAt: now.Add(hostAgentSessionLease),
	}
	assignment := &controlPlaneHostAgentAssignment{
		record: hostAgentAssignmentRecord{
			Version: hostAgentAPIVersion, RunID: runID, AgentID: "windows-agent-01", HostID: "maya-win-01", LockToken: "lock-1", State: "confirmed",
			Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default"}, SessionBindingRequired: true,
		},
		repoDir: repoDir, done: make(chan struct{}),
	}
	handler.hostAgents[agent.status.AgentID] = agent
	handler.assignments[runID] = assignment

	server := httptest.NewTLSServer(httpHandler)
	t.Cleanup(server.Close)
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime.ControlPlaneHTTPClient = server.Client()
	lockHeld := make(chan struct{})
	releaseLock := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- withRunLedgerLock(repoDir, runID, func() error {
			close(lockHeld)
			<-releaseLock
			return nil
		})
	}()
	<-lockHeld
	progress := hostAgentProgressRequest{
		Version: hostAgentAPIVersion, RunID: runID, LockToken: "lock-1", SessionID: "session-1", Checkpoint: 1, Ledger: record,
		Events: []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":2,\"type\":\"run.started\"}\n"), Log: []byte("started\n"),
	}
	progressDone := make(chan error, 1)
	observeValidation.Store(true)
	go func() {
		var response hostAgentStatusResponse
		progressDone <- postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+runID+"/progress", progress, runtime, http.StatusOK, &response)
	}()
	select {
	case <-validated:
	case <-time.After(2 * time.Second):
		close(releaseLock)
		t.Fatal("progress request did not pass Host Agent validation")
	}
	time.Sleep(20 * time.Millisecond)
	mutexAvailable := handler.mu.TryLock()
	if mutexAvailable {
		handler.mu.Unlock()
	}
	confirmDone := make(chan error, 1)
	go func() {
		var response hostAgentStatusResponse
		confirmDone <- postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+runID+"/confirm", hostAgentLockRequest{
			Version: hostAgentAPIVersion, RunID: runID, LockToken: "lock-1", SessionID: "session-1",
		}, runtime, http.StatusOK, &response)
	}()
	confirmCompletedDuringCheckpoint := false
	var confirmErr error
	select {
	case confirmErr = <-confirmDone:
		confirmCompletedDuringCheckpoint = true
	case <-time.After(50 * time.Millisecond):
	}
	sessionDone := make(chan error, 1)
	go func() {
		var response hostAgentStatusResponse
		sessionDone <- postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+runID+"/session", hostAgentSessionRequest{
			Version: hostAgentAPIVersion, RunID: runID, LockToken: "lock-1", SessionID: "session-1", BrokerSession: brokerSessionIdentity{BrokerAdapter: "fake", SessionID: "maya-session-1"},
		}, runtime, http.StatusOK, &response)
	}()
	sessionCompletedDuringCheckpoint := false
	var sessionErr error
	select {
	case sessionErr = <-sessionDone:
		sessionCompletedDuringCheckpoint = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseLock)
	if err := <-lockDone; err != nil {
		t.Fatalf("release held Run Ledger lock: %v", err)
	}
	if err := <-progressDone; err != nil {
		t.Fatalf("persist Host Agent progress: %v", err)
	}
	if !confirmCompletedDuringCheckpoint {
		confirmErr = <-confirmDone
	}
	if confirmErr != nil && !confirmCompletedDuringCheckpoint {
		t.Fatalf("retry Host Lock confirmation after checkpoint: %v", confirmErr)
	}
	if !sessionCompletedDuringCheckpoint {
		sessionErr = <-sessionDone
	}
	if sessionErr != nil && !sessionCompletedDuringCheckpoint {
		t.Fatalf("bind Maya UI Session after checkpoint: %v", sessionErr)
	}
	if !mutexAvailable {
		t.Fatal("Host Agent progress held the global Control Plane mutex while waiting on Run Ledger I/O")
	}
	if confirmCompletedDuringCheckpoint {
		t.Fatal("Host Lock confirmation retry was allowed to race an in-flight Host Agent checkpoint")
	}
	if sessionCompletedDuringCheckpoint {
		t.Fatal("Maya UI Session binding was allowed to race an in-flight Host Agent checkpoint")
	}
	handler.mu.Lock()
	bound := handler.assignments[runID].record.BrokerSession
	handler.mu.Unlock()
	if bound == nil || bound.BrokerAdapter != "fake" || bound.SessionID != "maya-session-1" {
		t.Fatalf("Maya UI Session binding after checkpoint = %+v", bound)
	}
}

func TestMergeHostAgentProgressEventsRejectsDuplicateOrReorderedSequences(t *testing.T) {
	current := []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n")
	tests := map[string][]byte{
		"duplicate":  []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":2,\"type\":\"run.started\"}\n{\"sequence\":2,\"type\":\"run.changed\"}\n"),
		"reordered":  []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":3,\"type\":\"run.changed\"}\n{\"sequence\":2,\"type\":\"run.started\"}\n"),
		"fractional": []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":2.5,\"type\":\"run.changed\"}\n"),
	}
	for name, incoming := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := mergeHostAgentProgressEvents(current, incoming); err == nil {
				t.Fatal("malformed progress events were accepted")
			}
		})
	}
	current = []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":2,\"type\":\"run.started\"}\n{\"sequence\":3,\"type\":\"run.changed\"}\n")
	stale := []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":2,\"type\":\"run.started\"}\n")
	if _, err := mergeHostAgentProgressEvents(current, stale); err == nil {
		t.Fatal("stale progress snapshot rewound acknowledged events")
	}
	middleMissing := []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":3,\"type\":\"run.changed\"}\n")
	if _, err := mergeHostAgentProgressEvents(current, middleMissing); err == nil {
		t.Fatal("progress snapshot silently dropped a middle event")
	}
	freshGap := []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":3,\"type\":\"run.changed\"}\n")
	if _, err := mergeHostAgentProgressEvents([]byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n"), freshGap); err == nil {
		t.Fatal("progress snapshot silently introduced an unmarked sequence gap")
	}
}

func TestMergeHostAgentTerminalEventsPreservesCheckpointGapAndTerminalTail(t *testing.T) {
	current := []byte("{\"sequence\":1,\"timestamp\":\"2026-07-16T00:00:00Z\",\"type\":\"run.accepted\"}\n" +
		"{\"details\":{\"firstOmittedSequence\":2,\"lastOmittedSequence\":3,\"omittedCount\":2},\"sequence\":2,\"timestamp\":\"2026-07-16T00:00:00Z\",\"type\":\"run-ledger.events.truncated\"}\n" +
		"{\"sequence\":4,\"type\":\"run.changed\"}\n")
	incoming := []byte("{\"sequence\":1,\"timestamp\":\"2026-07-16T00:00:00Z\",\"type\":\"run.accepted\"}\n" +
		"{\"sequence\":2,\"type\":\"run.started\"}\n" +
		"{\"sequence\":3,\"type\":\"run.executing\"}\n" +
		"{\"sequence\":4,\"type\":\"run.changed\"}\n" +
		"{\"sequence\":5,\"type\":\"run.completed\"}\n")

	merged, err := mergeHostAgentTerminalEvents(current, incoming)
	if err != nil {
		t.Fatalf("merge terminal events: %v", err)
	}
	if bytes.Contains(merged, []byte(`"type":"run.started"`)) || bytes.Contains(merged, []byte(`"type":"run.executing"`)) {
		t.Fatalf("terminal events restored unverifiable checkpoint gap: %s", merged)
	}
	if !bytes.Contains(merged, []byte(`"omittedCount":2`)) || !bytes.Contains(merged, []byte(`"sequence":5`)) {
		t.Fatalf("terminal merge lost the checkpoint gap or terminal tail: %s", merged)
	}
}

func TestHostAgentProgressFingerprintChangesOnlyWithStreamContent(t *testing.T) {
	progress := hostAgentProgressRequest{Events: []byte("event\n"), Log: []byte("log\n")}
	baseline := hostAgentProgressFingerprint(progress)
	if got := hostAgentProgressFingerprint(progress); got != baseline {
		t.Fatal("unchanged progress content produced a different fingerprint")
	}
	progress.Log = append(progress.Log, []byte("changed\n")...)
	if got := hostAgentProgressFingerprint(progress); got == baseline {
		t.Fatal("changed progress content produced the same fingerprint")
	}
}

func TestHostAgentProgressEventCountBound(t *testing.T) {
	content := bytes.Repeat([]byte("{}\n"), maximumHostAgentProgressEvents+1)
	if got := runLedgerEventLineCount(content); got != maximumHostAgentProgressEvents+1 {
		t.Fatalf("progress event line count = %d", got)
	}
}

func TestHostAgentProgressSnapshotFailureIsRetriedBeforeBecomingTerminal(t *testing.T) {
	var failures int
	transient := errors.New("partial trailing event")
	if err := terminalHostAgentProgressSnapshotError(&failures, transient); err != nil {
		t.Fatalf("first snapshot failure was terminal: %v", err)
	}
	if failures != 1 {
		t.Fatalf("snapshot failures = %d", failures)
	}
	if err := terminalHostAgentProgressSnapshotError(&failures, transient); err != nil {
		t.Fatalf("second snapshot failure was terminal: %v", err)
	}
	if err := terminalHostAgentProgressSnapshotError(&failures, transient); !errors.Is(err, transient) {
		t.Fatalf("third consecutive snapshot failure = %v", err)
	}
}

func TestHostAgentProgressRejectedWhileTerminalCommitIsFinishing(t *testing.T) {
	assignment := &controlPlaneHostAgentAssignment{
		record:    hostAgentAssignmentRecord{State: "confirmed"},
		finishing: true,
	}
	if hostAgentAssignmentAcceptsProgress(assignment) {
		t.Fatal("progress remained writable during terminal commit")
	}
}

func TestHostAgentProgressReboundsSanitizedArtifacts(t *testing.T) {
	temporaryDir := privateTempDir(t)
	var events bytes.Buffer
	for sequence := 1; sequence <= 20; sequence++ {
		_, _ = fmt.Fprintf(&events, "{\"details\":{\"path\":\"x\"},\"sequence\":%d,\"type\":\"run.changed\"}\n", sequence)
	}
	policy := runLedgerPolicy{MaxEvents: 20, MaxEventBytes: 1024, MaxLogBytes: 128}

	bounded, err := boundSanitizedHostAgentProgressArtifacts(
		temporaryDir, events.Bytes(), bytes.Repeat([]byte("x"), 100), policy,
		"2026-07-16T00:00:00Z", newHostAgentTextSanitizer([]string{"x"}),
	)

	if err != nil {
		t.Fatalf("bound sanitized progress: %v", err)
	}
	if len(bounded.Events) > policy.MaxEventBytes || len(bounded.Log) > policy.MaxLogBytes {
		t.Fatalf("sanitized progress exceeded bounds: events %d, log %d", len(bounded.Events), len(bounded.Log))
	}
	if bounded.EventBytes != len(bounded.Events) || bounded.LogBytes != len(bounded.Log) || !bounded.EventsTruncated || !bounded.LogTruncated {
		t.Fatalf("sanitized progress metadata = %+v", bounded)
	}
}

func TestHostAgentFailurePreservationAppendsExactTerminalFailureEvent(t *testing.T) {
	repoDir := privateTempDir(t)
	runID := "20260716T120000.000000000Z"
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, Scenario: "smoke", TargetProfile: "default", Host: "maya-win-01",
		State: "failed", Status: resultStatusFailed, AcceptedAt: "2026-07-16T12:00:00Z", UpdatedAt: "2026-07-16T12:01:00Z",
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)), Events: runLedgerEventsFileName, Log: runLedgerLogPath,
	}
	if err := writeRunLedgerRecord(repoDir, record); err != nil {
		t.Fatalf("write run ledger: %v", err)
	}
	acknowledged := []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n{\"sequence\":2,\"type\":\"run.failed-before-result-collection\"}\n")
	terminal := []byte("{\"sequence\":2,\"type\":\"run.failed\"}\n")

	if err := newRunLedgerStore(repoDir).PreserveAcknowledgedFailure(runID, acknowledged, terminal, nil, "Agent failed", defaultRunLedgerPolicy()); err != nil {
		t.Fatalf("preserve Host Agent failure: %v", err)
	}
	retained, err := os.ReadFile(filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read retained failure events: %v", err)
	}
	if !bytes.Contains(retained, []byte(`"sequence":3`)) || !bytes.Contains(retained, []byte(`"type":"run.failed"`)) {
		t.Fatalf("retained events omitted exact terminal failure: %s", retained)
	}
}

func TestHostAgentFailureSequenceAdvancesPastRetainedGap(t *testing.T) {
	repoDir := privateTempDir(t)
	runID := "20260716T120000.000000000Z"
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, Scenario: "smoke", TargetProfile: "default", Host: "maya-win-01",
		State: "failed", Status: resultStatusFailed, AcceptedAt: "2026-07-16T12:00:00Z", UpdatedAt: "2026-07-16T12:01:00Z",
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)), Events: runLedgerEventsFileName, Log: runLedgerLogPath,
	}
	if err := writeRunLedgerRecord(repoDir, record); err != nil {
		t.Fatalf("write run ledger: %v", err)
	}
	acknowledged := []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n" +
		"{\"details\":{\"firstOmittedSequence\":2,\"lastOmittedSequence\":6,\"omittedCount\":5},\"sequence\":2,\"type\":\"run-ledger.events.truncated\"}\n")

	if err := newRunLedgerStore(repoDir).PreserveAcknowledgedFailure(runID, acknowledged, []byte("{\"sequence\":2,\"type\":\"run.failed\"}\n"), nil, "Agent failed", defaultRunLedgerPolicy()); err != nil {
		t.Fatalf("preserve Host Agent failure: %v", err)
	}
	retained, err := os.ReadFile(filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read retained failure events: %v", err)
	}
	if !bytes.Contains(retained, []byte(`"sequence":7`)) || !bytes.Contains(retained, []byte(`"type":"run.failed"`)) {
		t.Fatalf("failure did not advance beyond retained gap: %s", retained)
	}
}

func TestAcceptedRunSurvivesDisconnectBeforeHostAgentAssignmentPickup(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	if code := RunWithRuntime([]string{
		"control-plane", "enroll-agent", "--control-plane", server.URL,
		"--agent-id", "windows-agent-01", "--host", "maya-win-01",
		"--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, io.Discard, io.Discard, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("enroll Windows Host Agent exit code = %d", code)
	}
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", hostAgentRegistrationRequest{
		Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01", Slots: 1, SessionBinding: true,
		Capabilities: testHostAgentCapabilityRecord("maya-win-01", time.Now()),
	}, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}
	submission, err := buildControlPlaneSubmission(repoDir, runOptions{ScenarioName: "smoke", StopAfter: stopAfterAlways})
	if err != nil {
		t.Fatalf("build Control Plane submission: %v", err)
	}
	body, _ := json.Marshal(submission)
	request := httptest.NewRequest(http.MethodPost, "https://maya-stall.example.com/v1/runs", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer operator-token")
	ctx, cancel := context.WithCancel(request.Context())
	request = request.WithContext(ctx)
	submitDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(&failingControlPlaneResponseWriter{header: make(http.Header)}, request)
		close(submitDone)
	}()
	concrete := handler.(*controlPlaneHandler)
	var runID string
	deadline := time.Now().Add(2 * time.Second)
	for runID == "" {
		concrete.mu.Lock()
		for candidate := range concrete.assignments {
			runID = candidate
		}
		concrete.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("accepted run was not retained before Agent pickup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	var assignment hostAgentAssignmentResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/next", hostAgentNextRequest{
		Version: hostAgentAPIVersion, SessionID: status.SessionID,
	}, runtime, http.StatusOK, &assignment); err != nil {
		t.Fatalf("pick up assignment after submitter disconnect: %v", err)
	}
	if assignment.RunID != runID {
		t.Fatalf("picked up Run ID = %s, want %s", assignment.RunID, runID)
	}
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+runID+"/confirm", hostAgentLockRequest{
		Version: hostAgentAPIVersion, RunID: runID, LockToken: assignment.LockToken, SessionID: status.SessionID,
	}, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("confirm assignment after submitter disconnect: %v", err)
	}
	var terminal runCommandJSON
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+runID+"/fail", hostAgentFailureRequest{
		Version: hostAgentAPIVersion, RunID: runID, LockToken: assignment.LockToken, SessionID: status.SessionID, Diagnostic: "controlled Agent stop before execution",
	}, runtime, http.StatusOK, &terminal); err != nil {
		t.Fatalf("settle assignment after submitter disconnect: %v", err)
	}
	<-submitDone
	assertOnlyControlPlaneRunState(t, dataDir, "failed")
}

func TestEnsureHostAgentDirectoryDistinguishesFileFromSymlink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("file"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	err := ensureHostAgentDirectory(path)
	if err == nil || !strings.Contains(err.Error(), "must be a directory and not a symlink") {
		t.Fatalf("regular-file diagnostic = %v", err)
	}
}

func TestHostAgentKeepsPollingAfterEmptyAssignmentResponse(t *testing.T) {
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	var polls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testHostAgentCredential {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		if polls.Add(1) == 1 {
			response.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(response).Encode(hostAgentAssignmentResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-assignment", RunID: "20260715T120000.000000000Z",
			AgentID: "windows-agent-01", HostID: "maya-win-01", LockToken: strings.Repeat("a", 32),
		})
	}))
	t.Cleanup(server.Close)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	assignment, err := pollHostAgentAssignment(hostAgentRunOnceOptions{
		ControlPlane: server.URL, AgentID: "windows-agent-01", CredentialEnv: "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, runtime, nil)
	if err != nil {
		t.Fatalf("poll Host Agent assignment: %v", err)
	}
	if polls.Load() != 2 || assignment.RunID != "20260715T120000.000000000Z" {
		t.Fatalf("polls = %d, assignment = %+v", polls.Load(), assignment)
	}
}

func TestHostAgentHeartbeatFailureCancelsBlockingAssignmentPoll(t *testing.T) {
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		close(started)
		<-release
	}))
	t.Cleanup(server.Close)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	heartbeatErrors := make(chan error, 1)
	finished := make(chan error, 1)
	go func() {
		_, err := pollHostAgentAssignment(hostAgentRunOnceOptions{
			ControlPlane: server.URL, AgentID: "windows-agent-01", CredentialEnv: "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
		}, runtime, heartbeatErrors)
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Host Agent assignment poll did not start")
	}
	heartbeatErrors <- errors.New("heartbeat fence lost")
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "heartbeat fence lost") {
			t.Fatalf("cancelled Host Agent assignment poll error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat failure did not cancel Host Agent assignment poll")
	}
}

func TestHostAgentEnrollmentRejectsShortCredentialAtBothBoundaries(t *testing.T) {
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "too-short")
	options := controlPlaneEnrollAgentOptions{
		ControlPlane: "https://control-plane.example.com", AgentID: "windows-agent-01", HostID: "maya-win-01",
		CredentialEnv: "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", TokenEnv: defaultControlPlaneTokenEnv,
	}
	if err := enrollControlPlaneHostAgent(options, defaultRunRuntime(), io.Discard); err == nil || !strings.Contains(err.Error(), "at least 32 bytes") {
		t.Fatalf("short enrollment credential error = %v", err)
	}

	handler, err := newControlPlaneHandler(privateTempDir(t), "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	content, err := json.Marshal(hostAgentEnrollmentRequest{
		Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01", Credential: "too-short",
	})
	if err != nil {
		t.Fatalf("marshal short enrollment: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/host-agents/enroll", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("create short enrollment request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer operator-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("send short enrollment request: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("short server enrollment HTTP = %d, want 400", response.StatusCode)
	}
}

func TestSanitizeHostAgentResultTextRemovesAgentLocalPaths(t *testing.T) {
	privateRoot := `C:\maya-stall\agent\runs\run-01\repo`
	value := privateRoot + "\n" + strings.ReplaceAll(privateRoot, `\`, `\\`) + "\n/agent/work/run-01/repo"
	sanitized := sanitizeHostAgentResultText(value, []string{privateRoot, "/agent/work/run-01/repo"})
	for _, privatePath := range []string{privateRoot, strings.ReplaceAll(privateRoot, `\`, `\\`), "/agent/work/run-01/repo"} {
		if strings.Contains(sanitized, privatePath) {
			t.Fatalf("sanitized result contains private path %q: %q", privatePath, sanitized)
		}
	}
	if strings.Count(sanitized, "[agent-workspace]") != 3 {
		t.Fatalf("sanitized result = %q", sanitized)
	}
	caseVariant := sanitizeHostAgentResultText(`c:\MAYA-STALL\Agent\runs`, []string{`C:\maya-stall\agent`})
	if strings.Contains(strings.ToLower(caseVariant), `c:\maya-stall\agent`) {
		t.Fatalf("case-variant Windows Agent path was not sanitized: %q", caseVariant)
	}
}

func TestSanitizeHostAgentTerminalRemovesAgentLocalPaths(t *testing.T) {
	privateRoot := `C:\maya-stall\agent`
	terminal := runCommandJSON{
		Diagnostic: privateRoot + `\runs\run-01`, RemediationHint: privateRoot + `\host`,
		Error: privateRoot + `\error`, FollowUpCommands: []string{privateRoot + `\follow-up`},
		Report: &reportView{Summary: privateRoot + `\summary`},
	}
	sanitizeHostAgentTerminal(&terminal, []string{privateRoot})
	for name, value := range map[string]string{
		"diagnostic": terminal.Diagnostic, "remediation": terminal.RemediationHint,
		"error": terminal.Error, "follow-up": terminal.FollowUpCommands[0], "report": terminal.Report.Summary,
	} {
		if strings.Contains(value, privateRoot) || !strings.Contains(value, "[agent-workspace]") {
			t.Fatalf("sanitized %s = %q", name, value)
		}
	}
}

func TestHostAgentFakeHostWorkspaceIsAssignmentScoped(t *testing.T) {
	workRoot := privateTempDir(t)
	runID := "20260715T120000.000000000Z"
	runRoot := filepath.Join(workRoot, "runs", runID)
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		t.Fatalf("create Agent run root: %v", err)
	}
	path, err := writeHostAgentFakeHostConfig(hostAgentRunOnceOptions{WorkRoot: workRoot}, hostAgentAssignmentResponse{
		RunID: runID, HostID: "maya-win-01", Submission: controlPlaneSubmission{TargetProfile: "default"},
	})
	if err != nil {
		t.Fatalf("write fake Host config: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake Host config: %v", err)
	}
	wantRoot := filepath.Join(runRoot, "host")
	if !bytes.Contains(content, []byte(wantRoot)) {
		t.Fatalf("fake Host config = %s, want assignment-scoped work root %s", content, wantRoot)
	}
	if path != filepath.Join(workRoot, "host-config.yaml") {
		t.Fatalf("fake Host config path = %s", path)
	}
}

func TestHostAgentFakeHostConfigRejectsSymlink(t *testing.T) {
	workRoot := privateTempDir(t)
	runID := "20260715T120000.000000000Z"
	if err := os.MkdirAll(filepath.Join(workRoot, "runs", runID), 0o700); err != nil {
		t.Fatalf("create Agent run root: %v", err)
	}
	victim := filepath.Join(workRoot, "victim.yaml")
	if err := os.WriteFile(victim, []byte("unchanged"), 0o600); err != nil {
		t.Fatalf("write symlink victim: %v", err)
	}
	if err := os.Symlink(victim, filepath.Join(workRoot, "host-config.yaml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := writeHostAgentFakeHostConfig(hostAgentRunOnceOptions{WorkRoot: workRoot}, hostAgentAssignmentResponse{
		RunID: runID, HostID: "maya-win-01", Submission: controlPlaneSubmission{TargetProfile: "default"},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("write through host-config symlink error = %v", err)
	}
	content, readErr := os.ReadFile(victim)
	if readErr != nil || string(content) != "unchanged" {
		t.Fatalf("symlink victim changed: content=%q error=%v", content, readErr)
	}
}

func TestHostAgentRealConfigSelectsAssignedLiveHostWithoutFallback(t *testing.T) {
	workRoot := privateTempDir(t)
	hostConfigPath := filepath.Join(workRoot, "hosts.yaml")
	hostConfig := `version: 1
targetProfiles:
  ci:
    hostPool: maya
hostPools:
  maya:
    hosts:
      - id: maya-win-01
        health: healthy
        transport: ssh
        ssh:
          host: maya-win-01
          user: maya-runner
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv/Scripts/python.exe
          repo: C:/maya-stall/GG_MayaSessiond
          mcpSource: C:/maya-stall/GG_MayaMCP
`
	if err := os.WriteFile(hostConfigPath, []byte(hostConfig), 0o600); err != nil {
		t.Fatalf("write real Agent Host config: %v", err)
	}
	assignment := hostAgentAssignmentResponse{
		RunID: "20260715T120000.000000000Z", HostID: "maya-win-01",
		Submission: controlPlaneSubmission{TargetProfile: "ci"},
	}
	if err := os.MkdirAll(filepath.Join(workRoot, "runs", assignment.RunID), 0o700); err != nil {
		t.Fatalf("create Agent run root: %v", err)
	}
	path, metadata, err := resolveHostAgentHostConfig(hostAgentRunOnceOptions{
		WorkRoot: workRoot, HostConfig: hostConfigPath,
	}, assignment)
	if err != nil {
		t.Fatalf("resolve real Agent Host config: %v", err)
	}
	if path == hostConfigPath || filepath.Dir(path) != filepath.Join(workRoot, "runs", assignment.RunID) {
		t.Fatalf("resolved Host config = %q, want private per-run snapshot", path)
	}
	if metadata.Profile != "ssh-sessiond" || metadata.HostAdapter != "ssh" || metadata.BrokerAdapter != "gg-mayasessiond" || !metadata.LiveProofEligible {
		t.Fatalf("resolved Agent runtime = %+v, want live ssh-sessiond", metadata)
	}
	if err := os.WriteFile(hostConfigPath, []byte("version: 1\ntargetProfiles:\n  ci:\n    hostPool: fake\nhostPools:\n  fake:\n    hosts:\n      - id: maya-win-01\n        health: healthy\n"), 0o600); err != nil {
		t.Fatalf("replace operator Host config: %v", err)
	}
	snapshot, err := loadUserHostConfig(path)
	if err != nil {
		t.Fatalf("load Agent Host config snapshot: %v", err)
	}
	hosts, err := hostCandidates(snapshot, "ci", "maya-win-01")
	if err != nil {
		t.Fatalf("select snapshot Host: %v", err)
	}
	resolved, err := resolveRuntimeForHost(hosts[0])
	if err != nil || !resolved.Metadata.LiveProofEligible {
		t.Fatalf("snapshot runtime = %+v, error = %v", resolved.Metadata, err)
	}
	assignment.HostID = "maya-win-02"
	if _, _, err := resolveHostAgentHostConfig(hostAgentRunOnceOptions{
		WorkRoot: workRoot, HostConfig: hostConfigPath,
	}, assignment); err == nil || !strings.Contains(err.Error(), "pinned Maya Host") {
		t.Fatalf("mismatched assigned Host error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(workRoot, "host-config.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("real Agent path created fake Host config: %v", err)
	}
}

func TestHostAgentCompletionSessionAllowsOnlyCleanPreSessionFailureOrExactBinding(t *testing.T) {
	session := &brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond", SessionID: "maya-session-01"}
	other := &brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond", SessionID: "maya-session-02"}
	partial := &brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond"}
	tests := []struct {
		name     string
		required bool
		status   string
		lock     *brokerSessionIdentity
		evidence *brokerSessionIdentity
		want     bool
	}{
		{name: "legacy in-flight assignment", status: resultStatusPassed, evidence: session, want: true},
		{name: "failed before session", required: true, status: resultStatusFailed, want: true},
		{name: "failed after unrecorded binding", required: true, status: resultStatusFailed, evidence: session, want: true},
		{name: "failed with malformed unrecorded binding", required: true, status: resultStatusFailed, evidence: partial, want: false},
		{name: "passed without session", required: true, status: resultStatusPassed, want: false},
		{name: "exact bound session", required: true, status: resultStatusPassed, lock: session, evidence: session, want: true},
		{name: "evidence omitted bound session", required: true, status: resultStatusFailed, lock: session, want: false},
		{name: "different bound session", required: true, status: resultStatusFailed, lock: session, evidence: other, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := validHostAgentCompletionSession(test.required, test.status, test.lock, test.evidence); got != test.want {
				t.Fatalf("valid completion session = %t, want %t", got, test.want)
			}
		})
	}
}

func TestHostAgentSessionBindingRetryStopsOnExecutionCancellation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		response.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	executionCancel := make(chan error, 1)
	executionCancel <- errors.New("heartbeat fence lost")
	err := postHostAgentSessionBinding(hostAgentRunOnceOptions{
		ControlPlane: server.URL, CredentialEnv: "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, "/session", hostAgentSessionRequest{}, runtime, &hostAgentStatusResponse{}, executionCancel)
	if err == nil || !strings.Contains(err.Error(), "heartbeat fence lost") {
		t.Fatalf("cancelled session binding error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("session binding requests = %d, want 1", requests.Load())
	}
}

func TestPostSessionOperationIsBoundedAfterCancellation(t *testing.T) {
	cancel := make(chan error, 1)
	cancel <- errors.New("cancelled")
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	run := freshRunLifecycle{runtime: runRuntime{Cancel: cancel, CancelWait: 10 * time.Millisecond}}
	err := run.runPostSessionOperation("artifact collection", func() error {
		<-release
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "artifact collection did not finish") {
		t.Fatalf("bounded post-session operation error = %v", err)
	}
	if !run.cancellationObserved || !run.sessionOperationUnsettled {
		t.Fatalf("post-session cancellation state = observed:%t unsettled:%t", run.cancellationObserved, run.sessionOperationUnsettled)
	}
}

func TestHostAgentCompletionIdentityRejectsAssignmentChanges(t *testing.T) {
	assignment := hostAgentAssignmentRecord{
		RunID: "20260715T120000.000000000Z", HostID: "maya-win-01",
		Submission: controlPlaneSubmission{Scenario: "smoke", TargetProfile: "default"},
	}
	valid := runCommandJSON{
		Version: controlPlaneAPIVersion, Kind: "run", Accepted: true, RunID: assignment.RunID,
		Scenario: "smoke", TargetProfile: "default", Host: "maya-win-01", StopPolicy: "stopped",
	}
	mutations := map[string]func(*runCommandJSON){
		"version":   func(value *runCommandJSON) { value.Version++ },
		"kind":      func(value *runCommandJSON) { value.Kind = "other" },
		"accepted":  func(value *runCommandJSON) { value.Accepted = false },
		"run":       func(value *runCommandJSON) { value.RunID = "20260715T120001.000000000Z" },
		"scenario":  func(value *runCommandJSON) { value.Scenario = "other" },
		"profile":   func(value *runCommandJSON) { value.TargetProfile = "other" },
		"host":      func(value *runCommandJSON) { value.Host = "maya-win-02" },
		"stop":      func(value *runCommandJSON) { value.StopPolicy = "unresolved" },
		"follow-up": func(value *runCommandJSON) { value.FollowUpCommands = []string{"unsafe"} },
	}
	if err := validateHostAgentCompletionIdentity(assignment, valid); err != nil {
		t.Fatalf("valid completion identity: %v", err)
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := valid
			mutate(&changed)
			if err := validateHostAgentCompletionIdentity(assignment, changed); err == nil {
				t.Fatal("changed completion identity was accepted")
			}
		})
	}
}

func TestFinishingHostAgentAssignmentRequiresImmutableTerminalIdentity(t *testing.T) {
	assigned := runLedgerRecord{
		Version: 1, RunID: "20260715T120000.000000000Z", Scenario: "smoke", TargetProfile: "default",
		Host: "maya-win-01", State: "assigned", AcceptedAt: "2026-07-15T12:00:00Z",
	}
	terminalLedger := assigned
	terminalLedger.State = "completed"
	terminalLedger.Status = resultStatusPassed
	terminal := runCommandJSON{
		Version: controlPlaneAPIVersion, Kind: "run", Accepted: true, RunID: assigned.RunID,
		Scenario: assigned.Scenario, TargetProfile: assigned.TargetProfile, Host: assigned.Host,
		Status: resultStatusPassed, StopPolicy: "stopped",
	}
	assignment := hostAgentAssignmentRecord{
		Version: hostAgentAPIVersion, RunID: assigned.RunID, AgentID: "windows-agent-01", HostID: assigned.Host,
		LockToken: strings.Repeat("a", 32), State: "finishing",
		Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: assigned.Scenario, TargetProfile: assigned.TargetProfile, StopAfter: stopAfterAlways},
		Terminal:   &terminal, TerminalLedger: &terminalLedger, AssignedLedger: &assigned,
	}
	if err := validateFinishingHostAgentAssignment(assignment); err != nil {
		t.Fatalf("valid finishing assignment: %v", err)
	}
	invalid := assignment
	wrongLedger := terminalLedger
	wrongLedger.RunID = "20260715T120001.000000000Z"
	invalid.TerminalLedger = &wrongLedger
	if err := validateFinishingHostAgentAssignment(invalid); err == nil {
		t.Fatal("finishing assignment accepted another Run Ledger identity")
	}
}

func TestRecoveredHostAgentDoesNotStealUnrelatedSharedLock(t *testing.T) {
	repoDir := privateTempDir(t)
	release, locked, err := acquireHostLock(repoDir, "maya-win-01")
	if err != nil || locked {
		t.Fatalf("acquire shared Host Lock: locked=%t error=%v", locked, err)
	}
	defer func() { _ = release() }()
	firstRun := "20260715T120000.000000000Z"
	if err := markHostLockActive(repoDir, "maya-win-01", firstRun); err != nil {
		t.Fatalf("mark first shared Host Lock: %v", err)
	}
	if err := reestablishHostAgentSharedLock(repoDir, "maya-win-01", "20260715T120001.000000000Z"); err == nil || !strings.Contains(err.Error(), "another run") {
		t.Fatalf("unrelated shared Host Lock recovery error = %v", err)
	}
	content, err := os.ReadFile(filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts", "maya-win-01.lock"))
	if err != nil || !bytes.Contains(content, []byte("activeRun: "+firstRun)) {
		t.Fatalf("unrelated shared Host Lock changed: content=%q error=%v", content, err)
	}
}

func TestHostAgentResultFileValidationRejectsHostileUploads(t *testing.T) {
	runID := "20260715T120000.000000000Z"
	ledger := filepath.ToSlash(filepath.Join(".maya-stall", "state", "ledger", "runs", runID, "run.json"))
	evidence := filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID, evidenceBundleFileName))
	valid := []controlPlaneFile{{Path: ledger, Kind: "file"}, {Path: evidence, Kind: "file"}}
	if err := validateHostAgentResultFiles(runID, valid); err != nil {
		t.Fatalf("valid Host Agent result files: %v", err)
	}
	cases := map[string][]controlPlaneFile{
		"absolute":        {{Path: "/tmp/run.json", Kind: "file"}, {Path: evidence, Kind: "file"}},
		"traversal":       {{Path: "../run.json", Kind: "file"}, {Path: evidence, Kind: "file"}},
		"backslash":       {{Path: strings.ReplaceAll(ledger, "/", `\`), Kind: "file"}, {Path: evidence, Kind: "file"}},
		"duplicate":       {valid[0], valid[0], valid[1]},
		"missing":         {valid[0]},
		"directory-bytes": {{Path: ledger, Kind: "directory", Content: []byte("bytes")}, valid[1]},
		"outside":         {{Path: ledger, Kind: "file"}, {Path: "outside.txt", Kind: "file"}, valid[1]},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateHostAgentResultFiles(runID, files); err == nil {
				t.Fatal("hostile Host Agent upload was accepted")
			}
		})
	}
}

func TestHostAgentResultTransferSanitizesAgentPaths(t *testing.T) {
	repoDir := t.TempDir()
	runID := "20260715T120000.000000000Z"
	privateRoot := `C:\Maya-Stall\Agent`
	for path, content := range map[string]string{
		filepath.Join(".maya-stall", "state", "ledger", "runs", runID, "run.json"):      `{"diagnostic":"c:\\maya-stall\\agent\\host"}`,
		filepath.Join(".maya-stall", "state", "ledger", "runs", runID, "events.ndjson"): `{"message":"C:\\MAYA-STALL\\AGENT\\runs"}`,
		filepath.Join("artifacts", "maya-stall", runID, evidenceBundleFileName):         `{"diagnostic":"C:\\maya-stall\\agent\\evidence"}`,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repoDir, path)), 0o700); err != nil {
			t.Fatalf("create result path: %v", err)
		}
		if err := os.WriteFile(filepath.Join(repoDir, path), []byte(content), 0o600); err != nil {
			t.Fatalf("write result path: %v", err)
		}
	}
	files, err := buildHostAgentResultFilesSanitized(repoDir, runID, []string{privateRoot})
	if err != nil {
		t.Fatalf("build sanitized Host Agent result: %v", err)
	}
	for _, file := range files {
		lowerContent := strings.ToLower(string(file.Content))
		if strings.Contains(lowerContent, strings.ToLower(privateRoot)) || strings.Contains(lowerContent, strings.ToLower(strings.ReplaceAll(privateRoot, `\`, `\\`))) {
			t.Fatalf("transferred %s contains Agent path: %s", file.Path, file.Content)
		}
	}
}

func TestQuarantinedHostAgentFailureRetainsWorkspace(t *testing.T) {
	var got hostAgentFailureRequest
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&got); err != nil {
			t.Fatalf("decode quarantine request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(runCommandJSON{Version: 1, Kind: "run", Status: resultStatusFailed})
	}))
	t.Cleanup(server.Close)
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	workRoot := privateTempDir(t)
	runID := "20260715T120000.000000000Z"
	runRoot := filepath.Join(workRoot, "runs", runID)
	if err := os.MkdirAll(runRoot, 0o700); err != nil {
		t.Fatalf("create quarantined Agent workspace: %v", err)
	}
	marker := filepath.Join(runRoot, "retained.txt")
	if err := os.WriteFile(marker, []byte("retained"), 0o600); err != nil {
		t.Fatalf("write quarantined Agent workspace: %v", err)
	}
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	err := quarantineConfirmedHostAgentAssignment(hostAgentRunOnceOptions{
		ControlPlane: server.URL, AgentID: "windows-agent-01", HostID: "maya-win-01", WorkRoot: workRoot,
		CredentialEnv: "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", SessionID: strings.Repeat("b", 32),
	}, hostAgentAssignmentResponse{RunID: runID, LockToken: strings.Repeat("a", 32)}, runtime, "stopping Maya", errors.New("stop failed"))
	if err == nil || !got.Quarantine {
		t.Fatalf("quarantined Host Agent error = %v request=%+v", err, got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("quarantined Agent workspace was removed: %v", err)
	}
}

func TestQuarantinedAssignmentRetainsBothHostLocksAndRejectsTakeover(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	runtime := defaultRunRuntime()
	handler, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}
	var runStdout bytes.Buffer
	var runStderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &runStdout, &runStderr, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "locked")
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	var assignment hostAgentAssignmentResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/next", hostAgentNextRequest{
		Version: hostAgentAPIVersion, SessionID: status.SessionID,
	}, runtime, http.StatusOK, &assignment); err != nil {
		t.Fatalf("read Host Agent assignment: %v", err)
	}
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+assignment.RunID+"/confirm", hostAgentLockRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: status.SessionID,
	}, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("confirm Host Agent assignment: %v", err)
	}
	var terminal runCommandJSON
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+assignment.RunID+"/fail", hostAgentFailureRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken,
		SessionID: status.SessionID, Diagnostic: "Maya session shutdown is unverified", Quarantine: true,
	}, runtime, http.StatusOK, &terminal); err != nil {
		t.Fatalf("quarantine Host Agent assignment: %v", err)
	}
	if code := <-runDone; code != 1 {
		t.Fatalf("quarantined Scenario exit code = %d; stdout: %s stderr: %s", code, runStdout.String(), runStderr.String())
	}
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "quarantined" || status.RunID != assignment.RunID || status.SessionID != "" {
		t.Fatalf("quarantined Host Agent status = %+v", status)
	}
	var replacement hostAgentStatusResponse
	err = postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &replacement)
	var statusErr *controlPlaneHTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("quarantined Agent takeover error = %v, want HTTP 409", err)
	}
	if _, locked, err := acquireHostLock(filepath.Join(dataDir, "fake-host"), "maya-win-01"); err != nil || !locked {
		t.Fatalf("shared fake Host Lock after quarantine: locked=%t err=%v", locked, err)
	}
	var durable hostAgentAssignmentRecord
	if err := readPrivateJSON(filepath.Join(dataDir, "assignments", assignment.RunID+".json"), &durable); err != nil || durable.State != "quarantined" {
		t.Fatalf("durable quarantined assignment = %+v err=%v", durable, err)
	}
}

func TestHostAgentRejectsRemoteRunIDPathTraversal(t *testing.T) {
	options := hostAgentRunOnceOptions{AgentID: "windows-agent-01", HostID: "maya-win-01"}
	for _, runID := range []string{"../../outside", "run/child", `run\\child`} {
		assignment := hostAgentAssignmentResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-assignment", RunID: runID,
			AgentID: options.AgentID, HostID: options.HostID, LockToken: strings.Repeat("a", 32),
		}
		if err := validateHostAgentAssignment(options, assignment); err == nil {
			t.Fatalf("remote Run ID %q was accepted", runID)
		}
	}
}

func TestHostAgentRejectsDotAgentIDs(t *testing.T) {
	for _, agentID := range []string{".", "..", "Windows-Agent-01", "agent.", "con"} {
		_, err := parseControlPlaneEnrollAgentArgs([]string{
			"--control-plane", "https://maya-stall.example.com", "--agent-id", agentID,
			"--host", "maya-win-01", "--credential-env", "MAYA_STALL_HOST_AGENT_CREDENTIAL",
		})
		if err == nil {
			t.Fatalf("enrollment accepted dot Agent ID %q", agentID)
		}
	}
	if _, err := parseControlPlaneEnrollAgentArgs([]string{
		"--control-plane", "https://maya-stall.example.com", "--agent-id", "windows-agent-01",
		"--host", "Maya-Win-01", "--credential-env", "MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}); err == nil {
		t.Fatal("enrollment accepted a case-variant Maya Host state ID")
	}
}

func TestHostAgentReportsPostConfirmationSetupFailureAndReleasesLock(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	if err := os.Mkdir(filepath.Join(agentWorkRoot, "host-config.yaml"), 0o700); err != nil {
		t.Fatalf("create blocking Host config directory: %v", err)
	}
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)

	agentDone := make(chan int, 1)
	go func() {
		agentDone <- RunWithRuntime([]string{
			"host-agent", "run-once", "--control-plane", server.URL,
			"--agent-id", "windows-agent-01", "--host", "maya-win-01",
			"--work-root", agentWorkRoot, "--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
		}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "ready")
	var runStdout bytes.Buffer
	var runStderr bytes.Buffer
	runCode := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &runStdout, &runStderr, repoDir, "test-version", runtime)
	if runCode != 1 {
		t.Fatalf("failed Host Agent Scenario exit code = %d, want 1; stdout: %s stderr: %s", runCode, runStdout.String(), runStderr.String())
	}
	results := decodeRunJSONLines(t, runStdout.Bytes())
	if len(results) != 2 || results[1].Host != "maya-win-01" || results[1].TargetProfile != "default" {
		t.Fatalf("failed Host Agent Scenario identity = %+v", results)
	}
	if agentCode := <-agentDone; agentCode != 1 {
		t.Fatalf("failed Windows Host Agent exit code = %d, want 1", agentCode)
	}
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "offline")
	entries, err := os.ReadDir(filepath.Join(dataDir, "host-locks"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("Host Locks after reported Agent failure = %+v, err %v", entries, err)
	}
	runEntries, err := os.ReadDir(filepath.Join(agentWorkRoot, "runs"))
	if err != nil || len(runEntries) != 0 {
		t.Fatalf("Agent run workspaces after reported failure = %+v, err %v", runEntries, err)
	}
}

func TestSecondHostAgentProcessRegistrationIsRejected(t *testing.T) {
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, t.TempDir(), server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var first hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &first); err != nil {
		t.Fatalf("register first Windows Host Agent process: %v", err)
	}
	var second hostAgentStatusResponse
	err = postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &second)
	var statusErr *controlPlaneHTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("second Windows Host Agent registration error = %v, want HTTP 409", err)
	}
	status := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.SessionID != first.SessionID || status.State != "ready" {
		t.Fatalf("Host Agent after second process registration = %+v, want first session unchanged", status)
	}
}

func TestExpiredHostAgentSessionAllowsReEnrollment(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	handler, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, t.TempDir(), server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var first hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &first); err != nil {
		t.Fatalf("register Windows Host Agent process: %v", err)
	}
	now = now.Add(hostAgentSessionLease)
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "rotated-token-0123456789abcdef0123456789abcdef")
	enrollTestHostAgent(t, t.TempDir(), server.URL, runtime)
	status := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "enrolled" || status.SessionID != "" {
		t.Fatalf("re-enrolled Host Agent status = %+v", status)
	}
}

func TestHostAgentMutationRevalidatesRotatedCredential(t *testing.T) {
	currentCredential := "current-token-0123456789abcdef0123456789abcdef"
	digest := sha256.Sum256([]byte(currentCredential))
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	handler.hostAgents["windows-agent-01"] = &controlPlaneHostAgent{
		enrollment: hostAgentEnrollmentRecord{
			Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01",
			CredentialSHA256: fmt.Sprintf("%x", digest),
		},
		status: hostAgentStatusResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-status", AgentID: "windows-agent-01", HostID: "maya-win-01",
			Slots: 1, State: "enrolled",
		},
		notify: make(chan struct{}),
	}
	content, err := json.Marshal(hostAgentRegistrationRequest{
		Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01", Slots: 1, SessionBinding: true,
		Capabilities: testHostAgentCapabilityRecord("maya-win-01", time.Now()),
	})
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/host-agents/windows-agent-01/register", bytes.NewReader(content))
	request.Header.Set("Authorization", "Bearer "+testHostAgentCredential)
	response := httptest.NewRecorder()
	handler.serveHostAgentRegistration(response, request, "windows-agent-01")
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("rotated credential registration HTTP = %d, want 401", response.Code)
	}
	if handler.hostAgents["windows-agent-01"].status.SessionID != "" {
		t.Fatal("revoked credential created a process session")
	}
}

func TestExpiredHostAgentSessionAllowsFencedProcessTakeover(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	handler, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var first hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &first); err != nil {
		t.Fatalf("register first Windows Host Agent process: %v", err)
	}
	var runStdout bytes.Buffer
	var runStderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &runStdout, &runStderr, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "locked")
	locked := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	staleRepo := filepath.Join(agentWorkRoot, "runs", locked.RunID, "repo")
	if err := os.MkdirAll(staleRepo, 0o700); err != nil {
		t.Fatalf("create stale takeover repo: %v", err)
	}
	staleRun := newFreshRun(staleRepo, runOptions{
		ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: locked.RunID,
	}, runtime).(*freshRunLifecycle)
	if err := staleRun.accept(); err != nil {
		t.Fatalf("accept stale takeover run: %v", err)
	}
	now = now.Add(hostAgentSessionLease + time.Second)
	var replacement hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &replacement); err != nil {
		t.Fatalf("take over expired Windows Host Agent session: %v", err)
	}
	if replacement.SessionID == "" || replacement.SessionID == first.SessionID || replacement.State != "locked" {
		t.Fatalf("replacement Windows Host Agent session = %+v, first = %+v", replacement, first)
	}
	var assignment hostAgentAssignmentResponse
	err = postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/next", hostAgentNextRequest{
		Version: hostAgentAPIVersion, SessionID: first.SessionID,
	}, runtime, http.StatusOK, &assignment)
	var statusErr *controlPlaneHTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("expired Host Agent process poll error = %v, want HTTP 409", err)
	}
	now = now.Add(hostAgentSessionLease + time.Second)
	var agentStdout bytes.Buffer
	var agentStderr bytes.Buffer
	agentCode := RunWithRuntime([]string{
		"host-agent", "run-once", "--control-plane", server.URL,
		"--agent-id", "windows-agent-01", "--host", "maya-win-01",
		"--work-root", agentWorkRoot, "--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, &agentStdout, &agentStderr, repoDir, "test-version", runtime)
	if agentCode != 0 {
		t.Fatalf("replacement Windows Host Agent exit code = %d; stdout: %s stderr: %s", agentCode, agentStdout.String(), agentStderr.String())
	}
	select {
	case runCode := <-runDone:
		if runCode != 0 {
			t.Fatalf("taken-over Scenario exit code = %d; stdout: %s stderr: %s", runCode, runStdout.String(), runStderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("taken-over Scenario did not complete")
	}
}

func TestExpiredReadyHostAgentIsNotSelected(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	handler, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}
	now = now.Add(hostAgentSessionLease + time.Second)
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "offline" || status.SessionID != "" {
		t.Fatalf("expired Host Agent status read = %+v, want offline", status)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("expired Agent Scenario exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "offline" || status.RunID != "" || status.SessionID != "" {
		t.Fatalf("expired ready Host Agent = %+v, want offline", status)
	}
}

func TestStaleCapabilityReportNeverReceivesAssignment(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	handler, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now.Add(-mayaHostCapabilityFreshness-time.Second))
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "default"}
	registration := hostAgentRegistrationRequest{
		Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01", Slots: 1, SessionBinding: true,
		Capabilities: report,
	}
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime); code != 1 {
		t.Fatalf("stale-report Scenario exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	results := decodeRunJSONLines(t, stdout.Bytes())
	if len(results) != 2 || !strings.Contains(results[1].Diagnostic, "capability record is stale") {
		t.Fatalf("stale-report result = %+v", results)
	}
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "ready" || status.RunID != "" {
		t.Fatalf("stale-report Agent received assignment: %+v", status)
	}
}

func TestSharedFakeHostLockQueuesCompatibleRunWithoutAgentAssignment(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}
	release, locked, err := acquireHostLock(filepath.Join(dataDir, "fake-host"), "maya-win-01")
	if err != nil || locked {
		t.Fatalf("acquire shared fake Host Lock fixture: locked=%t err=%v", locked, err)
	}
	defer func() { _ = release() }()
	done := make(chan int, 1)
	go func() {
		done <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	queuedRunID := waitForQueuedRunID(t, handler.(*controlPlaneHandler))
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "ready" || status.RunID != "" {
		t.Fatalf("Host Agent selected despite shared fake Host Lock: %+v", status)
	}
	if _, err := handler.(*controlPlaneHandler).cancelQueuedRun(queuedRunID); err != nil {
		t.Fatalf("cancel shared-lock queued Run: %v", err)
	}
	select {
	case code := <-done:
		if code != 1 {
			t.Fatalf("canceled shared-lock Run exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled shared-lock Run did not finish")
	}
}

func TestLegacyHostAgentWithoutSessionBindingDoesNotReceiveNewAssignment(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	runtime := defaultRunRuntime()
	handlerValue, err := newControlPlaneHandler(privateTempDir(t), "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	handler.hostAgents["windows-agent-01"] = &controlPlaneHostAgent{
		enrollment: hostAgentEnrollmentRecord{Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01"},
		status: hostAgentStatusResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-status", AgentID: "windows-agent-01", HostID: "maya-win-01",
			Slots: 1, State: "ready", SessionID: strings.Repeat("a", 32), SessionBinding: false,
		},
		notify: make(chan struct{}), sessionExpiresAt: time.Now().Add(time.Minute),
	}
	outcome, runErr := handler.runScenarioThroughHostAgent(repoDir, controlPlaneSubmission{
		Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways,
	}, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways}, runtime)
	if runErr == nil || !strings.Contains(runErr.Error(), "no registered ready Windows Host Agent") || outcome.Result.Status != resultStatusFailed {
		t.Fatalf("legacy Agent selection outcome = %+v, error = %v", outcome, runErr)
	}
	if status := handler.hostAgents["windows-agent-01"].status; status.State != "ready" || status.RunID != "" {
		t.Fatalf("legacy Agent mutated by rejected selection: %+v", status)
	}
}

func TestHostAgentAcceptanceReportingFailureDoesNotOrphanQueue(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	runtime := defaultRunRuntime()
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	handler.hostAgents["windows-agent-01"] = &controlPlaneHostAgent{
		enrollment: hostAgentEnrollmentRecord{Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01"},
		status: hostAgentStatusResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-status", AgentID: "windows-agent-01", HostID: "maya-win-01",
			Slots: 1, State: "locked", RunID: "existing-run", SessionID: strings.Repeat("a", 32), SessionBinding: true,
			Capabilities: testHostAgentCapabilityRecord("maya-win-01", runtime.Now()),
		},
		notify: make(chan struct{}), sessionExpiresAt: time.Now().Add(time.Minute),
	}
	checked := make(chan struct{})
	runtime.AcceptedCheck = func() error {
		close(checked)
		return errors.New("acceptance stream failed")
	}
	type result struct {
		outcome runOutcome
		err     error
	}
	done := make(chan result, 1)
	runID := "20260715T120000.000000000Z"
	go func() {
		outcome, runErr := handler.runScenarioThroughHostAgent(repoDir, controlPlaneSubmission{
			Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways,
		}, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, runtime)
		done <- result{outcome: outcome, err: runErr}
	}()
	if queuedRunID := waitForQueuedRunID(t, handler); queuedRunID != runID {
		t.Fatalf("queued Run ID = %s, want %s", queuedRunID, runID)
	}
	<-checked
	record, err := readRunLedgerRecord(repoDir, runID)
	if err != nil {
		t.Fatalf("read acceptance failure ledger: %v", err)
	}
	if record.State != "queued" || record.Status != "" {
		t.Fatalf("acceptance failure ledger = %+v", record)
	}
	agent := handler.hostAgents["windows-agent-01"]
	if agent.status.State != "locked" || agent.status.RunID != "existing-run" || handler.assignments[runID] != nil {
		t.Fatalf("Host Agent after acceptance failure = %+v", agent.status)
	}
	handler.mu.Lock()
	queued := handler.queuedRuns[runID]
	delete(handler.queuedRuns, runID)
	close(queued.done)
	handler.mu.Unlock()
	finished := <-done
	if !errors.Is(finished.err, errQueuedRunCanceled) {
		t.Fatalf("stopped acceptance-failure queue error = %v", finished.err)
	}
}

func TestHostAgentLeaseExpiryAfterAcceptanceKeepsRunQueued(t *testing.T) {
	fixtureRepo := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	runID := now.Format("20060102T150405.000000000Z")
	repoDir := filepath.Join(dataDir, "runs", runID, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(fixtureRepo, ".maya-stall.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, ".maya-stall.yaml"), config, 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	handler.hostAgents["windows-agent-01"] = &controlPlaneHostAgent{
		enrollment: hostAgentEnrollmentRecord{Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01"},
		status: hostAgentStatusResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-status", AgentID: "windows-agent-01", HostID: "maya-win-01",
			Slots: 1, State: "ready", SessionID: strings.Repeat("a", 32), SessionBinding: true,
			Capabilities: testHostAgentCapabilityRecord("maya-win-01", runtime.Now()),
		},
		notify: make(chan struct{}), sessionExpiresAt: now.Add(hostAgentSessionLease),
	}
	runtime.Accepted = func(runOutcome) { now = now.Add(hostAgentSessionLease) }
	type result struct {
		outcome runOutcome
		err     error
	}
	done := make(chan result, 1)
	go func() {
		outcome, runErr := handler.runScenarioThroughHostAgent(repoDir, controlPlaneSubmission{
			Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways,
		}, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, runtime)
		done <- result{outcome: outcome, err: runErr}
	}()
	runID = waitForQueuedRunID(t, handler)
	status := controlPlaneStatusResponse{RunID: runID}
	handler.addControlPlaneQueueStatus(&status)
	if status.WaitReason != "waiting-for-compatible-host" || status.QueuePosition != 1 {
		t.Fatalf("expired Agent queue status = %+v", status)
	}
	if _, err := handler.cancelQueuedRun(runID); err != nil {
		t.Fatalf("cancel queued Run after lease expiry: %v", err)
	}
	finished := <-done
	if !errors.Is(finished.err, errQueuedRunCanceled) || finished.outcome.Result.Status != resultStatusFailed {
		t.Fatalf("canceled expired-agent outcome = %+v, error = %v", finished.outcome, finished.err)
	}
	agent := handler.hostAgents["windows-agent-01"]
	if agent.status.State != "offline" || agent.status.RunID != "" || agent.status.SessionID != "" || len(handler.assignments) != 0 {
		t.Fatalf("Host Agent after expired reservation = %+v assignments=%d", agent.status, len(handler.assignments))
	}
}

func TestPartialHostAgentAssignmentTransitionQuarantinesSlot(t *testing.T) {
	dataDir := privateTempDir(t)
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	runID := now.Format("20060102T150405.000000000Z")
	repoDir := filepath.Join(dataDir, "runs", runID, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("create run repo: %v", err)
	}
	mustWriteFile(t, filepath.Join(repoDir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    expectedOutputs:
      scenarioResult: "outputs/smoke-result.json"
`)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	credentialHash := sha256.Sum256([]byte(testHostAgentCredential))
	enrollment := hostAgentEnrollmentRecord{
		Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01",
		CredentialSHA256: fmt.Sprintf("%x", credentialHash), EnrolledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writePrivateJSON(filepath.Join(dataDir, "host-agents", enrollment.AgentID, "enrollment.json"), enrollment); err != nil {
		t.Fatalf("persist enrollment fixture: %v", err)
	}
	handler.hostAgents["windows-agent-01"] = &controlPlaneHostAgent{
		enrollment: enrollment,
		status: hostAgentStatusResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-status", AgentID: "windows-agent-01", HostID: "maya-win-01",
			Slots: 1, State: "ready", SessionID: strings.Repeat("a", 32), SessionBinding: true,
			Capabilities: testHostAgentCapabilityRecord("maya-win-01", runtime.Now()),
		},
		notify: make(chan struct{}), sessionExpiresAt: now.Add(hostAgentSessionLease),
	}
	transactionDir := filepath.Join(dataDir, "assignment-transactions")
	if err := os.Remove(transactionDir); err != nil {
		t.Fatalf("remove transaction directory: %v", err)
	}
	if err := os.WriteFile(transactionDir, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("block transaction directory: %v", err)
	}
	outcome, runErr := handler.runScenarioThroughHostAgent(repoDir, controlPlaneSubmission{
		Version: controlPlaneAPIVersion, Scenario: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways,
	}, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: runID}, runtime)
	if runErr == nil || !strings.Contains(runErr.Error(), "slot quarantined") || outcome.Result.Status != resultStatusFailed {
		t.Fatalf("partial transition outcome = %+v, error = %v", outcome, runErr)
	}
	agent := handler.hostAgents["windows-agent-01"]
	if agent.status.State != "quarantined" || agent.status.RunID == "" || agent.status.SessionID != "" {
		t.Fatalf("Host Agent after partial transition = %+v", agent.status)
	}
	assignment := handler.assignments[outcome.RunID]
	if assignment == nil || assignment.record.State != "quarantined" || !assignment.finished {
		t.Fatalf("partial transition assignment = %+v", assignment)
	}
	if err := os.Remove(transactionDir); err != nil {
		t.Fatalf("remove blocked transaction path: %v", err)
	}
	if err := os.Mkdir(transactionDir, 0o700); err != nil {
		t.Fatalf("restore transaction directory: %v", err)
	}
	staleAssigned := assignment.record
	staleAssigned.State = "assigned"
	staleAssigned.Terminal = nil
	staleAssigned.TerminalLedger = nil
	if err := writePrivateJSON(filepath.Join(transactionDir, outcome.RunID+".json"), staleAssigned); err != nil {
		t.Fatalf("persist stale assigned transition: %v", err)
	}
	restarted, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("restart with quarantine tombstone: %v", err)
	}
	restartedAgent := restarted.(*controlPlaneHandler).hostAgents[enrollment.AgentID]
	if restartedAgent.status.State != "quarantined" || restartedAgent.status.RunID != outcome.RunID {
		t.Fatalf("restarted quarantine tombstone = %+v", restartedAgent.status)
	}
	restartedAssignment := restarted.(*controlPlaneHandler).assignments[outcome.RunID]
	if restartedAssignment == nil || restartedAssignment.record.State != "quarantined" || restartedAssignment.record.TerminalLedger == nil || restartedAssignment.record.TerminalLedger.Status != resultStatusFailed || restartedAssignment.record.TerminalLedger.Host != enrollment.HostID {
		t.Fatalf("recovered stale transition = %+v", restartedAssignment)
	}
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	restartedServer := httptest.NewTLSServer(restarted)
	t.Cleanup(restartedServer.Close)
	restartedRuntime := defaultRunRuntime()
	restartedRuntime.ControlPlaneHTTPClient = restartedServer.Client()
	registration := testHostAgentRegistration(enrollment.AgentID, enrollment.HostID, restartedRuntime.Now())
	var restartedStatus hostAgentStatusResponse
	if err := postControlPlaneJSON(restartedServer.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, restartedRuntime, http.StatusOK, &restartedStatus); err == nil {
		t.Fatal("restarted quarantined Windows Host Agent registration succeeded")
	} else if statusErr := new(controlPlaneHTTPStatusError); !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("restarted quarantined registration error = %v", err)
	}
}

func TestRegisteredHostAgentRejectsRetainedStopPolicyBeforeAssignment(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "--stop-after", "never", "smoke"}, &stdout, &stderr, repoDir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("retained Stop Policy exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "ready" || status.RunID != "" {
		t.Fatalf("Host Agent after retained Stop Policy = %+v", status)
	}
	for _, child := range []string{"assignments", "host-locks"} {
		entries, err := os.ReadDir(filepath.Join(dataDir, child))
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s after retained Stop Policy = %+v, err %v", child, entries, err)
		}
	}
}

func TestControlPlaneRecoversJournaledHostAgentAssignmentTransition(t *testing.T) {
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, t.TempDir(), server.URL, runtime)
	server.Close()

	record := hostAgentAssignmentRecord{
		Version: hostAgentAPIVersion, RunID: "20260715T120000.000000000Z", AgentID: "windows-agent-01", HostID: "maya-win-01",
		LockToken: strings.Repeat("a", 32), State: "confirmed", CreatedAt: "2026-07-15T12:00:00Z",
		Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways},
	}
	repoDir := filepath.Join(dataDir, "runs", record.RunID, "repo")
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		t.Fatalf("create recovered run repo: %v", err)
	}
	acceptedRun := newFreshRun(repoDir, runOptions{
		ScenarioName: "smoke", TargetProfile: "agent", StopAfter: stopAfterAlways, AssignedRunID: record.RunID,
	}, defaultRunRuntime()).(*freshRunLifecycle)
	if err := acceptedRun.accept(); err != nil {
		t.Fatalf("accept recovered run fixture: %v", err)
	}
	assignedLedger, err := readRunLedgerRecord(repoDir, record.RunID)
	if err != nil {
		t.Fatalf("read recovered assigned ledger: %v", err)
	}
	assignedLedger.Host = record.HostID
	record.AssignedLedger = &assignedLedger
	transactionPath := filepath.Join(dataDir, "assignment-transactions", record.RunID+".json")
	if err := writePrivateJSON(transactionPath, record); err != nil {
		t.Fatalf("persist interrupted transition: %v", err)
	}
	restarted, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("recover journaled transition: %v", err)
	}
	if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
		t.Fatalf("recovered transaction still exists: %v", err)
	}
	concrete := restarted.(*controlPlaneHandler)
	assignment := concrete.assignments[record.RunID]
	if assignment == nil || assignment.record.State != "confirmed" || assignment.record.LockToken != record.LockToken {
		t.Fatalf("recovered assignment = %+v", assignment)
	}
	restartedServer := httptest.NewTLSServer(restarted)
	t.Cleanup(restartedServer.Close)
	restartedRuntime := defaultRunRuntime()
	restartedRuntime.ControlPlaneHTTPClient = restartedServer.Client()
	registration := testHostAgentRegistration(record.AgentID, record.HostID, restartedRuntime.Now())
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(restartedServer.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, restartedRuntime, http.StatusOK, &status); err == nil {
		t.Fatal("replacement Windows Host Agent registered during restart grace")
	} else if statusErr := new(controlPlaneHTTPStatusError); !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("restart-grace registration error = %v", err)
	}
	concrete.hostAgents[record.AgentID].takeoverNotBefore = time.Now().Add(-time.Second)
	if err := postControlPlaneJSON(restartedServer.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, restartedRuntime, http.StatusOK, &status); err == nil {
		t.Fatal("replacement Windows Host Agent registered after confirmed assignment loss")
	} else if statusErr := new(controlPlaneHTTPStatusError); !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusConflict {
		t.Fatalf("expired restart-grace registration error = %v", err)
	}
	if assignment.record.State != "quarantined" || !assignment.finished {
		t.Fatalf("lost recovered assignment = %+v", assignment)
	}
	var lock hostAgentLockRecord
	if err := readPrivateJSON(filepath.Join(dataDir, "host-locks", record.HostID+".json"), &lock); err != nil {
		t.Fatalf("read quarantined recovered Host Lock: %v", err)
	}
	if lock.State != "quarantined" || lock.RunID != record.RunID {
		t.Fatalf("quarantined recovered Host Lock = %+v", lock)
	}
}

func TestControlPlaneRecoversEachPartialHostAgentTransitionWrite(t *testing.T) {
	for _, stage := range []string{"journal", "ledger", "host-lock", "assignment"} {
		t.Run("active-"+stage, func(t *testing.T) {
			dataDir := privateTempDir(t)
			handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
			if err != nil {
				t.Fatalf("create Control Plane handler: %v", err)
			}
			server := httptest.NewTLSServer(handler)
			t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
			t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
			runtime := defaultRunRuntime()
			runtime.ControlPlaneHTTPClient = server.Client()
			enrollTestHostAgent(t, t.TempDir(), server.URL, runtime)
			server.Close()
			record := hostAgentAssignmentRecord{
				Version: hostAgentAPIVersion, RunID: "20260715T120000.000000000Z", AgentID: "windows-agent-01", HostID: "maya-win-01",
				LockToken: strings.Repeat("a", 32), State: "confirmed", CreatedAt: "2026-07-15T12:00:00Z",
				Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways},
			}
			repoDir := filepath.Join(dataDir, "runs", record.RunID, "repo")
			if err := os.MkdirAll(repoDir, 0o700); err != nil {
				t.Fatalf("create recovered run repo: %v", err)
			}
			accepted := newFreshRun(repoDir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: record.RunID}, defaultRunRuntime()).(*freshRunLifecycle)
			if err := accepted.accept(); err != nil {
				t.Fatalf("accept recovered run: %v", err)
			}
			assigned, err := readRunLedgerRecord(repoDir, record.RunID)
			if err != nil {
				t.Fatalf("read assigned ledger: %v", err)
			}
			assigned.Host = record.HostID
			record.AssignedLedger = &assigned
			transactionPath := filepath.Join(dataDir, "assignment-transactions", record.RunID+".json")
			if err := writePrivateJSON(transactionPath, record); err != nil {
				t.Fatalf("write active transaction: %v", err)
			}
			transitionStore := newHostAgentTransitionStore(dataDir)
			if stage == "ledger" || stage == "host-lock" || stage == "assignment" {
				if err := newRunLedgerStore(repoDir).Replace(assigned); err != nil {
					t.Fatalf("write partial assigned ledger: %v", err)
				}
			}
			if stage == "host-lock" || stage == "assignment" {
				if err := transitionStore.persistHostLock(record, record.State); err != nil {
					t.Fatalf("write partial Host Lock: %v", err)
				}
			}
			if stage == "assignment" {
				if err := transitionStore.SaveAssignment(record); err != nil {
					t.Fatalf("write partial assignment: %v", err)
				}
			}
			if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err != nil {
				t.Fatalf("recover partial active transition: %v", err)
			}
			if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
				t.Fatalf("active transaction remains: %v", err)
			}
			var durable hostAgentAssignmentRecord
			if err := readPrivateJSON(filepath.Join(dataDir, "assignments", record.RunID+".json"), &durable); err != nil || durable.State != "confirmed" {
				t.Fatalf("recovered active assignment = %+v err=%v", durable, err)
			}
		})
	}

	for _, stage := range []string{"journal", "assignment", "terminal-ledger", "lock-removal"} {
		t.Run("completed-"+stage, func(t *testing.T) {
			dataDir := privateTempDir(t)
			handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
			if err != nil {
				t.Fatalf("create Control Plane handler: %v", err)
			}
			server := httptest.NewTLSServer(handler)
			t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
			t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
			runtime := defaultRunRuntime()
			runtime.ControlPlaneHTTPClient = server.Client()
			enrollTestHostAgent(t, t.TempDir(), server.URL, runtime)
			server.Close()
			record := hostAgentAssignmentRecord{
				Version: hostAgentAPIVersion, RunID: "20260715T120000.000000000Z", AgentID: "windows-agent-01", HostID: "maya-win-01",
				LockToken: strings.Repeat("a", 32), State: "confirmed", CreatedAt: "2026-07-15T12:00:00Z",
				Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways},
			}
			repoDir := filepath.Join(dataDir, "runs", record.RunID, "repo")
			if err := os.MkdirAll(repoDir, 0o700); err != nil {
				t.Fatalf("create completed run repo: %v", err)
			}
			accepted := newFreshRun(repoDir, runOptions{ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways, AssignedRunID: record.RunID}, defaultRunRuntime()).(*freshRunLifecycle)
			if err := accepted.accept(); err != nil {
				t.Fatalf("accept completed run: %v", err)
			}
			assigned, err := readRunLedgerRecord(repoDir, record.RunID)
			if err != nil {
				t.Fatalf("read completed assigned ledger: %v", err)
			}
			assigned.Host = record.HostID
			record.AssignedLedger = &assigned
			concrete := handler.(*controlPlaneHandler)
			transitionStore := newHostAgentTransitionStore(dataDir)
			if err := transitionStore.Commit(record, concrete.prepareRecoveredHostAgentTransition); err != nil {
				t.Fatalf("persist active transition fixture: %v", err)
			}
			terminalLedger := assigned
			terminalLedger.State = "failed"
			terminalLedger.Status = resultStatusFailed
			completed := record
			completed.State = "completed"
			completed.TerminalLedger = &terminalLedger
			completed.Terminal = &runCommandJSON{Version: 1, Kind: "run", Accepted: true, RunID: record.RunID, Status: resultStatusFailed}
			transactionPath := filepath.Join(dataDir, "assignment-transactions", record.RunID+".json")
			if err := writePrivateJSON(transactionPath, completed); err != nil {
				t.Fatalf("write completed transaction: %v", err)
			}
			if stage == "assignment" || stage == "terminal-ledger" || stage == "lock-removal" {
				if err := transitionStore.SaveAssignment(completed); err != nil {
					t.Fatalf("write partial completed assignment: %v", err)
				}
			}
			if stage == "terminal-ledger" || stage == "lock-removal" {
				if err := newRunLedgerStore(repoDir).Replace(terminalLedger); err != nil {
					t.Fatalf("write partial terminal ledger: %v", err)
				}
			}
			if stage == "lock-removal" {
				if err := os.Remove(concrete.hostLockPath(record.HostID)); err != nil {
					t.Fatalf("remove partial completed Host Lock: %v", err)
				}
			}
			if _, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime()); err != nil {
				t.Fatalf("recover partial completed transition: %v", err)
			}
			if _, err := os.Stat(transactionPath); !os.IsNotExist(err) {
				t.Fatalf("completed transaction remains: %v", err)
			}
			if _, err := os.Stat(concrete.hostLockPath(record.HostID)); !os.IsNotExist(err) {
				t.Fatalf("completed Host Lock remains: %v", err)
			}
			ledger, err := readRunLedgerRecord(repoDir, record.RunID)
			if err != nil || ledger.State != "failed" || ledger.Status != resultStatusFailed {
				t.Fatalf("recovered terminal ledger = %+v err=%v", ledger, err)
			}
		})
	}
}

func TestUnauthorizedWindowsHostAgentCannotRegisterOrMutateState(t *testing.T) {
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{
		"control-plane", "enroll-agent", "--control-plane", server.URL,
		"--agent-id", "windows-agent-01", "--host", "maya-win-01",
		"--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, &stdout, &stderr, t.TempDir(), "test-version", runtime); code != 0 {
		t.Fatalf("enroll Windows Host Agent exit code = %d; stderr: %s", code, stderr.String())
	}

	body, err := json.Marshal(hostAgentRegistrationRequest{
		Version: hostAgentAPIVersion, AgentID: "windows-agent-01", HostID: "maya-win-01", Slots: 1, SessionBinding: true,
		Capabilities: testHostAgentCapabilityRecord("maya-win-01", runtime.Now()),
	})
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/host-agents/windows-agent-01/register", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("create registration request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer wrong-agent-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("send unauthorized registration: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized registration HTTP status = %d, want 401", response.StatusCode)
	}
	status := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "enrolled" || status.RunID != "" {
		t.Fatalf("Host Agent state after unauthorized registration = %+v", status)
	}
	for _, child := range []string{"assignments", "host-locks"} {
		entries, err := os.ReadDir(filepath.Join(dataDir, child))
		if err != nil || len(entries) != 0 {
			t.Fatalf("%s after unauthorized registration = %+v, err %v", child, entries, err)
		}
	}
}

func TestStaleHostLockTokenIsRejectedWithoutMutation(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}

	var runStdout bytes.Buffer
	var runStderr bytes.Buffer
	runDone := make(chan int, 1)
	go func() {
		runDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &runStdout, &runStderr, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "locked")
	var assignment hostAgentAssignmentResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/next", hostAgentNextRequest{Version: hostAgentAPIVersion, SessionID: status.SessionID}, runtime, http.StatusOK, &assignment); err != nil {
		t.Fatalf("read Host Agent assignment: %v", err)
	}
	assignmentPath := filepath.Join(dataDir, "assignments", assignment.RunID+".json")
	before, err := os.ReadFile(assignmentPath)
	if err != nil {
		t.Fatalf("read assignment before stale confirmation: %v", err)
	}
	stale := hostAgentLockRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: strings.Repeat("0", len(assignment.LockToken)), SessionID: status.SessionID}
	content, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal stale Host Lock confirmation: %v", err)
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/host-agents/windows-agent-01/assignments/"+assignment.RunID+"/confirm", bytes.NewReader(content))
	if err != nil {
		t.Fatalf("create stale Host Lock request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+testHostAgentCredential)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("send stale Host Lock confirmation: %v", err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		t.Fatalf("stale Host Lock confirmation HTTP = %d, want 409", response.StatusCode)
	}
	after, err := os.ReadFile(assignmentPath)
	if err != nil {
		t.Fatalf("read assignment after stale confirmation: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("stale Host Lock confirmation mutated assignment\nbefore: %s\nafter: %s", before, after)
	}
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "locked" || status.RunID != assignment.RunID {
		t.Fatalf("Host Agent state after stale confirmation = %+v", status)
	}
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/"+assignment.RunID+"/confirm", hostAgentLockRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: status.SessionID,
	}, runtime, http.StatusOK, &status); err != nil {
		t.Fatalf("confirm Host Agent assignment after stale token: %v", err)
	}
	confirmed, err := os.ReadFile(assignmentPath)
	if err != nil {
		t.Fatalf("read confirmed assignment: %v", err)
	}
	staleSession := strings.Repeat("9", len(status.SessionID))
	staleHeartbeat := hostAgentHeartbeatRequest{Version: hostAgentAPIVersion, SessionID: staleSession, Capabilities: status.Capabilities}
	requests := []struct {
		name       string
		path       string
		body       any
		credential string
		want       int
	}{
		{"heartbeat-wrong-credential", "/v1/host-agents/windows-agent-01/heartbeat", hostAgentNextRequest{Version: hostAgentAPIVersion, SessionID: status.SessionID}, "wrong", http.StatusUnauthorized},
		{"heartbeat-stale-session", "/v1/host-agents/windows-agent-01/heartbeat", staleHeartbeat, testHostAgentCredential, http.StatusConflict},
		{"next-wrong-credential", "/v1/host-agents/windows-agent-01/assignments/next", hostAgentNextRequest{Version: hostAgentAPIVersion, SessionID: status.SessionID}, "wrong", http.StatusUnauthorized},
		{"next-stale-session", "/v1/host-agents/windows-agent-01/assignments/next", hostAgentNextRequest{Version: hostAgentAPIVersion, SessionID: staleSession}, testHostAgentCredential, http.StatusConflict},
		{"bind-wrong-credential", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/session", hostAgentSessionRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: status.SessionID, BrokerSession: brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond", SessionID: "maya-session-01"}}, "wrong", http.StatusUnauthorized},
		{"bind-stale-session", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/session", hostAgentSessionRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: staleSession, BrokerSession: brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond", SessionID: "maya-session-01"}}, testHostAgentCredential, http.StatusConflict},
		{"bind-stale-lock-token", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/session", hostAgentSessionRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: strings.Repeat("0", len(assignment.LockToken)), SessionID: status.SessionID, BrokerSession: brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond", SessionID: "maya-session-01"}}, testHostAgentCredential, http.StatusConflict},
		{"progress-wrong-credential", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/progress", hostAgentProgressRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: status.SessionID}, "wrong", http.StatusUnauthorized},
		{"progress-stale-session", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/progress", hostAgentProgressRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: staleSession}, testHostAgentCredential, http.StatusConflict},
		{"progress-stale-lock-token", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/progress", hostAgentProgressRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: strings.Repeat("0", len(assignment.LockToken)), SessionID: status.SessionID}, testHostAgentCredential, http.StatusConflict},
		{"fail-wrong-credential", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/fail", hostAgentFailureRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: status.SessionID, Diagnostic: "rejected"}, "wrong", http.StatusUnauthorized},
		{"fail-stale-session", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/fail", hostAgentFailureRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: staleSession, Diagnostic: "rejected"}, testHostAgentCredential, http.StatusConflict},
		{"complete-wrong-credential", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/complete", hostAgentCompletionRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: status.SessionID}, "wrong", http.StatusUnauthorized},
		{"complete-stale-session", "/v1/host-agents/windows-agent-01/assignments/" + assignment.RunID + "/complete", hostAgentCompletionRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: staleSession}, testHostAgentCredential, http.StatusConflict},
	}
	for _, test := range requests {
		t.Run(test.name, func(t *testing.T) {
			content, err := json.Marshal(test.body)
			if err != nil {
				t.Fatalf("marshal rejected request: %v", err)
			}
			request, err := http.NewRequest(http.MethodPost, server.URL+test.path, bytes.NewReader(content))
			if err != nil {
				t.Fatalf("create rejected request: %v", err)
			}
			request.Header.Set("Authorization", "Bearer "+test.credential)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("send rejected request: %v", err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != test.want {
				t.Fatalf("rejected request HTTP = %d, want %d", response.StatusCode, test.want)
			}
			after, err := os.ReadFile(assignmentPath)
			if err != nil || !bytes.Equal(after, confirmed) {
				t.Fatalf("rejected request mutated assignment: %v\nbefore: %s\nafter: %s", err, confirmed, after)
			}
		})
	}
	simulateHostAgentProcessExit(t, handler, "windows-agent-01")
	var replacement hostAgentStatusResponse
	err = postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &replacement)
	var registrationErr *controlPlaneHTTPStatusError
	if !errors.As(err, &registrationErr) || registrationErr.StatusCode != http.StatusConflict {
		t.Fatalf("confirmed Agent takeover error = %v, want HTTP 409", err)
	}
	select {
	case code := <-runDone:
		if code != 1 {
			t.Fatalf("quarantined Scenario exit code = %d; stdout: %s stderr: %s", code, runStdout.String(), runStderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Control Plane did not publish quarantined Scenario failure")
	}
	status = readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if status.State != "quarantined" {
		t.Fatalf("automatic quarantine status = %+v", status)
	}
	var result controlPlaneResultResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+assignment.RunID+"/result", runtime, &result); err != nil {
		t.Fatalf("read quarantined result: %v", err)
	}
	if !result.Final || result.Success || result.Status != resultStatusFailed || result.CleanupState != "failed" {
		t.Fatalf("quarantined durable result = %+v", result)
	}
}

func TestSecondAssignmentToSameMayaHostQueuesWithoutMutation(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var registered hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &registered); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}

	var firstStdout bytes.Buffer
	var firstStderr bytes.Buffer
	firstDone := make(chan int, 1)
	go func() {
		firstDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &firstStdout, &firstStderr, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "locked")
	locked := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	lockPath := filepath.Join(dataDir, "host-locks", "maya-win-01.json")
	assignmentPath := filepath.Join(dataDir, "assignments", locked.RunID+".json")
	lockBefore, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read first Host Lock: %v", err)
	}
	assignmentBefore, err := os.ReadFile(assignmentPath)
	if err != nil {
		t.Fatalf("read first assignment: %v", err)
	}

	var secondStdout bytes.Buffer
	var secondStderr bytes.Buffer
	secondDone := make(chan int, 1)
	go func() {
		secondDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, &secondStdout, &secondStderr, repoDir, "test-version", runtime)
	}()
	queuedRunID := waitForQueuedRunID(t, handler.(*controlPlaneHandler))
	lockAfter, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read first Host Lock after contention: %v", err)
	}
	assignmentAfter, err := os.ReadFile(assignmentPath)
	if err != nil {
		t.Fatalf("read first assignment after contention: %v", err)
	}
	if !bytes.Equal(lockBefore, lockAfter) || !bytes.Equal(assignmentBefore, assignmentAfter) {
		t.Fatalf("second assignment mutated the active Host Lock or assignment")
	}
	stillLocked := readHostAgentStatus(t, server.Client(), server.URL, "windows-agent-01")
	if stillLocked.State != "locked" || stillLocked.RunID != locked.RunID {
		t.Fatalf("Host Agent after second assignment = %+v, want first lock unchanged", stillLocked)
	}
	if _, err := handler.(*controlPlaneHandler).cancelQueuedRun(queuedRunID); err != nil {
		t.Fatalf("cancel second queued Run: %v", err)
	}
	select {
	case code := <-secondDone:
		if code != 1 {
			t.Fatalf("canceled second Run exit code = %d; stdout: %s stderr: %s", code, secondStdout.String(), secondStderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled second Run did not finish")
	}
	simulateHostAgentProcessExit(t, handler, "windows-agent-01")

	agentDone := make(chan int, 1)
	go func() {
		agentDone <- RunWithRuntime([]string{
			"host-agent", "run-once", "--control-plane", server.URL,
			"--agent-id", "windows-agent-01", "--host", "maya-win-01",
			"--work-root", agentWorkRoot, "--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
		}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	select {
	case code := <-agentDone:
		if code != 0 {
			t.Fatalf("Windows Host Agent cleanup exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Windows Host Agent did not finish first assignment")
	}
	select {
	case code := <-firstDone:
		if code != 0 {
			t.Fatalf("first Scenario exit code = %d; stdout: %s stderr: %s", code, firstStdout.String(), firstStderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Control Plane did not finish first assignment")
	}
}

func TestCompatibleRunWaitsInDurableQueueWhileHostIsBusy(t *testing.T) {
	repoDir := writeRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, repoDir, server.URL, runtime)
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	var registered hostAgentStatusResponse
	if err := postControlPlaneJSON(server.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, runtime, http.StatusOK, &registered); err != nil {
		t.Fatalf("register Windows Host Agent: %v", err)
	}

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()
	waitForHostAgentState(t, server.Client(), server.URL, "windows-agent-01", "locked")

	secondDone := make(chan int, 1)
	go func() {
		secondDone <- RunWithRuntime([]string{"run", "--json", "--control-plane", server.URL, "smoke"}, io.Discard, io.Discard, repoDir, "test-version", runtime)
	}()

	var queuedRunID string
	queuedObserved := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		concrete := handler.(*controlPlaneHandler)
		concrete.mu.Lock()
		for runID := range concrete.queuedRuns {
			queuedRunID = runID
		}
		concrete.mu.Unlock()
		if queuedRunID != "" {
			var status map[string]any
			if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+queuedRunID+"/status", runtime, &status); err == nil && status["state"] == "queued" {
				if status["queuePosition"] != float64(1) || status["waitReason"] != "compatible-hosts-busy" || status["hostPool"] != "default" {
					t.Fatalf("queued status = %+v", status)
				}
				if host, ok := status["host"]; ok && host != "" {
					t.Fatalf("queued Run acquired Host %v", host)
				}
				if requirements, ok := status["requiredCapabilities"].(map[string]any); !ok || len(requirements) == 0 {
					t.Fatalf("queued requirements = %#v", status["requiredCapabilities"])
				}
				select {
				case code := <-secondDone:
					t.Fatalf("queued Run returned early with exit code %d", code)
				default:
				}
				queuedObserved = true
				break
			}
		}
		if queuedObserved {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !queuedObserved {
		t.Fatalf("compatible Run %q never entered durable queue", queuedRunID)
	}

	simulateHostAgentProcessExit(t, handler, "windows-agent-01")
	for index := 0; index < 2; index++ {
		var agentStderr bytes.Buffer
		if code := RunWithRuntime([]string{
			"host-agent", "run-once", "--control-plane", server.URL,
			"--agent-id", "windows-agent-01", "--host", "maya-win-01",
			"--work-root", agentWorkRoot, "--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
		}, io.Discard, &agentStderr, repoDir, "test-version", runtime); code != 0 {
			t.Fatalf("Windows Host Agent run %d exit code = %d; stderr: %s", index+1, code, agentStderr.String())
		}
	}
	for name, done := range map[string]<-chan int{"first": firstDone, "queued": secondDone} {
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("%s Scenario exit code = %d", name, code)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s Scenario did not finish", name)
		}
	}
}

func TestControlPlaneRestartKeepsDurableHostLockUnavailable(t *testing.T) {
	dataDir := privateTempDir(t)
	handler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("create Control Plane handler: %v", err)
	}
	server := httptest.NewTLSServer(handler)
	t.Setenv(defaultControlPlaneTokenEnv, "operator-token")
	t.Setenv("TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", testHostAgentCredential)
	runtime := defaultRunRuntime()
	runtime.ControlPlaneHTTPClient = server.Client()
	enrollTestHostAgent(t, t.TempDir(), server.URL, runtime)
	server.Close()

	runID := "20260715T120000.000000000Z"
	token := strings.Repeat("a", 32)
	createdAt := "2026-07-15T12:00:00Z"
	assignment := hostAgentAssignmentRecord{
		Version: hostAgentAPIVersion, RunID: runID, AgentID: "windows-agent-01", HostID: "maya-win-01",
		LockToken: token, State: "assigned", CreatedAt: createdAt,
		Submission: controlPlaneSubmission{Version: controlPlaneAPIVersion, Scenario: "smoke", StopAfter: stopAfterAlways},
	}
	lock := hostAgentLockRecord{
		Version: hostAgentAPIVersion, RunID: runID, AgentID: "windows-agent-01", HostID: "maya-win-01",
		LockToken: token, State: "assigned", CreatedAt: createdAt,
	}
	if err := writePrivateJSON(filepath.Join(dataDir, "assignments", runID+".json"), assignment); err != nil {
		t.Fatalf("persist assignment fixture: %v", err)
	}
	if err := writePrivateJSON(filepath.Join(dataDir, "host-locks", "maya-win-01.json"), lock); err != nil {
		t.Fatalf("persist Host Lock fixture: %v", err)
	}

	restartStartedAt := time.Now()
	restartedHandler, err := newControlPlaneHandler(dataDir, "operator-token", defaultRunRuntime())
	if err != nil {
		t.Fatalf("restart Control Plane handler: %v", err)
	}
	sharedLock, err := os.ReadFile(filepath.Join(dataDir, "fake-host", ".maya-stall", "state", "locks", "hosts", "maya-win-01.lock"))
	if err != nil {
		t.Fatalf("read re-established shared Host Lock: %v", err)
	}
	if !bytes.Contains(sharedLock, []byte(fmt.Sprintf("pid: %d", os.Getpid()))) || !bytes.Contains(sharedLock, []byte("activeRun: "+runID)) {
		t.Fatalf("re-established shared Host Lock = %q", sharedLock)
	}
	restartedAgent := restartedHandler.(*controlPlaneHandler).hostAgents["windows-agent-01"]
	minimumGrace := hostAgentHeartbeatInterval + hostAgentHeartbeatRequestTimeout + defaultBrokerCancellationWait
	if restartedAgent.takeoverNotBefore.Before(restartStartedAt.Add(minimumGrace)) {
		t.Fatalf("restart takeover grace ends at %s, want at least %s", restartedAgent.takeoverNotBefore, restartStartedAt.Add(minimumGrace))
	}
	restartedAgent.takeoverNotBefore = time.Time{}
	restarted := httptest.NewTLSServer(restartedHandler)
	t.Cleanup(restarted.Close)
	restartedRuntime := defaultRunRuntime()
	restartedRuntime.ControlPlaneHTTPClient = restarted.Client()
	status := readHostAgentStatus(t, restarted.Client(), restarted.URL, "windows-agent-01")
	if status.State != "offline" || status.RunID != runID {
		t.Fatalf("restarted Host Agent status = %+v, want offline with durable Run ID", status)
	}
	registration := testHostAgentRegistration("windows-agent-01", "maya-win-01", runtime.Now())
	if err := postControlPlaneJSON(restarted.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/register", registration, restartedRuntime, http.StatusOK, &status); err != nil {
		t.Fatalf("register restarted Windows Host Agent: %v", err)
	}
	if status.State != "locked" || status.RunID != runID {
		t.Fatalf("restarted registered Host Agent status = %+v, want durable lock", status)
	}
	var next hostAgentAssignmentResponse
	if err := postControlPlaneJSON(restarted.URL, "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL", "/v1/host-agents/windows-agent-01/assignments/next", hostAgentNextRequest{Version: hostAgentAPIVersion, SessionID: status.SessionID}, restartedRuntime, http.StatusOK, &next); err != nil {
		t.Fatalf("read restarted assignment: %v", err)
	}
	if next.RunID != runID || next.LockToken != token || next.HostID != "maya-win-01" {
		t.Fatalf("restarted assignment = %+v", next)
	}
}

func enrollTestHostAgent(t *testing.T, workDir string, serverURL string, runtime runRuntime) {
	t.Helper()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := RunWithRuntime([]string{
		"control-plane", "enroll-agent", "--control-plane", serverURL,
		"--agent-id", "windows-agent-01", "--host", "maya-win-01",
		"--credential-env", "TEST_MAYA_STALL_HOST_AGENT_CREDENTIAL",
	}, &stdout, &stderr, workDir, "test-version", runtime); code != 0 {
		t.Fatalf("enroll Windows Host Agent exit code = %d; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
}

func testHostAgentCapabilityRecord(hostID string, now time.Time) mayaHostCapabilityRecord {
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: hostID, Health: "healthy"}, now)
	report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
	report.TargetProfiles = []string{"default"}
	report.TargetProfileHostPools = map[string]string{"default": "default"}
	return report
}

func testHostAgentRegistration(agentID string, hostID string, now time.Time) hostAgentRegistrationRequest {
	return hostAgentRegistrationRequest{
		Version: hostAgentAPIVersion, AgentID: agentID, HostID: hostID, Slots: 1, SessionBinding: true,
		Capabilities: testHostAgentCapabilityRecord(hostID, now),
	}
}

func simulateHostAgentProcessExit(t *testing.T, handler http.Handler, agentID string) {
	t.Helper()
	concrete, ok := handler.(*controlPlaneHandler)
	if !ok {
		t.Fatal("Control Plane test handler has unexpected type")
	}
	concrete.mu.Lock()
	defer concrete.mu.Unlock()
	agent := concrete.hostAgents[agentID]
	if agent == nil {
		t.Fatalf("Windows Host Agent %s not found", agentID)
	}
	agent.status.SessionID = ""
}

func TestHostAgentStatusHelpersUseProvidedOperatorToken(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer distinct-operator-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"version":1,"kind":"host-agent-status","agentId":"windows-agent-01","hostId":"maya-win-01","slots":1,"state":"ready"}`)
	}))
	t.Cleanup(server.Close)

	waitForHostAgentStateWithToken(t, server.Client(), server.URL, "distinct-operator-token", "windows-agent-01", "ready")
	status := readHostAgentStatusWithToken(t, server.Client(), server.URL, "distinct-operator-token", "windows-agent-01")
	if status.State != "ready" || requests.Load() != 2 {
		t.Fatalf("status = %+v, authenticated requests = %d", status, requests.Load())
	}
}

func waitForHostAgentState(t *testing.T, client *http.Client, serverURL string, agentID string, want string) {
	t.Helper()
	waitForHostAgentStateWithToken(t, client, serverURL, "operator-token", agentID, want)
}

func waitForHostAgentStateWithToken(t *testing.T, client *http.Client, serverURL string, operatorToken string, agentID string, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		request, err := http.NewRequest(http.MethodGet, serverURL+"/v1/host-agents/"+agentID+"/status", nil)
		if err != nil {
			t.Fatalf("create Host Agent status request: %v", err)
		}
		request.Header.Set("Authorization", "Bearer "+operatorToken)
		response, err := client.Do(request)
		if err == nil && response.StatusCode == http.StatusOK {
			var status struct {
				State string `json:"state"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&status)
			_ = response.Body.Close()
			if decodeErr == nil && status.State == want {
				return
			}
		} else if response != nil {
			_ = response.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("Host Agent %s did not reach state %s", agentID, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readHostAgentStatus(t *testing.T, client *http.Client, serverURL string, agentID string) hostAgentStatusResponse {
	t.Helper()
	return readHostAgentStatusWithToken(t, client, serverURL, "operator-token", agentID)
}

func readHostAgentStatusWithToken(t *testing.T, client *http.Client, serverURL string, operatorToken string, agentID string) hostAgentStatusResponse {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, serverURL+"/v1/host-agents/"+agentID+"/status", nil)
	if err != nil {
		t.Fatalf("create Host Agent status request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+operatorToken)
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("read Host Agent status: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("Host Agent status HTTP = %d, want 200", response.StatusCode)
	}
	var status hostAgentStatusResponse
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decode Host Agent status: %v", err)
	}
	return status
}
