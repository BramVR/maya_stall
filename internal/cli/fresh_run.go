package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type freshRun interface {
	Run() (runOutcome, error)
}

type freshRunLifecycle struct {
	repoDir                       string
	options                       runOptions
	runtime                       runRuntime
	configPath                    string
	scenario                      scenarioContract
	host                          hostRuntime
	resolvedRuntime               resolvedRuntime
	context                       runContext
	manifest                      runManifest
	localRunStateOwned            bool
	localEvidenceDirOwned         bool
	sessionStartAttempted         bool
	session                       brokerSessionIdentity
	sessionStarted                bool
	sessionSettled                bool
	sessionStopAttempted          bool
	cancellationObserved          bool
	sessionOperationUnsettled     bool
	sessionRetained               bool
	brokerResult                  ScenarioResult
	result                        ScenarioResult
	visualEvidence                []visualEvidenceArtifact
	visualEvidenceCaptureComplete bool
	validatorResult               []validatorResult
	stopPolicy                    string
	followUp                      []string
	releaseHostLock               bool
	stopHostLockHeartbeat         func() error
	checkHostLockHeartbeat        func() error
	skipSettleArtifactCollection  bool
	skipSettleVisualEvidence      bool
	accepted                      bool
	failedLayer                   runFailureLayer
	failure                       *runFailureEvidence
	selectionCleanupState         string
	acceptedAt                    time.Time
	ledgerPolicy                  runLedgerPolicy
	warnings                      []string
}

func newFreshRun(repoDir string, options runOptions, runtime runRuntime) freshRun {
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	return &freshRunLifecycle{
		repoDir:      repoDir,
		options:      options,
		runtime:      runtime,
		stopPolicy:   "stopped",
		ledgerPolicy: defaultRunLedgerPolicy(),
	}
}

func failAcceptedSubmission(repoDir string, options runOptions, runtime runRuntime, submissionErr error) (runOutcome, error) {
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	run := &freshRunLifecycle{
		repoDir:      repoDir,
		options:      options,
		runtime:      runtime,
		stopPolicy:   "stopped",
		ledgerPolicy: defaultRunLedgerPolicy(),
	}
	if err := run.accept(); err != nil {
		return runOutcome{}, errors.Join(submissionErr, err, run.cleanupUnacceptedOwnership())
	}
	if policy, available := availableRunLedgerPolicy(repoDir); available {
		run.ledgerPolicy = policy
		if retentionErr := newRunLedgerStore(run.repoDir).Prune(policy, run.acceptedAt, run.manifest.RunID); retentionErr != nil {
			submissionErr = errors.Join(submissionErr, fmt.Errorf("apply embedded run ledger retention: %w", retentionErr))
		}
	}
	run.failedLayer = failureLayerSubmission
	outcome, runErr := run.finishEarlyFailure(submissionErr)
	ledgerErr := newRunLedgerStore(run.repoDir).Finalize(outcome, run.manifest, run.ledgerPolicy, run.runtime.Now())
	if ledgerErr != nil {
		ledgerErr = fmt.Errorf("finalize embedded run ledger for %s: %w", run.manifest.RunID, ledgerErr)
	}
	return outcome, errors.Join(runErr, ledgerErr)
}

func (run *freshRunLifecycle) Run() (outcome runOutcome, err error) {
	defer func() {
		if run.accepted && err != nil && run.failedLayer == "" {
			if evidenceErr := run.ensureAcceptedFailureEvidence(err); evidenceErr != nil {
				err = errors.Join(err, evidenceErr)
			}
		}
		cleanupIncomplete := run.sessionStartAttempted && run.failure != nil && run.failure.CleanupState == "pending" && !run.releaseHostLock
		if run.sessionOperationUnsettled {
			run.releaseHostLock = false
			cleanupIncomplete = true
		}
		if run.sessionOperationUnsettled && run.sessionStarted && !run.sessionStopAttempted {
			if stopErr := run.stopSessionDuringSettlement(); stopErr != nil {
				err = errors.Join(err, fmt.Errorf("stop Maya UI Session for %s after unsettled host operation: %w", run.manifest.RunID, stopErr))
			}
		}
		if run.sessionStarted && run.sessionSettled && run.sessionStopAttempted && run.releaseHostLock && !run.sessionOperationUnsettled {
			syncErr := run.syncEvidenceEvents()
			if syncErr != nil {
				err = errors.Join(err, syncErr)
				cleanupIncomplete = true
				if preserveErr := run.preserveStoppedRunForCleanup(); preserveErr != nil {
					err = errors.Join(err, preserveErr)
				}
			} else if cleanupErr := run.finishStoppedRunCleanup(); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
				cleanupIncomplete = true
			}
		}
		if run.sessionStarted && !run.sessionSettled && run.sessionStopAttempted && run.sessionOperationUnsettled {
			run.releaseHostLock = false
			cleanupIncomplete = true
		}
		if run.sessionStarted && !run.sessionSettled && !run.sessionOperationUnsettled {
			if stopErr := run.stopSessionDuringSettlement(); stopErr != nil {
				err = errors.Join(err, fmt.Errorf("stop Maya UI Session for %s after run failure: %w", run.manifest.RunID, stopErr))
				run.releaseHostLock = false
				cleanupIncomplete = true
			} else {
				syncErr := run.syncEvidenceEvents()
				if syncErr != nil {
					err = errors.Join(err, syncErr)
					cleanupIncomplete = true
					if preserveErr := run.preserveStoppedRunForCleanup(); preserveErr != nil {
						err = errors.Join(err, preserveErr)
					}
				} else if cleanupErr := run.finishStoppedRunCleanup(); cleanupErr != nil {
					err = errors.Join(err, cleanupErr)
					cleanupIncomplete = true
				}
			}
		}
		if !run.sessionStartAttempted && !run.accepted {
			if cleanupErr := run.cleanupUnacceptedOwnership(); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}
		if run.stopHostLockHeartbeat != nil {
			if heartbeatErr := run.stopHostLockHeartbeat(); heartbeatErr != nil {
				err = errors.Join(err, fmt.Errorf("renew Host Lock for %s: %w", run.host.HostID, heartbeatErr))
			}
			run.stopHostLockHeartbeat = nil
		}
		if run.releaseHostLock {
			if releaseErr := run.host.release(); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("release Host Lock for %s: %w", run.host.HostID, releaseErr))
				cleanupIncomplete = true
			}
		}
		if run.accepted && err != nil {
			if run.failure == nil {
				if evidenceErr := run.ensureAcceptedFailureEvidence(err); evidenceErr != nil {
					err = errors.Join(err, evidenceErr)
				}
			}
			if run.failure != nil && run.failure.CleanupState == "pending" {
				switch {
				case run.stopPolicy == "kept" && run.sessionRetained:
					run.failure.CleanupState = "retained"
				case cleanupIncomplete:
					run.stopPolicy = "unresolved"
					run.followUp = nil
					run.failure.CleanupState = "failed"
				default:
					run.stopPolicy = "stopped"
					run.followUp = nil
					run.failure.CleanupState = "completed"
				}
			}
			run.result = ScenarioResult{Status: resultStatusFailed, Summary: err.Error()}
			if run.failure != nil {
				run.failure.Diagnostic = err.Error()
				if evidenceErr := run.updateTerminalFailureEvidence(); evidenceErr != nil {
					err = errors.Join(err, evidenceErr)
				}
			}
			outcome = run.currentOutcome()
		}
		if run.accepted {
			if ledgerErr := newRunLedgerStore(run.repoDir).Finalize(outcome, run.manifest, run.ledgerPolicy, run.runtime.Now()); ledgerErr != nil {
				err = errors.Join(err, fmt.Errorf("finalize embedded run ledger for %s: %w", run.manifest.RunID, ledgerErr))
			}
		}
	}()

	if err := run.setup(); err != nil {
		if run.accepted && run.failedLayer != "" {
			return run.finishEarlyFailure(err)
		}
		return run.finishFailedRun(err)
	}
	if err := run.cancellationError(); err != nil {
		return run.finishFailedRun(err)
	}
	if err := run.hostLockHeartbeatError(); err != nil {
		return run.finishFailedRun(err)
	}
	if err := run.execute(); err != nil {
		return run.finishFailedRun(err)
	}
	if err := run.hostLockHeartbeatError(); err != nil {
		return run.finishFailedRun(err)
	}
	if err := run.cancellationError(); err != nil {
		return run.finishFailedRun(err)
	}
	outcome, err = run.settle()
	return outcome, err
}

