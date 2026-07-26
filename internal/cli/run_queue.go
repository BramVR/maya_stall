package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	controlPlaneQueueVersion      = 1
	maximumControlPlaneQueuedRuns = 1000
)

type controlPlaneQueueRecord struct {
	Version      int                    `json:"version"`
	RunID        string                 `json:"runId"`
	State        string                 `json:"state"`
	AcceptedAt   string                 `json:"acceptedAt"`
	Submission   controlPlaneSubmission `json:"submission"`
	HostPool     string                 `json:"hostPool"`
	Requirements scenarioRequirements   `json:"requiredCapabilities"`
}

type controlPlaneQueueCancelRequest struct {
	Version int `json:"version"`
}

type controlPlaneQueuedRun struct {
	record             controlPlaneQueueRecord
	dispatching        bool
	canceled           bool
	cancellationActive bool
	waiterReleased     bool
	done               chan struct{}
	ready              chan struct{}
	run                *freshRunLifecycle
	selected           *controlPlaneHostAgent
	selectionErr       error
	capabilities       mayaHostCapabilityRecord
	selectedMayaBuild  string
	releaseHostLock    func() error
	lockContended      bool
}

var errQueuedRunCanceled = errors.New("Run canceled while queued")
var errControlPlaneQueueFull = errors.New("Control Plane Run queue is full") //nolint:staticcheck // Product terms preserve the user-facing diagnostic.

func (handler *controlPlaneHandler) loadControlPlaneQueue() error {
	root := filepath.Join(handler.dataDir, "queued-runs")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	removedTemporary := false
	for _, entry := range entries {
		if isQueuedRunAtomicTemporary(entry) {
			if err := os.Remove(filepath.Join(root, entry.Name())); err != nil {
				return err
			}
			removedTemporary = true
			continue
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("invalid queued Run path %s", entry.Name())
		}
		var record controlPlaneQueueRecord
		if err := readPrivateJSON(filepath.Join(root, entry.Name()), &record); err != nil {
			return err
		}
		if record.Version != controlPlaneQueueVersion || validateRunID(record.RunID) != nil || entry.Name() != record.RunID+".json" || (record.State != "admitting" && record.State != "queued" && record.State != "canceling") || record.Submission.Version != controlPlaneAPIVersion || record.Submission.Scenario == "" || record.HostPool == "" {
			return fmt.Errorf("invalid queued Run %s", entry.Name())
		}
		repoDir := filepath.Join(handler.dataDir, "runs", record.RunID, "repo")
		ledgerStore := newRunLedgerStore(repoDir)
		ledger, err := ledgerStore.Read(record.RunID)
		if handler.assignments[record.RunID] != nil {
			if err := handler.removeQueueIntent(record.RunID); err != nil {
				return err
			}
			continue
		}
		if record.State == "admitting" {
			ledgerExists, ledgerPathErr := ledgerStore.Occupied(record.RunID)
			if err != nil && ledgerPathErr == nil && !ledgerExists {
				if cleanupErr := cleanupAbandonedQueueAdmission(repoDir, record.RunID); cleanupErr != nil {
					return cleanupErr
				}
				handler.removeControlPlaneRunID(record.RunID)
				if err := handler.removeQueueIntent(record.RunID); err != nil {
					return err
				}
				continue
			}
			if ledgerPathErr != nil {
				return errors.Join(err, ledgerPathErr)
			}
			if terminalRunLedgerRecord(ledger) {
				if err := handler.removeQueueIntent(record.RunID); err != nil {
					return err
				}
				continue
			}
			if err != nil || ledger.AcceptedAt == "" || ledger.State != "submitted" && ledger.State != "queued" {
				return errors.Join(fmt.Errorf("admitting Run %s has no recoverable durable ledger", record.RunID), err)
			}
			if err := ensureDurableQueuedRunState(repoDir, record, &ledger); err != nil {
				return err
			}
			if err := ensureQueuedEvent(repoDir, record.RunID); err != nil {
				return err
			}
			if ledger.State == "submitted" {
				if err := markControlPlaneRunQueued(repoDir, record.RunID, handler.runtime.Now()); err != nil {
					return err
				}
				ledger.State = "queued"
			}
			if err := ensureDurableQueuedRunState(repoDir, record, &ledger); err != nil {
				return err
			}
			record.State = "queued"
			record.AcceptedAt = ledger.AcceptedAt
			if err := writePrivateJSON(handler.queuePath(record.RunID), record); err != nil {
				return err
			}
		}
		if record.State == "canceling" {
			// A durable cancel intent wins over dispatch after a process restart.
			if err != nil || ledger.AcceptedAt != record.AcceptedAt {
				return errors.Join(fmt.Errorf("canceling Run %s has no matching durable ledger", record.RunID), err)
			}
			if ledger.State == "submitted" {
				if err := ensureQueuedEvent(repoDir, record.RunID); err != nil {
					return err
				}
				if err := markControlPlaneRunQueued(repoDir, record.RunID, handler.runtime.Now()); err != nil {
					return err
				}
				ledger.State = "queued"
			}
			var cancellationErr error
			switch ledger.State {
			case "queued":
				run, loadErr := loadAcceptedHostAgentRun(repoDir, hostAgentAssignmentRecord{RunID: record.RunID, Submission: record.Submission}, handler.runtime)
				if loadErr != nil {
					cancellationErr = loadErr
				} else {
					cancellationErr = finalizeQueuedRunCancellation(run)
				}
			case "canceling", "canceled", "cleanup-failed":
				cancellationErr = cleanupQueuedRunCancellation(repoDir, record.RunID, handler.runtime.Now())
			default:
				return fmt.Errorf("canceling Run %s has invalid durable state %s", record.RunID, ledger.State)
			}
			if cancellationErr != nil {
				handler.queuedRuns[record.RunID] = &controlPlaneQueuedRun{record: record, canceled: true, done: make(chan struct{}), ready: make(chan struct{}, 1)}
				continue
			}
			if err := handler.removeQueueIntent(record.RunID); err != nil {
				return err
			}
			continue
		}
		if err == nil && (terminalRunLedgerRecord(ledger) || ledger.State == "canceled") {
			if err := handler.removeQueueIntent(record.RunID); err != nil {
				return err
			}
			continue
		}
		if err != nil || ledger.AcceptedAt != record.AcceptedAt || ledger.State != "queued" && ledger.State != "submitted" {
			return errors.Join(fmt.Errorf("queued Run %s has no matching durable ledger", record.RunID), err)
		}
		if ledger.State == "submitted" {
			repoDir := filepath.Join(handler.dataDir, "runs", record.RunID, "repo")
			if err := ensureQueuedEvent(repoDir, record.RunID); err != nil {
				return err
			}
			if err := markControlPlaneRunQueued(repoDir, record.RunID, handler.runtime.Now()); err != nil {
				return err
			}
		}
		if handler.waitingQueueCountLocked() >= maximumControlPlaneQueuedRuns {
			return fmt.Errorf("Control Plane Run queue exceeds %d durable entries", maximumControlPlaneQueuedRuns) //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
		}
		handler.queuedRuns[record.RunID] = &controlPlaneQueuedRun{record: record, done: make(chan struct{}), ready: make(chan struct{}, 1)}
	}
	if removedTemporary {
		return syncRunLedgerDirectory(root)
	}
	return nil
}

