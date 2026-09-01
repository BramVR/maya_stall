package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	windowsAbsoluteReportPath = regexp.MustCompile(`(?i)(?:[a-z]:[\\/]|\\\\)[^<>"'\r\n,;)\]}]*`)
	posixAbsoluteReportPath   = regexp.MustCompile(`(?:^|[^A-Za-z0-9_./-])/(?:[^<>"'\r\n,;)\]}]*)`)
)

const (
	evidenceReportFileName = "report.html"
	reportMediaType        = "text/html"
	reportViewVersion      = 1
	maximumReportBytes     = 512 * 1024
	maximumReportExcerpt   = 4096
	maximumReportText      = 512
	maximumReportItems     = 50
	maximumReportArtifacts = 200
)

type reportTerminalState struct {
	Lifecycle       string
	Cleanup         string
	Confidentiality string
	Next            string
}

type reportView struct {
	Version         int                     `json:"version"`
	Verdict         string                  `json:"verdict"`
	Status          string                  `json:"status"`
	FailureCategory string                  `json:"failureCategory,omitempty"`
	Scenario        string                  `json:"scenario"`
	RunID           string                  `json:"runId"`
	Revision        string                  `json:"revision"`
	TargetProfile   string                  `json:"targetProfile"`
	MayaIdentity    string                  `json:"mayaIdentity"`
	PluginIdentity  string                  `json:"pluginIdentity"`
	Duration        string                  `json:"duration"`
	Lifecycle       string                  `json:"lifecycle"`
	Cleanup         string                  `json:"cleanup"`
	Confidentiality string                  `json:"confidentiality"`
	Summary         string                  `json:"summary,omitempty"`
	Failure         reportFailureView       `json:"failure"`
	WorkflowState   string                  `json:"workflowState"`
	Steps           []reportStepView        `json:"steps,omitempty"`
	Assertions      []reportAssertionView   `json:"assertions,omitempty"`
	Measurements    []reportMeasurementView `json:"measurements,omitempty"`
	Validators      []validatorResult       `json:"validators,omitempty"`
	Artifacts       []reportArtifactView    `json:"artifacts,omitempty"`
	Counts          reportCounts            `json:"counts"`
	SchemaVersions  reportSchemaVersions    `json:"schemaVersions"`
	MissingEvidence []string                `json:"missingEvidence,omitempty"`
	Truncated       bool                    `json:"truncated"`
	NextCommand     string                  `json:"nextCommand,omitempty"`
}

type reportFailureView struct {
	Phase       string `json:"phase,omitempty"`
	Item        string `json:"item,omitempty"`
	Excerpt     string `json:"excerpt,omitempty"`
	Retryable   bool   `json:"retryable"`
	Remediation string `json:"remediation,omitempty"`
}

type reportStepView struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

type reportAssertionView struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Summary string `json:"summary,omitempty"`
}

type reportMeasurementView struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	Unit      string `json:"unit,omitempty"`
	Threshold string `json:"threshold,omitempty"`
	Passed    string `json:"passed,omitempty"`
}

type reportArtifactView struct {
	Label          string `json:"label"`
	Kind           string `json:"kind"`
	Path           string `json:"path"`
	Link           string `json:"link,omitempty"`
	MediaType      string `json:"mediaType"`
	Size           int64  `json:"size"`
	RecordedSize   int64  `json:"recordedSize,omitempty"`
	SHA256         string `json:"sha256"`
	RecordedSHA256 string `json:"recordedSha256,omitempty"`
	Origin         string `json:"origin,omitempty"`
	State          string `json:"state"`
}

type reportCounts struct {
	Assertions        int `json:"assertions"`
	FailedAssertions  int `json:"failedAssertions"`
	UnknownAssertions int `json:"unknownAssertions"`
	Measurements      int `json:"measurements"`
	Validators        int `json:"validators"`
	FailedValidators  int `json:"failedValidators"`
	UnknownValidators int `json:"unknownValidators"`
	Artifacts         int `json:"artifacts"`
}

type reportSchemaVersions struct {
	View     int `json:"view"`
	Evidence int `json:"evidence"`
	Manifest int `json:"manifest"`
}

type evidenceReportOptions struct {
	BundleDir string
	Output    string
}

func parseEvidenceReportArgs(args []string) (evidenceReportOptions, error) {
	var options evidenceReportOptions
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--output":
			index++
			if index >= len(args) || args[index] == "" {
				return evidenceReportOptions{}, newUsageError("evidence report needs an explicit --output path")
			}
			options.Output = args[index]
		default:
			if strings.HasPrefix(args[index], "-") {
				return evidenceReportOptions{}, newUsageError("unknown evidence report option %q", args[index])
			}
			if options.BundleDir != "" {
				return evidenceReportOptions{}, newUsageError("evidence report needs one Evidence Bundle directory")
			}
			options.BundleDir = args[index]
		}
	}
	if options.Output == "" || options.BundleDir == "" {
		return evidenceReportOptions{}, newUsageError("evidence report needs --output and one Evidence Bundle directory")
	}
	return options, nil
}

func renderExistingEvidenceReport(repoDir string, options evidenceReportOptions) (reportView, string, error) {
	bundleDir := resolveFromRepo(repoDir, options.BundleDir)
	output := resolveFromRepo(repoDir, options.Output)
	overlap, err := pathContainsOrSameChecked(bundleDir, output)
	if err != nil {
		return reportView{}, "", err
	}
	if overlap {
		return reportView{}, "", fmt.Errorf("evidence report output must be outside the verified Evidence Bundle")
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		return reportView{}, "", err
	}
	view, err := buildEvidenceReportView(bundleDir, bundle)
	if err != nil {
		return reportView{}, "", err
	}
	content, err := renderEvidenceReportHTML(view)
	if err != nil {
		return reportView{}, "", err
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return reportView{}, "", err
	}
	if err := rejectExistingFileLeaf(output); err != nil {
		return reportView{}, "", err
	}
	if err := os.WriteFile(output, content, 0o644); err != nil {
		return reportView{}, "", err
	}
	return view, output, nil
}