func (run *freshRunLifecycle) cancellationError() error {
	if run.runtime.Cancel == nil {
		return nil
	}
	select {
	case err := <-run.runtime.Cancel:
		run.cancellationObserved = true
		return err
	default:
		return nil
	}
}

func (run *freshRunLifecycle) runPostSessionOperation(name string, operation func() error) error {
	if run.runtime.Cancel == nil {
		return operation()
	}
	finished := make(chan error, 1)
	go func() { finished <- operation() }()
	select {
	case err := <-finished:
		return err
	case cancelErr := <-run.runtime.Cancel:
		run.cancellationObserved = true
		wait := run.runtime.CancelWait
		if wait <= 0 {
			wait = defaultBrokerCancellationWait
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case operationErr := <-finished:
			return errors.Join(cancelErr, operationErr)
		case <-timer.C:
			run.sessionOperationUnsettled = true
			return errors.Join(cancelErr, fmt.Errorf("%s did not finish within %s after cancellation", name, wait))
		}
	}
}

func (run *freshRunLifecycle) stopSessionAfterFailure() error {
	run.sessionStopAttempted = true
	if !run.cancellationObserved {
		err := run.runtime.Broker.StopSession(run.context, run.session)
		if err == nil {
			run.sessionSettled = true
		}
		return err
	}
	wait := run.runtime.CancelWait
	if wait <= 0 {
		wait = defaultBrokerCancellationWait
	}
	run.sessionStopAttempted = true
	stopped := make(chan error, 1)
	go func() { stopped <- run.runtime.Broker.StopSession(run.context, run.session) }()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case err := <-stopped:
		if err == nil {
			run.sessionSettled = true
		}
		return err
	case <-timer.C:
		run.sessionOperationUnsettled = true
		return fmt.Errorf("session broker did not stop within %s after cancellation", wait)
	}
}

func (run *freshRunLifecycle) stopSessionDuringSettlement() error {
	run.sessionStopAttempted = true
	if run.cancellationObserved {
		return run.stopSessionAfterFailure()
	}
	if run.runtime.Cancel == nil {
		return run.stopSessionAfterFailure()
	}
	stopped := make(chan error, 1)
	go func() { stopped <- run.runtime.Broker.StopSession(run.context, run.session) }()
	select {
	case err := <-stopped:
		if err == nil {
			run.sessionSettled = true
		}
		return err
	case cancelErr := <-run.runtime.Cancel:
		run.cancellationObserved = true
		wait := run.runtime.CancelWait
		if wait <= 0 {
			wait = defaultBrokerCancellationWait
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case stopErr := <-stopped:
			if stopErr == nil {
				run.sessionSettled = true
			}
			return errors.Join(cancelErr, stopErr)
		case <-timer.C:
			run.sessionOperationUnsettled = true
			return errors.Join(cancelErr, fmt.Errorf("session broker did not stop within %s after cancellation", wait))
		}
	}
}

func (run *freshRunLifecycle) cleanupUnacceptedOwnership() error {
	if run.accepted || !run.localRunStateOwned {
		return nil
	}
	runID := run.context.RunWorkspace.RunID()
	var cleanupErr error
	if err := cleanupRunState(run.repoDir, runID); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clean up pre-session Fresh Run state for %s: %w", runID, err))
	}
	if run.localEvidenceDirOwned {
		if err := os.Remove(run.context.EvidenceDir); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("clean up empty pre-session Evidence directory for %s: %w", runID, err))
		}
	}
	return cleanupErr
}

func (run *freshRunLifecycle) setup() error {
	if err := run.accept(); err != nil {
		return err
	}
	if err := run.cancellationError(); err != nil {
		return err
	}
	run.failedLayer = failureLayerRepoConfig
	config, configPath, err := loadRepoRunConfig(run.repoDir)
	if err != nil {
		return err
	}
	ledgerPolicy, err := resolveRunLedgerPolicy(config.RunLedger)
	if err != nil {
		return err
	}
	run.ledgerPolicy = ledgerPolicy
	if err := newRunLedgerStore(run.repoDir).Prune(run.ledgerPolicy, run.acceptedAt, run.manifest.RunID); err != nil {
		return fmt.Errorf("apply embedded run ledger retention: %w", err)
	}
	run.configPath = configPath
	run.failedLayer = failureLayerScenario
	scenario, err := resolveScenarioContract(config, run.options.ScenarioName)
	if err != nil {
		return err
	}
	if run.options.AssignedMayaBuild != "" {
		scenario.Config.SelectedMayaBuild = run.options.AssignedMayaBuild
	}
	run.scenario = scenario
	if err := validateScenarioInputs(run.repoDir, scenario); err != nil {
		return err
	}
	if err := validateScenarioRemotePaths(scenario.Config); err != nil {
		return err
	}
	if err := run.configureScenarioWorkspace(scenario, configPath); err != nil {
		return err
	}
	var resolved resolvedRuntime
	run.failedLayer = failureLayerHostSelection
	sweepRepoDirs, sweepRepoWarnings := keptSessionSweepRepoDirs(run.repoDir, run.options.KeptSessionRepoRoot)
	run.warnings = append(run.warnings, sweepRepoWarnings...)
	selectionOptions := run.options
	sweptHosts := make(map[string]bool)
	selectionOptions.BeforeHostLock = func(host mayaHostConfig) {
		if sweptHosts[host.ID] {
			return
		}
		sweptHosts[host.ID] = true
		for _, repoDir := range sweepRepoDirs {
			run.warnings = append(run.warnings, sweepKeptSessions(repoDir, host.ID, run.runtime.Now())...)
		}
	}
	host, err := selectHostForRunValidated(run.repoDir, selectionOptions, func(config mayaHostConfig) error {
		run.failedLayer = failureLayerRemoteCheck
		targetProfile := run.options.TargetProfile
		if targetProfile == "" {
			targetProfile = "default"
		}
		run.host = hostRuntime{TargetProfile: targetProfile, HostID: config.ID, Config: config}
		run.manifest.TargetProfile = targetProfile
		run.manifest.Host = config.ID
		var err error
		resolved, err = resolveRuntimeForHost(config)
		if err != nil {
			return err
		}
		run.resolvedRuntime = resolved
		run.manifest.Runtime = resolved.Metadata
		if err := validateTrustedPluginArtifactsRoot(config); err != nil {
			return err
		}
		return rejectMismatchedRuntimeOverride(resolved, run.runtime)
	})
	if err != nil {
		var validationErr *hostValidationError
		if errors.As(err, &validationErr) {
			run.selectionCleanupState = validationErr.cleanupState
		}
		return err
	}
	if host.release == nil {
		host.release = func() error { return nil }
	}
	run.host = host
	run.releaseHostLock = true
	run.stopHostLockHeartbeat, run.checkHostLockHeartbeat = startHostLockHeartbeat(run.host.renew)
	run.resolvedRuntime = resolved
	run.manifest.TargetProfile = host.TargetProfile
	run.manifest.Host = host.HostID
	run.manifest.Runtime = resolved.Metadata
	run.failedLayer = failureLayerRunState
	workspace, err := newRunWorkspace(run.repoDir, run.manifest.RunID, host.Config.WorkRoot, scenario.ScenarioResultPath)
	if err != nil {
		return err
	}
	run.context.RunWorkspace = workspace
	run.context.ScenarioResultPath = workspace.LocalScenarioResultPath()
	run.context.Environment[scenarioResultEnvVar] = workspace.LocalScenarioResultPath()
	if err := run.writeManifest(); err != nil {
		return err
	}
	if run.runtime.Host == nil {
		run.runtime.Host = resolved.Host
	}
	if run.runtime.Broker == nil {
		run.runtime.Broker = resolved.Broker
	}
	run.failedLayer = failureLayerRemoteCheck
	if err := rejectInvalidSessionBroker(run.runtime.Broker); err != nil {
		return err
	}
	if err := rejectUnsupportedEvidenceConfig(run.runtime.Broker, scenario.Config); err != nil {
		return err
	}
	readinessHost := resolved.Host
	if run.runtime.ReadinessHost != nil {
		readinessHost = run.runtime.ReadinessHost
	}
	readinessBroker := resolved.Broker
	if run.runtime.ReadinessBroker != nil {
		readinessBroker = run.runtime.ReadinessBroker
	}
	if err := run.cancellationError(); err != nil {
		return err
	}
	if err := probeHostReadiness(readinessHost, readinessBroker, host.HostID, preRunProbeLayerTimeout); err != nil {
		return err
	}
	if root := trustedPluginArtifactsRoot(host.Config); root != "" {
		run.context.Environment[trustedPluginArtifactsRootEnvVar] = root
	}
	run.failedLayer = failureLayerScenario
	// Freeze the payload before TrustCenter preflight so the checked paths are
	// the exact bytes later staged to the Maya Host.
	if err := snapshotRunPayload(run.context, scenario.Payload); err != nil {
		return err
	}
	run.failedLayer = failureLayerRemoteCheck
	if err := ensureTrustedPluginArtifactSnapshotAllowlistedForRun(run.context, host.Config, scenario); err != nil {
		return err
	}
	if err := run.cancellationError(); err != nil {
		return err
	}

	run.failedLayer = failureLayerRunState
	if err := writeRunRetentionRecord(run.context, newRunRetentionRecord(run.context, run.manifest, host.Config, "running", "", run.options.AssignedHardDeadline, run.runtime.Now())); err != nil {
		return err
	}
	if err := run.host.markActive(run.manifest.RunID); err != nil {
		return err
	}
	if err := appendEvent(run.context.EventsPath, "run.started", scenario.Name); err != nil {
		return err
	}
	run.failedLayer = ""
	if err := run.startFreshSession(); err != nil {
		return err
	}
	if err := run.cancellationError(); err != nil {
		return err
	}
	if err := run.renewHostLockNow(); err != nil {
		return err
	}
	if err := run.stagePayloadWithCancellation(scenario.Payload); err != nil {
		return err
	}
	return run.cancellationError()
}