func isQueuedRunAtomicTemporary(entry os.DirEntry) bool {
	name := entry.Name()
	marker := strings.Index(name, ".json.tmp-")
	info, err := entry.Info()
	if err != nil || !info.Mode().IsRegular() || marker < 2 || name[0] != '.' || marker+len(".json.tmp-") == len(name) {
		return false
	}
	return validateRunID(name[1:marker]) == nil
}

func (handler *controlPlaneHandler) resumeControlPlaneQueue() {
	handler.mu.Lock()
	records := make([]controlPlaneQueueRecord, 0, len(handler.queuedRuns))
	for _, queued := range handler.queuedRuns {
		if queued.canceled {
			continue
		}
		records = append(records, queued.record)
	}
	handler.mu.Unlock()
	for _, record := range records {
		record := record
		go func() {
			targetProfile := record.Submission.TargetProfile
			if targetProfile == "" {
				targetProfile = "default"
			}
			runtime := handler.runtime
			runtime.Accepted = nil
			runtime.AcceptedCheck = nil
			_, _ = handler.runScenarioThroughHostAgent(
				filepath.Join(handler.dataDir, "runs", record.RunID, "repo"),
				record.Submission,
				runOptions{ScenarioName: record.Submission.Scenario, TargetProfile: targetProfile, StopAfter: record.Submission.StopAfter, KeepTTL: keepTTLOrDefault(record.Submission.KeepTTL), AssignedRunID: record.RunID, SharedFakeWorkRoot: filepath.Join(handler.dataDir, "fake-host"), KeptSessionRepoRoot: filepath.Join(handler.dataDir, "runs")},
				runtime,
			)
		}()
	}
}

