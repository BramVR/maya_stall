package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type statusOptions struct {
	RunID                string
	JSON                 bool
	ControlPlane         string
	ControlPlaneTokenEnv string
}

type stopOptions struct {
	RunID                string
	ControlPlane         string
	ControlPlaneTokenEnv string
}

type extendOptions struct {
	RunID                string
	By                   time.Duration
	ControlPlane         string
	ControlPlaneTokenEnv string
}

type keptRun struct {
	RunID        string
	StateDir     string
	Manifest     runManifest
	Record       runRetentionRecord
	RemoteStatus retainedRunStatus
	Bundle       evidenceBundle
}

func parseStatusArgs(args []string) (statusOptions, error) {
	var options statusOptions
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--run":
			i++
			if i >= len(args) || args[i] == "" {
				return statusOptions{}, newUsageError("--run needs a run id")
			}
			if err := validateRunID(args[i]); err != nil {
				return statusOptions{}, err
			}
			options.RunID = args[i]
		case "--json":
			options.JSON = true
		case "--control-plane":
			i++
			if i >= len(args) || args[i] == "" || strings.HasPrefix(args[i], "--") {
				return statusOptions{}, newUsageError("--control-plane needs an HTTPS URL")
			}
			options.ControlPlane = args[i]
		case "--control-plane-token-env":
			i++
			if i >= len(args) || args[i] == "" || strings.HasPrefix(args[i], "--") {
				return statusOptions{}, newUsageError("--control-plane-token-env needs an environment variable name")
			}
			options.ControlPlaneTokenEnv = args[i]
		default:
			return statusOptions{}, newUsageError("unknown status option %q", args[i])
		}
	}
	return options, nil
}

func parseRunIDArg(command string, args []string) (string, error) {
	if len(args) != 1 {
		return "", newUsageError("%s needs one run id", command)
	}
	if err := validateRunID(args[0]); err != nil {
		return "", err
	}
	return args[0], nil
}

func parseStopArgs(args []string) (stopOptions, error) {
	var options stopOptions
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--control-plane":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return stopOptions{}, newUsageError("--control-plane needs an HTTPS URL")
			}
			options.ControlPlane = args[index]
		case "--control-plane-token-env":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return stopOptions{}, newUsageError("--control-plane-token-env needs an environment variable name")
			}
			options.ControlPlaneTokenEnv = args[index]
		default:
			if options.RunID != "" {
				return stopOptions{}, newUsageError("stop needs one run id")
			}
			if err := validateRunID(args[index]); err != nil {
				return stopOptions{}, err
			}
			options.RunID = args[index]
		}
	}
	if options.RunID == "" {
		return stopOptions{}, newUsageError("stop needs one run id")
	}
	if options.ControlPlane == "" && options.ControlPlaneTokenEnv != "" {
		return stopOptions{}, newUsageError("--control-plane-token-env requires --control-plane")
	}
	return options, nil
}

func parseExtendArgs(args []string) (extendOptions, error) {
	var options extendOptions
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--by":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return extendOptions{}, newUsageError("--by needs a duration")
			}
			by, err := time.ParseDuration(args[index])
			if err != nil || by <= 0 {
				return extendOptions{}, newUsageError("--by needs a positive duration")
			}
			options.By = by
		case "--control-plane":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return extendOptions{}, newUsageError("--control-plane needs an HTTPS URL")
			}
			options.ControlPlane = args[index]
		case "--control-plane-token-env":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return extendOptions{}, newUsageError("--control-plane-token-env needs an environment variable name")
			}
			options.ControlPlaneTokenEnv = args[index]
		default:
			if options.RunID != "" {
				return extendOptions{}, newUsageError("extend needs one run id")
			}
			if err := validateRunID(args[index]); err != nil {
				return extendOptions{}, err
			}
			options.RunID = args[index]
		}
	}
	if options.RunID == "" {
		return extendOptions{}, newUsageError("extend needs one run id")
	}
	if options.By <= 0 {
		return extendOptions{}, newUsageError("extend needs --by <duration>")
	}
	if options.ControlPlane == "" && options.ControlPlaneTokenEnv != "" {
		return extendOptions{}, newUsageError("--control-plane-token-env requires --control-plane")
	}
	return options, nil
}