func (run *freshRunLifecycle) accept() error {
	run.acceptedAt = run.runtime.Now().UTC()
	baseRunID := run.acceptedAt.Format("20060102T150405.000000000Z")
	if run.options.AssignedRunID != "" {
		if err := validateRunID(run.options.AssignedRunID); err != nil {
			return err
		}
		baseRunID = run.options.AssignedRunID
	}
	for attempt := 0; ; attempt++ {
		runID := baseRunID
		if attempt > 0 && run.options.AssignedRunID == "" {
			runID = fmt.Sprintf("%s-%d", baseRunID, attempt)
		} else if attempt > 0 {
			return fmt.Errorf("assigned Run ID %s already exists", baseRunID)
		}
		workspace, err := newRunWorkspace(run.repoDir, runID, "", evidenceStandaloneResultName)
		if err != nil {
			return err
		}
		context := runContext{
			RepoDir:            run.repoDir,
			RunWorkspace:       workspace,
			StateDir:           workspace.StateDir(),
			EvidenceDir:        workspace.EvidenceDir(),
			Workspace:          workspace.LocalWorkspace(),
			EventsPath:         workspace.EventsPath(),
			LogPath:            workspace.LogPath(),
			ScenarioResultPath: workspace.LocalScenarioResultPath(),
			Environment: map[string]string{
				scenarioResultEnvVar: workspace.LocalScenarioResultPath(),
			},
		}
		exists, err := newRunLedgerStore(run.repoDir).Occupied(runID)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		if _, err := os.Lstat(context.EvidenceDir); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		ownership, err := createCleanRunDirsWithOwnership(context)
		if err != nil {
			if ownership.StateDir {
				if cleanupErr := cleanupRunState(run.repoDir, runID); cleanupErr != nil {
					return errors.Join(err, cleanupErr)
				}
			}
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return err
		}
		run.context = context
		run.localRunStateOwned = ownership.StateDir
		run.localEvidenceDirOwned = ownership.EvidenceDir
		break
	}
	run.manifest = runManifest{
		Version:       evidenceSchemaVersion,
		RunID:         run.context.RunWorkspace.RunID(),
		Scenario:      run.options.ScenarioName,
		TargetProfile: run.options.TargetProfile,
	}
	if err := run.writeManifest(); err != nil {
		return err
	}
	if err := appendEvent(run.context.EventsPath, "run.accepted", run.options.ScenarioName); err != nil {
		return err
	}
	if len(run.options.AssignedEventPrefix) > 0 {
		if err := installAssignedEventPrefix(run.context.EventsPath, run.options.AssignedEventPrefix); err != nil {
			return err
		}
	}
	ledger := newRunLedgerStore(run.repoDir)
	if err := ledger.Initialize(run.manifest, run.acceptedAt, run.context.EventsPath); err != nil {
		return errors.Join(fmt.Errorf("initialize embedded run ledger: %w", err), ledger.Remove(run.manifest.RunID))
	}
	run.accepted = true
	if run.runtime.Accepted != nil {
		run.runtime.Accepted(run.currentOutcome())
	}
	if run.runtime.AcceptedCheck != nil {
		if err := run.runtime.AcceptedCheck(); err != nil {
			return err
		}
	}
	return nil
}

func (run *freshRunLifecycle) configureScenarioWorkspace(scenario scenarioContract, configPath string) error {
	workspace, err := newRunWorkspace(run.repoDir, run.manifest.RunID, "", scenario.ScenarioResultPath)
	if err != nil {
		return err
	}
	run.context.RunWorkspace = workspace
	run.context.ScenarioResultPath = workspace.LocalScenarioResultPath()
	run.context.Environment[scenarioResultEnvVar] = workspace.LocalScenarioResultPath()
	run.manifest.Scenario = scenario.Name
	run.manifest.ConfigPath = repoRelativePath(run.repoDir, configPath)
	run.manifest.Payload = scenario.Payload
	return run.writeManifest()
}

func (run *freshRunLifecycle) writeManifest() error {
	return writeJSONFile(filepath.Join(run.context.StateDir, evidenceManifestFileName), run.manifest)
}