func (handler *controlPlaneHandler) queueHostAgentRun(repoDir string, submission controlPlaneSubmission, options runOptions, requirements scenarioRequirements, runtime runRuntime) (*freshRunLifecycle, *controlPlaneHostAgent, mayaHostCapabilityRecord, string, func() error, error) {
	handler.mu.Lock()
	queued := handler.queuedRuns[options.AssignedRunID]
	handler.mu.Unlock()

	var run *freshRunLifecycle
	if queued != nil {
		loaded, err := loadAcceptedHostAgentRun(repoDir, hostAgentAssignmentRecord{RunID: queued.record.RunID, Submission: queued.record.Submission}, runtime)
		if err != nil {
			handler.mu.Lock()
			if handler.queuedRuns[queued.record.RunID] == queued {
				// No waiter remains after this return; keep restart-recoverable ownership inert.
				queued.dispatching = true
				queued.selectionErr = err
			}
			handler.mu.Unlock()
			return nil, nil, mayaHostCapabilityRecord{}, "", nil, err
		}
		run = loaded
		handler.mu.Lock()
		queued.run = run
		handler.mu.Unlock()
	} else {
		// Serialize acceptance through durable insertion so acceptance-time FIFO
		// cannot be bypassed by a later, faster submission.
		handler.queueAdmissionMu.Lock()
		admissionLocked := true
		defer func() {
			if admissionLocked {
				handler.queueAdmissionMu.Unlock()
			}
		}()
		handler.mu.Lock()
		if handler.waitingQueueCountLocked()+handler.queueAdmissions >= maximumControlPlaneQueuedRuns {
			handler.mu.Unlock()
			return nil, nil, mayaHostCapabilityRecord{}, "", nil, errControlPlaneQueueFull
		}
		hostPool, admissionErr := handler.queueAdmissionLocked(targetProfileForSubmission(submission), requirements)
		if admissionErr == nil {
			handler.queueAdmissions++
		}
		handler.mu.Unlock()
		if admissionErr != nil {
			return nil, nil, mayaHostCapabilityRecord{}, "", nil, admissionErr
		}
		admissionReserved := true
		defer func() {
			if admissionReserved {
				handler.mu.Lock()
				handler.queueAdmissions--
				handler.mu.Unlock()
			}
		}()
		acceptedCallback := runtime.Accepted
		acceptedCheck := runtime.AcceptedCheck
		deferred := runtime
		deferred.Accepted = nil
		deferred.AcceptedCheck = nil
		record := controlPlaneQueueRecord{
			Version: controlPlaneQueueVersion, RunID: options.AssignedRunID, State: "admitting",
			Submission: submission, HostPool: hostPool, Requirements: requirements,
		}
		if err := writePrivateJSON(handler.queuePath(record.RunID), record); err != nil {
			return nil, nil, mayaHostCapabilityRecord{}, "", nil, err
		}
		run = newFreshRun(repoDir, options, deferred).(*freshRunLifecycle)
		if err := run.accept(); err != nil {
			return run, nil, mayaHostCapabilityRecord{}, "", nil, errors.Join(err, run.cleanupUnacceptedOwnership(), handler.removeQueueIntent(record.RunID))
		}
		acceptancePublished := false
		publishAcceptance := func() {
			if acceptancePublished {
				return
			}
			if admissionLocked {
				handler.queueAdmissionMu.Unlock()
				admissionLocked = false
			}
			acceptancePublished = true
			if acceptedCallback != nil {
				acceptedCallback(run.currentOutcome())
			}
			if acceptedCheck != nil {
				_ = acceptedCheck()
			}
		}
		if err := appendEvent(run.context.EventsPath, "run.queued", "awaiting-host-assignment"); err != nil {
			publishAcceptance()
			return run, nil, mayaHostCapabilityRecord{}, "", nil, err
		}
		if err := markControlPlaneRunQueued(repoDir, record.RunID, runtime.Now()); err != nil {
			publishAcceptance()
			return run, nil, mayaHostCapabilityRecord{}, "", nil, err
		}
		if err := ensureDurableQueuedRunState(repoDir, record, nil); err != nil {
			publishAcceptance()
			return run, nil, mayaHostCapabilityRecord{}, "", nil, err
		}
		record.State = "queued"
		record.AcceptedAt = run.acceptedAt.Format(time.RFC3339Nano)
		if err := writePrivateJSON(handler.queuePath(record.RunID), record); err != nil {
			publishAcceptance()
			return run, nil, mayaHostCapabilityRecord{}, "", nil, err
		}
		handler.mu.Lock()
		handler.queueAdmissions--
		admissionReserved = false
		handler.queuedRuns[record.RunID] = &controlPlaneQueuedRun{record: record, done: make(chan struct{}), ready: make(chan struct{}, 1)}
		queued = handler.queuedRuns[record.RunID]
		queued.run = run
		handler.mu.Unlock()
		handler.queueAdmissionMu.Unlock()
		admissionLocked = false
		// Publish acceptance only after restart can recover the durable queue owner.
		publishAcceptance()
	}

	if queued == nil {
		handler.mu.Lock()
		queued = handler.queuedRuns[run.manifest.RunID]
		handler.mu.Unlock()
	}
	for {
		handler.mu.Lock()
		if queued.ready == nil {
			queued.ready = make(chan struct{}, 1)
		}
		handler.startQueueSchedulerLocked()
		selected, capabilities, build, release, selectionErr := handler.queuedSelectionLocked(run.manifest.RunID)
		handler.mu.Unlock()
		if selectionErr != nil {
			return run, nil, mayaHostCapabilityRecord{}, "", nil, selectionErr
		}
		if selected != nil {
			return run, selected, capabilities, build, release, nil
		}
		select {
		case <-queued.done:
			return run, nil, mayaHostCapabilityRecord{}, "", nil, errQueuedRunCanceled
		case <-queued.ready:
		}
	}
}

func (handler *controlPlaneHandler) queueAdmissionLocked(targetProfile string, requirements scenarioRequirements) (string, error) {
	now := handler.runtime.Now()
	hostPools := make(map[string]bool)
	for _, agent := range handler.hostAgents {
		if agent == nil || agent.status.SessionID == "" || agent.status.Slots != 1 || !agent.status.SessionBinding || !agent.status.DeadlineActions || !now.Before(agent.sessionExpiresAt) {
			continue
		}
		switch agent.status.State {
		case "ready", "reserving", "locked", "running":
		default:
			continue
		}
		if containsExactString(agent.status.Capabilities.TargetProfiles, targetProfile) && decideMayaHostCompatibility(requirements, agent.status.Capabilities, now).Compatible {
			if pool := agent.status.Capabilities.TargetProfileHostPools[targetProfile]; pool != "" {
				hostPools[pool] = true
			}
		}
	}
	if len(hostPools) == 1 {
		for pool := range hostPools {
			return pool, nil
		}
	}
	if len(hostPools) > 1 {
		pools := make([]string, 0, len(hostPools))
		for pool := range hostPools {
			pools = append(pools, pool)
		}
		sort.Strings(pools)
		return "", fmt.Errorf("Target Profile %s has conflicting Host Pool reports: %s", targetProfile, strings.Join(pools, ", ")) //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
	}
	_, reasons := compatibleHostAgentCandidates(handler.hostAgents, targetProfile, requirements, now)
	reason := "no registered ready Windows Host Agent is compatible"
	if len(reasons) > 0 {
		reason += ": " + strings.Join(reasons, "; ")
	} else {
		reason += ": compatible capability reports do not identify the Target Profile Host Pool"
	}
	return "", errors.New(reason)
}

func (handler *controlPlaneHandler) selectQueuedRun(runID string) (*controlPlaneHostAgent, mayaHostCapabilityRecord, string, func() error, error) {
	handler.dispatchQueuedRuns()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return handler.queuedSelectionLocked(runID)
}