func extendRunThroughMode(repoDir string, options extendOptions, runtime runRuntime) (string, error) {
	now := time.Now
	if runtime.Now != nil {
		now = runtime.Now
	}
	if options.ControlPlane != "" {
		var status controlPlaneStatusResponse
		request := controlPlaneKeepExtensionRequest{Version: controlPlaneAPIVersion, By: options.By.String()}
		if err := postControlPlaneJSON(options.ControlPlane, options.ControlPlaneTokenEnv, "/v1/runs/"+options.RunID+"/extend", request, runtime, http.StatusOK, &status); err != nil {
			return "", err
		}
		if status.Version != controlPlaneAPIVersion || status.Kind != "status" || status.RunID != options.RunID || status.KeepDeadline == "" {
			return "", errors.New("Control Plane did not confirm Kept Session extension") //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
		}
		return status.KeepDeadline, nil
	}
	var deadline string
	err := withRunRetentionMutationLock(repoDir, options.RunID, func() error {
		var extendErr error
		deadline, extendErr = extendEmbeddedKeptSession(repoDir, options, now)
		return extendErr
	})
	return deadline, err
}

func extendEmbeddedKeptSession(repoDir string, options extendOptions, now func() time.Time) (string, error) {
	run, err := readKeptRunState(repoDir, options.RunID)
	if err != nil {
		return "", err
	}
	if run.Record.KeepDeadline == "" {
		return "", fmt.Errorf("Kept Session %s has no deadline", options.RunID) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	keep, err := time.Parse(time.RFC3339Nano, run.Record.KeepDeadline)
	if err != nil || !now().Before(keep) {
		return "", fmt.Errorf("Kept Session %s has expired", options.RunID) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if run.Record.HardDeadline == "" {
		stampKeptSessionHardDeadline(&run.Record, now())
		hard, parseErr := time.Parse(time.RFC3339Nano, run.Record.HardDeadline)
		if parseErr != nil {
			return "", fmt.Errorf("Kept Session %s has invalid Host Lock hard deadline", options.RunID) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		if keep.After(hard) {
			keep = hard
			run.Record.KeepDeadline = hard.UTC().Format(time.RFC3339Nano)
		}
		if err := writeRunRetentionRecord(runContext{StateDir: run.StateDir}, run.Record); err != nil {
			return "", err
		}
	}
	hard, err := time.Parse(time.RFC3339Nano, run.Record.HardDeadline)
	if err != nil {
		return "", fmt.Errorf("Kept Session %s has invalid Host Lock hard deadline", options.RunID) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	extended := keep.Add(options.By)
	if extended.After(hard) {
		return "", fmt.Errorf("Kept Session extension exceeds Host Lock hard deadline %s", hard.UTC().Format(time.RFC3339Nano)) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	previousRecord := run.Record
	run.Record.KeepDeadline = extended.UTC().Format(time.RFC3339Nano)
	if err := writeRunRetentionRecord(runContext{StateDir: run.StateDir}, run.Record); err != nil {
		return "", err
	}
	if err := appendKeptSessionSweepEvent(repoDir, run, now(), map[string]string{
		"event": "kept-session-extended", "runId": options.RunID, "deadline": run.Record.KeepDeadline, "extension": options.By.String(),
	}); err != nil {
		rollbackErr := writeRunRetentionRecord(runContext{StateDir: run.StateDir}, previousRecord)
		return "", errors.Join(err, rollbackErr)
	}
	return run.Record.KeepDeadline, nil
}

func withRunRetentionMutationLock(repoDir string, runID string, action func() error) error {
	if err := validateRunID(runID); err != nil {
		return err
	}
	return withRunRetentionLock(repoDir, action)
}

func withRunRetentionLock(repoDir string, action func() error) error {
	relativeRoot := filepath.Join(".maya-stall", "state", "retention-locks")
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, relativeRoot); err != nil {
		return err
	}
	root := filepath.Join(repoDir, relativeRoot)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	return withRunLedgerFileLock(filepath.Join(root, ".mutation.lock"), true, action)
}

func stopRunThroughMode(repoDir string, options stopOptions, runtime runRuntime) (string, error) {
	if options.ControlPlane != "" {
		var status controlPlaneStatusResponse
		if err := postControlPlaneJSON(options.ControlPlane, options.ControlPlaneTokenEnv, "/v1/runs/"+options.RunID+"/cancel", struct {
			Version int `json:"version"`
		}{Version: controlPlaneAPIVersion}, runtime, http.StatusOK, &status); err != nil {
			return "", err
		}
		if status.Version != controlPlaneAPIVersion || status.Kind != "status" || status.RunID != options.RunID || status.State != "canceled" && status.State != "expiring" {
			return "", errors.New("Control Plane did not confirm Run stop request") //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
		}
		if status.State == "expiring" {
			return "stop-requested", nil
		}
		return "stopped", nil
	}
	now := time.Now
	if runtime.Now != nil {
		now = runtime.Now
	}
	if err := withRunRetentionMutationLock(repoDir, options.RunID, func() error {
		return stopRun(repoDir, options.RunID, now)
	}); err != nil {
		return "", err
	}
	return "stopped", nil
}

func validateRunID(runID string) error {
	if runID == "" {
		return newUsageError("run id must not be empty")
	}
	if runID == "." || runID == ".." || filepath.Clean(runID) != runID {
		return newUsageError("run id %q must name one run directory", runID)
	}
	if !hostIDPattern.MatchString(runID) {
		return newUsageError("run id %q must contain only letters, numbers, dots, underscores, or dashes", runID)
	}
	return nil
}

func printStatus(repoDir string, options statusOptions, stdout io.Writer) error {
	if options.RunID != "" {
		ledgerRecord, ledgerErr := newRunLedgerStore(repoDir).Read(options.RunID)
		if ledgerErr == nil {
			retained, err := runLedgerUsesRetainedState(repoDir, ledgerRecord)
			if err != nil {
				return err
			}
			if ledgerRecord.State != "kept" && !retained {
				return printLedgerRunStatus(repoDir, options.RunID, stdout)
			}
		}
		var ledgerUsageErr *usageError
		if ledgerErr != nil && !errors.As(ledgerErr, &ledgerUsageErr) {
			return ledgerErr
		}
		stateManifest := filepath.Join(repoDir, ".maya-stall", "state", "runs", options.RunID, evidenceManifestFileName)
		if _, err := os.Lstat(stateManifest); errors.Is(err, os.ErrNotExist) {
			if ledgerErr == nil && ledgerRecord.State == "kept" {
				return fmt.Errorf("kept run %q is missing transient Run State; refusing unverified ledger-only status", options.RunID)
			}
			return printLedgerRunStatus(repoDir, options.RunID, stdout)
		} else if err != nil {
			return err
		}
		run, err := readKeptRunState(repoDir, options.RunID)
		if err != nil {
			return err
		}
		if run.Record.Status == "running" && run.Record.StopPhase == "" {
			if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join("artifacts", "maya-stall", options.RunID)); err != nil {
				return err
			}
			evidencePath := filepath.Join(repoDir, "artifacts", "maya-stall", options.RunID, evidenceBundleFileName)
			if evidenceBytes, evidenceErr := os.ReadFile(evidencePath); evidenceErr == nil {
				if err := json.Unmarshal(evidenceBytes, &run.Bundle); err != nil {
					return fmt.Errorf("parse run evidence: %w", err)
				}
			} else if !errors.Is(evidenceErr, os.ErrNotExist) {
				return evidenceErr
			}
			run.Bundle.Status = run.Record.Status
		} else {
			run, err = readKeptRun(repoDir, options.RunID)
			if err != nil {
				return err
			}
		}
		if err := refreshKeptRunStatus(&run); err != nil {
			return err
		}
		printKeptRunStatus(stdout, run)
		return nil
	}
	runs, err := listKeptRuns(repoDir)
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "state: no kept sessions")
		return nil
	}
	for _, run := range runs {
		printKeptRunStatus(stdout, run)
	}
	return nil
}

func printKeptRunStatus(stdout io.Writer, run keptRun) {
	fmt.Fprintf(stdout, "run: %s\n", run.RunID)
	state := run.RemoteStatus.State
	if state == "" {
		state = "kept"
	}
	fmt.Fprintf(stdout, "state: %s\n", state)
	fmt.Fprintf(stdout, "scenario: %s\n", run.Manifest.Scenario)
	fmt.Fprintf(stdout, "targetProfile: %s\n", run.Manifest.TargetProfile)
	fmt.Fprintf(stdout, "host: %s\n", run.Manifest.Host)
	if run.Manifest.Runtime.Profile != "" {
		fmt.Fprintf(stdout, "runtime: %s\n", run.Manifest.Runtime.Profile)
		fmt.Fprintf(stdout, "hostAdapter: %s\n", run.Manifest.Runtime.HostAdapter)
		fmt.Fprintf(stdout, "brokerAdapter: %s\n", run.Manifest.Runtime.BrokerAdapter)
		fmt.Fprintf(stdout, "liveProofEligible: %t\n", run.Manifest.Runtime.LiveProofEligible)
	}
	fmt.Fprintf(stdout, "status: %s\n", run.Bundle.Status)
	if run.Record.RetentionReason != "" {
		fmt.Fprintf(stdout, "retentionReason: %s\n", run.Record.RetentionReason)
	}
	keepDeadline, keepRemaining := keptSessionTTLStatus(run.Record, time.Now())
	_, _ = fmt.Fprintf(stdout, "keepDeadline: %s\n", keepDeadline)
	_, _ = fmt.Fprintf(stdout, "keepRemaining: %s\n", keepRemaining)
	if run.RemoteStatus.BrokerStatus != "" {
		fmt.Fprintf(stdout, "remoteState: %s\n", run.RemoteStatus.BrokerStatus)
	}
	if run.RemoteStatus.SessionID != "" {
		fmt.Fprintf(stdout, "brokerSession: %s\n", run.RemoteStatus.SessionID)
	}
	if run.RemoteStatus.RemoteWorkspace != "" {
		fmt.Fprintf(stdout, "remoteWorkspace: %s\n", run.RemoteStatus.RemoteWorkspace)
	}
	if run.RemoteStatus.Detail != "" {
		fmt.Fprintf(stdout, "detail: %s\n", run.RemoteStatus.Detail)
	}
	fmt.Fprintf(stdout, "stateDir: %s\n", run.StateDir)
}

func keptSessionTTLStatus(record runRetentionRecord, now time.Time) (string, string) {
	if record.KeepDeadline == "" {
		return "unstamped", "unstamped"
	}
	deadline, err := time.Parse(time.RFC3339Nano, record.KeepDeadline)
	if err != nil {
		return record.KeepDeadline, "invalid"
	}
	remaining := deadline.Sub(now)
	if remaining <= 0 {
		return record.KeepDeadline, "expired"
	}
	if remaining < time.Minute {
		return record.KeepDeadline, "<1m left"
	}
	rounded := remaining.Round(time.Minute).String()
	rounded = strings.TrimSuffix(rounded, "0s")
	return record.KeepDeadline, rounded + " left"
}

func attachRun(repoDir string, runID string, stdout io.Writer) error {
	if err := validateRunID(runID); err != nil {
		return err
	}
	ledgerRecord, ledgerErr := newRunLedgerStore(repoDir).Read(runID)
	if ledgerErr == nil {
		retained, err := runLedgerUsesRetainedState(repoDir, ledgerRecord)
		if err != nil {
			return err
		}
		if ledgerRecord.State != "kept" && !retained {
			return attachLedgerRun(repoDir, runID, stdout)
		}
	}
	var ledgerUsageErr *usageError
	if ledgerErr != nil && !errors.As(ledgerErr, &ledgerUsageErr) {
		return ledgerErr
	}
	stateManifest := filepath.Join(repoDir, ".maya-stall", "state", "runs", runID, evidenceManifestFileName)
	if _, err := os.Lstat(stateManifest); errors.Is(err, os.ErrNotExist) {
		if ledgerErr == nil && ledgerRecord.State == "kept" {
			return fmt.Errorf("kept run %q is missing transient Run State; refusing unverified ledger-only attach", runID)
		}
		return attachLedgerRun(repoDir, runID, stdout)
	} else if err != nil {
		return err
	}
	run, err := readKeptRunState(repoDir, runID)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "run: %s\n", run.RunID)
	if err := attachLocalRunFiles(run, stdout); err != nil {
		return err
	}
	broker, err := retentionBrokerForRecord(run.Record)
	if err != nil {
		return err
	}
	capabilities := broker.RetentionCapabilities()
	if err := requireRetentionCapability(broker, run.Manifest.Runtime.BrokerAdapter, "attach/log observation", capabilities.AttachLogObservation); err != nil {
		return err
	}
	return broker.AttachRetainedRun(run.Record, stdout)
}