func (run *freshRunLifecycle) finishEarlyFailure(runErr error) (runOutcome, error) {
	cleanupState := run.selectionCleanupState
	if cleanupState == "" {
		cleanupState = "not-needed"
	}
	if run.stopHostLockHeartbeat != nil {
		if heartbeatErr := run.stopHostLockHeartbeat(); heartbeatErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("renew Host Lock for %s: %w", run.host.HostID, heartbeatErr))
		}
		run.stopHostLockHeartbeat = nil
		run.checkHostLockHeartbeat = nil
	}
	if run.releaseHostLock && !run.sessionStarted {
		if err := run.host.release(); err != nil {
			cleanupState = "failed"
			runErr = errors.Join(runErr, fmt.Errorf("release Host Lock for %s: %w", run.host.HostID, err))
		} else {
			cleanupState = "completed"
		}
		run.releaseHostLock = false
	}
	run.result = ScenarioResult{Status: resultStatusFailed, Summary: runErr.Error()}
	run.failure = &runFailureEvidence{
		FailedLayer:     string(run.failedLayer),
		Diagnostic:      runErr.Error(),
		RemediationHint: earlyFailureRemediation(run.failedLayer),
		CaptureState:    "not-started",
		CleanupState:    cleanupState,
	}
	if cleanupState == "failed" {
		run.stopPolicy = "unresolved"
	}
	if err := ensureFailureLog(run.context, runErr); err != nil {
		return run.currentOutcome(), errors.Join(runErr, err)
	}
	if err := appendEvent(run.context.EventsPath, "run.failed", string(run.failedLayer)); err != nil {
		return run.currentOutcome(), errors.Join(runErr, err)
	}
	if err := run.writeManifest(); err != nil {
		return run.currentOutcome(), errors.Join(runErr, err)
	}
	if err := writeMinimalEvidenceBundle(run.context, run.manifest, run.result, run.failure); err != nil {
		return run.currentOutcome(), errors.Join(runErr, err)
	}
	return run.currentOutcome(), runErr
}

type runFailureLayer string

const (
	failureLayerSubmission    runFailureLayer = "submission"
	failureLayerRepoConfig    runFailureLayer = "repo-config"
	failureLayerScenario      runFailureLayer = "scenario"
	failureLayerHostSelection runFailureLayer = "host-selection"
	failureLayerRemoteCheck   runFailureLayer = "remote-check"
	failureLayerRunState      runFailureLayer = "run-state"
	failureLayerExecution     runFailureLayer = "execution"
)

func earlyFailureRemediation(layer runFailureLayer) string {
	switch layer {
	case failureLayerSubmission:
		return "Fix the command syntax while keeping the intended Scenario, then submit it again."
	case failureLayerRepoConfig:
		return "Fix or create the Repo Run Config, then submit the Scenario again."
	case failureLayerScenario:
		return "Fix the named Scenario and its declared inputs, then submit it again."
	case failureLayerHostSelection:
		return "Check Target Profile, Host Pool, Maya Host health, and Host Locks, then retry."
	case failureLayerRemoteCheck:
		return "Run maya-stall doctor for the same Scenario and repair the reported remote prerequisite."
	case failureLayerRunState:
		return "Check local Run State and Evidence paths, permissions, and disk capacity, then retry."
	case failureLayerExecution:
		return "Inspect the run diagnostic and evidence, resolve any retained Maya session or Host Lock, then retry the Scenario."
	default:
		return "Review the diagnostic, correct the submission prerequisite, and retry."
	}
}

func (run *freshRunLifecycle) ensureAcceptedFailureEvidence(runErr error) error {
	run.failedLayer = failureLayerExecution
	run.result = ScenarioResult{Status: resultStatusFailed, Summary: runErr.Error()}
	annotateVisualEvidenceTarget(run.visualEvidence, run.manifest.TargetProfile, run.manifest.Host)
	captureState := "not-captured"
	if len(run.visualEvidence) > 0 {
		captureState = "partial"
		if run.visualEvidenceCaptureComplete {
			captureState = "completed"
		}
	}
	run.failure = &runFailureEvidence{
		FailedLayer:     string(run.failedLayer),
		Diagnostic:      runErr.Error(),
		RemediationHint: earlyFailureRemediation(run.failedLayer),
		CaptureState:    captureState,
		CleanupState:    "pending",
	}
	bundlePath := filepath.Join(run.context.EvidenceDir, evidenceBundleFileName)
	_, bundleErr := os.Stat(bundlePath)
	if bundleErr != nil && !errors.Is(bundleErr, os.ErrNotExist) {
		return bundleErr
	}
	if err := run.recordTerminalFailureEvent(runErr); err != nil {
		return err
	}
	if bundleErr == nil {
		return run.updateTerminalFailureEvidence()
	}
	if err := ensureFailureLog(run.context, runErr); err != nil {
		return err
	}
	if err := run.writeManifest(); err != nil {
		return err
	}
	if err := writeMinimalEvidenceBundle(run.context, run.manifest, run.result, run.failure); err != nil {
		return err
	}
	return run.updateTerminalFailureEvidence()
}

func (run *freshRunLifecycle) recordTerminalFailureEvent(runErr error) error {
	if _, err := os.Stat(run.context.EventsPath); err == nil {
		if err := appendEvent(run.context.EventsPath, "run.failed", runErr.Error()); err != nil {
			return err
		}
		return copySequencedEvents(run.context.EventsPath, filepath.Join(run.context.EvidenceDir, evidenceEventsFileName))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return appendSequencedEvidenceEvent(filepath.Join(run.context.EvidenceDir, evidenceEventsFileName), "run.failed", runErr.Error())
}

func appendSequencedEvidenceEvent(path string, eventName string, detail string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	trimmed := bytes.TrimSpace(content)
	sequence := 1
	if len(trimmed) > 0 {
		lines := bytes.Split(trimmed, []byte{'\n'})
		for index, line := range lines {
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				return fmt.Errorf("parse run event %d: %w", index+1, err)
			}
		}
		sequence = len(lines) + 1
	}
	structured := newRunEventRecord(map[string]string{"detail": detail, "event": eventName})
	structured["sequence"] = sequence
	record, err := json.Marshal(structured)
	if err != nil {
		return err
	}
	var updated bytes.Buffer
	if len(trimmed) > 0 {
		updated.Write(trimmed)
		updated.WriteByte('\n')
	}
	updated.Write(record)
	updated.WriteByte('\n')
	if err := rejectExistingFileLeaf(path); err != nil {
		return err
	}
	return os.WriteFile(path, updated.Bytes(), 0o644)
}

func (run *freshRunLifecycle) updateTerminalFailureEvidence() error {
	path := filepath.Join(run.context.EvidenceDir, evidenceBundleFileName)
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return err
	}
	if len(run.visualEvidence) > 0 {
		bundle.VisualEvidence = mergeVisualEvidence(bundle.VisualEvidence, run.visualEvidence)
	}
	bundle.Status = resultStatusFailed
	bundle.Failure = run.failure
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	return writeJSONFile(path, bundle)
}

func writeMinimalEvidenceBundle(context runContext, manifest runManifest, result ScenarioResult, failure *runFailureEvidence) error {
	for _, file := range []struct {
		source      string
		destination string
	}{
		{filepath.Join(context.StateDir, evidenceManifestFileName), filepath.Join(context.EvidenceDir, evidenceManifestFileName)},
		{context.LogPath, filepath.Join(context.EvidenceDir, evidenceLogPath)},
	} {
		if err := copyFallbackEvidenceFile(file.source, file.destination); err != nil {
			return err
		}
	}
	if err := copySequencedEvents(context.EventsPath, filepath.Join(context.EvidenceDir, evidenceEventsFileName)); err != nil {
		return err
	}
	bundle := evidenceBundle{
		Version:       evidenceSchemaVersion,
		RunID:         manifest.RunID,
		Scenario:      manifest.Scenario,
		Status:        result.Status,
		TargetProfile: manifest.TargetProfile,
		Host:          manifest.Host,
		Runtime:       manifest.Runtime,
		BrokerSession: manifest.BrokerSession,
		Manifest:      evidenceManifestFileName,
		Events:        evidenceEventsFileName,
		Log:           evidenceLogPath,
		Payload:       manifest.Payload,
		Failure:       failure,
	}
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	return writeJSONFile(filepath.Join(context.EvidenceDir, evidenceBundleFileName), bundle)
}

func copyFallbackEvidenceFile(source string, destination string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := rejectExistingFileLeaf(destination); err != nil {
		return err
	}
	return os.WriteFile(destination, content, 0o644)
}

