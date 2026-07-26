package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

const defaultBrokerCancellationWait = 10 * time.Second

const resultStatusPassed = "passed"
const resultStatusFailed = "failed"
const scenarioResultEnvVar = "MAYA_STALL_SCENARIO_RESULT"
const trustedPluginArtifactsRootEnvVar = "MAYA_STALL_TRUSTED_PLUGIN_ARTIFACTS_ROOT"
const stopAfterSuccess = "success"
const stopAfterFailure = "failure"
const stopAfterAlways = "always"
const stopAfterNever = "never"
const evidenceSchemaVersion = 1

type runRuntime struct {
	Host                   runHost
	Broker                 sessionBroker
	ReadinessHost          runHost
	ReadinessBroker        sessionBroker
	Now                    func() time.Time
	Accepted               func(runOutcome)
	AcceptedCheck          func() error
	SessionStarted         func(brokerSessionIdentity) error
	ControlPlaneHTTPClient *http.Client
	ControlPlaneServe      func(controlPlaneServeOptions, http.Handler) error
	HostAgentHeartbeat     <-chan time.Time
	Cancel                 <-chan error
	CancelWait             time.Duration
}

type runHost interface {
	StagePayload(runContext, []manifestPayload) error
}

type artifactCollector interface {
	CollectArtifacts(runContext, scenarioContract) error
}

type failureArtifactCollector interface {
	CollectFailureArtifacts(runContext, scenarioContract) error
}

type sessionBroker interface {
	// StartFreshSession makes sure the run begins from a clean Maya UI
	// Session instead of inheriting prior broker state, and returns the
	// session identity for run evidence.
	StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error)
	RunScenario(runContext, scenarioConfig) (ScenarioResult, error)
	// StopSession stops the Maya UI Session identified by the given
	// identity so `stopPolicy: stopped` means the session is stopped, not
	// only that the remote workspace is removed. Later callers (kept-session
	// expiry, debug attach) can stop a session by identity through the same
	// method.
	StopSession(runContext, brokerSessionIdentity) error
}

type mayaSessionBuildVerifier interface {
	VerifyMayaBuild(runContext, brokerSessionIdentity, string) error
}

// brokerSessionIdentity identifies the Maya UI Session a Session Broker
// started or stopped for a run. It lands in run evidence as an additive
// `brokerSession` field.
type brokerSessionIdentity struct {
	BrokerAdapter string `json:"brokerAdapter"`
	SessionID     string `json:"sessionId,omitempty"`
}

type runRetentionBroker interface {
	RetentionCapabilities() brokerCapabilities
	RetainRun(runContext, runManifest, string) (retainedSessionRecord, error)
	StatusRetainedRun(runRetentionRecord) (retainedRunStatus, error)
	AttachRetainedRun(runRetentionRecord, io.Writer) error
	StopRetainedRun(runRetentionRecord) error
	CleanupRun(runContext) error
}

type screenshotCapturer interface {
	CaptureScreenshot(runContext, screenshotRequest) (visualEvidenceArtifact, error)
}

type recordingCapturer interface {
	CaptureRecording(runContext, recordingRequest) (visualEvidenceArtifact, error)
}

type recoveredScenarioVisualEvidencePolicy interface {
	CaptureVisualEvidenceAfterRecoveredScenario(error) bool
}

type desktopClicker interface {
	ClickDesktop(desktopClickRequest) error
}

type screenshotRequest struct {
	Name string
}

type recordingRequest struct {
	Name     string
	Duration time.Duration
	FPS      int
}

type desktopClickRequest struct {
	RemoteRoot string
	X          int
	Y          int
}

type runContext struct {
	RepoDir            string
	RunWorkspace       runWorkspace
	StateDir           string
	EvidenceDir        string
	Workspace          string
	EventsPath         string
	LogPath            string
	ScenarioResultPath string
	Environment        map[string]string
}

type runOutcome struct {
	RunID             string
	Scenario          string
	TargetProfile     string
	Host              string
	StateDir          string
	EvidenceDir       string
	Result            ScenarioResult
	Validators        []validatorResult
	StopPolicy        string
	FollowUpCommands  []string
	Accepted          bool
	Failure           *runFailureEvidence
	DurabilityWarning string
	Warnings          []string
}

type runManifest struct {
	Version       int                    `json:"version"`
	RunID         string                 `json:"runId"`
	Scenario      string                 `json:"scenario"`
	TargetProfile string                 `json:"targetProfile"`
	Host          string                 `json:"host"`
	Runtime       runtimeMetadata        `json:"runtime"`
	BrokerSession *brokerSessionIdentity `json:"brokerSession,omitempty"`
	ConfigPath    string                 `json:"configPath"`
	Payload       []manifestPayload      `json:"payload"`
}

type manifestPayload struct {
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Staged string `json:"staged"`
}

type manifestPayloadDeclaration struct {
	Kind   string
	Source string
}