func (handler *controlPlaneHandler) dispatchQueuedRuns() {
	handler.queueDispatchMu.Lock()
	defer handler.queueDispatchMu.Unlock()

	handler.mu.Lock()
	handler.queueDispatchCycles++
	now := handler.runtime.Now()
	for _, queued := range handler.queuedRuns {
		queued.lockContended = false
	}
	handler.expireReadyHostAgentSessionsLocked(now)
	hosts := make([]*controlPlaneHostAgent, 0, len(handler.hostAgents))
	for _, agent := range handler.hostAgents {
		if agent != nil && agent.status.State == "ready" && agent.status.RunID == "" && agent.status.SessionID != "" && agent.status.Slots == 1 && agent.status.SessionBinding && agent.status.DeadlineActions && now.Before(agent.sessionExpiresAt) {
			hosts = append(hosts, agent)
		}
	}
	sort.Slice(hosts, func(i, j int) bool {
		if hosts[i].status.HostID != hosts[j].status.HostID {
			return hosts[i].status.HostID < hosts[j].status.HostID
		}
		return hosts[i].status.AgentID < hosts[j].status.AgentID
	})
	handler.mu.Unlock()

	selectionErrors := make(map[*controlPlaneQueuedRun]error)
	// Each Host claims the earliest compatible Run; earlier incompatible Runs do not idle capacity.
	for _, host := range hosts {
		handler.mu.Lock()
		now = handler.runtime.Now()
		if handler.hostAgents[host.status.AgentID] != host || host.status.State != "ready" || host.status.RunID != "" || host.status.SessionID == "" || host.status.Slots != 1 || !host.status.SessionBinding || !host.status.DeadlineActions || !now.Before(host.sessionExpiresAt) {
			handler.mu.Unlock()
			continue
		}
		var candidate *controlPlaneQueuedRun
		for _, item := range handler.orderedQueuedRunsLocked() {
			profile := queueTargetProfile(item.record)
			if item.dispatching || item.canceled || !containsExactString(host.status.Capabilities.TargetProfiles, profile) || host.status.Capabilities.TargetProfileHostPools[profile] != item.record.HostPool {
				continue
			}
			if decideMayaHostCompatibility(item.record.Requirements, host.status.Capabilities, now).Compatible {
				candidate = item
				break
			}
		}
		if candidate == nil {
			handler.mu.Unlock()
			continue
		}
		candidate.dispatching = true
		host.status.State = "reserving"
		agentID := host.status.AgentID
		hostID := host.status.HostID
		sessionID := host.status.SessionID
		runID := candidate.record.RunID
		handler.mu.Unlock()

		release, locked, err := handler.queueAcquireHostLock(filepath.Join(handler.dataDir, "fake-host"), hostID)

		handler.mu.Lock()
		now = handler.runtime.Now()
		profile := queueTargetProfile(candidate.record)
		decision := decideMayaHostCompatibility(candidate.record.Requirements, host.status.Capabilities, now)
		candidateClaimCurrent := handler.queuedRuns[runID] == candidate && candidate.dispatching && !candidate.canceled && candidate.selected == nil
		hostClaimCurrent := handler.hostAgents[agentID] == host && host.status.State == "reserving" && host.status.RunID == "" && host.status.SessionID == sessionID
		claimCurrent := candidateClaimCurrent && hostClaimCurrent
		eligible := claimCurrent && now.Before(host.sessionExpiresAt) && containsExactString(host.status.Capabilities.TargetProfiles, profile) && host.status.Capabilities.TargetProfileHostPools[profile] == candidate.record.HostPool && decision.Compatible
		if err != nil {
			if hostClaimCurrent {
				handler.resetQueueHostClaimLocked(host, sessionID, now)
			}
			if candidateClaimCurrent {
				candidate.dispatching = false
			}
			if selectionErrors[candidate] == nil {
				selectionErrors[candidate] = fmt.Errorf("acquire Host Lock for %s: %w", hostID, err)
			}
			handler.mu.Unlock()
			continue
		}
		if locked {
			if hostClaimCurrent {
				handler.resetQueueHostClaimLocked(host, sessionID, now)
			}
			if candidateClaimCurrent {
				candidate.dispatching = false
				candidate.lockContended = true
			}
			handler.mu.Unlock()
			continue
		}
		if !eligible {
			if hostClaimCurrent {
				handler.resetQueueHostClaimLocked(host, sessionID, now)
			}
			if candidateClaimCurrent {
				candidate.dispatching = false
			}
			handler.mu.Unlock()
			releaseErr := release()
			if releaseErr != nil {
				handler.mu.Lock()
				if handler.queuedRuns[runID] == candidate && !candidate.canceled && !candidate.dispatching {
					selectionErrors[candidate] = fmt.Errorf("release changed Host Lock for %s: %w", hostID, releaseErr)
				}
				handler.mu.Unlock()
			}
			continue
		}
		candidate.selectionErr = nil
		candidate.lockContended = false
		candidate.selected = host
		candidate.capabilities = snapshotMayaHostCapabilityRecord(host.status.Capabilities)
		candidate.selectedMayaBuild = decision.SelectedMayaBuild
		candidate.releaseHostLock = release
		delete(selectionErrors, candidate)
		handler.signalQueuedRunLocked(candidate)
		handler.mu.Unlock()
	}
	handler.mu.Lock()
	for candidate, selectionErr := range selectionErrors {
		if candidate.selected != nil || candidate.dispatching || candidate.canceled || candidate.lockContended || handler.queueHasCompatibleBusyHostLocked(candidate, handler.runtime.Now()) {
			continue
		}
		candidate.selectionErr = selectionErr
		candidate.dispatching = true
		handler.signalQueuedRunLocked(candidate)
	}
	handler.mu.Unlock()
}

func (handler *controlPlaneHandler) expireReadyHostAgentSessionsLocked(now time.Time) {
	for _, agent := range handler.hostAgents {
		if agent.status.State == "ready" && agent.status.SessionID != "" && !now.Before(agent.sessionExpiresAt) {
			agent.status.State = "offline"
			agent.status.SessionID = ""
			agent.sessionExpiresAt = time.Time{}
			_ = handler.persistHostAgentStatus(agent)
		}
	}
}