func startHostLockHeartbeat(renew func() error) (func() error, func() error) {
	if renew == nil {
		return func() error { return nil }, func() error { return nil }
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	var errMu sync.Mutex
	var renewalErr error
	go func() {
		ticker := time.NewTicker(hostSideLockHeartbeatInterval)
		defer ticker.Stop()
		defer close(done)
		for {
			select {
			case <-ticker.C:
				if err := renew(); err != nil {
					errMu.Lock()
					if renewalErr == nil {
						renewalErr = err
					}
					errMu.Unlock()
				}
			case <-stop:
				return
			}
		}
	}()
	check := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return renewalErr
	}
	var once sync.Once
	stopHeartbeat := func() error {
		once.Do(func() {
			close(stop)
			<-done
		})
		return check()
	}
	return stopHeartbeat, check
}

func (run *freshRunLifecycle) hostLockHeartbeatError() error {
	if run.checkHostLockHeartbeat == nil {
		return nil
	}
	if err := run.checkHostLockHeartbeat(); err != nil {
		return fmt.Errorf("renew Host Lock for %s: %w", run.host.HostID, err)
	}
	return nil
}

func (run *freshRunLifecycle) renewHostLockNow() error {
	if err := run.hostLockHeartbeatError(); err != nil {
		return err
	}
	if run.host.renew == nil {
		return nil
	}
	if err := run.host.renew(); err != nil {
		return fmt.Errorf("renew Host Lock for %s before host operation: %w", run.host.HostID, err)
	}
	return nil
}

func (run *freshRunLifecycle) startFreshSession() error {
	if err := run.renewHostLockNow(); err != nil {
		return err
	}
	run.sessionStartAttempted = true
	session, err := run.startSessionWithCancellation()
	if err == nil && (session.BrokerAdapter == "" || session.SessionID == "") {
		run.releaseHostLock = false
		err = fmt.Errorf("session broker started without an owned session identity")
	}
	if session.BrokerAdapter != "" {
		run.session = session
		run.sessionStarted = true
		run.manifest.BrokerSession = &session
		if manifestErr := writeJSONFile(filepath.Join(run.context.StateDir, "manifest.json"), run.manifest); manifestErr != nil {
			return errors.Join(err, manifestErr)
		}
		record, recordErr := readRunRetentionRecord(run.repoDir, run.context.StateDir, run.manifest)
		if recordErr == nil {
			record.RemoteSession = retainedSessionRecord{
				BrokerAdapter: session.BrokerAdapter,
				SessionID:     session.SessionID,
				Status:        "running",
			}
			if retention, ok := run.runtime.Broker.(runRetentionBroker); ok {
				record.BrokerCapabilities = retention.RetentionCapabilities()
			}
			recordErr = writeRunRetentionRecord(run.context, record)
		}
		if recordErr != nil {
			return errors.Join(err, fmt.Errorf("record owned Maya UI Session for %s: %w", run.manifest.RunID, recordErr))
		}
		if run.runtime.SessionStarted != nil {
			if bindErr := run.runtime.SessionStarted(session); bindErr != nil {
				return errors.Join(err, fmt.Errorf("bind Maya UI Session for %s: %w", run.manifest.RunID, bindErr))
			}
		}
		if err == nil && run.scenario.Config.SelectedMayaBuild != "" {
			verifier, ok := run.runtime.Broker.(mayaSessionBuildVerifier)
			if !ok {
				err = fmt.Errorf("session broker cannot verify selected Maya build %s", run.scenario.Config.SelectedMayaBuild)
			} else if verifyErr := verifier.VerifyMayaBuild(run.context, session, run.scenario.Config.SelectedMayaBuild); verifyErr != nil {
				err = verifyErr
			}
		}
	}
	if err != nil {
		if evidenceErr := ensureFailureLog(run.context, err); evidenceErr != nil {
			return errors.Join(err, evidenceErr)
		}
		if evidenceErr := appendEvent(run.context.EventsPath, "run.failed-before-result-collection", err.Error()); evidenceErr != nil {
			return errors.Join(err, evidenceErr)
		}
		if evidenceErr := run.writeBrokerFailureEvidence(err); evidenceErr != nil {
			return errors.Join(err, evidenceErr)
		}
		run.result = ScenarioResult{Status: resultStatusFailed, Summary: fmt.Sprintf("Scenario failed before result collection: %v", err)}
		return err
	}
	return nil
}

func (run *freshRunLifecycle) startSessionWithCancellation() (brokerSessionIdentity, error) {
	if run.runtime.Cancel == nil {
		return run.runtime.Broker.StartFreshSession(run.context, run.scenario.Config)
	}
	type result struct {
		session brokerSessionIdentity
		err     error
	}
	finished := make(chan result, 1)
	go func() {
		session, err := run.runtime.Broker.StartFreshSession(run.context, run.scenario.Config)
		finished <- result{session: session, err: err}
	}()
	select {
	case outcome := <-finished:
		return outcome.session, outcome.err
	case cancelErr := <-run.runtime.Cancel:
		run.cancellationObserved = true
		wait := run.runtime.CancelWait
		if wait <= 0 {
			wait = defaultBrokerCancellationWait
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case outcome := <-finished:
			return outcome.session, errors.Join(cancelErr, outcome.err)
		case <-timer.C:
			run.sessionOperationUnsettled = true
			run.releaseHostLock = false
			return brokerSessionIdentity{}, errors.Join(cancelErr, fmt.Errorf("session broker did not finish startup within %s after cancellation", wait))
		}
	}
}

func (run *freshRunLifecycle) stagePayloadWithCancellation(payload []manifestPayload) error {
	if run.runtime.Cancel == nil {
		return run.runtime.Host.StagePayload(run.context, payload)
	}
	finished := make(chan error, 1)
	go func() { finished <- run.runtime.Host.StagePayload(run.context, payload) }()
	select {
	case err := <-finished:
		return err
	case cancelErr := <-run.runtime.Cancel:
		run.cancellationObserved = true
		wait := run.runtime.CancelWait
		if wait <= 0 {
			wait = defaultBrokerCancellationWait
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case err := <-finished:
			return errors.Join(cancelErr, err)
		case <-timer.C:
			run.sessionOperationUnsettled = true
			return errors.Join(cancelErr, fmt.Errorf("host payload staging did not finish within %s after cancellation", wait))
		}
	}
}

func (run *freshRunLifecycle) execute() error {
	if err := run.renewHostLockNow(); err != nil {
		return err
	}
	result, err := run.runBrokerScenario()
	if lockErr := run.renewHostLockNow(); lockErr != nil {
		return errors.Join(err, lockErr)
	}
	if err != nil {
		if evidenceErr := run.collectBrokerFailureArtifacts(err); evidenceErr != nil {
			return errors.Join(err, evidenceErr)
		}
		recovered, recoveryErr := run.recoverCompletedScenarioAfterBrokerFailure(err)
		if recoveryErr != nil {
			return errors.Join(err, recoveryErr)
		}
		if recovered {
			return nil
		}
		run.result = ScenarioResult{Status: resultStatusFailed, Summary: fmt.Sprintf("Scenario failed before result collection: %v", err)}
		if evidenceErr := run.writeBrokerFailureEvidence(err); evidenceErr != nil {
			return errors.Join(err, evidenceErr)
		}
		return err
	}
	run.brokerResult = result
	run.result = result
	return nil
}