func runLedgerUsesRetainedState(repoDir string, record runLedgerRecord) (bool, error) {
	if record.StopPhase == "host-lock-released" {
		return false, nil
	}
	if record.State == "kept" {
		return true, nil
	}
	if record.State != "submitted" && record.State != "cleanup-failed" {
		return false, nil
	}
	return isRetainedRunRecord(repoDir, record.RunID)
}

func isRetainedRunRecord(repoDir string, runID string) (bool, error) {
	path := filepath.Join(repoDir, ".maya-stall", "state", "runs", runID, "run-record.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("retained Run Record %s must be a regular file", path)
	}
	content, err := readRunLedgerBytes(path)
	if err != nil {
		return false, err
	}
	var record runRetentionRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return false, fmt.Errorf("parse retained Run Record: %w", err)
	}
	if record.StopPhase != "" {
		return false, nil
	}
	return record.Status == "kept" && record.RetentionReason != "" || record.Status == "running" && record.RemoteSession.SessionID != "", nil
}

func stopRun(repoDir string, runID string, now func() time.Time) error {
	if now == nil {
		now = time.Now
	}
	if err := prepareRunLedgerForStop(repoDir, runID, now()); err != nil {
		return err
	}
	stopErr := stopKeptRun(repoDir, runID, now())
	ledgerErr := updateRunLedgerAfterStop(repoDir, runID, stopErr, now())
	return errors.Join(stopErr, ledgerErr)
}

