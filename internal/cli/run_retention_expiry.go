package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func keptSessionSweepRepoDirs(repoDir string, repoRoot string) ([]string, []string) {
	if repoRoot == "" {
		return []string{repoDir}, nil
	}
	info, err := os.Lstat(repoRoot)
	if errors.Is(err, os.ErrNotExist) {
		return []string{repoDir}, nil
	}
	if err != nil {
		return []string{repoDir}, []string{fmt.Sprintf("list configured kept-session repositories: %v", err)}
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return []string{repoDir}, []string{fmt.Sprintf("configured kept-session repository root %s must be a directory, not a symlink", repoRoot)}
	}
	entries, err := os.ReadDir(repoRoot)
	if err != nil {
		return []string{repoDir}, []string{fmt.Sprintf("list configured kept-session repositories: %v", err)}
	}
	repos := make([]string, 0, len(entries)+1)
	seen := make(map[string]bool)
	add := func(path string) {
		path = filepath.Clean(path)
		if !seen[path] {
			seen[path] = true
			repos = append(repos, path)
		}
	}
	add(repoDir)
	var warnings []string
	for _, entry := range entries {
		if !entry.IsDir() || validateRunID(entry.Name()) != nil {
			continue
		}
		candidate := filepath.Join(repoRoot, entry.Name(), "repo")
		candidateInfo, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("inspect configured kept-session repository %s: %v", candidate, err))
			continue
		}
		if candidateInfo.Mode()&os.ModeSymlink != 0 || !candidateInfo.IsDir() {
			warnings = append(warnings, fmt.Sprintf("configured kept-session repository %s must be a directory, not a symlink", candidate))
			continue
		}
		add(candidate)
	}
	sort.Strings(repos)
	return repos, warnings
}

func sweepKeptSessions(repoDir string, hostID string, now time.Time) []string {
	var warnings []string
	if err := withRunRetentionLock(repoDir, func() error {
		warnings = sweepKeptSessionsUnlocked(repoDir, hostID, now)
		return nil
	}); err != nil {
		return []string{fmt.Sprintf("lock kept-session expiry mutations for host %s: %v", hostID, err)}
	}
	return warnings
}

