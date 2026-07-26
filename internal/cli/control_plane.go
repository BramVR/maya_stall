package cli

import (
	"bytes"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const controlPlaneAPIVersion = 1
const defaultControlPlaneTokenEnv = "MAYA_STALL_CONTROL_PLANE_TOKEN"
const maximumControlPlaneSubmissionBytes = 32 * 1024 * 1024
const maximumControlPlaneSnapshotEstimateBytes = maximumControlPlaneSubmissionBytes - 64*1024
const maximumControlPlaneDefaultResponseBytes = 8 * 1024 * 1024
const maximumControlPlaneLedgerResponseBytes = 6*maximumRunLedgerMaxLogBytes + 1024*1024
const controlPlaneEventPollInterval = 50 * time.Millisecond
const maximumControlPlaneHistoryRuns = 1000
const maximumControlPlaneHistoryScanRuns = 1000

type controlPlaneSubmission struct {
	Version         int                `json:"version"`
	Scenario        string             `json:"scenario"`
	TargetProfile   string             `json:"targetProfile,omitempty"`
	StopAfter       string             `json:"stopAfter"`
	KeepTTL         string             `json:"keepTTL,omitempty"`
	ConfigName      string             `json:"configName"`
	Config          []byte             `json:"config"`
	Files           []controlPlaneFile `json:"files,omitempty"`
	SubmissionError string             `json:"submissionError,omitempty"`
}

type controlPlaneFile struct {
	Path    string `json:"path"`
	Kind    string `json:"kind"`
	Content []byte `json:"content,omitempty"`
}

type controlPlaneStatusResponse struct {
	Version       int                   `json:"version"`
	Kind          string                `json:"kind"`
	RunID         string                `json:"runId"`
	Scenario      string                `json:"scenario"`
	TargetProfile string                `json:"targetProfile,omitempty"`
	Host          string                `json:"host,omitempty"`
	State         string                `json:"state"`
	Status        string                `json:"status,omitempty"`
	CleanupState  string                `json:"cleanupState"`
	AcceptedAt    string                `json:"acceptedAt"`
	Evidence      string                `json:"evidence"`
	QueuePosition int                   `json:"queuePosition,omitempty"`
	WaitReason    string                `json:"waitReason,omitempty"`
	HostPool      string                `json:"hostPool,omitempty"`
	Requirements  *scenarioRequirements `json:"requiredCapabilities,omitempty"`
	KeepDeadline  string                `json:"keepDeadline,omitempty"`
	KeepRemaining string                `json:"keepRemaining,omitempty"`
	IdleDeadline  string                `json:"idleDeadline,omitempty"`
	HardDeadline  string                `json:"hardDeadline,omitempty"`
	ExpiryReason  string                `json:"expiryReason,omitempty"`
}

type controlPlaneKeepExtensionRequest struct {
	Version int    `json:"version"`
	By      string `json:"by"`
}

type controlPlaneEventsResponse struct {
	Version                int              `json:"version"`
	Kind                   string           `json:"kind"`
	RunID                  string           `json:"runId"`
	Events                 []map[string]any `json:"events"`
	RequestedSequence      int              `json:"requestedSequence,omitempty"`
	FirstAvailableSequence int              `json:"firstAvailableSequence,omitempty"`
	NextSequence           int              `json:"nextSequence,omitempty"`
	EventsOmitted          int              `json:"eventsOmitted"`
	EventsTruncated        bool             `json:"eventsTruncated"`
}

type controlPlaneEventStreamRecord struct {
	Version         int            `json:"version"`
	Kind            string         `json:"kind"`
	RunID           string         `json:"runId"`
	Sequence        int            `json:"sequence,omitempty"`
	Event           map[string]any `json:"event,omitempty"`
	EventsOmitted   int            `json:"eventsOmitted,omitempty"`
	EventsTruncated bool           `json:"eventsTruncated,omitempty"`
	NextSequence    int            `json:"nextSequence"`
	State           string         `json:"state,omitempty"`
}

type controlPlaneLogsResponse struct {
	Version   int    `json:"version"`
	Kind      string `json:"kind"`
	RunID     string `json:"runId"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
}

type controlPlaneResultResponse struct {
	Version      int               `json:"version"`
	Kind         string            `json:"kind"`
	RunID        string            `json:"runId"`
	State        string            `json:"state"`
	Status       string            `json:"status"`
	CleanupState string            `json:"cleanupState"`
	Final        bool              `json:"final"`
	Success      bool              `json:"success"`
	Result       ScenarioResult    `json:"result"`
	Validators   []validatorResult `json:"validators,omitempty"`
	Evidence     string            `json:"evidence"`
}

type controlPlaneEvidenceResponse struct {
	Version int            `json:"version"`
	Kind    string         `json:"kind"`
	RunID   string         `json:"runId"`
	Bundle  evidenceBundle `json:"bundle"`
}

type runReadOptions struct {
	RunID                string
	JSON                 bool
	FromSequence         int
	FromSequenceSet      bool
	ControlPlane         string
	ControlPlaneTokenEnv string
}

type controlPlaneServeOptions struct {
	Listen           string
	DataDir          string
	TLSCert          string
	TLSKey           string
	TokenEnv         string
	HostLockIdle     time.Duration
	HostLockLifetime time.Duration
}

func parseControlPlaneServeArgs(args []string, workDir string) (controlPlaneServeOptions, error) {
	options := controlPlaneServeOptions{
		Listen: "127.0.0.1:8443", TokenEnv: defaultControlPlaneTokenEnv,
		HostLockIdle: defaultHostLockIdleTimeout, HostLockLifetime: defaultHostLockHardLifetime,
	}
	for index := 0; index < len(args); index++ {
		flag := args[index]
		switch flag {
		case "--listen", "--data-dir", "--tls-cert", "--tls-key", "--token-env", "--host-lock-idle-timeout", "--host-lock-hard-lifetime":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return controlPlaneServeOptions{}, newUsageError("%s needs a value", flag)
			}
			switch flag {
			case "--listen":
				options.Listen = args[index]
			case "--data-dir":
				options.DataDir = resolveFromRepo(workDir, args[index])
			case "--tls-cert":
				options.TLSCert = resolveFromRepo(workDir, args[index])
			case "--tls-key":
				options.TLSKey = resolveFromRepo(workDir, args[index])
			case "--token-env":
				options.TokenEnv = args[index]
			case "--host-lock-idle-timeout":
				duration, parseErr := time.ParseDuration(args[index])
				if parseErr != nil || duration < minimumHostLockIdleTimeout {
					return controlPlaneServeOptions{}, newUsageError("--host-lock-idle-timeout needs a duration of at least %s", minimumHostLockIdleTimeout)
				}
				options.HostLockIdle = duration
			case "--host-lock-hard-lifetime":
				duration, parseErr := time.ParseDuration(args[index])
				if parseErr != nil || duration <= 0 {
					return controlPlaneServeOptions{}, newUsageError("--host-lock-hard-lifetime needs a positive duration")
				}
				options.HostLockLifetime = duration
			}
		default:
			return controlPlaneServeOptions{}, newUsageError("unknown control-plane serve option %q", flag)
		}
	}
	if options.DataDir == "" {
		return controlPlaneServeOptions{}, newUsageError("control-plane serve needs --data-dir")
	}
	if options.TLSCert == "" || options.TLSKey == "" {
		return controlPlaneServeOptions{}, newUsageError("control-plane serve needs --tls-cert and --tls-key")
	}
	if options.HostLockLifetime <= options.HostLockIdle {
		return controlPlaneServeOptions{}, newUsageError("--host-lock-hard-lifetime must exceed --host-lock-idle-timeout")
	}
	_, port, err := net.SplitHostPort(options.Listen)
	if err != nil {
		return controlPlaneServeOptions{}, newUsageError("--listen needs host:port")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return controlPlaneServeOptions{}, newUsageError("--listen needs a port between 1 and 65535")
	}
	return options, nil
}

func runControlPlaneServer(options controlPlaneServeOptions, runtime runRuntime, stdout io.Writer) error {
	token, ok := os.LookupEnv(options.TokenEnv)
	if !ok || token == "" {
		return fmt.Errorf("control plane token environment variable %s is not set", options.TokenEnv)
	}
	for _, path := range []string{options.TLSCert, options.TLSKey} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("TLS path %s must be a regular file, not a symlink", path)
		}
	}
	handler, err := newControlPlaneHandlerWithPolicy(options.DataDir, token, runtime, hostLockDeadlinePolicy{
		IdleTimeout: options.HostLockIdle, HardLifetime: options.HostLockLifetime,
	})
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "controlPlane: https://%s\ndata: %s\n", options.Listen, options.DataDir)
	if runtime.ControlPlaneServe != nil {
		return runtime.ControlPlaneServe(options, handler)
	}
	server := &http.Server{
		Addr:              options.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Minute,
		IdleTimeout:       time.Minute,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	err = server.ListenAndServeTLS(options.TLSCert, options.TLSKey)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

type controlPlaneHandler struct {
	dataDir               string
	token                 string
	runtime               runRuntime
	mu                    sync.Mutex
	queueAdmissionMu      sync.Mutex
	queueDispatchMu       sync.Mutex
	hostAgents            map[string]*controlPlaneHostAgent
	assignments           map[string]*controlPlaneHostAgentAssignment
	queuedRuns            map[string]*controlPlaneQueuedRun
	queueAdmissions       int
	queueSchedulerRunning bool
	queueDispatchCycles   uint64
	queueAcquireHostLock  func(string, string) (func() error, bool, error)
	removeRejectedRunRoot func(string) error
	hostLockPolicy        hostLockDeadlinePolicy
	runIDs                []string
}

func newControlPlaneHandler(dataDir string, token string, runtime runRuntime) (http.Handler, error) {
	return newControlPlaneHandlerWithPolicy(dataDir, token, runtime, defaultHostLockDeadlinePolicy())
}

func newControlPlaneHandlerWithPolicy(dataDir string, token string, runtime runRuntime, hostLockPolicy hostLockDeadlinePolicy) (http.Handler, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("control plane token must not be empty")
	}
	if dataDir == "" {
		return nil, fmt.Errorf("control plane data directory must not be empty")
	}
	if hostLockPolicy.IdleTimeout <= 0 || hostLockPolicy.HardLifetime <= hostLockPolicy.IdleTimeout {
		return nil, fmt.Errorf("Host Lock deadline policy needs a positive idle timeout and a longer hard lifetime") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	var err error
	dataDir, err = filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	if err := ensureControlPlaneRootDirectory(dataDir); err != nil {
		return nil, err
	}
	if err := ensurePrivateControlPlaneDirectory(filepath.Join(dataDir, "runs")); err != nil {
		return nil, err
	}
	if err := ensurePrivateControlPlaneDirectory(filepath.Join(dataDir, "fake-host")); err != nil {
		return nil, err
	}
	for _, child := range []string{"host-agents", "host-locks", "assignments", "assignment-transactions", "queued-runs", "incoming"} {
		if err := ensurePrivateControlPlaneDirectory(filepath.Join(dataDir, child)); err != nil {
			return nil, err
		}
	}
	runIDs, err := loadControlPlaneRunIndex(dataDir)
	if err != nil {
		return nil, err
	}
	handler := &controlPlaneHandler{
		dataDir: dataDir, token: token, runtime: runtime,
		hostAgents: make(map[string]*controlPlaneHostAgent), assignments: make(map[string]*controlPlaneHostAgentAssignment), queuedRuns: make(map[string]*controlPlaneQueuedRun),
		queueAcquireHostLock: acquireHostLock, removeRejectedRunRoot: os.RemoveAll, hostLockPolicy: hostLockPolicy, runIDs: runIDs,
	}
	if err := handler.loadHostAgentState(); err != nil {
		return nil, err
	}
	if err := handler.loadControlPlaneQueue(); err != nil {
		return nil, err
	}
	handler.resumeControlPlaneQueue()
	return handler, nil
}

func loadControlPlaneRunIndex(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dataDir, "runs"))
	if err != nil {
		return nil, err
	}
	runIDs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && validateRunID(entry.Name()) == nil {
			runIDs = append(runIDs, entry.Name())
		}
	}
	sortControlPlaneRunIDsNewest(runIDs)
	return runIDs, nil
}

func ensureControlPlaneRootDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.MkdirAll(path, 0o700)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("control plane data path %s must be a directory, not a symlink", path)
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	return fmt.Errorf("control plane data path %s must already be private (0700 or stricter)", path)
}

func ensurePrivateControlPlaneDirectory(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("control plane data path %s must be a directory, not a symlink", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("control plane data path %s must already be private (0700 or stricter)", path)
	}
	return nil
}

func (handler *controlPlaneHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if err := handler.observeHostLockDeadlines(); err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Host Lock expiry")
		return
	}
	if handler.serveHostAgentAPI(response, request) {
		return
	}
	provided, bearer := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !bearer || len(provided) != len(handler.token) || subtle.ConstantTimeCompare([]byte(provided), []byte(handler.token)) != 1 {
		writeControlPlaneError(response, http.StatusUnauthorized, "authentication required")
		return
	}
	if request.URL.Path == "/v1/runs" && request.Method == http.MethodGet {
		handler.serveRunHistory(response, request)
		return
	}
	if handler.serveKeptSessionExtension(response, request) {
		return
	}
	if handler.serveQueuedRunCancel(response, request) {
		return
	}
	if request.URL.Path != "/v1/runs" || request.Method != http.MethodPost {
		handler.serveRunRead(response, request)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maximumControlPlaneSubmissionBytes+1))
	if err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "read submission")
		return
	}
	if len(body) > maximumControlPlaneSubmissionBytes {
		writeControlPlaneError(response, http.StatusRequestEntityTooLarge, "submission exceeds size limit")
		return
	}
	var submission controlPlaneSubmission
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&submission); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid JSON submission")
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid JSON submission")
		return
	}
	if submission.Version != controlPlaneAPIVersion || submission.Scenario == "" {
		writeControlPlaneError(response, http.StatusBadRequest, "unsupported submission")
		return
	}
	if !isValidStopAfter(submission.StopAfter) {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Stop Policy")
		return
	}
	keepTTL, err := parseKeepTTL(submission.KeepTTL)
	if err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid kept-session TTL")
		return
	}
	if submission.StopAfter != stopAfterAlways && keepTTL > handler.hostLockPolicy.HardLifetime {
		writeControlPlaneError(response, http.StatusBadRequest, "kept-session TTL exceeds Host Lock hard lifetime")
		return
	}
	if err := validateControlPlaneSubmissionFiles(submission); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid submission files")
		return
	}

	handler.mu.Lock()
	if request.Context().Err() != nil {
		handler.mu.Unlock()
		return
	}
	runID, repoDir, err := handler.reserveRun(submission.Scenario)
	if err != nil {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusInternalServerError, "reserve run")
		return
	}
	if err := materializeControlPlaneSubmission(repoDir, submission); err != nil {
		_ = os.RemoveAll(filepath.Dir(repoDir))
		handler.removeControlPlaneRunID(runID)
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusBadRequest, "invalid submission files")
		return
	}
	handler.mu.Unlock()
	if request.Context().Err() != nil {
		_ = os.RemoveAll(filepath.Dir(repoDir))
		handler.mu.Lock()
		handler.removeControlPlaneRunID(runID)
		handler.mu.Unlock()
		return
	}
	targetProfile := submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	options := runOptions{
		ScenarioName: submission.Scenario, TargetProfile: targetProfile, StopAfter: submission.StopAfter,
		KeepTTL: keepTTLOrDefault(submission.KeepTTL), AssignedRunID: runID, SharedFakeWorkRoot: filepath.Join(handler.dataDir, "fake-host"),
		KeptSessionRepoRoot: filepath.Join(handler.dataDir, "runs"),
	}
	serverRuntime := handler.runtime
	previousAccepted := serverRuntime.Accepted
	previousAcceptedCheck := serverRuntime.AcceptedCheck
	acceptanceStarted := false
	acceptedWritten := false
	encoder := json.NewEncoder(response)
	serverRuntime.Accepted = func(acceptedOutcome runOutcome) {
		acceptanceStarted = true
		accepted := runCommandJSONForOutcome(acceptedOutcome)
		accepted.Kind = "run-accepted"
		accepted.Status = "submitted"
		accepted.StateDir = "/v1/runs/" + runID + "/status"
		accepted.EvidenceDir = "/v1/runs/" + runID + "/evidence"
		response.Header().Set("Content-Type", "application/x-ndjson")
		response.WriteHeader(http.StatusCreated)
		if err := encoder.Encode(accepted); err == nil {
			if err := http.NewResponseController(response).Flush(); err == nil {
				acceptedWritten = true
			}
		}
		if previousAccepted != nil {
			previousAccepted(acceptedOutcome)
		}
	}
	serverRuntime.AcceptedCheck = func() error {
		if previousAcceptedCheck != nil {
			return previousAcceptedCheck()
		}
		return nil
	}
	var outcome runOutcome
	var runErr error
	if submission.SubmissionError != "" {
		outcome, runErr = failAcceptedSubmission(repoDir, options, serverRuntime, errors.New(submission.SubmissionError))
	} else if handler.hasHostAgentEnrollments() {
		outcome, runErr = handler.runScenarioThroughHostAgent(repoDir, submission, options, serverRuntime)
	} else {
		outcome, runErr = runScenario(repoDir, options, serverRuntime)
	}
	if !acceptedWritten {
		if !acceptanceStarted && errors.Is(runErr, errControlPlaneQueueFull) {
			if err := handler.removeRejectedRunRoot(filepath.Dir(repoDir)); err != nil {
				writeControlPlaneError(response, http.StatusInternalServerError, "clean up rejected run")
				return
			}
			handler.mu.Lock()
			handler.removeControlPlaneRunID(runID)
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusTooManyRequests, "run queue is full")
			return
		}
		if !acceptanceStarted {
			writeControlPlaneError(response, http.StatusInternalServerError, "accept run")
		}
		return
	}
	result := controlPlaneTerminalResponse(outcome, runErr, repoDir)
	_ = json.NewEncoder(response).Encode(result)
}

func (handler *controlPlaneHandler) serveKeptSessionExtension(response http.ResponseWriter, request *http.Request) bool {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if request.Method != http.MethodPost || len(parts) != 4 || parts[0] != "v1" || parts[1] != "runs" || parts[3] != "extend" {
		return false
	}
	if !handler.authorizedOperator(request) {
		writeControlPlaneError(response, http.StatusUnauthorized, "authentication required")
		return true
	}
	runID := parts[2]
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Run ID")
		return true
	}
	var extension controlPlaneKeepExtensionRequest
	if err := decodeBoundedControlPlaneJSON(request.Body, 1024, &extension); err != nil || extension.Version != controlPlaneAPIVersion {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Kept Session extension")
		return true
	}
	by, err := time.ParseDuration(extension.By)
	if err != nil || by <= 0 {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Kept Session extension")
		return true
	}
	result, err := handler.extendControlPlaneKeptSession(runID, by)
	if err != nil {
		if errors.Is(err, errControlPlaneRunNotKept) || errors.Is(err, errControlPlaneHostLockExpired) || errors.Is(err, errControlPlaneExtensionRejected) {
			writeControlPlaneError(response, http.StatusConflict, err.Error())
		} else {
			writeControlPlaneError(response, http.StatusInternalServerError, "persist Kept Session extension")
		}
		return true
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(result)
	return true
}

var (
	errControlPlaneRunNotKept        = errors.New("run is not a Kept Session")
	errControlPlaneHostLockExpired   = errors.New("Host Lock deadline expired")      //nolint:staticcheck // Product term starts the user-facing diagnostic.
	errControlPlaneExtensionRejected = errors.New("Kept Session extension rejected") //nolint:staticcheck // Product term starts the user-facing diagnostic.
)

func (handler *controlPlaneHandler) extendControlPlaneKeptSession(runID string, by time.Duration) (controlPlaneStatusResponse, error) {
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	if assignment == nil {
		handler.mu.Unlock()
		return controlPlaneStatusResponse{}, errControlPlaneRunNotKept
	}
	handler.mu.Unlock()
	assignment.checkpointMu.Lock()
	defer assignment.checkpointMu.Unlock()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	assignment = handler.assignments[runID]
	if assignment == nil || assignment.record.State != "kept" {
		return controlPlaneStatusResponse{}, errControlPlaneRunNotKept
	}
	now := handler.runtime.Now()
	if assignment.record.expiryReason(now) != "" {
		return controlPlaneStatusResponse{}, errControlPlaneHostLockExpired
	}
	previous := assignment.record
	if err := assignment.record.extendKept(now, by, handler.hostLockPolicy); err != nil {
		assignment.record = previous
		return controlPlaneStatusResponse{}, errors.Join(errControlPlaneExtensionRejected, err)
	}
	handler.appendHostLockEventLocked(assignment, "kept-session.extended", by.String())
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
		assignment.record = previous
		return controlPlaneStatusResponse{}, err
	}
	record := *assignment.record.AssignedLedger
	result := controlPlaneStatusFromRecord(assignment.repoDir, record, "/v1/runs/"+runID+"/evidence")
	result.KeepDeadline = assignment.record.KeepDeadline
	result.KeepRemaining = hostLockDeadlineRemaining(assignment.record.KeepDeadline, now)
	result.IdleDeadline = assignment.record.IdleDeadline
	result.HardDeadline = assignment.record.HardDeadline
	return result, nil
}

func (handler *controlPlaneHandler) serveRunHistory(response http.ResponseWriter, request *http.Request) {
	handler.mu.Lock()
	indexedRunIDs := append([]string(nil), handler.runIDs...)
	handler.mu.Unlock()
	names, nextBeforeRunID, omittedAtLeast, err := boundedControlPlaneHistoryWindow(indexedRunIDs, request.URL.Query().Get("beforeRunId"))
	if err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid run history cursor")
		return
	}
	options := historyOptions{
		Scenario: request.URL.Query().Get("scenario"), Host: request.URL.Query().Get("host"),
		State: request.URL.Query().Get("state"), Since: request.URL.Query().Get("since"),
	}
	cutoff, err := historySinceCutoff(options.Since, handler.runtime.Now())
	if err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid run history filter")
		return
	}
	records := make([]runLedgerRecord, 0, maximumControlPlaneHistoryRuns)
	for _, runID := range names {
		repoDir := filepath.Join(handler.dataDir, "runs", runID, "repo")
		exists, err := newRunLedgerStore(repoDir).HasRecord(runID)
		if err == nil && !exists {
			continue
		} else if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read run history")
			return
		}
		record, _, _, err := handler.readControlPlaneRunRecord(repoDir, runID)
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read run history")
			return
		}
		matches, err := runMatchesHistoryOptions(record, options, cutoff)
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read run history")
			return
		}
		if !matches {
			continue
		}
		if !appendBoundedControlPlaneHistory(&records, record) {
			if omittedAtLeast == 0 {
				omittedAtLeast = 1
			}
			break
		}
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(runHistoryResponse{
		Version: controlPlaneAPIVersion, Kind: "history", Runs: records,
		RunsOmittedAtLeast: omittedAtLeast, RunsTruncated: omittedAtLeast > 0, NextBeforeRunID: nextBeforeRunID,
	})
}

func boundedControlPlaneHistoryWindow(runIDs []string, beforeRunID string) ([]string, string, int, error) {
	start := 0
	if beforeRunID != "" {
		if validateRunID(beforeRunID) != nil {
			return nil, "", 0, fmt.Errorf("invalid Run ID")
		}
		start = -1
		for index, runID := range runIDs {
			if runID == beforeRunID {
				start = index + 1
				break
			}
		}
		if start < 0 {
			return nil, "", 0, fmt.Errorf("unknown Run ID")
		}
	}
	end := start + maximumControlPlaneHistoryScanRuns
	if end > len(runIDs) {
		end = len(runIDs)
	}
	window := append([]string(nil), runIDs[start:end]...)
	if end == len(runIDs) {
		return window, "", 0, nil
	}
	return window, window[len(window)-1], len(runIDs) - end, nil
}

func (handler *controlPlaneHandler) removeControlPlaneRunID(runID string) {
	for index, indexedRunID := range handler.runIDs {
		if indexedRunID == runID {
			handler.runIDs = append(handler.runIDs[:index], handler.runIDs[index+1:]...)
			return
		}
	}
}

func sortControlPlaneRunIDsNewest(runIDs []string) {
	sort.Slice(runIDs, func(i int, j int) bool {
		left, _ := acceptedAtFromRunID(runIDs[i])
		right, _ := acceptedAtFromRunID(runIDs[j])
		if left.Equal(right) {
			return runIDCollisionOrdinal(runIDs[i]) > runIDCollisionOrdinal(runIDs[j])
		}
		return left.After(right)
	})
}

func appendBoundedControlPlaneHistory(records *[]runLedgerRecord, record runLedgerRecord) bool {
	if len(*records) >= maximumControlPlaneHistoryRuns {
		return false
	}
	*records = append(*records, record)
	return true
}

func controlPlaneTerminalResponse(outcome runOutcome, runErr error, repoDir string) runCommandJSON {
	result := runCommandJSONForOutcome(outcome)
	result.StateDir = "/v1/runs/" + outcome.RunID + "/status"
	result.EvidenceDir = "/v1/runs/" + outcome.RunID + "/evidence"
	result.Diagnostic = sanitizeControlPlaneText(result.Diagnostic, repoDir)
	runDiagnostic := ""
	if runErr != nil {
		runDiagnostic = sanitizeControlPlaneText(runErr.Error(), repoDir)
	}
	if runErr != nil && (result.Status == resultStatusPassed || result.Diagnostic == "" || runDiagnostic != result.Diagnostic) {
		result.Status = resultStatusFailed
		result.FailedLayer = string(failureLayerRunState)
		result.Diagnostic = runDiagnostic
		result.RemediationHint = earlyFailureRemediation(failureLayerRunState)
	}
	return result
}

func (handler *controlPlaneHandler) serveRunRead(response http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if request.Method != http.MethodGet || len(parts) != 4 || parts[0] != "v1" || parts[1] != "runs" {
		http.NotFound(response, request)
		return
	}
	runID := parts[2]
	if err := validateRunID(runID); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Run ID")
		return
	}
	repoDir := filepath.Join(handler.dataDir, "runs", runID, "repo")
	record, assignmentActive, assignmentBacked, err := handler.readControlPlaneRunRecord(repoDir, runID)
	if err != nil {
		var usageErr *usageError
		if errors.As(err, &usageErr) {
			writeControlPlaneError(response, http.StatusNotFound, "run not found")
			return
		}
		writeControlPlaneError(response, http.StatusInternalServerError, "read run status")
		return
	}
	if parts[3] == "evidence" && (assignmentActive || record.State == "submitted" || record.State == "queued") {
		writeControlPlaneError(response, http.StatusConflict, "run evidence is not yet durable")
		return
	}
	followEvents := parts[3] == "events" && request.URL.Query().Get("follow") == "true"
	if (parts[3] == "events" || parts[3] == "logs") && !assignmentBacked && !followEvents && !terminalRunLedgerRecord(record) {
		if err := newRunLedgerStore(repoDir).Refresh(runID, handler.runtime.Now()); err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "refresh active run stream")
			return
		}
		refreshed, _, _, err := handler.readControlPlaneRunRecord(repoDir, runID)
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read active run stream")
			return
		}
		record = refreshed
	}
	switch parts[3] {
	case "status":
		result := controlPlaneStatusFromRecord(repoDir, record, "/v1/runs/"+runID+"/evidence")
		addKeptSessionTTLToStatus(repoDir, runID, handler.runtime.Now(), &result)
		handler.addControlPlaneHostLockDeadlines(runID, &result)
		handler.addControlPlaneQueueStatus(&result)
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(result)
	case "events":
		fromSequence := 1
		explicitSequence := false
		if value := request.URL.Query().Get("fromSequence"); value != "" {
			explicitSequence = true
			fromSequence, err = strconv.Atoi(value)
			if err != nil || fromSequence < 1 {
				writeControlPlaneError(response, http.StatusBadRequest, "invalid event sequence")
				return
			}
		}
		if request.URL.Query().Get("follow") == "true" {
			handler.serveControlPlaneEventStream(response, request, repoDir, record, fromSequence, shouldRefreshControlPlaneEventStream(assignmentBacked, record))
			return
		}
		var result controlPlaneEventsResponse
		if explicitSequence {
			result, err = readControlPlaneEvents(repoDir, record, fromSequence)
		} else {
			result, err = readControlPlaneEvents(repoDir, record)
		}
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read run events")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(result)
	case "logs":
		result, err := readControlPlaneLogs(repoDir, record)
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read run logs")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(result)
	case "result":
		result, err := readControlPlaneResult(repoDir, record)
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read run result")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(result)
	case "evidence":
		result, err := readControlPlaneEvidence(repoDir, record)
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "read run evidence")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(result)
	default:
		http.NotFound(response, request)
	}
}

func (handler *controlPlaneHandler) readControlPlaneRunRecord(repoDir string, runID string) (runLedgerRecord, bool, bool, error) {
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	assignmentActive := assignment != nil && assignment.record.State != "quarantined"
	if assignment != nil && assignment.record.State == "quarantined" && assignment.record.TerminalLedger != nil {
		record := *assignment.record.TerminalLedger
		handler.mu.Unlock()
		return record, false, true, nil
	}
	if assignment != nil && assignment.record.State == "finishing" && assignment.record.AssignedLedger != nil {
		record := *assignment.record.AssignedLedger
		record.State = "finishing"
		record.Status = ""
		record.CompletedAt = ""
		handler.mu.Unlock()
		return record, true, true, nil
	}
	if assignmentActive && assignment.record.AssignedLedger != nil {
		record := *assignment.record.AssignedLedger
		handler.mu.Unlock()
		return record, true, true, nil
	}
	handler.mu.Unlock()
	record, err := newRunLedgerStore(repoDir).Read(runID)
	return record, assignmentActive, assignment != nil, err
}

func (handler *controlPlaneHandler) serveControlPlaneEventStream(response http.ResponseWriter, request *http.Request, repoDir string, record runLedgerRecord, fromSequence int, refreshTransient bool) {
	response.Header().Set("Content-Type", "application/x-ndjson")
	response.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(response)
	lastSequence := fromSequence - 1
	written := 0
	var sourceStamp controlPlaneEventSourceStamp
	var err error
	if refreshTransient {
		sourceStamp, err = refreshControlPlaneEventSource(repoDir, record.RunID, handler.runtime.Now())
		if err != nil {
			sourceStamp, _ = controlPlaneEventSourceStampForRun(repoDir, record.RunID)
		}
	}
	record, _, _, err = handler.readControlPlaneRunRecord(repoDir, record.RunID)
	if err != nil {
		return
	}
	lastSnapshot := controlPlaneEventSnapshot(record)
	firstSnapshot := true
	writeRecord := func(streamRecord controlPlaneEventStreamRecord, reserve int) bool {
		content, err := json.Marshal(streamRecord)
		if err != nil || written+len(content)+1 > maximumRunLedgerMaxEventBytes-reserve {
			return false
		}
		content = append(content, '\n')
		if _, err := response.Write(content); err != nil {
			return false
		}
		written += len(content)
		return controller.Flush() == nil
	}
	for {
		if !firstSnapshot {
			if refreshTransient {
				currentSource, err := controlPlaneEventSourceStampForRun(repoDir, record.RunID)
				if err != nil {
					return
				}
				if currentSource != sourceStamp {
					refreshedSource, refreshErr := refreshControlPlaneEventSource(repoDir, record.RunID, handler.runtime.Now())
					if refreshErr == nil {
						sourceStamp = refreshedSource
					}
				}
			}
			refreshed, _, _, err := handler.readControlPlaneRunRecord(repoDir, record.RunID)
			if err != nil {
				return
			}
			currentSnapshot := controlPlaneEventSnapshot(refreshed)
			if currentSnapshot == lastSnapshot {
				if !waitForControlPlaneEventPoll(request) {
					return
				}
				continue
			}
			record = refreshed
			lastSnapshot = currentSnapshot
		}
		firstSnapshot = false
		events, err := readControlPlaneEvents(repoDir, record, lastSequence+1)
		if err != nil {
			return
		}
		markerPending := events.EventsTruncated && events.EventsOmitted > 0 && events.FirstAvailableSequence > 0
		writeGapMarker := func() bool {
			if writeRecord(controlPlaneEventStreamRecord{
				Version: controlPlaneAPIVersion, Kind: "events-truncated", RunID: record.RunID,
				EventsOmitted: events.EventsOmitted, EventsTruncated: true, NextSequence: events.FirstAvailableSequence,
			}, 512) {
				if gapLastSequence := events.FirstAvailableSequence - 1; gapLastSequence > lastSequence {
					lastSequence = gapLastSequence
				}
				return true
			}
			_ = writeRecord(controlPlaneEventStreamRecord{
				Version: controlPlaneAPIVersion, Kind: "stream-truncated", RunID: record.RunID,
				EventsTruncated: true, NextSequence: events.FirstAvailableSequence, State: record.State,
			}, 0)
			return false
		}
		for _, event := range events.Events {
			if event["type"] == "run-ledger.events.truncated" {
				continue
			}
			sequence := ledgerEventSequence(event)
			if markerPending && sequence >= events.FirstAvailableSequence {
				if !writeGapMarker() {
					return
				}
				markerPending = false
			}
			// Sequence is the identity and cursor: replayed snapshots never re-emit acknowledged events.
			if sequence <= lastSequence {
				continue
			}
			if !writeRecord(controlPlaneEventStreamRecord{
				Version: controlPlaneAPIVersion, Kind: "event", RunID: record.RunID,
				Sequence: sequence, Event: event, NextSequence: sequence + 1,
			}, 512) {
				_ = writeRecord(controlPlaneEventStreamRecord{
					Version: controlPlaneAPIVersion, Kind: "stream-truncated", RunID: record.RunID,
					EventsTruncated: true, NextSequence: lastSequence + 1, State: record.State,
				}, 0)
				return
			}
			lastSequence = sequence
		}
		if markerPending {
			if !writeGapMarker() {
				return
			}
		}
		if terminalRunLedgerRecord(record) {
			_ = writeRecord(controlPlaneEventStreamRecord{
				Version: controlPlaneAPIVersion, Kind: "stream-end", RunID: record.RunID,
				NextSequence: lastSequence + 1, State: record.State,
			}, 0)
			return
		}
		if !waitForControlPlaneEventPoll(request) {
			return
		}
	}
}

type controlPlaneEventSourceStamp struct {
	Exists   bool
	Size     int64
	Modified int64
}

type controlPlaneEventSnapshotIdentity struct {
	State           string
	UpdatedAt       string
	EventCount      int
	EventBytes      int
	EventsOmitted   int
	EventsTruncated bool
}

func controlPlaneEventSourceStampForRun(repoDir string, runID string) (controlPlaneEventSourceStamp, error) {
	path := filepath.Join(repoDir, ".maya-stall", "state", "runs", runID, "events.jsonl")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return controlPlaneEventSourceStamp{}, nil
	}
	if err != nil {
		return controlPlaneEventSourceStamp{}, err
	}
	return controlPlaneEventSourceStamp{Exists: true, Size: info.Size(), Modified: info.ModTime().UnixNano()}, nil
}

func refreshControlPlaneEventSource(repoDir string, runID string, now time.Time) (controlPlaneEventSourceStamp, error) {
	before, err := controlPlaneEventSourceStampForRun(repoDir, runID)
	if err != nil || !before.Exists {
		return before, err
	}
	if err := newRunLedgerStore(repoDir).Refresh(runID, now); err != nil {
		return controlPlaneEventSourceStamp{}, err
	}
	after, err := controlPlaneEventSourceStampForRun(repoDir, runID)
	if err != nil {
		return controlPlaneEventSourceStamp{}, err
	}
	if after != before {
		// Preserve the pre-refresh stamp so a concurrent append forces another bounded refresh.
		return before, nil
	}
	return after, nil
}

func controlPlaneEventSnapshot(record runLedgerRecord) controlPlaneEventSnapshotIdentity {
	return controlPlaneEventSnapshotIdentity{
		State: record.State, UpdatedAt: record.UpdatedAt, EventCount: record.EventCount,
		EventBytes: record.EventBytes, EventsOmitted: record.EventsOmitted, EventsTruncated: record.EventsTruncated,
	}
}

func waitForControlPlaneEventPoll(request *http.Request) bool {
	timer := time.NewTimer(controlPlaneEventPollInterval)
	defer timer.Stop()
	select {
	case <-request.Context().Done():
		return false
	case <-timer.C:
		return true
	}
}

func terminalRunLedgerRecord(record runLedgerRecord) bool {
	switch record.State {
	case "completed", "failed", "canceled", "kept", "cleanup-failed":
		return true
	default:
		return false
	}
}

func shouldRefreshControlPlaneEventStream(assignmentBacked bool, record runLedgerRecord) bool {
	return !assignmentBacked && !terminalRunLedgerRecord(record)
}

func readControlPlaneEvidence(repoDir string, record runLedgerRecord) (controlPlaneEvidenceResponse, error) {
	evidenceDir, err := safeControlPlaneEvidenceDir(repoDir, record)
	if err != nil {
		return controlPlaneEvidenceResponse{}, err
	}
	content, err := os.ReadFile(filepath.Join(evidenceDir, evidenceBundleFileName))
	if err != nil {
		return controlPlaneEvidenceResponse{}, err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return controlPlaneEvidenceResponse{}, err
	}
	if bundle.RunID != record.RunID {
		return controlPlaneEvidenceResponse{}, fmt.Errorf("evidence bundle identifies another run")
	}
	if bundle.Failure != nil {
		bundle.Failure.Diagnostic = sanitizeControlPlaneText(bundle.Failure.Diagnostic, repoDir)
	}
	sanitizeControlPlaneValidators(bundle.Validators, repoDir)
	return controlPlaneEvidenceResponse{Version: controlPlaneAPIVersion, Kind: "evidence", RunID: record.RunID, Bundle: bundle}, nil
}

func controlPlaneStatusFromRecord(repoDir string, record runLedgerRecord, evidence string) controlPlaneStatusResponse {
	return controlPlaneStatusResponse{
		Version:       controlPlaneAPIVersion,
		Kind:          "status",
		RunID:         record.RunID,
		Scenario:      record.Scenario,
		TargetProfile: record.TargetProfile,
		Host:          record.Host,
		State:         record.State,
		Status:        record.Status,
		CleanupState:  controlPlaneCleanupState(repoDir, record),
		AcceptedAt:    record.AcceptedAt,
		Evidence:      evidence,
	}
}

func readControlPlaneResult(repoDir string, record runLedgerRecord) (controlPlaneResultResponse, error) {
	switch record.State {
	case "completed", "failed", "canceled", "cleanup-failed", "kept":
	default:
		return controlPlaneResultResponse{
			Version: controlPlaneAPIVersion, Kind: "result", RunID: record.RunID,
			State: record.State, Status: record.Status, CleanupState: "pending",
			Final: false, Success: false, Result: ScenarioResult{Status: record.Status},
			Evidence: "/v1/runs/" + record.RunID + "/evidence",
		}, nil
	}
	evidenceDir, err := safeControlPlaneEvidenceDir(repoDir, record)
	if err != nil {
		return controlPlaneResultResponse{}, err
	}
	content, err := os.ReadFile(filepath.Join(evidenceDir, evidenceBundleFileName))
	if err != nil {
		return controlPlaneResultResponse{}, err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return controlPlaneResultResponse{}, err
	}
	if bundle.RunID != record.RunID {
		return controlPlaneResultResponse{}, fmt.Errorf("evidence bundle identifies another run")
	}
	result := ScenarioResult{Status: bundle.Status}
	if bundle.ScenarioResult != "" {
		clean := cleanEvidenceArtifactPath(bundle.ScenarioResult)
		if clean == "" {
			return controlPlaneResultResponse{}, fmt.Errorf("invalid Scenario Result path")
		}
		if err := ensureWorkspacePathHasNoSymlinkAncestor(evidenceDir, clean); err != nil {
			return controlPlaneResultResponse{}, err
		}
		resultContent, err := os.ReadFile(filepath.Join(evidenceDir, filepath.FromSlash(clean)))
		if err != nil {
			return controlPlaneResultResponse{}, err
		}
		if err := json.Unmarshal(resultContent, &result); err != nil {
			return controlPlaneResultResponse{}, err
		}
	} else if bundle.Failure != nil {
		result.Summary = bundle.Failure.Diagnostic
	}
	result.Summary = sanitizeControlPlaneText(result.Summary, repoDir)
	sanitizeControlPlaneValidators(bundle.Validators, repoDir)
	cleanupState := controlPlaneCleanupState(repoDir, record)
	final := record.State == "completed" || record.State == "failed" || record.State == "canceled" || record.State == "cleanup-failed"
	success := record.State == "completed" && record.Status == resultStatusPassed && result.Status == resultStatusPassed && cleanupState == "completed"
	return controlPlaneResultResponse{
		Version: controlPlaneAPIVersion, Kind: "result", RunID: record.RunID,
		State: record.State, Status: record.Status, CleanupState: cleanupState,
		Final: final, Success: success, Result: result, Validators: bundle.Validators,
		Evidence: "/v1/runs/" + record.RunID + "/evidence",
	}, nil
}

func readControlPlaneLogs(repoDir string, record runLedgerRecord) (controlPlaneLogsResponse, error) {
	current, content, err := newRunLedgerStore(repoDir).ReadLog(record.RunID)
	if err != nil {
		return controlPlaneLogsResponse{}, err
	}
	record.LogTruncated = current.LogTruncated
	sanitized := sanitizeControlPlaneText(string(content), repoDir)
	return controlPlaneLogsResponse{
		Version: controlPlaneAPIVersion, Kind: "logs", RunID: record.RunID,
		Content: sanitized, Bytes: len(sanitized), Truncated: record.LogTruncated,
	}, nil
}

func readControlPlaneEvents(repoDir string, record runLedgerRecord, requestedSequence ...int) (controlPlaneEventsResponse, error) {
	current, content, err := newRunLedgerStore(repoDir).ReadEvents(record.RunID)
	if err != nil {
		return controlPlaneEventsResponse{}, err
	}
	record.EventCount = current.EventCount
	record.EventsOmitted = current.EventsOmitted
	record.EventsTruncated = current.EventsTruncated
	fromSequence := 1
	includeCursors := len(requestedSequence) > 0
	if includeCursors {
		fromSequence = requestedSequence[0]
	}
	events := make([]map[string]any, 0, record.EventCount)
	gapFirstSequence := 0
	gapLastSequence := 0
	contentTruncated := false
	for index, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return controlPlaneEventsResponse{}, fmt.Errorf("parse event %d: %w", index+1, err)
		}
		event = sanitizeControlPlaneValue(event, repoDir).(map[string]any)
		if event["type"] == "run-ledger.events.truncated" {
			if omitted, ok := runLedgerOmittedEventCount(line); ok && omitted > 0 {
				gapFirstSequence = ledgerEventSequence(event)
				gapLastSequence = gapFirstSequence + omitted - 1
			}
			continue
		}
		if event["type"] == "run-ledger.event.truncated" && ledgerEventSequence(event) >= fromSequence {
			contentTruncated = true
		}
		if ledgerEventSequence(event) >= fromSequence {
			events = append(events, event)
		}
	}
	firstAvailable := 0
	nextSequence := fromSequence
	eventsOmitted := 0
	eventsTruncated := gapLastSequence > 0 && fromSequence <= gapLastSequence
	if eventsTruncated {
		firstMissing := fromSequence
		if firstMissing < gapFirstSequence {
			firstMissing = gapFirstSequence
		}
		eventsOmitted = gapLastSequence - firstMissing + 1
		firstAvailable = gapLastSequence + 1
	} else if len(events) > 0 {
		firstAvailable = ledgerEventSequence(events[0])
	}
	if len(events) > 0 {
		nextSequence = ledgerEventSequence(events[len(events)-1]) + 1
	}
	response := controlPlaneEventsResponse{
		Version: controlPlaneAPIVersion, Kind: "events", RunID: record.RunID, Events: events,
		EventsOmitted: eventsOmitted, EventsTruncated: eventsTruncated || contentTruncated,
	}
	if includeCursors {
		response.RequestedSequence = fromSequence
		response.FirstAvailableSequence = firstAvailable
		response.NextSequence = nextSequence
	}
	return response, nil
}

func controlPlaneCleanupState(repoDir string, record runLedgerRecord) string {
	switch record.State {
	case "completed", "failed", "canceled":
		evidenceDir, err := safeControlPlaneEvidenceDir(repoDir, record)
		if err != nil {
			return "unresolved"
		}
		content, err := os.ReadFile(filepath.Join(evidenceDir, evidenceBundleFileName))
		if err != nil {
			return "unresolved"
		}
		var bundle evidenceBundle
		if json.Unmarshal(content, &bundle) != nil || bundle.RunID != record.RunID {
			return "unresolved"
		}
		if bundle.Failure != nil && bundle.Failure.CleanupState != "" {
			return bundle.Failure.CleanupState
		}
		return "completed"
	case "kept":
		return "retained"
	case "cleanup-failed":
		return "failed"
	default:
		return "pending"
	}
}

func safeControlPlaneEvidenceDir(repoDir string, record runLedgerRecord) (string, error) {
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, record.EvidenceDir); err != nil {
		return "", err
	}
	evidenceDir := filepath.Join(repoDir, filepath.FromSlash(record.EvidenceDir))
	if err := ensureWorkspacePathHasNoSymlinkAncestor(evidenceDir, evidenceBundleFileName); err != nil {
		return "", err
	}
	return evidenceDir, nil
}

func (handler *controlPlaneHandler) reserveRun(scenario string) (string, string, error) {
	base := handler.runtime.Now().UTC().Format("20060102T150405.000000000Z")
	for attempt := 0; ; attempt++ {
		runID := base
		if attempt > 0 {
			runID = fmt.Sprintf("%s-%d", base, attempt)
		}
		runRoot := filepath.Join(handler.dataDir, "runs", runID)
		if err := os.Mkdir(runRoot, 0o700); errors.Is(err, os.ErrExist) {
			continue
		} else if err != nil {
			return "", "", err
		}
		repoDir := filepath.Join(runRoot, "repo")
		if err := os.Mkdir(repoDir, 0o700); err != nil {
			return "", "", err
		}
		// The submission path holds handler.mu across reservation and indexing.
		handler.runIDs = append(handler.runIDs, runID)
		sortControlPlaneRunIDsNewest(handler.runIDs)
		return runID, repoDir, nil
	}
}

func writeControlPlaneError(response http.ResponseWriter, status int, message string) {
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(status)
	_ = json.NewEncoder(response).Encode(map[string]any{"version": controlPlaneAPIVersion, "error": message})
}

func runScenarioThroughMode(repoDir string, options runOptions, runtime runRuntime) (runOutcome, error) {
	if options.ControlPlane == "" {
		if options.ControlPlaneSet {
			return runOutcome{}, newUsageError("--control-plane needs an HTTPS URL")
		}
		if options.ControlPlaneTokenEnv != "" {
			return runOutcome{}, newUsageError("--control-plane-token-env requires --control-plane")
		}
		if options.StopAfter != stopAfterAlways && options.KeepTTL > defaultHostLockHardLifetime {
			return runOutcome{}, newUsageError("--keep-ttl exceeds embedded Kept Session retention limit %s", defaultHostLockHardLifetime)
		}
		return runScenario(repoDir, options, runtime)
	}
	return submitControlPlaneScenario(repoDir, options, runtime)
}

func printRunHistoryThroughMode(repoDir string, options historyOptions, now time.Time, stdout io.Writer, runtime runRuntime) error {
	if options.ControlPlane == "" {
		if options.ControlPlaneTokenEnv != "" {
			return newUsageError("--control-plane-token-env requires --control-plane")
		}
		if options.BeforeRunID != "" {
			return newUsageError("--before-run requires --control-plane")
		}
		return printRunHistory(repoDir, options, now, stdout)
	}
	var history runHistoryResponse
	query := url.Values{}
	query.Set("scenario", options.Scenario)
	query.Set("host", options.Host)
	query.Set("state", options.State)
	query.Set("since", options.Since)
	query.Set("beforeRunId", options.BeforeRunID)
	path := "/v1/runs"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	if err := getControlPlaneJSON(options.ControlPlane, options.ControlPlaneTokenEnv, path, runtime, &history); err != nil {
		return err
	}
	if history.Version != controlPlaneAPIVersion || history.Kind != "history" {
		return fmt.Errorf("control plane returned an unsupported history response")
	}
	if options.JSON {
		return json.NewEncoder(stdout).Encode(history)
	}
	if len(history.Runs) == 0 {
		if _, err := fmt.Fprintln(stdout, "state: no runs"); err != nil {
			return err
		}
	} else {
		for _, record := range history.Runs {
			if _, err := fmt.Fprintf(stdout, "run: %s\nscenario: %s\nhost: %s\nstate: %s\nstatus: %s\nacceptedAt: %s\n", record.RunID, record.Scenario, record.Host, record.State, record.Status, record.AcceptedAt); err != nil {
				return err
			}
		}
	}
	if history.RunsTruncated {
		_, err := fmt.Fprintf(stdout, "historyTruncated: true\nrunsOmittedAtLeast: %d\nnextBeforeRunId: %s\n", history.RunsOmittedAtLeast, history.NextBeforeRunID)
		return err
	}
	return nil
}

func failAcceptedSubmissionThroughMode(repoDir string, options runOptions, runtime runRuntime, submissionErr error) (runOutcome, error) {
	if options.ControlPlane == "" {
		if options.ControlPlaneSet {
			return runOutcome{}, newUsageError("--control-plane needs an HTTPS URL")
		}
		if options.ControlPlaneTokenEnv != "" {
			return runOutcome{}, newUsageError("--control-plane-token-env requires --control-plane")
		}
		return failAcceptedSubmission(repoDir, options, runtime, submissionErr)
	}
	return submitControlPlaneScenarioWithFailure(repoDir, options, runtime, submissionErr)
}

func printStatusThroughMode(repoDir string, options statusOptions, stdout io.Writer, runtime runRuntime) error {
	if options.ControlPlane == "" {
		if options.ControlPlaneTokenEnv != "" {
			return newUsageError("--control-plane-token-env requires --control-plane")
		}
		if options.JSON {
			if options.RunID == "" {
				return newUsageError("embedded JSON status needs --run")
			}
			result, err := readEmbeddedStatusResponse(repoDir, options.RunID)
			if err != nil {
				return err
			}
			return json.NewEncoder(stdout).Encode(result)
		}
		return printStatus(repoDir, options, stdout)
	}
	if options.RunID == "" {
		return newUsageError("configured Control Plane status needs --run")
	}
	var result controlPlaneStatusResponse
	if err := getControlPlaneJSON(options.ControlPlane, options.ControlPlaneTokenEnv, "/v1/runs/"+options.RunID+"/status", runtime, &result); err != nil {
		return err
	}
	if result.Version != controlPlaneAPIVersion || result.Kind != "status" || result.RunID != options.RunID {
		return fmt.Errorf("control plane returned an unsupported status response")
	}
	if options.JSON {
		return json.NewEncoder(stdout).Encode(result)
	}
	_, err := fmt.Fprintf(stdout, "run: %s\nstate: %s\nscenario: %s\ntargetProfile: %s\nhost: %s\nstatus: %s\ncleanupState: %s\nacceptedAt: %s\nevidence: %s\n",
		result.RunID, result.State, result.Scenario, result.TargetProfile, result.Host, result.Status, result.CleanupState, result.AcceptedAt, result.Evidence)
	if err == nil && result.State == "queued" {
		_, err = fmt.Fprintf(stdout, "queuePosition: %d\nwaitReason: %s\nhostPool: %s\n", result.QueuePosition, result.WaitReason, result.HostPool)
	}
	if err == nil && result.KeepDeadline != "" {
		_, err = fmt.Fprintf(stdout, "keepDeadline: %s\nkeepRemaining: %s\n", result.KeepDeadline, result.KeepRemaining)
	}
	if err == nil && result.IdleDeadline != "" {
		_, err = fmt.Fprintf(stdout, "idleDeadline: %s\nhardDeadline: %s\n", result.IdleDeadline, result.HardDeadline)
	}
	if err == nil && result.ExpiryReason != "" {
		_, err = fmt.Fprintf(stdout, "expiryReason: %s\n", result.ExpiryReason)
	}
	return err
}

func addKeptSessionTTLToStatus(repoDir string, runID string, now time.Time, result *controlPlaneStatusResponse) {
	if result.State != "kept" && result.State != "cleanup-failed" {
		return
	}
	run, err := readKeptRunState(repoDir, runID)
	if err != nil {
		return
	}
	result.KeepDeadline, result.KeepRemaining = keptSessionTTLStatus(run.Record, now)
}

func readEmbeddedStatusResponse(repoDir string, runID string) (controlPlaneStatusResponse, error) {
	record, err := newRunLedgerStore(repoDir).Read(runID)
	if err != nil {
		return controlPlaneStatusResponse{}, err
	}
	retained, err := runLedgerUsesRetainedState(repoDir, record)
	if err != nil {
		return controlPlaneStatusResponse{}, err
	}
	if !retained {
		return controlPlaneStatusFromRecord(repoDir, record, record.EvidenceDir), nil
	}
	run, err := readKeptRunState(repoDir, runID)
	if err != nil {
		return controlPlaneStatusResponse{}, err
	}
	if run.Record.Status == "running" && run.Record.StopPhase == "" {
		evidencePath := filepath.Join(repoDir, "artifacts", "maya-stall", runID, evidenceBundleFileName)
		if evidenceBytes, evidenceErr := os.ReadFile(evidencePath); evidenceErr == nil {
			if err := json.Unmarshal(evidenceBytes, &run.Bundle); err != nil {
				return controlPlaneStatusResponse{}, fmt.Errorf("parse run evidence: %w", err)
			}
		} else if !errors.Is(evidenceErr, os.ErrNotExist) {
			return controlPlaneStatusResponse{}, evidenceErr
		}
		run.Bundle.Status = run.Record.Status
	} else {
		run, err = readKeptRun(repoDir, runID)
		if err != nil {
			return controlPlaneStatusResponse{}, err
		}
	}
	if err := refreshKeptRunStatus(&run); err != nil {
		return controlPlaneStatusResponse{}, err
	}
	result := controlPlaneStatusFromRecord(repoDir, record, record.EvidenceDir)
	if run.RemoteStatus.State != "" {
		result.State = run.RemoteStatus.State
	}
	result.Status = run.Bundle.Status
	if result.State == "kept" || result.State == "cleanup-failed" {
		result.KeepDeadline, result.KeepRemaining = keptSessionTTLStatus(run.Record, time.Now())
	}
	return result, nil
}

func (handler *controlPlaneHandler) addControlPlaneHostLockDeadlines(runID string, result *controlPlaneStatusResponse) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	assignment := handler.assignments[runID]
	if assignment == nil {
		return
	}
	result.IdleDeadline = assignment.record.IdleDeadline
	result.HardDeadline = assignment.record.HardDeadline
	if assignment.record.KeepDeadline != "" {
		result.KeepDeadline = assignment.record.KeepDeadline
		result.KeepRemaining = hostLockDeadlineRemaining(assignment.record.KeepDeadline, handler.runtime.Now())
	}
	result.ExpiryReason = assignment.record.ExpiryReason
	if assignment.record.State == "expiring" || assignment.record.State == "finishing" {
		result.State = assignment.record.State
		result.CleanupState = "pending"
	}
}

func parseRunReadArgs(command string, args []string) (runReadOptions, error) {
	options := runReadOptions{FromSequence: 1}
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--json":
			options.JSON = true
		case "--control-plane", "--control-plane-token-env", "--from-sequence":
			flag := args[index]
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return runReadOptions{}, newUsageError("%s needs a value", flag)
			}
			switch flag {
			case "--from-sequence":
				if command != "events" {
					return runReadOptions{}, newUsageError("%s does not accept --from-sequence", command)
				}
				sequence, err := strconv.Atoi(args[index])
				if err != nil || sequence < 1 {
					return runReadOptions{}, newUsageError("--from-sequence needs a positive integer")
				}
				options.FromSequence = sequence
				options.FromSequenceSet = true
			case "--control-plane":
				options.ControlPlane = args[index]
			case "--control-plane-token-env":
				options.ControlPlaneTokenEnv = args[index]
			}
		default:
			if strings.HasPrefix(args[index], "-") {
				return runReadOptions{}, newUsageError("unknown %s option %q", command, args[index])
			}
			if options.RunID != "" {
				return runReadOptions{}, newUsageError("%s needs one run id", command)
			}
			options.RunID = args[index]
		}
	}
	if err := validateRunID(options.RunID); err != nil {
		return runReadOptions{}, err
	}
	return options, nil
}

func printRunReadThroughMode(repoDir string, resource string, options runReadOptions, stdout io.Writer, runtime runRuntime) error {
	if options.ControlPlane == "" && options.ControlPlaneTokenEnv != "" {
		return newUsageError("--control-plane-token-env requires --control-plane")
	}
	switch resource {
	case "events":
		var result controlPlaneEventsResponse
		if options.ControlPlane != "" {
			path := "/v1/runs/" + options.RunID + "/events"
			if options.FromSequenceSet {
				path += "?fromSequence=" + strconv.Itoa(options.FromSequence)
			}
			if err := getControlPlaneJSON(options.ControlPlane, options.ControlPlaneTokenEnv, path, runtime, &result); err != nil {
				return err
			}
		} else {
			record, err := newRunLedgerStore(repoDir).Read(options.RunID)
			if err != nil {
				return err
			}
			if options.FromSequenceSet {
				result, err = readControlPlaneEvents(repoDir, record, options.FromSequence)
			} else {
				result, err = readControlPlaneEvents(repoDir, record)
			}
			if err != nil {
				return err
			}
		}
		if result.Version != controlPlaneAPIVersion || result.Kind != "events" || result.RunID != options.RunID {
			return fmt.Errorf("run event source returned an unsupported response")
		}
		if options.JSON {
			return json.NewEncoder(stdout).Encode(result)
		}
		if _, err := fmt.Fprintf(stdout, "run: %s\nevents:\n", result.RunID); err != nil {
			return err
		}
		for _, event := range result.Events {
			content, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintln(stdout, string(content)); err != nil {
				return err
			}
		}
		if result.EventsTruncated {
			_, err := fmt.Fprintf(stdout, "eventsTruncated: true\neventsOmitted: %d\n", result.EventsOmitted)
			return err
		}
		return nil
	case "logs":
		var result controlPlaneLogsResponse
		if options.ControlPlane != "" {
			if err := getControlPlaneJSON(options.ControlPlane, options.ControlPlaneTokenEnv, "/v1/runs/"+options.RunID+"/logs", runtime, &result); err != nil {
				return err
			}
		} else {
			record, err := newRunLedgerStore(repoDir).Read(options.RunID)
			if err != nil {
				return err
			}
			result, err = readControlPlaneLogs(repoDir, record)
			if err != nil {
				return err
			}
		}
		if result.Version != controlPlaneAPIVersion || result.Kind != "logs" || result.RunID != options.RunID {
			return fmt.Errorf("run log source returned an unsupported response")
		}
		if options.JSON {
			return json.NewEncoder(stdout).Encode(result)
		}
		_, err := fmt.Fprintf(stdout, "run: %s\nlogs:\n%s", result.RunID, result.Content)
		if err == nil && result.Truncated {
			_, err = fmt.Fprintln(stdout, "logTruncated: true")
		}
		return err
	case "result":
		var result controlPlaneResultResponse
		if options.ControlPlane != "" {
			if err := getControlPlaneJSON(options.ControlPlane, options.ControlPlaneTokenEnv, "/v1/runs/"+options.RunID+"/result", runtime, &result); err != nil {
				return err
			}
		} else {
			record, err := newRunLedgerStore(repoDir).Read(options.RunID)
			if err != nil {
				return err
			}
			result, err = readControlPlaneResult(repoDir, record)
			if err != nil {
				return err
			}
		}
		if result.Version != controlPlaneAPIVersion || result.Kind != "result" || result.RunID != options.RunID {
			return fmt.Errorf("run result source returned an unsupported response")
		}
		if options.JSON {
			return json.NewEncoder(stdout).Encode(result)
		}
		_, err := fmt.Fprintf(stdout, "run: %s\nstate: %s\nstatus: %s\ncleanupState: %s\nfinal: %t\nsuccess: %t\nsummary: %s\nevidence: %s\n",
			result.RunID, result.State, result.Status, result.CleanupState, result.Final, result.Success, result.Result.Summary, result.Evidence)
		return err
	default:
		return fmt.Errorf("%s reads are not implemented", resource)
	}
}

func getControlPlaneJSON(rawURL string, tokenEnv string, path string, runtime runRuntime, target any) error {
	endpoint, err := parseControlPlaneURL(rawURL)
	if err != nil {
		return err
	}
	if tokenEnv == "" {
		tokenEnv = defaultControlPlaneTokenEnv
	}
	token, ok := os.LookupEnv(tokenEnv)
	if !ok || token == "" {
		return fmt.Errorf("control plane token environment variable %s is not set", tokenEnv)
	}
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(endpoint.String(), "/")+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	client := controlPlaneHTTPClient(runtime.ControlPlaneHTTPClient)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("read Control Plane: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("control plane read failed with HTTP %d", response.StatusCode)
	}
	limit := int64(maximumControlPlaneDefaultResponseBytes)
	resourcePath := path
	if query := strings.IndexByte(resourcePath, '?'); query >= 0 {
		resourcePath = resourcePath[:query]
	}
	if strings.HasSuffix(resourcePath, "/events") || strings.HasSuffix(resourcePath, "/logs") {
		// JSON can expand each retained byte to a six-byte Unicode escape.
		limit = int64(maximumControlPlaneLedgerResponseBytes)
	}
	return decodeBoundedControlPlaneJSON(response.Body, limit, target)
}

func parseControlPlaneURL(rawURL string) (*url.URL, error) {
	endpoint, err := url.Parse(rawURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" && endpoint.Path != "/" {
		return nil, newUsageError("--control-plane needs an origin-only HTTPS URL without credentials, path, query, or fragment")
	}
	return endpoint, nil
}

func controlPlaneHTTPClient(base *http.Client) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &client
}

func decodeBoundedControlPlaneJSON(reader io.Reader, limit int64, target any) error {
	content, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if int64(len(content)) > limit {
		return fmt.Errorf("control plane response exceeds %d bytes", limit)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("control plane response contains multiple JSON values")
		}
		return err
	}
	return nil
}

func validateControlPlaneSubmissionFiles(submission controlPlaneSubmission) error {
	if len(submission.SubmissionError) > 4096 {
		return fmt.Errorf("submission error exceeds size limit")
	}
	if submission.ConfigName == "" {
		if len(submission.Config) != 0 {
			return fmt.Errorf("repo run config content needs a configName")
		}
	} else if submission.ConfigName != defaultConfigName && submission.ConfigName != "maya-stall.yaml" {
		return fmt.Errorf("unsupported Repo Run Config name")
	}
	if len(submission.Files) > 10_000 {
		return fmt.Errorf("too many submission files")
	}
	kinds := make(map[string]string, len(submission.Files))
	for _, file := range submission.Files {
		clean, err := cleanRepoRelativePath(file.Path)
		if err != nil {
			return err
		}
		clean = filepath.ToSlash(clean)
		if clean != file.Path || clean == submission.ConfigName || kinds[clean] != "" {
			return fmt.Errorf("duplicate or non-canonical submission path")
		}
		if file.Kind != "file" && file.Kind != "directory" {
			return fmt.Errorf("unsupported submission file kind")
		}
		if file.Kind == "directory" && len(file.Content) != 0 {
			return fmt.Errorf("submission directory must not contain bytes")
		}
		kinds[clean] = file.Kind
	}
	for path := range kinds {
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(path))); parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			if kinds[parent] == "file" {
				return fmt.Errorf("submission file cannot contain child paths")
			}
		}
	}
	return nil
}

func sanitizeControlPlaneText(value string, repoDir string) string {
	if value == "" {
		return ""
	}
	privatePaths := []struct {
		path        string
		replacement string
	}{
		{path: repoDir, replacement: "<run-repository>"},
	}
	runRoot := filepath.Dir(repoDir)
	runsDir := filepath.Dir(runRoot)
	if filepath.Base(repoDir) == "repo" && filepath.Base(runsDir) == "runs" {
		privatePaths = append(privatePaths, struct {
			path        string
			replacement string
		}{path: filepath.Dir(runsDir), replacement: "<control-plane-data>"})
	}
	for _, private := range privatePaths {
		for _, privatePath := range []string{private.path, filepath.ToSlash(private.path)} {
			value = strings.ReplaceAll(value, privatePath, private.replacement)
		}
	}
	return value
}

func sanitizeControlPlaneValidators(validators []validatorResult, repoDir string) {
	for index := range validators {
		validators[index].Message = sanitizeControlPlaneText(validators[index].Message, repoDir)
	}
}

func sanitizeControlPlaneValue(value any, repoDir string) any {
	switch typed := value.(type) {
	case string:
		return sanitizeControlPlaneText(typed, repoDir)
	case []any:
		for index := range typed {
			typed[index] = sanitizeControlPlaneValue(typed[index], repoDir)
		}
		return typed
	case map[string]any:
		for key := range typed {
			typed[key] = sanitizeControlPlaneValue(typed[key], repoDir)
		}
		return typed
	default:
		return value
	}
}

func submitControlPlaneScenario(repoDir string, options runOptions, runtime runRuntime) (runOutcome, error) {
	return submitControlPlaneScenarioWithFailure(repoDir, options, runtime, nil)
}

func submitControlPlaneScenarioWithFailure(repoDir string, options runOptions, runtime runRuntime, submissionErr error) (runOutcome, error) {
	if options.HostOptionsSet {
		return runOutcome{}, newUsageError("configured Control Plane mode owns Maya Host selection and does not accept client host configuration")
	}
	endpoint, err := parseControlPlaneURL(options.ControlPlane)
	if err != nil {
		return runOutcome{}, err
	}
	submission, err := buildControlPlaneSubmission(repoDir, options)
	if err != nil {
		return runOutcome{}, err
	}
	if submissionErr != nil {
		submission.SubmissionError = submissionErr.Error()
	}
	content, err := json.Marshal(submission)
	if err != nil {
		return runOutcome{}, err
	}
	if len(content) > maximumControlPlaneSubmissionBytes {
		return runOutcome{}, fmt.Errorf("control plane submission exceeds %d bytes", maximumControlPlaneSubmissionBytes)
	}
	tokenEnv := options.ControlPlaneTokenEnv
	if tokenEnv == "" {
		tokenEnv = defaultControlPlaneTokenEnv
	}
	token, ok := os.LookupEnv(tokenEnv)
	if !ok || token == "" {
		return runOutcome{}, fmt.Errorf("control plane token environment variable %s is not set", tokenEnv)
	}
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(endpoint.String(), "/")+"/v1/runs", bytes.NewReader(content))
	if err != nil {
		return runOutcome{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	client := controlPlaneHTTPClient(runtime.ControlPlaneHTTPClient)
	response, err := client.Do(request)
	if err != nil {
		return runOutcome{}, fmt.Errorf("submit Scenario to Control Plane: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		return runOutcome{}, fmt.Errorf("control plane submission failed with HTTP %d", response.StatusCode)
	}
	result, err := decodeControlPlaneSubmissionStream(response.Body, func(accepted runCommandJSON) {
		if runtime.Accepted != nil {
			runtime.Accepted(runOutcomeFromCommandJSON(accepted))
		}
	})
	if err != nil {
		return runOutcome{}, fmt.Errorf("decode Control Plane submission: %w", err)
	}
	outcome := runOutcomeFromCommandJSON(result)
	if outcome.Result.Status != resultStatusPassed {
		return outcome, fmt.Errorf("control plane run %s finished with status %s", outcome.RunID, outcome.Result.Status)
	}
	return outcome, nil
}

func decodeControlPlaneSubmissionStream(reader io.Reader, onAccepted func(runCommandJSON)) (runCommandJSON, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 1024*1024+1))
	decoder.DisallowUnknownFields()
	var accepted runCommandJSON
	if err := decoder.Decode(&accepted); err != nil {
		return runCommandJSON{}, err
	}
	if accepted.Version != controlPlaneAPIVersion || accepted.Kind != "run-accepted" || !accepted.Accepted || accepted.RunID == "" {
		return runCommandJSON{}, fmt.Errorf("control plane returned an unsupported acceptance response")
	}
	if onAccepted != nil {
		onAccepted(accepted)
	}
	var terminal runCommandJSON
	if err := decoder.Decode(&terminal); err != nil {
		return runCommandJSON{}, err
	}
	if terminal.Version != controlPlaneAPIVersion || terminal.Kind != "run" || terminal.RunID != accepted.RunID {
		return runCommandJSON{}, fmt.Errorf("control plane returned an unsupported terminal response")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return runCommandJSON{}, fmt.Errorf("control plane submission contains more than two records")
		}
		return runCommandJSON{}, err
	}
	return terminal, nil
}

func buildControlPlaneSubmission(repoDir string, options runOptions) (controlPlaneSubmission, error) {
	submission := controlPlaneSubmission{
		Version: controlPlaneAPIVersion, Scenario: options.ScenarioName, TargetProfile: options.TargetProfile,
		StopAfter: options.StopAfter,
	}
	if options.KeepTTL > 0 {
		submission.KeepTTL = options.KeepTTL.String()
	}
	configPath, err := DiscoverConfig(repoDir)
	if errors.Is(err, errRepoRunConfigNotFound) {
		return submission, nil
	}
	if err != nil {
		return controlPlaneSubmission{}, err
	}
	configContent, configInfo, err := readControlPlaneSnapshotFile(repoDir, filepath.Base(configPath))
	if err != nil {
		return controlPlaneSubmission{}, err
	}
	estimatedBytes := int64(4096 + len(filepath.Base(configPath)))
	if err := addControlPlaneSnapshotEstimate(&estimatedBytes, configInfo.Size(), ""); err != nil {
		return controlPlaneSubmission{}, err
	}
	submission.ConfigName = filepath.Base(configPath)
	submission.Config = configContent
	var config repoRunConfig
	if err := decodeKnownYAMLFields(configContent, &config); err != nil || config.Version != 1 || len(config.Scenarios) == 0 {
		return submission, nil
	}
	scenario, err := resolveScenarioContract(config, options.ScenarioName)
	if err != nil {
		return submission, nil
	}
	seen := map[string]bool{submission.ConfigName: true}
	for _, payload := range scenario.Payload {
		if err := appendControlPlanePath(repoDir, payload.Source, &submission.Files, seen, &estimatedBytes); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return controlPlaneSubmission{}, err
		}
	}
	return submission, nil
}

func parseKeepTTL(value string) (time.Duration, error) {
	if value == "" {
		return defaultKeepTTL, nil
	}
	keepTTL, err := time.ParseDuration(value)
	if err != nil || keepTTL <= 0 {
		return 0, fmt.Errorf("invalid keep TTL %q", value)
	}
	return keepTTL, nil
}

func keepTTLOrDefault(value string) time.Duration {
	keepTTL, err := parseKeepTTL(value)
	if err != nil {
		return defaultKeepTTL
	}
	return keepTTL
}

func appendControlPlanePath(repoDir string, relativePath string, files *[]controlPlaneFile, seen map[string]bool, estimatedBytes *int64) error {
	clean, err := cleanRepoRelativePath(relativePath)
	if err != nil {
		return err
	}
	if err := ensureWorkspacePathHasNoSymlinkAncestor(repoDir, clean); err != nil {
		return fmt.Errorf("payload path %s must stay within the repo without symlinks: %w", relativePath, err)
	}
	root := filepath.Join(repoDir, clean)
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("payload path %s must not contain a symlink", relativePath)
		}
		relative, err := filepath.Rel(repoDir, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if seen[relative] {
			return nil
		}
		seen[relative] = true
		if entry.IsDir() {
			if err := addControlPlaneSnapshotEstimate(estimatedBytes, 0, relative); err != nil {
				return err
			}
			*files = append(*files, controlPlaneFile{Path: relative, Kind: "directory"})
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("payload path %s must contain only regular files", relativePath)
		}
		if err := addControlPlaneSnapshotEstimate(estimatedBytes, info.Size(), relative); err != nil {
			return err
		}
		content, openedInfo, err := readControlPlaneSnapshotFile(repoDir, relative)
		if err != nil {
			return err
		}
		if openedInfo.Size() != info.Size() {
			return fmt.Errorf("payload path %s changed while creating the snapshot", relative)
		}
		*files = append(*files, controlPlaneFile{Path: relative, Kind: "file", Content: content})
		return nil
	})
}

func readControlPlaneSnapshotFile(repoDir string, relativePath string) ([]byte, os.FileInfo, error) {
	if err := ensureWorkspacePathHasNoSymlinkAncestor(repoDir, relativePath); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(repoDir, filepath.FromSlash(relativePath))
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() < 0 || openedInfo.Size() > maximumControlPlaneSubmissionBytes {
		return nil, nil, fmt.Errorf("snapshot path %s must be a bounded regular file", relativePath)
	}
	if err := ensureWorkspacePathHasNoSymlinkAncestor(repoDir, relativePath); err != nil {
		return nil, nil, err
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 || !pathInfo.Mode().IsRegular() || !os.SameFile(openedInfo, pathInfo) {
		return nil, nil, fmt.Errorf("snapshot path %s changed or became a symlink while opening", relativePath)
	}
	content, err := io.ReadAll(io.LimitReader(file, maximumControlPlaneSubmissionBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(content) > maximumControlPlaneSubmissionBytes || int64(len(content)) != openedInfo.Size() {
		return nil, nil, fmt.Errorf("snapshot path %s changed or exceeded the submission size limit while reading", relativePath)
	}
	return content, openedInfo, nil
}

func addControlPlaneSnapshotEstimate(total *int64, contentBytes int64, path string) error {
	if contentBytes < 0 || contentBytes > maximumControlPlaneSubmissionBytes {
		return fmt.Errorf("control plane snapshot exceeds submission size limit")
	}
	encodedBytes := ((contentBytes + 2) / 3) * 4
	encodedPath, err := json.Marshal(path)
	if err != nil {
		return err
	}
	*total += encodedBytes + int64(len(encodedPath)) + 128
	if *total > maximumControlPlaneSnapshotEstimateBytes {
		return fmt.Errorf("control plane snapshot exceeds submission size limit")
	}
	return nil
}

func materializeControlPlaneSubmission(repoDir string, submission controlPlaneSubmission) error {
	if submission.ConfigName == "" && len(submission.Config) == 0 {
		return nil
	}
	if submission.ConfigName != defaultConfigName && submission.ConfigName != "maya-stall.yaml" {
		return fmt.Errorf("unsupported Repo Run Config name")
	}
	if err := os.WriteFile(filepath.Join(repoDir, submission.ConfigName), submission.Config, 0o600); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, file := range submission.Files {
		clean, err := cleanRepoRelativePath(file.Path)
		if err != nil {
			return err
		}
		clean = filepath.ToSlash(clean)
		if seen[clean] || clean == submission.ConfigName {
			return fmt.Errorf("duplicate submission path")
		}
		seen[clean] = true
		path := filepath.Join(repoDir, filepath.FromSlash(clean))
		switch file.Kind {
		case "directory":
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case "file":
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(path, file.Content, 0o600); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported submission file kind")
		}
	}
	return nil
}

func runCommandJSONForOutcome(outcome runOutcome) runCommandJSON {
	result := runCommandJSON{
		Version: controlPlaneAPIVersion, Kind: "run", Accepted: outcome.Accepted, RunID: outcome.RunID,
		Scenario: outcome.Scenario, TargetProfile: outcome.TargetProfile, Host: outcome.Host,
		Status: outcome.Result.Status, StopPolicy: outcome.StopPolicy,
		Warnings: outcome.Warnings,
	}
	if outcome.Failure != nil {
		result.FailedLayer = outcome.Failure.FailedLayer
		result.Diagnostic = outcome.Failure.Diagnostic
		result.RemediationHint = outcome.Failure.RemediationHint
	}
	return result
}

func runOutcomeFromCommandJSON(result runCommandJSON) runOutcome {
	outcome := runOutcome{
		RunID: result.RunID, Scenario: result.Scenario, TargetProfile: result.TargetProfile, Host: result.Host,
		StateDir: result.StateDir, EvidenceDir: result.EvidenceDir, Result: ScenarioResult{Status: result.Status},
		StopPolicy: result.StopPolicy, FollowUpCommands: result.FollowUpCommands, Accepted: result.Accepted,
		Warnings: result.Warnings,
	}
	if result.FailedLayer != "" {
		outcome.Failure = &runFailureEvidence{FailedLayer: result.FailedLayer, Diagnostic: result.Diagnostic, RemediationHint: result.RemediationHint}
	}
	return outcome
}