func pathContainsOrSameChecked(root string, path string) (bool, error) {
	rootPath, err := canonicalPathForOverlap(root)
	if err != nil {
		return false, err
	}
	pathValue, err := canonicalPathForOverlap(path)
	if err != nil {
		return false, err
	}
	return pathContainsOrSame(rootPath, pathValue), nil
}

func finalizeEvidenceReport(bundleDir string, terminal reportTerminalState) (reportView, error) {
	return finalizeEvidenceReportWithBundleMutation(bundleDir, terminal, nil)
}

func finalizeEvidenceReportWithBundleMutation(bundleDir string, terminal reportTerminalState, mutate func(*evidenceBundle)) (reportView, error) {
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		return reportView{}, err
	}
	if mutate != nil {
		mutate(&bundle)
	}
	bundle.LifecycleState = defaultReportValue(terminal.Lifecycle, bundle.LifecycleState, "running")
	bundle.CleanupState = defaultReportValue(terminal.Cleanup, bundle.CleanupState, "pending")
	bundle.ConfidentialityState = defaultReportValue(terminal.Confidentiality, bundle.ConfidentialityState, "private")
	bundle.NextCommand = defaultReportValue(terminal.Next, bundle.NextCommand, "maya-stall result "+bundle.RunID)
	// Canonical evidence is frozen before the report is projected. The final
	// manifest alone owns report.html metadata, keeping the digest reference
	// one-way and avoiding a report/evidence digest cycle.
	bundle.Report = nil
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	view, err := buildEvidenceReportView(bundleDir, bundle)
	if err != nil {
		return reportView{}, err
	}
	content, err := renderEvidenceReportHTML(view)
	if err != nil {
		return reportView{}, err
	}
	hash := sha256.Sum256(content)
	report := &evidenceArtifact{
		Label: "report", Kind: "report", Path: evidenceReportFileName,
		MediaType: reportMediaType, Size: int64(len(content)), SHA256: hex.EncodeToString(hash[:]),
	}
	manifestPath := filepath.Join(bundleDir, evidenceManifestFileName)
	manifestContent, err := os.ReadFile(manifestPath)
	if err != nil {
		return reportView{}, err
	}
	var manifest runManifest
	if err := json.Unmarshal(manifestContent, &manifest); err != nil {
		return reportView{}, fmt.Errorf("parse Evidence Bundle manifest: %w", err)
	}
	if err := validateEvidenceManifestIdentity(bundle, manifest, true); err != nil {
		return reportView{}, err
	}
	manifest.Report = report
	evidenceContent, err := marshalEvidenceReportJSON(bundle)
	if err != nil {
		return reportView{}, err
	}
	manifestContent, err = marshalEvidenceReportJSON(manifest)
	if err != nil {
		return reportView{}, err
	}
	if err := commitEvidenceReportFiles(bundleDir, map[string][]byte{
		evidenceBundleFileName:   evidenceContent,
		evidenceReportFileName:   content,
		evidenceManifestFileName: manifestContent,
	}); err != nil {
		return reportView{}, err
	}
	return view, nil
}

func marshalEvidenceReportJSON(value any) ([]byte, error) {
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(content, '\n'), nil
}

type evidenceReportFileSnapshot struct {
	content []byte
	exists  bool
}

func commitEvidenceReportFiles(bundleDir string, files map[string][]byte) error {
	// The manifest is the publication authority. Writing the report before the
	// evidence projection makes every crash window fail its existing digest;
	// the new manifest becomes visible only after both projections are durable.
	order := []string{evidenceReportFileName, evidenceBundleFileName, evidenceManifestFileName}
	snapshots := make(map[string]evidenceReportFileSnapshot, len(order))
	for _, name := range order {
		path := filepath.Join(bundleDir, name)
		content, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshots[name] = evidenceReportFileSnapshot{}
			continue
		}
		if err != nil {
			return err
		}
		snapshots[name] = evidenceReportFileSnapshot{content: content, exists: true}
	}
	var committed []string
	for _, name := range order {
		if err := writeRunLedgerBytes(filepath.Join(bundleDir, name), files[name]); err != nil {
			return errors.Join(err, restoreEvidenceReportFiles(bundleDir, snapshots, committed))
		}
		committed = append(committed, name)
	}
	return nil
}

func restoreEvidenceReportFiles(bundleDir string, snapshots map[string]evidenceReportFileSnapshot, committed []string) error {
	var restoreErr error
	for index := len(committed) - 1; index >= 0; index-- {
		name := committed[index]
		path := filepath.Join(bundleDir, name)
		snapshot := snapshots[name]
		if snapshot.exists {
			restoreErr = errors.Join(restoreErr, writeRunLedgerBytes(path, snapshot.content))
		} else {
			restoreErr = errors.Join(restoreErr, os.Remove(path))
		}
	}
	return restoreErr
}

