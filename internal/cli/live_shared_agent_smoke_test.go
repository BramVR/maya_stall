package cli

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runOptInRealSharedHostAgentRunSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	options.Host = liveSmokeHostForContention(t, options)
	host := liveSmokeHostConfigByID(t, options, options.Host)
	restoreLiveSessionBrokerFixtures(t, options)

	repoDir := writeLiveRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	t.Setenv(defaultControlPlaneTokenEnv, "shared-live-operator-token")
	t.Setenv("TEST_MAYA_STALL_LIVE_HOST_AGENT_CREDENTIAL", testHostAgentCredential)

	bound := make(chan brokerSessionIdentity, 1)
	continueRun := make(chan struct{})
	runtime := defaultRunRuntime()
	runtime.SessionStarted = func(session brokerSessionIdentity) error {
		bound <- session
		select {
		case <-continueRun:
			return nil
		case <-time.After(30 * time.Second):
			return errors.New("live shared-path test did not release the bound Maya UI Session")
		}
	}
	handlerValue, err := newControlPlaneHandler(dataDir, "shared-live-operator-token", runtime)
	if err != nil {
		t.Fatalf("create live shared Control Plane: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	runtime.ControlPlaneHTTPClient = server.Client()

	var enrollStdout, enrollStderr bytes.Buffer
	if code := RunWithRuntime([]string{
		"control-plane", "enroll-agent", "--control-plane", server.URL,
		"--agent-id", "windows-agent-live", "--host", options.Host,
		"--credential-env", "TEST_MAYA_STALL_LIVE_HOST_AGENT_CREDENTIAL",
	}, &enrollStdout, &enrollStderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("enroll live Windows Host Agent exit code = %d; stdout: %s stderr: %s", code, enrollStdout.String(), enrollStderr.String())
	}

	agentDone := make(chan int, 1)
	var agentStdout, agentStderr bytes.Buffer
	go func() {
		agentDone <- RunWithRuntime([]string{
			"host-agent", "run-once", "--control-plane", server.URL,
			"--agent-id", "windows-agent-live", "--host", options.Host,
			"--work-root", agentWorkRoot, "--host-config", options.HostConfig,
			"--credential-env", "TEST_MAYA_STALL_LIVE_HOST_AGENT_CREDENTIAL",
		}, &agentStdout, &agentStderr, repoDir, "test-version", runtime)
	}()
	waitForHostAgentStateWithToken(t, server.Client(), server.URL, "shared-live-operator-token", "windows-agent-live", "ready")

	runDone := make(chan int, 1)
	var runStdout, runStderr bytes.Buffer
	go func() {
		runDone <- RunWithRuntime([]string{
			"run", "--json", "--control-plane", server.URL,
			"--target-profile", options.TargetProfile, "smoke",
		}, &runStdout, &runStderr, repoDir, "test-version", runtime)
	}()

	var session brokerSessionIdentity
	select {
	case session = <-bound:
	case <-time.After(5 * time.Minute):
		t.Fatalf("live shared run did not bind a Maya UI Session; agent stdout: %s stderr: %s", agentStdout.String(), agentStderr.String())
	}
	status := readHostAgentStatusWithToken(t, server.Client(), server.URL, "shared-live-operator-token", "windows-agent-live")
	if status.State != "running" || status.RunID == "" {
		t.Fatalf("live Host Agent status at session binding = %+v", status)
	}
	var sharedLock hostAgentLockRecord
	if err := readPrivateJSON(handler.hostLockPath(options.Host), &sharedLock); err != nil {
		t.Fatalf("read live shared Host Lock: %v", err)
	}
	if sharedLock.RunID != status.RunID || sharedLock.HostID != options.Host || sharedLock.LockToken == "" || sharedLock.BrokerSession == nil || *sharedLock.BrokerSession != session {
		t.Fatalf("live shared Host Lock binding = %+v, session = %+v", sharedLock, session)
	}
	close(continueRun)

	select {
	case code := <-agentDone:
		if code != 0 {
			t.Fatalf("live Windows Host Agent exit code = %d; stdout: %s stderr: %s", code, agentStdout.String(), agentStderr.String())
		}
	case <-time.After(10 * time.Minute):
		t.Fatal("live Windows Host Agent did not complete")
	}
	select {
	case code := <-runDone:
		if code != 0 {
			t.Fatalf("live shared Scenario exit code = %d; stdout: %s stderr: %s", code, runStdout.String(), runStderr.String())
		}
	case <-time.After(time.Minute):
		t.Fatal("live shared Control Plane submission did not complete")
	}

	results := decodeRunJSONLines(t, runStdout.Bytes())
	if len(results) != 2 || results[0].Kind != "run-accepted" || results[1].Status != resultStatusPassed || results[1].Host != options.Host || results[1].TargetProfile != options.TargetProfile {
		t.Fatalf("live shared Scenario output = %+v", results)
	}
	runID := results[0].RunID
	serverRepo := filepath.Join(dataDir, "runs", runID, "repo")
	evidenceDir := filepath.Join(serverRepo, "artifacts", "maya-stall", runID)
	bundle := assertLiveSmokeEvidenceBundle(t, evidenceDir)
	if bundle.BrokerSession == nil || *bundle.BrokerSession != session || bundle.Runtime.Profile != "ssh-sessiond" || !bundle.Runtime.LiveProofEligible {
		t.Fatalf("live shared Evidence runtime/session = %+v / %+v", bundle.Runtime, bundle.BrokerSession)
	}
	if bundle.TargetProfile != options.TargetProfile || bundle.Host != options.Host {
		t.Fatalf("live shared Evidence target = %q/%q", bundle.TargetProfile, bundle.Host)
	}
	assertLiveFreshRunSessionStopped(t, host, bundle.BrokerSession)
	assertLiveSharedRemoteWorkspaceRemoved(t, host, runID)

	if _, err := os.Lstat(filepath.Join(agentWorkRoot, "runs", runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live Agent run workspace residue: %v", err)
	}
	if _, err := os.Lstat(handler.hostLockPath(options.Host)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live shared Host Lock residue: %v", err)
	}
	if layer := remoteHostLockLayer(host); layer.Status != "ok" || layer.State != "unlocked" {
		t.Fatalf("live Maya Host Lock residue: %+v", layer)
	}
	for _, path := range []string{
		filepath.Join(repoDir, ".maya-stall", "state", "ledger", "runs", runID),
		filepath.Join(repoDir, "artifacts", "maya-stall", runID),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("submitting checkout owned shared run state %s: %v", path, err)
		}
	}
	var completed hostAgentAssignmentRecord
	if err := readPrivateJSON(filepath.Join(dataDir, "assignments", runID+".json"), &completed); err != nil {
		t.Fatalf("read completed live assignment: %v", err)
	}
	if completed.State != "completed" || !sameBrokerSession(completed.BrokerSession, bundle.BrokerSession) {
		t.Fatalf("completed live assignment = %+v", completed)
	}
	waitForHostAgentStateWithToken(t, server.Client(), server.URL, "shared-live-operator-token", "windows-agent-live", "offline")
	t.Log("Control Plane assigned the registered Agent; shared Host Lock bound the exact broker session; real Maya evidence transferred; broker inactive; Agent and remote workspaces removed before lock release")
}

func runOptInRealSharedKeptSessionDeadlineSmoke(t *testing.T) {
	options, ok := realSSHSmokeOptionsFromEnv(t)
	if !ok {
		return
	}
	options.Host = liveSmokeHostForContention(t, options)
	host := liveSmokeHostConfigByID(t, options, options.Host)
	restoreLiveSessionBrokerFixtures(t, options)

	repoDir := writeLiveRunConfigFixture(t)
	dataDir := privateTempDir(t)
	agentWorkRoot := privateTempDir(t)
	t.Setenv(defaultControlPlaneTokenEnv, "kept-live-operator-token")
	t.Setenv("TEST_MAYA_STALL_LIVE_KEPT_AGENT_CREDENTIAL", testHostAgentCredential)
	start := time.Now().UTC()
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	handlerValue, err := newControlPlaneHandlerWithPolicy(dataDir, "kept-live-operator-token", runtime, hostLockDeadlinePolicy{
		IdleTimeout: 30 * time.Minute, HardLifetime: 3 * time.Hour,
	})
	if err != nil {
		t.Fatalf("create live Kept Session Control Plane: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	server := httptest.NewTLSServer(handler)
	t.Cleanup(func() { closeHostAgentTestServer(server, handler) })
	runtime.ControlPlaneHTTPClient = server.Client()

	var enrollStdout, enrollStderr bytes.Buffer
	if code := RunWithRuntime([]string{
		"control-plane", "enroll-agent", "--control-plane", server.URL,
		"--agent-id", "windows-agent-kept-live", "--host", options.Host,
		"--credential-env", "TEST_MAYA_STALL_LIVE_KEPT_AGENT_CREDENTIAL",
	}, &enrollStdout, &enrollStderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("enroll live kept Agent exit code = %d; stdout: %s stderr: %s", code, enrollStdout.String(), enrollStderr.String())
	}

	agentDone := make(chan int, 1)
	var agentStdout, agentStderr bytes.Buffer
	go func() {
		agentDone <- RunWithRuntime([]string{
			"host-agent", "run-once", "--control-plane", server.URL,
			"--agent-id", "windows-agent-kept-live", "--host", options.Host,
			"--work-root", agentWorkRoot, "--host-config", options.HostConfig,
			"--credential-env", "TEST_MAYA_STALL_LIVE_KEPT_AGENT_CREDENTIAL",
		}, &agentStdout, &agentStderr, repoDir, "test-version", runtime)
	}()
	waitForHostAgentStateWithToken(t, server.Client(), server.URL, "kept-live-operator-token", "windows-agent-kept-live", "ready")

	runDone := make(chan int, 1)
	var runStdout, runStderr bytes.Buffer
	go func() {
		runDone <- RunWithRuntime([]string{
			"run", "--json", "--control-plane", server.URL, "--target-profile", options.TargetProfile,
			"--keep-ttl", "1h", "--stop-after", "never", "smoke",
		}, &runStdout, &runStderr, repoDir, "test-version", runtime)
	}()
	waitForLiveHostAgentStateWithToken(t, server.Client(), server.URL, "kept-live-operator-token", "windows-agent-kept-live", "kept", 10*time.Minute)
	status := readHostAgentStatusWithToken(t, server.Client(), server.URL, "kept-live-operator-token", "windows-agent-kept-live")
	if status.RunID == "" {
		t.Fatal("live Kept Session has no Run ID")
	}
	runID := status.RunID
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	var session *brokerSessionIdentity
	var initialDeadline string
	if assignment != nil {
		session = assignment.record.BrokerSession
		initialDeadline = assignment.record.KeepDeadline
	}
	handler.mu.Unlock()
	if assignment == nil {
		t.Fatalf("live Kept Session %s has no durable assignment", runID)
	}
	if session == nil || session.SessionID == "" || initialDeadline == "" {
		t.Fatalf("live Kept Session identity/deadline = %+v / %q", session, initialDeadline)
	}

	var extendStdout, extendStderr bytes.Buffer
	if code := RunWithRuntime([]string{
		"extend", "--control-plane", server.URL, "--by", "30m", runID,
	}, &extendStdout, &extendStderr, repoDir, "test-version", runtime); code != 0 {
		t.Fatalf("extend live Kept Session exit code = %d; stdout: %s stderr: %s", code, extendStdout.String(), extendStderr.String())
	}
	handler.mu.Lock()
	extendedDeadline := handler.assignments[runID].record.KeepDeadline
	extensionCount := handler.assignments[runID].record.ExtensionCount
	handler.mu.Unlock()
	initial, err := time.Parse(time.RFC3339Nano, initialDeadline)
	if err != nil || extendedDeadline != initial.Add(30*time.Minute).Format(time.RFC3339Nano) || extensionCount != 1 {
		t.Fatalf("live Kept Session extension = deadline %q count %d, initial %q err %v", extendedDeadline, extensionCount, initialDeadline, err)
	}

	deadline, err := time.Parse(time.RFC3339Nano, extendedDeadline)
	if err != nil {
		t.Fatalf("parse live extended deadline: %v", err)
	}
	advanceHostAgentTestClock(t, handler, &clock, "windows-agent-kept-live", runID, deadline)
	var expired controlPlaneStatusResponse
	if err := getControlPlaneJSON(server.URL, "", "/v1/runs/"+runID+"/status", runtime, &expired); err != nil {
		t.Fatalf("observe live Kept Session expiry: %v", err)
	}
	if expired.State != "expiring" || expired.ExpiryReason != "kept-session" {
		t.Fatalf("live expired Kept Session status = %+v", expired)
	}

	select {
	case code := <-agentDone:
		if code != 0 {
			t.Fatalf("live kept Agent exit code = %d; stdout: %s stderr: %s", code, agentStdout.String(), agentStderr.String())
		}
	case <-time.After(10 * time.Minute):
		t.Fatalf("live kept Agent did not clean expired session; stdout: %s stderr: %s", agentStdout.String(), agentStderr.String())
	}
	select {
	case code := <-runDone:
		if code != 0 {
			t.Fatalf("live kept Run exit code = %d; stdout: %s stderr: %s", code, runStdout.String(), runStderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatal("live kept Control Plane submission did not finish")
	}
	assertLiveFreshRunSessionStopped(t, host, session)
	assertLiveSharedRemoteWorkspaceRemoved(t, host, runID)
	if _, err := os.Lstat(filepath.Join(agentWorkRoot, "runs", runID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live kept Agent workspace residue: %v", err)
	}
	if _, err := os.Lstat(handler.hostLockPath(options.Host)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live kept shared Host Lock residue: %v", err)
	}
	var completed hostAgentAssignmentRecord
	if err := readPrivateJSON(filepath.Join(dataDir, "assignments", runID+".json"), &completed); err != nil {
		t.Fatalf("read completed live kept assignment: %v", err)
	}
	eventTypes := make(map[string]bool, len(completed.DeadlineEvents))
	for _, event := range completed.DeadlineEvents {
		eventTypes[event.Type] = true
	}
	for _, want := range []string{"kept-session.started", "kept-session.extended", "host-lock.expired"} {
		if !eventTypes[want] {
			t.Fatalf("live kept assignment missing %s: %+v", want, completed.DeadlineEvents)
		}
	}
	t.Log("real shared Kept Session extended within policy; controllable deadline expiry stopped the exact bound Maya UI Session; run-owned state and Host Lock were released")
}

func waitForLiveHostAgentStateWithToken(t *testing.T, client *http.Client, serverURL string, operatorToken string, agentID string, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		status := readHostAgentStatusWithToken(t, client, serverURL, operatorToken, agentID)
		if status.State == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("live Host Agent %s did not reach state %s; last status: %+v", agentID, want, status)
		}
		time.Sleep(time.Second)
	}
}

func assertLiveSharedRemoteWorkspaceRemoved(t *testing.T, host mayaHostConfig, runID string) {
	t.Helper()
	path := remoteJoin(host.WorkRoot, "runs", runID)
	script := "$path = " + powerShellSingleQuoted(path) + "; if (Test-Path -LiteralPath $path) { Write-Output 'present' } else { Write-Output 'absent' }"
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sshCommandTimeout)
	if err != nil {
		t.Fatalf("verify live shared remote workspace cleanup: %v", err)
	}
	if strings.TrimSpace(string(raw)) != "absent" {
		t.Fatalf("live shared remote workspace %s remains: %s", path, raw)
	}
}