func sweepKeptSessionsUnlocked(repoDir string, hostID string, now time.Time) []string {
	runs, err := listKeptRuns(repoDir)
	if err != nil {
		return []string{fmt.Sprintf("sweep kept sessions for host %s: %v", hostID, err)}
	}
	var warnings []string
	for _, run := range runs {
		if run.Record.Host != hostID {
			continue
		}
		if run.Record.LegacyMissingRecord {
			warnings = append(warnings, fmt.Sprintf("kept run %s on host %s has no Run Record; refusing expiry cleanup", run.RunID, hostID))
			continue
		}
		if run.Record.HardDeadline == "" {
			stampKeptSessionHardDeadline(&run.Record, now)
			if keep, keepErr := time.Parse(time.RFC3339Nano, run.Record.KeepDeadline); keepErr == nil {
				hard, _ := time.Parse(time.RFC3339Nano, run.Record.HardDeadline)
				if keep.After(hard) {
					run.Record.KeepDeadline = run.Record.HardDeadline
				}
			}
			if err := writeRunRetentionRecord(runContext{StateDir: run.StateDir}, run.Record); err != nil {
				warnings = append(warnings, fmt.Sprintf("stamp Host Lock hard-lifetime grace for run %s on host %s: %v", run.RunID, hostID, err))
				continue
			}
		}
		if run.Record.KeepDeadline == "" {
			stampKeptSessionDeadline(&run.Record, now, keepTTLFromRecord(run.Record, defaultKeepTTL))
			if err := writeRunRetentionRecord(runContext{StateDir: run.StateDir}, run.Record); err != nil {
				warnings = append(warnings, fmt.Sprintf("stamp kept-session grace for run %s on host %s: %v", run.RunID, hostID, err))
				continue
			}
			if err := appendKeptSessionSweepEvent(repoDir, run, now, map[string]string{
				"event": "kept-session-grace-stamped", "runId": run.RunID, "host": hostID,
				"deadline": run.Record.KeepDeadline, "outcome": "grace-stamped",
			}); err != nil {
				warnings = append(warnings, fmt.Sprintf("record kept-session grace for run %s on host %s: %v", run.RunID, hostID, err))
			}
			continue
		}
		deadline, err := time.Parse(time.RFC3339Nano, run.Record.KeepDeadline)
		if err != nil {
			deadlineErr := fmt.Errorf("kept run %s on host %s has invalid keep deadline %q", run.RunID, hostID, run.Record.KeepDeadline)
			event := map[string]string{
				"event": "kept-session-expired", "runId": run.RunID, "host": hostID,
				"deadline": run.Record.KeepDeadline, "outcome": "invalid-deadline", "detail": deadlineErr.Error(),
			}
			if eventErr := appendKeptSessionSweepEvent(repoDir, run, now, event); eventErr != nil {
				warnings = append(warnings, fmt.Sprintf("record invalid kept-session deadline for run %s on host %s: %v", run.RunID, hostID, eventErr))
			}
			warnings = append(warnings, deadlineErr.Error())
			continue
		}
		if deadline.After(now) {
			continue
		}
		if !run.Record.BrokerCapabilities.StopRetainedSession {
			capabilityErr := unsupportedBrokerCapabilityError(run.Manifest.Runtime.BrokerAdapter, "stop retained session")
			event := map[string]string{
				"event": "kept-session-expired", "runId": run.RunID, "host": hostID,
				"deadline": run.Record.KeepDeadline, "outcome": "unsupported", "detail": capabilityErr.Error(),
			}
			if err := appendKeptSessionSweepEvent(repoDir, run, now, event); err != nil {
				warnings = append(warnings, fmt.Sprintf("record unsupported kept-session expiry for run %s on host %s: %v", run.RunID, hostID, err))
			}
			warnings = append(warnings, fmt.Sprintf("expire kept run %s on host %s: %v", run.RunID, hostID, capabilityErr))
			continue
		}
		event := map[string]string{
			"event": "kept-session-expired", "runId": run.RunID, "host": hostID,
			"deadline": run.Record.KeepDeadline, "outcome": "broker-cleaned",
		}
		stopErr, eventErr := stopRunWithKeptExpiryEvent(repoDir, run, now, event)
		if eventErr != nil {
			warnings = append(warnings, fmt.Sprintf("record kept-session expiry for run %s on host %s: %v", run.RunID, hostID, eventErr))
		}
		if stopErr != nil {
			event["outcome"] = "failed"
			event["detail"] = stopErr.Error()
			_ = appendEventRecord(filepath.Join(run.StateDir, "events.jsonl"), event)
			_ = refreshRunLedgerArtifacts(repoDir, run.RunID, now)
			warnings = append(warnings, fmt.Sprintf("expire kept run %s on host %s: %v", run.RunID, hostID, stopErr))
		}
	}
	return warnings
}

func stopRunWithKeptExpiryEvent(repoDir string, run keptRun, now time.Time, event map[string]string) (error, error) {
	if err := prepareRunLedgerForStop(repoDir, run.RunID, now); err != nil {
		return err, nil
	}
	var eventErr error
	stopErr := stopKeptRunAfterBrokerStop(repoDir, run.RunID, now, func(stateDir string) {
		eventRun := run
		eventRun.StateDir = stateDir
		eventErr = appendKeptSessionSweepEvent(repoDir, eventRun, now, event)
	})
	ledgerErr := updateRunLedgerAfterStop(repoDir, run.RunID, stopErr, now)
	return errors.Join(stopErr, ledgerErr), eventErr
}

func appendKeptSessionSweepEvent(repoDir string, run keptRun, now time.Time, event map[string]string) error {
	if err := appendEventRecord(filepath.Join(run.StateDir, "events.jsonl"), event); err != nil {
		return err
	}
	return refreshRunLedgerArtifacts(repoDir, run.RunID, now)
}
