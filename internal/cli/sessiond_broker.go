package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	sessiondScenarioTimeout       = 10 * time.Minute
	sessiondCommandTimeout        = 2 * time.Minute
	sessiondSessionStartTimeout   = 5 * time.Minute
	sessiondSessionPollInterval   = 5 * time.Second
	sessiondRecoveryTimeout       = 3 * time.Minute
	defaultSessiondRecoveryTask   = "MayaStallSessiondUI"
	sessiondReasonCommandPortDown = "command-port-unreachable"
)

var errSSHCommandTimedOut = errors.New("ssh command timed out")

type ggMayaSessiondBroker struct {
	host mayaHostConfig
}

type sessiondCommandResult struct {
	OK    bool   `json:"ok"`
	Tool  string `json:"tool"`
	Error string `json:"error"`
}

type sessiondCaptureResult struct {
	OK      bool   `json:"ok"`
	Tool    string `json:"tool"`
	Error   string `json:"error"`
	Content []struct {
		Type     string `json:"type"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	} `json:"content"`
	Output struct {
		MimeType string `json:"mime_type"`
		Format   string `json:"format"`
	} `json:"output"`
}

type sessiondStatusResult struct {
	StateDir      string `json:"state_dir"`
	HasState      bool   `json:"has_state"`
	DerivedStatus string `json:"derived_status"`
	State         struct {
		Status          string `json:"status"`
		SessionID       string `json:"session_id"`
		DaemonPID       int    `json:"daemon_pid"`
		MayaPID         int    `json:"maya_pid"`
		MCPPID          int    `json:"mcp_pid"`
		MayaAlive       bool   `json:"maya_alive"`
		MCPAlive        bool   `json:"mcp_alive"`
		CallServerReady bool   `json:"call_server_ready"`
		DaemonLog       string `json:"daemon_log"`
		HeartbeatAt     string `json:"heartbeat_at"`
	} `json:"state"`
	ProcessAlive map[string]bool `json:"process_alive"`
}

type sessiondHealthResult struct {
	OK                 bool
	Reason             string
	Detail             string
	Hint               string
	Recoverable        bool
	Recovered          bool
	InteractiveDesktop bool
}

func sessionBrokerForConfig(host mayaHostConfig) sessionBroker {
	if host.Broker.isGGMayaSessiond() {
		return ggMayaSessiondBroker{host: host}
	}
	if reason := host.Broker.invalidReason(); reason != "" {
		return invalidSessionBroker{err: fmt.Errorf("%s", reason)}
	}
	return fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "fake Scenario completed"}}
}

func (broker ggMayaSessiondBroker) StartFreshSession(context runContext, scenario scenarioConfig) (brokerSessionIdentity, error) {
	if err := broker.validate(); err != nil {
		return brokerSessionIdentity{}, err
	}
	status, err := broker.status()
	if err != nil {
		return brokerSessionIdentity{}, fmt.Errorf("inspect inherited gg_mayasessiond Maya UI Session: %w", err)
	}
	previousSessionID := status.State.SessionID
	if sessiondSessionLooksActive(status) {
		if err := broker.stopSessiondSession(); err != nil {
			return brokerSessionIdentity{}, fmt.Errorf("stop inherited gg_mayasessiond Maya UI Session: %w", err)
		}
	}
	started := brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond"}
	if err := broker.restartSessionBrokerTask("fresh-run"); err != nil {
		return started, fmt.Errorf("restart gg_mayasessiond for a fresh Maya UI Session: %w", err)
	}
	identity, err := broker.awaitFreshSession(previousSessionID)
	if err != nil {
		if identity.BrokerAdapter == "" {
			identity = started
		}
		return identity, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.fresh", identity.SessionID); err != nil {
		return identity, err
	}
	return identity, nil
}

func (broker ggMayaSessiondBroker) VerifyMayaBuild(context runContext, session brokerSessionIdentity, expected string) error {
	status, err := broker.status()
	if err != nil {
		return fmt.Errorf("inspect fresh Maya UI Session before build verification: %w", err)
	}
	if session.SessionID == "" || status.State.SessionID != session.SessionID {
		return fmt.Errorf("fresh Maya UI Session changed before build verification")
	}
	probePath := remoteJoin(context.RunWorkspace.RemoteWorkspace(), ".maya-stall-maya-build.py")
	resultPath := remoteJoin(context.RunWorkspace.RemoteWorkspace(), ".maya-stall-maya-build.txt")
	if err := broker.stageRemoteFile(probePath, []byte(mayaBuildProbeScript(resultPath))); err != nil {
		return fmt.Errorf("stage Maya build verification: %w", err)
	}
	result, err := broker.callTool("script.execute", []string{"file_path=" + probePath, "timeout=30"}, sessiondCommandTimeout)
	if err != nil {
		return fmt.Errorf("verify selected Maya build %s: %w", expected, err)
	}
	if !result.OK {
		return fmt.Errorf("verify selected Maya build %s: gg_mayasessiond script.execute failed: %s", expected, result.Error)
	}
	tempFile, err := os.CreateTemp("", "maya-stall-maya-build-*")
	if err != nil {
		return err
	}
	localPath := tempFile.Name()
	if err := tempFile.Close(); err != nil {
		_ = os.Remove(localPath)
		return err
	}
	defer func() { _ = os.Remove(localPath) }()
	batch := newSFTPBatch()
	batch.get(resultPath, localPath, false)
	if err := runSFTPBatch(broker.host, batch.String()); err != nil {
		return fmt.Errorf("download Maya build verification: %w", err)
	}
	content, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	actual := strings.TrimSpace(string(content))
	if !sameMayaBuildVersion(actual, expected) {
		return fmt.Errorf("selected Maya build %s but fresh Maya UI Session reports %s", expected, actual)
	}
	return appendEvent(context.EventsPath, "maya.build.verified", actual)
}

func mayaBuildProbeScript(resultPath string) string {
	return "from pathlib import Path\nfrom maya import cmds\nparts = [str(cmds.about(majorVersion=True)), str(cmds.about(minorVersion=True)), str(cmds.about(patchVersion=True))]\nwhile len(parts) > 1 and parts[-1] == '0':\n    parts.pop()\nPath(" + pythonString(resultPath) + ").write_text('.'.join(parts), encoding='utf-8')\n"
}

func (broker ggMayaSessiondBroker) StopSession(context runContext, session brokerSessionIdentity) error {
	if err := broker.validate(); err != nil {
		return err
	}
	status, err := broker.status()
	if err != nil {
		return err
	}
	if session.SessionID == "" {
		return fmt.Errorf("refusing to stop gg_mayasessiond without this Fresh Run's session id")
	}
	if status.State.SessionID == "" {
		return fmt.Errorf("gg_mayasessiond did not report a session id; refusing to stop a session this run cannot prove it owns")
	}
	if status.State.SessionID != session.SessionID {
		return fmt.Errorf("gg_mayasessiond session id changed from %s to %s since this Fresh Run started; refusing to stop a session this run does not own", session.SessionID, status.State.SessionID)
	}
	if !sessiondSessionLooksActive(status) {
		return appendEvent(context.EventsPath, "broker.session.already-stopped", session.SessionID)
	}
	if err := broker.stopSessiondSession(); err != nil {
		return err
	}
	return appendEvent(context.EventsPath, "broker.session.stopped", session.SessionID)
}

func sessiondSessionLooksActive(status sessiondStatusResult) bool {
	derived := sessiondEffectiveStatus(status)
	return status.State.MayaAlive || status.State.MCPAlive || (status.HasState && derived != "" && !strings.EqualFold(derived, "stopped"))
}

func sessiondEffectiveStatus(status sessiondStatusResult) string {
	if status.DerivedStatus != "" {
		return status.DerivedStatus
	}
	return status.State.Status
}

func sessiondFreshSessionReady(status sessiondStatusResult) bool {
	return strings.EqualFold(sessiondEffectiveStatus(status), "running") && status.State.CallServerReady && status.State.SessionID != ""
}

func (broker ggMayaSessiondBroker) awaitFreshSession(previousSessionID string) (brokerSessionIdentity, error) {
	deadline := time.Now().Add(sessiondSessionStartTimeout)
	var lastDetail string
	identity := brokerSessionIdentity{BrokerAdapter: "gg-mayasessiond"}
	for {
		status, err := broker.status()
		if err == nil && status.State.SessionID != "" {
			identity.SessionID = status.State.SessionID
		}
		switch {
		case err != nil:
			lastDetail = err.Error()
		case !strings.EqualFold(sessiondEffectiveStatus(status), "running"):
			lastDetail = fmt.Sprintf("gg_mayasessiond session status is %q, not running", sessiondEffectiveStatus(status))
		case !status.State.CallServerReady:
			lastDetail = "gg_mayasessiond call server is not ready yet"
		case status.State.SessionID == "":
			lastDetail = "gg_mayasessiond did not report a broker session id"
		case previousSessionID != "" && status.State.SessionID == previousSessionID:
			lastDetail = fmt.Sprintf("gg_mayasessiond still reports inherited session %s", previousSessionID)
		case sessiondFreshSessionReady(status):
			return identity, nil
		}
		if !time.Now().Before(deadline) {
			return identity, fmt.Errorf("fresh gg_mayasessiond Maya UI Session was not ready within %s: %s", sessiondSessionStartTimeout, lastDetail)
		}
		time.Sleep(sessiondSessionPollInterval)
	}
}

func (broker ggMayaSessiondBroker) stopSessiondSession() error {
	raw, err := broker.runSessiondCLI([]string{"stop", "--state-dir", broker.host.Broker.StateDir, "--wait-timeout-seconds", "120", "--json"}, sessiondCommandTimeout+2*time.Minute)
	if err != nil {
		return err
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("parse gg_mayasessiond stop JSON: %w", err)
	}
	ok, found := result["ok"].(bool)
	if !found || !ok {
		if message, _ := result["error"].(string); message != "" {
			return fmt.Errorf("gg_mayasessiond stop failed: %s", message)
		}
		return fmt.Errorf("gg_mayasessiond stop failed")
	}
	return nil
}

func (broker ggMayaSessiondBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := broker.validate(); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	if err := os.WriteFile(context.LogPath, []byte("gg_mayasessiond Session Broker ran Scenario\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	wrapperPath := context.RunWorkspace.RemoteScenarioWrapperPath()
	wrapper, err := broker.scenarioWrapper(context, scenario)
	if err != nil {
		return ScenarioResult{}, err
	}
	if err := broker.stageRemoteFile(wrapperPath, []byte(wrapper)); err != nil {
		return ScenarioResult{}, fmt.Errorf("stage gg_mayasessiond Scenario wrapper: %w", err)
	}
	result, err := broker.callTool("script.execute", []string{
		"file_path=" + wrapperPath,
		"timeout=" + strconv.Itoa(int(sessiondScenarioTimeout/time.Second)),
	}, sessiondScenarioTimeout+sshCommandTimeout)
	if err != nil {
		return ScenarioResult{}, err
	}
	if !result.OK {
		return ScenarioResult{}, fmt.Errorf("gg_mayasessiond script.execute failed: %s", result.Error)
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", "completed"); err != nil {
		return ScenarioResult{}, err
	}
	return ScenarioResult{Status: resultStatusPassed, Summary: "gg_mayasessiond Scenario completed"}, nil
}

func (broker ggMayaSessiondBroker) CaptureScreenshot(context runContext, request screenshotRequest) (visualEvidenceArtifact, error) {
	if err := broker.validate(); err != nil {
		return visualEvidenceArtifact{}, err
	}
	name := filepath.Base(filepath.ToSlash(request.Name))
	if name == "" || name == "." || name == ".." {
		name = evidenceDefaultScreenshotName
	}
	name = forceVisualEvidenceExtension(name, ".png")
	if err := appendVisualEvidenceCaptureRequested(context, "screenshot", visualEvidenceOriginBrokerCapture, name); err != nil {
		return visualEvidenceArtifact{}, err
	}
	remoteRoot := broker.remoteVisualEvidenceRoot(context, "screenshot")
	defer func() {
		_ = broker.removeRemotePath(remoteRoot)
	}()
	data, err := captureWindowsDesktopScreenshot(sshWindowsDesktopTransport(broker.host), remoteRoot)
	if err != nil {
		return visualEvidenceArtifact{}, err
	}
	return registerVisualEvidenceBytes(context, "screenshot", visualEvidenceOriginBrokerCapture, name, "image/png", data)
}

func (broker ggMayaSessiondBroker) CaptureRecording(context runContext, request recordingRequest) (visualEvidenceArtifact, error) {
	if err := broker.validate(); err != nil {
		return visualEvidenceArtifact{}, err
	}
	name := filepath.Base(filepath.ToSlash(request.Name))
	if name == "" || name == "." || name == ".." {
		name = evidenceDefaultRecordingName
	}
	name = forceVisualEvidenceExtension(name, ".mp4")
	if err := appendVisualEvidenceCaptureRequested(context, "recording", visualEvidenceOriginBrokerCapture, name); err != nil {
		return visualEvidenceArtifact{}, err
	}
	remoteRoot := broker.remoteVisualEvidenceRoot(context, "recording")
	defer func() {
		_ = broker.removeRemotePath(remoteRoot)
	}()
	data, err := captureWindowsDesktopRecording(sshWindowsDesktopTransport(broker.host), remoteRoot, request.Duration, request.FPS, "")
	if err != nil {
		return visualEvidenceArtifact{}, err
	}
	artifact, err := registerVisualEvidenceBytes(context, "recording", visualEvidenceOriginBrokerCapture, name, "video/mp4", data)
	if err != nil {
		return visualEvidenceArtifact{}, err
	}
	artifact.DurationSeconds = request.Duration.Seconds()
	artifact.FPS = request.FPS
	return artifact, nil
}

func (broker ggMayaSessiondBroker) ClickDesktop(request desktopClickRequest) error {
	if err := broker.validate(); err != nil {
		return err
	}
	remoteRoot := strings.TrimSpace(request.RemoteRoot)
	if remoteRoot == "" {
		return fmt.Errorf("desktop click requires remote root")
	}
	defer func() {
		_ = broker.removeRemotePath(remoteRoot)
	}()
	return clickWindowsDesktop(sshWindowsDesktopTransport(broker.host), remoteRoot, request.X, request.Y)
}

func (broker ggMayaSessiondBroker) RetentionCapabilities() brokerCapabilities {
	return brokerCapabilities{
		RetainOnFailure:          true,
		StatusRetainedSession:    true,
		AttachLogObservation:     true,
		StopRetainedSession:      true,
		CleanupRetainedWorkspace: true,
	}
}

func (broker ggMayaSessiondBroker) RetainRun(context runContext, manifest runManifest, reason string) (retainedSessionRecord, error) {
	if manifest.BrokerSession == nil || manifest.BrokerSession.SessionID == "" {
		return retainedSessionRecord{}, fmt.Errorf("cannot retain run %s without its broker session identity", manifest.RunID)
	}
	status, err := broker.status()
	if err != nil {
		return retainedSessionRecord{}, err
	}
	if status.DerivedStatus == "" {
		status.DerivedStatus = status.State.Status
	}
	if status.State.SessionID != manifest.BrokerSession.SessionID {
		return retainedSessionRecord{}, fmt.Errorf("gg_mayasessiond session id changed from %s to %s before retention; refusing to retain a session this run does not own", manifest.BrokerSession.SessionID, status.State.SessionID)
	}
	if !sessiondSessionLooksActive(status) {
		return retainedSessionRecord{}, fmt.Errorf("gg_mayasessiond session %s is not active; refusing to report it as retained", manifest.BrokerSession.SessionID)
	}
	return retainedSessionRecord{
		BrokerAdapter: manifest.BrokerSession.BrokerAdapter,
		SessionID:     manifest.BrokerSession.SessionID,
		Status:        status.DerivedStatus,
		Metadata: map[string]any{
			"reason":             reason,
			"remoteRunRoot":      context.RunWorkspace.RemoteRunRoot(),
			"remoteWorkspace":    context.RunWorkspace.RemoteWorkspace(),
			"daemonPid":          status.State.DaemonPID,
			"mayaPid":            status.State.MayaPID,
			"mcpPid":             status.State.MCPPID,
			"callServerReady":    status.State.CallServerReady,
			"heartbeatAt":        status.State.HeartbeatAt,
			"sessiondStateDir":   status.StateDir,
			"sessiondDaemonLog":  status.State.DaemonLog,
			"sessiondHasState":   status.HasState,
			"sessiondMayaAlive":  status.State.MayaAlive,
			"sessiondMCPAlive":   status.State.MCPAlive,
			"sessiondStateValue": status.State.Status,
		},
	}, nil
}

func (broker ggMayaSessiondBroker) StatusRetainedRun(record runRetentionRecord) (retainedRunStatus, error) {
	status, err := broker.status()
	if err != nil {
		return retainedRunStatus{}, err
	}
	derived := status.DerivedStatus
	if derived == "" {
		derived = status.State.Status
	}
	result := retainedRunStatus{
		State:           "kept",
		BrokerStatus:    derived,
		SessionID:       status.State.SessionID,
		RemoteWorkspace: record.RemoteWorkspace,
		Detail:          "gg_mayasessiond reports retained session alive",
	}
	if record.RemoteSession.SessionID == "" {
		result.State = "stale"
		result.Detail = "retained run record has no broker session id; run state is incomplete"
		return result, nil
	}
	if status.State.SessionID == "" {
		result.State = "stale"
		result.Detail = "gg_mayasessiond did not report a broker session id; retained session cannot be verified"
		return result, nil
	}
	if !status.HasState || derived == "" || strings.EqualFold(derived, "stopped") {
		result.State = "stale"
		result.Detail = "gg_mayasessiond no longer reports this retained session; run state is stale and may need cleanup"
		return result, nil
	}
	if record.RemoteSession.SessionID != "" && status.State.SessionID != "" && record.RemoteSession.SessionID != status.State.SessionID {
		result.State = "stale"
		result.Detail = "gg_mayasessiond session id changed since this run was retained; run state is orphaned"
	}
	return result, nil
}

func (broker ggMayaSessiondBroker) AttachRetainedRun(record runRetentionRecord, stdout io.Writer) error {
	status, err := broker.StatusRetainedRun(record)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "broker:")
	fmt.Fprintln(stdout, "adapter: gg-mayasessiond")
	fmt.Fprintf(stdout, "session: %s\n", status.SessionID)
	fmt.Fprintf(stdout, "remoteWorkspace: %s\n", record.RemoteWorkspace)
	fmt.Fprintf(stdout, "remoteState: %s\n", status.BrokerStatus)
	report, err := broker.runSessiondCLI([]string{"report", "--state-dir", broker.host.Broker.StateDir, "--limit", "40", "--json"}, sessiondCommandTimeout)
	if err != nil {
		return err
	}
	fmt.Fprintln(stdout, "brokerReport:")
	_, err = stdout.Write(append(bytes.TrimSpace(report), '\n'))
	return err
}

func (broker ggMayaSessiondBroker) StopRetainedRun(record runRetentionRecord) error {
	remoteRunRoot, err := retainedRemoteRunRoot(record)
	if err != nil {
		return err
	}
	status, err := broker.StatusRetainedRun(record)
	if err != nil {
		return err
	}
	if status.State != "kept" {
		if strings.TrimSpace(remoteRunRoot) == "" {
			return fmt.Errorf("refusing to stop retained run %s because broker state is %s and no remote workspace cleanup path is recorded: %s", record.RunID, status.State, status.Detail)
		}
	} else if err := broker.stopSessiondSession(); err != nil {
		return err
	}
	if strings.TrimSpace(remoteRunRoot) != "" {
		if err := broker.removeRemotePath(remoteRunRoot); err != nil {
			return fmt.Errorf("cleanup retained remote run workspace: %w", err)
		}
	}
	return nil
}

func (broker ggMayaSessiondBroker) CleanupRun(context runContext) error {
	return broker.removeRemotePath(context.RunWorkspace.RemoteRunRoot())
}

func retainedRemoteRunRoot(record runRetentionRecord) (string, error) {
	if err := validateRunID(record.RunID); err != nil {
		return "", err
	}
	workRoot := strings.TrimSpace(record.HostConfig.WorkRoot)
	if workRoot == "" {
		return "", fmt.Errorf("retained run %s is missing host workRoot for remote cleanup", record.RunID)
	}
	expected := remoteJoin(remotePath(workRoot), "runs", record.RunID)
	if recorded := strings.TrimSpace(record.RemoteRunRoot); recorded != "" && recorded != expected {
		return "", fmt.Errorf("retained run %s remote cleanup path %q does not match expected %q", record.RunID, recorded, expected)
	}
	return expected, nil
}

func (broker ggMayaSessiondBroker) remoteVisualEvidenceRoot(context runContext, kind string) string {
	return remoteJoin(context.RunWorkspace.RemoteRunRoot(), "visual-evidence", kind)
}

func (broker ggMayaSessiondBroker) validate() error {
	if !broker.host.usesRealSSH() {
		return fmt.Errorf("gg_mayasessiond broker requires transport: ssh")
	}
	if err := validateRealSSHConnection(broker.host); err != nil {
		return err
	}
	if strings.TrimSpace(broker.host.WorkRoot) == "" {
		return fmt.Errorf("gg_mayasessiond broker requires workRoot")
	}
	if strings.TrimSpace(broker.host.Broker.StateDir) == "" {
		return fmt.Errorf("gg_mayasessiond broker requires broker.stateDir")
	}
	if err := validateSessionBrokerStateDir(broker.host.Broker.StateDir); err != nil {
		return err
	}
	if strings.TrimSpace(broker.host.Broker.Python) == "" {
		return fmt.Errorf("gg_mayasessiond broker requires broker.python")
	}
	if strings.TrimSpace(broker.host.Broker.Repo) == "" {
		return fmt.Errorf("gg_mayasessiond broker requires broker.repo")
	}
	if err := validateSessionBrokerRepo(broker.host.Broker.Repo); err != nil {
		return err
	}
	return nil
}

func (broker ggMayaSessiondBroker) status() (sessiondStatusResult, error) {
	return broker.statusWithTimeout(sessiondCommandTimeout)
}

func (broker ggMayaSessiondBroker) ProbeSessionBroker(timeout time.Duration) error {
	status, err := broker.preRunProbeStatus(timeout)
	if err != nil {
		return err
	}
	if !status.HasState {
		return fmt.Errorf("gg_mayasessiond is not running: no canonical state")
	}
	derivedStatus := strings.TrimSpace(status.DerivedStatus)
	stateStatus := strings.TrimSpace(status.State.Status)
	if derivedStatus == "" || stateStatus == "" || !strings.EqualFold(derivedStatus, stateStatus) {
		return fmt.Errorf("gg_mayasessiond is not running: inconsistent status")
	}
	if !strings.EqualFold(derivedStatus, "running") && !strings.EqualFold(derivedStatus, "stopped") {
		return fmt.Errorf("gg_mayasessiond is not running")
	}
	return nil
}

func (broker ggMayaSessiondBroker) statusWithTimeout(timeout time.Duration) (sessiondStatusResult, error) {
	raw, err := broker.runSessiondCLI([]string{"status", "--state-dir", broker.host.Broker.StateDir, "--json"}, timeout)
	return decodeSessiondStatus(raw, err)
}

func (broker ggMayaSessiondBroker) preRunProbeStatus(timeout time.Duration) (sessiondStatusResult, error) {
	// The no-op marker keeps bounded readiness fixture traffic distinct from
	// lifecycle status calls without changing the Session Broker CLI contract.
	raw, err := broker.runSessiondCLIWithMarker(
		[]string{"status", "--state-dir", broker.host.Broker.StateDir, "--json"},
		timeout,
		"$null = 'maya-stall-prerun-session-broker-probe'\n",
		true,
	)
	return decodeSessiondStatus(raw, err)
}

func decodeSessiondStatus(raw []byte, err error) (sessiondStatusResult, error) {
	if err != nil {
		return sessiondStatusResult{}, err
	}
	var envelope struct {
		OK    *bool  `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.OK != nil && !*envelope.OK {
		if envelope.Error != "" {
			return sessiondStatusResult{}, fmt.Errorf("gg_mayasessiond status failed: %s", envelope.Error)
		}
		return sessiondStatusResult{}, fmt.Errorf("gg_mayasessiond status failed")
	}
	var result sessiondStatusResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return sessiondStatusResult{}, fmt.Errorf("parse gg_mayasessiond status JSON: %w", err)
	}
	return result, nil
}

