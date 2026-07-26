package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHostLockDeadlinesExpireActiveRunsByIdleAndHardLifetime(t *testing.T) {
	start := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	policy := hostLockDeadlinePolicy{
		IdleTimeout:  30 * time.Minute,
		HardLifetime: 2 * time.Hour,
	}

	tests := []struct {
		name       string
		heartbeats []time.Duration
		observe    time.Duration
		wantReason string
	}{
		{name: "idle", observe: 30 * time.Minute, wantReason: "idle"},
		{
			name:       "hard lifetime despite heartbeat",
			heartbeats: []time.Duration{20 * time.Minute, 40 * time.Minute, 60 * time.Minute, 80 * time.Minute, 100 * time.Minute, 119 * time.Minute},
			observe:    2 * time.Hour,
			wantReason: "hard-lifetime",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deadlines := newHostLockDeadlines(start, policy)
			for _, heartbeat := range test.heartbeats {
				if err := deadlines.recordHeartbeat(start.Add(heartbeat), policy); err != nil {
					t.Fatalf("record heartbeat: %v", err)
				}
			}
			if reason := deadlines.expiryReason(start.Add(test.observe)); reason != test.wantReason {
				t.Fatalf("expiry reason = %q, want %q", reason, test.wantReason)
			}
		})
	}
}

func TestHostLockDeadlinesRecordHeartbeatAndLostAgent(t *testing.T) {
	start := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	policy := hostLockDeadlinePolicy{IdleTimeout: 30 * time.Minute, HardLifetime: 4 * time.Hour}
	deadlines := newHostLockDeadlines(start, policy)

	heartbeatAt := start.Add(20 * time.Minute)
	if err := deadlines.recordHeartbeat(heartbeatAt, policy); err != nil {
		t.Fatalf("record heartbeat: %v", err)
	}
	if reason := deadlines.expiryReason(start.Add(49 * time.Minute)); reason != "" {
		t.Fatalf("live Agent expiry reason = %q, want none", reason)
	}
	if reason := deadlines.expiryReason(start.Add(50 * time.Minute)); reason != "idle" {
		t.Fatalf("lost Agent expiry reason = %q, want idle", reason)
	}
	expired := deadlines
	if err := expired.recordHeartbeat(start.Add(50*time.Minute), policy); err == nil || !strings.Contains(err.Error(), "idle deadline") {
		t.Fatalf("expired idle heartbeat error = %v", err)
	}
	if expired != deadlines {
		t.Fatalf("expired heartbeat changed deadlines from %+v to %+v", deadlines, expired)
	}
}

func TestHostLockDeadlinesKeepAndExtendWithinPolicy(t *testing.T) {
	start := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	policy := hostLockDeadlinePolicy{IdleTimeout: 30 * time.Minute, HardLifetime: 3 * time.Hour}
	deadlines := newHostLockDeadlines(start, policy)

	keptAt := start.Add(10 * time.Minute)
	if err := deadlines.markKept(keptAt, 90*time.Minute, policy); err != nil {
		t.Fatalf("mark kept: %v", err)
	}
	if got, want := deadlines.KeepDeadline, keptAt.Add(90*time.Minute).Format(time.RFC3339Nano); got != want {
		t.Fatalf("keep deadline = %s, want %s", got, want)
	}
	extendedAt := start.Add(time.Hour)
	idleBeforeExtension := deadlines.IdleDeadline
	if err := deadlines.extendKept(extendedAt, 45*time.Minute, policy); err != nil {
		t.Fatalf("extend kept: %v", err)
	}
	if got, want := deadlines.KeepDeadline, keptAt.Add(135*time.Minute).Format(time.RFC3339Nano); got != want {
		t.Fatalf("extended deadline = %s, want %s", got, want)
	}
	if got := deadlines.ExtensionCount; got != 1 {
		t.Fatalf("extension count = %d, want 1", got)
	}
	if deadlines.IdleDeadline != idleBeforeExtension {
		t.Fatalf("extension renewed idle deadline from %s to %s", idleBeforeExtension, deadlines.IdleDeadline)
	}
}

func TestHostLockDeadlinesRejectExtensionBeyondHardLifetime(t *testing.T) {
	start := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	policy := hostLockDeadlinePolicy{IdleTimeout: 30 * time.Minute, HardLifetime: 2 * time.Hour}
	deadlines := newHostLockDeadlines(start, policy)
	if err := deadlines.markKept(start, 90*time.Minute, policy); err != nil {
		t.Fatalf("mark kept: %v", err)
	}

	err := deadlines.extendKept(start.Add(time.Hour), 31*time.Minute, policy)
	if err == nil || !strings.Contains(err.Error(), "hard deadline") {
		t.Fatalf("extension error = %v, want hard deadline rejection", err)
	}
}