func stopKeptRun(repoDir string, runID string, now time.Time) error {
	return stopKeptRunAfterBrokerStop(repoDir, runID, now, nil)
}

func stopKeptRunAfterBrokerStop(repoDir string, runID string, now time.Time, afterBrokerStop func(string)) error {
	manifest, stateDir, found, err := readStopRunManifest(repoDir, runID)
	if err != nil {
		return err
	}
	if !found {
		record, ledgerErr := newRunLedgerStore(repoDir).Read(runID)
		if ledgerErr == nil && record.StopPhase == "host-lock-released" {
			return cleanupRunState(repoDir, runID)
		}
		return releaseMatchingKeptHostLock(repoDir, runID)
	}
	if err := validateHostID(manifest.Host); err != nil {
		return err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return err
	}
	record, err := readRunRetentionRecord(repoDir, stateDir, manifest)
	if err != nil {
		return err
	}
	ledgerRecord, ledgerErr := newRunLedgerStore(repoDir).Read(runID)
	if ledgerErr == nil && record.StopPhase == "" {
		switch ledgerRecord.StopPhase {
		case "session-stopped", "broker-cleaned", "host-lock-released":
			record.StopPhase = ledgerRecord.StopPhase
		}
	}
	if record.StopPhase == "host-lock-released" {
		return finishReleasedStopRun(repoDir, stateDir, manifest.Host, runID, record, now)
	}
	cleanupFailedRunning := ledgerErr == nil && ledgerRecord.State == "cleanup-failed" && record.Status == "running" && record.StopPhase == ""
	activeCleanupRecovery := false
	if cleanupFailedRunning {
		activeCleanupRecovery, err = repoHostLockUsesActiveRun(repoDir, manifest.Host, runID)
		if err != nil {
			return err
		}
		if activeCleanupRecovery {
			if err := requireStaleSubmittedRunController(repoDir, manifest.Host, runID); err != nil {
				return err
			}
		}
	}
	var lockErr error
	if cleanupFailedRunning || record.StopPhase == "session-stopped" || record.StopPhase == "broker-cleaned" {
		lockErr = ensureRunHasCleanupPendingLock(repoDir, manifest, runID, record.HostLockAuthoritative || record.StopPhase == "broker-cleaned")
	} else {
		lockErr = ensureRunHasKeptLock(repoDir, manifest, runID, record.HostLockAuthoritative)
	}
	if lockErr != nil {
		return lockErr
	}
	if record.HostLockAuthoritative {
		if record.StopPhase == "" {
			var verifyErr error
			if activeCleanupRecovery {
				verifyErr = verifyCleanupHostSideLockForRun(record.HostConfig, runID)
			} else {
				verifyErr = verifyKeptHostSideLockForRun(record.HostConfig, runID)
			}
			if verifyErr != nil {
				return verifyErr
			}
		}
	}
	if record.LegacyMissingRecord && manifest.Runtime.BrokerAdapter != "fake" {
		if _, ledgerErr := newRunLedgerStore(repoDir).Read(runID); ledgerErr == nil {
			return fmt.Errorf("kept run %q is missing its durable Run Record; refusing to stop the broker session or release the authoritative Host Lock", runID)
		} else {
			var usageErr *usageError
			if !errors.As(ledgerErr, &usageErr) {
				return ledgerErr
			}
			if err := removeRepoHostLockForRun(repoDir, manifest.Host, runID); err != nil {
				return err
			}
			return cleanupRunState(repoDir, runID)
		}
	}
	broker, err := retentionBrokerForRecord(record)
	if err != nil {
		return err
	}
	capabilities := broker.RetentionCapabilities()
	if err := requireRetentionCapability(broker, manifest.Runtime.BrokerAdapter, "stop retained session", capabilities.StopRetainedSession); err != nil {
		return err
	}
	if record.StopPhase == "" || record.StopPhase == "session-stopped" {
		if err := broker.StopRetainedRun(record); err != nil {
			return err
		}
	}
	record.StopPhase = "broker-cleaned"
	if err := checkpointRunLedgerStopPhase(repoDir, runID, record.StopPhase, now); err != nil {
		return err
	}
	transientRecordErr := writeRunRetentionRecord(runContext{StateDir: stateDir}, record)
	if afterBrokerStop != nil {
		afterBrokerStop(stateDir)
	}
	if record.HostLockAuthoritative {
		if err := releaseHostSideLock(record.HostConfig, runID); err != nil {
			if !errors.Is(err, errHostLockOwnershipChanged) {
				return errors.Join(transientRecordErr, err)
			}
		}
	}
	finishErr := finishReleasedStopRun(repoDir, stateDir, manifest.Host, runID, record, now)
	if finishErr == nil {
		return nil
	}
	return errors.Join(transientRecordErr, finishErr)
}