func (broker ggMayaSessiondBroker) ensureReady() (sessiondHealthResult, error) {
	health := broker.sessionBrokerHealth()
	if health.OK || !health.Recoverable {
		return health, nil
	}
	if err := broker.recoverSessionBroker(health.Reason); err != nil {
		health.Detail = fmt.Sprintf("%s; automatic recovery failed: %v", health.Detail, err)
		return health, nil
	}
	deadline := time.Now().Add(sessiondRecoveryTimeout)
	var last sessiondHealthResult
	for time.Now().Before(deadline) {
		last = broker.sessionBrokerHealth()
		if last.OK {
			last.Recovered = true
			return last, nil
		}
		time.Sleep(time.Second)
	}
	if last.Detail != "" {
		health = last
	}
	health.Detail = fmt.Sprintf("%s; automatic recovery did not restore gg_mayasessiond commandPort health", health.Detail)
	return health, nil
}

func (broker ggMayaSessiondBroker) sessionBrokerHealth() sessiondHealthResult {
	if err := broker.validate(); err != nil {
		return unrecoverableSessiondHealth("broker-config-invalid", err.Error(), "Configure gg_mayasessiond paths in host config. See docs/setup/windows-maya-host.md#session-broker.")
	}
	doctorRaw, err := broker.runSessiondCLI(sessiondDoctorArgs(broker.host), sessiondCommandTimeout)
	if err != nil {
		return unrecoverableSessiondHealth("sessiond-unreachable", err.Error(), "Start or repair gg_mayasessiond on this Maya Host. See docs/setup/windows-maya-host.md#session-broker.")
	}
	var doctor sessiondDoctorOutput
	if err := json.Unmarshal(doctorRaw, &doctor); err != nil {
		return unrecoverableSessiondHealth("invalid-doctor-json", "invalid doctor JSON", "Update gg_mayasessiond or fix its CLI JSON output. See docs/setup/windows-maya-host.md#session-broker.")
	}
	if !doctor.OK {
		detail := "gg_mayasessiond doctor failed"
		if failing := failingSessiondDoctorChecks(doctor); len(failing) > 0 {
			detail += ": " + strings.Join(failing, ", ")
		}
		return unrecoverableSessiondHealth("sessiond-doctor-failed", detail, "Repair the failing gg_mayasessiond doctor check. See docs/setup/windows-maya-host.md#session-broker.")
	}
	status, err := broker.status()
	if err != nil {
		return unrecoverableSessiondHealth("sessiond-status-unavailable", err.Error(), "Start or repair gg_mayasessiond on this Maya Host. See docs/setup/windows-maya-host.md#session-broker.")
	}
	effectiveStatus := status.DerivedStatus
	if effectiveStatus == "" {
		effectiveStatus = status.State.Status
	}
	if effectiveStatus != "running" {
		return unrecoverableSessiondHealth("sessiond-not-running", "gg_mayasessiond is not running", "Start the interactive gg_mayasessiond broker. See docs/setup/windows-maya-host.md#session-broker.")
	}
	if !status.State.CallServerReady {
		return recoverableSessiondHealth("command-port-not-ready", "gg_mayasessiond commandPort call server is not ready", "Restart the interactive gg_mayasessiond broker so Maya commandPort localhost:7001 is reacquired. See docs/setup/windows-maya-host.md#session-broker.")
	}
	processes, err := mayaProcessSessions(broker.host)
	if err != nil {
		return unrecoverableSessiondHealth("maya-process-unavailable", err.Error(), "Check Maya process state from the Windows host. See docs/setup/windows-maya-host.md#interactive-desktop.")
	}
	if len(processes) == 0 {
		return unrecoverableSessiondHealth("maya-not-running", "maya.exe is not running", "Start gg_mayasessiond from the interactive Windows desktop. See docs/setup/windows-maya-host.md#interactive-desktop.")
	}
	for _, process := range processes {
		if process.SessionID == 0 {
			return unrecoverableSessiondHealth("interactive-desktop-unavailable", "maya.exe is running in Windows Services session 0", "Restart gg_mayasessiond from the interactive Windows desktop. See docs/setup/windows-maya-host.md#interactive-desktop.")
		}
	}
	if err := broker.probeScriptExecute(); err != nil {
		reason := "script-execute-unavailable"
		hint := "Repair gg_mayasessiond script.execute access to the Maya Stall work root. See docs/setup/windows-maya-host.md#session-broker."
		if isCommandPortError(err) {
			reason = sessiondReasonCommandPortDown
			hint = "Restart the interactive gg_mayasessiond broker so Maya commandPort localhost:7001 is reacquired. See docs/setup/windows-maya-host.md#session-broker."
		}
		if isKnownSessiondScriptExecuteResponseError(err) {
			reason = "script-execute-invalid-response"
			hint = "Restart the interactive gg_mayasessiond broker so script.execute returns a valid tool result. See docs/setup/windows-maya-host.md#session-broker."
		}
		if reason == sessiondReasonCommandPortDown || reason == "script-execute-invalid-response" {
			return recoverableSessiondHealth(reason, err.Error(), hint)
		}
		return unrecoverableSessiondHealth(reason, err.Error(), hint)
	}
	return sessiondHealthResult{OK: true, Detail: "gg_mayasessiond reachable; Maya UI is interactive", InteractiveDesktop: true}
}