func TestHostAgentActionResponsePreservesActiveExpiryOrigin(t *testing.T) {
	now := time.Date(2026, 7, 23, 9, 30, 0, 0, time.UTC)
	response := hostAgentActionResponse(hostAgentAssignmentRecord{
		RunID: "20260723T090000.000000000Z", State: "expiring", ExpiryFromState: "confirmed", ExpiryReason: "hard-lifetime",
	}, now, "cleanup", "hard-lifetime")
	if response.Action != "cleanup" || response.ExpiryFromState != "confirmed" || response.ExpiryReason != "hard-lifetime" {
		t.Fatalf("active expiry action = %+v", response)
	}
}

func TestExpiredHostLockCleanupObservesStoppedSessionAndRefusesForeignSession(t *testing.T) {
	start := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	policy := hostLockDeadlinePolicy{IdleTimeout: 5 * time.Minute, HardLifetime: time.Hour}
	deadlines := newHostLockDeadlines(start, policy)
	now := start.Add(policy.IdleTimeout)
	if reason := deadlines.expiryReason(now); reason != "idle" {
		t.Fatalf("expiry reason = %q, want idle", reason)
	}

	tests := []struct {
		name      string
		status    string
		wantEvent string
		wantError string
	}{
		{
			name:      "already stopped exact session",
			status:    `{"has_state":true,"derived_status":"stopped","state":{"status":"stopped","session_id":"session-owned","maya_alive":false,"mcp_alive":false}}`,
			wantEvent: `"type":"broker.session.already-stopped"`,
		},
		{
			name:      "foreign active session",
			status:    `{"has_state":true,"derived_status":"running","state":{"status":"running","session_id":"session-foreign","maya_alive":true,"mcp_alive":true}}`,
			wantError: "does not own",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			sshPath := writeSequencedFakeSSHCommand(t, dir, filepath.Join(dir, "ssh.log"), []string{test.status})
			eventsPath := filepath.Join(dir, "events.jsonl")
			broker := ggMayaSessiondBroker{host: mayaHostConfig{
				Transport: "ssh",
				SSH:       sshConfig{Host: "maya-win-01", Binary: sshPath},
				WorkRoot:  "C:/maya-stall",
				Broker: brokerConfig{
					Type: "gg-mayasessiond", StateDir: "C:/maya-stall/sessiond-ui",
					Python: "C:/maya-stall/sessiond-venv311/Scripts/python.exe", Repo: "C:/maya-stall/tools/GG_MayaSessiond",
				},
			}}

			err := broker.StopSession(runContext{EventsPath: eventsPath}, brokerSessionIdentity{
				BrokerAdapter: "gg-mayasessiond", SessionID: "session-owned",
			})
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("expired cleanup error = %v, want containing %q", err, test.wantError)
				}
				if _, statErr := os.Stat(eventsPath); !os.IsNotExist(statErr) {
					t.Fatalf("foreign session cleanup wrote events: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("expired cleanup: %v", err)
			}
			events, err := os.ReadFile(eventsPath)
			if err != nil {
				t.Fatalf("read already-stopped event: %v", err)
			}
			if !strings.Contains(string(events), test.wantEvent) {
				t.Fatalf("already-stopped event missing %s:\n%s", test.wantEvent, events)
			}
		})
	}
}

func TestHostLockDeadlineEventsAreIdempotentAcrossCompletionRetry(t *testing.T) {
	repoDir := t.TempDir()
	runID := "20260723T090000.000000000Z"
	now := time.Date(2026, 7, 23, 9, 30, 0, 0, time.UTC)
	record := runLedgerRecord{
		Version: runLedgerSchemaVersion, RunID: runID, Scenario: "smoke", State: "failed",
		AcceptedAt: now.Add(-time.Hour).Format(time.RFC3339Nano), UpdatedAt: now.Format(time.RFC3339Nano),
		Events: runLedgerEventsFileName, Log: runLedgerLogPath,
		EvidenceDir: filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)),
	}
	if err := writeRunLedgerRecord(repoDir, record); err != nil {
		t.Fatalf("write Run Ledger: %v", err)
	}
	eventsPath := filepath.Join(runLedgerDir(repoDir, runID), runLedgerEventsFileName)
	if err := writeRunLedgerBytes(eventsPath, []byte("{\"sequence\":1,\"type\":\"run.accepted\"}\n")); err != nil {
		t.Fatalf("write Run Ledger events: %v", err)
	}
	if err := writeRunLedgerBytes(filepath.Join(runLedgerDir(repoDir, runID), filepath.FromSlash(runLedgerLogPath)), nil); err != nil {
		t.Fatalf("write Run Ledger log: %v", err)
	}
	events := []hostLockDeadlineEvent{
		{Type: "kept-session.extended", Detail: "30m0s", Timestamp: now.Format(time.RFC3339Nano), Ordinal: 1},
		{Type: "kept-session.extended", Detail: "30m0s", Timestamp: now.Format(time.RFC3339Nano), Ordinal: 2},
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := appendHostLockDeadlineEventsToLedger(repoDir, &record, events, now); err != nil {
			t.Fatalf("append deadline event attempt %d: %v", attempt+1, err)
		}
	}
	content, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read Run Ledger events: %v", err)
	}
	if got := bytes.Count(content, []byte(`"deadlineEventId"`)); got != 2 {
		t.Fatalf("deadline event copies = %d, want 2:\n%s", got, content)
	}
}