func (run *freshRunLifecycle) runBrokerScenario() (ScenarioResult, error) {
	if run.runtime.Cancel == nil {
		return run.runtime.Broker.RunScenario(run.context, run.scenario.Config)
	}
	type brokerRun struct {
		result ScenarioResult
		err    error
	}
	finished := make(chan brokerRun, 1)
	go func() {
		result, err := run.runtime.Broker.RunScenario(run.context, run.scenario.Config)
		finished <- brokerRun{result: result, err: err}
	}()
	select {
	case outcome := <-finished:
		return outcome.result, outcome.err
	case cancelErr := <-run.runtime.Cancel:
		run.cancellationObserved = true
		wait := run.runtime.CancelWait
		if wait <= 0 {
			wait = defaultBrokerCancellationWait
		}
		stopped := make(chan error, 1)
		run.sessionStopAttempted = true
		go func() { stopped <- run.runtime.Broker.StopSession(run.context, run.session) }()
		timer := time.NewTimer(wait)
		defer timer.Stop()
		var outcome brokerRun
		var stopErr error
		brokerDone := false
		stopDone := false
		for !brokerDone || !stopDone {
			select {
			case outcome = <-finished:
				brokerDone = true
			case stopErr = <-stopped:
				stopDone = true
			case <-timer.C:
				run.sessionOperationUnsettled = true
				return ScenarioResult{}, errors.Join(cancelErr, stopErr, fmt.Errorf("session broker did not stop within %s after cancellation", wait))
			}
		}
		if stopErr == nil {
			run.sessionSettled = true
		}
		return outcome.result, errors.Join(cancelErr, stopErr, outcome.err)
	}
}

func (run *freshRunLifecycle) collectBrokerFailureArtifacts(runErr error) error {
	if err := ensureFailureLog(run.context, runErr); err != nil {
		return err
	}
	summary := fmt.Sprintf("Scenario failed before result collection: %v", runErr)
	if err := appendEvent(run.context.EventsPath, "run.failed-before-result-collection", summary); err != nil {
		return err
	}
	if collector, ok := run.runtime.Host.(failureArtifactCollector); ok {
		if err := run.runPostSessionOperation("Host failure artifact collection", func() error {
			return collector.CollectFailureArtifacts(run.context, run.scenario)
		}); err != nil {
			if eventErr := appendEvent(run.context.EventsPath, "run.failed-before-result-collection.artifact-collection-failed", err.Error()); eventErr != nil {
				return eventErr
			}
		} else if err := appendEvent(run.context.EventsPath, "run.failed-before-result-collection.artifacts-collected", "best-effort"); err != nil {
			return err
		}
	} else if collector, ok := run.runtime.Host.(artifactCollector); ok {
		if err := run.runPostSessionOperation("Host artifact collection", func() error {
			return collector.CollectArtifacts(run.context, run.scenario)
		}); err != nil {
			if eventErr := appendEvent(run.context.EventsPath, "run.failed-before-result-collection.artifact-collection-failed", err.Error()); eventErr != nil {
				return eventErr
			}
		} else if err := appendEvent(run.context.EventsPath, "run.failed-before-result-collection.artifacts-collected", "best-effort"); err != nil {
			return err
		}
	}
	return nil
}

func (run *freshRunLifecycle) recoverCompletedScenarioAfterBrokerFailure(runErr error) (bool, error) {
	if err := validateScenarioResultPath(run.context, run.scenario.ScenarioResultPath); err != nil {
		return false, err
	}
	resultDocument, found, err := readScenarioResultDocument(run.context.ScenarioResultPath)
	if err != nil {
		if eventErr := appendEvent(run.context.EventsPath, "run.recovered-after-broker-failure.scenario-result-unreadable", err.Error()); eventErr != nil {
			return false, eventErr
		}
		return false, nil
	}
	if !found {
		if err := appendEvent(run.context.EventsPath, "run.recovered-after-broker-failure.scenario-result-missing", runErr.Error()); err != nil {
			return false, err
		}
		return false, nil
	}
	if resultDocument.Result.Status != resultStatusPassed {
		detail := resultDocument.Result.Status
		if detail == "" {
			detail = "missing"
		}
		if err := appendEvent(run.context.EventsPath, "run.recovered-after-broker-failure.rejected", "Scenario Result status "+detail); err != nil {
			return false, err
		}
		return false, nil
	}
	run.result = resultDocument.Result
	run.brokerResult = ScenarioResult{Status: resultStatusPassed, Summary: "Scenario completed before broker failure"}
	run.skipSettleArtifactCollection = true
	run.skipSettleVisualEvidence = true
	if policy, ok := run.runtime.Broker.(recoveredScenarioVisualEvidencePolicy); ok {
		run.skipSettleVisualEvidence = !policy.CaptureVisualEvidenceAfterRecoveredScenario(runErr)
	}
	return true, appendEvent(run.context.EventsPath, "run.recovered-after-broker-failure", runErr.Error())
}

func (run *freshRunLifecycle) writeBrokerFailureEvidence(runErr error) error {
	summary := fmt.Sprintf("Scenario failed before result collection: %v", runErr)
	result := ScenarioResult{Status: resultStatusFailed, Summary: summary}
	if err := preserveCollectedScenarioResultOrWriteFailure(run.context, run.scenario.ScenarioResultPath, result); err != nil {
		return err
	}
	artifacts, err := run.captureFailureScreenshot()
	if err != nil {
		return err
	}
	run.visualEvidence = mergeVisualEvidence(run.visualEvidence, artifacts)
	if len(artifacts) > 0 {
		capturePlan, planErr := planScenarioVisualEvidence(run.scenario.Name, run.scenario.Config.Evidence)
		run.visualEvidenceCaptureComplete = planErr == nil && len(artifacts) == len(capturePlan)
	}
	return writeEvidenceBundle(run.context, run.manifest, run.scenario, result, run.visualEvidence, nil)
}

func (run *freshRunLifecycle) captureFailureScreenshot() ([]visualEvidenceArtifact, error) {
	var artifacts []visualEvidenceArtifact
	if capturer, ok := run.runtime.Broker.(screenshotCapturer); ok && run.scenario.Config.Evidence.Screenshots.Enabled {
		artifact, err := capturer.CaptureScreenshot(run.context, screenshotRequest{Name: "failure-desktop.png"})
		if err == nil {
			artifacts = append(artifacts, artifact)
			annotateVisualEvidenceTarget(artifacts, run.manifest.TargetProfile, run.manifest.Host)
			if eventErr := appendEvent(run.context.EventsPath, "visual-evidence.failure-screenshot", artifact.Path); eventErr != nil {
				return nil, eventErr
			}
		} else if eventErr := appendEvent(run.context.EventsPath, "visual-evidence.failure-screenshot.failed", err.Error()); eventErr != nil {
			return nil, eventErr
		}
	}
	return artifacts, nil
}

func preserveCollectedScenarioResultOrWriteFailure(context runContext, resultPath string, result ScenarioResult) error {
	if err := validateScenarioResultPath(context, resultPath); err != nil {
		return err
	}
	_, found, err := readScenarioResultDocument(context.ScenarioResultPath)
	if err != nil {
		if eventErr := appendEvent(context.EventsPath, "run.failed-before-result-collection.scenario-result-unreadable", err.Error()); eventErr != nil {
			return eventErr
		}
		found = false
	}
	if found {
		return copyFile(context.ScenarioResultPath, filepath.Join(context.EvidenceDir, evidenceScenarioResultFileName))
	}
	return writeScenarioResult(context, resultPath, newScenarioResultDocument(result))
}

func ensureFailureLog(context runContext, runErr error) error {
	if _, err := os.Stat(context.LogPath); err == nil {
		return appendFile(context.LogPath, fmt.Sprintf("Scenario failed before result collection: %v\n", runErr))
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.WriteFile(context.LogPath, []byte(fmt.Sprintf("Scenario failed before result collection: %v\n", runErr)), 0o644)
}

func appendFile(path string, content string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
	}()
	_, err = file.WriteString(content)
	return err
}