func recoverableSessiondHealth(reason string, detail string, hint string) sessiondHealthResult {
	return sessiondHealthResult{Reason: reason, Detail: detail, Hint: hint, Recoverable: true}
}

func unrecoverableSessiondHealth(reason string, detail string, hint string) sessiondHealthResult {
	return sessiondHealthResult{Reason: reason, Detail: detail, Hint: hint}
}

func isCommandPortError(err error) bool {
	message := sshFailureClassificationText(err)
	return strings.Contains(message, "commandport") || strings.Contains(message, "localhost:7001")
}

// Older live broker sessions can retain a handler that returns a malformed
// tool result instead of the expected object; restarting the session repairs it.
func isKnownSessiondScriptExecuteResponseError(err error) bool {
	message := sshFailureClassificationText(err)
	return strings.Contains(message, "'int' object has no attribute 'get'") ||
		strings.Contains(message, "expecting value: line 1 column 1 (char 0)")
}

func (ggMayaSessiondBroker) CaptureVisualEvidenceAfterRecoveredScenario(err error) bool {
	return isKnownSessiondScriptExecuteResponseError(err)
}

func (broker ggMayaSessiondBroker) recoverSessionBroker(reason string) error {
	taskName := sessiondRecoveryTaskName(broker.host)
	if _, err := runSSHCommandOutput(broker.host, encodedPowerShellCommand(sessiondRecoveryScript(broker.host, taskName, reason)), sessiondCommandTimeout+2*time.Minute); err != nil {
		return fmt.Errorf("restart scheduled task %q: %w", taskName, err)
	}
	return nil
}

