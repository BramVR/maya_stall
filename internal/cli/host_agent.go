package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const hostAgentAPIVersion = 1
const maximumHostAgentCredentialBytes = 4096
const minimumHostAgentCredentialBytes = 32
const hostAgentSessionLease = time.Minute
const hostAgentHeartbeatInterval = 15 * time.Second
const hostAgentHeartbeatRequestTimeout = 10 * time.Second
const hostAgentProgressInterval = time.Second
const maximumHostAgentProgressEventBytes = 4 * 1024 * 1024
const maximumHostAgentProgressLogBytes = 4 * 1024 * 1024
const maximumHostAgentProgressEvents = 10000
const maximumConsecutiveHostAgentProgressSnapshotFailures = 3

type controlPlaneEnrollAgentOptions struct {
	ControlPlane  string
	AgentID       string
	HostID        string
	CredentialEnv string
	TokenEnv      string
}

type hostAgentRunOnceOptions struct {
	ControlPlane  string
	AgentID       string
	HostID        string
	WorkRoot      string
	HostConfig    string
	CredentialEnv string
	SessionID     string
	Capabilities  mayaHostCapabilityRecord
}

type hostAgentEnrollmentRequest struct {
	Version    int    `json:"version"`
	AgentID    string `json:"agentId"`
	HostID     string `json:"hostId"`
	Credential string `json:"credential"`
}

type hostAgentEnrollmentRecord struct {
	Version          int    `json:"version"`
	AgentID          string `json:"agentId"`
	HostID           string `json:"hostId"`
	CredentialSHA256 string `json:"credentialSha256"`
	EnrolledAt       string `json:"enrolledAt"`
}

type hostAgentRegistrationRequest struct {
	Version         int                      `json:"version"`
	AgentID         string                   `json:"agentId"`
	HostID          string                   `json:"hostId"`
	Slots           int                      `json:"slots"`
	SessionBinding  bool                     `json:"sessionBinding"`
	DeadlineActions bool                     `json:"deadlineActions"`
	Capabilities    mayaHostCapabilityRecord `json:"capabilities"`
}

type hostAgentNextRequest struct {
	Version   int    `json:"version"`
	SessionID string `json:"sessionId"`
}

type hostAgentHeartbeatRequest struct {
	Version      int                      `json:"version"`
	SessionID    string                   `json:"sessionId"`
	Capabilities mayaHostCapabilityRecord `json:"capabilities"`
}

type hostAgentStatusResponse struct {
	Version         int                      `json:"version"`
	Kind            string                   `json:"kind"`
	AgentID         string                   `json:"agentId"`
	HostID          string                   `json:"hostId"`
	Slots           int                      `json:"slots"`
	State           string                   `json:"state"`
	RunID           string                   `json:"runId,omitempty"`
	SessionID       string                   `json:"sessionId,omitempty"`
	SessionBinding  bool                     `json:"sessionBinding"`
	DeadlineActions bool                     `json:"deadlineActions"`
	Capabilities    mayaHostCapabilityRecord `json:"-"`
}

type hostAgentAssignmentResponse struct {
	Version           int                      `json:"version"`
	Kind              string                   `json:"kind"`
	RunID             string                   `json:"runId"`
	AgentID           string                   `json:"agentId"`
	HostID            string                   `json:"hostId"`
	LockToken         string                   `json:"lockToken"`
	Submission        controlPlaneSubmission   `json:"submission"`
	EventPrefix       []byte                   `json:"eventPrefix,omitempty"`
	Capabilities      mayaHostCapabilityRecord `json:"-"`
	SelectedMayaBuild string                   `json:"selectedMayaBuild,omitempty"`
	Action            string                   `json:"action,omitempty"`
	HardDeadline      string                   `json:"hardDeadline,omitempty"`
	KeepDeadline      string                   `json:"keepDeadline,omitempty"`
	BrokerSession     *brokerSessionIdentity   `json:"brokerSession,omitempty"`
	ExpiryFromState   string                   `json:"expiryFromState,omitempty"`
	ExpiryReason      string                   `json:"expiryReason,omitempty"`
	DeadlineEvents    []hostLockDeadlineEvent  `json:"deadlineEvents,omitempty"`
}

type hostAgentStatusResponseAlias hostAgentStatusResponse

func (status hostAgentStatusResponse) MarshalJSON() ([]byte, error) {
	var capabilities *mayaHostCapabilityRecord
	if status.Capabilities.Version != 0 {
		snapshot := snapshotMayaHostCapabilityRecord(status.Capabilities)
		capabilities = &snapshot
	}
	return json.Marshal(struct {
		hostAgentStatusResponseAlias
		Capabilities *mayaHostCapabilityRecord `json:"capabilities,omitempty"`
	}{hostAgentStatusResponseAlias: hostAgentStatusResponseAlias(status), Capabilities: capabilities})
}

func (status *hostAgentStatusResponse) UnmarshalJSON(content []byte) error {
	decoded := struct {
		hostAgentStatusResponseAlias
		Capabilities *mayaHostCapabilityRecord `json:"capabilities,omitempty"`
	}{}
	if err := decodeHostAgentExtensionJSON(content, &decoded); err != nil {
		return err
	}
	*status = hostAgentStatusResponse(decoded.hostAgentStatusResponseAlias)
	if decoded.Capabilities != nil {
		status.Capabilities = snapshotMayaHostCapabilityRecord(*decoded.Capabilities)
	}
	return nil
}

type hostAgentAssignmentResponseAlias hostAgentAssignmentResponse

func (assignment hostAgentAssignmentResponse) MarshalJSON() ([]byte, error) {
	var capabilities *mayaHostCapabilityRecord
	if assignment.Capabilities.Version != 0 {
		snapshot := snapshotMayaHostCapabilityRecord(assignment.Capabilities)
		capabilities = &snapshot
	}
	return json.Marshal(struct {
		hostAgentAssignmentResponseAlias
		Capabilities *mayaHostCapabilityRecord `json:"capabilities,omitempty"`
	}{hostAgentAssignmentResponseAlias: hostAgentAssignmentResponseAlias(assignment), Capabilities: capabilities})
}

func (assignment *hostAgentAssignmentResponse) UnmarshalJSON(content []byte) error {
	decoded := struct {
		hostAgentAssignmentResponseAlias
		Capabilities *mayaHostCapabilityRecord `json:"capabilities,omitempty"`
	}{}
	if err := decodeHostAgentExtensionJSON(content, &decoded); err != nil {
		return err
	}
	*assignment = hostAgentAssignmentResponse(decoded.hostAgentAssignmentResponseAlias)
	if decoded.Capabilities != nil {
		assignment.Capabilities = snapshotMayaHostCapabilityRecord(*decoded.Capabilities)
	}
	return nil
}

func decodeHostAgentExtensionJSON(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("Host Agent response contains multiple JSON values") //nolint:staticcheck // Host Agent is a product term.
		}
		return err
	}
	return nil
}

type hostAgentLockRequest struct {
	Version   int    `json:"version"`
	RunID     string `json:"runId"`
	LockToken string `json:"lockToken"`
	SessionID string `json:"sessionId"`
}

type hostAgentSessionRequest struct {
	Version       int                   `json:"version"`
	RunID         string                `json:"runId"`
	LockToken     string                `json:"lockToken"`
	SessionID     string                `json:"sessionId"`
	BrokerSession brokerSessionIdentity `json:"brokerSession"`
}

type hostAgentProgressRequest struct {
	Version    int             `json:"version"`
	RunID      string          `json:"runId"`
	LockToken  string          `json:"lockToken"`
	SessionID  string          `json:"sessionId"`
	Checkpoint int64           `json:"checkpoint"`
	Ledger     runLedgerRecord `json:"ledger"`
	Events     []byte          `json:"events"`
	Log        []byte          `json:"log"`
}

type hostAgentCompletionRequest struct {
	Version   int                `json:"version"`
	RunID     string             `json:"runId"`
	LockToken string             `json:"lockToken"`
	SessionID string             `json:"sessionId"`
	Terminal  runCommandJSON     `json:"terminal"`
	Files     []controlPlaneFile `json:"files"`
}

type hostAgentKeptRequest struct {
	Version   int                      `json:"version"`
	RunID     string                   `json:"runId"`
	LockToken string                   `json:"lockToken"`
	SessionID string                   `json:"sessionId"`
	Outcome   runCommandJSON           `json:"outcome"`
	Progress  hostAgentProgressRequest `json:"progress"`
}

type hostAgentKeptResponse struct {
	Version         int                     `json:"version"`
	Kind            string                  `json:"kind"`
	RunID           string                  `json:"runId"`
	State           string                  `json:"state"`
	Action          string                  `json:"action"`
	IdleDeadline    string                  `json:"idleDeadline"`
	HardDeadline    string                  `json:"hardDeadline"`
	KeepDeadline    string                  `json:"keepDeadline"`
	KeepRemaining   string                  `json:"keepRemaining"`
	ExpiryFromState string                  `json:"expiryFromState,omitempty"`
	ExpiryReason    string                  `json:"expiryReason,omitempty"`
	BrokerSession   *brokerSessionIdentity  `json:"brokerSession,omitempty"`
	DeadlineEvents  []hostLockDeadlineEvent `json:"deadlineEvents,omitempty"`
}

type hostAgentFailureRequest struct {
	Version    int    `json:"version"`
	RunID      string `json:"runId"`
	LockToken  string `json:"lockToken"`
	SessionID  string `json:"sessionId"`
	Diagnostic string `json:"diagnostic"`
	Quarantine bool   `json:"quarantine,omitempty"`
}

type hostAgentMutationOutbox struct {
	Version    int                         `json:"version"`
	RunID      string                      `json:"runId"`
	LockToken  string                      `json:"lockToken"`
	Kind       string                      `json:"kind"`
	Cleanup    *hostAgentCleanupMutation   `json:"cleanup,omitempty"`
	Completion *hostAgentCompletionRequest `json:"completion,omitempty"`
	Failure    *hostAgentFailureRequest    `json:"failure,omitempty"`
}

type hostAgentCleanupMutation struct {
	BrokerSession   brokerSessionIdentity `json:"brokerSession"`
	ExpiryFromState string                `json:"expiryFromState"`
}