func (run *freshRunLifecycle) settle() (runOutcome, error) {
	if err := run.cancellationError(); err != nil {
		return runOutcome{}, err
	}
	if err := run.renewHostLockNow(); err != nil {
		return runOutcome{}, err
	}
	if collector, ok := run.runtime.Host.(artifactCollector); ok && !run.skipSettleArtifactCollection {
		if err := run.renewHostLockNow(); err != nil {
			return runOutcome{}, err
		}
		if err := run.runPostSessionOperation("Host artifact collection", func() error {
			return collector.CollectArtifacts(run.context, run.scenario)
		}); err != nil {
			return runOutcome{}, err
		}
		if err := run.hostLockHeartbeatError(); err != nil {
			return runOutcome{}, err
		}
		if err := run.cancellationError(); err != nil {
			return runOutcome{}, err
		}
	}
	if err := validateScenarioResultPath(run.context, run.scenario.ScenarioResultPath); err != nil {
		return runOutcome{}, err
	}
	resultDocument, found, err := readScenarioResultDocument(run.context.ScenarioResultPath)
	if err != nil {
		return runOutcome{}, err
	}
	if found {
		run.result = resultDocument.Result
		if run.result.Status == "" {
			run.result.Status = run.brokerResult.Status
		}
		if run.result.Summary == "" {
			run.result.Summary = run.brokerResult.Summary
		}
	} else {
		resultDocument = newScenarioResultDocument(run.result)
	}
	if run.result.Status == "" {
		run.result.Status = resultStatusPassed
	}
	if !run.skipSettleVisualEvidence {
		if err := run.renewHostLockNow(); err != nil {
			return runOutcome{}, err
		}
		var visualEvidence []visualEvidenceArtifact
		err = run.runPostSessionOperation("visual evidence collection", func() error {
			var collectionErr error
			visualEvidence, collectionErr = collectScenarioVisualEvidence(run.runtime.Broker, run.context, run.scenario.Name, run.scenario.Config.Evidence)
			return collectionErr
		})
		if !run.sessionOperationUnsettled {
			run.visualEvidence = visualEvidence
		}
		if err != nil {
			return runOutcome{}, err
		}
		run.visualEvidenceCaptureComplete = true
		if err := run.cancellationError(); err != nil {
			return runOutcome{}, err
		}
	}
	annotateVisualEvidenceTarget(run.visualEvidence, run.manifest.TargetProfile, run.manifest.Host)
	runScopedVisualEvidence, err := readRunScopedVisualEvidence(run.context)
	if err != nil {
		return runOutcome{}, err
	}
	run.visualEvidence = mergeVisualEvidence(run.visualEvidence, runScopedVisualEvidence)
	resultDocument.setResult(run.result)
	if err := writeScenarioResult(run.context, run.scenario.ScenarioResultPath, resultDocument); err != nil {
		return runOutcome{}, err
	}
	run.validatorResult, err = validateRunOutputs(run.context, run.scenario, run.result)
	if err != nil {
		return runOutcome{}, err
	}
	validatorFailed := hasValidatorFailure(run.validatorResult)
	if validatorFailed {
		run.result.Status = resultStatusFailed
	}
	resultDocument.setResult(run.result)
	if err := writeScenarioResult(run.context, run.scenario.ScenarioResultPath, resultDocument); err != nil {
		return runOutcome{}, err
	}
	if validatorFailed && run.skipSettleVisualEvidence {
		artifacts, err := run.captureFailureScreenshot()
		if err != nil {
			return runOutcome{}, err
		}
		run.visualEvidence = mergeVisualEvidence(run.visualEvidence, artifacts)
	}
	if !shouldStopAfter(run.options.StopAfter, run.result.Status) {
		run.configureKeptStopPolicy()
	}
	if err := appendEvent(run.context.EventsPath, "run.completed", run.result.Status); err != nil {
		return runOutcome{}, err
	}
	if err := writeEvidenceBundle(run.context, run.manifest, run.scenario, run.result, run.visualEvidence, run.validatorResult); err != nil {
		return runOutcome{}, err
	}
	if run.stopPolicy == "stopped" {
		if err := run.cancellationError(); err != nil {
			return runOutcome{}, err
		}
		if err := run.renewHostLockNow(); err != nil {
			return runOutcome{}, err
		}
		if !run.sessionSettled {
			if err := run.stopSessionDuringSettlement(); err != nil {
				return runOutcome{}, fmt.Errorf("stop Maya UI Session for %s: %w", run.manifest.RunID, err)
			}
		}
		if err := run.cancellationError(); err != nil {
			return runOutcome{}, err
		}
		syncErr := run.syncEvidenceEvents()
		if syncErr != nil {
			preserveErr := run.preserveStoppedRunForCleanup()
			return runOutcome{}, errors.Join(syncErr, preserveErr)
		}
		if cleanupErr := run.finishStoppedRunCleanup(); cleanupErr != nil {
			return runOutcome{}, cleanupErr
		}
	}
	if run.stopPolicy == "kept" {
		if err := run.hostLockHeartbeatError(); err != nil {
			return runOutcome{}, err
		}
		if err := run.retainSession(run.retentionReason()); err != nil {
			return runOutcome{}, err
		}
	}
	return run.currentOutcome(), nil
}

func (run *freshRunLifecycle) configureKeptStopPolicy() string {
	run.stopPolicy = "kept"
	if len(run.followUp) == 0 {
		run.followUp = append(run.followUp,
			fmt.Sprintf("maya-stall status --run %s", run.manifest.RunID),
			fmt.Sprintf("maya-stall attach %s", run.manifest.RunID),
			fmt.Sprintf("maya-stall stop %s", run.manifest.RunID),
		)
	}
	return run.retentionReason()
}

func (run *freshRunLifecycle) finishFailedRun(runErr error) (runOutcome, error) {
	if lockErr := run.hostLockHeartbeatError(); lockErr != nil {
		return runOutcome{}, errors.Join(runErr, lockErr)
	}
	if !run.sessionStarted || shouldStopAfter(run.options.StopAfter, resultStatusFailed) {
		return runOutcome{}, runErr
	}
	if run.result.Status == "" {
		run.result = ScenarioResult{Status: resultStatusFailed, Summary: fmt.Sprintf("Scenario failed before result collection: %v", runErr)}
	}
	if _, err := os.Stat(filepath.Join(run.context.EvidenceDir, evidenceBundleFileName)); errors.Is(err, os.ErrNotExist) {
		if evidenceErr := ensureFailureLog(run.context, runErr); evidenceErr != nil {
			return runOutcome{}, errors.Join(runErr, evidenceErr)
		}
		if evidenceErr := appendEvent(run.context.EventsPath, "run.failed-before-result-collection", runErr.Error()); evidenceErr != nil {
			return runOutcome{}, errors.Join(runErr, evidenceErr)
		}
		if evidenceErr := run.writeBrokerFailureEvidence(runErr); evidenceErr != nil {
			return runOutcome{}, errors.Join(runErr, evidenceErr)
		}
	} else if err != nil {
		return runOutcome{}, errors.Join(runErr, err)
	}
	reason := run.configureKeptStopPolicy()
	if retainErr := run.retainSession(reason); retainErr != nil {
		return runOutcome{}, errors.Join(runErr, retainErr)
	}
	return run.currentOutcome(), runErr
}

func (run *freshRunLifecycle) retentionReason() string {
	if run.options.StopAfter == stopAfterSuccess {
		return "keep-on-failure"
	}
	return "stop-after-" + run.options.StopAfter
}