func (handler *controlPlaneHandler) queueHasCompatibleBusyHostLocked(queued *controlPlaneQueuedRun, now time.Time) bool {
	targetProfile := queueTargetProfile(queued.record)
	for _, agent := range handler.hostAgents {
		if agent == nil {
			continue
		}
		switch agent.status.State {
		case "reserving", "locked", "running":
		default:
			continue
		}
		if agent.status.SessionID == "" || agent.status.Slots != 1 || !agent.status.SessionBinding || !agent.status.DeadlineActions || !now.Before(agent.sessionExpiresAt) || !containsExactString(agent.status.Capabilities.TargetProfiles, targetProfile) || agent.status.Capabilities.TargetProfileHostPools[targetProfile] != queued.record.HostPool {
			continue
		}
		if decideMayaHostCompatibility(queued.record.Requirements, agent.status.Capabilities, now).Compatible {
			return true
		}
	}
	return false
}

func (handler *controlPlaneHandler) resetQueueHostClaimLocked(host *controlPlaneHostAgent, sessionID string, now time.Time) {
	if host.status.State != "reserving" || host.status.RunID != "" || host.status.SessionID != sessionID {
		return
	}
	if sessionID == "" || !now.Before(host.sessionExpiresAt) {
		host.status.State = "offline"
		host.status.SessionID = ""
		host.sessionExpiresAt = time.Time{}
		_ = handler.persistHostAgentStatus(host)
		return
	}
	host.status.State = "ready"
}

func (handler *controlPlaneHandler) queuedSelectionLocked(runID string) (*controlPlaneHostAgent, mayaHostCapabilityRecord, string, func() error, error) {
	queued := handler.queuedRuns[runID]
	if queued != nil && queued.selected != nil {
		return queued.selected, queued.capabilities, queued.selectedMayaBuild, queued.releaseHostLock, nil
	}
	if queued != nil && queued.selectionErr != nil {
		return nil, mayaHostCapabilityRecord{}, "", nil, queued.selectionErr
	}
	return nil, mayaHostCapabilityRecord{}, "", nil, nil
}

func (handler *controlPlaneHandler) startQueueSchedulerLocked() {
	if handler.queueSchedulerRunning || handler.waitingQueueCountLocked() == 0 {
		return
	}
	handler.queueSchedulerRunning = true
	go handler.runQueueScheduler()
}

func (handler *controlPlaneHandler) runQueueScheduler() {
	ticker := time.NewTicker(controlPlaneEventPollInterval)
	defer ticker.Stop()
	for {
		handler.mu.Lock()
		if handler.waitingQueueCountLocked() == 0 {
			handler.queueSchedulerRunning = false
			handler.mu.Unlock()
			return
		}
		handler.mu.Unlock()
		handler.dispatchQueuedRuns()
		<-ticker.C
	}
}

func (handler *controlPlaneHandler) signalQueuedRunLocked(queued *controlPlaneQueuedRun) {
	if queued.ready == nil {
		queued.ready = make(chan struct{}, 1)
	}
	select {
	case queued.ready <- struct{}{}:
	default:
	}
}

func (handler *controlPlaneHandler) orderedQueuedRunsLocked() []*controlPlaneQueuedRun {
	queue := make([]*controlPlaneQueuedRun, 0, len(handler.queuedRuns))
	accepted := make(map[*controlPlaneQueuedRun]time.Time, len(handler.queuedRuns))
	for _, queued := range handler.queuedRuns {
		if queued.canceled {
			continue
		}
		queue = append(queue, queued)
		if timestamp, err := time.Parse(time.RFC3339Nano, queued.record.AcceptedAt); err == nil {
			accepted[queued] = timestamp
		}
	}
	sort.Slice(queue, func(i, j int) bool {
		left, leftOK := accepted[queue[i]]
		right, rightOK := accepted[queue[j]]
		if leftOK && rightOK && !left.Equal(right) {
			return left.Before(right)
		}
		if leftOK != rightOK {
			return leftOK
		}
		return queue[i].record.RunID < queue[j].record.RunID
	})
	return queue
}

func (handler *controlPlaneHandler) waitingQueueCountLocked() int {
	count := 0
	for _, queued := range handler.queuedRuns {
		if !queued.canceled {
			count++
		}
	}
	return count
}

func (handler *controlPlaneHandler) resetQueuedDispatchLocked(runID string) {
	queued := handler.queuedRuns[runID]
	if queued == nil {
		return
	}
	queued.dispatching = false
	queued.selected = nil
	queued.selectionErr = nil
	queued.capabilities = mayaHostCapabilityRecord{}
	queued.selectedMayaBuild = ""
	queued.releaseHostLock = nil
	queued.lockContended = false
}