func invalidateEvidenceReport(bundleDir string) error {
	reportPath := filepath.Join(bundleDir, evidenceReportFileName)
	if info, err := os.Lstat(reportPath); err == nil {
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("evidence bundle report leaf must be a regular file or symlink")
		}
		if removeErr := os.Remove(reportPath); removeErr != nil {
			return removeErr
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	manifestPath, ok := safeBundlePath(bundleDir, evidenceManifestFileName)
	if !ok {
		return fmt.Errorf("cannot invalidate unsafe Evidence Bundle manifest")
	}
	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest runManifest
	if err := json.Unmarshal(content, &manifest); err != nil {
		return err
	}
	manifest.Report = nil
	content, err = marshalEvidenceReportJSON(manifest)
	if err != nil {
		return err
	}
	return writeRunLedgerBytes(manifestPath, content)
}

func defaultReportValue(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func reportTerminalStateForOutcome(outcome runOutcome) reportTerminalState {
	state := reportTerminalState{Confidentiality: "private", Next: "maya-stall result " + outcome.RunID}
	switch outcome.StopPolicy {
	case "kept":
		state.Lifecycle, state.Cleanup = "kept", "retained"
	case "unresolved":
		state.Lifecycle, state.Cleanup = "cleanup-failed", "failed"
	default:
		state.Cleanup = "completed"
		if outcome.Result.Status == resultStatusPassed {
			state.Lifecycle = "completed"
		} else {
			state.Lifecycle = "failed"
		}
	}
	if outcome.Failure != nil && outcome.Failure.CleanupState != "" {
		state.Cleanup = outcome.Failure.CleanupState
		if state.Cleanup == "failed" {
			state.Lifecycle = "cleanup-failed"
		}
	}
	if len(outcome.FollowUpCommands) > 0 {
		state.Next = outcome.FollowUpCommands[0]
	}
	return state
}

func buildEvidenceReportView(bundleDir string, bundle evidenceBundle) (reportView, error) {
	view := reportView{
		Version: reportViewVersion, Status: defaultReportValue(bundle.Status, "unknown"),
		Scenario: defaultReportValue(bundle.Scenario, "unknown"), RunID: defaultReportValue(bundle.RunID, "unknown"),
		TargetProfile: defaultReportValue(bundle.TargetProfile, "unknown"), Revision: "not recorded",
		MayaIdentity: "not recorded", PluginIdentity: "not recorded", Duration: "not recorded",
		Lifecycle: defaultReportValue(bundle.LifecycleState, "unknown"), Cleanup: defaultReportValue(bundle.CleanupState, failureCleanup(bundle.Failure), "unknown"),
		Confidentiality: defaultReportValue(bundle.ConfidentialityState, "private"), WorkflowState: "not-reached",
		NextCommand:    defaultReportValue(bundle.NextCommand, "maya-stall result "+bundle.RunID),
		SchemaVersions: reportSchemaVersions{View: reportViewVersion, Evidence: bundle.Version},
	}
	manifestPath, ok := safeBundlePath(bundleDir, bundle.Manifest)
	if ok {
		content, err := os.ReadFile(manifestPath)
		if err == nil {
			var manifest runManifest
			if json.Unmarshal(content, &manifest) == nil {
				view.SchemaVersions.Manifest = manifest.Version
			} else {
				view.MissingEvidence = append(view.MissingEvidence, "manifest is malformed")
			}
		} else {
			view.MissingEvidence = append(view.MissingEvidence, "manifest is unavailable")
		}
	} else {
		view.MissingEvidence = append(view.MissingEvidence, "manifest path is missing or unsafe")
	}
	resultFields := map[string]any{}
	if bundle.Events == "" {
		view.MissingEvidence = append(view.MissingEvidence, "events path is missing")
	}
	if bundle.Log == "" {
		view.MissingEvidence = append(view.MissingEvidence, "log path is missing")
	}
	if resultPath, ok := safeBundlePath(bundleDir, bundle.ScenarioResult); ok {
		content, overLimit, err := readBoundedReportFile(resultPath, maximumReportBytes)
		if err == nil && !overLimit {
			decoder := json.NewDecoder(bytes.NewReader(content))
			decoder.UseNumber()
			if decoder.Decode(&resultFields) == nil && resultFields != nil && decoder.Decode(&struct{}{}) == io.EOF && validateReportResultFields(resultFields) == nil {
				recordedStatus := reportString(resultFields["status"])
				if bundle.Status != "" && recordedStatus != bundle.Status {
					view.MissingEvidence = append(view.MissingEvidence, "Scenario Result status disagrees with Evidence Bundle status")
				}
				view.Summary = reportString(resultFields["summary"])
				view.Revision = defaultReportValue(reportString(resultFields["revision"]), view.Revision)
				view.MayaIdentity = defaultReportValue(reportString(resultFields["maya"]), reportString(resultFields["mayaIdentity"]), view.MayaIdentity)
				view.PluginIdentity = defaultReportValue(reportString(resultFields["plugin"]), reportString(resultFields["pluginIdentity"]), view.PluginIdentity)
				view.Duration = defaultReportValue(reportString(resultFields["duration"]), reportString(resultFields["durationSeconds"]), view.Duration)
				view.Assertions = reportAssertions(resultFields["assertions"])
				view.Measurements = reportMeasurements(resultFields["measurements"])
				view.Steps = reportSteps(resultFields["steps"])
				if len(view.Steps) > 0 {
					view.WorkflowState = "recorded"
				}
			} else {
				view.MissingEvidence = append(view.MissingEvidence, "Scenario Result is malformed")
			}
		} else if overLimit {
			view.MissingEvidence = append(view.MissingEvidence, "Scenario Result exceeds the report input limit")
			view.Truncated = true
		} else {
			view.MissingEvidence = append(view.MissingEvidence, "Scenario Result is unavailable")
		}
	} else {
		view.MissingEvidence = append(view.MissingEvidence, "Scenario Result was not reached")
	}
	view.Validators = append([]validatorResult(nil), bundle.Validators...)
	view.Failure = reportFailure(bundle, view)
	artifacts, artifactCount, truncated, artifactProblems := reportArtifacts(bundleDir, bundle)
	view.Artifacts = artifacts
	view.Truncated = view.Truncated || truncated || reportEventsTruncated(bundleDir, bundle.Events)
	view.MissingEvidence = append(view.MissingEvidence, artifactProblems...)
	for _, assertion := range view.Assertions {
		if assertion.Status == "unknown" {
			view.MissingEvidence = append(view.MissingEvidence, fmt.Sprintf("assertion %s has unknown status", assertion.Name))
		}
	}
	for _, validator := range view.Validators {
		if validator.Status != resultStatusPassed && validator.Status != resultStatusFailed {
			view.MissingEvidence = append(view.MissingEvidence, fmt.Sprintf("Validator %s has unknown status", validator.Type))
		}
	}
	view.Counts = countReportItems(view)
	view.Counts.Artifacts = artifactCount
	if len(view.MissingEvidence) == 0 && len(view.Artifacts) == 0 {
		view.MissingEvidence = append(view.MissingEvidence, "artifact inventory is empty")
	}
	sort.Strings(view.MissingEvidence)
	view.FailureCategory = reportFailureCategory(bundle, view)
	view.Verdict = reportVerdict(bundle, view)
	return boundEvidenceReportView(view), nil
}

func failureCleanup(failure *runFailureEvidence) string {
	if failure == nil {
		return ""
	}
	return failure.CleanupState
}

func reportFailure(bundle evidenceBundle, view reportView) reportFailureView {
	if bundle.Failure != nil {
		return reportFailureView{
			Phase: bundle.Failure.FailedLayer, Excerpt: boundedReportText(bundle.Failure.Diagnostic),
			Retryable: reportRetryable(bundle.Failure.Diagnostic), Remediation: bundle.Failure.RemediationHint,
		}
	}
	for _, validator := range bundle.Validators {
		if validator.Status == resultStatusFailed {
			return reportFailureView{Phase: "validation", Item: validator.Type, Excerpt: boundedReportText(validator.Message), Retryable: false, Remediation: "Fix the failed Validator input or expected output, then rerun the Scenario."}
		}
	}
	for _, assertion := range view.Assertions {
		if assertion.Status == resultStatusFailed {
			return reportFailureView{Phase: "scenario", Item: assertion.Name, Excerpt: boundedReportText(assertion.Summary), Retryable: false, Remediation: "Fix the failed Scenario assertion, then rerun the Scenario."}
		}
	}
	return reportFailureView{}
}

func reportFailureCategory(bundle evidenceBundle, view reportView) string {
	if view.Cleanup == "failed" || view.Lifecycle == "cleanup-failed" {
		return "cleanup-failed"
	}
	if view.Lifecycle == "kept" || view.Cleanup == "retained" {
		return "kept"
	}
	if bundle.Failure != nil {
		diagnostic := strings.ToLower(bundle.Failure.Diagnostic)
		if strings.Contains(diagnostic, "timed out") || strings.Contains(diagnostic, "timeout") {
			return "timeout"
		}
		switch bundle.Failure.FailedLayer {
		case string(failureLayerHostSelection):
			return "host-failing"
		case string(failureLayerRemoteCheck):
			return "transport-failing"
		case string(failureLayerExecution), string(failureLayerScenario):
			return "product-failing"
		default:
			return "artifact-failing"
		}
	}
	if view.Counts.FailedAssertions > 0 {
		return "product-failing"
	}
	if view.Counts.FailedValidators > 0 || view.Counts.UnknownAssertions > 0 || view.Counts.UnknownValidators > 0 || len(view.MissingEvidence) > 0 {
		return "artifact-failing"
	}
	return ""
}

func reportVerdict(bundle evidenceBundle, view reportView) string {
	category := reportFailureCategory(bundle, view)
	if category != "" {
		return category
	}
	if bundle.Status == resultStatusPassed && view.Lifecycle == "completed" && view.Cleanup == "completed" {
		return "passed"
	}
	if bundle.Status == resultStatusFailed {
		return "product-failing"
	}
	return "not-final"
}

func reportRetryable(diagnostic string) bool {
	value := strings.ToLower(diagnostic)
	return strings.Contains(value, "timeout") || strings.Contains(value, "timed out") || strings.Contains(value, "temporar") || strings.Contains(value, "connection reset")
}

func reportArtifacts(bundleDir string, bundle evidenceBundle) ([]reportArtifactView, int, bool, []string) {
	catalog := evidenceBundleCatalog(bundle)
	views := make([]reportArtifactView, 0, min(len(catalog), maximumReportArtifacts))
	var problems []string
	truncated := false
	artifactCount := 0
	for _, artifact := range catalog {
		if artifact.Kind == "report" || artifact.Path == evidenceReportFileName {
			continue
		}
		artifactCount++
		view := reportArtifactView{Label: artifact.Label, Kind: artifact.Kind, Path: artifact.Path, MediaType: artifact.MediaType, Origin: artifact.Origin, State: "missing"}
		path, ok := safeBundlePath(bundleDir, artifact.Path)
		if !ok {
			view.State = "unsafe"
			problems = append(problems, fmt.Sprintf("artifact %s is %s", view.Path, view.State))
			if len(views) < maximumReportArtifacts {
				views = append(views, view)
			} else {
				truncated = true
			}
			continue
		}
		view.Link = safeReportLink(artifact.Path)
		info, err := os.Lstat(path)
		actualSizeKnown := false
		if err == nil && info.Mode().IsRegular() {
			view.State = "available"
			// evidence.json and manifest.json receive the report metadata after
			// rendering. Including their post-render bytes here would create an
			// indirect digest cycle and make a later read-only render differ.
			if artifact.Path != evidenceBundleFileName && artifact.Path != evidenceManifestFileName {
				actualSizeKnown = true
				view.Size = info.Size()
				view.SHA256, err = fileSHA256Hex(path)
				if err != nil {
					view.State = "unreadable"
				}
			}
		} else if err == nil {
			view.State = "unsafe"
		} else if !os.IsNotExist(err) {
			view.State = "unreadable"
		}
		if artifact.SHA256 != "" {
			view.RecordedSHA256 = artifact.SHA256
			if view.State == "available" && view.SHA256 != "" && view.SHA256 != artifact.SHA256 {
				view.State = "hash-mismatch"
			}
		}
		if artifact.Size > 0 {
			view.RecordedSize = artifact.Size
			if view.State == "available" && actualSizeKnown && view.Size != artifact.Size {
				view.State = "size-mismatch"
			}
		}
		if view.State != "available" {
			problems = append(problems, fmt.Sprintf("artifact %s is %s", view.Path, view.State))
		}
		if len(views) < maximumReportArtifacts {
			views = append(views, view)
		} else {
			truncated = true
		}
	}
	sort.SliceStable(views, func(i, j int) bool { return views[i].Path < views[j].Path })
	sort.Strings(problems)
	return views, artifactCount, truncated, problems
}

func safeBundlePath(bundleDir string, relative string) (string, bool) {
	clean := cleanEvidenceArtifactPath(relative)
	if clean == "" || clean != filepath.ToSlash(relative) {
		return "", false
	}
	rootInfo, err := os.Lstat(bundleDir)
	if err != nil || rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return "", false
	}
	target := filepath.Join(bundleDir, filepath.FromSlash(clean))
	current := bundleDir
	parts := strings.Split(clean, "/")
	for index, part := range parts {
		current = filepath.Join(current, filepath.FromSlash(part))
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			break
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 || index < len(parts)-1 && !info.IsDir() {
			return "", false
		}
	}
	canonicalRoot, err := canonicalPathForOverlap(bundleDir)
	if err != nil {
		return "", false
	}
	canonicalTarget, err := canonicalPathForOverlap(target)
	if err != nil || !pathContainsOrSame(canonicalRoot, canonicalTarget) {
		return "", false
	}
	return target, true
}

func safeReportLink(path string) string {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for index := range parts {
		parts[index] = url.PathEscape(parts[index])
	}
	return strings.Join(parts, "/")
}

func reportEventsTruncated(bundleDir string, relative string) bool {
	path, ok := safeBundlePath(bundleDir, relative)
	if !ok {
		return false
	}
	content, overLimit, err := readBoundedReportFile(path, maximumReportBytes)
	if err != nil {
		return false
	}
	return overLimit || bytes.Contains(bytes.ToLower(content), []byte("truncat"))
}

func readBoundedReportFile(path string, limit int64) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("report input %s must be a regular file", filepath.Base(path))
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(content)) > limit {
		return content[:limit], true, nil
	}
	return content, false, nil
}