func finishReleasedStopRun(repoDir string, stateDir string, hostID string, runID string, record runRetentionRecord, now time.Time) error {
	if err := removeRepoHostLockForRun(repoDir, hostID, runID); err != nil {
		return err
	}
	if err := checkpointRunLedgerStopPhase(repoDir, runID, "host-lock-released", now); err != nil {
		return err
	}
	record.StopPhase = "host-lock-released"
	transientRecordErr := writeRunRetentionRecord(runContext{StateDir: stateDir}, record)
	cleanupErr := cleanupRunState(repoDir, runID)
	if cleanupErr == nil {
		return nil
	}
	return errors.Join(transientRecordErr, cleanupErr)
}

func removeRepoHostLockForRun(repoDir string, hostID string, runID string) error {
	if err := validateHostID(hostID); err != nil {
		return err
	}
	lockDir := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts")
	lockPath := filepath.Join(lockDir, hostID+".lock")
	return withLocalHostSideMutex(lockDir, hostID, func() error {
		info, err := os.Lstat(lockPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("host lock %s must be a regular file, not a symlink", lockPath)
		}
		content, err := os.ReadFile(lockPath)
		if err != nil {
			return err
		}
		owner := parseHostLockOwner(string(content))
		if owner.KeptRun == runID || owner.ActiveRun == runID {
			return os.Remove(lockPath)
		}
		if owner.KeptRun != "" || owner.ActiveRun != "" {
			return nil
		}
		return fmt.Errorf("host lock %s has no recognizable run owner", lockPath)
	})
}