func (handler *controlPlaneHandler) refreshQueuedReservationCapabilitiesLocked(runID string, targetProfile string, requirements scenarioRequirements, selected *controlPlaneHostAgent, assignment *hostAgentAssignmentRecord) error {
	queued := handler.queuedRuns[runID]
	current := selected.status.Capabilities
	decision := decideMayaHostCompatibility(requirements, current, handler.runtime.Now())
	if queued == nil || !containsExactString(current.TargetProfiles, targetProfile) || current.TargetProfileHostPools[targetProfile] != queued.record.HostPool || !decision.Compatible {
		return errors.New("Windows Host Agent capabilities changed during reservation") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	assignment.Capabilities = snapshotMayaHostCapabilityRecord(current)
	assignment.SelectedMayaBuild = decision.SelectedMayaBuild
	return nil
}

func queueTargetProfile(record controlPlaneQueueRecord) string {
	if record.Submission.TargetProfile == "" {
		return "default"
	}
	return record.Submission.TargetProfile
}

func (handler *controlPlaneHandler) addControlPlaneQueueStatus(status *controlPlaneStatusResponse) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	queued := handler.queuedRuns[status.RunID]
	if queued == nil {
		return
	}
	if queued.canceled && queued.record.State != "canceling" {
		switch status.State {
		case "completed", "failed", "canceled", "cleanup-failed", "kept":
			return
		}
	}
	if queued.record.State == "canceling" {
		if status.State == "canceled" || status.State == "cleanup-failed" {
			return
		}
		status.State = "canceling"
		status.Status = ""
		status.Host = ""
		status.CleanupState = "pending"
		status.HostPool = queued.record.HostPool
		requirements := queued.record.Requirements
		status.Requirements = &requirements
		return
	}
	status.State = "queued"
	status.Status = ""
	status.Host = ""
	status.CleanupState = "not-required"
	status.HostPool = queued.record.HostPool
	requirements := queued.record.Requirements
	status.Requirements = &requirements
	for index, item := range handler.orderedQueuedRunsLocked() {
		if item.record.RunID == status.RunID {
			status.QueuePosition = index + 1
			break
		}
	}
	status.WaitReason = handler.queueWaitReasonLocked(queued)
}

func targetProfileForSubmission(submission controlPlaneSubmission) string {
	if submission.TargetProfile == "" {
		return "default"
	}
	return submission.TargetProfile
}

func ensureQueuedEvent(repoDir string, runID string) error {
	stateEvents := filepath.Join(repoDir, ".maya-stall", "state", "runs", runID, "events.jsonl")
	content, err := os.ReadFile(stateEvents)
	if err != nil {
		return err
	}
	if bytes.Contains(content, []byte(`"type":"run.queued"`)) {
		return nil
	}
	_, content, err = newRunLedgerStore(repoDir).ReadEvents(runID)
	if err != nil {
		return err
	}
	if bytes.Contains(content, []byte(`"type":"run.queued"`)) {
		return nil
	}
	return appendEvent(stateEvents, "run.queued", "awaiting-host-assignment")
}

func (handler *controlPlaneHandler) queueWaitReasonLocked(queued *controlPlaneQueuedRun) string {
	if queued.lockContended {
		return "compatible-hosts-busy"
	}
	now := handler.runtime.Now()
	targetProfile := queueTargetProfile(queued.record)
	compatibleHostBusy := false
	for _, agent := range handler.hostAgents {
		if agent == nil || agent.status.State == "offline" || agent.status.State == "quarantined" || agent.status.SessionID == "" || agent.status.Slots != 1 || !agent.status.SessionBinding || !agent.status.DeadlineActions || !now.Before(agent.sessionExpiresAt) || !containsExactString(agent.status.Capabilities.TargetProfiles, targetProfile) || agent.status.Capabilities.TargetProfileHostPools[targetProfile] != queued.record.HostPool {
			continue
		}
		if decideMayaHostCompatibility(queued.record.Requirements, agent.status.Capabilities, now).Compatible {
			if agent.status.State == "ready" {
				return "awaiting-host-assignment"
			}
			compatibleHostBusy = true
		}
	}
	if compatibleHostBusy {
		return "compatible-hosts-busy"
	}
	return "waiting-for-compatible-host"
}

func markControlPlaneRunQueued(repoDir string, runID string, now time.Time) error {
	store := newRunLedgerStore(repoDir)
	return store.UpdateWithArtifacts(runID, func(record *runLedgerRecord) error {
		record.State = "queued"
		record.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		return nil
	})
}

func (handler *controlPlaneHandler) queuePath(runID string) string {
	return filepath.Join(handler.dataDir, "queued-runs", runID+".json")
}

func (handler *controlPlaneHandler) removeQueuedRunLocked(runID string) error {
	if err := handler.removeQueueIntent(runID); err != nil {
		if queued := handler.queuedRuns[runID]; queued != nil {
			// Keep a failed durable-intent cleanup inert until restart reconciliation.
			queued.canceled = true
		}
		return err
	}
	delete(handler.queuedRuns, runID)
	return nil
}