func reportString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		return value.String()
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(value)
	case nil:
		return ""
	default:
		content, _ := json.Marshal(value)
		return string(content)
	}
}

func validateReportResultFields(fields map[string]any) error {
	status, ok := fields["status"]
	if !ok {
		return fmt.Errorf("status is missing")
	}
	if _, ok := status.(string); !ok {
		return fmt.Errorf("status has the wrong type")
	}
	if status != resultStatusPassed && status != resultStatusFailed {
		return fmt.Errorf("status is not passed or failed")
	}
	for _, key := range []string{"summary", "revision", "maya", "mayaIdentity", "plugin", "pluginIdentity", "duration"} {
		if value, exists := fields[key]; exists {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s has the wrong type", key)
			}
		}
	}
	if value, exists := fields["durationSeconds"]; exists && !reportScalar(value) {
		return fmt.Errorf("durationSeconds has the wrong type")
	}
	for _, key := range []string{"assertions", "measurements", "steps"} {
		value, exists := fields[key]
		if !exists {
			continue
		}
		items, ok := value.([]any)
		if !ok {
			return fmt.Errorf("%s has the wrong type", key)
		}
		for _, item := range items {
			object, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("%s contains a non-object item", key)
			}
			if err := validateReportResultItem(key, object); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateReportResultItem(kind string, fields map[string]any) error {
	stringFields := map[string][]string{
		"assertions":   {"name", "status", "summary"},
		"measurements": {"name", "unit"},
		"steps":        {"id", "name", "status", "summary"},
	}
	for _, key := range stringFields[kind] {
		if value, exists := fields[key]; exists {
			if _, ok := value.(string); !ok {
				return fmt.Errorf("%s item %s has the wrong type", kind, key)
			}
		}
	}
	if kind == "assertions" {
		if value, exists := fields["passed"]; exists {
			if _, ok := value.(bool); !ok {
				return fmt.Errorf("assertion passed has the wrong type")
			}
		}
	}
	if kind == "measurements" {
		for _, key := range []string{"value", "threshold", "passed"} {
			if value, exists := fields[key]; exists && !reportScalar(value) {
				return fmt.Errorf("measurement %s has the wrong type", key)
			}
		}
	}
	return nil
}

func reportScalar(value any) bool {
	switch value.(type) {
	case string, json.Number, float64, bool:
		return true
	default:
		return false
	}
}

func reportAssertions(value any) []reportAssertionView {
	items, _ := value.([]any)
	assertions := make([]reportAssertionView, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		status := "unknown"
		if passed, ok := fields["passed"].(bool); ok {
			if passed {
				status = resultStatusPassed
			} else {
				status = resultStatusFailed
			}
		} else if recorded := reportString(fields["status"]); recorded == resultStatusPassed || recorded == resultStatusFailed {
			status = recorded
		}
		assertions = append(assertions, reportAssertionView{Name: defaultReportValue(reportString(fields["name"]), "unnamed assertion"), Status: status, Summary: reportString(fields["summary"])})
	}
	return assertions
}

func reportMeasurements(value any) []reportMeasurementView {
	items, _ := value.([]any)
	measurements := make([]reportMeasurementView, 0, len(items))
	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		measurements = append(measurements, reportMeasurementView{
			Name: defaultReportValue(reportString(fields["name"]), "unnamed measurement"), Value: reportString(fields["value"]),
			Unit: reportString(fields["unit"]), Threshold: reportString(fields["threshold"]), Passed: defaultReportValue(reportString(fields["passed"]), "unknown"),
		})
	}
	return measurements
}