func (broker ggMayaSessiondBroker) restartSessionBrokerTask(reason string) error {
	taskName := sessiondRecoveryTaskName(broker.host)
	if _, err := runSSHCommandOutput(broker.host, encodedPowerShellCommand(sessiondTaskRestartScript(taskName, reason)), sessiondCommandTimeout+2*time.Minute); err != nil {
		return fmt.Errorf("restart scheduled task %q: %w", taskName, err)
	}
	return nil
}

func sessiondTaskRestartScript(taskName string, reason string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$taskName = %s
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
if ($task.State -eq 'Running') {
  Stop-ScheduledTask -InputObject $task
  $stopped = $false
  for ($attempt = 0; $attempt -lt 90; $attempt++) {
    Start-Sleep -Seconds 1
    $task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
    if ($task.State -ne 'Running') {
      $stopped = $true
      break
    }
  }
  if (-not $stopped) {
    throw "scheduled task $taskName did not stop before restart"
  }
}
Start-ScheduledTask -InputObject $task
[pscustomobject]@{ok=$true; task=$taskName; reason=%s} | ConvertTo-Json -Compress`,
		powerShellSingleQuoted(taskName),
		powerShellSingleQuoted(reason),
	)
}

func sessiondRecoveryScript(host mayaHostConfig, taskName string, reason string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$taskName = %s
$stopError = $null
try {
  Set-Location -LiteralPath %s
  & %s -m gg_maya_sessiond.cli @('stop','--state-dir',%s,'--wait-timeout-seconds','120','--json') | Out-String | Write-Output
} catch {
  $stopError = $_.Exception.Message
  Write-Output "gg_mayasessiond stop during recovery failed: $stopError"
}
$task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
if ($task.State -eq "Running") {
  Stop-ScheduledTask -InputObject $task
  $stopped = $false
  for ($attempt = 0; $attempt -lt 90; $attempt++) {
    Start-Sleep -Seconds 1
    $task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
    if ($task.State -ne "Running") {
      $stopped = $true
      break
    }
  }
  if (-not $stopped) {
    throw "scheduled task $taskName did not stop before restart"
  }
}
Start-ScheduledTask -InputObject $task
[pscustomobject]@{ok=$true; task=$taskName; reason=%s; stopError=$stopError} | ConvertTo-Json -Compress`,
		powerShellSingleQuoted(taskName),
		powerShellSingleQuoted(host.Broker.Repo),
		powerShellSingleQuoted(host.Broker.Python),
		powerShellSingleQuoted(host.Broker.StateDir),
		powerShellSingleQuoted(reason),
	)
}