func readStopRunManifest(repoDir string, runID string) (runManifest, string, bool, error) {
	if err := validateRunID(runID); err != nil {
		return runManifest{}, "", false, err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "runs", runID)); err != nil {
		return runManifest{}, "", false, err
	}
	stateDir := filepath.Join(repoDir, ".maya-stall", "state", "runs", runID)
	manifestBytes, err := os.ReadFile(filepath.Join(stateDir, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return runManifest{}, stateDir, false, nil
	}
	if err != nil {
		return runManifest{}, "", false, err
	}
	var manifest runManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return runManifest{}, "", false, fmt.Errorf("parse kept run manifest: %w", err)
	}
	return manifest, stateDir, true, nil
}

func releaseMatchingKeptHostLock(repoDir string, runID string) error {
	if err := validateRunID(runID); err != nil {
		return err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return err
	}
	lockDir := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts")
	entries, err := os.ReadDir(lockDir)
	if errors.Is(err, os.ErrNotExist) {
		return newUsageError("kept run %q not found", runID)
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lock" {
			continue
		}
		lockPath := filepath.Join(lockDir, entry.Name())
		keptRun, found, err := readHostLockKeptRun(lockPath)
		if err != nil {
			return err
		}
		if found && keptRun == runID {
			content, err := os.ReadFile(lockPath)
			if err != nil {
				return err
			}
			if parseHostLockOwner(string(content)).Authoritative {
				return fmt.Errorf("kept run %q is missing its run record; refusing to release only the repo-local Host Lock mirror", runID)
			}
			return removeRepoHostLockForRun(repoDir, strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())), runID)
		}
	}
	return newUsageError("kept run %q not found", runID)
}