func reportSteps(value any) []reportStepView {
	items, _ := value.([]any)
	steps := make([]reportStepView, 0, len(items))
	for index, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		steps = append(steps, reportStepView{
			ID: defaultReportValue(reportString(fields["id"]), strconv.Itoa(index+1)), Name: defaultReportValue(reportString(fields["name"]), reportString(fields["id"]), "unnamed step"),
			Status: defaultReportValue(reportString(fields["status"]), "unknown"), Summary: reportString(fields["summary"]),
		})
	}
	return steps
}

func countReportItems(view reportView) reportCounts {
	counts := reportCounts{Assertions: len(view.Assertions), Measurements: len(view.Measurements), Validators: len(view.Validators), Artifacts: len(view.Artifacts)}
	for _, assertion := range view.Assertions {
		switch assertion.Status {
		case resultStatusFailed:
			counts.FailedAssertions++
		case resultStatusPassed:
		default:
			counts.UnknownAssertions++
		}
	}
	for _, validator := range view.Validators {
		switch validator.Status {
		case resultStatusFailed:
			counts.FailedValidators++
		case resultStatusPassed:
		default:
			counts.UnknownValidators++
		}
	}
	return counts
}

func boundedReportText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maximumReportExcerpt {
		return value
	}
	return value[:maximumReportExcerpt] + " [truncated]"
}