func TestHostAgentAssignmentCompletionRequiresConfirmation(t *testing.T) {
	tests := []struct {
		name     string
		record   hostAgentAssignmentRecord
		terminal runCommandJSON
		want     bool
	}{
		{name: "confirmed", record: hostAgentAssignmentRecord{State: "confirmed"}, want: true},
		{
			name: "expired confirmed recovery", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "confirmed"},
			terminal: runCommandJSON{Status: resultStatusFailed, Diagnostic: "active Host Lock deadline expired before completion"}, want: true,
		},
		{name: "expired confirmed stopped-success cleanup proof", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "confirmed"}, terminal: runCommandJSON{Status: resultStatusPassed}, want: true},
		{name: "expired kept", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "kept"}, want: true},
		{name: "expired unconfirmed", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "assigned"}},
		{name: "assigned", record: hostAgentAssignmentRecord{State: "assigned"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hostAgentAssignmentAcceptsCompletion(test.record, test.terminal); got != test.want {
				t.Fatalf("completion accepted = %t, want %t", got, test.want)
			}
		})
	}
}

func TestHostAgentFailureCannotReleaseUncleanedExpiredSession(t *testing.T) {
	session := &brokerSessionIdentity{BrokerAdapter: "fake", SessionID: "maya-session-1"}
	tests := []struct {
		name       string
		record     hostAgentAssignmentRecord
		quarantine bool
		want       bool
	}{
		{name: "confirmed", record: hostAgentAssignmentRecord{State: "confirmed"}, want: true},
		{name: "expired assigned", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "assigned"}, want: true},
		{name: "expired confirmed before session", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "confirmed"}, want: true},
		{name: "expired confirmed bound session", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "confirmed", BrokerSession: session}},
		{name: "expired kept", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "kept", BrokerSession: session}},
		{name: "expired kept quarantine", record: hostAgentAssignmentRecord{State: "expiring", ExpiryFromState: "kept", BrokerSession: session}, quarantine: true, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hostAgentAssignmentAcceptsFailure(test.record, test.quarantine); got != test.want {
				t.Fatalf("failure accepted = %t, want %t", got, test.want)
			}
		})
	}
}

func TestHostLockExpiryPersistenceFailureIsFailClosed(t *testing.T) {
	dataDir := privateTempDir(t)
	start := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	now := start.Add(defaultHostLockIdleTimeout)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	handlerValue, err := newControlPlaneHandler(dataDir, "operator-token", runtime)
	if err != nil {
		t.Fatalf("create Control Plane: %v", err)
	}
	handler := handlerValue.(*controlPlaneHandler)
	runID := "20260723T090000.000000000Z"
	handler.assignments[runID] = &controlPlaneHostAgentAssignment{
		record: hostAgentAssignmentRecord{
			Version: hostAgentAPIVersion, RunID: runID, AgentID: "windows-agent-01", HostID: "maya-win-01",
			LockToken: strings.Repeat("a", 32), State: "assigned",
			hostLockDeadlines: newHostLockDeadlines(start, defaultHostLockDeadlinePolicy()),
		},
		done: make(chan struct{}),
	}
	transactions := filepath.Join(dataDir, "assignment-transactions")
	if err := os.RemoveAll(transactions); err != nil {
		t.Fatalf("remove transaction directory: %v", err)
	}
	if err := os.WriteFile(transactions, []byte("blocked"), 0o600); err != nil {
		t.Fatalf("block transaction directory: %v", err)
	}

	if err := handler.observeHostLockDeadlines(); err == nil {
		t.Fatal("expiry persistence failure was ignored")
	}
	if got := handler.assignments[runID].record.State; got != "assigned" {
		t.Fatalf("failed expiry transition state = %q, want assigned rollback", got)
	}
}
