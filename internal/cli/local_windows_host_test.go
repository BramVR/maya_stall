//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalWindowsHostStagesVerifiedRuntimeInputCollectsOutputAndCleansOwnedRun(t *testing.T) {
	repoDir := t.TempDir()
	workRoot := filepath.Join(t.TempDir(), "maya-stall-local")
	hostConfig := mayaHostConfig{
		ID:        "local-test",
		Transport: "local",
		WorkRoot:  workRoot,
		Broker: brokerConfig{
			Structured: true,
			Type:       "gg-mayasessiond",
			StateDir:   filepath.Join(workRoot, "sessiond"),
			Python:     `C:\Python311\python.exe`,
			Repo:       `C:\src\GG_MayaSessiond`,
			MCPSource:  `C:\src\GG_MayaMCP`,
			MayaExe:    `C:\Program Files\Autodesk\Maya2025\bin\maya.exe`,
			Port:       7123,
		},
	}
	workspace, err := newRunWorkspace(repoDir, "run-local-stage", workRoot, "scenario-result.json")
	if err != nil {
		t.Fatal(err)
	}
	item := manifestPayload{Name: "scene", Kind: "runtimeInput:file", Staged: filepath.Join("payload", "runtimeInputs", "scenes", "input.ma")}
	localSnapshot := workspace.LocalPayloadPath(item)
	if err := os.MkdirAll(filepath.Dir(localSnapshot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localSnapshot, []byte("original runtime input"), 0o644); err != nil {
		t.Fatal(err)
	}
	item.Size, item.SHA256, err = summarizePlanPayload(localSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	context := runContext{RepoDir: repoDir, RunWorkspace: workspace, Workspace: workspace.LocalWorkspace()}
	host := localWindowsHost{host: hostConfig}
	remoteWorkspace := filepath.FromSlash(workspace.RemoteWorkspace())
	if err := os.MkdirAll(remoteWorkspace, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".maya-stall-maya-build.py", ".maya-stall-maya-build.txt"} {
		if err := os.WriteFile(filepath.Join(remoteWorkspace, name), []byte("owned build probe\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := host.StagePayload(context, []manifestPayload{item}); err != nil {
		t.Fatal(err)
	}
	staged := filepath.FromSlash(workspace.RemotePayloadPath(item))
	if got, err := os.ReadFile(staged); err != nil || string(got) != "original runtime input" {
		t.Fatalf("staged runtime input = %q, %v", got, err)
	}
	if err := os.WriteFile(localSnapshot, []byte("tampered snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := host.StagePayload(context, []manifestPayload{item}); err == nil || !strings.Contains(err.Error(), "changed after snapshot") {
		t.Fatalf("tampered snapshot StagePayload error = %v", err)
	}

	remoteOutput := filepath.FromSlash(workspace.RemoteOutputPath("proof/result.txt"))
	if err := os.MkdirAll(filepath.Dir(remoteOutput), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remoteOutput, []byte("owned output"), 0o644); err != nil {
		t.Fatal(err)
	}
	contract := scenarioContract{Outputs: []scenarioOutputPath{{Path: "proof/result.txt"}}}
	if err := host.CollectArtifacts(context, contract); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(context.Workspace, "proof", "result.txt")); err != nil || string(got) != "owned output" {
		t.Fatalf("collected output = %q, %v", got, err)
	}

	broker := ggMayaSessiondBroker{host: hostConfig}
	if err := broker.CleanupRun(context); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.FromSlash(workspace.RemoteRunRoot())); !os.IsNotExist(err) {
		t.Fatalf("owned local Run workspace remains after cleanup: %v", err)
	}
}

func TestLocalWindowsHostRejectsUnexpectedPreStageRunResidue(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "maya-stall-local")
	hostConfig := mayaHostConfig{
		ID: "local-test", Transport: "local", WorkRoot: workRoot,
		Broker: brokerConfig{Structured: true, Type: "gg-mayasessiond", StateDir: filepath.Join(workRoot, "sessiond"), Python: `C:\python.exe`, Repo: `C:\sessiond`, MCPSource: `C:\mcp`, MayaExe: `C:\maya.exe`, Port: 7123},
	}
	workspace, err := newRunWorkspace(t.TempDir(), "run-local-residue", workRoot, "scenario-result.json")
	if err != nil {
		t.Fatal(err)
	}
	unexpected := filepath.Join(filepath.FromSlash(workspace.RemoteWorkspace()), "unexpected.txt")
	if err := os.MkdirAll(filepath.Dir(unexpected), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unexpected, []byte("not owned by build verification\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	context := runContext{RunWorkspace: workspace}
	if err := (localWindowsHost{host: hostConfig}).StagePayload(context, nil); err == nil || !strings.Contains(err.Error(), "unexpected pre-stage path") {
		t.Fatalf("unexpected pre-stage residue error = %v", err)
	}
}

func TestLocalWindowsBrokerStartsAndOwnsExactDirectSessiondSession(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "maya-stall-local")
	hostConfig := mayaHostConfig{
		ID: "local-test", Transport: "local", WorkRoot: workRoot,
		Broker: brokerConfig{Structured: true, Type: "gg-mayasessiond", StateDir: filepath.Join(workRoot, "sessiond"), Python: `C:\Python311\python.exe`, Repo: `C:\src\GG_MayaSessiond`, MCPSource: `C:\src\GG_MayaMCP`, MayaExe: `C:\Program Files\Autodesk\Maya2025\bin\maya.exe`, Port: 7123},
	}
	previous := runLocalSessiondCommand
	t.Cleanup(func() { runLocalSessiondCommand = previous })
	var calls [][]string
	runLocalSessiondCommand = func(_ ggMayaSessiondBroker, args []string, _ time.Duration) ([]byte, error) {
		calls = append(calls, append([]string(nil), args...))
		switch args[0] {
		case "start":
			return []byte(`{"ok":true}`), nil
		case "status":
			if len(calls) == 1 {
				return []byte(`{"state_dir":"state","has_state":false,"state":{},"process_alive":{"daemon":false,"maya":false,"mcp":false},"derived_status":"missing"}`), nil
			}
			status := sessiondStatusResult{HasState: true, DerivedStatus: "running"}
			status.State.Status = "running"
			status.State.SessionID = "owned-local-session"
			status.State.CallServerReady = true
			return json.Marshal(status)
		case "call":
			if containsString(args, "scene.info") {
				return []byte(`{"ok":true,"tool":"scene.info"}`), nil
			}
			if containsString(args, "viewport.capture") {
				return []byte(`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"AA==","mimeType":"image/jpeg"}]}`), nil
			}
			t.Fatalf("unexpected local Sessiond readiness call %#v", args)
			return nil, nil
		default:
			t.Fatalf("unexpected local Sessiond operation %q", args[0])
			return nil, nil
		}
	}
	broker := ggMayaSessiondBroker{host: hostConfig}
	eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(eventsPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	identity, err := broker.StartFreshSession(runContext{EventsPath: eventsPath}, scenarioConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if identity.SessionID != "owned-local-session" || identity.BrokerAdapter != "gg-mayasessiond" {
		t.Fatalf("session identity = %+v", identity)
	}
	if len(calls) != 5 || calls[1][0] != "start" || !containsString(calls[1], "--maya-exe") || !containsString(calls[1], "--mcp-src") || !containsString(calls[1], "--port") || !containsString(calls[3], "scene.info") || !containsString(calls[4], "viewport.capture") {
		t.Fatalf("local Sessiond calls = %#v", calls)
	}
}

func TestLocalWindowsBrokerWaitsForMayaToolReadinessAfterCallServerStarts(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "maya-stall-local")
	broker := ggMayaSessiondBroker{host: mayaHostConfig{
		ID: "local-test", Transport: "local", WorkRoot: workRoot,
		Broker: brokerConfig{Structured: true, Type: "gg-mayasessiond", StateDir: filepath.Join(workRoot, "sessiond"), Python: `C:\Python311\python.exe`, Repo: `C:\src\GG_MayaSessiond`, MCPSource: `C:\src\GG_MayaMCP`, MayaExe: `C:\Program Files\Autodesk\Maya2025\bin\maya.exe`, Port: 7123},
	}}
	previousCommand := runLocalSessiondCommand
	previousPoll := waitSessiondSessionPoll
	t.Cleanup(func() {
		runLocalSessiondCommand = previousCommand
		waitSessiondSessionPoll = previousPoll
	})
	statusCalls := 0
	probeCalls := 0
	captureCalls := 0
	pollCalls := 0
	waitSessiondSessionPoll = func() { pollCalls++ }
	runLocalSessiondCommand = func(_ ggMayaSessiondBroker, args []string, _ time.Duration) ([]byte, error) {
		switch args[0] {
		case "start":
			return []byte(`{"ok":true}`), nil
		case "status":
			statusCalls++
			if statusCalls == 1 {
				return []byte(`{"has_state":false,"state":{},"derived_status":"missing"}`), nil
			}
			return []byte(`{"has_state":true,"derived_status":"running","state":{"status":"running","session_id":"owned-local-session","call_server_ready":true}}`), nil
		case "call":
			if containsString(args, "scene.info") {
				probeCalls++
				if probeCalls == 1 {
					return []byte(`{"ok":true,"tool":"status"}`), nil
				}
				return []byte(`{"ok":true,"tool":"scene.info"}`), nil
			}
			if containsString(args, "viewport.capture") {
				captureCalls++
				if captureCalls == 1 {
					return []byte(`{"ok":true,"tool":"viewport.capture"}`), nil
				}
				return []byte(`{"ok":true,"tool":"viewport.capture","content":[{"type":"image","data":"AA==","mimeType":"image/jpeg"}]}`), nil
			}
			t.Fatalf("unexpected local Sessiond readiness call %#v", args)
			return nil, nil
		default:
			t.Fatalf("unexpected local Sessiond operation %q", args[0])
			return nil, nil
		}
	}

	identity, err := broker.StartFreshSession(runContext{EventsPath: filepath.Join(t.TempDir(), "events.jsonl")}, scenarioConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if identity.SessionID != "owned-local-session" || probeCalls != 3 || captureCalls != 2 || pollCalls != 2 {
		t.Fatalf("identity=%+v probeCalls=%d captureCalls=%d pollCalls=%d, want owned identity after scene and viewport readiness retries", identity, probeCalls, captureCalls, pollCalls)
	}
}

func TestLocalWindowsBrokerRefusesCleanupOutsideOwnedRunsRoot(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "maya-stall-local")
	broker := ggMayaSessiondBroker{host: mayaHostConfig{Transport: "local", WorkRoot: workRoot}}
	if err := broker.removeRemotePath(workRoot); err == nil {
		t.Fatal("removeRemotePath accepted workRoot instead of an owned Run path")
	}
}

func TestLocalWindowsDesktopTransportPreservesBinaryStdout(t *testing.T) {
	transport := localWindowsDesktopTransport{workRoot: t.TempDir()}
	data, err := transport.RunPowerShell(`[byte[]]$data = 1,2,3,4
[Console]::OpenStandardOutput().Write($data, 0, $data.Length)`, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("binary stdout = %v", data)
	}
}

func TestLocalWindowsHostLockIsAuthoritativeAcrossConsumingReposAndBindsSession(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "shared-local-slot")
	host := mayaHostConfig{ID: "shared-local", Transport: "local", WorkRoot: workRoot, Broker: brokerConfig{StateDir: filepath.Join(workRoot, "sessiond"), Python: `C:\python.exe`, Repo: `C:\sessiond`}}
	first, locked, err := acquireRunHostLock(t.TempDir(), host)
	if err != nil || locked {
		t.Fatalf("first Host Lock: locked=%t err=%v", locked, err)
	}
	if err := first.markActive("run-first"); err != nil {
		t.Fatal(err)
	}
	if err := first.bindSession("owned-session-first"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workRoot, "state", "locks", "hosts", "host.lock"))
	if err != nil {
		t.Fatal(err)
	}
	owner := parseHostLockOwner(string(content))
	if owner.ActiveRun != "run-first" || owner.BrokerSession != "owned-session-first" {
		t.Fatalf("authoritative Host Lock owner = %+v", owner)
	}
	aliasHost := host
	aliasHost.ID = "shared-local-alias"
	second, locked, err := acquireRunHostLock(t.TempDir(), aliasHost)
	if err != nil || !locked {
		t.Fatalf("second Consuming Repo Host Lock: locked=%t err=%v", locked, err)
	}
	if second.release != nil {
		t.Fatal("locked contender unexpectedly received a release capability")
	}
	if err := first.release(); err != nil {
		t.Fatal(err)
	}
	third, locked, err := acquireRunHostLock(t.TempDir(), aliasHost)
	if err != nil || locked {
		t.Fatalf("Host Lock after owned release: locked=%t err=%v", locked, err)
	}
	if err := third.release(); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredLocalWindowsHostLockRequiresExplicitlyInactiveOwnedSession(t *testing.T) {
	workRoot := filepath.Join(t.TempDir(), "shared-local-slot")
	host := mayaHostConfig{
		ID: "shared-local", Transport: "local", WorkRoot: workRoot,
		Broker: brokerConfig{Structured: true, Type: "gg-mayasessiond", StateDir: filepath.Join(workRoot, "sessiond"), Python: `C:\python.exe`, Repo: `C:\sessiond`, MCPSource: `C:\mcp`, MayaExe: `C:\maya.exe`, Port: 7123},
	}
	previous := runLocalSessiondCommand
	t.Cleanup(func() { runLocalSessiondCommand = previous })
	active := true
	runLocalSessiondCommand = func(_ ggMayaSessiondBroker, args []string, _ time.Duration) ([]byte, error) {
		if args[0] != "status" {
			t.Fatalf("unexpected local Sessiond operation %q", args[0])
		}
		status := sessiondStatusResult{HasState: true, DerivedStatus: "running", ProcessAlive: map[string]bool{"daemon": true, "maya": true, "mcp": true}}
		status.State.Status = "running"
		status.State.SessionID = "owned-session"
		status.State.MayaAlive = true
		status.State.MCPAlive = true
		if !active {
			status.DerivedStatus = "stopped"
			status.State.Status = "stopped"
			status.State.MayaAlive = false
			status.State.MCPAlive = false
			status.ProcessAlive = map[string]bool{"daemon": false, "maya": false, "mcp": false}
		}
		return json.Marshal(status)
	}

	first, locked, err := acquireRunHostLock(t.TempDir(), host)
	if err != nil || locked {
		t.Fatalf("first Host Lock: locked=%t err=%v", locked, err)
	}
	if err := first.markActive("run-first"); err != nil {
		t.Fatal(err)
	}
	if err := first.bindSession("owned-session"); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(workRoot, "state", "locks", "hosts", "host.lock")
	content, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(content), "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "leaseExpiresAt:") {
			lines[index] = "leaseExpiresAt: " + time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano)
		}
	}
	if err := os.WriteFile(lockPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	_, locked, err = acquireRunHostLock(t.TempDir(), host)
	if err != nil || !locked {
		t.Fatalf("expired lock with active owned Session: locked=%t err=%v", locked, err)
	}
	active = false
	recovered, locked, err := acquireRunHostLock(t.TempDir(), host)
	if err != nil || locked {
		t.Fatalf("expired lock with explicitly stopped Session: locked=%t err=%v", locked, err)
	}
	if err := recovered.release(); err != nil {
		t.Fatal(err)
	}
}

func TestOptInLocalWindowsDesktopCaptureProducesPNG(t *testing.T) {
	if os.Getenv("MAYA_STALL_TEST_LOCAL_WINDOWS_CAPTURE") != "1" {
		t.Skip("set MAYA_STALL_TEST_LOCAL_WINDOWS_CAPTURE=1 on a logged-in Windows workstation")
	}
	data, err := captureLocalWindowsDesktopScreenshot(localWindowsDesktopTransport{workRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 8 || !bytes.Equal(data[:8], []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("desktop capture is not a nonempty PNG: bytes=%d prefix=%x", len(data), data[:min(len(data), 8)])
	}
}