func cleanupRunState(repoDir string, runID string) error {
	if err := validateRunID(runID); err != nil {
		return err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "runs", runID)); err != nil {
		return err
	}
	stateDir := filepath.Join(repoDir, ".maya-stall", "state", "runs", runID)
	return os.RemoveAll(stateDir)
}

func listKeptRuns(repoDir string) ([]keptRun, error) {
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return nil, err
	}
	lockDir := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts")
	entries, err := os.ReadDir(lockDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	runs := make([]keptRun, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".lock" {
			continue
		}
		runID, found, err := readHostLockKeptRun(filepath.Join(lockDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		run, err := readKeptRun(repoDir, runID)
		if err != nil {
			return nil, err
		}
		if err := refreshKeptRunStatus(&run); err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

func readKeptRun(repoDir string, runID string) (keptRun, error) {
	run, err := readKeptRunState(repoDir, runID)
	if err != nil {
		return keptRun{}, err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join("artifacts", "maya-stall", runID)); err != nil {
		return keptRun{}, err
	}
	evidenceBytes, err := os.ReadFile(filepath.Join(repoDir, "artifacts", "maya-stall", runID, "evidence.json"))
	if err != nil {
		return keptRun{}, err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(evidenceBytes, &bundle); err != nil {
		return keptRun{}, fmt.Errorf("parse kept run evidence: %w", err)
	}
	run.Bundle = bundle
	return run, nil
}

func readKeptRunState(repoDir string, runID string) (keptRun, error) {
	manifest, stateDir, err := readKeptRunManifest(repoDir, runID)
	if err != nil {
		return keptRun{}, err
	}
	record, err := readRunRetentionRecord(repoDir, stateDir, manifest)
	if err != nil {
		return keptRun{}, err
	}
	active := record.Status == "running" && record.StopPhase == ""
	var lockErr error
	if active {
		lockErr = ensureRunHasCleanupPendingLock(repoDir, manifest, runID, record.HostLockAuthoritative)
	} else {
		lockErr = ensureRunHasKeptLock(repoDir, manifest, runID, record.HostLockAuthoritative)
	}
	if lockErr != nil {
		return keptRun{}, lockErr
	}
	if record.HostLockAuthoritative {
		var verifyErr error
		if active {
			verifyErr = verifyHostSideLockForRun(record.HostConfig, runID)
		} else {
			verifyErr = verifyKeptHostSideLockForRun(record.HostConfig, runID)
		}
		if verifyErr != nil {
			return keptRun{}, verifyErr
		}
	}
	return keptRun{RunID: runID, StateDir: stateDir, Manifest: manifest, Record: record}, nil
}

func readRunRetentionRecord(repoDir string, stateDir string, manifest runManifest) (runRetentionRecord, error) {
	if err := ensureWorkspacePathHasNoSymlinkAncestor(stateDir, "run-record.json"); err != nil {
		return runRetentionRecord{}, err
	}
	content, err := readRunLedgerBytes(filepath.Join(stateDir, "run-record.json"))
	if errors.Is(err, os.ErrNotExist) {
		return fallbackRunRetentionRecord(repoDir, stateDir, manifest), nil
	}
	if err != nil {
		return runRetentionRecord{}, err
	}
	var record runRetentionRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return runRetentionRecord{}, fmt.Errorf("parse kept run record: %w", err)
	}
	if record.RemoteSession.SessionID == "" && manifest.BrokerSession != nil {
		record.RemoteSession = retainedSessionRecord{
			BrokerAdapter: manifest.BrokerSession.BrokerAdapter,
			SessionID:     manifest.BrokerSession.SessionID,
			Status:        "running",
		}
	}
	return record, nil
}

func refreshKeptRunStatus(run *keptRun) error {
	if run.Record.LegacyMissingRecord && run.Manifest.Runtime.BrokerAdapter != "fake" {
		run.RemoteStatus = retainedRunStatus{
			State:           "stale",
			BrokerStatus:    "unknown",
			RemoteWorkspace: run.Record.RemoteWorkspace,
			Detail:          "missing Run Record; broker-backed status is unavailable for this legacy kept run",
		}
		return nil
	}
	broker, err := retentionBrokerForRecord(run.Record)
	if err != nil {
		return err
	}
	capabilities := broker.RetentionCapabilities()
	if err := requireRetentionCapability(broker, run.Manifest.Runtime.BrokerAdapter, "status retained session", capabilities.StatusRetainedSession); err != nil {
		return err
	}
	status, err := broker.StatusRetainedRun(run.Record)
	if err != nil {
		return err
	}
	if run.Record.Status == "running" && run.Record.StopPhase == "" {
		status.State = "running"
		status.Detail = "Maya UI Session is owned by an active or cleanup-pending run"
	}
	run.RemoteStatus = status
	return nil
}

func readKeptRunManifest(repoDir string, runID string) (runManifest, string, error) {
	if err := validateRunID(runID); err != nil {
		return runManifest{}, "", err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "runs", runID)); err != nil {
		return runManifest{}, "", err
	}
	stateDir := filepath.Join(repoDir, ".maya-stall", "state", "runs", runID)
	manifestBytes, err := os.ReadFile(filepath.Join(stateDir, "manifest.json"))
	if errors.Is(err, os.ErrNotExist) {
		return runManifest{}, "", newUsageError("kept run %q not found", runID)
	}
	if err != nil {
		return runManifest{}, "", err
	}
	var manifest runManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return runManifest{}, "", fmt.Errorf("parse kept run manifest: %w", err)
	}
	return manifest, stateDir, nil
}

func ensureRunHasKeptLock(repoDir string, manifest runManifest, runID string, allowMissingMirror bool) error {
	if err := validateHostID(manifest.Host); err != nil {
		return err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return err
	}
	lockPath := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts", manifest.Host+".lock")
	keptRun, found, err := readHostLockKeptRun(lockPath)
	if err != nil {
		return err
	}
	if !found {
		if allowMissingMirror {
			return nil
		}
		return fmt.Errorf("Host Lock for %s is not a kept run lock", manifest.Host)
	}
	if keptRun != runID {
		return fmt.Errorf("Host Lock for %s belongs to kept run %s, not %s", manifest.Host, keptRun, runID)
	}
	return nil
}

func ensureRunHasCleanupPendingLock(repoDir string, manifest runManifest, runID string, allowMissingMirror bool) error {
	if err := validateHostID(manifest.Host); err != nil {
		return err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return err
	}
	lockPath := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts", manifest.Host+".lock")
	info, err := os.Lstat(lockPath)
	if errors.Is(err, os.ErrNotExist) && allowMissingMirror {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("host lock %s must be a regular file, not a symlink", lockPath)
	}
	content, err := os.ReadFile(lockPath)
	if err != nil {
		return err
	}
	owner := parseHostLockOwner(string(content))
	if owner.KeptRun == runID || owner.ActiveRun == runID {
		return nil
	}
	return fmt.Errorf("host lock for %s does not belong to cleanup-pending run %s", manifest.Host, runID)
}

func readHostLockKeptRun(lockPath string) (string, bool, error) {
	info, err := os.Lstat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("Host Lock %s must not be a symlink", lockPath)
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("Host Lock %s must be a regular file", lockPath)
	}
	content, err := os.ReadFile(lockPath)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) == "keptRun" {
			return strings.TrimSpace(value), strings.TrimSpace(value) != "", nil
		}
	}
	return "", false, nil
}

func copyRunStateTextFile(stateDir string, relativePath string, stdout io.Writer) error {
	if err := ensureWorkspacePathHasNoSymlinkAncestor(stateDir, relativePath); err != nil {
		return err
	}
	return copyTextFile(filepath.Join(stateDir, relativePath), stdout)
}

func copyTextFile(path string, stdout io.Writer) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	file, err := openRunLedgerRead(path)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(stdout, file)
	return err
}