func sessiondRecoveryTaskName(host mayaHostConfig) string {
	if task := strings.TrimSpace(host.Broker.RecoveryTask); task != "" {
		return task
	}
	return defaultSessiondRecoveryTask
}

func (broker ggMayaSessiondBroker) scenarioWrapper(context runContext, scenario scenarioConfig) (string, error) {
	resultPath := context.RunWorkspace.RemoteScenarioResultPath()
	if err := rejectSFTPRepoPath(scenario.ExpectedOutputs.ScenarioResult); err != nil {
		return "", err
	}
	scripts, err := remotePayloadScripts(context.RunWorkspace, scenario.Payload)
	if err != nil {
		return "", err
	}
	includePaths, err := remotePayloadIncludePaths(context.RunWorkspace, scenario.Payload)
	if err != nil {
		return "", err
	}
	environment := map[string]string{scenarioResultEnvVar: resultPath}
	for key, value := range context.Environment {
		if key == "" || key == scenarioResultEnvVar || value == "" {
			continue
		}
		environment[key] = value
	}
	var builder strings.Builder
	builder.WriteString("import json, os, runpy, sys, traceback\n")
	builder.WriteString("result_path = ")
	builder.WriteString(pythonString(resultPath))
	builder.WriteString("\n")
	builder.WriteString("run_modules_root = ")
	builder.WriteString(pythonString(context.RunWorkspace.RemoteRunModulesRoot()))
	builder.WriteString("\n")
	builder.WriteString("maya_stall_environment = ")
	builder.WriteString(pythonStringMap(environment))
	builder.WriteString("\n")
	builder.WriteString("previous_cwd = os.getcwd()\n")
	builder.WriteString("previous_sys_path = list(sys.path)\n")
	builder.WriteString("previous_environment = {}\n")
	builder.WriteString("def _maya_stall_write_result(status, summary, traceback_text=None, overwrite=False):\n")
	builder.WriteString("    if os.path.exists(result_path) and not overwrite:\n")
	builder.WriteString("        return\n")
	builder.WriteString("    payload = {'status': status, 'summary': summary}\n")
	builder.WriteString("    if traceback_text is not None:\n")
	builder.WriteString("        payload['traceback'] = traceback_text\n")
	builder.WriteString("    with open(result_path, 'w', encoding='utf-8') as handle:\n")
	builder.WriteString("        json.dump(payload, handle)\n")
	builder.WriteString("        handle.write('\\n')\n")
	builder.WriteString("def _maya_stall_should_overwrite_failure():\n")
	builder.WriteString("    if not os.path.exists(result_path):\n")
	builder.WriteString("        return True\n")
	builder.WriteString("    try:\n")
	builder.WriteString("        with open(result_path, 'r', encoding='utf-8') as handle:\n")
	builder.WriteString("            payload = json.load(handle)\n")
	builder.WriteString("    except Exception:\n")
	builder.WriteString("        return True\n")
	builder.WriteString("    return payload.get('status') in (None, '', 'passed')\n")
	builder.WriteString("def _maya_stall_clear_run_modules():\n")
	builder.WriteString("    root = os.path.normcase(os.path.abspath(run_modules_root))\n")
	builder.WriteString("    for name, module in list(sys.modules.items()):\n")
	builder.WriteString("        module_file = getattr(module, '__file__', None)\n")
	builder.WriteString("        if not module_file:\n")
	builder.WriteString("            continue\n")
	builder.WriteString("        try:\n")
	builder.WriteString("            module_path = os.path.normcase(os.path.abspath(module_file))\n")
	builder.WriteString("        except Exception:\n")
	builder.WriteString("            continue\n")
	builder.WriteString("        if module_path.startswith(root + os.sep):\n")
	builder.WriteString("            sys.modules.pop(name, None)\n")
	builder.WriteString("class _MayaStallStopScenario(Exception):\n")
	builder.WriteString("    pass\n")
	builder.WriteString("try:\n")
	builder.WriteString("    for key in maya_stall_environment:\n")
	builder.WriteString("        previous_environment[key] = os.environ.get(key)\n")
	builder.WriteString("    for key, value in maya_stall_environment.items():\n")
	builder.WriteString("        os.environ[key] = value\n")
	builder.WriteString("    os.makedirs(os.path.dirname(result_path), exist_ok=True)\n")
	builder.WriteString("    os.chdir(")
	builder.WriteString(pythonString(context.RunWorkspace.RemoteWorkspace()))
	builder.WriteString(")\n")
	builder.WriteString("    _maya_stall_clear_run_modules()\n")
	builder.WriteString("    for include_path in reversed(")
	builder.WriteString(pythonStringList(includePaths))
	builder.WriteString("):\n        sys.path.insert(0, include_path)\n")
	builder.WriteString("    script_paths = ")
	builder.WriteString(pythonStringList(scripts))
	builder.WriteString("\n")
	builder.WriteString("    for script_index, script_path in enumerate(script_paths):\n")
	builder.WriteString("        try:\n")
	builder.WriteString("            runpy.run_path(script_path, run_name='__main__')\n")
	builder.WriteString("        except SystemExit as exc:\n")
	builder.WriteString("            code = exc.code\n")
	builder.WriteString("            if code is None or code == 0:\n")
	builder.WriteString("                if script_index == len(script_paths) - 1:\n")
	builder.WriteString("                    _maya_stall_write_result('passed', 'gg_mayasessiond Scenario completed')\n")
	builder.WriteString("                else:\n")
	builder.WriteString("                    _maya_stall_write_result('failed', 'Scenario exited before running all scripts', overwrite=_maya_stall_should_overwrite_failure())\n")
	builder.WriteString("            else:\n")
	builder.WriteString("                _maya_stall_write_result('failed', 'Scenario exited with code %s' % code, overwrite=_maya_stall_should_overwrite_failure())\n")
	builder.WriteString("            raise _MayaStallStopScenario()\n")
	builder.WriteString("    _maya_stall_write_result('passed', 'gg_mayasessiond Scenario completed')\n")
	builder.WriteString("except _MayaStallStopScenario:\n")
	builder.WriteString("    pass\n")
	builder.WriteString("except Exception as exc:\n")
	builder.WriteString("    _maya_stall_write_result('failed', str(exc), traceback.format_exc(), overwrite=_maya_stall_should_overwrite_failure())\n")
	builder.WriteString("finally:\n")
	builder.WriteString("    sys.path[:] = previous_sys_path\n")
	builder.WriteString("    os.chdir(previous_cwd)\n")
	builder.WriteString("    for key, value in previous_environment.items():\n")
	builder.WriteString("        if value is None:\n")
	builder.WriteString("            os.environ.pop(key, None)\n")
	builder.WriteString("        else:\n")
	builder.WriteString("            os.environ[key] = value\n")
	builder.WriteString("    _maya_stall_clear_run_modules()\n")
	return builder.String(), nil
}