func (handler *controlPlaneHandler) removeQueueIntent(runID string) error {
	if err := os.Remove(handler.queuePath(runID)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRunLedgerDirectory(filepath.Join(handler.dataDir, "queued-runs"))
}

func cleanupAbandonedQueueAdmission(repoDir string, runID string) error {
	cleanupErr := cleanupRunState(repoDir, runID)
	evidenceDir := filepath.Join(repoDir, "artifacts", "maya-stall", runID)
	if err := os.Remove(evidenceDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	cleanupErr = errors.Join(cleanupErr, newRunLedgerStore(repoDir).Remove(runID))
	if cleanupErr != nil {
		return cleanupErr
	}
	runRoot := filepath.Dir(repoDir)
	if err := os.RemoveAll(runRoot); err != nil {
		return err
	}
	return syncRunLedgerDirectory(filepath.Dir(runRoot))
}

func ensureDurableQueuedRunState(repoDir string, queue controlPlaneQueueRecord, durable *runLedgerRecord) error {
	snapshot, err := newRunLedgerStore(repoDir).Snapshot(queue.RunID)
	if err != nil {
		return err
	}
	ledger := snapshot.Record
	if durable != nil {
		ledger = *durable
	}
	manifest, stateDir, found, err := readStopRunManifest(repoDir, queue.RunID)
	if err != nil {
		return err
	}
	if !found {
		manifest = runManifest{Version: evidenceSchemaVersion, RunID: queue.RunID, Scenario: ledger.Scenario, TargetProfile: ledger.TargetProfile, Host: ledger.Host}
		if err := writeJSONFile(filepath.Join(stateDir, evidenceManifestFileName), manifest); err != nil {
			return err
		}
	} else if manifest.RunID != queue.RunID || manifest.Scenario != ledger.Scenario {
		return fmt.Errorf("queued Run transient manifest does not match durable ledger")
	}
	for _, artifact := range []struct {
		content []byte
		target  string
	}{
		{snapshot.Events, filepath.Join(stateDir, runLedgerEventsFileName)},
		{snapshot.Log, filepath.Join(stateDir, filepath.FromSlash(evidenceLogPath))},
	} {
		info, err := os.Lstat(artifact.target)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.MkdirAll(filepath.Dir(artifact.target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(artifact.target, artifact.content, 0o644); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("queued Run transient artifact %s must be a regular file", artifact.target)
		}
		if err := syncRunLedgerFile(artifact.target); err != nil {
			return err
		}
	}
	if err := syncRunLedgerFile(filepath.Join(stateDir, evidenceManifestFileName)); err != nil {
		return err
	}
	evidenceDir := filepath.Join(repoDir, "artifacts", "maya-stall", queue.RunID)
	if err := os.MkdirAll(evidenceDir, 0o755); err != nil {
		return err
	}
	return errors.Join(syncRunLedgerDirectoryChain(stateDir, repoDir), syncRunLedgerDirectoryChain(evidenceDir, repoDir))
}

func (handler *controlPlaneHandler) serveQueuedRunCancel(response http.ResponseWriter, request *http.Request) bool {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if request.Method != http.MethodPost || len(parts) != 4 || parts[0] != "v1" || parts[1] != "runs" || parts[3] != "cancel" {
		return false
	}
	runID := parts[2]
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Run ID")
		return true
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 1025))
	if err != nil || len(body) > 1024 {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid queued Run cancellation")
		return true
	}
	var cancellation controlPlaneQueueCancelRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cancellation) != nil || cancellation.Version != controlPlaneAPIVersion || !errors.Is(decoder.Decode(&struct{}{}), io.EOF) {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid queued Run cancellation")
		return true
	}
	status, err := handler.cancelQueuedRun(runID)
	if err != nil {
		if errors.Is(err, errQueuedRunNotFound) {
			status, err = handler.requestKeptSessionStop(runID)
		}
	}
	if err != nil {
		if errors.Is(err, errQueuedRunDispatching) {
			writeControlPlaneError(response, http.StatusConflict, "queued Run assignment has started")
		} else if errors.Is(err, errQueuedRunCanceling) {
			writeControlPlaneError(response, http.StatusConflict, "queued Run cancellation is already in progress")
		} else if errors.Is(err, errQueuedRunNotFound) {
			writeControlPlaneError(response, http.StatusConflict, "run is not queued")
		} else {
			writeControlPlaneError(response, http.StatusInternalServerError, "cancel queued run")
		}
		return true
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(status)
	return true
}

func (handler *controlPlaneHandler) requestKeptSessionStop(runID string) (controlPlaneStatusResponse, error) {
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	if assignment == nil {
		handler.mu.Unlock()
		return controlPlaneStatusResponse{}, errQueuedRunNotFound
	}
	handler.mu.Unlock()
	assignment.checkpointMu.Lock()
	defer assignment.checkpointMu.Unlock()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	assignment = handler.assignments[runID]
	if assignment == nil || assignment.record.State != "kept" && (assignment.record.State != "expiring" || assignment.record.ExpiryFromState != "kept" || assignment.record.KeepDeadline == "") {
		return controlPlaneStatusResponse{}, errQueuedRunNotFound
	}
	if assignment.record.State == "kept" {
		previous := assignment.record
		assignment.record.ExpiryFromState = assignment.record.State
		assignment.record.State = "expiring"
		handler.appendHostLockEventLocked(assignment, "kept-session.stop-requested", "authorized explicit stop")
		if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
			assignment.record = previous
			return controlPlaneStatusResponse{}, err
		}
	}
	agent := handler.hostAgents[assignment.record.AgentID]
	if agent != nil {
		agent.status.State = "cleaning"
		if err := handler.persistHostAgentStatus(agent); err != nil {
			return controlPlaneStatusResponse{}, err
		}
		handler.signalHostAgent(agent)
	}
	record := *assignment.record.AssignedLedger
	status := controlPlaneStatusFromRecord(assignment.repoDir, record, "/v1/runs/"+runID+"/evidence")
	status.State = "expiring"
	status.CleanupState = "pending"
	status.KeepDeadline = assignment.record.KeepDeadline
	status.KeepRemaining = hostLockDeadlineRemaining(assignment.record.KeepDeadline, handler.runtime.Now())
	status.IdleDeadline = assignment.record.IdleDeadline
	status.HardDeadline = assignment.record.HardDeadline
	return status, nil
}

var (
	errQueuedRunDispatching = errors.New("queued Run is dispatching")
	errQueuedRunCanceling   = errors.New("queued Run cancellation is in progress")
	errQueuedRunNotFound    = errors.New("queued Run not found")
)