func boundEvidenceReportView(view reportView) reportView {
	sanitizeReportViewText(&view, redactAbsoluteReportPaths)
	bound := func(value string) string {
		if len(strings.TrimSpace(value)) > maximumReportText {
			view.Truncated = true
		}
		return boundedReportString(value, maximumReportText)
	}
	view.Verdict, view.Status, view.FailureCategory = bound(view.Verdict), bound(view.Status), bound(view.FailureCategory)
	view.Scenario, view.RunID, view.Revision = bound(view.Scenario), bound(view.RunID), bound(view.Revision)
	view.TargetProfile, view.MayaIdentity, view.PluginIdentity = bound(view.TargetProfile), bound(view.MayaIdentity), bound(view.PluginIdentity)
	view.Duration, view.Lifecycle, view.Cleanup = bound(view.Duration), bound(view.Lifecycle), bound(view.Cleanup)
	view.Confidentiality, view.WorkflowState, view.NextCommand = bound(view.Confidentiality), bound(view.WorkflowState), bound(view.NextCommand)
	if len(strings.TrimSpace(view.Summary)) > maximumReportExcerpt || len(strings.TrimSpace(view.Failure.Excerpt)) > maximumReportExcerpt || len(strings.TrimSpace(view.Failure.Remediation)) > maximumReportExcerpt {
		view.Truncated = true
	}
	view.Summary = boundedReportString(view.Summary, maximumReportExcerpt)
	view.Failure.Phase, view.Failure.Item = bound(view.Failure.Phase), bound(view.Failure.Item)
	view.Failure.Excerpt = boundedReportText(view.Failure.Excerpt)
	view.Failure.Remediation = boundedReportString(view.Failure.Remediation, maximumReportExcerpt)

	view.Steps, view.Truncated = boundReportSteps(view.Steps, view.Truncated)
	view.Assertions, view.Truncated = boundReportAssertions(view.Assertions, view.Truncated)
	view.Measurements, view.Truncated = boundReportMeasurements(view.Measurements, view.Truncated)
	view.Validators, view.Truncated = boundReportValidators(view.Validators, view.Truncated)
	for index := range view.Artifacts {
		artifact := &view.Artifacts[index]
		artifact.Label, artifact.Kind, artifact.Path = bound(artifact.Label), bound(artifact.Kind), bound(artifact.Path)
		artifact.Link, artifact.MediaType, artifact.SHA256 = bound(artifact.Link), bound(artifact.MediaType), bound(artifact.SHA256)
		artifact.RecordedSHA256 = bound(artifact.RecordedSHA256)
		artifact.Origin, artifact.State = bound(artifact.Origin), bound(artifact.State)
	}
	if len(view.MissingEvidence) > maximumReportItems {
		view.MissingEvidence = view.MissingEvidence[:maximumReportItems]
		view.Truncated = true
	}
	for index := range view.MissingEvidence {
		view.MissingEvidence[index] = bound(view.MissingEvidence[index])
	}
	return fitEvidenceReportView(view)
}

func fitEvidenceReportView(view reportView) reportView {
	for {
		content, err := executeEvidenceReportTemplate(view)
		if err != nil || len(content) < maximumReportBytes {
			return view
		}
		view.Truncated = true
		changed := false
		view.Artifacts, changed = halveReportSlice(view.Artifacts, changed)
		view.Validators, changed = halveReportSlice(view.Validators, changed)
		view.Measurements, changed = halveReportSlice(view.Measurements, changed)
		view.Assertions, changed = halveReportSlice(view.Assertions, changed)
		view.Steps, changed = halveReportSlice(view.Steps, changed)
		if !changed {
			return view
		}
	}
}

func halveReportSlice[T any](items []T, changed bool) ([]T, bool) {
	if len(items) == 0 {
		return items, changed
	}
	return items[:len(items)/2], true
}

func sanitizeReportViewText(view *reportView, sanitize func(string) string) {
	if view == nil || sanitize == nil {
		return
	}
	view.Verdict, view.Status, view.FailureCategory = sanitize(view.Verdict), sanitize(view.Status), sanitize(view.FailureCategory)
	view.Scenario, view.RunID, view.Revision = sanitize(view.Scenario), sanitize(view.RunID), sanitize(view.Revision)
	view.TargetProfile, view.MayaIdentity, view.PluginIdentity = sanitize(view.TargetProfile), sanitize(view.MayaIdentity), sanitize(view.PluginIdentity)
	view.Duration, view.Lifecycle, view.Cleanup = sanitize(view.Duration), sanitize(view.Lifecycle), sanitize(view.Cleanup)
	view.Confidentiality, view.Summary, view.WorkflowState = sanitize(view.Confidentiality), sanitize(view.Summary), sanitize(view.WorkflowState)
	view.NextCommand = sanitize(view.NextCommand)
	view.Failure.Phase, view.Failure.Item = sanitize(view.Failure.Phase), sanitize(view.Failure.Item)
	view.Failure.Excerpt, view.Failure.Remediation = sanitize(view.Failure.Excerpt), sanitize(view.Failure.Remediation)
	for index := range view.Steps {
		view.Steps[index].ID, view.Steps[index].Name = sanitize(view.Steps[index].ID), sanitize(view.Steps[index].Name)
		view.Steps[index].Status, view.Steps[index].Summary = sanitize(view.Steps[index].Status), sanitize(view.Steps[index].Summary)
	}
	for index := range view.Assertions {
		view.Assertions[index].Name, view.Assertions[index].Summary = sanitize(view.Assertions[index].Name), sanitize(view.Assertions[index].Summary)
	}
	for index := range view.Measurements {
		item := &view.Measurements[index]
		item.Name, item.Value, item.Unit = sanitize(item.Name), sanitize(item.Value), sanitize(item.Unit)
		item.Threshold, item.Passed = sanitize(item.Threshold), sanitize(item.Passed)
	}
	for index := range view.Validators {
		item := &view.Validators[index]
		item.Type, item.Status, item.Message = sanitize(item.Type), sanitize(item.Status), sanitize(item.Message)
	}
	for index := range view.Artifacts {
		item := &view.Artifacts[index]
		item.Label, item.Kind, item.Path = sanitize(item.Label), sanitize(item.Kind), sanitize(item.Path)
		item.Link, item.MediaType, item.SHA256 = sanitize(item.Link), sanitize(item.MediaType), sanitize(item.SHA256)
		item.RecordedSHA256 = sanitize(item.RecordedSHA256)
		item.Origin, item.State = sanitize(item.Origin), sanitize(item.State)
	}
	for index := range view.MissingEvidence {
		view.MissingEvidence[index] = sanitize(view.MissingEvidence[index])
	}
}