type ScenarioResult struct {
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

type scenarioResultDocument struct {
	Result ScenarioResult
	Fields map[string]any
}

type evidenceBundle struct {
	Version        int                      `json:"version"`
	RunID          string                   `json:"runId"`
	Scenario       string                   `json:"scenario"`
	Status         string                   `json:"status"`
	TargetProfile  string                   `json:"targetProfile"`
	Host           string                   `json:"host"`
	Runtime        runtimeMetadata          `json:"runtime"`
	BrokerSession  *brokerSessionIdentity   `json:"brokerSession,omitempty"`
	Manifest       string                   `json:"manifest"`
	Events         string                   `json:"events"`
	Log            string                   `json:"log"`
	ScenarioResult string                   `json:"scenarioResult,omitempty"`
	Payload        []manifestPayload        `json:"payload"`
	VisualEvidence []visualEvidenceArtifact `json:"visualEvidence,omitempty"`
	Outputs        []outputArtifact         `json:"outputs,omitempty"`
	Artifacts      []evidenceArtifact       `json:"artifacts,omitempty"`
	Validators     []validatorResult        `json:"validators,omitempty"`
	Failure        *runFailureEvidence      `json:"failure,omitempty"`
}

type runFailureEvidence struct {
	FailedLayer     string `json:"failedLayer"`
	Diagnostic      string `json:"diagnostic"`
	RemediationHint string `json:"remediationHint"`
	CaptureState    string `json:"captureState"`
	CleanupState    string `json:"cleanupState"`
}

type visualEvidenceArtifact struct {
	Kind            string  `json:"kind"`
	Path            string  `json:"path"`
	MediaType       string  `json:"mediaType"`
	Origin          string  `json:"origin,omitempty"`
	SHA256          string  `json:"sha256,omitempty"`
	DurationSeconds float64 `json:"durationSeconds,omitempty"`
	FPS             int     `json:"fps,omitempty"`
	TargetProfile   string  `json:"targetProfile,omitempty"`
	Host            string  `json:"host,omitempty"`
}

type outputArtifact struct {
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
}

type validatorResult struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

func runScenario(repoDir string, options runOptions, runtime runRuntime) (outcome runOutcome, err error) {
	return newFreshRun(repoDir, options, runtime).Run()
}

func rejectUnsupportedEvidenceConfig(broker sessionBroker, scenario scenarioConfig) error {
	return nil
}

func recordingDeferredError() error {
	return fmt.Errorf("session broker does not support recording capture")
}

func rejectMismatchedRuntimeOverride(resolved resolvedRuntime, runtime runRuntime) error {
	if resolved.Metadata.Profile != "ssh-sessiond" || runtime.Broker == nil {
		return nil
	}
	if _, ok := runtime.Broker.(ggMayaSessiondBroker); !ok {
		return fmt.Errorf("ssh-sessiond runtime requires gg_mayasessiond Session Broker adapter")
	}
	return nil
}

func rejectInvalidSessionBroker(broker sessionBroker) error {
	switch broker := broker.(type) {
	case invalidSessionBroker:
		return broker.err
	case ggMayaSessiondBroker:
		return broker.validate()
	}
	return nil
}

func isValidStopAfter(value string) bool {
	switch value {
	case stopAfterSuccess, stopAfterFailure, stopAfterAlways, stopAfterNever:
		return true
	default:
		return false
	}
}

func shouldStopAfter(stopAfter string, status string) bool {
	switch stopAfter {
	case stopAfterSuccess:
		return status == resultStatusPassed
	case stopAfterFailure:
		return status != resultStatusPassed
	case stopAfterNever:
		return false
	case stopAfterAlways, "":
		return true
	default:
		return true
	}
}

type runDirOwnership struct {
	StateDir    bool
	EvidenceDir bool
}

func createCleanRunDirs(context runContext) error {
	_, err := createCleanRunDirsWithOwnership(context)
	return err
}

func createCleanRunDirsWithOwnership(context runContext) (runDirOwnership, error) {
	var ownership runDirOwnership
	for _, path := range []string{
		filepath.Join(".maya-stall", "state", "runs"),
		filepath.Join("artifacts", "maya-stall"),
	} {
		if err := ensureOutputPathHasNoSymlinkParent(context.RepoDir, path); err != nil {
			return ownership, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(context.StateDir), 0o755); err != nil {
		return ownership, err
	}
	if err := os.Mkdir(context.StateDir, 0o755); err != nil {
		return ownership, fmt.Errorf("create clean run state: %w", err)
	}
	ownership.StateDir = true
	for _, path := range []string{
		context.Workspace,
		context.RunWorkspace.LocalPayloadRoot(),
		filepath.Dir(context.LogPath),
	} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return ownership, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(context.EvidenceDir), 0o755); err != nil {
		return ownership, err
	}
	if err := os.Mkdir(context.EvidenceDir, 0o755); err != nil {
		return ownership, fmt.Errorf("create clean Evidence directory: %w", err)
	}
	ownership.EvidenceDir = true
	return ownership, nil
}

func snapshotRunPayload(context runContext, payload []manifestPayload) error {
	for _, item := range payload {
		if err := rejectSFTPRepoPath(item.Source); err != nil {
			return fmt.Errorf("snapshot %s payload: %w", item.Kind, err)
		}
		if err := validatePayloadPathForTransport(context.RepoDir, item.Source); err != nil {
			return fmt.Errorf("snapshot %s payload %s: %w", item.Kind, item.Source, err)
		}
	}

	// Copy shallower declarations first. Their frozen tree already contains any
	// explicitly declared descendants, so later overlaps need no second write.
	snapshotPayload := append([]manifestPayload(nil), payload...)
	sort.SliceStable(snapshotPayload, func(left int, right int) bool {
		return strings.Count(snapshotPayload[left].Source, "/") < strings.Count(snapshotPayload[right].Source, "/")
	})
	copiedRoots := make(map[string][]string)
	for _, item := range snapshotPayload {
		covered := false
		for _, root := range copiedRoots[item.Kind] {
			if item.Source == root || strings.HasPrefix(item.Source, root+"/") {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		source := filepath.Join(context.RepoDir, item.Source)
		destination := context.RunWorkspace.LocalPayloadPath(item)
		if err := copyPath(source, destination); err != nil {
			return fmt.Errorf("snapshot %s payload %s: %w", item.Kind, item.Source, err)
		}
		copiedRoots[item.Kind] = append(copiedRoots[item.Kind], item.Source)
	}
	return nil
}

func ensureOutputPathHasNoSymlinkParent(repoDir string, relativePath string) error {
	current := repoDir
	for _, part := range strings.Split(filepath.ToSlash(relativePath), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("output path %s must not be a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("output path %s must be a directory", current)
		}
	}
	return nil
}

func buildManifestPayload(payload runPayload) ([]manifestPayload, error) {
	var manifest []manifestPayload
	for _, declaration := range manifestPayloadDeclarations(payload) {
		item, err := buildManifestPayloadItem(declaration.Kind, declaration.Source)
		if err != nil {
			return nil, err
		}
		manifest = append(manifest, item)
	}
	return manifest, nil
}

func manifestPayloadDeclarations(payload runPayload) []manifestPayloadDeclaration {
	var declarations []manifestPayloadDeclaration
	for _, item := range []struct {
		kind  string
		paths []string
	}{
		{kind: "mayaScripts", paths: append(payload.MayaScripts, payload.Scripts...)},
		{kind: "scenes", paths: payload.Scenes},
		{kind: "pluginArtifacts", paths: payload.PluginArtifacts},
		{kind: "expectedOutputs", paths: payload.ExpectedOutputs},
		{kind: "includePaths", paths: payload.IncludePaths},
	} {
		for _, source := range item.paths {
			declarations = append(declarations, manifestPayloadDeclaration{Kind: item.kind, Source: source})
		}
	}
	return declarations
}

func buildManifestPayloadItem(kind string, source string) (manifestPayload, error) {
	cleanSource, err := cleanRepoRelativePath(source)
	if err != nil {
		return manifestPayload{}, err
	}
	cleanSource = filepath.ToSlash(cleanSource)
	return manifestPayload{
		Kind:   kind,
		Source: cleanSource,
		Staged: filepath.Join("payload", kind, cleanSource),
	}, nil
}

func cleanRepoRelativePath(path string) (string, error) {
	if strings.Contains(path, `\`) {
		return "", fmt.Errorf("repo path %q must use forward slashes, not backslashes", path)
	}
	clean := filepath.Clean(path)
	if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("repo path %q must be repo-relative", path)
	}
	if isReservedPayloadPath(clean) {
		return "", fmt.Errorf("repo path %q is reserved for Maya Stall run state and artifacts", path)
	}
	return clean, nil
}

func isReservedPayloadPath(path string) bool {
	slashed := strings.ToLower(filepath.ToSlash(path))
	for _, reserved := range []string{".maya-stall", "artifacts/maya-stall"} {
		if slashed == reserved ||
			strings.HasPrefix(slashed, reserved+"/") ||
			strings.HasPrefix(reserved, slashed+"/") {
			return true
		}
	}
	return false
}

func newScenarioResultDocument(result ScenarioResult) scenarioResultDocument {
	document := scenarioResultDocument{Fields: make(map[string]any)}
	document.setResult(result)
	return document
}

func readScenarioResultDocument(path string) (scenarioResultDocument, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return scenarioResultDocument{}, false, nil
	}
	if err != nil {
		return scenarioResultDocument{}, false, err
	}
	var fields map[string]any
	if err := decodeJSONUseNumber(content, &fields); err != nil {
		return scenarioResultDocument{}, false, fmt.Errorf("parse Scenario Result %s: %w", path, err)
	}
	if fields == nil {
		return scenarioResultDocument{}, false, fmt.Errorf("Scenario Result %s must be a JSON object", path)
	}
	var result ScenarioResult
	if err := decodeJSONUseNumber(content, &result); err != nil {
		return scenarioResultDocument{}, false, fmt.Errorf("parse Scenario Result %s: %w", path, err)
	}
	return scenarioResultDocument{Result: result, Fields: fields}, true, nil
}

func (document *scenarioResultDocument) setResult(result ScenarioResult) {
	document.Result = result
	document.Fields["status"] = result.Status
	if result.Summary != "" {
		document.Fields["summary"] = result.Summary
	} else {
		delete(document.Fields, "summary")
	}
}

func writeScenarioResult(context runContext, resultPath string, result scenarioResultDocument) error {
	if err := validateScenarioResultPath(context, resultPath); err != nil {
		return err
	}
	workspaceResult := filepath.Join(context.Workspace, resultPath)
	if err := writeJSONFile(workspaceResult, result.Fields); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(context.EvidenceDir, "scenario-result.json"), result.Fields)
}

func validateScenarioResultPath(context runContext, resultPath string) error {
	if err := ensureWorkspacePathHasNoSymlinkAncestor(context.Workspace, resultPath); err != nil {
		return fmt.Errorf("Scenario Result path %q: %w", resultPath, err)
	}
	return nil
}

func validateRunOutputs(context runContext, scenario scenarioContract, result ScenarioResult) ([]validatorResult, error) {
	var results []validatorResult
	for _, validator := range scenario.Config.Validators {
		switch validator.Type {
		case "scenarioResultStatus":
			want := validator.Status
			if want == "" {
				want = resultStatusPassed
			}
			if result.Status == want {
				results = append(results, validatorResult{Type: validator.Type, Status: resultStatusPassed, Message: fmt.Sprintf("Scenario Result status is %q", result.Status)})
			} else {
				results = append(results, validatorResult{Type: validator.Type, Status: resultStatusFailed, Message: fmt.Sprintf("Scenario Result status %q, want %q", result.Status, want)})
			}
		case "outputExists":
			results = append(results, validateOutputExists(context, validator))
		case "jsonEquals":
			results = append(results, validateJSONEquals(context, validator))
		case "numericApprox":
			results = append(results, validateNumericApprox(context, validator))
		case "fileHash":
			results = append(results, validateFileHash(context, validator))
		case "visualEvidence":
			results = append(results, validateVisualEvidence(context, validator))
		default:
			return nil, fmt.Errorf("unknown Validator type %q", validator.Type)
		}
	}
	return results, nil
}

func validateOutputExists(context runContext, validator validatorConfig) validatorResult {
	path, err := validatorWorkspacePath(context, validator)
	if err != nil {
		return failedValidator(validator.Type, err.Error())
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return failedValidator(validator.Type, fmt.Sprintf("required output %q is missing", validator.Path))
	}
	if err != nil {
		return failedValidator(validator.Type, err.Error())
	}
	if info.IsDir() {
		return failedValidator(validator.Type, fmt.Sprintf("required output %q is a directory", validator.Path))
	}
	return passedValidator(validator.Type, fmt.Sprintf("required output %q exists", validator.Path))
}

func validateJSONEquals(context runContext, validator validatorConfig) validatorResult {
	value, result := validatorJSONValue(context, validator)
	if result.Status == resultStatusFailed {
		return result
	}
	if valuesEqual(value, validator.Equals) {
		return passedValidator(validator.Type, fmt.Sprintf("%s equals expected value", validator.JSONPath))
	}
	return failedValidator(validator.Type, fmt.Sprintf("%s = %v, want %v", validator.JSONPath, value, validator.Equals))
}

func validateNumericApprox(context runContext, validator validatorConfig) validatorResult {
	value, result := validatorJSONValue(context, validator)
	if result.Status == resultStatusFailed {
		return result
	}
	if got, want, ok := numericPair(value, validator.Equals); ok {
		if math.Abs(got-want) <= validator.Tolerance {
			return passedValidator(validator.Type, fmt.Sprintf("%s = %v within %v of %v", validator.JSONPath, got, validator.Tolerance, want))
		}
		return failedValidator(validator.Type, fmt.Sprintf("%s = %v, want %v +/- %v", validator.JSONPath, got, want, validator.Tolerance))
	}
	gotArray, gotOK := numericArray(value)
	wantArray, wantOK := numericArray(validator.Equals)
	if !gotOK || !wantOK {
		return failedValidator(validator.Type, "numericApprox values must be numeric scalars or arrays")
	}
	if len(gotArray) != len(wantArray) {
		return failedValidator(validator.Type, fmt.Sprintf("%s length = %d, want %d", validator.JSONPath, len(gotArray), len(wantArray)))
	}
	for index := range gotArray {
		if math.Abs(gotArray[index]-wantArray[index]) > validator.Tolerance {
			return failedValidator(validator.Type, fmt.Sprintf("%s[%d] = %v, want %v +/- %v", validator.JSONPath, index, gotArray[index], wantArray[index], validator.Tolerance))
		}
	}
	return passedValidator(validator.Type, fmt.Sprintf("%s numeric array within %v", validator.JSONPath, validator.Tolerance))
}

func validateFileHash(context runContext, validator validatorConfig) validatorResult {
	path, err := validatorWorkspacePath(context, validator)
	if err != nil {
		return failedValidator(validator.Type, err.Error())
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return failedValidator(validator.Type, fmt.Sprintf("hashed file %q is missing", validator.Path))
	}
	if err != nil {
		return failedValidator(validator.Type, err.Error())
	}
	hash := fmt.Sprintf("%x", sha256.Sum256(content))
	if strings.EqualFold(hash, validator.SHA256) {
		return passedValidator(validator.Type, fmt.Sprintf("%q sha256 matches", validator.Path))
	}
	return failedValidator(validator.Type, fmt.Sprintf("%q sha256 %s, want %s", validator.Path, hash, validator.SHA256))
}

func validateVisualEvidence(context runContext, validator validatorConfig) validatorResult {
	required := true
	if validator.Required != nil {
		required = *validator.Required
	}
	if !required {
		return passedValidator(validator.Type, "Visual Evidence not required")
	}
	for _, dir := range []string{"screenshots", "recordings", "visual"} {
		found, err := hasRegularFile(filepath.Join(context.EvidenceDir, dir))
		if err != nil {
			return failedValidator(validator.Type, err.Error())
		}
		if found {
			return passedValidator(validator.Type, "Visual Evidence present")
		}
	}
	return failedValidator(validator.Type, "Visual Evidence is missing")
}

func validatorWorkspacePath(context runContext, validator validatorConfig) (string, error) {
	if validator.Path == "" {
		return "", fmt.Errorf("%s Validator missing path", validator.Type)
	}
	path, err := cleanRepoRelativePath(validator.Path)
	if err != nil {
		return "", err
	}
	if err := ensureWorkspacePathHasNoSymlinkAncestor(context.Workspace, path); err != nil {
		return "", err
	}
	return filepath.Join(context.Workspace, path), nil
}

func ensureWorkspacePathHasNoSymlinkAncestor(workspace string, relativePath string) error {
	current := workspace
	for _, part := range strings.Split(filepath.ToSlash(relativePath), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("Validator output path %s must not be or contain a symlink", current)
		}
	}
	return nil
}

func validatorJSONValue(context runContext, validator validatorConfig) (any, validatorResult) {
	path, err := validatorWorkspacePath(context, validator)
	if err != nil {
		return nil, failedValidator(validator.Type, err.Error())
	}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, failedValidator(validator.Type, fmt.Sprintf("JSON file %q is missing", validator.Path))
	}
	if err != nil {
		return nil, failedValidator(validator.Type, err.Error())
	}
	var document any
	if err := decodeJSONUseNumber(content, &document); err != nil {
		return nil, failedValidator(validator.Type, fmt.Sprintf("parse JSON %q: %v", validator.Path, err))
	}
	value, ok := lookupJSONPath(document, validator.JSONPath)
	if !ok {
		return nil, failedValidator(validator.Type, fmt.Sprintf("JSON path %q is missing", validator.JSONPath))
	}
	return value, passedValidator(validator.Type, "")
}

func lookupJSONPath(document any, path string) (any, bool) {
	if path == "$" {
		return document, true
	}
	if !strings.HasPrefix(path, "$.") {
		return nil, false
	}
	current := document
	for _, part := range strings.Split(strings.TrimPrefix(path, "$."), ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func valuesEqual(got any, want any) bool {
	gotNumber, gotNumeric := numberValue(got)
	wantNumber, wantNumeric := numberValue(want)
	if gotNumeric && wantNumeric {
		return gotNumber == wantNumber
	}
	gotSlice, gotIsSlice := got.([]any)
	wantSlice, wantIsSlice := want.([]any)
	if gotIsSlice || wantIsSlice {
		if !gotIsSlice || !wantIsSlice || len(gotSlice) != len(wantSlice) {
			return false
		}
		for index := range gotSlice {
			if !valuesEqual(gotSlice[index], wantSlice[index]) {
				return false
			}
		}
		return true
	}
	gotMap, gotIsMap := got.(map[string]any)
	wantMap, wantIsMap := want.(map[string]any)
	if gotIsMap || wantIsMap {
		if !gotIsMap || !wantIsMap || len(gotMap) != len(wantMap) {
			return false
		}
		for key, gotValue := range gotMap {
			wantValue, ok := wantMap[key]
			if !ok || !valuesEqual(gotValue, wantValue) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(got, want)
}

func numberValue(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case float64:
		return number, true
	case float32:
		return float64(number), true
	case json.Number:
		parsed, err := number.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func numericPair(got any, want any) (float64, float64, bool) {
	gotNumber, gotOK := numberValue(got)
	wantNumber, wantOK := numberValue(want)
	return gotNumber, wantNumber, gotOK && wantOK
}

func numericArray(value any) ([]float64, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	numbers := make([]float64, 0, len(items))
	for _, item := range items {
		number, ok := numberValue(item)
		if !ok {
			return nil, false
		}
		numbers = append(numbers, number)
	}
	return numbers, true
}

func sha256HexOfBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func fileSHA256Hex(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
	}()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hasRegularFile(dir string) (bool, error) {
	errFound := errors.New("found regular file")
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode().IsRegular() {
				return errFound
			}
		}
		return nil
	})
	if errors.Is(err, errFound) {
		return true, nil
	}
	return false, err
}

func passedValidator(validatorType string, message string) validatorResult {
	return validatorResult{Type: validatorType, Status: resultStatusPassed, Message: message}
}

func failedValidator(validatorType string, message string) validatorResult {
	return validatorResult{Type: validatorType, Status: resultStatusFailed, Message: message}
}

func hasValidatorFailure(results []validatorResult) bool {
	for _, result := range results {
		if result.Status != resultStatusPassed {
			return true
		}
	}
	return false
}

func writeEvidenceBundle(context runContext, manifest runManifest, scenario scenarioContract, result ScenarioResult, capturedVisualEvidence []visualEvidenceArtifact, validators []validatorResult) error {
	outputs, err := copyEvidenceOutputs(context, scenario)
	if err != nil {
		return err
	}
	if err := copyFile(filepath.Join(context.StateDir, "manifest.json"), filepath.Join(context.EvidenceDir, "manifest.json")); err != nil {
		return err
	}
	if err := copySequencedEvents(context.EventsPath, filepath.Join(context.EvidenceDir, evidenceEventsFileName)); err != nil {
		return err
	}
	if err := copyFile(context.LogPath, filepath.Join(context.EvidenceDir, "logs", "session.log")); err != nil {
		return err
	}
	visualEvidence, err := discoverVisualEvidence(context.EvidenceDir)
	if err != nil {
		return err
	}
	visualEvidence = mergeVisualEvidence(capturedVisualEvidence, visualEvidence)
	if err := rejectNonBrokerVisualEvidenceForLiveProof(manifest.Runtime, visualEvidence); err != nil {
		return err
	}
	bundle := evidenceBundle{
		Version:        evidenceSchemaVersion,
		RunID:          manifest.RunID,
		Scenario:       manifest.Scenario,
		Status:         result.Status,
		TargetProfile:  manifest.TargetProfile,
		Host:           manifest.Host,
		Runtime:        manifest.Runtime,
		BrokerSession:  manifest.BrokerSession,
		Manifest:       evidenceManifestFileName,
		Events:         evidenceEventsFileName,
		Log:            evidenceLogPath,
		ScenarioResult: evidenceScenarioResultFileName,
		Payload:        manifest.Payload,
		VisualEvidence: visualEvidence,
		Outputs:        outputs,
		Validators:     validators,
	}
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	return writeJSONFile(filepath.Join(context.EvidenceDir, evidenceBundleFileName), bundle)
}

func rejectNonBrokerVisualEvidenceForLiveProof(runtime runtimeMetadata, artifacts []visualEvidenceArtifact) error {
	if !runtime.LiveProofEligible {
		return nil
	}
	for _, artifact := range artifacts {
		if artifact.Origin != visualEvidenceOriginBrokerCapture {
			return fmt.Errorf("live-proof-eligible Evidence Bundle requires Session Broker captured Visual Evidence: %s has origin %q", artifact.Path, visualEvidenceOriginLabel(artifact.Origin))
		}
	}
	return nil
}

func visualEvidenceOriginLabel(origin string) string {
	if origin == "" {
		return "unknown"
	}
	return origin
}

func mergeVisualEvidence(preferred []visualEvidenceArtifact, discovered []visualEvidenceArtifact) []visualEvidenceArtifact {
	seen := make(map[string]bool)
	artifacts := make([]visualEvidenceArtifact, 0, len(preferred)+len(discovered))
	add := func(artifact visualEvidenceArtifact) {
		artifact.Path = filepath.ToSlash(artifact.Path)
		if artifact.Path == "" || seen[artifact.Path] {
			return
		}
		seen[artifact.Path] = true
		artifacts = append(artifacts, artifact)
	}
	for _, artifact := range preferred {
		add(artifact)
	}
	for _, artifact := range discovered {
		add(artifact)
	}
	sort.Slice(artifacts, func(i, j int) bool {
		return artifacts[i].Path < artifacts[j].Path
	})
	return artifacts
}

func copyEvidenceOutputs(context runContext, scenario scenarioContract) ([]outputArtifact, error) {
	seen := make(map[string]bool)
	var outputs []outputArtifact
	if err := copyEvidenceOutputDir(context, "outputs", seen, &outputs); err != nil {
		return nil, err
	}
	for _, output := range scenario.Outputs {
		if err := copyEvidenceOutputPath(context, output.Path, seen, &outputs); err != nil {
			return nil, err
		}
	}
	sort.Slice(outputs, func(i, j int) bool {
		return outputs[i].Path < outputs[j].Path
	})
	return outputs, nil
}

func copyEvidenceOutputPath(context runContext, relativePath string, seen map[string]bool, outputs *[]outputArtifact) error {
	if relativePath == "" {
		return nil
	}
	clean, err := cleanRepoRelativePath(relativePath)
	if err != nil {
		return nil
	}
	if err := ensureWorkspacePathHasNoSymlinkAncestor(context.Workspace, clean); err != nil {
		return nil
	}
	source := filepath.Join(context.Workspace, clean)
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if info.IsDir() {
		return copyEvidenceOutputDir(context, clean, seen, outputs)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	return copyEvidenceOutputFile(context, source, clean, seen, outputs)
}

func copyEvidenceOutputDir(context runContext, relativePath string, seen map[string]bool, outputs *[]outputArtifact) error {
	source := filepath.Join(context.Workspace, relativePath)
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyEvidenceOutputFile(context, path, filepath.Join(relativePath, relative), seen, outputs)
	})
}

func copyEvidenceOutputFile(context runContext, source string, relativePath string, seen map[string]bool, outputs *[]outputArtifact) error {
	clean := filepath.ToSlash(filepath.Clean(relativePath))
	if isReservedEvidenceArtifactPath(clean) {
		return nil
	}
	if seen[clean] {
		return nil
	}
	destination := filepath.Join(context.EvidenceDir, filepath.FromSlash(clean))
	if err := copyFile(source, destination); err != nil {
		return err
	}
	seen[clean] = true
	*outputs = append(*outputs, outputArtifact{Path: clean, MediaType: mediaTypeForPath(clean)})
	return nil
}

func discoverVisualEvidence(evidenceDir string) ([]visualEvidenceArtifact, error) {
	var artifacts []visualEvidenceArtifact
	for _, spec := range []struct {
		dir       string
		kind      string
		mediaType string
	}{
		{dir: "screenshots", kind: "screenshot"},
		{dir: "recordings", kind: "recording", mediaType: "video/mp4"},
	} {
		root := filepath.Join(evidenceDir, spec.dir)
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil || entry.IsDir() {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			relative, err := filepath.Rel(evidenceDir, path)
			if err != nil {
				return err
			}
			hash, err := fileSHA256Hex(path)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, visualEvidenceArtifact{
				Kind:      spec.kind,
				Path:      filepath.ToSlash(relative),
				MediaType: visualEvidenceMediaType(spec.mediaType, relative),
				Origin:    visualEvidenceOriginDiscovered,
				SHA256:    hash,
			})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("discover %s Visual Evidence: %w", spec.kind, err)
		}
	}
	return artifacts, nil
}

func visualEvidenceMediaType(defaultType string, relativePath string) string {
	if defaultType != "" {
		return defaultType
	}
	return mediaTypeForPath(relativePath)
}

func repoRelativePath(repoDir string, path string) string {
	relative, err := filepath.Rel(repoDir, path)
	if err != nil {
		return path
	}
	return relative
}

func appendEvent(path string, event string, detail string) error {
	return appendEventRecord(path, map[string]string{
		"event":  event,
		"detail": detail,
	})
}

func appendEventRecord(path string, record map[string]string) error {
	content, err := json.Marshal(newRunEventRecord(record))
	if err != nil {
		return err
	}
	if err := rejectExistingFileLeaf(path); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(content, '\n'))
	return err
}

func installAssignedEventPrefix(path string, content []byte) error {
	if len(content) > maximumHostAgentProgressEventBytes || runLedgerEventLineCount(content) > maximumHostAgentProgressEvents {
		return errors.New("assigned event prefix exceeds Windows Host Agent limits")
	}
	sequence := 0
	lastType := ""
	for _, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return errors.New("assigned event prefix is invalid")
		}
		sequence++
		if ledgerEventSequence(event) != sequence {
			return errors.New("assigned event prefix sequence is invalid")
		}
		if sequence == 1 && event["type"] != "run.accepted" {
			return errors.New("assigned event prefix does not begin with run.accepted")
		}
		lastType = fmt.Sprint(event["type"])
	}
	if sequence == 0 {
		return errors.New("assigned event prefix is empty")
	}
	if lastType != "run.queued" {
		return errors.New("assigned event prefix does not end with run.queued")
	}
	return writeRunLedgerBytes(path, append(bytes.TrimSpace(content), '\n'))
}

func newRunEventRecord(record map[string]string) map[string]any {
	eventType := record["event"]
	details := make(map[string]any)
	structured := map[string]any{
		"event":     eventType,
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"phase":     runEventPhase(eventType),
		"type":      eventType,
		"stream":    "lifecycle",
		"details":   details,
	}
	for key, value := range record {
		if key == "event" {
			continue
		}
		structured[key] = value
		if key == "detail" {
			details["message"] = value
		} else {
			details[key] = value
		}
	}
	return structured
}

func runEventPhase(eventType string) string {
	switch {
	case strings.HasPrefix(eventType, "run.accepted"):
		return "submission"
	case strings.HasPrefix(eventType, "run.queued"):
		return "queuing"
	case strings.HasPrefix(eventType, "run.canceled"):
		return "finalizing"
	case strings.HasPrefix(eventType, "run.started"), strings.HasPrefix(eventType, "broker.session.fresh"):
		return "launching"
	case strings.HasPrefix(eventType, "broker.session.started"):
		return "executing"
	case strings.HasPrefix(eventType, "visual-evidence"), strings.HasPrefix(eventType, "broker.screenshot"), strings.HasPrefix(eventType, "broker.recording"):
		return "collecting"
	case strings.HasPrefix(eventType, "broker.session.stopped"):
		return "cleaning"
	case strings.HasPrefix(eventType, "run.completed"), strings.HasPrefix(eventType, "run.failed"):
		return "finalizing"
	case strings.HasPrefix(eventType, "attach"):
		return "debugging"
	default:
		return "lifecycle"
	}
}

func copySequencedEvents(source string, destination string, fallbackTimestamps ...string) error {
	fallbackTimestamp := time.Now().UTC().Format(time.RFC3339Nano)
	if len(fallbackTimestamps) > 0 && fallbackTimestamps[0] != "" {
		fallbackTimestamp = fallbackTimestamps[0]
	}
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	var sequenced bytes.Buffer
	sequence := 0
	for index, line := range bytes.Split(bytes.TrimSpace(content), []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("parse run event %d: %w", index+1, err)
		}
		sequence++
		event = normalizeRunLedgerEvent(event, sequence, fallbackTimestamp)
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		sequenced.Write(encoded)
		sequenced.WriteByte('\n')
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	if err := rejectExistingFileLeaf(destination); err != nil {
		return err
	}
	return os.WriteFile(destination, sequenced.Bytes(), 0o644)
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := rejectExistingFileLeaf(path); err != nil {
		return err
	}
	return os.WriteFile(path, append(content, '\n'), 0o644)
}

func rejectExistingFileLeaf(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	return nil
}

func decodeJSONUseNumber(content []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

type fakeHost struct {
	SSHStatus string
}

func (fakeHost) ValidateTransportConfig() error { return nil }

func (host fakeHost) ProbeTransport(time.Duration) error {
	return validateFakeTransportStatus(host.SSHStatus)
}

func validateFakeTransportStatus(status string) error {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "ok", "healthy", "reachable":
		return nil
	default:
		return fmt.Errorf("fake SSH transport is %q", strings.TrimSpace(status))
	}
}

func (fakeHost) StagePayload(context runContext, payload []manifestPayload) error {
	return validatePayloadSnapshotForStage(context, payload)
}

func ensurePayloadPathHasNoSymlinkAncestor(repoDir string, relativePath string) error {
	current := repoDir
	for _, part := range strings.Split(filepath.ToSlash(relativePath), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("payload path %s must not be or contain a symlink", current)
		}
	}
	return nil
}

// fakeSessionLifecycle implements the fake Session Broker session lifecycle.
// It is embeddable so test brokers share the same fresh-session and stop
// behavior as fakeSessionBroker.
type fakeSessionLifecycle struct{}

func (fakeSessionLifecycle) ProbeSessionBroker(time.Duration) error { return nil }

func (fakeSessionLifecycle) StartFreshSession(context runContext, scenario scenarioConfig) (brokerSessionIdentity, error) {
	identity := brokerSessionIdentity{
		BrokerAdapter: "fake",
		SessionID:     "fake-" + context.RunWorkspace.RunID(),
	}
	if err := appendEvent(context.EventsPath, "broker.session.fresh", identity.SessionID); err != nil {
		return brokerSessionIdentity{}, err
	}
	return identity, nil
}

func (fakeSessionLifecycle) StopSession(context runContext, session brokerSessionIdentity) error {
	return appendEvent(context.EventsPath, "broker.session.stopped", session.SessionID)
}

type fakeSessionBroker struct {
	fakeSessionLifecycle
	Result ScenarioResult
}

func (broker fakeSessionBroker) RunScenario(context runContext, scenario scenarioConfig) (ScenarioResult, error) {
	if err := appendEvent(context.EventsPath, "broker.session.started", scenario.MayaVersion); err != nil {
		return ScenarioResult{}, err
	}
	log := fmt.Sprintf("fake Session Broker ran Scenario for Maya %s\n", scenario.MayaVersion)
	if err := os.WriteFile(context.LogPath, []byte(log), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	if err := appendEvent(context.EventsPath, "broker.session.finished", broker.Result.Status); err != nil {
		return ScenarioResult{}, err
	}
	return broker.Result, nil
}

func (fakeSessionBroker) VerifyMayaBuild(runContext, brokerSessionIdentity, string) error {
	return nil
}

func (fakeSessionBroker) RetentionCapabilities() brokerCapabilities {
	return brokerCapabilities{
		RetainOnFailure:          true,
		StatusRetainedSession:    true,
		AttachLogObservation:     true,
		StopRetainedSession:      true,
		CleanupRetainedWorkspace: true,
	}
}

func (fakeSessionBroker) RetainRun(context runContext, manifest runManifest, reason string) (retainedSessionRecord, error) {
	return retainedSessionRecord{
		BrokerAdapter: "fake",
		SessionID:     "fake-" + manifest.RunID,
		Status:        "running",
		Metadata: map[string]any{
			"reason":          reason,
			"remoteWorkspace": context.RunWorkspace.RemoteWorkspace(),
		},
	}, nil
}

func (fakeSessionBroker) StatusRetainedRun(record runRetentionRecord) (retainedRunStatus, error) {
	return retainedRunStatus{
		State:           "kept",
		Detail:          "fake Session Broker retained this run",
		BrokerStatus:    "running",
		SessionID:       record.RemoteSession.SessionID,
		RemoteWorkspace: record.RemoteWorkspace,
	}, nil
}

func (fakeSessionBroker) AttachRetainedRun(record runRetentionRecord, stdout io.Writer) error {
	fmt.Fprintf(stdout, "broker:\n")
	fmt.Fprintf(stdout, "adapter: fake\n")
	fmt.Fprintf(stdout, "session: %s\n", record.RemoteSession.SessionID)
	fmt.Fprintf(stdout, "remoteWorkspace: %s\n", record.RemoteWorkspace)
	return nil
}

func (fakeSessionBroker) StopRetainedRun(runRetentionRecord) error {
	return nil
}

func (fakeSessionBroker) CleanupRun(runContext) error {
	return nil
}

func (fakeSessionBroker) CaptureScreenshot(context runContext, request screenshotRequest) (visualEvidenceArtifact, error) {
	name := request.Name
	if name == "" {
		name = evidenceDefaultScreenshotName
	}
	if err := appendVisualEvidenceCaptureRequested(context, "screenshot", visualEvidenceOriginFakeBrokerCapture, name); err != nil {
		return visualEvidenceArtifact{}, err
	}
	return registerVisualEvidenceBytes(context, "screenshot", visualEvidenceOriginFakeBrokerCapture, name, "image/png", []byte("fake screenshot\n"))
}

func (fakeSessionBroker) CaptureRecording(context runContext, request recordingRequest) (visualEvidenceArtifact, error) {
	name := request.Name
	if name == "" {
		name = evidenceDefaultRecordingName
	}
	if err := appendVisualEvidenceCaptureRequested(context, "recording", visualEvidenceOriginFakeBrokerCapture, name); err != nil {
		return visualEvidenceArtifact{}, err
	}
	content := []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2', 'f', 'a', 'k', 'e', '\n'}
	return registerVisualEvidenceBytes(context, "recording", visualEvidenceOriginFakeBrokerCapture, name, "video/mp4", content)
}

func (fakeSessionBroker) ClickDesktop(desktopClickRequest) error {
	return nil
}

type invalidSessionBroker struct {
	err error
}

func (broker invalidSessionBroker) StartFreshSession(runContext, scenarioConfig) (brokerSessionIdentity, error) {
	return brokerSessionIdentity{}, broker.err
}

func (broker invalidSessionBroker) RunScenario(runContext, scenarioConfig) (ScenarioResult, error) {
	return ScenarioResult{}, broker.err
}

func (broker invalidSessionBroker) StopSession(runContext, brokerSessionIdentity) error {
	return broker.err
}

func (broker invalidSessionBroker) CaptureScreenshot(runContext, screenshotRequest) (visualEvidenceArtifact, error) {
	return visualEvidenceArtifact{}, broker.err
}

func (broker invalidSessionBroker) CaptureRecording(runContext, recordingRequest) (visualEvidenceArtifact, error) {
	return visualEvidenceArtifact{}, broker.err
}

func (broker invalidSessionBroker) ClickDesktop(desktopClickRequest) error {
	return broker.err
}

func sessionBrokerDisplayName(broker sessionBroker) string {
	switch broker.(type) {
	case ggMayaSessiondBroker:
		return "gg_mayasessiond Session Broker"
	default:
		return "fake Session Broker"
	}
}

func copyPath(source string, destination string) error {
	linkInfo, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("payload path must not be a symlink")
	}
	info, err := os.Stat(source)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("payload path must not contain symlink %s", path)
			}
			if !entry.IsDir() {
				entryInfo, err := entry.Info()
				if err != nil {
					return err
				}
				if !entryInfo.Mode().IsRegular() {
					return fmt.Errorf("payload path %s must be a regular file", path)
				}
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			target := filepath.Join(destination, relative)
			if entry.IsDir() {
				return os.MkdirAll(target, 0o755)
			}
			return copyFile(path, target)
		})
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("payload path %s must be a regular file", source)
	}
	return copyFile(source, destination)
}

func copyFile(source string, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer output.Close()
	_, err = io.Copy(output, input)
	return err
}