func remotePayloadScripts(workspace runWorkspace, payload runPayload) ([]string, error) {
	paths := append([]string{}, payload.MayaScripts...)
	paths = append(paths, payload.Scripts...)
	remote := make([]string, 0, len(paths))
	for _, source := range paths {
		remotePath, err := workspace.remotePayloadKindPath("mayaScripts", source)
		if err != nil {
			return nil, err
		}
		remote = append(remote, remotePath)
	}
	return remote, nil
}

func remotePayloadIncludePaths(workspace runWorkspace, payload runPayload) ([]string, error) {
	remote := make([]string, 0, len(payload.IncludePaths))
	for _, source := range payload.IncludePaths {
		remotePath, err := workspace.remotePayloadKindPath("includePaths", source)
		if err != nil {
			return nil, err
		}
		remote = append(remote, remotePath)
	}
	return remote, nil
}

func (broker ggMayaSessiondBroker) callCapture() (sessiondCaptureResult, error) {
	raw, err := broker.runSessiondCLI([]string{"call", "--state-dir", broker.host.Broker.StateDir, "viewport.capture", "format=jpeg", "width=1024", "height=576", "quality=85", "--json"}, sessiondCommandTimeout)
	if err != nil {
		return sessiondCaptureResult{}, err
	}
	var result sessiondCaptureResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return sessiondCaptureResult{}, fmt.Errorf("parse gg_mayasessiond viewport.capture JSON: %w", err)
	}
	return result, nil
}

func (broker ggMayaSessiondBroker) callTool(tool string, args []string, timeout time.Duration) (sessiondCommandResult, error) {
	cliArgs := []string{"call", "--state-dir", broker.host.Broker.StateDir, tool}
	cliArgs = append(cliArgs, args...)
	cliArgs = append(cliArgs, "--json")
	raw, err := broker.runSessiondCLI(cliArgs, timeout)
	if err != nil {
		return sessiondCommandResult{}, err
	}
	var result sessiondCommandResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return sessiondCommandResult{}, fmt.Errorf("parse gg_mayasessiond %s JSON: %w", tool, err)
	}
	return result, nil
}

func (broker ggMayaSessiondBroker) runSessiondCLI(args []string, timeout time.Duration) ([]byte, error) {
	return broker.runSessiondCLIWithMarker(args, timeout, "", false)
}

