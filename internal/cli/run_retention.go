package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

type brokerCapabilities struct {
	RetainOnFailure          bool `json:"retainOnFailure"`
	StatusRetainedSession    bool `json:"statusRetainedSession"`
	AttachLogObservation     bool `json:"attachLogObservation"`
	StopRetainedSession      bool `json:"stopRetainedSession"`
	CleanupRetainedWorkspace bool `json:"cleanupRetainedWorkspace"`
}

type retainedSessionRecord struct {
	BrokerAdapter string         `json:"brokerAdapter"`
	SessionID     string         `json:"sessionId,omitempty"`
	Status        string         `json:"status,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

type retainedRunStatus struct {
	State           string `json:"state"`
	Detail          string `json:"detail,omitempty"`
	BrokerStatus    string `json:"brokerStatus,omitempty"`
	SessionID       string `json:"sessionId,omitempty"`
	RemoteWorkspace string `json:"remoteWorkspace,omitempty"`
}

type runRetentionRecord struct {
	RunID                 string                `json:"runId"`
	Scenario              string                `json:"scenario"`
	TargetProfile         string                `json:"targetProfile"`
	Host                  string                `json:"host"`
	Runtime               runtimeMetadata       `json:"runtime"`
	Status                string                `json:"status"`
	StopPhase             string                `json:"stopPhase,omitempty"`
	RetentionReason       string                `json:"retentionReason,omitempty"`
	LocalStateDir         string                `json:"localStateDir"`
	LocalEvidenceDir      string                `json:"localEvidenceDir"`
	LocalWorkspace        string                `json:"localWorkspace"`
	ScenarioResultPath    string                `json:"scenarioResultPath,omitempty"`
	RemoteRunRoot         string                `json:"remoteRunRoot,omitempty"`
	RemoteWorkspace       string                `json:"remoteWorkspace,omitempty"`
	HostConfig            mayaHostConfig        `json:"hostConfig"`
	HostLockAuthoritative bool                  `json:"hostLockAuthoritative,omitempty"`
	BrokerCapabilities    brokerCapabilities    `json:"brokerCapabilities"`
	RemoteSession         retainedSessionRecord `json:"remoteSession"`
	KeepTTL               string                `json:"keepTTL,omitempty"`
	KeepDeadline          string                `json:"keepDeadline,omitempty"`
	HardDeadline          string                `json:"hardDeadline,omitempty"`
	CreatedAt             string                `json:"createdAt"`
	UpdatedAt             string                `json:"updatedAt"`
	LegacyMissingRecord   bool                  `json:"-"`
}

func newRunRetentionRecord(context runContext, manifest runManifest, host mayaHostConfig, status string, reason string, hardDeadline string, now time.Time) runRetentionRecord {
	timestamp := now.UTC().Format(time.RFC3339Nano)
	record := runRetentionRecord{
		RunID:                 manifest.RunID,
		Scenario:              manifest.Scenario,
		TargetProfile:         manifest.TargetProfile,
		Host:                  manifest.Host,
		Runtime:               manifest.Runtime,
		Status:                status,
		RetentionReason:       reason,
		LocalStateDir:         context.StateDir,
		LocalEvidenceDir:      context.EvidenceDir,
		LocalWorkspace:        context.Workspace,
		ScenarioResultPath:    context.ScenarioResultPath,
		RemoteRunRoot:         context.RunWorkspace.RemoteRunRoot(),
		RemoteWorkspace:       context.RunWorkspace.RemoteWorkspace(),
		HostConfig:            host,
		HostLockAuthoritative: hasAuthoritativeHostSideLock(host),
		CreatedAt:             timestamp,
		UpdatedAt:             timestamp,
		HardDeadline:          hardDeadline,
	}
	if isKeptRetentionStatus(status) {
		stampKeptSessionDeadline(&record, now, defaultKeepTTL)
	}
	return record
}

func writeRunRetentionRecord(context runContext, record runRetentionRecord) error {
	if isKeptRetentionStatus(record.Status) {
		stampKeptSessionHardDeadline(&record, time.Now())
		if record.KeepDeadline == "" {
			stampKeptSessionDeadline(&record, time.Now(), keepTTLFromRecord(record, defaultKeepTTL))
		}
	}
	record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return writeRunLedgerBytes(filepath.Join(context.StateDir, "run-record.json"), append(content, '\n'))
}

func isKeptRetentionStatus(status string) bool {
	return status == "kept" || status == "retained"
}

func stampKeptSessionDeadline(record *runRetentionRecord, now time.Time, keepTTL time.Duration) {
	if keepTTL <= 0 {
		keepTTL = defaultKeepTTL
	}
	stampKeptSessionHardDeadline(record, now)
	keep := now.UTC().Add(keepTTL)
	if hard, err := time.Parse(time.RFC3339Nano, record.HardDeadline); err == nil && keep.After(hard) {
		keep = hard
	}
	record.KeepTTL = keepTTL.String()
	record.KeepDeadline = keep.Format(time.RFC3339Nano)
}

func stampKeptSessionHardDeadline(record *runRetentionRecord, now time.Time) {
	if record.HardDeadline == "" {
		record.HardDeadline = now.UTC().Add(defaultHostLockHardLifetime).Format(time.RFC3339Nano)
	}
}

func keepTTLFromRecord(record runRetentionRecord, fallback time.Duration) time.Duration {
	if keepTTL, err := time.ParseDuration(record.KeepTTL); err == nil && keepTTL > 0 {
		return keepTTL
	}
	if fallback > 0 {
		return fallback
	}
	return defaultKeepTTL
}

func fallbackRunRetentionRecord(repoDir string, stateDir string, manifest runManifest) runRetentionRecord {
	workspace, _ := newRunWorkspace(repoDir, manifest.RunID, "", "")
	return runRetentionRecord{
		RunID:            manifest.RunID,
		Scenario:         manifest.Scenario,
		TargetProfile:    manifest.TargetProfile,
		Host:             manifest.Host,
		Runtime:          manifest.Runtime,
		Status:           "kept",
		LocalStateDir:    stateDir,
		LocalEvidenceDir: filepath.Join(repoDir, "artifacts", "maya-stall", manifest.RunID),
		LocalWorkspace:   filepath.Join(stateDir, "workspace"),
		HostConfig:       mayaHostConfig{ID: manifest.Host},
		RemoteSession: retainedSessionRecord{
			BrokerAdapter: manifest.Runtime.BrokerAdapter,
			Status:        "unknown",
		},
		LegacyMissingRecord: true,
		BrokerCapabilities: brokerCapabilities{
			RetainOnFailure:          manifest.Runtime.BrokerAdapter == "fake",
			StatusRetainedSession:    manifest.Runtime.BrokerAdapter == "fake",
			AttachLogObservation:     manifest.Runtime.BrokerAdapter == "fake",
			StopRetainedSession:      manifest.Runtime.BrokerAdapter == "fake",
			CleanupRetainedWorkspace: manifest.Runtime.BrokerAdapter == "fake",
		},
		RemoteRunRoot:   workspace.RemoteRunRoot(),
		RemoteWorkspace: workspace.RemoteWorkspace(),
	}
}

func retentionBrokerForRecord(record runRetentionRecord) (runRetentionBroker, error) {
	switch record.Runtime.BrokerAdapter {
	case "", "fake":
		return fakeSessionBroker{}, nil
	case "gg-mayasessiond":
		return ggMayaSessiondBroker{host: record.HostConfig}, nil
	default:
		return nil, unsupportedBrokerCapabilityError(record.Runtime.BrokerAdapter, "run-retention")
	}
}

func unsupportedBrokerCapabilityError(adapter string, capability string) error {
	if adapter == "" {
		adapter = "unknown"
	}
	return fmt.Errorf("Session Broker %q does not support %s for kept sessions; see docs/adr/0033-manage-run-retention-on-owned-hosts.md and docs/setup/windows-maya-host.md#host-lock-and-retention for cleanup guidance", adapter, capability)
}

func requireRetentionCapability(broker runRetentionBroker, adapter string, capability string, supported bool) error {
	if broker == nil || !supported {
		return unsupportedBrokerCapabilityError(adapter, capability)
	}
	return nil
}

func attachLocalRunFiles(run keptRun, stdout io.Writer) error {
	fmt.Fprintln(stdout, "events:")
	if err := copyRunStateTextFile(run.StateDir, "events.jsonl", stdout); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "logs:")
	if err := copyRunStateTextFile(run.StateDir, filepath.Join("logs", "session.log"), stdout); err != nil {
		return err
	}
	if run.Record.ScenarioResultPath != "" {
		relativeResult, err := filepath.Rel(run.StateDir, run.Record.ScenarioResultPath)
		if err != nil || filepath.IsAbs(relativeResult) || strings.HasPrefix(filepath.ToSlash(relativeResult), "../") || filepath.ToSlash(relativeResult) == ".." || !strings.HasPrefix(filepath.ToSlash(relativeResult), "workspace/") {
			return fmt.Errorf("Scenario Result path for run %s must stay under kept run workspace", run.RunID)
		}
		fmt.Fprintln(stdout, "scenarioResult:")
		if err := copyRunStateTextFile(run.StateDir, relativeResult, stdout); err != nil {
			return err
		}
	}
	if run.Record.LocalEvidenceDir != "" {
		fmt.Fprintf(stdout, "evidence: %s\n", run.Record.LocalEvidenceDir)
	}
	return nil
}