func (handler *controlPlaneHandler) cancelQueuedRun(runID string) (controlPlaneStatusResponse, error) {
	handler.mu.Lock()
	handler.expireReadyHostAgentSessionsLocked(handler.runtime.Now())
	queued := handler.queuedRuns[runID]
	if queued == nil {
		handler.mu.Unlock()
		return controlPlaneStatusResponse{}, errQueuedRunNotFound
	}
	if queued.dispatching {
		handler.mu.Unlock()
		return controlPlaneStatusResponse{}, errQueuedRunDispatching
	}
	if queued.cancellationActive {
		handler.mu.Unlock()
		return controlPlaneStatusResponse{}, errQueuedRunCanceling
	}
	if !queued.canceled {
		queued.canceled = true
		queued.record.State = "canceling"
		// Persist the intent before terminal evidence so restart can never dispatch a half-canceled Run.
		if err := writePrivateJSON(handler.queuePath(runID), queued.record); err != nil {
			queued.canceled = false
			queued.record.State = "queued"
			handler.mu.Unlock()
			return controlPlaneStatusResponse{}, err
		}
	}
	queued.cancellationActive = true
	run := queued.run
	record := queued.record
	handler.mu.Unlock()

	repoDir := filepath.Join(handler.dataDir, "runs", runID, "repo")
	err := handler.completeQueuedRunCancellation(repoDir, record, run)
	if err != nil {
		ledger, ledgerErr := newRunLedgerStore(repoDir).Read(runID)
		terminalCancellation := ledgerErr == nil && (ledger.State == "canceled" || ledger.State == "cleanup-failed")
		handler.mu.Lock()
		queued.cancellationActive = false
		if terminalCancellation {
			handler.releaseQueuedWaiterLocked(queued)
		}
		handler.mu.Unlock()
		return controlPlaneStatusResponse{}, err
	}
	handler.mu.Lock()
	removeErr := handler.removeQueueIntent(runID)
	if removeErr == nil {
		delete(handler.queuedRuns, runID)
		handler.releaseQueuedWaiterLocked(queued)
	} else {
		queued.cancellationActive = false
		// Terminal cancellation owns the submitter outcome even if intent cleanup needs retry.
		handler.releaseQueuedWaiterLocked(queued)
	}
	handler.mu.Unlock()
	if removeErr != nil {
		return controlPlaneStatusResponse{}, removeErr
	}
	ledger, err := newRunLedgerStore(repoDir).Read(runID)
	if err != nil {
		return controlPlaneStatusResponse{}, err
	}
	return controlPlaneStatusFromRecord(repoDir, ledger, "/v1/runs/"+runID+"/evidence"), nil
}

func (handler *controlPlaneHandler) releaseQueuedWaiterLocked(queued *controlPlaneQueuedRun) {
	if queued.waiterReleased {
		return
	}
	close(queued.done)
	queued.waiterReleased = true
}

func (handler *controlPlaneHandler) completeQueuedRunCancellation(repoDir string, record controlPlaneQueueRecord, run *freshRunLifecycle) error {
	ledger, err := newRunLedgerStore(repoDir).Read(record.RunID)
	if err != nil {
		return err
	}
	if ledger.State == "canceling" || ledger.State == "canceled" || ledger.State == "cleanup-failed" {
		return cleanupQueuedRunCancellation(repoDir, record.RunID, handler.runtime.Now())
	}
	if run == nil {
		run, err = loadAcceptedHostAgentRun(repoDir, hostAgentAssignmentRecord{RunID: record.RunID, Submission: record.Submission}, handler.runtime)
		if err != nil {
			return err
		}
	}
	return finalizeQueuedRunCancellation(run)
}

func finalizeQueuedRunCancellation(run *freshRunLifecycle) error {
	if err := prepareQueuedRunCancellation(run); err != nil {
		return err
	}
	return cleanupQueuedRunCancellation(run.repoDir, run.manifest.RunID, run.runtime.Now())
}

func prepareQueuedRunCancellation(run *freshRunLifecycle) error {
	cancelErr := errQueuedRunCanceled
	run.failedLayer = failureLayerHostSelection
	run.result = ScenarioResult{Status: resultStatusFailed, Summary: cancelErr.Error()}
	run.failure = &runFailureEvidence{
		FailedLayer: string(failureLayerHostSelection), Diagnostic: cancelErr.Error(), RemediationHint: "Submit the Scenario again if it should still run.",
		CaptureState: "not-started", CleanupState: "not-required",
	}
	if err := writeQueuedCancellationLog(run.context.LogPath, cancelErr); err != nil {
		return err
	}
	if err := ensureQueuedCancellationEvent(run.context.EventsPath); err != nil {
		return err
	}
	if err := run.writeManifest(); err != nil {
		return err
	}
	if err := writeMinimalEvidenceBundle(run.context, run.manifest, run.result, run.failure); err != nil {
		return err
	}
	if err := newRunLedgerStore(run.repoDir).UpdateWithArtifacts(run.manifest.RunID, func(record *runLedgerRecord) error {
		now := run.runtime.Now().UTC().Format(time.RFC3339Nano)
		record.State = "canceling"
		record.Status = resultStatusFailed
		record.UpdatedAt = now
		record.CompletedAt = ""
		return nil
	}); err != nil {
		return err
	}
	return nil
}

func cleanupQueuedRunCancellation(repoDir string, runID string, now time.Time) error {
	if err := cleanupRunState(repoDir, runID); err != nil {
		ledgerErr := updateQueuedCancellationLedgerState(repoDir, runID, "cleanup-failed", now)
		return errors.Join(fmt.Errorf("clean up canceled queued Run state: %w", err), ledgerErr)
	}
	return updateQueuedCancellationLedgerState(repoDir, runID, "canceled", now)
}

func updateQueuedCancellationLedgerState(repoDir string, runID string, state string, now time.Time) error {
	return newRunLedgerStore(repoDir).Update(runID, func(record *runLedgerRecord) error {
		record.State = state
		record.Status = resultStatusFailed
		record.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		if record.CompletedAt == "" {
			record.CompletedAt = record.UpdatedAt
		}
		return nil
	})
}

func writeQueuedCancellationLog(path string, cancelErr error) error {
	if err := rejectExistingFileLeaf(path); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(fmt.Sprintf("Scenario canceled before assignment: %v\n", cancelErr)), 0o644)
}

func ensureQueuedCancellationEvent(path string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		if json.Unmarshal(line, &event) == nil && event["type"] == "run.canceled" {
			return nil
		}
	}
	return appendEvent(path, "run.canceled", "operator-requested")
}