func (broker ggMayaSessiondBroker) runSessiondCLIWithMarker(args []string, timeout time.Duration, marker string, failClosedOnTimeout bool) ([]byte, error) {
	if err := broker.validate(); err != nil {
		return nil, err
	}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, powerShellSingleQuoted(arg))
	}
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
%s
Set-Location -LiteralPath %s
& %s -m gg_maya_sessiond.cli @(%s)`, marker, powerShellSingleQuoted(broker.host.Broker.Repo), powerShellSingleQuoted(broker.host.Broker.Python), strings.Join(quoted, ","))
	raw, err := runSSHCommandOutput(broker.host, encodedPowerShellCommand(script), timeout)
	if err != nil {
		if failClosedOnTimeout && errors.Is(err, errSSHCommandTimedOut) {
			return nil, fmt.Errorf("run gg_mayasessiond %s: %w", args[0], err)
		}
		if args[0] == "status" {
			if jsonOutput, ok := sessiondStatusJSONFromFailedOutput(raw); ok {
				return jsonOutput, nil
			}
		}
		if jsonOutput, ok := sessiondJSONFromFailedOutput(raw); ok {
			return jsonOutput, nil
		}
		return nil, fmt.Errorf("run gg_mayasessiond %s: %w", args[0], err)
	}
	jsonOutput := trimToJSON(raw)
	if !isSessiondJSONDocument(jsonOutput) {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(jsonOutput, &object); err == nil && hasAnyJSONKey(object, "level", "msg") {
			return nil, fmt.Errorf("gg_mayasessiond %s returned no sessiond JSON result", args[0])
		}
		var document json.RawMessage
		if err := json.Unmarshal(jsonOutput, &document); err != nil {
			return nil, fmt.Errorf("gg_mayasessiond %s returned no sessiond JSON result", args[0])
		}
	}
	return jsonOutput, nil
}

func (broker ggMayaSessiondBroker) stageRemoteFile(path string, content []byte) error {
	if err := broker.validate(); err != nil {
		return err
	}
	if err := rejectSFTPBatchUnsafePath(path); err != nil {
		return err
	}
	tempFile, err := os.CreateTemp("", "maya-stall-sessiond-*")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.Write(content); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	batch := newSFTPBatch()
	batch.mkdirAll(remoteDir(path))
	batch.put(tempPath, path)
	return runSFTPBatch(broker.host, batch.String())
}

func (broker ggMayaSessiondBroker) probeScriptExecute() (err error) {
	runID := fmt.Sprintf("doctor-%d", time.Now().UTC().UnixNano())
	probeRoot := remoteJoin(broker.host.WorkRoot, "runs", runID)
	probePath := remoteJoin(probeRoot, "workspace", ".maya-stall-doctor.py")
	if err := broker.stageRemoteFile(probePath, []byte("print('maya-stall doctor script.execute ok')\n")); err != nil {
		return fmt.Errorf("stage gg_mayasessiond script.execute probe: %w", err)
	}
	defer func() {
		if cleanupErr := broker.removeRemotePath(probeRoot); cleanupErr != nil {
			cleanupErr = fmt.Errorf("cleanup gg_mayasessiond script.execute probe: %w", cleanupErr)
			if err == nil {
				err = cleanupErr
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	result, err := broker.callTool("script.execute", []string{
		"file_path=" + probePath,
		"timeout=30",
	}, sessiondCommandTimeout)
	if err != nil {
		return fmt.Errorf("run gg_mayasessiond script.execute probe: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("gg_mayasessiond script.execute probe failed: %s", result.Error)
	}
	return nil
}

func (broker ggMayaSessiondBroker) probeDesktopRecordingReadiness() (err error) {
	runID := fmt.Sprintf("doctor-%d", time.Now().UTC().UnixNano())
	probeRoot := remoteJoin(broker.host.WorkRoot, "runs", runID)
	remoteRoot := remoteJoin(probeRoot, "visual-evidence", "recording")
	defer func() {
		if cleanupErr := broker.removeRemotePath(probeRoot); cleanupErr != nil {
			cleanupErr = fmt.Errorf("cleanup desktop recording readiness probe: %w", cleanupErr)
			if err == nil {
				err = cleanupErr
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	if _, err := captureWindowsDesktopRecording(sshWindowsDesktopTransport(broker.host), remoteRoot, time.Second, 1, ""); err != nil {
		return err
	}
	return nil
}

func (broker ggMayaSessiondBroker) removeRemotePath(path string) error {
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
if (Test-Path -LiteralPath %s) {
  Remove-Item -LiteralPath %s -Recurse -Force
}`, powerShellSingleQuoted(path), powerShellSingleQuoted(path))
	_, err := runSSHCommandOutput(broker.host, encodedPowerShellCommand(script), sessiondCommandTimeout)
	return err
}

func captureImageData(result sessiondCaptureResult) ([]byte, string, error) {
	mediaType := result.Output.MimeType
	for _, item := range result.Content {
		if item.Type != "image" {
			continue
		}
		if item.Data == "" {
			continue
		}
		if item.MimeType != "" {
			mediaType = item.MimeType
		}
		data, err := base64.StdEncoding.DecodeString(item.Data)
		if err != nil {
			return nil, "", fmt.Errorf("decode viewport.capture image data: %w", err)
		}
		if mediaType == "" {
			mediaType = "image/jpeg"
		}
		return data, mediaType, nil
	}
	return nil, "", fmt.Errorf("gg_mayasessiond viewport.capture returned no image data")
}

func visualEvidenceNameForMediaType(name string, mediaType string) string {
	if name == "" || name == "." || name == ".." {
		name = "screenshot"
	}
	extension := ".jpg"
	switch mediaType {
	case "image/png":
		extension = ".png"
	case "image/jpeg", "":
		extension = ".jpg"
	default:
		extension = ".bin"
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if base == "" || base == "." || base == ".." {
		base = "screenshot"
	}
	return base + extension
}

func pythonString(value string) string {
	content, _ := json.Marshal(value)
	return string(content)
}

func pythonStringList(values []string) string {
	content, _ := json.Marshal(values)
	return string(content)
}

func pythonStringMap(values map[string]string) string {
	content, _ := json.Marshal(values)
	return string(content)
}

func trimToJSON(raw []byte) []byte {
	trimmed := bytes.TrimSpace(raw)
	var firstDocument json.RawMessage
	for start := bytes.IndexAny(trimmed, "{["); start >= 0; {
		jsonOutput := trimmed[start:]
		var document json.RawMessage
		decoder := json.NewDecoder(bytes.NewReader(jsonOutput))
		advance := 1
		if err := decoder.Decode(&document); err == nil {
			if offset := int(decoder.InputOffset()); offset > 0 {
				advance = offset
			}
			document = bytes.TrimSpace(document)
			if firstDocument == nil {
				firstDocument = append(json.RawMessage(nil), document...)
			}
			if isSessiondJSONDocument(document) {
				return document
			}
		}
		next := bytes.IndexAny(trimmed[start+advance:], "{[")
		if next < 0 {
			break
		}
		start += advance + next
	}
	if firstDocument != nil {
		return firstDocument
	}
	return trimmed
}

func isSessiondJSONDocument(document []byte) bool {
	if len(document) == 0 || document[0] != '{' {
		return false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(document, &object); err != nil {
		return false
	}
	if raw, ok := object["ok"]; ok {
		var okValue bool
		if err := json.Unmarshal(raw, &okValue); err == nil && !hasAnyJSONKey(object, "level") {
			return hasAnyJSONKey(object, "tool", "checks", "content", "output", "error", "status")
		}
	}
	if isSessiondStatusJSONObject(object) {
		return true
	}
	if raw, ok := object["state"]; ok && !hasAnyJSONKey(object, "level", "msg") {
		var state map[string]json.RawMessage
		if err := json.Unmarshal(raw, &state); err == nil {
			return hasAnyJSONKey(state, "status")
		}
	}
	return false
}

func isSessiondStatusJSONObject(object map[string]json.RawMessage) bool {
	if hasAnyJSONKey(object, "level", "msg") {
		return false
	}
	var hasState bool
	if err := json.Unmarshal(object["has_state"], &hasState); err != nil {
		return false
	}
	var derivedStatus string
	if err := json.Unmarshal(object["derived_status"], &derivedStatus); err != nil || strings.TrimSpace(derivedStatus) == "" {
		return false
	}
	var state map[string]json.RawMessage
	return json.Unmarshal(object["state"], &state) == nil
}

func hasAnyJSONKey(object map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if _, ok := object[key]; ok {
			return true
		}
	}
	return false
}

func sessiondJSONFromFailedOutput(raw []byte) ([]byte, bool) {
	jsonOutput := trimToJSON(raw)
	if !isSessiondJSONDocument(jsonOutput) {
		return nil, false
	}
	var object map[string]any
	if err := json.Unmarshal(jsonOutput, &object); err != nil {
		return nil, false
	}
	if _, ok := object["ok"]; !ok {
		return nil, false
	}
	okValue, ok := object["ok"].(bool)
	if !ok || okValue {
		return nil, false
	}
	return jsonOutput, true
}

func sessiondStatusJSONFromFailedOutput(raw []byte) ([]byte, bool) {
	jsonOutput := trimToJSON(raw)
	if !isSessiondJSONDocument(jsonOutput) {
		return nil, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(jsonOutput, &object); err != nil || !isSessiondStatusJSONObject(object) {
		return nil, false
	}
	var hasState bool
	if err := json.Unmarshal(object["has_state"], &hasState); err != nil {
		return nil, false
	}
	var derivedStatus string
	if err := json.Unmarshal(object["derived_status"], &derivedStatus); err != nil {
		return nil, false
	}
	if !hasState {
		return jsonOutput, derivedStatus == "missing"
	}
	if derivedStatus != "stopped" {
		return nil, false
	}
	var state struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(object["state"], &state); err != nil || state.Status != "stopped" {
		return nil, false
	}
	return jsonOutput, true
}

func runSSHCommandOutput(host mayaHostConfig, remoteCommand []string, timeout time.Duration) ([]byte, error) {
	return runSSHCommandOutputWithInput(host, remoteCommand, "", timeout)
}

func runSSHCommandOutputWithInput(host mayaHostConfig, remoteCommand []string, input string, timeout time.Duration) ([]byte, error) {
	binary := host.SSH.Binary
	if binary == "" {
		binary = "ssh"
	}
	if timeout <= 0 {
		timeout = sshCommandTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, binary, append(sshArgs(host), remoteCommand...)...)
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return stdout.Bytes(), fmt.Errorf("%w after %s", errSSHCommandTimedOut, timeout)
		}
		detail := sshFailureDetail(err, stderr.String())
		err = sanitizedSSHExecutionErrorFor(err, stderr.String())
		if detail != "" {
			return stdout.Bytes(), fmt.Errorf("ssh command failed: %w: %s", err, detail)
		}
		return stdout.Bytes(), fmt.Errorf("ssh command failed: %w", err)
	}
	return stdout.Bytes(), nil
}