func redactAbsoluteReportPaths(value string) string {
	value = windowsAbsoluteReportPath.ReplaceAllString(value, "[absolute path]")
	return posixAbsoluteReportPath.ReplaceAllStringFunc(value, func(match string) string {
		if strings.HasPrefix(match, "/") {
			return "[absolute path]"
		}
		return match[:1] + "[absolute path]"
	})
}

func boundedReportString(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + " [truncated]"
}

func boundReportSteps(items []reportStepView, truncated bool) ([]reportStepView, bool) {
	if len(items) > maximumReportItems {
		items, truncated = items[:maximumReportItems], true
	}
	for index := range items {
		items[index].ID = boundReportItemText(items[index].ID, &truncated)
		items[index].Name = boundReportItemText(items[index].Name, &truncated)
		items[index].Status = boundReportItemText(items[index].Status, &truncated)
		items[index].Summary = boundReportItemText(items[index].Summary, &truncated)
	}
	return items, truncated
}

func boundReportAssertions(items []reportAssertionView, truncated bool) ([]reportAssertionView, bool) {
	if len(items) > maximumReportItems {
		items, truncated = items[:maximumReportItems], true
	}
	for index := range items {
		items[index].Name = boundReportItemText(items[index].Name, &truncated)
		items[index].Status = boundReportItemText(items[index].Status, &truncated)
		items[index].Summary = boundReportItemText(items[index].Summary, &truncated)
	}
	return items, truncated
}

func boundReportMeasurements(items []reportMeasurementView, truncated bool) ([]reportMeasurementView, bool) {
	if len(items) > maximumReportItems {
		items, truncated = items[:maximumReportItems], true
	}
	for index := range items {
		items[index].Name = boundReportItemText(items[index].Name, &truncated)
		items[index].Value = boundReportItemText(items[index].Value, &truncated)
		items[index].Unit = boundReportItemText(items[index].Unit, &truncated)
		items[index].Threshold = boundReportItemText(items[index].Threshold, &truncated)
		items[index].Passed = boundReportItemText(items[index].Passed, &truncated)
	}
	return items, truncated
}

func boundReportValidators(items []validatorResult, truncated bool) ([]validatorResult, bool) {
	if len(items) > maximumReportItems {
		items, truncated = items[:maximumReportItems], true
	}
	for index := range items {
		items[index].Type = boundReportItemText(items[index].Type, &truncated)
		items[index].Status = boundReportItemText(items[index].Status, &truncated)
		items[index].Message = boundReportItemText(items[index].Message, &truncated)
	}
	return items, truncated
}

func boundReportItemText(value string, truncated *bool) string {
	if len(strings.TrimSpace(value)) > maximumReportText {
		*truncated = true
	}
	return boundedReportString(value, maximumReportText)
}

func reportViewFromOutcome(outcome runOutcome) reportView {
	view := reportView{
		Version: reportViewVersion, RunID: outcome.RunID, Scenario: outcome.Scenario, Status: outcome.Result.Status,
		TargetProfile: outcome.TargetProfile, Summary: outcome.Result.Summary, Validators: append([]validatorResult(nil), outcome.Validators...),
		Revision: "not recorded", MayaIdentity: "not recorded", PluginIdentity: "not recorded", Duration: "not recorded", Confidentiality: "private", WorkflowState: "not-reached",
	}
	terminal := reportTerminalStateForOutcome(outcome)
	view.Lifecycle, view.Cleanup, view.NextCommand = terminal.Lifecycle, terminal.Cleanup, terminal.Next
	if outcome.Failure != nil {
		view.Failure = reportFailureView{Phase: outcome.Failure.FailedLayer, Excerpt: boundedReportText(outcome.Failure.Diagnostic), Retryable: reportRetryable(outcome.Failure.Diagnostic), Remediation: outcome.Failure.RemediationHint}
	}
	view.Counts = countReportItems(view)
	view.FailureCategory = reportFailureCategory(evidenceBundle{Status: outcome.Result.Status, Failure: outcome.Failure, Validators: outcome.Validators}, view)
	view.Verdict = reportVerdict(evidenceBundle{Status: outcome.Result.Status, Failure: outcome.Failure}, view)
	return boundEvidenceReportView(view)
}

func reportViewForOutcome(outcome runOutcome) reportView {
	if outcome.Report != nil {
		return *outcome.Report
	}
	if outcome.EvidenceDir != "" {
		if bundle, err := readEvidenceBundleFile(outcome.EvidenceDir); err == nil {
			if view, err := buildEvidenceReportView(outcome.EvidenceDir, bundle); err == nil {
				return view
			}
		}
	}
	return reportViewFromOutcome(outcome)
}

func reportViewPointer(outcome runOutcome) *reportView {
	view := reportViewForOutcome(outcome)
	return &view
}

func renderEvidenceReportHTML(view reportView) ([]byte, error) {
	view = boundEvidenceReportView(view)
	output, err := executeEvidenceReportTemplate(view)
	if err != nil {
		return nil, err
	}
	if len(output) >= maximumReportBytes {
		return nil, fmt.Errorf("report.html is %d bytes; must be less than %d", len(output), maximumReportBytes)
	}
	return output, nil
}