type hostAgentLockRecord struct {
	Version   int    `json:"version"`
	RunID     string `json:"runId"`
	AgentID   string `json:"agentId"`
	HostID    string `json:"hostId"`
	LockToken string `json:"lockToken"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
	hostLockDeadlines
	ExpiryFromState        string                 `json:"expiryFromState,omitempty"`
	ExpiryReason           string                 `json:"expiryReason,omitempty"`
	SessionBindingRequired bool                   `json:"sessionBindingRequired,omitempty"`
	BrokerSession          *brokerSessionIdentity `json:"brokerSession,omitempty"`
}

type hostAgentAssignmentRecord struct {
	Version   int    `json:"version"`
	RunID     string `json:"runId"`
	AgentID   string `json:"agentId"`
	HostID    string `json:"hostId"`
	LockToken string `json:"lockToken"`
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
	hostLockDeadlines
	Submission             controlPlaneSubmission   `json:"submission"`
	EventPrefix            []byte                   `json:"eventPrefix,omitempty"`
	Capabilities           mayaHostCapabilityRecord `json:"capabilities"`
	SelectedMayaBuild      string                   `json:"selectedMayaBuild,omitempty"`
	SessionBindingRequired bool                     `json:"sessionBindingRequired,omitempty"`
	BrokerSession          *brokerSessionIdentity   `json:"brokerSession,omitempty"`
	Terminal               *runCommandJSON          `json:"terminal,omitempty"`
	TerminalLedger         *runLedgerRecord         `json:"terminalLedger,omitempty"`
	AssignedLedger         *runLedgerRecord         `json:"assignedLedger,omitempty"`
	ProgressSequence       int64                    `json:"progressSequence,omitempty"`
	ProgressDigest         string                   `json:"progressDigest,omitempty"`
	DeadlineEvents         []hostLockDeadlineEvent  `json:"deadlineEvents,omitempty"`
	ExpiryFromState        string                   `json:"expiryFromState,omitempty"`
	ExpiryReason           string                   `json:"expiryReason,omitempty"`
}

type hostLockDeadlineEvent struct {
	Type      string `json:"type"`
	Detail    string `json:"detail"`
	Timestamp string `json:"timestamp"`
	Ordinal   int    `json:"ordinal"`
}

type controlPlaneHostAgent struct {
	enrollment        hostAgentEnrollmentRecord
	status            hostAgentStatusResponse
	notify            chan struct{}
	sessionExpiresAt  time.Time
	takeoverNotBefore time.Time
}

type controlPlaneHostAgentAssignment struct {
	checkpointMu          sync.Mutex
	record                hostAgentAssignmentRecord
	repoDir               string
	done                  chan struct{}
	outcome               runOutcome
	err                   error
	run                   *freshRunLifecycle
	finishing             bool
	finished              bool
	originalLedger        *runLedgerRecord
	terminalLedger        *runLedgerRecord
	sharedFakeHostRelease func() error
}

type controlPlaneHTTPStatusError struct {
	StatusCode int
	Message    string
}

func (err *controlPlaneHTTPStatusError) Error() string {
	if err.Message != "" {
		return fmt.Sprintf("Control Plane request failed with HTTP %d: %s", err.StatusCode, err.Message)
	}
	return fmt.Sprintf("Control Plane request failed with HTTP %d", err.StatusCode)
}

func validateHostAgentID(agentID string) error {
	return validateHostAgentStateID(agentID, "Windows Host Agent")
}

func validateHostAgentHostID(hostID string) error {
	if err := validateHostID(hostID); err != nil {
		return err
	}
	return validateHostAgentStateID(hostID, "Maya Host")
}

func validateHostAgentStateID(value string, label string) error {
	if len(value) == 0 || len(value) > 63 {
		return fmt.Errorf("%s id must contain 1 through 63 portable characters", label)
	}
	for index, character := range []byte(value) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '-' && index > 0 && index < len(value)-1 {
			continue
		}
		return fmt.Errorf("%s id must use lowercase ASCII letters, digits, and interior hyphens", label)
	}
	switch value {
	case "con", "prn", "aux", "nul", "com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9", "lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		return fmt.Errorf("%s id %q is reserved on Windows", label, value)
	}
	return nil
}

func parseControlPlaneEnrollAgentArgs(args []string) (controlPlaneEnrollAgentOptions, error) {
	var options controlPlaneEnrollAgentOptions
	for index := 0; index < len(args); index++ {
		flag := args[index]
		switch flag {
		case "--control-plane", "--agent-id", "--host", "--credential-env", "--token-env":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return controlPlaneEnrollAgentOptions{}, newUsageError("%s needs a value", flag)
			}
			switch flag {
			case "--control-plane":
				options.ControlPlane = args[index]
			case "--agent-id":
				options.AgentID = args[index]
			case "--host":
				options.HostID = args[index]
			case "--credential-env":
				options.CredentialEnv = args[index]
			case "--token-env":
				options.TokenEnv = args[index]
			}
		default:
			return controlPlaneEnrollAgentOptions{}, newUsageError("unknown control-plane enroll-agent option %q", flag)
		}
	}
	if _, err := parseControlPlaneURL(options.ControlPlane); err != nil {
		return controlPlaneEnrollAgentOptions{}, err
	}
	if err := validateHostAgentID(options.AgentID); err != nil {
		return controlPlaneEnrollAgentOptions{}, newUsageError("invalid Windows Host Agent id: %v", err)
	}
	if err := validateHostAgentHostID(options.HostID); err != nil {
		return controlPlaneEnrollAgentOptions{}, err
	}
	if options.CredentialEnv == "" {
		return controlPlaneEnrollAgentOptions{}, newUsageError("control-plane enroll-agent needs --credential-env")
	}
	return options, nil
}

func parseHostAgentRunOnceArgs(args []string, workDir string) (hostAgentRunOnceOptions, error) {
	var options hostAgentRunOnceOptions
	for index := 0; index < len(args); index++ {
		flag := args[index]
		switch flag {
		case "--control-plane", "--agent-id", "--host", "--work-root", "--host-config", "--credential-env":
			index++
			if index >= len(args) || args[index] == "" || strings.HasPrefix(args[index], "--") {
				return hostAgentRunOnceOptions{}, newUsageError("%s needs a value", flag)
			}
			switch flag {
			case "--control-plane":
				options.ControlPlane = args[index]
			case "--agent-id":
				options.AgentID = args[index]
			case "--host":
				options.HostID = args[index]
			case "--work-root":
				options.WorkRoot = resolveFromRepo(workDir, args[index])
			case "--host-config":
				options.HostConfig = resolveFromRepo(workDir, args[index])
			case "--credential-env":
				options.CredentialEnv = args[index]
			}
		default:
			return hostAgentRunOnceOptions{}, newUsageError("unknown host-agent run-once option %q", flag)
		}
	}
	if _, err := parseControlPlaneURL(options.ControlPlane); err != nil {
		return hostAgentRunOnceOptions{}, err
	}
	if err := validateHostAgentID(options.AgentID); err != nil {
		return hostAgentRunOnceOptions{}, newUsageError("invalid Windows Host Agent id: %v", err)
	}
	if err := validateHostAgentHostID(options.HostID); err != nil {
		return hostAgentRunOnceOptions{}, err
	}
	if options.WorkRoot == "" {
		return hostAgentRunOnceOptions{}, newUsageError("host-agent run-once needs --work-root")
	}
	if options.CredentialEnv == "" {
		return hostAgentRunOnceOptions{}, newUsageError("host-agent run-once needs --credential-env")
	}
	return options, nil
}

func enrollControlPlaneHostAgent(options controlPlaneEnrollAgentOptions, runtime runRuntime, stdout io.Writer) error {
	credential, ok := os.LookupEnv(options.CredentialEnv)
	if !ok || len(credential) < minimumHostAgentCredentialBytes {
		return fmt.Errorf("Windows Host Agent credential environment variable %s must contain at least %d bytes", options.CredentialEnv, minimumHostAgentCredentialBytes) //nolint:staticcheck // Product name starts the user-facing diagnostic.
	}
	if len(credential) > maximumHostAgentCredentialBytes {
		return fmt.Errorf("Windows Host Agent credential exceeds size limit") //nolint:staticcheck // Product name starts the user-facing diagnostic.
	}
	request := hostAgentEnrollmentRequest{
		Version: hostAgentAPIVersion, AgentID: options.AgentID, HostID: options.HostID, Credential: credential,
	}
	var status hostAgentStatusResponse
	if err := postControlPlaneJSON(options.ControlPlane, options.TokenEnv, "/v1/host-agents/enroll", request, runtime, http.StatusCreated, &status); err != nil {
		return err
	}
	_, err := fmt.Fprintf(stdout, "agent: %s\nhost: %s\nstate: %s\nslots: %d\n", status.AgentID, status.HostID, status.State, status.Slots)
	return err
}

func postControlPlaneJSON(rawURL string, tokenEnv string, path string, body any, runtime runRuntime, wantStatus int, target any) error {
	return postControlPlaneJSONWithLimit(rawURL, tokenEnv, path, body, runtime, wantStatus, target, maximumControlPlaneDefaultResponseBytes)
}

func postControlPlaneJSONWithLimit(rawURL string, tokenEnv string, path string, body any, runtime runRuntime, wantStatus int, target any, responseLimit int64) error {
	return postControlPlaneJSONWithLimitContext(context.Background(), rawURL, tokenEnv, path, body, runtime, wantStatus, target, responseLimit)
}

func postControlPlaneJSONWithLimitContext(ctx context.Context, rawURL string, tokenEnv string, path string, body any, runtime runRuntime, wantStatus int, target any, responseLimit int64) error {
	endpoint, err := parseControlPlaneURL(rawURL)
	if err != nil {
		return err
	}
	if tokenEnv == "" {
		tokenEnv = defaultControlPlaneTokenEnv
	}
	token, ok := os.LookupEnv(tokenEnv)
	if !ok || token == "" {
		return fmt.Errorf("Control Plane credential environment variable %s is not set", tokenEnv) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	content, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if len(content) > maximumControlPlaneSubmissionBytes {
		return fmt.Errorf("Control Plane request exceeds size limit") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint.String(), "/")+path, bytes.NewReader(content))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := controlPlaneHTTPClient(runtime.ControlPlaneHTTPClient).Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != wantStatus {
		var failure struct {
			Error string `json:"error"`
		}
		_ = decodeBoundedControlPlaneJSON(response.Body, 4096, &failure)
		return &controlPlaneHTTPStatusError{StatusCode: response.StatusCode, Message: failure.Error}
	}
	if target == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	return decodeBoundedControlPlaneJSON(response.Body, responseLimit, target)
}

func ensureHostAgentDirectory(path string) error {
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
		return fmt.Errorf("Windows Host Agent directory %s must be a directory and not a symlink", path) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	return validateHostAgentDirectoryPermissions(path, info)
}

func (handler *controlPlaneHandler) loadHostAgentState() error {
	root := filepath.Join(handler.dataDir, "host-agents")
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || validateHostAgentID(entry.Name()) != nil {
			return fmt.Errorf("invalid Windows Host Agent state directory %s", entry.Name())
		}
		content, err := os.ReadFile(filepath.Join(root, entry.Name(), "enrollment.json"))
		if err != nil {
			return err
		}
		var enrollment hostAgentEnrollmentRecord
		if err := json.Unmarshal(content, &enrollment); err != nil {
			return err
		}
		if enrollment.Version != hostAgentAPIVersion || enrollment.AgentID != entry.Name() || validateHostAgentHostID(enrollment.HostID) != nil || len(enrollment.CredentialSHA256) != sha256.Size*2 {
			return fmt.Errorf("invalid Windows Host Agent enrollment %s", entry.Name())
		}
		status := hostAgentStatusResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-status", AgentID: enrollment.AgentID,
			HostID: enrollment.HostID, Slots: 1, State: "offline",
		}
		var persistedStatus hostAgentStatusResponse
		if err := readPrivateJSON(filepath.Join(root, entry.Name(), "status.json"), &persistedStatus); err != nil {
			return err
		}
		if persistedStatus.Version != hostAgentAPIVersion || persistedStatus.Kind != "host-agent-status" || persistedStatus.AgentID != enrollment.AgentID || persistedStatus.HostID != enrollment.HostID || persistedStatus.Slots != 1 {
			return fmt.Errorf("invalid Windows Host Agent status %s", entry.Name())
		}
		if persistedStatus.State == "quarantined" {
			if validateRunID(persistedStatus.RunID) != nil || persistedStatus.SessionID != "" {
				return fmt.Errorf("invalid quarantined Windows Host Agent status %s", entry.Name())
			}
			status.State = "quarantined"
			status.RunID = persistedStatus.RunID
		}
		handler.hostAgents[entry.Name()] = &controlPlaneHostAgent{
			enrollment: enrollment,
			status:     status,
			notify:     make(chan struct{}),
		}
	}
	if err := handler.recoverHostAgentQuarantineMarkers(); err != nil {
		return err
	}
	if err := newHostAgentTransitionStore(handler.dataDir).Recover(handler.prepareRecoveredHostAgentTransition); err != nil {
		return err
	}
	lockEntries, err := os.ReadDir(filepath.Join(handler.dataDir, "host-locks"))
	if err != nil {
		return err
	}
	for _, entry := range lockEntries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("invalid durable Host Lock path %s", entry.Name())
		}
		hostID := strings.TrimSuffix(entry.Name(), ".json")
		if validateHostAgentHostID(hostID) != nil {
			return fmt.Errorf("invalid durable Host Lock path %s", entry.Name())
		}
		var lock hostAgentLockRecord
		if err := readPrivateJSON(filepath.Join(handler.dataDir, "host-locks", entry.Name()), &lock); err != nil {
			return err
		}
		validState := lock.State == "assigned" || lock.State == "confirmed" || lock.State == "kept" || lock.State == "expiring" || lock.State == "finishing" || lock.State == "quarantined"
		if lock.Version != hostAgentAPIVersion || lock.HostID != hostID || validateRunID(lock.RunID) != nil || lock.LockToken == "" || !validState || invalidBrokerSession(lock.BrokerSession) {
			return fmt.Errorf("invalid durable Host Lock for %s", hostID)
		}
		agent := handler.hostAgents[lock.AgentID]
		if agent == nil || agent.enrollment.HostID != hostID || agent.status.RunID != "" && (agent.status.State != "quarantined" || agent.status.RunID != lock.RunID) {
			return fmt.Errorf("durable Host Lock for %s has no unique enrollment", hostID)
		}
		var assignment hostAgentAssignmentRecord
		if err := readPrivateJSON(filepath.Join(handler.dataDir, "assignments", lock.RunID+".json"), &assignment); err != nil {
			return err
		}
		if assignment.Version != hostAgentAPIVersion || assignment.RunID != lock.RunID || assignment.AgentID != lock.AgentID || assignment.HostID != lock.HostID || !sameLockToken(assignment.LockToken, lock.LockToken) || assignment.State != lock.State || assignment.ExpiryFromState != lock.ExpiryFromState || assignment.ExpiryReason != lock.ExpiryReason || assignment.SessionBindingRequired != lock.SessionBindingRequired || !sameBrokerSession(assignment.BrokerSession, lock.BrokerSession) {
			return fmt.Errorf("durable assignment for %s does not match its Host Lock", lock.RunID)
		}
		if assignment.HardDeadline == "" && lock.HardDeadline == "" {
			assignment.hostLockDeadlines = newHostLockDeadlines(handler.runtime.Now(), handler.hostLockPolicy)
			if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment, handler.prepareRecoveredHostAgentTransition); err != nil {
				return fmt.Errorf("stamp durable Host Lock deadlines for %s: %w", lock.RunID, err)
			}
			lock.hostLockDeadlines = assignment.hostLockDeadlines
		} else if assignment.hostLockDeadlines != lock.hostLockDeadlines || assignment.validate() != nil {
			return fmt.Errorf("invalid durable Host Lock deadlines for %s", hostID)
		}
		if assignment.State == "finishing" {
			if assignment.Terminal == nil || assignment.TerminalLedger == nil {
				return fmt.Errorf("finishing Host Agent assignment %s has no terminal state", assignment.RunID)
			}
			if err := validateFinishingHostAgentAssignment(assignment); err != nil {
				return fmt.Errorf("invalid finishing Host Agent assignment %s: %w", assignment.RunID, err)
			}
			if !agent.status.DeadlineActions {
				repoDir := filepath.Join(handler.dataDir, "runs", assignment.RunID, "repo")
				if err := cleanupRunState(repoDir, assignment.RunID); err != nil {
					return fmt.Errorf("resume legacy finishing Host Agent cleanup: %w", err)
				}
				if err := removeRepoHostLockForRun(filepath.Join(handler.dataDir, "fake-host"), assignment.HostID, assignment.RunID); err != nil {
					return fmt.Errorf("resume legacy finishing shared Host Lock release: %w", err)
				}
				completed := assignment
				completed.State = "completed"
				if err := newHostAgentTransitionStore(handler.dataDir).Commit(completed, handler.prepareRecoveredHostAgentTransition); err != nil {
					return fmt.Errorf("resume legacy completed Host Agent transition: %w", err)
				}
				continue
			}
		}
		agent.status.RunID = lock.RunID
		if lock.State == "quarantined" || agent.status.State == "quarantined" {
			agent.status.State = "quarantined"
			assignment.State = "quarantined"
		}
		agent.takeoverNotBefore = handler.runtime.Now().Add(hostAgentHeartbeatInterval + hostAgentHeartbeatRequestTimeout + defaultBrokerCancellationWait)
		sharedRepo := filepath.Join(handler.dataDir, "fake-host")
		if err := reestablishHostAgentSharedLock(sharedRepo, assignment.HostID, assignment.RunID); err != nil {
			return fmt.Errorf("re-establish shared Host Lock for %s: %w", assignment.RunID, err)
		}
		hostIDCopy := lock.HostID
		runIDCopy := lock.RunID
		handler.assignments[lock.RunID] = &controlPlaneHostAgentAssignment{
			record:  assignment,
			repoDir: filepath.Join(handler.dataDir, "runs", lock.RunID, "repo"),
			done:    make(chan struct{}),
			sharedFakeHostRelease: func() error {
				return removeRepoHostLockForRun(sharedRepo, hostIDCopy, runIDCopy)
			},
		}
	}
	return nil
}

func validateFinishingHostAgentAssignment(assignment hostAgentAssignmentRecord) error {
	if assignment.Terminal == nil || assignment.TerminalLedger == nil || assignment.AssignedLedger == nil {
		return fmt.Errorf("terminal state is incomplete")
	}
	if err := validateHostAgentCompletionIdentity(assignment, *assignment.Terminal); err != nil {
		return err
	}
	expectedProfile := assignment.Submission.TargetProfile
	if expectedProfile == "" {
		expectedProfile = "default"
	}
	ledger := assignment.TerminalLedger
	if ledger.RunID != assignment.RunID || ledger.Scenario != assignment.Submission.Scenario || ledger.TargetProfile != expectedProfile || ledger.Host != assignment.HostID || ledger.AcceptedAt != assignment.AssignedLedger.AcceptedAt || ledger.Status != assignment.Terminal.Status {
		return fmt.Errorf("terminal Run Ledger identity does not match assignment")
	}
	if ledger.Status == resultStatusPassed && ledger.State != "completed" || ledger.Status == resultStatusFailed && ledger.State != "failed" || ledger.Status != resultStatusPassed && ledger.Status != resultStatusFailed {
		return fmt.Errorf("terminal Run Ledger state does not match status")
	}
	return nil
}

func reestablishHostAgentSharedLock(repoDir string, hostID string, runID string) error {
	if err := validateHostID(hostID); err != nil {
		return err
	}
	if err := validateRunID(runID); err != nil {
		return err
	}
	lockDir := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts")
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return err
	}
	return withLocalHostSideMutex(lockDir, hostID, func() error {
		lockPath := filepath.Join(lockDir, hostID+".lock")
		content, readErr := os.ReadFile(lockPath)
		created := false
		if errors.Is(readErr, os.ErrNotExist) {
			_, locked, err := acquireHostLockAtDir(lockDir, hostID)
			if err != nil {
				return err
			}
			if locked {
				return fmt.Errorf("shared Host Lock is owned by another process")
			}
			created = true
		} else if readErr != nil {
			return readErr
		} else {
			owner := parseHostLockOwner(string(content))
			if owner.ActiveRun != runID {
				return fmt.Errorf("shared Host Lock belongs to another run")
			}
			processID := ""
			for _, line := range strings.Split(string(content), "\n") {
				key, value, ok := strings.Cut(line, ":")
				if ok && strings.TrimSpace(key) == "pid" {
					processID = strings.TrimSpace(value)
					break
				}
			}
			if processID == "" {
				return fmt.Errorf("shared Host Lock has no process owner")
			}
			if processID != fmt.Sprintf("%d", os.Getpid()) {
				stale, err := isStaleHostLock(lockPath)
				if err != nil {
					return err
				}
				if !stale {
					return fmt.Errorf("shared Host Lock is owned by another live process")
				}
			}
		}
		active := fmt.Sprintf("host: %s\npid: %d\nactiveRun: %s\nauthoritativeHostLock: false\n", hostID, os.Getpid(), runID)
		if err := replaceHostLockOwnerAtDir(lockDir, hostID, active); err != nil {
			if created {
				return errors.Join(err, os.Remove(lockPath))
			}
			return err
		}
		return nil
	})
}

func (handler *controlPlaneHandler) recoverHostAgentQuarantineMarkers() error {
	for agentID, agent := range handler.hostAgents {
		path := handler.hostAgentQuarantinePath(agentID)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		var record hostAgentAssignmentRecord
		if err := readPrivateJSON(path, &record); err != nil {
			return err
		}
		if record.Version != hostAgentAPIVersion || record.State != "quarantined" || record.AgentID != agentID || record.HostID != agent.enrollment.HostID || validateRunID(record.RunID) != nil || record.LockToken == "" || record.AssignedLedger == nil {
			return fmt.Errorf("invalid Windows Host Agent quarantine marker %s", agentID)
		}
		repoDir := filepath.Join(handler.dataDir, "runs", record.RunID, "repo")
		sharedRepo := filepath.Join(handler.dataDir, "fake-host")
		assignment := &controlPlaneHostAgentAssignment{
			record: record, repoDir: repoDir, done: make(chan struct{}),
			sharedFakeHostRelease: func() error {
				return removeRepoHostLockForRun(sharedRepo, record.HostID, record.RunID)
			},
		}
		handler.assignments[record.RunID] = assignment
		agent.status.State = "quarantined"
		agent.status.RunID = record.RunID
		agent.status.SessionID = ""
		if _, _, err := handler.quarantineHostAgentAssignment(assignment, agent, "Control Plane restarted during an incomplete Windows Host Agent assignment transition"); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := syncRunLedgerDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	}
	return nil
}

func (handler *controlPlaneHandler) prepareRecoveredHostAgentTransition(record hostAgentAssignmentRecord, entryName string) (hostAgentAssignmentRecord, error) {
	agent := handler.hostAgents[record.AgentID]
	if agent == nil {
		return hostAgentAssignmentRecord{}, fmt.Errorf("invalid Host Agent transaction %s", entryName)
	}
	return prepareRecoveredHostAgentTransitionSnapshot(handler.dataDir, agent.enrollment, agent.status, record, entryName)
}

func prepareRecoveredHostAgentTransitionSnapshot(dataDir string, enrollment hostAgentEnrollmentRecord, status hostAgentStatusResponse, record hostAgentAssignmentRecord, entryName string) (hostAgentAssignmentRecord, error) {
	if record.Version != hostAgentAPIVersion || validateRunID(record.RunID) != nil || entryName != record.RunID+".json" || enrollment.AgentID != record.AgentID || enrollment.HostID != record.HostID || record.LockToken == "" || record.State != "assigned" && record.State != "confirmed" && record.State != "kept" && record.State != "expiring" && record.State != "finishing" && record.State != "quarantined" && record.State != "completed" {
		return hostAgentAssignmentRecord{}, fmt.Errorf("invalid Host Agent transaction %s", entryName)
	}
	if status.State != "quarantined" || status.RunID != record.RunID || record.State == "completed" {
		return record, nil
	}
	terminalLedger, err := newRunLedgerStore(filepath.Join(dataDir, "runs", record.RunID, "repo")).Read(record.RunID)
	if err != nil {
		return hostAgentAssignmentRecord{}, fmt.Errorf("recover quarantined Host Agent transaction: %w", err)
	}
	targetProfile := record.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	terminal := runCommandJSON{
		Version: controlPlaneAPIVersion, Kind: "run", Accepted: true, RunID: record.RunID,
		Scenario: record.Submission.Scenario, TargetProfile: targetProfile, Host: record.HostID,
		Status: terminalLedger.Status, StopPolicy: "unresolved",
	}
	record.State = "quarantined"
	record.AssignedLedger = &terminalLedger
	record.TerminalLedger = &terminalLedger
	record.Terminal = &terminal
	return record, nil
}

func loadAcceptedHostAgentRun(repoDir string, record hostAgentAssignmentRecord, runtime runRuntime) (*freshRunLifecycle, error) {
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	manifest, stateDir, found, err := readStopRunManifest(repoDir, record.RunID)
	if err != nil || !found {
		return nil, errors.Join(fmt.Errorf("accepted Host Agent run manifest not found"), err)
	}
	workspace, err := newRunWorkspace(repoDir, record.RunID, "", evidenceStandaloneResultName)
	if err != nil {
		return nil, err
	}
	context := runContext{
		RepoDir: repoDir, RunWorkspace: workspace, StateDir: stateDir, EvidenceDir: workspace.EvidenceDir(),
		Workspace: workspace.LocalWorkspace(), EventsPath: workspace.EventsPath(), LogPath: workspace.LogPath(),
		ScenarioResultPath: workspace.LocalScenarioResultPath(),
		Environment:        map[string]string{scenarioResultEnvVar: workspace.LocalScenarioResultPath()},
	}
	policy := defaultRunLedgerPolicy()
	if available, ok := availableRunLedgerPolicy(repoDir); ok {
		policy = available
	}
	acceptedAt := runtime.Now()
	if ledger, ledgerErr := newRunLedgerStore(repoDir).Read(record.RunID); ledgerErr == nil {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, ledger.AcceptedAt); parseErr == nil {
			acceptedAt = parsed
		}
	}
	targetProfile := record.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	return &freshRunLifecycle{
		repoDir: repoDir, options: runOptions{
			ScenarioName: record.Submission.Scenario, TargetProfile: targetProfile, HostPin: record.HostID,
			StopAfter: record.Submission.StopAfter, KeepTTL: keepTTLOrDefault(record.Submission.KeepTTL), AssignedRunID: record.RunID,
		}, runtime: runtime, context: context, manifest: manifest, accepted: true, acceptedAt: acceptedAt,
		stopPolicy: "stopped", ledgerPolicy: policy,
	}, nil
}

func readPrivateJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("private state path %s must be a regular file, not a symlink", path)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("private state path %s contains trailing data", path)
	}
	return nil
}

func (handler *controlPlaneHandler) hasHostAgentEnrollments() bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	return len(handler.hostAgents) > 0
}

func (handler *controlPlaneHandler) serveHostAgentAPI(response http.ResponseWriter, request *http.Request) bool {
	parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "host-agents" {
		return false
	}
	if len(parts) == 3 && parts[2] == "enroll" {
		if !handler.authorizedOperator(request) {
			writeControlPlaneError(response, http.StatusUnauthorized, "authentication required")
			return true
		}
		if request.Method != http.MethodPost {
			http.NotFound(response, request)
			return true
		}
		handler.serveHostAgentEnrollment(response, request)
		return true
	}
	if len(parts) == 4 && parts[3] == "status" {
		if !handler.authorizedOperator(request) {
			writeControlPlaneError(response, http.StatusUnauthorized, "authentication required")
			return true
		}
		if request.Method != http.MethodGet {
			http.NotFound(response, request)
			return true
		}
		handler.serveHostAgentStatus(response, parts[2])
		return true
	}
	if len(parts) < 4 || !handler.authorizedHostAgent(request, parts[2]) {
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication required")
		return true
	}
	switch {
	case len(parts) == 4 && parts[3] == "register" && request.Method == http.MethodPost:
		handler.serveHostAgentRegistration(response, request, parts[2])
	case len(parts) == 4 && parts[3] == "heartbeat" && request.Method == http.MethodPost:
		handler.serveHostAgentHeartbeat(response, request, parts[2])
	case len(parts) == 5 && parts[3] == "assignments" && parts[4] == "next" && request.Method == http.MethodPost:
		handler.serveHostAgentNextAssignment(response, request, parts[2])
	case len(parts) == 6 && parts[3] == "assignments" && parts[5] == "confirm" && request.Method == http.MethodPost:
		handler.serveHostAgentConfirm(response, request, parts[2], parts[4])
	case len(parts) == 6 && parts[3] == "assignments" && parts[5] == "session" && request.Method == http.MethodPost:
		handler.serveHostAgentSession(response, request, parts[2], parts[4])
	case len(parts) == 6 && parts[3] == "assignments" && parts[5] == "progress" && request.Method == http.MethodPost:
		handler.serveHostAgentProgress(response, request, parts[2], parts[4])
	case len(parts) == 6 && parts[3] == "assignments" && parts[5] == "kept" && request.Method == http.MethodPost:
		handler.serveHostAgentKept(response, request, parts[2], parts[4])
	case len(parts) == 6 && parts[3] == "assignments" && parts[5] == "action" && request.Method == http.MethodPost:
		handler.serveHostAgentAction(response, request, parts[2], parts[4])
	case len(parts) == 6 && parts[3] == "assignments" && parts[5] == "complete" && request.Method == http.MethodPost:
		handler.serveHostAgentComplete(response, request, parts[2], parts[4])
	case len(parts) == 6 && parts[3] == "assignments" && parts[5] == "fail" && request.Method == http.MethodPost:
		handler.serveHostAgentFail(response, request, parts[2], parts[4])
	default:
		http.NotFound(response, request)
	}
	return true
}

func (handler *controlPlaneHandler) authorizedOperator(request *http.Request) bool {
	provided, bearer := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return bearer && len(provided) == len(handler.token) && subtle.ConstantTimeCompare([]byte(provided), []byte(handler.token)) == 1
}

func (handler *controlPlaneHandler) authorizedHostAgent(request *http.Request, agentID string) bool {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	agent := handler.hostAgents[agentID]
	return requestMatchesHostAgentCredential(request, agent)
}

func requestMatchesHostAgentCredential(request *http.Request, agent *controlPlaneHostAgent) bool {
	if agent == nil {
		return false
	}
	provided, bearer := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	if !bearer || len(provided) == 0 || len(provided) > maximumHostAgentCredentialBytes {
		return false
	}
	digest := sha256.Sum256([]byte(provided))
	want, err := hex.DecodeString(agent.enrollment.CredentialSHA256)
	return err == nil && len(want) == len(digest) && subtle.ConstantTimeCompare(digest[:], want) == 1
}

func decodeHostAgentRequest(request *http.Request, target any) error {
	return decodeBoundedControlPlaneJSON(request.Body, maximumControlPlaneSubmissionBytes, target)
}

func (handler *controlPlaneHandler) serveHostAgentEnrollment(response http.ResponseWriter, request *http.Request) {
	var enrollment hostAgentEnrollmentRequest
	if err := decodeHostAgentRequest(request, &enrollment); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent enrollment")
		return
	}
	if enrollment.Version != hostAgentAPIVersion || validateHostAgentID(enrollment.AgentID) != nil || validateHostAgentHostID(enrollment.HostID) != nil || len(enrollment.Credential) < minimumHostAgentCredentialBytes || len(enrollment.Credential) > maximumHostAgentCredentialBytes {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent enrollment")
		return
	}
	digest := sha256.Sum256([]byte(enrollment.Credential))
	nowTime := handler.runtime.Now()
	now := nowTime.UTC().Format(time.RFC3339Nano)
	record := hostAgentEnrollmentRecord{
		Version: hostAgentAPIVersion, AgentID: enrollment.AgentID, HostID: enrollment.HostID,
		CredentialSHA256: hex.EncodeToString(digest[:]), EnrolledAt: now,
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if existing := handler.hostAgents[enrollment.AgentID]; existing != nil && existing.status.SessionID != "" && !nowTime.Before(existing.sessionExpiresAt) {
		existing.status.SessionID = ""
		existing.sessionExpiresAt = time.Time{}
		if existing.status.RunID == "" {
			existing.status.State = "offline"
		} else {
			existing.status.State = "locked"
		}
		if err := handler.persistHostAgentStatus(existing); err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "persist expired Windows Host Agent session")
			return
		}
	}
	if existing := handler.hostAgents[enrollment.AgentID]; existing != nil && (existing.status.RunID != "" || existing.status.SessionID != "" || existing.status.State == "reserving") {
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent is active")
		return
	}
	for id, existing := range handler.hostAgents {
		if id != enrollment.AgentID && existing.enrollment.HostID == enrollment.HostID {
			writeControlPlaneError(response, http.StatusConflict, "Maya Host is already enrolled")
			return
		}
	}
	agentDir := filepath.Join(handler.dataDir, "host-agents", enrollment.AgentID)
	if err := ensurePrivateControlPlaneDirectory(agentDir); err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent enrollment")
		return
	}
	status := hostAgentStatusResponse{
		Version: hostAgentAPIVersion, Kind: "host-agent-status", AgentID: enrollment.AgentID,
		HostID: enrollment.HostID, Slots: 1, State: "enrolled",
	}
	if err := writePrivateJSON(filepath.Join(agentDir, "enrollment.json"), record); err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent enrollment")
		return
	}
	if err := writePrivateJSON(filepath.Join(agentDir, "status.json"), status); err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent status")
		return
	}
	handler.hostAgents[enrollment.AgentID] = &controlPlaneHostAgent{enrollment: record, status: status, notify: make(chan struct{})}
	response.Header().Set("Content-Type", "application/json")
	response.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(response).Encode(status)
}

func (handler *controlPlaneHandler) serveHostAgentStatus(response http.ResponseWriter, agentID string) {
	handler.mu.Lock()
	agent := handler.hostAgents[agentID]
	if agent == nil {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusNotFound, "Windows Host Agent not found")
		return
	}
	if agent.status.SessionID != "" && !handler.runtime.Now().Before(agent.sessionExpiresAt) {
		assignment := handler.assignments[agent.status.RunID]
		if assignment != nil && assignment.record.State == "confirmed" && !assignment.finishing {
			handler.mu.Unlock()
			if _, _, err := handler.quarantineHostAgentAssignment(assignment, agent, "Windows Host Agent process-session lease expired before Maya shutdown was verified"); err != nil {
				writeControlPlaneError(response, http.StatusInternalServerError, "persist expired Windows Host Agent quarantine")
				return
			}
			handler.mu.Lock()
		} else {
			agent.status.SessionID = ""
			agent.sessionExpiresAt = time.Time{}
			if agent.status.RunID == "" {
				agent.status.State = "offline"
			} else {
				agent.status.State = "locked"
			}
			_ = handler.persistHostAgentStatus(agent)
		}
	}
	status := agent.status
	handler.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(status)
}

func (handler *controlPlaneHandler) serveHostAgentRegistration(response http.ResponseWriter, request *http.Request, agentID string) {
	var registration hostAgentRegistrationRequest
	if err := decodeHostAgentRequest(request, &registration); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent registration")
		return
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if agent == nil || registration.Version != hostAgentAPIVersion || registration.AgentID != agentID || registration.HostID != agent.enrollment.HostID || registration.Slots != 1 || registration.Capabilities.Version != mayaHostCapabilityRecordVersion || !completeTargetProfileHostPoolMapping(registration.Capabilities) {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent registration")
		return
	}
	now := handler.runtime.Now()
	if agent.status.State == "quarantined" {
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent assignment is quarantined")
		return
	}
	if agent.status.RunID != "" {
		if assignment := handler.assignments[agent.status.RunID]; assignment != nil {
			if assignment.record.State == "quarantined" {
				writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent assignment is quarantined")
				return
			}
			if assignment.record.State != "assigned" && assignment.record.State != "confirmed" && !registration.DeadlineActions {
				writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent does not support the outstanding Host Lock deadline action")
				return
			}
			if assignment.finishing {
				writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent completion is still committing")
				return
			}
			sessionExpired := agent.status.SessionID != "" && !now.Before(agent.sessionExpiresAt)
			restartGraceExpired := agent.status.SessionID == "" && !now.Before(agent.takeoverNotBefore)
			if assignment.record.State == "confirmed" && !assignment.finishing && (sessionExpired || restartGraceExpired) {
				writeControlPlaneError(response, http.StatusConflict, "confirmed Windows Host Agent assignment is waiting for its Host Lock idle deadline")
				return
			}
		}
	}
	if agent.status.RunID != "" && agent.status.SessionID == "" && now.Before(agent.takeoverNotBefore) {
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent recovery grace is still active")
		return
	}
	if agent.status.SessionID != "" && now.Before(agent.sessionExpiresAt) {
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent already has an active process")
		return
	}
	sessionID, err := newHostAgentLockToken()
	if err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "create Windows Host Agent session")
		return
	}
	candidate := agent.status
	candidate.SessionID = sessionID
	candidate.SessionBinding = registration.SessionBinding
	candidate.DeadlineActions = registration.DeadlineActions
	candidate.Capabilities = registration.Capabilities
	if candidate.RunID == "" {
		candidate.State = "ready"
	} else {
		candidate.State = "locked"
	}
	statusPath := filepath.Join(handler.dataDir, "host-agents", candidate.AgentID, "status.json")
	if err := writePrivateJSON(statusPath, candidate); err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent registration")
		return
	}
	agent.status = candidate
	agent.sessionExpiresAt = now.Add(hostAgentSessionLease)
	agent.takeoverNotBefore = time.Time{}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(agent.status)
}

func (handler *controlPlaneHandler) serveHostAgentHeartbeat(response http.ResponseWriter, request *http.Request, agentID string) {
	var heartbeat hostAgentHeartbeatRequest
	if err := decodeHostAgentRequest(request, &heartbeat); err != nil || heartbeat.Version != hostAgentAPIVersion || heartbeat.SessionID == "" || heartbeat.Capabilities.Version != mayaHostCapabilityRecordVersion || !completeTargetProfileHostPoolMapping(heartbeat.Capabilities) {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent heartbeat")
		return
	}
	handler.mu.Lock()
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	var checkpointAssignment *controlPlaneHostAgentAssignment
	if agent != nil && agent.status.RunID != "" {
		checkpointAssignment = handler.assignments[agent.status.RunID]
	}
	if checkpointAssignment != nil {
		handler.mu.Unlock()
		checkpointAssignment.checkpointMu.Lock()
		defer checkpointAssignment.checkpointMu.Unlock()
		handler.mu.Lock()
	}
	defer handler.mu.Unlock()
	agent = handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) || !handler.acceptHostAgentSession(agent, heartbeat.SessionID) {
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent session changed")
		return
	}
	if agent.status.RunID != "" {
		assignment := handler.assignments[agent.status.RunID]
		if assignment == nil || assignment != checkpointAssignment || assignment.record.AgentID != agentID {
			writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
			return
		}
		if assignment.record.State == "expiring" || assignment.record.State == "finishing" {
			agent.status.Capabilities = heartbeat.Capabilities
			if err := handler.persistHostAgentStatus(agent); err != nil {
				writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent capabilities")
				return
			}
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(agent.status)
			return
		}
		if assignment.record.expiryReason(handler.runtime.Now()) != "" {
			writeControlPlaneError(response, http.StatusConflict, "Host Lock deadline expired")
			return
		}
		previous := assignment.record.hostLockDeadlines
		if err := assignment.record.recordHeartbeat(handler.runtime.Now(), handler.hostLockPolicy); err != nil {
			writeControlPlaneError(response, http.StatusConflict, "Host Lock deadline expired")
			return
		}
		if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
			assignment.record.hostLockDeadlines = previous
			writeControlPlaneError(response, http.StatusInternalServerError, "persist Host Lock heartbeat")
			return
		}
	}
	agent.status.Capabilities = heartbeat.Capabilities
	if err := handler.persistHostAgentStatus(agent); err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent capabilities")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(agent.status)
}

func (handler *controlPlaneHandler) touchHostAgentSession(agent *controlPlaneHostAgent) {
	agent.sessionExpiresAt = handler.runtime.Now().Add(hostAgentSessionLease)
}

func (handler *controlPlaneHandler) acceptHostAgentSession(agent *controlPlaneHostAgent, sessionID string) bool {
	if agent == nil || !sameLockToken(sessionID, agent.status.SessionID) {
		return false
	}
	if !handler.runtime.Now().Before(agent.sessionExpiresAt) {
		agent.status.SessionID = ""
		agent.sessionExpiresAt = time.Time{}
		if agent.status.RunID == "" {
			agent.status.State = "offline"
		} else {
			agent.status.State = "locked"
		}
		_ = handler.persistHostAgentStatus(agent)
		return false
	}
	handler.touchHostAgentSession(agent)
	return true
}

func (handler *controlPlaneHandler) serveHostAgentNextAssignment(response http.ResponseWriter, request *http.Request, agentID string) {
	var next hostAgentNextRequest
	if err := decodeHostAgentRequest(request, &next); err != nil || next.Version != hostAgentAPIVersion || next.SessionID == "" {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent poll")
		return
	}
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		handler.mu.Lock()
		agent := handler.hostAgents[agentID]
		if !requestMatchesHostAgentCredential(request, agent) {
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
			return
		}
		if agent == nil {
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusNotFound, "Windows Host Agent not found")
			return
		}
		if !handler.acceptHostAgentSession(agent, next.SessionID) {
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent session changed")
			return
		}
		if agent.status.RunID != "" {
			assignment := handler.assignments[agent.status.RunID]
			if assignment != nil && (assignment.record.State == "assigned" || assignment.record.State == "confirmed" || assignment.record.State == "kept" || assignment.record.State == "expiring" || assignment.record.State == "finishing") {
				if assignment.record.State != "assigned" && assignment.record.State != "confirmed" && !agent.status.DeadlineActions {
					handler.mu.Unlock()
					writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent does not support Host Lock deadline actions")
					return
				}
				result := assignmentResponse(assignment.record)
				handler.mu.Unlock()
				response.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(response).Encode(result)
				return
			}
		}
		notify := agent.notify
		handler.mu.Unlock()
		select {
		case <-notify:
			continue
		case <-timer.C:
			response.WriteHeader(http.StatusNoContent)
			return
		case <-request.Context().Done():
			handler.disconnectHostAgentSession(agentID, next.SessionID)
			return
		}
	}
}

func (handler *controlPlaneHandler) disconnectHostAgentSession(agentID string, sessionID string) {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	agent := handler.hostAgents[agentID]
	if agent != nil && agent.status.RunID == "" && sameLockToken(sessionID, agent.status.SessionID) {
		agent.status.State = "offline"
		agent.status.SessionID = ""
		agent.sessionExpiresAt = time.Time{}
		_ = handler.persistHostAgentStatus(agent)
	}
}

func assignmentResponse(record hostAgentAssignmentRecord) hostAgentAssignmentResponse {
	action := "execute"
	switch record.State {
	case "kept":
		action = "wait"
	case "expiring":
		action = "cleanup"
	case "finishing":
		action = "finish"
	}
	return hostAgentAssignmentResponse{
		Version: hostAgentAPIVersion, Kind: "host-agent-assignment", RunID: record.RunID,
		AgentID: record.AgentID, HostID: record.HostID, LockToken: record.LockToken, Submission: record.Submission, EventPrefix: record.EventPrefix, Capabilities: record.Capabilities, SelectedMayaBuild: record.SelectedMayaBuild,
		Action: action, HardDeadline: record.HardDeadline, KeepDeadline: record.KeepDeadline, BrokerSession: record.BrokerSession, ExpiryFromState: record.ExpiryFromState, ExpiryReason: record.ExpiryReason,
		DeadlineEvents: append([]hostLockDeadlineEvent(nil), record.DeadlineEvents...),
	}
}

func (handler *controlPlaneHandler) serveHostAgentConfirm(response http.ResponseWriter, request *http.Request, agentID string, runID string) {
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Agent Run ID")
		return
	}
	var confirmation hostAgentLockRequest
	if err := decodeHostAgentRequest(request, &confirmation); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Lock confirmation")
		return
	}
	handler.mu.Lock()
	checkpointAssignment := handler.assignments[runID]
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if checkpointAssignment == nil {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	handler.mu.Unlock()
	checkpointAssignment.checkpointMu.Lock()
	defer checkpointAssignment.checkpointMu.Unlock()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	assignment := handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if confirmation.Version != hostAgentAPIVersion || confirmation.RunID != runID || assignment == nil || assignment.record.AgentID != agentID || agent == nil || !sameLockToken(confirmation.LockToken, assignment.record.LockToken) || assignment.record.State != "assigned" && assignment.record.State != "confirmed" || !handler.acceptHostAgentSession(agent, confirmation.SessionID) {
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	previousState := assignment.record.State
	assignment.record.State = "confirmed"
	agent.status.State = "running"
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
		assignment.record.State = previousState
		agent.status.State = "locked"
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Host Lock confirmation")
		return
	}
	_ = handler.persistHostAgentStatus(agent)
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(agent.status)
}

func (handler *controlPlaneHandler) serveHostAgentSession(response http.ResponseWriter, request *http.Request, agentID string, runID string) {
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Agent Run ID")
		return
	}
	var binding hostAgentSessionRequest
	if err := decodeHostAgentRequest(request, &binding); err != nil || binding.BrokerSession.BrokerAdapter == "" || binding.BrokerSession.SessionID == "" {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Maya UI Session binding")
		return
	}
	handler.mu.Lock()
	checkpointAssignment := handler.assignments[runID]
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if checkpointAssignment == nil {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	handler.mu.Unlock()
	checkpointAssignment.checkpointMu.Lock()
	defer checkpointAssignment.checkpointMu.Unlock()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	assignment := handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if binding.Version != hostAgentAPIVersion || binding.RunID != runID || assignment == nil || assignment.record.AgentID != agentID || agent == nil || !assignment.record.SessionBindingRequired || !sameLockToken(binding.LockToken, assignment.record.LockToken) || assignment.record.State != "confirmed" || !handler.acceptHostAgentSession(agent, binding.SessionID) {
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	if assignment.record.BrokerSession != nil {
		if *assignment.record.BrokerSession != binding.BrokerSession {
			writeControlPlaneError(response, http.StatusConflict, "Host Lock Maya UI Session changed")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(agent.status)
		return
	}
	previous := assignment.record
	bound := binding.BrokerSession
	assignment.record.BrokerSession = &bound
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
		assignment.record = previous
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Maya UI Session binding")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(agent.status)
}

func (handler *controlPlaneHandler) serveHostAgentProgress(response http.ResponseWriter, request *http.Request, agentID string, runID string) {
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Agent Run ID")
		return
	}
	var progress hostAgentProgressRequest
	if err := decodeHostAgentRequest(request, &progress); err != nil || len(progress.Events) > maximumHostAgentProgressEventBytes || len(progress.Log) > maximumHostAgentProgressLogBytes || runLedgerEventLineCount(progress.Events) > maximumHostAgentProgressEvents {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent progress")
		return
	}
	handler.mu.Lock()
	checkpointAssignment := handler.assignments[runID]
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if checkpointAssignment == nil {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	handler.mu.Unlock()

	// Serialize checkpoints for one assignment without blocking unrelated Control Plane reads.
	checkpointAssignment.checkpointMu.Lock()
	defer checkpointAssignment.checkpointMu.Unlock()
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	deadlineExpired := assignment != nil && assignment.record.State == "expiring" && assignment.record.ExpiryFromState == "confirmed" &&
		assignment.record.AgentID == agentID && sameLockToken(progress.LockToken, assignment.record.LockToken) && handler.acceptHostAgentSession(agent, progress.SessionID)
	if progress.Version != hostAgentAPIVersion || progress.RunID != runID || progress.Checkpoint < 1 || !hostAgentAssignmentAcceptsProgress(assignment) || assignment.record.AgentID != agentID || agent == nil || !sameLockToken(progress.LockToken, assignment.record.LockToken) || !handler.acceptHostAgentSession(agent, progress.SessionID) {
		handler.mu.Unlock()
		if deadlineExpired {
			writeControlPlaneError(response, http.StatusConflict, "Host Lock deadline expired")
			return
		}
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	expectedProfile := assignment.record.Submission.TargetProfile
	if expectedProfile == "" {
		expectedProfile = "default"
	}
	if progress.Ledger.RunID != runID || progress.Ledger.Scenario != assignment.record.Submission.Scenario || progress.Ledger.TargetProfile != expectedProfile || progress.Ledger.Host != assignment.record.HostID || progress.Ledger.State != "submitted" {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent progress changed run identity")
		return
	}
	fingerprint := hostAgentProgressFingerprint(progress)
	digest := hex.EncodeToString(fingerprint[:])
	if progress.Checkpoint == assignment.record.ProgressSequence {
		if digest != assignment.record.ProgressDigest {
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent checkpoint identity changed")
			return
		}
		status := agent.status
		handler.mu.Unlock()
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(status)
		return
	}
	if progress.Checkpoint != assignment.record.ProgressSequence+1 {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent checkpoint is stale or skipped")
		return
	}
	previousAssignment := assignment.record
	repoDir := assignment.repoDir
	transitionEnrollment := agent.enrollment
	transitionStatus := agent.status
	handler.mu.Unlock()

	updatedAssignment := previousAssignment
	if err := updatedAssignment.recordHeartbeat(handler.runtime.Now(), handler.hostLockPolicy); err != nil {
		writeControlPlaneError(response, http.StatusConflict, "Host Lock deadline expired")
		return
	}
	var identityErr error
	ledgerStore := newRunLedgerStore(repoDir)
	persistErr := ledgerStore.UpdateSnapshot(runID, func(snapshot *runLedgerSnapshot) error {
		mergedEvents, err := mergeHostAgentProgressEvents(snapshot.Events, progress.Events)
		if err != nil {
			identityErr = err
			return err
		}
		eventCount, eventsOmitted, eventsTruncated, err := retainedRunLedgerEventMetadata(bytes.NewReader(mergedEvents))
		if err != nil {
			return err
		}
		snapshot.Events = mergedEvents
		snapshot.Log = progress.Log
		snapshot.Record.EventCount = eventCount
		snapshot.Record.EventsOmitted = eventsOmitted
		snapshot.Record.EventsTruncated = eventsTruncated
		snapshot.Record.EventBytes = len(mergedEvents)
		snapshot.Record.LogBytes = len(progress.Log)
		snapshot.Record.LogTruncated = progress.Ledger.LogTruncated
		snapshot.Record.Host = previousAssignment.HostID
		snapshot.Record.UpdatedAt = handler.runtime.Now().UTC().Format(time.RFC3339Nano)
		copy := snapshot.Record
		updatedAssignment.AssignedLedger = &copy
		updatedAssignment.ProgressSequence = progress.Checkpoint
		updatedAssignment.ProgressDigest = digest
		return nil
	})
	if persistErr == nil {
		prepareTransition := func(record hostAgentAssignmentRecord, entryName string) (hostAgentAssignmentRecord, error) {
			return prepareRecoveredHostAgentTransitionSnapshot(handler.dataDir, transitionEnrollment, transitionStatus, record, entryName)
		}
		persistErr = newHostAgentTransitionStore(handler.dataDir).Commit(updatedAssignment, prepareTransition)
	}
	// The per-run lock is released before taking handler.mu; quarantine may wait
	// on the ledger while fenced by handler.mu without creating a lock cycle.
	if identityErr != nil {
		writeControlPlaneError(response, http.StatusConflict, "Windows Host Agent progress changed event identity")
		return
	}
	if persistErr != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist active Run Ledger checkpoint")
		return
	}
	handler.mu.Lock()
	assignment = handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if assignment != checkpointAssignment || !hostAgentAssignmentAcceptsProgress(assignment) || assignment.record.ProgressSequence != previousAssignment.ProgressSequence || !sameBrokerSession(assignment.record.BrokerSession, previousAssignment.BrokerSession) || agent == nil || !requestMatchesHostAgentCredential(request, agent) || !sameLockToken(progress.LockToken, assignment.record.LockToken) || !sameLockToken(progress.SessionID, agent.status.SessionID) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	assignment.record = updatedAssignment
	status := agent.status
	handler.mu.Unlock()
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(status)
}

func hostAgentAssignmentAcceptsProgress(assignment *controlPlaneHostAgentAssignment) bool {
	return assignment != nil && assignment.record.State == "confirmed" && !assignment.finishing
}

func mergeHostAgentKeptProgress(repoDir string, assignment hostAgentAssignmentRecord, progress hostAgentProgressRequest, now time.Time) (runLedgerRecord, error) {
	expectedProfile := assignment.Submission.TargetProfile
	if expectedProfile == "" {
		expectedProfile = "default"
	}
	if progress.Version != hostAgentAPIVersion || progress.RunID != assignment.RunID || !sameLockToken(progress.LockToken, assignment.LockToken) ||
		progress.Ledger.RunID != assignment.RunID || progress.Ledger.Scenario != assignment.Submission.Scenario ||
		progress.Ledger.TargetProfile != expectedProfile || progress.Ledger.Host != assignment.HostID || progress.Ledger.State != "submitted" {
		return runLedgerRecord{}, fmt.Errorf("Kept Session progress changed run identity") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	var mergedRecord runLedgerRecord
	err := newRunLedgerStore(repoDir).UpdateSnapshot(assignment.RunID, func(snapshot *runLedgerSnapshot) error {
		mergedEvents, err := mergeHostAgentProgressEvents(snapshot.Events, progress.Events)
		if err != nil {
			return err
		}
		eventCount, eventsOmitted, eventsTruncated, err := retainedRunLedgerEventMetadata(bytes.NewReader(mergedEvents))
		if err != nil {
			return err
		}
		snapshot.Events = mergedEvents
		snapshot.Log = progress.Log
		snapshot.Record.EventCount = eventCount
		snapshot.Record.EventsOmitted = eventsOmitted
		snapshot.Record.EventsTruncated = eventsTruncated
		snapshot.Record.EventBytes = len(mergedEvents)
		snapshot.Record.LogBytes = len(progress.Log)
		snapshot.Record.LogTruncated = progress.Ledger.LogTruncated
		snapshot.Record.Host = assignment.HostID
		snapshot.Record.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		mergedRecord = snapshot.Record
		return nil
	})
	return mergedRecord, err
}

func (handler *controlPlaneHandler) serveHostAgentKept(response http.ResponseWriter, request *http.Request, agentID string, runID string) {
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Agent Run ID")
		return
	}
	var kept hostAgentKeptRequest
	if err := decodeHostAgentRequest(request, &kept); err != nil || len(kept.Progress.Events) > maximumHostAgentProgressEventBytes || len(kept.Progress.Log) > maximumHostAgentProgressLogBytes || runLedgerEventLineCount(kept.Progress.Events) > maximumHostAgentProgressEvents {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Kept Session transition")
		return
	}
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) || assignment == nil {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	handler.mu.Unlock()
	assignment.checkpointMu.Lock()
	defer assignment.checkpointMu.Unlock()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	assignment = handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if kept.Version != hostAgentAPIVersion || kept.RunID != runID || kept.Progress.RunID != runID || !sameLockToken(kept.Progress.LockToken, kept.LockToken) || !sameLockToken(kept.Progress.SessionID, kept.SessionID) || assignment == nil || assignment.record.AgentID != agentID || agent == nil || !requestMatchesHostAgentCredential(request, agent) || !sameLockToken(kept.LockToken, assignment.record.LockToken) || assignment.finishing || !handler.acceptHostAgentSession(agent, kept.SessionID) {
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	if err := validateHostAgentKeptIdentity(assignment.record, kept.Outcome); err != nil || assignment.record.BrokerSession == nil || assignment.record.AssignedLedger == nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Kept Session transition")
		return
	}
	if assignment.record.State == "kept" || assignment.record.State == "expiring" {
		action := "wait"
		if assignment.record.State == "expiring" {
			action = "cleanup"
		}
		agent.status.State = assignment.record.State
		if err := handler.persistHostAgentStatus(agent); err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "persist Kept Session Agent status")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(hostAgentActionResponse(assignment.record, handler.runtime.Now(), action, assignment.record.ExpiryReason))
		return
	}
	if assignment.record.State != "confirmed" {
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	keptLedger, err := mergeHostAgentKeptProgress(assignment.repoDir, assignment.record, kept.Progress, handler.runtime.Now())
	if err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Kept Session progress")
		return
	}
	previous := assignment.record
	assignment.record.State = "kept"
	keptLedger.State = "kept"
	keptLedger.Status = kept.Outcome.Status
	keptLedger.UpdatedAt = handler.runtime.Now().UTC().Format(time.RFC3339Nano)
	assignment.record.AssignedLedger = &keptLedger
	if err := assignment.record.markKept(handler.runtime.Now(), keepTTLOrDefault(assignment.record.Submission.KeepTTL), handler.hostLockPolicy); err != nil {
		assignment.record = previous
		writeControlPlaneError(response, http.StatusConflict, "Host Lock deadline expired")
		return
	}
	handler.appendHostLockEventLocked(assignment, "kept-session.started", assignment.record.KeepDeadline)
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
		assignment.record = previous
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Kept Session Host Lock")
		return
	}
	agent.status.State = "kept"
	if err := handler.persistHostAgentStatus(agent); err != nil {
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Kept Session Agent status")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(hostAgentActionResponse(assignment.record, handler.runtime.Now(), "wait", ""))
}

func validateHostAgentKeptIdentity(assignment hostAgentAssignmentRecord, outcome runCommandJSON) error {
	targetProfile := assignment.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	if outcome.Version != controlPlaneAPIVersion || outcome.Kind != "run" || !outcome.Accepted || outcome.RunID != assignment.RunID || outcome.Scenario != assignment.Submission.Scenario || outcome.TargetProfile != targetProfile || outcome.Host != assignment.HostID || outcome.StopPolicy != "kept" {
		return fmt.Errorf("kept result does not match assignment")
	}
	return nil
}

func (handler *controlPlaneHandler) serveHostAgentAction(response http.ResponseWriter, request *http.Request, agentID string, runID string) {
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Agent Run ID")
		return
	}
	var action hostAgentLockRequest
	if err := decodeHostAgentRequest(request, &action); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Lock action poll")
		return
	}
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) || assignment == nil {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	handler.mu.Unlock()
	assignment.checkpointMu.Lock()
	defer assignment.checkpointMu.Unlock()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	assignment = handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if action.Version != hostAgentAPIVersion || action.RunID != runID || assignment == nil || assignment.record.AgentID != agentID || agent == nil || !requestMatchesHostAgentCredential(request, agent) || !sameLockToken(action.LockToken, assignment.record.LockToken) || !handler.acceptHostAgentSession(agent, action.SessionID) {
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	expiryReason := assignment.record.ExpiryReason
	if assignment.record.State == "kept" {
		expiryReason = assignment.record.expiryReason(handler.runtime.Now())
	}
	if assignment.record.State == "kept" && expiryReason != "" {
		previous := assignment.record
		assignment.record.ExpiryFromState = assignment.record.State
		assignment.record.ExpiryReason = expiryReason
		assignment.record.State = "expiring"
		handler.appendHostLockEventLocked(assignment, "host-lock.expired", expiryReason)
		if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
			assignment.record = previous
			writeControlPlaneError(response, http.StatusInternalServerError, "persist Host Lock expiry")
			return
		}
		agent.status.State = "cleaning"
		if err := handler.persistHostAgentStatus(agent); err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "persist Host Lock expiry Agent status")
			return
		}
	}
	if assignment.record.State != "kept" && assignment.record.State != "expiring" {
		writeControlPlaneError(response, http.StatusConflict, "Host Lock has no Kept Session action")
		return
	}
	nextAction := "wait"
	if assignment.record.State == "expiring" {
		nextAction = "cleanup"
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(hostAgentActionResponse(assignment.record, handler.runtime.Now(), nextAction, expiryReason))
}

func hostAgentActionResponse(record hostAgentAssignmentRecord, now time.Time, action string, expiryReason string) hostAgentKeptResponse {
	return hostAgentKeptResponse{
		Version: hostAgentAPIVersion, Kind: "host-agent-kept-action", RunID: record.RunID, State: record.State,
		Action: action, IdleDeadline: record.IdleDeadline, HardDeadline: record.HardDeadline,
		KeepDeadline: record.KeepDeadline, KeepRemaining: hostLockDeadlineRemaining(record.KeepDeadline, now),
		ExpiryFromState: record.ExpiryFromState, ExpiryReason: expiryReason,
		BrokerSession: record.BrokerSession, DeadlineEvents: append([]hostLockDeadlineEvent(nil), record.DeadlineEvents...),
	}
}

func (handler *controlPlaneHandler) appendHostLockEventLocked(assignment *controlPlaneHostAgentAssignment, event string, detail string) {
	assignment.record.DeadlineEvents = append(assignment.record.DeadlineEvents, hostLockDeadlineEvent{
		Type: event, Detail: detail, Timestamp: handler.runtime.Now().UTC().Format(time.RFC3339Nano),
		Ordinal: len(assignment.record.DeadlineEvents) + 1,
	})
}

func (handler *controlPlaneHandler) observeHostLockDeadlines() error {
	type expiryCandidate struct {
		assignment *controlPlaneHostAgentAssignment
		runID      string
	}
	handler.mu.Lock()
	candidates := make([]expiryCandidate, 0)
	now := handler.runtime.Now()
	for _, assignment := range handler.assignments {
		if assignment != nil && !assignment.finishing && (assignment.record.State == "assigned" || assignment.record.State == "confirmed" || assignment.record.State == "kept") && assignment.record.expiryReason(now) != "" {
			candidates = append(candidates, expiryCandidate{assignment: assignment, runID: assignment.record.RunID})
		}
	}
	handler.mu.Unlock()
	var observeErr error
	for _, candidate := range candidates {
		candidate.assignment.checkpointMu.Lock()
		handler.mu.Lock()
		assignment := handler.assignments[candidate.runID]
		now = handler.runtime.Now()
		if assignment == candidate.assignment && !assignment.finishing && (assignment.record.State == "assigned" || assignment.record.State == "confirmed" || assignment.record.State == "kept") {
			reason := assignment.record.expiryReason(now)
			if reason != "" {
				previous := assignment.record
				assignment.record.ExpiryFromState = assignment.record.State
				assignment.record.ExpiryReason = reason
				assignment.record.State = "expiring"
				handler.appendHostLockEventLocked(assignment, "host-lock.expired", reason)
				if err := newHostAgentTransitionStore(handler.dataDir).Commit(assignment.record, handler.prepareRecoveredHostAgentTransition); err != nil {
					assignment.record = previous
					observeErr = errors.Join(observeErr, err)
				} else {
					if agent := handler.hostAgents[assignment.record.AgentID]; agent != nil {
						agent.status.State = "cleaning"
						_ = handler.persistHostAgentStatus(agent)
						handler.signalHostAgent(agent)
					}
				}
			}
		}
		handler.mu.Unlock()
		candidate.assignment.checkpointMu.Unlock()
	}
	return observeErr
}

func runLedgerEventLineCount(content []byte) int {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 {
		return 0
	}
	return bytes.Count(trimmed, []byte{'\n'}) + 1
}

func mergeHostAgentProgressEvents(current []byte, incoming []byte) ([]byte, error) {
	currentLines := bytes.Split(bytes.TrimSpace(current), []byte{'\n'})
	incomingLines := bytes.Split(bytes.TrimSpace(incoming), []byte{'\n'})
	if len(currentLines) == 0 || len(incomingLines) == 0 {
		return nil, fmt.Errorf("active event stream is empty")
	}
	type parsedEvent struct {
		sequence  int
		eventType string
		encoded   []byte
	}
	parse := func(line []byte) (parsedEvent, error) {
		var identity struct {
			Sequence json.Number `json:"sequence"`
			Type     string      `json:"type"`
		}
		if err := json.Unmarshal(line, &identity); err != nil {
			return parsedEvent{}, err
		}
		sequence64, err := identity.Sequence.Int64()
		if err != nil || sequence64 < 1 || int64(int(sequence64)) != sequence64 || identity.Type == "" {
			return parsedEvent{}, fmt.Errorf("invalid event identity")
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return parsedEvent{}, err
		}
		encoded, err := json.Marshal(event)
		return parsedEvent{sequence: int(sequence64), eventType: identity.Type, encoded: encoded}, err
	}
	parseOrdered := func(label string, lines [][]byte) ([]parsedEvent, error) {
		parsed := make([]parsedEvent, 0, len(lines))
		expectedSequence := 1
		gapSeen := false
		for _, line := range lines {
			event, err := parse(line)
			if err != nil || event.sequence != expectedSequence {
				return nil, fmt.Errorf("invalid %s event order", label)
			}
			parsed = append(parsed, event)
			if event.eventType == "run-ledger.events.truncated" {
				gapLast, ok := validHostAgentEventGap(event.encoded, event.sequence)
				if gapSeen || !ok {
					return nil, fmt.Errorf("invalid %s event gap", label)
				}
				gapSeen = true
				expectedSequence = gapLast + 1
			} else {
				expectedSequence++
			}
		}
		if parsed[0].sequence != 1 {
			return nil, fmt.Errorf("invalid %s first event", label)
		}
		return parsed, nil
	}
	currentEvents, err := parseOrdered("current", currentLines)
	if err != nil {
		return nil, err
	}
	incomingEvents, err := parseOrdered("incoming", incomingLines)
	if err != nil {
		return nil, err
	}
	lastActualSequence := func(events []parsedEvent) int {
		last := 0
		for _, event := range events {
			if event.eventType != "run-ledger.events.truncated" {
				last = event.sequence
			}
		}
		return last
	}
	if lastActualSequence(incomingEvents) < lastActualSequence(currentEvents) {
		return nil, fmt.Errorf("incoming event snapshot is stale")
	}
	gapRange := func(events []parsedEvent) (int, int) {
		for _, event := range events {
			if event.eventType == "run-ledger.events.truncated" {
				if omitted, ok := runLedgerOmittedEventCount(event.encoded); ok && omitted > 0 {
					return event.sequence, event.sequence + omitted - 1
				}
			}
		}
		return 0, 0
	}
	currentGapFirst, currentGapLast := gapRange(currentEvents)
	incomingGapFirst, incomingGapLast := gapRange(incomingEvents)
	incomingActual := make(map[int]struct{}, len(incomingEvents))
	for _, event := range incomingEvents {
		if event.eventType != "run-ledger.events.truncated" {
			incomingActual[event.sequence] = struct{}{}
			if event.sequence >= currentGapFirst && event.sequence <= currentGapLast && currentGapFirst > 0 {
				return nil, fmt.Errorf("incoming snapshot restored an unverifiable event sequence")
			}
		}
	}
	for _, event := range currentEvents {
		if event.sequence == 1 || event.eventType == "run-ledger.events.truncated" {
			continue
		}
		insideIncomingGap := incomingGapFirst > 0 && event.sequence >= incomingGapFirst && event.sequence <= incomingGapLast
		if _, ok := incomingActual[event.sequence]; !ok && !insideIncomingGap {
			return nil, fmt.Errorf("incoming snapshot dropped an acknowledged event sequence")
		}
	}
	existing := make(map[int][]byte)
	for _, event := range currentEvents {
		if event.eventType != "run-ledger.events.truncated" {
			existing[event.sequence] = event.encoded
		}
	}
	merged := make([][]byte, 0, len(incomingLines))
	merged = append(merged, currentLines[0])
	for _, event := range incomingEvents {
		if event.sequence == 1 {
			continue
		}
		if prior, ok := existing[event.sequence]; ok && event.eventType != "run-ledger.events.truncated" && !bytes.Equal(prior, event.encoded) {
			return nil, fmt.Errorf("event sequence %d changed", event.sequence)
		}
		merged = append(merged, event.encoded)
	}
	return append(bytes.Join(merged, []byte{'\n'}), '\n'), nil
}

func validHostAgentEventGap(encoded []byte, sequence int) (int, bool) {
	omitted, ok := runLedgerOmittedEventCount(encoded)
	if !ok || omitted < 1 {
		return 0, false
	}
	var event map[string]any
	if err := json.Unmarshal(encoded, &event); err != nil {
		return 0, false
	}
	details, ok := event["details"].(map[string]any)
	if !ok {
		return 0, false
	}
	first, firstOK := numberValue(details["firstOmittedSequence"])
	last, lastOK := numberValue(details["lastOmittedSequence"])
	wantLast := sequence + omitted - 1
	if !firstOK || !lastOK || first != float64(sequence) || last != float64(wantLast) {
		return 0, false
	}
	return wantLast, true
}

func mergeHostAgentTerminalEvents(current []byte, incoming []byte) ([]byte, error) {
	currentLines := bytes.Split(bytes.TrimSpace(current), []byte{'\n'})
	incomingLines := bytes.Split(bytes.TrimSpace(incoming), []byte{'\n'})
	if len(currentLines) == 0 || len(incomingLines) == 0 {
		return nil, fmt.Errorf("terminal event stream is empty")
	}
	if merged, authoritySuffix, err := mergeHostAgentTerminalEventsAfterAuthoritySuffix(currentLines, incoming); authoritySuffix {
		return merged, err
	}
	gap := func(label string, lines [][]byte) (int, []byte, error) {
		last := 0
		var marker []byte
		for _, line := range lines {
			var identity struct {
				Sequence json.Number `json:"sequence"`
				Type     string      `json:"type"`
			}
			if err := json.Unmarshal(line, &identity); err != nil {
				return 0, nil, fmt.Errorf("invalid %s event identity", label)
			}
			if identity.Type != "run-ledger.events.truncated" {
				continue
			}
			if marker != nil {
				return 0, nil, fmt.Errorf("multiple %s event gaps", label)
			}
			sequence, err := identity.Sequence.Int64()
			gapLast, ok := validHostAgentEventGap(line, int(sequence))
			if err != nil || sequence != 2 || !ok {
				return 0, nil, fmt.Errorf("invalid %s event gap", label)
			}
			last = gapLast
			marker = line
		}
		return last, marker, nil
	}
	currentGapLast, currentMarker, err := gap("current", currentLines)
	if err != nil {
		return nil, err
	}
	incomingGapLast, incomingMarker, err := gap("incoming", incomingLines)
	if err != nil {
		return nil, err
	}
	gapLast := currentGapLast
	marker := currentMarker
	if incomingGapLast > gapLast {
		gapLast = incomingGapLast
		marker = incomingMarker
	}
	if gapLast == 0 {
		return mergeHostAgentProgressEvents(current, incoming)
	}

	// Once live clients receive a gap marker, terminal history must never restore
	// those unverifiable identities. Keep the widest gap and the configured tail.
	sanitized := make([][]byte, 0, len(incomingLines)+1)
	sanitized = append(sanitized, incomingLines[0], marker)
	for _, line := range incomingLines[1:] {
		var identity struct {
			Sequence json.Number `json:"sequence"`
			Type     string      `json:"type"`
		}
		if err := json.Unmarshal(line, &identity); err != nil {
			return nil, fmt.Errorf("invalid incoming event identity")
		}
		sequence, err := identity.Sequence.Int64()
		if err != nil {
			return nil, fmt.Errorf("invalid incoming event identity")
		}
		if identity.Type == "run-ledger.events.truncated" || sequence >= 2 && sequence <= int64(gapLast) {
			continue
		}
		sanitized = append(sanitized, line)
	}
	return mergeHostAgentProgressEvents(current, append(bytes.Join(sanitized, []byte{'\n'}), '\n'))
}

func mergeHostAgentTerminalEventsAfterAuthoritySuffix(currentLines [][]byte, incoming []byte) ([]byte, bool, error) {
	var baseLines [][]byte
	var authorityLines [][]byte
	var stagedExecutionLines [][]byte
	authoritySeen := false
	stagedExecutionSeen := false
	for _, line := range currentLines {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, false, fmt.Errorf("invalid current event identity")
		}
		id, authority := event["deadlineEventId"].(string)
		if authority && id != "" {
			if stagedExecutionSeen {
				return nil, true, fmt.Errorf("Host Lock authority event follows staged execution") //nolint:staticcheck // Product term starts the user-facing diagnostic.
			}
			authoritySeen = true
			authorityLines = append(authorityLines, line)
			continue
		}
		if authoritySeen {
			stagedExecutionSeen = true
			stagedExecutionLines = append(stagedExecutionLines, line)
		} else {
			baseLines = append(baseLines, line)
		}
	}
	if !authoritySeen {
		return nil, false, nil
	}
	if len(baseLines) == 0 {
		return nil, true, fmt.Errorf("Host Lock authority events have no acknowledged execution prefix") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	mergedExecution, err := mergeHostAgentProgressEvents(append(bytes.Join(baseLines, []byte{'\n'}), '\n'), incoming)
	if err != nil {
		return nil, true, err
	}
	lastBaseSequence := 0
	for _, line := range baseLines {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, true, fmt.Errorf("invalid acknowledged execution event")
		}
		if sequence := ledgerEventSequence(event); sequence > lastBaseSequence {
			lastBaseSequence = sequence
		}
	}
	result := append([][]byte(nil), baseLines...)
	nextSequence := lastBaseSequence + 1
	for _, line := range authorityLines {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, true, fmt.Errorf("invalid Host Lock authority event")
		}
		if ledgerEventSequence(event) != nextSequence {
			return nil, true, fmt.Errorf("Host Lock authority event sequence changed") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		nextSequence++
		result = append(result, line)
	}
	mergedExecutionLines := make([][]byte, 0)
	for _, line := range bytes.Split(bytes.TrimSpace(mergedExecution), []byte{'\n'}) {
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return nil, true, fmt.Errorf("invalid incoming terminal event")
		}
		if ledgerEventSequence(event) <= lastBaseSequence {
			continue
		}
		if id, authority := event["deadlineEventId"].(string); authority && id != "" {
			continue
		}
		event["sequence"] = nextSequence
		nextSequence++
		encoded, err := json.Marshal(event)
		if err != nil {
			return nil, true, err
		}
		mergedExecutionLines = append(mergedExecutionLines, encoded)
	}
	if len(stagedExecutionLines) > len(mergedExecutionLines) {
		return nil, true, fmt.Errorf("incoming terminal events are older than the staged execution tail")
	}
	for index, staged := range stagedExecutionLines {
		same, err := sameHostAgentEventIgnoringSequence(staged, mergedExecutionLines[index])
		if err != nil {
			return nil, true, err
		}
		if !same {
			return nil, true, fmt.Errorf("staged terminal event %d changed", index+1)
		}
	}
	result = append(result, mergedExecutionLines...)
	normalized := append(bytes.Join(result, []byte{'\n'}), '\n')
	current := append(bytes.Join(currentLines, []byte{'\n'}), '\n')
	if bytes.Equal(normalized, current) {
		return current, true, nil
	}
	return normalized, true, nil
}

func sameHostAgentEventIgnoringSequence(left []byte, right []byte) (bool, error) {
	var leftEvent map[string]any
	if err := json.Unmarshal(left, &leftEvent); err != nil {
		return false, fmt.Errorf("invalid staged terminal event")
	}
	var rightEvent map[string]any
	if err := json.Unmarshal(right, &rightEvent); err != nil {
		return false, fmt.Errorf("invalid merged terminal event")
	}
	delete(leftEvent, "sequence")
	delete(rightEvent, "sequence")
	return reflect.DeepEqual(leftEvent, rightEvent), nil
}

func (handler *controlPlaneHandler) serveHostAgentComplete(response http.ResponseWriter, request *http.Request, agentID string, runID string) {
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Agent Run ID")
		return
	}
	var completion hostAgentCompletionRequest
	if err := decodeHostAgentRequest(request, &completion); err != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent completion")
		return
	}
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if assignment == nil {
		handler.mu.Unlock()
		var completed hostAgentAssignmentRecord
		err := readPrivateJSON(filepath.Join(handler.dataDir, "assignments", runID+".json"), &completed)
		if err == nil && completed.State == "completed" && completed.AgentID == agentID && sameLockToken(completion.LockToken, completed.LockToken) && completed.Terminal != nil {
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(completed.Terminal)
			return
		}
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	if assignment.record.State == "finishing" {
		if completion.Version != hostAgentAPIVersion || completion.RunID != runID || assignment.record.AgentID != agentID || agent == nil || !sameLockToken(completion.LockToken, assignment.record.LockToken) || !handler.acceptHostAgentSession(agent, completion.SessionID) {
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
			return
		}
		terminal, err := handler.resumeFinishingHostAgentAssignment(assignment, agent)
		handler.mu.Unlock()
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "resume Windows Host Agent completion")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(terminal)
		return
	}
	if completion.Version != hostAgentAPIVersion || completion.RunID != runID || assignment == nil || assignment.record.AgentID != agentID || agent == nil || !sameLockToken(completion.LockToken, assignment.record.LockToken) || !hostAgentAssignmentAcceptsCompletion(assignment.record, completion.Terminal) || assignment.finishing || !handler.acceptHostAgentSession(agent, completion.SessionID) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	handler.mu.Unlock()
	assignment.checkpointMu.Lock()
	defer assignment.checkpointMu.Unlock()
	handler.mu.Lock()
	assignment = handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if completion.Version != hostAgentAPIVersion || completion.RunID != runID || assignment == nil || assignment.record.AgentID != agentID || agent == nil || !requestMatchesHostAgentCredential(request, agent) || !sameLockToken(completion.LockToken, assignment.record.LockToken) || !hostAgentAssignmentAcceptsCompletion(assignment.record, completion.Terminal) || assignment.finishing || !handler.acceptHostAgentSession(agent, completion.SessionID) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	assignment.finishing = true
	handler.mu.Unlock()

	outcome, originalLedger, terminalLedger, err := handler.acceptHostAgentCompletion(assignment, completion)
	if err != nil {
		handler.mu.Lock()
		if current := handler.assignments[runID]; current == assignment {
			assignment.finishing = false
		}
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent completion")
		return
	}
	if assignment.record.State == "expiring" && assignment.record.ExpiryFromState == "confirmed" && completion.Terminal.Status == resultStatusPassed {
		outcome, terminalLedger, err = handler.convertDeadlineRacedHostAgentCompletion(assignment, terminalLedger)
		if err != nil {
			handler.mu.Lock()
			if current := handler.assignments[runID]; current == assignment {
				assignment.finishing = false
			}
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusInternalServerError, "convert deadline-raced Windows Host Agent completion")
			return
		}
	}
	assignment.originalLedger = &originalLedger
	assignment.terminalLedger = &terminalLedger
	if err := handler.finishHostAgentAssignment(assignment, completion.SessionID, outcome, nil); err != nil {
		_ = handler.resetHostAgentFinishing(assignment)
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent completion")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(controlPlaneTerminalResponse(outcome, nil, assignment.repoDir))
}

func (handler *controlPlaneHandler) serveHostAgentFail(response http.ResponseWriter, request *http.Request, agentID string, runID string) {
	if validateRunID(runID) != nil {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Host Agent Run ID")
		return
	}
	var failure hostAgentFailureRequest
	if err := decodeHostAgentRequest(request, &failure); err != nil || failure.Version != hostAgentAPIVersion || failure.RunID != runID || failure.Diagnostic == "" || len(failure.Diagnostic) > 512 {
		writeControlPlaneError(response, http.StatusBadRequest, "invalid Windows Host Agent failure")
		return
	}
	handler.mu.Lock()
	assignment := handler.assignments[runID]
	agent := handler.hostAgents[agentID]
	if !requestMatchesHostAgentCredential(request, agent) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusUnauthorized, "Windows Host Agent authentication changed")
		return
	}
	if assignment == nil {
		handler.mu.Unlock()
		var completed hostAgentAssignmentRecord
		err := readPrivateJSON(filepath.Join(handler.dataDir, "assignments", runID+".json"), &completed)
		if err == nil && completed.State == "completed" && completed.AgentID == agentID && sameLockToken(failure.LockToken, completed.LockToken) && completed.Terminal != nil {
			response.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(response).Encode(completed.Terminal)
			return
		}
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	if assignment != nil && assignment.record.State == "finishing" {
		if failure.RunID != runID || assignment.record.AgentID != agentID || agent == nil || failure.Quarantine || !sameLockToken(failure.LockToken, assignment.record.LockToken) || !handler.acceptHostAgentSession(agent, failure.SessionID) {
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
			return
		}
		terminal, err := handler.resumeFinishingHostAgentAssignment(assignment, agent)
		handler.mu.Unlock()
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "resume Windows Host Agent failure")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(terminal)
		return
	}
	if assignment == nil || assignment.record.AgentID != agentID || agent == nil || !sameLockToken(failure.LockToken, assignment.record.LockToken) || !hostAgentAssignmentAcceptsFailure(assignment.record, failure.Quarantine) || assignment.finishing || !handler.acceptHostAgentSession(agent, failure.SessionID) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	if failure.Quarantine {
		handler.mu.Unlock()
		outcome, runErr, err := handler.quarantineHostAgentAssignment(assignment, agent, failure.Diagnostic)
		if err != nil {
			writeControlPlaneError(response, http.StatusInternalServerError, "persist quarantined Windows Host Agent assignment")
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(response).Encode(controlPlaneTerminalResponse(outcome, runErr, assignment.repoDir))
		return
	}
	handler.mu.Unlock()
	assignment.checkpointMu.Lock()
	defer assignment.checkpointMu.Unlock()
	handler.mu.Lock()
	assignment = handler.assignments[runID]
	agent = handler.hostAgents[agentID]
	if assignment == nil || assignment.record.AgentID != agentID || agent == nil || !requestMatchesHostAgentCredential(request, agent) || !sameLockToken(failure.LockToken, assignment.record.LockToken) || !hostAgentAssignmentAcceptsFailure(assignment.record, failure.Quarantine) || assignment.finishing || !handler.acceptHostAgentSession(agent, failure.SessionID) {
		handler.mu.Unlock()
		writeControlPlaneError(response, http.StatusConflict, "Host Lock ownership changed")
		return
	}
	assignment.finishing = true
	handler.mu.Unlock()

	runErr := errors.New(failure.Diagnostic)
	run := assignment.run
	if run == nil {
		var err error
		run, err = loadAcceptedHostAgentRun(assignment.repoDir, assignment.record, handler.runtime)
		if err != nil {
			handler.mu.Lock()
			assignment.finishing = false
			handler.mu.Unlock()
			writeControlPlaneError(response, http.StatusInternalServerError, "load durable Windows Host Agent run")
			return
		}
		assignment.run = run
	}
	ledgerSnapshot, err := newRunLedgerStore(run.repoDir).Snapshot(run.manifest.RunID)
	if err != nil {
		_ = handler.resetHostAgentFinishing(assignment)
		writeControlPlaneError(response, http.StatusInternalServerError, "read durable Windows Host Agent run")
		return
	}
	originalLedger := ledgerSnapshot.Record
	acknowledgedEvents := ledgerSnapshot.Events
	acknowledgedLog := ledgerSnapshot.Log
	targetProfile := assignment.record.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	run.host.HostID = assignment.record.HostID
	run.host.TargetProfile = targetProfile
	run.manifest.Host = assignment.record.HostID
	run.manifest.TargetProfile = targetProfile
	run.failedLayer = failureLayerRunState
	outcome, finishErr := run.finishEarlyFailure(runErr)
	terminalEvents, terminalEventsErr := readRunLedgerBytes(run.context.EventsPath)
	terminalLedger, transactionErr := newRunLedgerStore(run.repoDir).FinalizeAcknowledgedFailure(
		outcome, run.manifest, run.ledgerPolicy, run.runtime.Now(),
		acknowledgedEvents, terminalEvents, acknowledgedLog, failure.Diagnostic, &originalLedger,
	)
	runErr = errors.Join(finishErr, terminalEventsErr, transactionErr)
	if transactionErr != nil {
		_ = handler.resetHostAgentFinishing(assignment)
		writeControlPlaneError(response, http.StatusInternalServerError, "stage Windows Host Agent failure")
		return
	}
	assignment.originalLedger = &originalLedger
	assignment.terminalLedger = &terminalLedger
	if err := handler.finishHostAgentAssignment(assignment, failure.SessionID, outcome, runErr); err != nil {
		_ = handler.resetHostAgentFinishing(assignment)
		writeControlPlaneError(response, http.StatusInternalServerError, "persist Windows Host Agent failure")
		return
	}
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(controlPlaneTerminalResponse(outcome, runErr, assignment.repoDir))
}

func (handler *controlPlaneHandler) quarantineHostAgentAssignment(assignment *controlPlaneHostAgentAssignment, agent *controlPlaneHostAgent, diagnostic string) (runOutcome, error, error) {
	assignment.checkpointMu.Lock()
	defer assignment.checkpointMu.Unlock()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.assignments[assignment.record.RunID] != assignment || handler.hostAgents[assignment.record.AgentID] != agent || assignment.finishing {
		ownershipErr := errors.New("Host Lock ownership changed") //nolint:staticcheck // Host Lock is a product term.
		return runOutcome{}, ownershipErr, ownershipErr
	}
	if assignment.record.State == "quarantined" && assignment.finished {
		return assignment.outcome, assignment.err, nil
	}
	if !hostAgentAssignmentFinalizable(assignment.record) && assignment.record.State != "quarantined" {
		ownershipErr := errors.New("Host Lock ownership changed") //nolint:staticcheck // Host Lock is a product term.
		return runOutcome{}, ownershipErr, ownershipErr
	}
	return handler.quarantineHostAgentAssignmentLocked(assignment, agent, diagnostic)
}

// quarantineHostAgentAssignmentLocked runs with checkpointMu and handler.mu held.
func (handler *controlPlaneHandler) quarantineHostAgentAssignmentLocked(assignment *controlPlaneHostAgentAssignment, agent *controlPlaneHostAgent, diagnostic string) (runOutcome, error, error) {
	runErr := errors.New(diagnostic)
	var outcome runOutcome
	run := assignment.run
	if run == nil {
		var err error
		run, err = loadAcceptedHostAgentRun(assignment.repoDir, assignment.record, handler.runtime)
		if err != nil {
			return runOutcome{}, runErr, err
		}
		assignment.run = run
	}
	ledgerStore := newRunLedgerStore(run.repoDir)
	acknowledged, transactionErr := ledgerStore.Snapshot(assignment.record.RunID)
	if transactionErr != nil {
		return runOutcome{}, runErr, transactionErr
	}
	targetProfile := assignment.record.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	run.host.HostID = assignment.record.HostID
	run.host.TargetProfile = targetProfile
	run.manifest.Host = assignment.record.HostID
	run.manifest.TargetProfile = targetProfile
	run.stopPolicy = "unresolved"
	if err := run.ensureAcceptedFailureEvidence(runErr); err != nil {
		return runOutcome{}, runErr, err
	}
	run.failure.CleanupState = "failed"
	run.failure.RemediationHint = "Verify and stop the retained Maya session; this version keeps the quarantined Host Lock fail-closed."
	if err := run.updateTerminalFailureEvidence(); err != nil {
		return runOutcome{}, runErr, err
	}
	terminalEvents, transactionErr := readRunLedgerBytes(run.context.EventsPath)
	if transactionErr != nil {
		return runOutcome{}, runErr, transactionErr
	}
	outcome = run.currentOutcome()
	terminalLedger, transactionErr := ledgerStore.FinalizeAcknowledgedFailure(
		outcome, run.manifest, run.ledgerPolicy, run.runtime.Now(),
		acknowledged.Events, terminalEvents, acknowledged.Log, diagnostic, nil,
	)
	if transactionErr != nil {
		return runOutcome{}, runErr, transactionErr
	}
	quarantined := assignment.record
	quarantined.State = "quarantined"
	quarantined.AssignedLedger = &terminalLedger
	quarantined.TerminalLedger = &terminalLedger
	terminal := controlPlaneTerminalResponse(outcome, runErr, assignment.repoDir)
	quarantined.Terminal = &terminal
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(quarantined, handler.prepareRecoveredHostAgentTransition); err != nil {
		return runOutcome{}, runErr, err
	}
	assignment.record = quarantined
	assignment.outcome = outcome
	assignment.err = runErr
	assignment.terminalLedger = &terminalLedger
	if !assignment.finished {
		assignment.finished = true
		close(assignment.done)
	}
	agent.status.State = "quarantined"
	agent.status.SessionID = ""
	agent.sessionExpiresAt = time.Time{}
	_ = handler.persistHostAgentStatus(agent)
	handler.signalHostAgent(agent)
	return outcome, runErr, nil
}

func (handler *controlPlaneHandler) resetHostAgentFinishing(assignment *controlPlaneHostAgentAssignment) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if handler.assignments[assignment.record.RunID] == assignment && hostAgentAssignmentFinalizable(assignment.record) {
		if assignment.originalLedger != nil {
			if err := newRunLedgerStore(assignment.repoDir).Replace(*assignment.originalLedger); err != nil {
				return err
			}
		}
		assignment.finishing = false
	}
	return nil
}

func (handler *controlPlaneHandler) finishHostAgentAssignment(assignment *controlPlaneHostAgentAssignment, sessionID string, outcome runOutcome, runErr error) error {
	handler.mu.Lock()
	defer handler.mu.Unlock()
	current := handler.assignments[assignment.record.RunID]
	if current != assignment || !hostAgentAssignmentFinalizable(assignment.record) || !assignment.finishing {
		return fmt.Errorf("Host Lock ownership changed") //nolint:staticcheck // Host Lock is a product term.
	}
	agent := handler.hostAgents[assignment.record.AgentID]
	if !handler.acceptHostAgentSession(agent, sessionID) {
		return fmt.Errorf("Windows Host Agent session changed before completion commit") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	finishing := assignment.record
	finishing.State = "finishing"
	terminal := controlPlaneTerminalResponse(outcome, runErr, assignment.repoDir)
	finishing.Terminal = &terminal
	finishing.TerminalLedger = assignment.terminalLedger
	finishing.AssignedLedger = assignment.terminalLedger
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(finishing, handler.prepareRecoveredHostAgentTransition); err != nil {
		return err
	}
	assignment.record = finishing
	assignment.outcome = outcome
	assignment.err = runErr
	if !agent.status.DeadlineActions {
		_, err := handler.resumeFinishingHostAgentAssignment(assignment, agent)
		return err
	}
	assignment.finishing = false
	agent.status.State = "cleaning"
	_ = handler.persistHostAgentStatus(agent)
	handler.signalHostAgent(agent)
	return nil
}

func hostAgentAssignmentAcceptsCompletion(record hostAgentAssignmentRecord, terminal runCommandJSON) bool {
	if record.State == "confirmed" {
		return true
	}
	if record.State != "expiring" {
		return false
	}
	if record.ExpiryFromState == "kept" {
		return true
	}
	// A passed terminal is only admission to deep cleanup-proof validation.
	// The handler converts valid raced success to deadline failure before commit.
	return record.ExpiryFromState == "confirmed"
}

func hostAgentAssignmentFinalizable(record hostAgentAssignmentRecord) bool {
	return record.State == "confirmed" || record.State == "expiring"
}

func hostAgentAssignmentAcceptsFailure(record hostAgentAssignmentRecord, quarantine bool) bool {
	if quarantine {
		return hostAgentAssignmentFinalizable(record)
	}
	return record.State == "confirmed" ||
		record.State == "expiring" && (record.ExpiryFromState == "assigned" || record.ExpiryFromState == "confirmed" && record.BrokerSession == nil)
}

func (handler *controlPlaneHandler) resumeFinishingHostAgentAssignment(assignment *controlPlaneHostAgentAssignment, agent *controlPlaneHostAgent) (runCommandJSON, error) {
	if assignment.record.Terminal == nil || assignment.record.TerminalLedger == nil {
		return runCommandJSON{}, fmt.Errorf("finishing Host Agent assignment has no terminal state")
	}
	if err := cleanupRunState(assignment.repoDir, assignment.record.RunID); err != nil {
		return runCommandJSON{}, err
	}
	if assignment.sharedFakeHostRelease != nil {
		if err := assignment.sharedFakeHostRelease(); err != nil {
			return runCommandJSON{}, err
		}
		assignment.sharedFakeHostRelease = nil
	} else if err := removeRepoHostLockForRun(filepath.Join(handler.dataDir, "fake-host"), assignment.record.HostID, assignment.record.RunID); err != nil {
		return runCommandJSON{}, err
	}
	completed := assignment.record
	completed.State = "completed"
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(completed, handler.prepareRecoveredHostAgentTransition); err != nil {
		return runCommandJSON{}, err
	}
	assignment.record = completed
	assignment.outcome = runOutcomeFromCommandJSON(*completed.Terminal)
	if assignment.err == nil && completed.Terminal.Status == resultStatusFailed {
		diagnostic := completed.Terminal.Diagnostic
		if diagnostic == "" {
			diagnostic = completed.Terminal.Error
		}
		if diagnostic == "" {
			diagnostic = "Windows Host Agent run failed before cleanup acknowledgement" //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		assignment.err = errors.New(diagnostic)
	}
	assignment.finishing = false
	if !assignment.finished {
		assignment.finished = true
		close(assignment.done)
	}
	agent.status.State = "offline"
	agent.status.RunID = ""
	agent.status.SessionID = ""
	agent.sessionExpiresAt = time.Time{}
	_ = handler.persistHostAgentStatus(agent)
	delete(handler.assignments, assignment.record.RunID)
	handler.signalHostAgent(agent)
	return *completed.Terminal, nil
}

func (handler *controlPlaneHandler) quarantineHostAgentCleanupFailure(assignment *controlPlaneHostAgentAssignment, agent *controlPlaneHostAgent, outcome runOutcome, cleanupErr error) error {
	failure := &runFailureEvidence{
		FailedLayer: string(failureLayerRunState), Diagnostic: cleanupErr.Error(),
		RemediationHint: "Inspect cleanup state; this version keeps the quarantined Host Lock fail-closed.",
		CaptureState:    "completed", CleanupState: "failed",
	}
	outcome.Result = ScenarioResult{Status: resultStatusFailed, Summary: cleanupErr.Error()}
	outcome.StopPolicy = "unresolved"
	outcome.FollowUpCommands = nil
	outcome.Failure = failure
	ledger := *assignment.terminalLedger
	ledger.State = "cleanup-failed"
	ledger.Status = resultStatusFailed
	ledger.UpdatedAt = handler.runtime.Now().UTC().Format(time.RFC3339Nano)
	ledger.CompletedAt = ledger.UpdatedAt
	if err := writeControlPlaneCleanupFailureEvidence(assignment.repoDir, assignment.record.RunID, failure); err != nil {
		return errors.Join(cleanupErr, err)
	}
	quarantined := assignment.record
	quarantined.State = "quarantined"
	quarantined.AssignedLedger = &ledger
	quarantined.TerminalLedger = &ledger
	terminal := controlPlaneTerminalResponse(outcome, cleanupErr, assignment.repoDir)
	quarantined.Terminal = &terminal
	if err := newHostAgentTransitionStore(handler.dataDir).Commit(quarantined, handler.prepareRecoveredHostAgentTransition); err != nil {
		return errors.Join(cleanupErr, err)
	}
	assignment.record = quarantined
	assignment.outcome = outcome
	assignment.err = cleanupErr
	assignment.finishing = false
	assignment.finished = true
	assignment.terminalLedger = &ledger
	agent.status.State = "quarantined"
	agent.status.SessionID = ""
	agent.sessionExpiresAt = time.Time{}
	_ = handler.persistHostAgentStatus(agent)
	close(assignment.done)
	handler.signalHostAgent(agent)
	return cleanupErr
}

func writeControlPlaneCleanupFailureEvidence(repoDir string, runID string, failure *runFailureEvidence) error {
	path := filepath.Join(repoDir, "artifacts", "maya-stall", runID, evidenceBundleFileName)
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var bundle evidenceBundle
	if err := json.Unmarshal(content, &bundle); err != nil {
		return err
	}
	bundle.Status = resultStatusFailed
	bundle.Failure = failure
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	return writeJSONFile(path, bundle)
}

func sameLockToken(got string, want string) bool {
	return got != "" && len(got) == len(want) && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (handler *controlPlaneHandler) runScenarioThroughHostAgent(repoDir string, submission controlPlaneSubmission, options runOptions, runtime runRuntime) (runOutcome, error) {
	targetProfile := submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	requirements, err := scenarioRequirementsForScheduling(repoDir, submission.Scenario)
	if err != nil {
		return failHostAgentSelection(repoDir, options, runtime, err)
	}
	run, selected, selectedCapabilities, selectedMayaBuild, sharedFakeHostRelease, err := handler.queueHostAgentRun(repoDir, submission, options, requirements, runtime)
	if err != nil {
		if errors.Is(err, errControlPlaneQueueFull) {
			return runOutcome{}, err
		}
		if errors.Is(err, errQueuedRunCanceled) && run != nil {
			return run.currentOutcome(), err
		}
		if run != nil && run.accepted {
			run.failedLayer = failureLayerHostSelection
			outcome, finishErr := run.finishEarlyFailure(err)
			ledgerErr := newRunLedgerStore(run.repoDir).Finalize(outcome, run.manifest, run.ledgerPolicy, run.runtime.Now())
			var queueErr error
			if ledgerErr == nil {
				handler.mu.Lock()
				queueErr = handler.removeQueuedRunLocked(run.manifest.RunID)
				handler.mu.Unlock()
			}
			return outcome, errors.Join(finishErr, ledgerErr, queueErr)
		}
		return failHostAgentSelection(repoDir, options, runtime, err)
	}
	failBeforeTransition := func(runErr error) (runOutcome, error) {
		outcome, finishErr := handler.finishAcceptedHostAgentFailure(run, selected, errors.Join(runErr, sharedFakeHostRelease()))
		terminalLedger, terminalErr := newRunLedgerStore(run.repoDir).Read(run.manifest.RunID)
		var queueErr error
		if terminalErr == nil && terminalRunLedgerRecord(terminalLedger) {
			handler.mu.Lock()
			queueErr = handler.removeQueuedRunLocked(run.manifest.RunID)
			handler.mu.Unlock()
		} else {
			queueErr = errors.Join(errors.New("retain queued Run ownership until terminal failure is durable"), terminalErr)
		}
		return outcome, errors.Join(finishErr, queueErr)
	}
	runID := run.manifest.RunID
	sharedFakeRepo := filepath.Join(handler.dataDir, "fake-host")
	if err := markHostLockActive(sharedFakeRepo, selected.status.HostID, runID); err != nil {
		return failBeforeTransition(err)
	}
	sharedFakeHostRelease = func() error {
		return removeRepoHostLockForRun(sharedFakeRepo, selected.status.HostID, runID)
	}
	lockToken, err := newHostAgentLockToken()
	if err != nil {
		return failBeforeTransition(err)
	}
	createdAt := handler.runtime.Now().UTC().Format(time.RFC3339Nano)
	ledgerSnapshot, err := newRunLedgerStore(repoDir).Snapshot(runID)
	if err != nil {
		return failBeforeTransition(err)
	}
	assignedLedger := ledgerSnapshot.Record
	assignedLedger.State = "submitted"
	assignedLedger.Host = selected.status.HostID
	eventPrefix := ledgerSnapshot.Events
	record := hostAgentAssignmentRecord{
		Version: hostAgentAPIVersion, RunID: runID, AgentID: selected.status.AgentID, HostID: selected.status.HostID,
		LockToken: lockToken, State: "assigned", CreatedAt: createdAt, Submission: submission, EventPrefix: eventPrefix,
		Capabilities: selectedCapabilities, SelectedMayaBuild: selectedMayaBuild, SessionBindingRequired: selected.status.SessionBinding, AssignedLedger: &assignedLedger,
	}
	record.hostLockDeadlines = newHostLockDeadlines(handler.runtime.Now(), handler.hostLockPolicy)
	targetProfile = record.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	run.host.HostID = record.HostID
	run.host.TargetProfile = targetProfile
	run.manifest.Host = record.HostID
	run.manifest.TargetProfile = targetProfile
	assignment := &controlPlaneHostAgentAssignment{record: record, repoDir: repoDir, done: make(chan struct{}), run: run, sharedFakeHostRelease: sharedFakeHostRelease}

	handler.mu.Lock()
	if handler.hostAgents[selected.status.AgentID] != selected || selected.status.State != "reserving" || selected.status.RunID != "" {
		handler.resetQueuedDispatchLocked(runID)
		handler.mu.Unlock()
		if releaseErr := sharedFakeHostRelease(); releaseErr != nil {
			sharedFakeHostRelease = func() error { return nil }
			return failBeforeTransition(errors.Join(errors.New("Windows Host Agent reservation changed"), releaseErr)) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		return handler.runScenarioThroughHostAgent(repoDir, submission, options, runtime)
	}
	if selected.status.SessionID == "" || !handler.runtime.Now().Before(selected.sessionExpiresAt) {
		selected.status.State = "offline"
		selected.status.SessionID = ""
		selected.sessionExpiresAt = time.Time{}
		_ = handler.persistHostAgentStatus(selected)
		handler.resetQueuedDispatchLocked(runID)
		handler.mu.Unlock()
		if releaseErr := sharedFakeHostRelease(); releaseErr != nil {
			sharedFakeHostRelease = func() error { return nil }
			return failBeforeTransition(errors.Join(errors.New("Windows Host Agent process-session lease expired during reservation"), releaseErr)) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		return handler.runScenarioThroughHostAgent(repoDir, submission, options, runtime)
	}
	quarantineMarkerPath := handler.hostAgentQuarantinePath(record.AgentID)
	quarantineIntent := record
	quarantineIntent.State = "quarantined"
	if err := writePrivateJSON(quarantineMarkerPath, quarantineIntent); err != nil {
		handler.mu.Unlock()
		return failBeforeTransition(fmt.Errorf("persist Windows Host Agent assignment write-ahead marker: %w", err))
	}
	handler.mu.Unlock()
	var transitionErr error
	var eligibilityErr error
	handler.mu.Lock()
	if handler.hostAgents[selected.status.AgentID] != selected || selected.status.State != "reserving" || selected.status.RunID != "" {
		eligibilityErr = errors.Join(eligibilityErr, errors.New("Windows Host Agent reservation changed during acceptance")) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if selected.status.SessionID == "" || !handler.runtime.Now().Before(selected.sessionExpiresAt) {
		eligibilityErr = errors.Join(eligibilityErr, errors.New("Windows Host Agent process-session lease expired during acceptance")) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if transitionErr == nil && eligibilityErr == nil {
		eligibilityErr = handler.refreshQueuedReservationCapabilitiesLocked(runID, targetProfile, requirements, selected, &record)
	}
	if eligibilityErr != nil {
		handler.mu.Unlock()
		markerCleanupErr := os.Remove(quarantineMarkerPath)
		if errors.Is(markerCleanupErr, os.ErrNotExist) {
			markerCleanupErr = nil
		} else if markerCleanupErr == nil {
			markerCleanupErr = syncRunLedgerDirectory(filepath.Dir(quarantineMarkerPath))
		}
		releaseErr := sharedFakeHostRelease()
		if releaseErr == nil {
			sharedFakeHostRelease = func() error { return nil }
		}
		if markerCleanupErr == nil && releaseErr == nil {
			handler.mu.Lock()
			var statusErr error
			if handler.hostAgents[selected.status.AgentID] != selected || selected.status.State != "reserving" || selected.status.RunID != "" {
				statusErr = errors.New("Windows Host Agent reservation changed during rollback") //nolint:staticcheck // Product term starts the user-facing diagnostic.
			} else {
				if selected.status.SessionID == "" || !handler.runtime.Now().Before(selected.sessionExpiresAt) {
					selected.status.State = "offline"
					selected.status.SessionID = ""
					selected.sessionExpiresAt = time.Time{}
				} else {
					selected.status.State = "ready"
				}
				statusErr = handler.persistHostAgentStatus(selected)
			}
			if statusErr == nil {
				handler.resetQueuedDispatchLocked(runID)
			}
			handler.mu.Unlock()
			if statusErr != nil {
				return failBeforeTransition(errors.Join(eligibilityErr, statusErr))
			}
			return handler.runScenarioThroughHostAgent(repoDir, submission, options, runtime)
		}
		return failBeforeTransition(errors.Join(eligibilityErr, markerCleanupErr, releaseErr))
	}
	if transitionErr == nil {
		transitionErr = newHostAgentTransitionStore(handler.dataDir).Commit(record, handler.prepareRecoveredHostAgentTransition)
	}
	if transitionErr == nil {
		transitionErr = handler.removeQueuedRunLocked(runID)
	}
	if transitionErr == nil {
		if err := os.Remove(quarantineMarkerPath); err != nil {
			transitionErr = err
		} else if err := syncRunLedgerDirectory(filepath.Dir(quarantineMarkerPath)); err != nil {
			transitionErr = err
		}
	}
	if transitionErr != nil {
		record.State = "quarantined"
		assignment.record = record
		handler.assignments[runID] = assignment
		markerErr := writePrivateJSON(quarantineMarkerPath, record)
		selected.status.State = "quarantined"
		selected.status.RunID = runID
		selected.status.SessionID = ""
		selected.sessionExpiresAt = time.Time{}
		var statusErr error
		if markerErr == nil {
			statusErr = handler.persistHostAgentStatus(selected)
		}
		handler.mu.Unlock()
		outcome, failureErr := handler.finishAcceptedHostAgentFailure(run, selected, errors.Join(transitionErr, markerErr, statusErr, errors.New("Windows Host Agent slot quarantined after incomplete assignment transition"))) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		handler.mu.Lock()
		terminalLedger, terminalLedgerErr := newRunLedgerStore(repoDir).Read(runID)
		var quarantineTransitionErr error
		var markerCleanupErr error
		var queueErr error
		if terminalLedgerErr == nil {
			terminal := controlPlaneTerminalResponse(outcome, failureErr, repoDir)
			record.AssignedLedger = &terminalLedger
			record.TerminalLedger = &terminalLedger
			record.Terminal = &terminal
			quarantineTransitionErr = newHostAgentTransitionStore(handler.dataDir).Commit(record, handler.prepareRecoveredHostAgentTransition)
			if quarantineTransitionErr == nil {
				assignment.record = record
				assignment.terminalLedger = &terminalLedger
				queueErr = handler.removeQueuedRunLocked(runID)
				if err := os.Remove(quarantineMarkerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
					markerCleanupErr = err
				} else if err == nil {
					markerCleanupErr = syncRunLedgerDirectory(filepath.Dir(quarantineMarkerPath))
				}
			}
		}
		failureErr = errors.Join(failureErr, terminalLedgerErr, quarantineTransitionErr, queueErr, markerCleanupErr)
		assignment.outcome = outcome
		assignment.err = failureErr
		assignment.finished = true
		close(assignment.done)
		handler.mu.Unlock()
		return outcome, failureErr
	}
	selected.status.State = "locked"
	selected.status.RunID = runID
	_ = handler.persistHostAgentStatus(selected)
	handler.assignments[runID] = assignment
	handler.signalHostAgent(selected)
	handler.mu.Unlock()

	for {
		handler.mu.Lock()
		if assignment.finished {
			handler.mu.Unlock()
			break
		}
		agent := handler.hostAgents[assignment.record.AgentID]
		now := handler.runtime.Now()
		sessionExpired := agent != nil && agent.status.SessionID != "" && !now.Before(agent.sessionExpiresAt)
		restartGraceExpired := agent != nil && agent.status.SessionID == "" && !now.Before(agent.takeoverNotBefore)
		if assignment.record.State == "confirmed" && !assignment.finishing && (sessionExpired || restartGraceExpired) {
			handler.mu.Unlock()
			_, _, _ = handler.quarantineHostAgentAssignment(assignment, agent, "Windows Host Agent process-session lease expired before Maya shutdown was verified")
			handler.mu.Lock()
		}
		done := assignment.done
		handler.mu.Unlock()
		select {
		case <-done:
		case <-time.After(hostAgentHeartbeatInterval):
			continue
		}
		break
	}
	handler.mu.Lock()
	outcome := assignment.outcome
	runErr := assignment.err
	handler.mu.Unlock()
	return outcome, runErr
}

func failHostAgentSelection(repoDir string, options runOptions, runtime runRuntime, selectionErr error) (runOutcome, error) {
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	run := &freshRunLifecycle{
		repoDir: repoDir, options: options, runtime: runtime, stopPolicy: "stopped", ledgerPolicy: defaultRunLedgerPolicy(),
	}
	if err := run.accept(); err != nil {
		return runOutcome{}, errors.Join(selectionErr, err, run.cleanupUnacceptedOwnership())
	}
	if policy, available := availableRunLedgerPolicy(repoDir); available {
		run.ledgerPolicy = policy
	}
	run.failedLayer = failureLayerHostSelection
	outcome, runErr := run.finishEarlyFailure(selectionErr)
	ledgerErr := newRunLedgerStore(run.repoDir).Finalize(outcome, run.manifest, run.ledgerPolicy, run.runtime.Now())
	return outcome, errors.Join(runErr, ledgerErr)
}

func (handler *controlPlaneHandler) finishAcceptedHostAgentFailure(run *freshRunLifecycle, selected *controlPlaneHostAgent, runErr error) (runOutcome, error) {
	handler.mu.Lock()
	if selected.status.State == "reserving" {
		selected.status.State = "ready"
		_ = handler.persistHostAgentStatus(selected)
	}
	handler.mu.Unlock()
	run.failedLayer = failureLayerHostSelection
	outcome, finishErr := run.finishEarlyFailure(runErr)
	ledgerErr := newRunLedgerStore(run.repoDir).Finalize(outcome, run.manifest, run.ledgerPolicy, run.runtime.Now())
	return outcome, errors.Join(finishErr, ledgerErr)
}

func newHostAgentLockToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func (handler *controlPlaneHandler) signalHostAgent(agent *controlPlaneHostAgent) {
	close(agent.notify)
	agent.notify = make(chan struct{})
}

func (handler *controlPlaneHandler) persistHostAgentStatus(agent *controlPlaneHostAgent) error {
	return writePrivateJSON(filepath.Join(handler.dataDir, "host-agents", agent.status.AgentID, "status.json"), agent.status)
}

func (handler *controlPlaneHandler) hostLockPath(hostID string) string {
	return newHostAgentTransitionStore(handler.dataDir).hostLockPath(hostID)
}

func (handler *controlPlaneHandler) hostAgentQuarantinePath(agentID string) string {
	return filepath.Join(handler.dataDir, "host-agents", agentID, "quarantine.json")
}

func sameBrokerSession(left *brokerSessionIdentity, right *brokerSessionIdentity) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func invalidBrokerSession(session *brokerSessionIdentity) bool {
	return session != nil && (session.BrokerAdapter == "" || session.SessionID == "")
}

func writePrivateJSON(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeRunLedgerBytes(path, append(content, '\n'))
}

func (handler *controlPlaneHandler) acceptHostAgentCompletion(assignment *controlPlaneHostAgentAssignment, completion hostAgentCompletionRequest) (runOutcome, runLedgerRecord, runLedgerRecord, error) {
	expectedProfile := assignment.record.Submission.TargetProfile
	if expectedProfile == "" {
		expectedProfile = "default"
	}
	if err := validateHostAgentCompletionIdentity(assignment.record, completion.Terminal); err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("terminal result does not match assignment")
	}
	if err := validateHostAgentResultFiles(assignment.record.RunID, completion.Files); err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	stagingRoot, err := os.MkdirTemp(filepath.Join(handler.dataDir, "incoming"), "."+assignment.record.RunID+"-*")
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	defer func() { _ = os.RemoveAll(stagingRoot) }()
	if err := os.Chmod(stagingRoot, 0o700); err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	if err := materializeControlPlaneFiles(stagingRoot, completion.Files); err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	record, err := newRunLedgerStore(stagingRoot).Read(assignment.record.RunID)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	if record.Host != assignment.record.HostID || record.Status == resultStatusPassed && record.State != "completed" || record.Status == resultStatusFailed && record.State != "failed" || record.Status != resultStatusPassed && record.Status != resultStatusFailed {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent did not return a cleaned terminal run") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	result, err := readControlPlaneResult(stagingRoot, record)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	if !result.Final || result.CleanupState != "completed" {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent cleanup is not complete") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	evidence, err := readControlPlaneEvidence(stagingRoot, record)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	if evidence.Bundle.Scenario != assignment.record.Submission.Scenario || evidence.Bundle.TargetProfile != expectedProfile || evidence.Bundle.Host != assignment.record.HostID {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent Evidence Bundle changed immutable run identity") //nolint:staticcheck // Product terms start the user-facing diagnostic.
	}
	if !validHostAgentCompletionSession(assignment.record.SessionBindingRequired, record.Status, assignment.record.BrokerSession, evidence.Bundle.BrokerSession) {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent Evidence Bundle does not match the Host Lock Maya UI Session") //nolint:staticcheck // Product terms start the user-facing diagnostic.
	}
	for _, artifact := range evidence.Bundle.VisualEvidence {
		if artifact.TargetProfile != "" && artifact.TargetProfile != expectedProfile || artifact.Host != "" && artifact.Host != assignment.record.HostID {
			return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent Visual Evidence changed target identity") //nolint:staticcheck // Product terms start the user-facing diagnostic.
		}
	}
	stagedOutcome := runOutcomeFromCommandJSON(completion.Terminal)
	if stagedOutcome.Result.Status != record.Status || result.Status != record.Status || result.Result.Status != record.Status || evidence.Bundle.Status != record.Status {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent terminal status does not match durable result") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	original, err := newRunLedgerStore(assignment.repoDir).Read(assignment.record.RunID)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	if record.RunID != original.RunID || record.Scenario != original.Scenario || record.TargetProfile != original.TargetProfile || record.Host != original.Host {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent changed immutable Run Ledger identity") //nolint:staticcheck // Product terms start the user-facing diagnostic.
	}
	merged := record
	merged.Version = original.Version
	merged.RunID = original.RunID
	merged.Scenario = original.Scenario
	merged.TargetProfile = original.TargetProfile
	merged.Host = original.Host
	merged.AcceptedAt = original.AcceptedAt
	merged.EvidenceDir = original.EvidenceDir
	merged.Events = original.Events
	merged.Log = original.Log
	stagedOutcome.RunID = original.RunID
	stagedOutcome.Scenario = original.Scenario
	stagedOutcome.TargetProfile = original.TargetProfile
	stagedOutcome.Host = original.Host
	stagedOutcome.Accepted = true
	stagedOutcome.StopPolicy = "stopped"
	stagedOutcome.FollowUpCommands = nil
	policy := defaultRunLedgerPolicy()
	if available, ok := availableRunLedgerPolicy(assignment.repoDir); ok {
		policy = available
	}
	merged.EventCount = 0
	merged.EventBytes = 0
	merged.EventsOmitted = 0
	merged.EventsTruncated = false
	merged.LogBytes = 0
	merged.LogTruncated = false
	stagedLedgerStore := newRunLedgerStore(stagingRoot)
	if err := stagedLedgerStore.SyncArtifacts(&merged, policy); err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	_, currentEvents, err := newRunLedgerStore(assignment.repoDir).ReadEvents(original.RunID)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	_, stagedEvents, err := stagedLedgerStore.ReadEvents(merged.RunID)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	identityPreservingEvents, err := mergeHostAgentTerminalEvents(currentEvents, stagedEvents)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, fmt.Errorf("Windows Host Agent terminal events changed acknowledged identity: %w", err) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if err := stagedLedgerStore.ReplaceEvents(merged.RunID, identityPreservingEvents); err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	merged.EventCount, merged.EventsOmitted, merged.EventsTruncated, err = retainedRunLedgerEventMetadata(bytes.NewReader(identityPreservingEvents))
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	merged.EventBytes = len(identityPreservingEvents)
	stagedFiles, err := buildHostAgentResultFiles(stagingRoot, assignment.record.RunID)
	if err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	if err := copyHostAgentResultFiles(assignment.repoDir, stagedFiles, assignment.record.RunID); err != nil {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, err
	}
	serverResult, err := readControlPlaneResult(assignment.repoDir, merged)
	if err != nil || !serverResult.Final || serverResult.CleanupState != "completed" {
		return runOutcome{}, runLedgerRecord{}, runLedgerRecord{}, errors.Join(fmt.Errorf("transferred Windows Host Agent result is not durable"), err)
	}
	stagedOutcome.StateDir = "/v1/runs/" + assignment.record.RunID + "/status"
	stagedOutcome.EvidenceDir = "/v1/runs/" + assignment.record.RunID + "/evidence"
	return stagedOutcome, original, merged, nil
}

func (handler *controlPlaneHandler) convertDeadlineRacedHostAgentCompletion(assignment *controlPlaneHostAgentAssignment, record runLedgerRecord) (runOutcome, runLedgerRecord, error) {
	diagnostic := "active Host Lock deadline expired before its Windows Host Agent reported completion" //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
	failure := &runFailureEvidence{
		FailedLayer: string(failureLayerRunState), Diagnostic: diagnostic,
		RemediationHint: earlyFailureRemediation(failureLayerRunState), CaptureState: "completed", CleanupState: "completed",
	}
	evidenceDir, err := safeControlPlaneEvidenceDir(assignment.repoDir, record)
	if err != nil {
		return runOutcome{}, record, err
	}
	evidence, err := readControlPlaneEvidence(assignment.repoDir, record)
	if err != nil {
		return runOutcome{}, record, err
	}
	evidence.Bundle.Status = resultStatusFailed
	evidence.Bundle.Failure = failure
	evidence.Bundle.Artifacts = buildEvidenceBundleCatalog(evidence.Bundle)
	if evidence.Bundle.ScenarioResult != "" {
		clean := cleanEvidenceArtifactPath(evidence.Bundle.ScenarioResult)
		if clean == "" {
			return runOutcome{}, record, fmt.Errorf("invalid deadline-raced Scenario Result path")
		}
		if err := ensureWorkspacePathHasNoSymlinkAncestor(evidenceDir, clean); err != nil {
			return runOutcome{}, record, err
		}
		result := newScenarioResultDocument(ScenarioResult{Status: resultStatusFailed, Summary: diagnostic})
		if err := writeJSONFile(filepath.Join(evidenceDir, filepath.FromSlash(clean)), result.Fields); err != nil {
			return runOutcome{}, record, err
		}
	}
	if err := writeJSONFile(filepath.Join(evidenceDir, evidenceBundleFileName), evidence.Bundle); err != nil {
		return runOutcome{}, record, err
	}
	now := handler.runtime.Now()
	record.State = "failed"
	record.Status = resultStatusFailed
	record.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	record.CompletedAt = record.UpdatedAt
	policy := defaultRunLedgerPolicy()
	if configured, ok := availableRunLedgerPolicy(assignment.repoDir); ok {
		policy = configured
	}
	store := newRunLedgerStore(assignment.repoDir)
	if err := store.SyncArtifacts(&record, policy); err != nil {
		return runOutcome{}, record, err
	}
	if err := store.Replace(record); err != nil {
		return runOutcome{}, record, err
	}
	outcome := runOutcome{
		RunID: record.RunID, Scenario: record.Scenario, TargetProfile: record.TargetProfile, Host: record.Host,
		StateDir: "/v1/runs/" + record.RunID + "/status", EvidenceDir: "/v1/runs/" + record.RunID + "/evidence",
		Result: ScenarioResult{Status: resultStatusFailed, Summary: diagnostic}, StopPolicy: "stopped", Accepted: true, Failure: failure,
	}
	return outcome, record, nil
}

func appendHostLockDeadlineEventsToLedger(repoDir string, record *runLedgerRecord, events []hostLockDeadlineEvent, now time.Time) error {
	if len(events) == 0 {
		return nil
	}
	var updated runLedgerRecord
	err := newRunLedgerStore(repoDir).UpdateSnapshot(record.RunID, func(snapshot *runLedgerSnapshot) error {
		existingIDs := make(map[string]struct{})
		lastSequence := 0
		for _, line := range bytes.Split(snapshot.Events, []byte{'\n'}) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				return err
			}
			if sequence := ledgerEventSequence(event); sequence > lastSequence {
				lastSequence = sequence
			}
			if id, ok := event["deadlineEventId"].(string); ok && id != "" {
				existingIDs[id] = struct{}{}
			}
		}
		combined := append([]byte(nil), snapshot.Events...)
		appended := false
		for _, event := range events {
			id := hostLockDeadlineEventID(event)
			if _, exists := existingIDs[id]; exists {
				continue
			}
			existingIDs[id] = struct{}{}
			appended = true
			lastSequence++
			content, err := json.Marshal(map[string]any{
				"event": event.Type, "type": event.Type, "timestamp": event.Timestamp,
				"phase": "retention", "stream": "lifecycle", "sequence": lastSequence,
				"detail": event.Detail, "details": map[string]any{"message": event.Detail},
				"deadlineEventId": id,
			})
			if err != nil {
				return err
			}
			combined = append(combined, content...)
			combined = append(combined, '\n')
		}
		eventCount, eventsOmitted, eventsTruncated, err := retainedRunLedgerEventMetadata(bytes.NewReader(combined))
		if err != nil {
			return err
		}
		snapshot.Events = combined
		snapshot.Record.EventCount = eventCount
		snapshot.Record.EventsOmitted = eventsOmitted
		snapshot.Record.EventsTruncated = eventsTruncated
		snapshot.Record.EventBytes = len(combined)
		if appended {
			snapshot.Record.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		}
		updated = snapshot.Record
		return nil
	})
	if err != nil {
		return err
	}
	*record = updated
	return nil
}

func syncHostAgentDeadlineEvents(repoDir string, runID string, events []hostLockDeadlineEvent) error {
	if len(events) == 0 {
		return nil
	}
	record, err := newRunLedgerStore(repoDir).Read(runID)
	if err != nil {
		return err
	}
	eventTime, err := time.Parse(time.RFC3339Nano, events[len(events)-1].Timestamp)
	if err != nil {
		return fmt.Errorf("invalid Host Lock deadline event timestamp: %w", err)
	}
	return appendHostLockDeadlineEventsToLedger(repoDir, &record, events, eventTime)
}

func hostLockDeadlineEventID(event hostLockDeadlineEvent) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d", event.Type, event.Detail, event.Timestamp, event.Ordinal)))
	return hex.EncodeToString(sum[:])
}

func validHostAgentCompletionSession(required bool, status string, lockSession *brokerSessionIdentity, evidenceSession *brokerSessionIdentity) bool {
	if !required {
		return true
	}
	if lockSession == nil {
		return status == resultStatusFailed && !invalidBrokerSession(evidenceSession)
	}
	if evidenceSession == nil {
		return false
	}
	return sameBrokerSession(lockSession, evidenceSession)
}

func validateHostAgentCompletionIdentity(assignment hostAgentAssignmentRecord, terminal runCommandJSON) error {
	expectedProfile := assignment.Submission.TargetProfile
	if expectedProfile == "" {
		expectedProfile = "default"
	}
	if terminal.Version != controlPlaneAPIVersion || terminal.Kind != "run" || !terminal.Accepted || terminal.RunID != assignment.RunID || terminal.Scenario != assignment.Submission.Scenario || terminal.TargetProfile != expectedProfile || terminal.Host != assignment.HostID || terminal.StopPolicy != "stopped" || len(terminal.FollowUpCommands) != 0 {
		return fmt.Errorf("terminal result does not match assignment")
	}
	return nil
}

func validateHostAgentResultFiles(runID string, files []controlPlaneFile) error {
	if len(files) == 0 || len(files) > 10_000 {
		return fmt.Errorf("Windows Host Agent result file count is invalid") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	ledgerRoot := filepath.ToSlash(filepath.Join(".maya-stall", "state", "ledger", "runs", runID))
	evidenceRoot := filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID))
	seen := make(map[string]bool, len(files))
	kinds := make(map[string]string, len(files))
	for _, file := range files {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file.Path)))
		if file.Path == "" || filepath.IsAbs(filepath.FromSlash(file.Path)) || strings.Contains(file.Path, "\\") || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("invalid Windows Host Agent result path")
		}
		if clean != file.Path || seen[clean] || file.Kind != "file" && file.Kind != "directory" || file.Kind == "directory" && len(file.Content) != 0 {
			return fmt.Errorf("invalid Windows Host Agent result path")
		}
		if clean != ledgerRoot && !strings.HasPrefix(clean, ledgerRoot+"/") && clean != evidenceRoot && !strings.HasPrefix(clean, evidenceRoot+"/") {
			return fmt.Errorf("Windows Host Agent result path is outside durable run data") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		seen[clean] = true
		kinds[clean] = file.Kind
	}
	for path := range kinds {
		for parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(path))); parent != "."; parent = filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent))) {
			if kinds[parent] == "file" {
				return fmt.Errorf("Windows Host Agent result file cannot contain child paths") //nolint:staticcheck // Product term starts the user-facing diagnostic.
			}
		}
	}
	for _, required := range []string{
		filepath.ToSlash(filepath.Join(ledgerRoot, "run.json")),
		filepath.ToSlash(filepath.Join(evidenceRoot, evidenceBundleFileName)),
	} {
		if !seen[required] {
			return fmt.Errorf("Windows Host Agent result is missing %s", required) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
	}
	return nil
}

func materializeControlPlaneFiles(repoDir string, files []controlPlaneFile) error {
	for _, file := range files {
		path := filepath.Join(repoDir, filepath.FromSlash(file.Path))
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
			return fmt.Errorf("unsupported Windows Host Agent result file kind")
		}
	}
	return nil
}

func copyHostAgentResultFiles(repoDir string, files []controlPlaneFile, runID string) error {
	ordered := append([]controlPlaneFile(nil), files...)
	runRecord := filepath.ToSlash(filepath.Join(".maya-stall", "state", "ledger", "runs", runID, "run.json"))
	sort.SliceStable(ordered, func(left int, right int) bool {
		if ordered[left].Path == runRecord {
			return false
		}
		if ordered[right].Path == runRecord {
			return true
		}
		return ordered[left].Path < ordered[right].Path
	})
	for _, file := range ordered {
		if file.Path == runRecord {
			continue
		}
		path := filepath.Join(repoDir, filepath.FromSlash(file.Path))
		if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Dir(file.Path)); err != nil {
			return err
		}
		if file.Kind == "directory" {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
			continue
		}
		if err := writeRunLedgerBytes(path, file.Content); err != nil {
			return err
		}
	}
	return nil
}

func hostAgentMutationOutboxPath(workRoot string, runID string) string {
	return filepath.Join(workRoot, "completion-outbox", runID+".json")
}

func persistHostAgentMutationOutbox(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, outbox hostAgentMutationOutbox) error {
	if err := ensureHostAgentDirectory(filepath.Join(options.WorkRoot, "completion-outbox")); err != nil {
		return err
	}
	outbox.Version = hostAgentAPIVersion
	outbox.RunID = assignment.RunID
	outbox.LockToken = assignment.LockToken
	return writePrivateJSON(hostAgentMutationOutboxPath(options.WorkRoot, assignment.RunID), outbox)
}

func persistHostAgentCleanupOutbox(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, brokerSession *brokerSessionIdentity, expiryFromState string) error {
	if brokerSession == nil {
		return fmt.Errorf("Host Lock cleanup has no exact Maya UI Session identity") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	cleanup := hostAgentCleanupMutation{BrokerSession: *brokerSession, ExpiryFromState: expiryFromState}
	return persistHostAgentMutationOutbox(options, assignment, hostAgentMutationOutbox{
		Kind: "cleanup", Cleanup: &cleanup,
	})
}

func removeHostAgentMutationOutbox(workRoot string, runID string) error {
	path := hostAgentMutationOutboxPath(workRoot, runID)
	if err := ensureHostAgentDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncRunLedgerDirectory(filepath.Dir(path))
}

func reconcileAcknowledgedHostAgentOutbox(options hostAgentRunOnceOptions, runtime runRuntime, stdout io.Writer) error {
	dir := filepath.Join(options.WorkRoot, "completion-outbox")
	if err := ensureHostAgentDirectory(dir); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" || validateRunID(strings.TrimSuffix(entry.Name(), ".json")) != nil {
			return fmt.Errorf("invalid Windows Host Agent completion outbox entry %s", entry.Name()) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		runID := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(dir, entry.Name())
		var outbox hostAgentMutationOutbox
		if err := readPrivateJSON(path, &outbox); err != nil {
			return err
		}
		if outbox.Version != hostAgentAPIVersion || outbox.RunID != runID || outbox.LockToken == "" || outbox.Kind != "complete" && outbox.Kind != "fail" {
			return fmt.Errorf("unacknowledged Windows Host Agent outbox %s requires its durable assignment", entry.Name()) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		assignment := hostAgentAssignmentResponse{
			Version: hostAgentAPIVersion, Kind: "host-agent-assignment", RunID: runID,
			AgentID: options.AgentID, HostID: options.HostID, LockToken: outbox.LockToken, Action: "finish",
		}
		replayed, replayErr := replayHostAgentMutationOutbox(options, assignment, runtime, stdout)
		if !replayed {
			return fmt.Errorf("Windows Host Agent outbox %s was not reconciled", entry.Name()) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		if _, err := os.Stat(path); err == nil {
			return errors.Join(fmt.Errorf("Windows Host Agent outbox %s remains unacknowledged", entry.Name()), replayErr) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func cleanHostAgentRunRoot(workRoot string, runID string) error {
	runRoot := filepath.Join(workRoot, "runs", runID)
	if err := os.RemoveAll(runRoot); err != nil {
		return err
	}
	if _, err := os.Lstat(runRoot); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("verify Windows Host Agent workspace cleanup: %w", err) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	return nil
}

func replayHostAgentMutationOutbox(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, runtime runRuntime, stdout io.Writer) (bool, error) {
	path := hostAgentMutationOutboxPath(options.WorkRoot, assignment.RunID)
	if err := ensureHostAgentDirectory(filepath.Dir(path)); err != nil {
		return true, err
	}
	var outbox hostAgentMutationOutbox
	if err := readPrivateJSON(path, &outbox); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return true, err
	}
	if outbox.Version != hostAgentAPIVersion || outbox.RunID != assignment.RunID || !sameLockToken(outbox.LockToken, assignment.LockToken) {
		return true, fmt.Errorf("Windows Host Agent completion outbox ownership changed") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if outbox.Kind == "fail" && outbox.Failure != nil && assignment.Action == "cleanup" {
		// Once durable authority orders exact-session cleanup, a failure mutation
		// that was never accepted cannot supersede that recoverable action.
		if err := removeHostAgentMutationOutbox(options.WorkRoot, assignment.RunID); err != nil {
			return true, err
		}
		return false, nil
	}
	// A completion outbox already contains the stopped-session evidence and all
	// durable result files. Replay lets the server validate that stronger proof;
	// a raced success is converted to deadline failure before lock release.
	switch outbox.Kind {
	case "cleanup":
		if outbox.Cleanup == nil || outbox.Completion != nil || outbox.Failure != nil || !sameBrokerSession(&outbox.Cleanup.BrokerSession, assignment.BrokerSession) || outbox.Cleanup.ExpiryFromState != "kept" && outbox.Cleanup.ExpiryFromState != "confirmed" {
			return true, fmt.Errorf("invalid Windows Host Agent cleanup outbox") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		repoDir := filepath.Join(options.WorkRoot, "runs", assignment.RunID, "repo")
		stateDir := filepath.Join(repoDir, ".maya-stall", "state", "runs", assignment.RunID)
		if _, err := os.Lstat(stateDir); err == nil {
			if err := verifyKeptSessionCleanupIdentity(repoDir, assignment.RunID, assignment.HostID, assignment.BrokerSession); err != nil {
				return true, err
			}
			if err := stopRun(repoDir, assignment.RunID, runtime.Now); err != nil {
				return true, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return true, err
		}
		var cleanupErr error
		if outbox.Cleanup.ExpiryFromState == "confirmed" {
			cleanupErr = errors.New("active Host Lock deadline expired before its Windows Host Agent reported completion") //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
		}
		return true, completeStoppedExpiredHostAgentAssignment(options, assignment, runtime, cleanupErr, stdout)
	case "complete":
		if outbox.Cleanup != nil || outbox.Completion == nil || outbox.Failure != nil {
			return true, fmt.Errorf("invalid Windows Host Agent completion outbox") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		completion := *outbox.Completion
		completion.SessionID = options.SessionID
		var terminal runCommandJSON
		endpoint := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/complete"
		if assignment.Action != "finish" {
			if err := postHostAgentCompletion(options, endpoint, completion, runtime, &terminal); err != nil {
				return true, err
			}
		}
		if err := cleanHostAgentRunRoot(options.WorkRoot, assignment.RunID); err != nil {
			return true, err
		}
		if err := postHostAgentCompletion(options, endpoint, completion, runtime, &terminal); err != nil {
			return true, err
		}
		if err := removeHostAgentMutationOutbox(options.WorkRoot, assignment.RunID); err != nil {
			return true, err
		}
		_, _ = fmt.Fprintf(stdout, "agent: %s\nhost: %s\nrun: %s\nstatus: %s\n", options.AgentID, options.HostID, assignment.RunID, terminal.Status)
		if terminal.Status == resultStatusFailed {
			return true, errors.New("replayed failed Windows Host Agent completion") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		return true, nil
	case "fail":
		if outbox.Cleanup != nil || outbox.Failure == nil || outbox.Completion != nil {
			return true, fmt.Errorf("invalid Windows Host Agent failure outbox") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		failure := *outbox.Failure
		failure.SessionID = options.SessionID
		var terminal runCommandJSON
		endpoint := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/fail"
		if assignment.Action != "finish" {
			if err := postHostAgentMutation(options, endpoint, failure, runtime, &terminal); err != nil {
				return true, err
			}
		}
		if err := cleanHostAgentRunRoot(options.WorkRoot, assignment.RunID); err != nil {
			return true, err
		}
		if err := postHostAgentMutation(options, endpoint, failure, runtime, &terminal); err != nil {
			return true, err
		}
		if err := removeHostAgentMutationOutbox(options.WorkRoot, assignment.RunID); err != nil {
			return true, err
		}
		return true, errors.New(failure.Diagnostic)
	default:
		return true, fmt.Errorf("unsupported Windows Host Agent completion outbox kind %q", outbox.Kind) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
}

func finishHostAgentCompletion(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, completion hostAgentCompletionRequest, runtime runRuntime, stdout io.Writer) error {
	copy := completion
	if err := persistHostAgentMutationOutbox(options, assignment, hostAgentMutationOutbox{
		Kind: "complete", Completion: &copy,
	}); err != nil {
		return err
	}
	var terminal runCommandJSON
	endpoint := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/complete"
	if err := postHostAgentCompletion(options, endpoint, completion, runtime, &terminal); err != nil {
		return err
	}
	if err := cleanHostAgentRunRoot(options.WorkRoot, assignment.RunID); err != nil {
		return err
	}
	if err := postHostAgentCompletion(options, endpoint, completion, runtime, &terminal); err != nil {
		return err
	}
	if err := removeHostAgentMutationOutbox(options.WorkRoot, assignment.RunID); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "agent: %s\nhost: %s\nrun: %s\nstatus: %s\n", options.AgentID, options.HostID, assignment.RunID, terminal.Status)
	if terminal.Status == resultStatusFailed {
		diagnostic := terminal.Diagnostic
		if diagnostic == "" {
			diagnostic = "Windows Host Agent completion failed" //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		return errors.New(diagnostic)
	}
	return nil
}

func runHostAgentOnce(options hostAgentRunOnceOptions, runtime runRuntime, stdout io.Writer) error {
	if err := ensureHostAgentDirectory(options.WorkRoot); err != nil {
		return err
	}
	if err := ensureHostAgentDirectory(filepath.Join(options.WorkRoot, "runs")); err != nil {
		return err
	}
	capabilities, err := hostAgentCapabilityRecord(options, runtime.Now())
	if err != nil {
		return err
	}
	options.Capabilities = capabilities
	registration := hostAgentRegistrationRequest{
		Version: hostAgentAPIVersion, AgentID: options.AgentID, HostID: options.HostID, Slots: 1, SessionBinding: true, DeadlineActions: true,
		Capabilities: capabilities,
	}
	var status hostAgentStatusResponse
	registerPath := "/v1/host-agents/" + options.AgentID + "/register"
	if err := postControlPlaneJSON(options.ControlPlane, options.CredentialEnv, registerPath, registration, runtime, http.StatusOK, &status); err != nil {
		return err
	}
	if status.SessionID == "" {
		return fmt.Errorf("Control Plane did not return a Windows Host Agent session") //nolint:staticcheck // Product terms start the user-facing diagnostic.
	}
	options.SessionID = status.SessionID
	if status.RunID == "" {
		if err := reconcileAcknowledgedHostAgentOutbox(options, runtime, stdout); err != nil {
			return err
		}
	}
	stopHeartbeat, heartbeatErrors, executionCancel := startHostAgentHeartbeat(options, runtime)
	defer stopHeartbeat()
	if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
		return err
	}
	assignment, err := pollHostAgentAssignment(options, runtime, heartbeatErrors)
	if err != nil {
		return err
	}
	if err := validateHostAgentAssignment(options, assignment); err != nil {
		return err
	}
	if replayed, replayErr := replayHostAgentMutationOutbox(options, assignment, runtime, stdout); replayed {
		return replayErr
	}
	if assignment.Action == "finish" {
		return fmt.Errorf("finishing Windows Host Agent assignment has no durable terminal mutation outbox") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if assignment.Action != "" && assignment.Action != "execute" {
		return resumeKeptHostAgentAssignment(options, assignment, runtime, heartbeatErrors, stdout)
	}
	if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
		return err
	}
	lock := hostAgentLockRequest{Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: options.SessionID}
	confirmPath := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/confirm"
	if err := postHostAgentMutation(options, confirmPath, lock, runtime, &status); err != nil {
		return err
	}
	if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "renewing its process-session fence", err)
	}

	runRoot := filepath.Join(options.WorkRoot, "runs", assignment.RunID)
	repoDir := filepath.Join(runRoot, "repo")
	if err := os.RemoveAll(runRoot); err != nil {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "cleaning a stale takeover workspace", err)
	}
	if err := os.MkdirAll(repoDir, 0o700); err != nil {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "preparing its run workspace", err)
	}
	if err := materializeControlPlaneSubmission(repoDir, assignment.Submission); err != nil {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "materializing the submitted snapshot", err)
	}
	if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "renewing its process-session fence", err)
	}
	hostConfigPath, _, err := resolveHostAgentHostConfig(options, assignment)
	if err != nil {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "preparing its Host config", err)
	}
	agentRuntime := runtime
	previousSessionStarted := agentRuntime.SessionStarted
	agentRuntime.Accepted = nil
	agentRuntime.AcceptedCheck = nil
	agentRuntime.ControlPlaneServe = nil
	agentRuntime.Host = nil
	agentRuntime.Broker = nil
	agentRuntime.ReadinessHost = nil
	agentRuntime.ReadinessBroker = nil
	agentRuntime.Cancel = executionCancel
	agentRuntime.SessionStarted = func(session brokerSessionIdentity) error {
		binding := hostAgentSessionRequest{
			Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken,
			SessionID: options.SessionID, BrokerSession: session,
		}
		path := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/session"
		if err := postHostAgentSessionBinding(options, path, binding, runtime, &status, executionCancel); err != nil {
			return err
		}
		if previousSessionStarted != nil {
			return previousSessionStarted(session)
		}
		return nil
	}
	targetProfile := assignment.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	stopProgress := startHostAgentProgress(options, assignment, repoDir, runtime, executionCancel)
	outcome, runErr := runScenario(repoDir, runOptions{
		ScenarioName: assignment.Submission.Scenario, TargetProfile: targetProfile, HostPin: assignment.HostID,
		HostConfig: hostConfigPath, StopAfter: assignment.Submission.StopAfter, KeepTTL: keepTTLOrDefault(assignment.Submission.KeepTTL), AssignedRunID: assignment.RunID, AssignedMayaBuild: assignment.SelectedMayaBuild,
		AssignedEventPrefix: assignment.EventPrefix, AssignedHardDeadline: assignment.HardDeadline,
	}, agentRuntime)
	progressErr := stopProgress()
	operationalErr := errors.Join(runErr, progressErr)
	heartbeatErr := currentHostAgentHeartbeatError(heartbeatErrors)
	if progressErr != nil && isHostLockDeadlineError(progressErr) {
		runErr = errors.Join(runErr, progressErr)
		operationalErr = errors.Join(runErr, heartbeatErr)
	}
	if outcome.StopPolicy == "unresolved" || outcome.Failure != nil && outcome.Failure.CleanupState == "failed" {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "stopping its Maya UI Session", errors.Join(operationalErr, heartbeatErr))
	}
	if heartbeatErr != nil {
		if !isHostLockDeadlineError(heartbeatErr) {
			return failConfirmedHostAgentAssignment(options, assignment, runtime, "renewing its process-session fence", errors.Join(operationalErr, heartbeatErr))
		}
		runErr = errors.Join(runErr, heartbeatErr)
		operationalErr = errors.Join(runErr, progressErr)
	}
	if progressErr != nil && !isHostLockDeadlineError(progressErr) {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "checkpointing its active Run Ledger", operationalErr)
	}
	if outcome.Host != assignment.HostID || outcome.Scenario != assignment.Submission.Scenario || outcome.TargetProfile != targetProfile {
		identityErr := fmt.Errorf("Windows Host Agent run did not reach the assigned Host identity") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "executing the assigned Scenario", errors.Join(runErr, identityErr))
	}
	if outcome.StopPolicy == "kept" {
		keptAction, err := reportHostAgentKeptSession(options, assignment, outcome, runtime, repoDir, []string{repoDir, options.WorkRoot})
		if err != nil {
			return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "recording its Kept Session", errors.Join(runErr, err))
		}
		for keptAction.Action == "wait" {
			if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
				return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "renewing its kept Host Lock", errors.Join(runErr, err))
			}
			time.Sleep(time.Second)
			keptAction, err = pollHostAgentKeptAction(options, assignment, runtime)
			if err != nil {
				return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "waiting for Kept Session expiry", errors.Join(runErr, err))
			}
		}
		if keptAction.Action != "cleanup" {
			return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "reading its Kept Session action", errors.Join(runErr, fmt.Errorf("unsupported Kept Session action %q", keptAction.Action)))
		}
		if keptAction.ExpiryFromState != "kept" && keptAction.ExpiryFromState != "confirmed" {
			return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "reading its Kept Session expiry origin", errors.Join(runErr, fmt.Errorf("unsupported Host Lock expiry origin %q", keptAction.ExpiryFromState)))
		}
		if err := verifyKeptSessionCleanupIdentity(repoDir, assignment.RunID, assignment.HostID, keptAction.BrokerSession); err != nil {
			return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "verifying its retained Maya UI Session identity", errors.Join(runErr, err))
		}
		if keptAction.ExpiryFromState == "kept" {
			if err := syncHostAgentDeadlineEvents(repoDir, assignment.RunID, keptAction.DeadlineEvents); err != nil {
				return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "synchronizing its Host Lock deadline events", errors.Join(runErr, err))
			}
		}
		var expiredErr error
		if keptAction.ExpiryFromState == "confirmed" {
			expiredErr = errors.New("active Host Lock deadline expired before its Windows Host Agent reported completion") //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
			recovered, err := loadAcceptedHostAgentRun(repoDir, hostAgentAssignmentRecord{
				RunID: assignment.RunID, HostID: assignment.HostID, Submission: assignment.Submission,
			}, runtime)
			if err != nil {
				return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "loading its deadline-expired Run state", errors.Join(runErr, err))
			}
			recovered.host.HostID = assignment.HostID
			recovered.manifest.Host = assignment.HostID
			if err := recovered.ensureAcceptedFailureEvidence(expiredErr); err != nil {
				return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "synthesizing its deadline-expired failure evidence", errors.Join(runErr, err))
			}
		}
		if err := persistHostAgentCleanupOutbox(options, assignment, keptAction.BrokerSession, keptAction.ExpiryFromState); err != nil {
			return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "journaling its retained Maya UI Session cleanup", errors.Join(runErr, err))
		}
		if err := stopRun(repoDir, assignment.RunID, runtime.Now); err != nil {
			return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "expiring its exact retained Maya UI Session", errors.Join(runErr, err))
		}
		if expiredErr != nil {
			return completeStoppedExpiredHostAgentAssignment(options, assignment, runtime, expiredErr, stdout)
		}
		outcome.StopPolicy = "stopped"
		outcome.FollowUpCommands = nil
	}
	files, snapshotErr := buildHostAgentResultFilesSanitized(repoDir, assignment.RunID, []string{repoDir, options.WorkRoot})
	if snapshotErr != nil {
		return failConfirmedHostAgentAssignment(options, assignment, runtime, "collecting durable result state", errors.Join(runErr, snapshotErr))
	}
	if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
		if !isHostLockDeadlineError(err) {
			return failConfirmedHostAgentAssignment(options, assignment, runtime, "renewing its process-session fence", errors.Join(runErr, err))
		}
		runErr = errors.Join(runErr, err)
	}
	terminal := controlPlaneTerminalResponse(outcome, runErr, repoDir)
	sanitizeHostAgentTerminal(&terminal, []string{repoDir, options.WorkRoot})
	completion := hostAgentCompletionRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken,
		SessionID: options.SessionID, Terminal: terminal, Files: files,
	}
	if err := finishHostAgentCompletion(options, assignment, completion, runtime, stdout); err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

func resumeKeptHostAgentAssignment(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, runtime runRuntime, heartbeatErrors <-chan error, stdout io.Writer) error {
	if assignment.KeepDeadline == "" {
		return resumeExpiredActiveHostAgentAssignment(options, assignment, runtime)
	}
	action := hostAgentKeptResponse{Action: assignment.Action, DeadlineEvents: assignment.DeadlineEvents}
	var err error
	for action.Action == "wait" {
		if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
			return err
		}
		time.Sleep(time.Second)
		action, err = pollHostAgentKeptAction(options, assignment, runtime)
		if err != nil {
			return err
		}
	}
	if action.Action != "cleanup" {
		return fmt.Errorf("Control Plane returned unsupported Kept Session action %q", action.Action) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	runRoot := filepath.Join(options.WorkRoot, "runs", assignment.RunID)
	repoDir := filepath.Join(runRoot, "repo")
	if _, err := os.Lstat(repoDir); errors.Is(err, os.ErrNotExist) {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "reclaiming an expired retained Host Lock with missing Agent state", errors.New("expired retained assignment has no Agent run workspace"))
	} else if err != nil {
		return err
	}
	if err := verifyKeptSessionCleanupIdentity(repoDir, assignment.RunID, assignment.HostID, assignment.BrokerSession); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "verifying its recovered retained Maya UI Session identity", err)
	}
	if err := syncHostAgentDeadlineEvents(repoDir, assignment.RunID, action.DeadlineEvents); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "synchronizing its Host Lock deadline events", err)
	}
	if err := persistHostAgentCleanupOutbox(options, assignment, assignment.BrokerSession, "kept"); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "journaling its recovered retained Maya UI Session cleanup", err)
	}
	if err := stopRun(repoDir, assignment.RunID, runtime.Now); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "recovering its exact retained Maya UI Session", err)
	}
	return completeStoppedExpiredHostAgentAssignment(options, assignment, runtime, nil, stdout)
}

func resumeExpiredActiveHostAgentAssignment(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, runtime runRuntime) error {
	expiredErr := errors.New("active Host Lock deadline expired before its Windows Host Agent reported completion") //nolint:staticcheck // Product terms preserve the user-facing diagnostic.
	runRoot := filepath.Join(options.WorkRoot, "runs", assignment.RunID)
	repoDir := filepath.Join(runRoot, "repo")
	if assignment.BrokerSession == nil {
		if assignment.ExpiryFromState == "assigned" {
			return failConfirmedHostAgentAssignment(options, assignment, runtime, "reclaiming its unconfirmed expired Host Lock", expiredErr)
		}
		if _, err := os.Lstat(repoDir); errors.Is(err, os.ErrNotExist) {
			return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "recovering an expired active assignment with missing Agent run state", expiredErr)
		} else if err != nil {
			return err
		}
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "recovering an expired active assignment without an exact Maya UI Session identity", expiredErr)
	}
	if _, err := os.Lstat(repoDir); errors.Is(err, os.ErrNotExist) {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "recovering an expired active assignment with missing Agent run state", expiredErr)
	} else if err != nil {
		return err
	}
	if err := verifyKeptSessionCleanupIdentity(repoDir, assignment.RunID, assignment.HostID, assignment.BrokerSession); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "verifying its expired active Maya UI Session identity", err)
	}
	recovered, err := loadAcceptedHostAgentRun(repoDir, hostAgentAssignmentRecord{
		RunID: assignment.RunID, HostID: assignment.HostID, Submission: assignment.Submission,
	}, runtime)
	if err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "loading its expired active Run state", err)
	}
	recovered.host.HostID = assignment.HostID
	recovered.manifest.Host = assignment.HostID
	if err := recovered.ensureAcceptedFailureEvidence(expiredErr); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "synthesizing its expired active failure evidence", err)
	}
	if err := persistHostAgentCleanupOutbox(options, assignment, assignment.BrokerSession, "confirmed"); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "journaling its expired active Maya UI Session cleanup", err)
	}
	if err := stopRun(repoDir, assignment.RunID, runtime.Now); err != nil {
		return quarantineConfirmedHostAgentAssignment(options, assignment, runtime, "stopping its exact expired active Maya UI Session", err)
	}
	return completeStoppedExpiredHostAgentAssignment(options, assignment, runtime, expiredErr, io.Discard)
}

func completeStoppedExpiredHostAgentAssignment(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, runtime runRuntime, runErr error, stdout io.Writer) error {
	repoDir := filepath.Join(options.WorkRoot, "runs", assignment.RunID, "repo")
	record, err := newRunLedgerStore(repoDir).Read(assignment.RunID)
	if err != nil {
		return err
	}
	result, err := readControlPlaneResult(repoDir, record)
	if err != nil {
		return err
	}
	outcome := runOutcome{
		RunID: assignment.RunID, Scenario: record.Scenario, TargetProfile: record.TargetProfile, Host: record.Host,
		StateDir: "/v1/runs/" + assignment.RunID + "/status", EvidenceDir: "/v1/runs/" + assignment.RunID + "/evidence",
		Result: result.Result, Validators: result.Validators, StopPolicy: "stopped", Accepted: true,
	}
	if result.Status == resultStatusFailed && runErr == nil {
		diagnostic := result.Result.Summary
		evidence, evidenceErr := readControlPlaneEvidence(repoDir, record)
		if evidenceErr != nil {
			return evidenceErr
		}
		outcome.Failure = evidence.Bundle.Failure
		if outcome.Failure != nil && outcome.Failure.Diagnostic != "" {
			diagnostic = outcome.Failure.Diagnostic
		}
		if diagnostic == "" {
			diagnostic = "retained Maya Stall run failed before deadline cleanup"
		}
		runErr = errors.New(diagnostic)
	}
	files, err := buildHostAgentResultFilesSanitized(repoDir, assignment.RunID, []string{repoDir, options.WorkRoot})
	if err != nil {
		return err
	}
	terminal := controlPlaneTerminalResponse(outcome, runErr, repoDir)
	sanitizeHostAgentTerminal(&terminal, []string{repoDir, options.WorkRoot})
	completion := hostAgentCompletionRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken,
		SessionID: options.SessionID, Terminal: terminal, Files: files,
	}
	if err := finishHostAgentCompletion(options, assignment, completion, runtime, stdout); err != nil {
		return errors.Join(runErr, err)
	}
	return runErr
}

func verifyKeptSessionCleanupIdentity(repoDir string, runID string, hostID string, expected *brokerSessionIdentity) error {
	if expected == nil || expected.BrokerAdapter == "" || expected.SessionID == "" {
		return fmt.Errorf("shared Host Lock has no exact retained Maya UI Session identity")
	}
	kept, err := readKeptRunState(repoDir, runID)
	if err != nil {
		return err
	}
	if kept.Manifest.RunID != runID || kept.Manifest.Host != hostID || !sameBrokerSession(kept.Manifest.BrokerSession, expected) {
		return fmt.Errorf("retained Maya UI Session identity does not match the shared Host Lock")
	}
	if kept.Record.RunID != runID || kept.Record.Host != hostID || kept.Record.HostConfig.ID != hostID || kept.Record.RemoteSession.BrokerAdapter != expected.BrokerAdapter || kept.Record.RemoteSession.SessionID != expected.SessionID {
		return fmt.Errorf("retained Run Record identity does not match the shared Host Lock")
	}
	return nil
}

func reportHostAgentKeptSession(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, outcome runOutcome, runtime runRuntime, repoDir string, privateRoots []string) (hostAgentKeptResponse, error) {
	result := runCommandJSONForOutcome(outcome)
	sanitizeHostAgentTerminal(&result, privateRoots)
	progress, err := buildHostAgentProgress(repoDir, assignment, options)
	if err != nil {
		return hostAgentKeptResponse{}, err
	}
	request := hostAgentKeptRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken,
		SessionID: options.SessionID, Outcome: result, Progress: progress,
	}
	var response hostAgentKeptResponse
	path := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/kept"
	err = postControlPlaneJSON(options.ControlPlane, options.CredentialEnv, path, request, runtime, http.StatusOK, &response)
	if err != nil {
		return hostAgentKeptResponse{}, err
	}
	if response.Version != hostAgentAPIVersion || response.Kind != "host-agent-kept-action" || response.RunID != assignment.RunID {
		return hostAgentKeptResponse{}, fmt.Errorf("Control Plane returned an unsupported Kept Session response") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	return response, nil
}

func pollHostAgentKeptAction(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, runtime runRuntime) (hostAgentKeptResponse, error) {
	request := hostAgentLockRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: options.SessionID,
	}
	var response hostAgentKeptResponse
	path := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/action"
	err := postControlPlaneJSON(options.ControlPlane, options.CredentialEnv, path, request, runtime, http.StatusOK, &response)
	if err != nil {
		return hostAgentKeptResponse{}, err
	}
	if response.Version != hostAgentAPIVersion || response.Kind != "host-agent-kept-action" || response.RunID != assignment.RunID {
		return hostAgentKeptResponse{}, fmt.Errorf("Control Plane returned an unsupported Kept Session action") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	return response, nil
}

func startHostAgentProgress(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, repoDir string, runtime runRuntime, executionCancel chan<- error) func() error {
	done := make(chan struct{})
	stopped := make(chan struct{})
	errorsOut := make(chan error, 1)
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(hostAgentProgressInterval)
		defer ticker.Stop()
		var acknowledgedFingerprint [sha256.Size]byte
		hasAcknowledgedProgress := false
		progressSnapshotFailures := 0
		nextCheckpoint := int64(1)
		var pending *hostAgentProgressRequest
		var pendingFingerprint [sha256.Size]byte
		for {
			select {
			case <-ticker.C:
				if pending == nil {
					progress, err := buildHostAgentProgress(repoDir, assignment, options)
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					if err != nil {
						if terminalErr := terminalHostAgentProgressSnapshotError(&progressSnapshotFailures, err); terminalErr != nil {
							errorsOut <- terminalErr
							return
						}
						continue
					}
					progressSnapshotFailures = 0
					fingerprint := hostAgentProgressFingerprint(progress)
					if hasAcknowledgedProgress && fingerprint == acknowledgedFingerprint {
						continue
					}
					progress.Checkpoint = nextCheckpoint
					pending = &progress
					pendingFingerprint = fingerprint
				}
				path := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/progress"
				var status hostAgentStatusResponse
				requestContext, cancelRequest := context.WithTimeout(context.Background(), hostAgentHeartbeatRequestTimeout)
				err := postControlPlaneJSONWithLimitContext(requestContext, options.ControlPlane, options.CredentialEnv, path, *pending, runtime, http.StatusOK, &status, maximumControlPlaneSubmissionBytes)
				cancelRequest()
				if err != nil {
					var statusErr *controlPlaneHTTPStatusError
					if errors.As(err, &statusErr) && statusErr.StatusCode >= 400 && statusErr.StatusCode < 500 {
						if isHostLockDeadlineError(err) {
							select {
							case executionCancel <- fmt.Errorf("Windows Host Agent progress deadline: %w", err): //nolint:staticcheck // Product term starts the user-facing diagnostic.
							default:
							}
						}
						errorsOut <- err
						return
					}
					// Heartbeats own Agent liveness; retry transient checkpoint failures on the next bounded tick.
					continue
				}
				acknowledgedFingerprint = pendingFingerprint
				hasAcknowledgedProgress = true
				nextCheckpoint++
				pending = nil
			case <-done:
				return
			}
		}
	}()
	return func() error {
		close(done)
		<-stopped
		select {
		case err := <-errorsOut:
			return fmt.Errorf("checkpoint active Run Ledger: %w", err)
		default:
			return nil
		}
	}
}

func terminalHostAgentProgressSnapshotError(failures *int, snapshotErr error) error {
	*failures++
	if *failures < maximumConsecutiveHostAgentProgressSnapshotFailures {
		return nil
	}
	return snapshotErr
}

func hostAgentProgressFingerprint(progress hostAgentProgressRequest) [sha256.Size]byte {
	digest := sha256.New()
	_, _ = digest.Write(progress.Events)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(progress.Log)
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], digest.Sum(nil))
	return fingerprint
}

type boundedHostAgentProgressArtifacts struct {
	Events          []byte
	Log             []byte
	EventCount      int
	EventsOmitted   int
	EventsTruncated bool
	EventBytes      int
	LogBytes        int
	LogTruncated    bool
}

func boundSanitizedHostAgentProgressArtifacts(temporaryDir string, events []byte, logContent []byte, policy runLedgerPolicy, acceptedAt string, sanitizer hostAgentTextSanitizer) (boundedHostAgentProgressArtifacts, error) {
	eventsSource := filepath.Join(temporaryDir, "sanitized-events-source.jsonl")
	eventsPath := filepath.Join(temporaryDir, "sanitized-events.jsonl")
	if err := writeRunLedgerBytes(eventsSource, []byte(sanitizer.sanitize(string(events)))); err != nil {
		return boundedHostAgentProgressArtifacts{}, err
	}
	eventCount, eventsOmitted, eventsTruncated, eventBytes, err := copyBoundedLedgerEvents(eventsSource, eventsPath, policy.MaxEvents, policy.MaxEventBytes, acceptedAt)
	if err != nil {
		return boundedHostAgentProgressArtifacts{}, err
	}
	boundedEvents, err := os.ReadFile(eventsPath)
	if err != nil {
		return boundedHostAgentProgressArtifacts{}, err
	}
	logSource := filepath.Join(temporaryDir, "sanitized-log-source.txt")
	logPath := filepath.Join(temporaryDir, "sanitized-log.txt")
	if err := writeRunLedgerBytes(logSource, []byte(sanitizer.sanitize(string(logContent)))); err != nil {
		return boundedHostAgentProgressArtifacts{}, err
	}
	logBytes, logTruncated, err := copyBoundedLedgerLog(logSource, logPath, policy.MaxLogBytes)
	if err != nil {
		return boundedHostAgentProgressArtifacts{}, err
	}
	boundedLog, err := os.ReadFile(logPath)
	if err != nil {
		return boundedHostAgentProgressArtifacts{}, err
	}
	return boundedHostAgentProgressArtifacts{
		Events: boundedEvents, Log: boundedLog, EventCount: eventCount, EventsOmitted: eventsOmitted,
		EventsTruncated: eventsTruncated, EventBytes: eventBytes, LogBytes: logBytes, LogTruncated: logTruncated,
	}, nil
}

func buildHostAgentProgress(repoDir string, assignment hostAgentAssignmentResponse, options hostAgentRunOnceOptions) (hostAgentProgressRequest, error) {
	record, err := newRunLedgerStore(repoDir).Read(assignment.RunID)
	if err != nil {
		return hostAgentProgressRequest{}, err
	}
	temporaryDir, err := os.MkdirTemp(options.WorkRoot, ".progress-*")
	if err != nil {
		return hostAgentProgressRequest{}, err
	}
	defer func() { _ = os.RemoveAll(temporaryDir) }()
	policy := hostAgentProgressLedgerPolicy(repoDir)
	eventsPath := filepath.Join(temporaryDir, runLedgerEventsFileName)
	_, _, _, _, err = copyBoundedLedgerEvents(
		filepath.Join(repoDir, ".maya-stall", "state", "runs", assignment.RunID, "events.jsonl"),
		eventsPath, policy.MaxEvents, policy.MaxEventBytes, record.AcceptedAt,
	)
	if err != nil {
		return hostAgentProgressRequest{}, err
	}
	events, err := os.ReadFile(eventsPath)
	if err != nil {
		return hostAgentProgressRequest{}, err
	}
	logPath := filepath.Join(temporaryDir, "session.log")
	logSource := filepath.Join(repoDir, ".maya-stall", "state", "runs", assignment.RunID, filepath.FromSlash(evidenceLogPath))
	_, _, err = copyBoundedLedgerLog(logSource, logPath, policy.MaxLogBytes)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeRunLedgerBytes(logPath, nil); err != nil {
			return hostAgentProgressRequest{}, err
		}
	} else if err != nil {
		return hostAgentProgressRequest{}, err
	}
	logContent, err := os.ReadFile(logPath)
	if err != nil {
		return hostAgentProgressRequest{}, err
	}
	sanitizer := newHostAgentTextSanitizer([]string{repoDir, options.WorkRoot})
	bounded, err := boundSanitizedHostAgentProgressArtifacts(temporaryDir, events, logContent, policy, record.AcceptedAt, sanitizer)
	if err != nil {
		return hostAgentProgressRequest{}, err
	}
	targetProfile := assignment.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	record.TargetProfile = targetProfile
	record.Host = assignment.HostID
	record.State = "submitted"
	record.Status = ""
	record.CompletedAt = ""
	record.EventCount = bounded.EventCount
	record.EventsOmitted = bounded.EventsOmitted
	record.EventsTruncated = bounded.EventsTruncated
	record.EventBytes = bounded.EventBytes
	record.LogBytes = bounded.LogBytes
	record.LogTruncated = bounded.LogTruncated
	return hostAgentProgressRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken, SessionID: options.SessionID,
		Ledger: record, Events: bounded.Events, Log: bounded.Log,
	}, nil
}

func hostAgentProgressLedgerPolicy(repoDir string) runLedgerPolicy {
	policy := defaultRunLedgerPolicy()
	if configured, available := availableRunLedgerPolicy(repoDir); available {
		policy = configured
	}
	if policy.MaxEvents > maximumHostAgentProgressEvents {
		policy.MaxEvents = maximumHostAgentProgressEvents
	}
	if policy.MaxEventBytes > maximumHostAgentProgressEventBytes {
		policy.MaxEventBytes = maximumHostAgentProgressEventBytes
	}
	if policy.MaxLogBytes > maximumHostAgentProgressLogBytes {
		policy.MaxLogBytes = maximumHostAgentProgressLogBytes
	}
	return policy
}

func startHostAgentHeartbeat(options hostAgentRunOnceOptions, runtime runRuntime) (func(), <-chan error, chan error) {
	done := make(chan struct{})
	stopped := make(chan struct{})
	heartbeatErrors := make(chan error, 1)
	executionCancel := make(chan error, 1)
	heartbeatRuntime := runtime
	heartbeatClient := controlPlaneHTTPClient(runtime.ControlPlaneHTTPClient)
	heartbeatClient.Timeout = hostAgentHeartbeatRequestTimeout
	heartbeatRuntime.ControlPlaneHTTPClient = heartbeatClient
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(hostAgentHeartbeatInterval)
		defer ticker.Stop()
		heartbeat := ticker.C
		if heartbeatRuntime.HostAgentHeartbeat != nil {
			heartbeat = heartbeatRuntime.HostAgentHeartbeat
		}
		for {
			select {
			case <-heartbeat:
				var status hostAgentStatusResponse
				capabilities, err := hostAgentCapabilityRecord(options, heartbeatRuntime.Now())
				if err == nil {
					err = postControlPlaneJSON(options.ControlPlane, options.CredentialEnv, "/v1/host-agents/"+options.AgentID+"/heartbeat", hostAgentHeartbeatRequest{
						Version: hostAgentAPIVersion, SessionID: options.SessionID, Capabilities: capabilities,
					}, heartbeatRuntime, http.StatusOK, &status)
				}
				if err == nil && status.State == "cleaning" {
					select {
					case executionCancel <- errors.New("Host Lock deadline expired"): //nolint:staticcheck // Product term starts the user-facing diagnostic.
					default:
					}
					// The accepted heartbeat just renewed one full cleanup lease.
					// Stop renewing so a wedged cleaner becomes replaceable.
					return
				}
				if err != nil {
					heartbeatErr := fmt.Errorf("Windows Host Agent process-session heartbeat: %w", err) //nolint:staticcheck // Product term starts the user-facing diagnostic.
					heartbeatErrors <- heartbeatErr
					select {
					case executionCancel <- heartbeatErr:
					default:
					}
					return
				}
			case <-done:
				return
			}
		}
	}()
	var stoppedOnce bool
	return func() {
		if !stoppedOnce {
			stoppedOnce = true
			close(done)
			<-stopped
		}
	}, heartbeatErrors, executionCancel
}

func currentHostAgentHeartbeatError(heartbeatErrors <-chan error) error {
	select {
	case err := <-heartbeatErrors:
		return err
	default:
		return nil
	}
}

func isHostLockDeadlineError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Host Lock deadline expired")
}

func postHostAgentCompletion(options hostAgentRunOnceOptions, path string, completion hostAgentCompletionRequest, runtime runRuntime, terminal *runCommandJSON) error {
	return postHostAgentMutation(options, path, completion, runtime, terminal)
}

func postHostAgentSessionBinding(options hostAgentRunOnceOptions, path string, binding hostAgentSessionRequest, runtime runRuntime, target any, executionCancel <-chan error) error {
	for {
		requestContext, cancelRequest := context.WithTimeout(context.Background(), hostAgentHeartbeatRequestTimeout)
		err := postControlPlaneJSONWithLimitContext(requestContext, options.ControlPlane, options.CredentialEnv, path, binding, runtime, http.StatusOK, target, maximumControlPlaneSubmissionBytes)
		cancelRequest()
		if err == nil {
			return nil
		}
		var statusErr *controlPlaneHTTPStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode >= 400 && statusErr.StatusCode < 500 {
			return err
		}
		retry := time.NewTimer(time.Second)
		select {
		case cancelErr := <-executionCancel:
			if !retry.Stop() {
				<-retry.C
			}
			return errors.Join(err, cancelErr)
		case <-retry.C:
		}
	}
}

func postHostAgentMutation(options hostAgentRunOnceOptions, path string, body any, runtime runRuntime, target any) error {
	for {
		err := postControlPlaneJSON(options.ControlPlane, options.CredentialEnv, path, body, runtime, http.StatusOK, target)
		if err == nil {
			return nil
		}
		var statusErr *controlPlaneHTTPStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode >= 400 && statusErr.StatusCode < 500 {
			return err
		}
		time.Sleep(time.Second)
	}
}

func pollHostAgentAssignment(options hostAgentRunOnceOptions, runtime runRuntime, heartbeatErrors <-chan error) (hostAgentAssignmentResponse, error) {
	nextPath := "/v1/host-agents/" + options.AgentID + "/assignments/next"
	for {
		if err := currentHostAgentHeartbeatError(heartbeatErrors); err != nil {
			return hostAgentAssignmentResponse{}, err
		}
		var assignment hostAgentAssignmentResponse
		pollContext, cancelPoll := context.WithTimeout(context.Background(), 30*time.Second+hostAgentHeartbeatRequestTimeout)
		watcherDone := make(chan struct{})
		var watchedHeartbeatError error
		go func() {
			defer close(watcherDone)
			select {
			case heartbeatErr := <-heartbeatErrors:
				watchedHeartbeatError = heartbeatErr
				cancelPoll()
			case <-pollContext.Done():
			}
		}()
		err := postControlPlaneJSONWithLimitContext(pollContext, options.ControlPlane, options.CredentialEnv, nextPath, hostAgentNextRequest{Version: hostAgentAPIVersion, SessionID: options.SessionID}, runtime, http.StatusOK, &assignment, maximumControlPlaneSubmissionBytes+1024*1024)
		cancelPoll()
		<-watcherDone
		if watchedHeartbeatError != nil {
			return hostAgentAssignmentResponse{}, watchedHeartbeatError
		}
		if heartbeatErr := currentHostAgentHeartbeatError(heartbeatErrors); heartbeatErr != nil {
			return hostAgentAssignmentResponse{}, heartbeatErr
		}
		var statusErr *controlPlaneHTTPStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNoContent {
			continue
		}
		if err != nil {
			return hostAgentAssignmentResponse{}, err
		}
		return assignment, nil
	}
}

func validateHostAgentAssignment(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse) error {
	if assignment.Version != hostAgentAPIVersion || assignment.Kind != "host-agent-assignment" || assignment.AgentID != options.AgentID || assignment.HostID != options.HostID || validateRunID(assignment.RunID) != nil || assignment.LockToken == "" {
		return fmt.Errorf("Control Plane returned an unsupported Windows Host Agent assignment") //nolint:staticcheck // Product terms start the user-facing diagnostic.
	}
	if assignment.Action != "" && assignment.Action != "execute" && assignment.Action != "wait" && assignment.Action != "cleanup" && assignment.Action != "finish" {
		return fmt.Errorf("Control Plane returned an unsupported Windows Host Agent action") //nolint:staticcheck // Product terms start the user-facing diagnostic.
	}
	if assignment.Capabilities.Version != 0 {
		var config repoRunConfig
		if err := decodeKnownYAMLFields(assignment.Submission.Config, &config); err != nil || config.Version != 1 {
			return fmt.Errorf("Control Plane returned an assignment with invalid Scenario requirements") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		scenario, ok := config.Scenarios[assignment.Submission.Scenario]
		if !ok {
			return fmt.Errorf("Control Plane returned an assignment with unknown Scenario requirements") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		selected, compatible := selectMayaBuild([]string{assignment.Capabilities.Capabilities.SessionMayaBuild}, normalizedScenarioRequirements(scenario).Maya)
		if !compatible || selected != assignment.SelectedMayaBuild {
			return fmt.Errorf("Control Plane selected Maya build %q but the assignment capability snapshot matches %q", assignment.SelectedMayaBuild, selected) //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
	}
	return nil
}

func failConfirmedHostAgentAssignment(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, runtime runRuntime, phase string, runErr error) error {
	failure := hostAgentFailureRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken,
		SessionID: options.SessionID, Diagnostic: "Windows Host Agent failed while " + phase,
	}
	copy := failure
	if err := persistHostAgentMutationOutbox(options, assignment, hostAgentMutationOutbox{
		Kind: "fail", Failure: &copy,
	}); err != nil {
		return errors.Join(runErr, err)
	}
	path := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/fail"
	var terminal runCommandJSON
	failErr := postHostAgentMutation(options, path, failure, runtime, &terminal)
	if failErr == nil {
		failErr = cleanHostAgentRunRoot(options.WorkRoot, assignment.RunID)
	}
	if failErr == nil {
		failErr = postHostAgentMutation(options, path, failure, runtime, &terminal)
	}
	if failErr == nil {
		failErr = removeHostAgentMutationOutbox(options.WorkRoot, assignment.RunID)
	}
	return errors.Join(runErr, failErr)
}

func quarantineConfirmedHostAgentAssignment(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse, runtime runRuntime, phase string, runErr error) error {
	failure := hostAgentFailureRequest{
		Version: hostAgentAPIVersion, RunID: assignment.RunID, LockToken: assignment.LockToken,
		SessionID: options.SessionID, Diagnostic: "Windows Host Agent could not verify Maya session shutdown while " + phase,
		Quarantine: true,
	}
	path := "/v1/host-agents/" + options.AgentID + "/assignments/" + assignment.RunID + "/fail"
	var terminal runCommandJSON
	quarantineErr := postHostAgentMutation(options, path, failure, runtime, &terminal)
	return errors.Join(runErr, quarantineErr, errors.New(failure.Diagnostic))
}

func writeHostAgentFakeHostConfig(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse) (string, error) {
	targetProfile := assignment.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	profileName, err := json.Marshal(targetProfile)
	if err != nil {
		return "", err
	}
	hostID, err := json.Marshal(assignment.HostID)
	if err != nil {
		return "", err
	}
	runRoot := filepath.Join(options.WorkRoot, "runs", assignment.RunID)
	workRoot, err := json.Marshal(filepath.Join(runRoot, "host"))
	if err != nil {
		return "", err
	}
	content := []byte(fmt.Sprintf("version: 1\ntargetProfiles:\n  %s:\n    hostPool: agent\nhostPools:\n  agent:\n    hosts:\n      - id: %s\n        health: healthy\n        workRoot: %s\n", profileName, hostID, workRoot))
	path := filepath.Join(options.WorkRoot, "host-config.yaml")
	if err := writeRunLedgerBytes(path, content); err != nil {
		return "", err
	}
	return path, nil
}

func resolveHostAgentHostConfig(options hostAgentRunOnceOptions, assignment hostAgentAssignmentResponse) (string, runtimeMetadata, error) {
	if options.HostConfig == "" {
		path, err := writeHostAgentFakeHostConfig(options, assignment)
		return path, runtimeMetadata{
			Profile: "fake-local", HostAdapter: "fake", BrokerAdapter: "fake",
			BrokerConfigSource: "generated Host Agent fake config",
		}, err
	}
	content, err := os.ReadFile(options.HostConfig)
	if err != nil {
		return "", runtimeMetadata{}, fmt.Errorf("load Windows Host Agent Host config: %w", err) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	snapshotPath := filepath.Join(options.WorkRoot, "runs", assignment.RunID, "host-config.yaml")
	if err := writeRunLedgerBytes(snapshotPath, content); err != nil {
		return "", runtimeMetadata{}, fmt.Errorf("snapshot Windows Host Agent Host config: %w", err) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	config, err := loadUserHostConfig(snapshotPath)
	if err != nil {
		return "", runtimeMetadata{}, fmt.Errorf("load Windows Host Agent Host config snapshot: %w", err) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	targetProfile := assignment.Submission.TargetProfile
	if targetProfile == "" {
		targetProfile = "default"
	}
	hosts, err := hostCandidates(config, targetProfile, assignment.HostID)
	if err != nil {
		return "", runtimeMetadata{}, err
	}
	resolved, err := resolveLiveHostAgentRuntime(hosts[0])
	if err != nil {
		return "", runtimeMetadata{}, err
	}
	return snapshotPath, resolved.Metadata, nil
}

func resolveLiveHostAgentRuntime(host mayaHostConfig) (resolvedRuntime, error) {
	resolved, err := resolveRuntimeForHost(host)
	if err != nil {
		return resolvedRuntime{}, err
	}
	if !resolved.Metadata.LiveProofEligible {
		return resolvedRuntime{}, fmt.Errorf("Windows Host Agent --host-config must select a live-proof-eligible Maya Host; refusing fake fallback") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	return resolved, nil
}

func buildHostAgentResultFiles(repoDir string, runID string) ([]controlPlaneFile, error) {
	return buildHostAgentResultFilesSanitized(repoDir, runID, nil)
}

func buildHostAgentResultFilesSanitized(repoDir string, runID string, privateRoots []string) ([]controlPlaneFile, error) {
	var files []controlPlaneFile
	seen := make(map[string]bool)
	estimated := int64(4096)
	sanitizer := newHostAgentTextSanitizer(privateRoots)
	for _, root := range []string{
		filepath.ToSlash(filepath.Join(".maya-stall", "state", "ledger", "runs", runID)),
		filepath.ToSlash(filepath.Join("artifacts", "maya-stall", runID)),
	} {
		if err := appendHostAgentResultPath(repoDir, root, sanitizer, &files, seen, &estimated); err != nil {
			return nil, err
		}
	}
	if err := validateHostAgentResultFiles(runID, files); err != nil {
		return nil, err
	}
	return files, nil
}

func appendHostAgentResultPath(repoDir string, relativeRoot string, sanitizer hostAgentTextSanitizer, files *[]controlPlaneFile, seen map[string]bool, estimated *int64) error {
	root := filepath.Join(repoDir, filepath.FromSlash(relativeRoot))
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Windows Host Agent result must not contain symlinks") //nolint:staticcheck // Product term starts the user-facing diagnostic.
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
			if err := addControlPlaneSnapshotEstimate(estimated, 0, relative); err != nil {
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
			return fmt.Errorf("Windows Host Agent result must contain only regular files") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		if err := addControlPlaneSnapshotEstimate(estimated, info.Size(), relative); err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if int64(len(content)) != info.Size() {
			return fmt.Errorf("Windows Host Agent result changed while reading") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
		if len(sanitizer) > 0 && utf8.Valid(content) && !bytes.ContainsRune(content, '\x00') {
			content = []byte(sanitizer.sanitize(string(content)))
		}
		*files = append(*files, controlPlaneFile{Path: relative, Kind: "file", Content: content})
		return nil
	})
}

type hostAgentTextSanitizer []*regexp.Regexp

func newHostAgentTextSanitizer(privateRoots []string) hostAgentTextSanitizer {
	variants := make([]string, 0, len(privateRoots)*3)
	for _, root := range privateRoots {
		if root == "" {
			continue
		}
		variants = append(variants, root, filepath.ToSlash(root), strings.ReplaceAll(root, `\`, `\\`))
	}
	sort.SliceStable(variants, func(left int, right int) bool { return len(variants[left]) > len(variants[right]) })
	sanitizer := make(hostAgentTextSanitizer, 0, len(variants))
	for _, variant := range variants {
		sanitizer = append(sanitizer, regexp.MustCompile(`(?i)`+regexp.QuoteMeta(variant)))
	}
	return sanitizer
}

func (sanitizer hostAgentTextSanitizer) sanitize(value string) string {
	for _, pattern := range sanitizer {
		value = pattern.ReplaceAllStringFunc(value, func(string) string {
			return "[agent-workspace]"
		})
	}
	return value
}

func sanitizeHostAgentResultText(value string, privateRoots []string) string {
	return newHostAgentTextSanitizer(privateRoots).sanitize(value)
}

func sanitizeHostAgentTerminal(terminal *runCommandJSON, privateRoots []string) {
	sanitizer := newHostAgentTextSanitizer(privateRoots)
	terminal.Diagnostic = sanitizer.sanitize(terminal.Diagnostic)
	terminal.RemediationHint = sanitizer.sanitize(terminal.RemediationHint)
	terminal.Error = sanitizer.sanitize(terminal.Error)
	for index := range terminal.FollowUpCommands {
		terminal.FollowUpCommands[index] = sanitizer.sanitize(terminal.FollowUpCommands[index])
	}
}