func firstUsefulStderrLineUnbounded(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#< CLIXML") || strings.HasPrefix(line, "<Objs ") {
			continue
		}
		return line
	}
	return ""
}

func sshFailureDetail(err error, stderr string) string {
	if strings.TrimSpace(stderr) == "" {
		return ""
	}
	// OpenSSH forwards remote exit codes and combines remote stderr with its own;
	// exit 255 is ambiguous, so only known product diagnostics from other exits
	// may cross the public boundary, and only in canonical form.
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() != 255 {
		privateDetail := strings.ToLower(stderr)
		for _, diagnostic := range publicRemoteSSHFailureDiagnostics {
			if strings.Contains(privateDetail, diagnostic.match) {
				return diagnostic.message
			}
		}
	}
	return "SSH operation reported an error"
}

func sftpFailureDetail(stderr string) string {
	if firstUsefulStderrLineUnbounded(stderr) == "" {
		return ""
	}
	return "SFTP operation reported an error"
}

var publicRemoteSSHFailureDiagnostics = []struct {
	match   string
	message string
}{
	{"agent work root grants access to another identity", "Agent work root grants access to another identity"},
	{"remote capture root is not writable", "remote capture root is not writable"},
	{"schtasks.exe is required for interactive desktop capture", "schtasks.exe is required for interactive desktop capture"},
	{"schtasks.exe is required for interactive desktop control modal proof", "schtasks.exe is required for interactive desktop control modal proof"},
	{"schtasks.exe is required for interactive desktop control", "schtasks.exe is required for interactive desktop control"},
	{"windows powershell desktop assemblies system.windows.forms and system.drawing are required", "Windows PowerShell desktop assemblies System.Windows.Forms and System.Drawing are required"},
	{"interactive desktop session is unavailable for screenshot capture", "interactive desktop session is unavailable for screenshot capture"},
	{"interactive desktop session is unavailable for recording capture", "interactive desktop session is unavailable for recording capture"},
	{"setcursorpos failed; interactive desktop session is unavailable for desktop control", "SetCursorPos failed; interactive desktop session is unavailable for desktop control"},
	{"failed to create interactive desktop screenshot task with schtasks.exe /it", "failed to create interactive desktop screenshot task with schtasks.exe /IT; ensure an interactive desktop session is logged in"},
	{"failed to create interactive desktop recording task with schtasks.exe /it", "failed to create interactive desktop recording task with schtasks.exe /IT; ensure an interactive desktop session is logged in"},
	{"failed to create interactive desktop click task with schtasks.exe /it", "failed to create interactive desktop click task with schtasks.exe /IT; ensure an interactive desktop session is logged in"},
	{"failed to create interactive desktop control modal task with schtasks.exe /it", "failed to create interactive desktop control modal task with schtasks.exe /IT; ensure an interactive desktop session is logged in"},
	{"scheduled interactive desktop screenshot did not produce output", "scheduled interactive desktop screenshot did not produce output; ensure an interactive desktop session is logged in"},
	{"scheduled interactive desktop recording did not produce output", "scheduled interactive desktop recording did not produce output; ensure an interactive desktop session is logged in"},
	{"scheduled interactive desktop click did not complete", "scheduled interactive desktop click did not complete; ensure an interactive desktop session is logged in"},
	{"scheduled interactive desktop control modal did not appear", "scheduled interactive desktop control modal did not appear; ensure an interactive desktop session is logged in"},
	{"desktop control modal fixture did not close after click", "desktop control modal fixture did not close after click"},
	{"compress-archive is not recognized", "Compress-Archive is not recognized"},
	{"timed out waiting for host lock mutex file", "timed out waiting for Host Lock mutex file"},
	{"did not stop before restart", "scheduled Session Broker task did not stop before restart"},
	{"access denied removing trusted plugin artifact", "access denied removing trusted Plugin Artifact"},
	{"ssh connection lost after task start", "ssh connection lost after task start"},
}

type sanitizedSSHExecutionError struct {
	cause         error
	message       string
	privateDetail string
}

func (err *sanitizedSSHExecutionError) Error() string { return err.message }
func (err *sanitizedSSHExecutionError) Unwrap() error { return err.cause }

func sanitizedSSHExecutionErrorFor(err error, privateDetail string) error {
	return sanitizedTransportExecutionErrorFor(err, privateDetail, "SSH")
}

func sanitizedSFTPExecutionErrorFor(err error, privateDetail string) error {
	return sanitizedTransportExecutionErrorFor(err, privateDetail, "SFTP")
}

func sanitizedTransportExecutionErrorFor(err error, privateDetail string, executableLabel string) error {
	message := fmt.Sprintf("[%s executable] could not start", executableLabel)
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message = exitErr.Error()
	}
	return &sanitizedSSHExecutionError{cause: err, message: message, privateDetail: privateDetail}
}

// Recovery classification may inspect remote application failures, but raw SSH
// stderr must never cross the public diagnostic boundary through Error().
func sshFailureClassificationText(err error) string {
	message := strings.ToLower(err.Error())
	var sshErr *sanitizedSSHExecutionError
	if errors.As(err, &sshErr) {
		message += "\n" + strings.ToLower(sshErr.privateDetail)
	}
	return message
}