func (run *freshRunLifecycle) retainSession(reason string) error {
	retention, ok := run.runtime.Broker.(runRetentionBroker)
	if !ok || !retention.RetentionCapabilities().RetainOnFailure {
		return unsupportedBrokerCapabilityError(run.manifest.Runtime.BrokerAdapter, "retain-on-failure")
	}
	if run.stopHostLockHeartbeat != nil {
		if err := run.stopHostLockHeartbeat(); err != nil {
			return fmt.Errorf("renew Host Lock for %s: %w", run.host.HostID, err)
		}
		run.stopHostLockHeartbeat = nil
		run.checkHostLockHeartbeat = nil
	}
	if err := run.host.markKept(run.manifest.RunID); err != nil {
		return err
	}
	session, err := retention.RetainRun(run.context, run.manifest, reason)
	if err != nil {
		return err
	}
	if session.BrokerAdapter != run.session.BrokerAdapter || session.SessionID != run.session.SessionID {
		return fmt.Errorf("session broker retained a different Maya UI Session: started %s/%s, retained %s/%s", run.session.BrokerAdapter, run.session.SessionID, session.BrokerAdapter, session.SessionID)
	}
	record := newRunRetentionRecord(run.context, run.manifest, run.host.Config, "kept", reason, run.options.AssignedHardDeadline, run.runtime.Now())
	stampKeptSessionDeadline(&record, run.runtime.Now(), effectiveKeepTTL(run.options.KeepTTL))
	record.BrokerCapabilities = retention.RetentionCapabilities()
	record.RemoteSession = session
	if err := writeRunRetentionRecord(run.context, record); err != nil {
		return err
	}
	run.sessionSettled = true
	run.sessionRetained = true
	run.releaseHostLock = false
	return run.syncEvidenceEvents()
}

func (run *freshRunLifecycle) currentOutcome() runOutcome {
	scenario := run.scenario.Name
	if scenario == "" {
		scenario = run.options.ScenarioName
	}
	targetProfile := run.host.TargetProfile
	if targetProfile == "" {
		targetProfile = run.options.TargetProfile
	}
	return runOutcome{
		RunID:            run.manifest.RunID,
		Scenario:         scenario,
		TargetProfile:    targetProfile,
		Host:             run.host.HostID,
		StateDir:         run.context.StateDir,
		EvidenceDir:      run.context.EvidenceDir,
		Result:           run.result,
		Validators:       run.validatorResult,
		StopPolicy:       run.stopPolicy,
		FollowUpCommands: run.followUp,
		Accepted:         run.accepted,
		Failure:          run.failure,
		Warnings:         append([]string(nil), run.warnings...),
	}
}

func effectiveKeepTTL(keepTTL time.Duration) time.Duration {
	if keepTTL > 0 {
		return keepTTL
	}
	return defaultKeepTTL
}

func (run *freshRunLifecycle) finishStoppedRunCleanup() error {
	if err := run.preserveStoppedRunForCleanup(); err != nil {
		return err
	}
	if retention, ok := run.runtime.Broker.(runRetentionBroker); ok && retention.RetentionCapabilities().CleanupRetainedWorkspace {
		if err := retention.CleanupRun(run.context); err != nil {
			return fmt.Errorf("clean up remote run workspace for %s: %w", run.manifest.RunID, err)
		}
	}
	record, err := readRunRetentionRecord(run.repoDir, run.context.StateDir, run.manifest)
	if err != nil {
		return err
	}
	record.StopPhase = "broker-cleaned"
	if err := writeRunRetentionRecord(run.context, record); err != nil {
		return err
	}
	if err := checkpointRunLedgerStopPhase(run.repoDir, run.manifest.RunID, record.StopPhase, run.runtime.Now()); err != nil {
		return err
	}
	if err := run.host.release(); err != nil {
		return fmt.Errorf("release Host Lock for %s: %w", run.host.HostID, err)
	}
	run.releaseHostLock = false
	record.StopPhase = "host-lock-released"
	if err := writeRunRetentionRecord(run.context, record); err != nil {
		return err
	}
	if err := checkpointRunLedgerStopPhase(run.repoDir, run.manifest.RunID, record.StopPhase, run.runtime.Now()); err != nil {
		return err
	}
	if err := cleanupRunState(run.repoDir, run.manifest.RunID); err != nil {
		return fmt.Errorf("clean up Fresh Run state for %s: %w", run.manifest.RunID, err)
	}
	return nil
}

func (run *freshRunLifecycle) preserveStoppedRunForCleanup() error {
	var preservationErr error
	if run.stopHostLockHeartbeat != nil {
		if err := run.stopHostLockHeartbeat(); err != nil {
			preservationErr = errors.Join(preservationErr, fmt.Errorf("renew Host Lock for %s: %w", run.host.HostID, err))
		}
		run.stopHostLockHeartbeat = nil
		run.checkHostLockHeartbeat = nil
	}
	record := newRunRetentionRecord(run.context, run.manifest, run.host.Config, "kept", "cleanup-pending-after-ledger-failure", run.options.AssignedHardDeadline, run.runtime.Now())
	record.StopPhase = "session-stopped"
	record.RemoteSession = retainedSessionRecord{
		BrokerAdapter: run.session.BrokerAdapter,
		SessionID:     run.session.SessionID,
		Status:        "stopped",
	}
	if retention, ok := run.runtime.Broker.(runRetentionBroker); ok {
		record.BrokerCapabilities = retention.RetentionCapabilities()
	}
	if err := writeRunRetentionRecord(run.context, record); err != nil {
		return errors.Join(preservationErr, run.completeCleanupAfterRecoveryFailure(fmt.Errorf("record stopped run cleanup recovery for %s: %w", run.manifest.RunID, err)))
	}
	if err := run.host.markKept(run.manifest.RunID); err != nil {
		return errors.Join(preservationErr, run.completeCleanupAfterRecoveryFailure(fmt.Errorf("mark stopped run cleanup recovery for %s: %w", run.manifest.RunID, err)))
	}
	run.releaseHostLock = false
	return preservationErr
}

func (run *freshRunLifecycle) completeCleanupAfterRecoveryFailure(recoveryErr error) error {
	if err := checkpointRunLedgerStopPhase(run.repoDir, run.manifest.RunID, "session-stopped", run.runtime.Now()); err != nil {
		run.releaseHostLock = false
		return errors.Join(recoveryErr, fmt.Errorf("checkpoint stopped cleanup recovery for %s: %w", run.manifest.RunID, err))
	}
	if retention, ok := run.runtime.Broker.(runRetentionBroker); ok && retention.RetentionCapabilities().CleanupRetainedWorkspace {
		if err := retention.CleanupRun(run.context); err != nil {
			run.releaseHostLock = false
			return errors.Join(recoveryErr, fmt.Errorf("clean up remote run workspace for %s after recovery failure: %w", run.manifest.RunID, err))
		}
	}
	if err := checkpointRunLedgerStopPhase(run.repoDir, run.manifest.RunID, "broker-cleaned", run.runtime.Now()); err != nil {
		run.releaseHostLock = false
		return errors.Join(recoveryErr, fmt.Errorf("checkpoint cleaned broker recovery for %s: %w", run.manifest.RunID, err))
	}
	if err := run.host.release(); err != nil {
		run.releaseHostLock = false
		return errors.Join(recoveryErr, fmt.Errorf("release Host Lock for %s after recovery failure: %w", run.host.HostID, err))
	}
	run.releaseHostLock = false
	if err := checkpointRunLedgerStopPhase(run.repoDir, run.manifest.RunID, "host-lock-released", run.runtime.Now()); err != nil {
		return errors.Join(recoveryErr, fmt.Errorf("checkpoint released Host Lock recovery for %s: %w", run.manifest.RunID, err))
	}
	if err := cleanupRunState(run.repoDir, run.manifest.RunID); err != nil {
		return errors.Join(recoveryErr, fmt.Errorf("clean up Fresh Run state for %s after recovery failure: %w", run.manifest.RunID, err))
	}
	return recoveryErr
}

func (run *freshRunLifecycle) syncEvidenceEvents() error {
	if _, err := os.Stat(run.context.EvidenceDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if _, err := os.ReadFile(run.context.EventsPath); err != nil {
		return fmt.Errorf("read run events after stopping run: %w", err)
	}
	if err := copySequencedEvents(run.context.EventsPath, filepath.Join(run.context.EvidenceDir, evidenceEventsFileName)); err != nil {
		return fmt.Errorf("update Evidence Bundle events after stopping run: %w", err)
	}
	return nil
}