func executeEvidenceReportTemplate(view reportView) ([]byte, error) {
	var output bytes.Buffer
	if err := evidenceReportTemplate.Execute(&output, view); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

var evidenceReportTemplate = template.Must(template.New("report").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Maya Stall report: {{.Scenario}} / {{.RunID}}</title>
<style>body{font:15px/1.5 system-ui,sans-serif;max-width:1050px;margin:2rem auto;padding:0 1rem;color:#18202a;background:#f6f7f9}main,section{background:white;border:1px solid #d7dce2;border-radius:8px;padding:1rem 1.25rem;margin:1rem 0}h1,h2{line-height:1.2}dt{font-weight:650}dd{margin:0 0 .6rem}code{overflow-wrap:anywhere}a{color:#0758a8}.verdict{font-weight:750}.failed{color:#9f1d20}.passed{color:#126a3a}.muted{color:#59636e}table{border-collapse:collapse;width:100%}th,td{text-align:left;vertical-align:top;border-bottom:1px solid #e3e6ea;padding:.45rem}ul{padding-left:1.25rem}</style></head>
<body><main><h1>Maya Stall Evidence report</h1><p class="verdict {{if eq .Verdict "passed"}}passed{{else}}failed{{end}}">Verdict: {{.Verdict}}</p><p>{{.Summary}}</p>
<dl><dt>Status</dt><dd>{{.Status}}</dd><dt>Scenario</dt><dd>{{.Scenario}}</dd><dt>Run ID</dt><dd><code>{{.RunID}}</code></dd><dt>Revision</dt><dd>{{.Revision}}</dd><dt>Target Profile</dt><dd>{{.TargetProfile}}</dd><dt>Maya</dt><dd>{{.MayaIdentity}}</dd><dt>Plug-in</dt><dd>{{.PluginIdentity}}</dd><dt>Duration</dt><dd>{{.Duration}}</dd><dt>Lifecycle</dt><dd>{{.Lifecycle}}</dd><dt>Cleanup</dt><dd>{{.Cleanup}}</dd><dt>Confidentiality</dt><dd>{{.Confidentiality}}</dd></dl></main>
<section id="diagnosis"><h2>Failure-first diagnosis</h2>{{if .Failure.Phase}}<dl><dt>First failed phase</dt><dd>{{.Failure.Phase}}</dd><dt>Assertion or Validator</dt><dd>{{.Failure.Item}}</dd><dt>Error excerpt</dt><dd><code>{{.Failure.Excerpt}}</code></dd><dt>Retryable</dt><dd>{{.Failure.Retryable}}</dd><dt>Remediation</dt><dd>{{.Failure.Remediation}}</dd><dt>Next command</dt><dd><code>{{.NextCommand}}</code></dd></dl>{{else}}<p>No failure recorded. Next command: <code>{{.NextCommand}}</code></p>{{end}}</section>
<section id="workflow"><h2>Workflow</h2><p>State: {{.WorkflowState}}{{if .Truncated}}; truncated{{end}}</p>{{if .Steps}}<ol>{{range .Steps}}<li><strong>{{.Name}}</strong>, {{.Status}}{{if .Summary}}: {{.Summary}}{{end}}</li>{{end}}</ol>{{else}}<p class="muted">Named steps and checkpoints were not reached or were not recorded.</p>{{end}}</section>
<section id="proof"><h2>Proof</h2><p>Assertions: {{.Counts.Assertions}}, failed: {{.Counts.FailedAssertions}}, unknown: {{.Counts.UnknownAssertions}}. Measurements: {{.Counts.Measurements}}. Validators: {{.Counts.Validators}}, failed: {{.Counts.FailedValidators}}, unknown: {{.Counts.UnknownValidators}}. Artifacts: {{.Counts.Artifacts}}.</p>
{{if .Assertions}}<h3>Assertions</h3><ul>{{range .Assertions}}<li>{{.Name}}: {{.Status}}{{if .Summary}}, {{.Summary}}{{end}}</li>{{end}}</ul>{{end}}
{{if .Measurements}}<h3>Measurements</h3><ul>{{range .Measurements}}<li>{{.Name}}: {{.Value}} {{.Unit}}{{if .Threshold}}, threshold {{.Threshold}}{{end}}{{if .Passed}}, passed {{.Passed}}{{end}}</li>{{end}}</ul>{{end}}
{{if .Validators}}<h3>Validators</h3><ul>{{range .Validators}}<li>{{.Type}}: {{.Status}}, {{.Message}}</li>{{end}}</ul>{{end}}
<h3>Artifact inventory</h3><table><thead><tr><th>Artifact</th><th>Type</th><th>Bytes</th><th>SHA-256</th><th>State</th></tr></thead><tbody>{{range .Artifacts}}<tr><td>{{if .Link}}<a href="{{.Link}}">{{.Path}}</a>{{else}}{{.Path}}{{end}}</td><td>{{.MediaType}}{{if .Origin}}; {{.Origin}}{{end}}</td><td>{{.Size}}{{if .RecordedSize}}; recorded {{.RecordedSize}}{{end}}</td><td><code>{{.SHA256}}</code>{{if .RecordedSHA256}}<br><span class="muted">recorded <code>{{.RecordedSHA256}}</code></span>{{end}}</td><td>{{.State}}</td></tr>{{end}}</tbody></table></section>
<section id="integrity"><h2>Integrity</h2><dl><dt>Report schema</dt><dd>{{.SchemaVersions.View}}</dd><dt>Evidence schema</dt><dd>{{.SchemaVersions.Evidence}}</dd><dt>Manifest schema</dt><dd>{{.SchemaVersions.Manifest}}</dd><dt>Truncated</dt><dd>{{.Truncated}}</dd></dl>{{if .MissingEvidence}}<h3>Missing evidence</h3><ul>{{range .MissingEvidence}}<li>{{.}}</li>{{end}}</ul>{{else}}<p>No missing evidence recorded.</p>{{end}}<p class="muted">Canonical JSON records and artifact bytes remain authoritative. This HTML is a projection and does not contain its own manifest digest.</p></section>
</body></html>
`))
