package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceReportFinalizationIsDeterministicAndManifested(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	terminal := reportTerminalState{Lifecycle: "completed", Cleanup: "completed", Confidentiality: "private", Next: "maya-stall result run-report"}
	first, err := finalizeEvidenceReport(bundleDir, terminal)
	if err != nil {
		t.Fatalf("finalize report: %v", err)
	}
	firstBytes := mustReadReportFile(t, filepath.Join(bundleDir, evidenceReportFileName))
	second, err := finalizeEvidenceReport(bundleDir, terminal)
	if err != nil {
		t.Fatalf("finalize report again: %v", err)
	}
	secondBytes := mustReadReportFile(t, filepath.Join(bundleDir, evidenceReportFileName))
	if !bytes.Equal(firstBytes, secondBytes) {
		index := 0
		for index < len(firstBytes) && index < len(secondBytes) && firstBytes[index] == secondBytes[index] {
			index++
		}
		t.Fatalf("identical canonical input produced different report bytes at %d: first %.160q second %.160q", index, firstBytes[index:], secondBytes[index:])
	}
	if first.Verdict != "passed" || second.Verdict != first.Verdict || len(firstBytes) >= maximumReportBytes {
		t.Fatalf("report view/size = verdict %q/%q bytes %d", first.Verdict, second.Verdict, len(firstBytes))
	}

	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Report == nil || bundle.Report.Path != evidenceReportFileName || bundle.Report.MediaType != reportMediaType || bundle.Report.Size != int64(len(firstBytes)) {
		t.Fatalf("bundle report metadata = %+v", bundle.Report)
	}
	hash := sha256.Sum256(firstBytes)
	if bundle.Report.SHA256 != hex.EncodeToString(hash[:]) {
		t.Fatalf("bundle report hash = %q", bundle.Report.SHA256)
	}
	var manifest runManifest
	readJSONFile(t, filepath.Join(bundleDir, evidenceManifestFileName), &manifest)
	if manifest.Report == nil || *manifest.Report != *bundle.Report {
		t.Fatalf("manifest report metadata = %+v, bundle = %+v", manifest.Report, bundle.Report)
	}
	evidenceBytes := mustReadReportFile(t, filepath.Join(bundleDir, evidenceBundleFileName))
	if bytes.Contains(evidenceBytes, []byte(`"report"`)) {
		t.Fatalf("canonical evidence contains circular report metadata: %s", evidenceBytes)
	}
}

func TestEvidenceReportEscapesUntrustedContentAndUsesOnlySafeRelativeLinks(t *testing.T) {
	malicious := `<script>alert("x")</script><img src="https://evil.example/x"> C:\Users\Jane Doe\scene.ma; /Users/Jane Doe/scene.ma; scene=/private/equal.ma; path:/Users/alice/colon.ma; file:///home/user/url.ma`
	bundleDir := writeReportFixture(t, reportFixtureOptions{Scenario: malicious, Summary: malicious, ArtifactPath: "outputs/a name.txt"})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	content := string(mustReadReportFile(t, filepath.Join(bundleDir, evidenceReportFileName)))
	for _, forbidden := range []string{"<script>", "<img", `src="`, "url(", "javascript:", `C:\Users`, `Jane Doe`, "/private/equal", "/Users/alice", "/home/user"} {
		if strings.Contains(strings.ToLower(content), strings.ToLower(forbidden)) {
			t.Fatalf("report contains active untrusted content %q", forbidden)
		}
	}
	if !strings.Contains(content, "&lt;script&gt;") || !strings.Contains(content, `href="outputs/a%20name.txt"`) {
		t.Fatalf("report did not escape text or encode safe link:\n%s", content)
	}
	for _, section := range []string{`id="diagnosis"`, `id="workflow"`, `id="proof"`, `id="integrity"`} {
		if !strings.Contains(content, section) {
			t.Fatalf("report missing stable section %s", section)
		}
	}
}

func TestEvidenceReportRejectsSymlinkedBundleInputs(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	outside := filepath.Join(t.TempDir(), "outside.json")
	mustWriteFile(t, outside, `{"status":"passed","summary":"private outside data"}`+"\n")
	resultPath := filepath.Join(bundleDir, evidenceScenarioResultFileName)
	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, resultPath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	content := string(mustReadReportFile(t, filepath.Join(bundleDir, evidenceReportFileName)))
	if view.Verdict != "artifact-failing" || strings.Contains(content, "private outside data") {
		t.Fatalf("symlinked Scenario Result report = %+v\n%s", view, content)
	}
}

func TestEvidenceReportCommandRejectsSymlinkedPrimaryMetadata(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	evidencePath := filepath.Join(bundleDir, evidenceBundleFileName)
	outside := filepath.Join(t.TempDir(), evidenceBundleFileName)
	content := mustReadReportFile(t, evidencePath)
	if err := os.WriteFile(outside, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(evidencePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, evidencePath); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	output := filepath.Join(t.TempDir(), "report.html")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"evidence", "report", "--output", output, bundleDir}, &stdout, &stderr, "", "test-version"); code != 1 || !strings.Contains(stderr.String(), "metadata path is unsafe") {
		t.Fatalf("symlinked primary metadata exit = %d; stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("unsafe primary metadata produced a report: %v", err)
	}
}

func TestEvidenceReportRejectsArtifactUnderSymlinkedParent(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	outputDir := filepath.Join(bundleDir, "outputs")
	if err := os.Remove(filepath.Join(outputDir, "result.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(outputDir); err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	mustWriteFile(t, filepath.Join(outsideDir, "result.txt"), "private outside artifact\n")
	if err := os.Symlink(outsideDir, outputDir); err != nil {
		t.Skipf("symlink creation unavailable: %v", err)
	}
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "artifact-failing" || !containsReportMissingEvidence(view, "artifact outputs/result.txt is unsafe") {
		t.Fatalf("symlink-parent artifact report = %+v", view)
	}
}

func TestEvidenceReportDoesNotOpenNonRegularPrimaryInputs(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	resultPath := filepath.Join(bundleDir, evidenceScenarioResultFileName)
	if err := os.Remove(resultPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(resultPath, 0o700); err != nil {
		t.Fatal(err)
	}
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "artifact-failing" || !containsReportMissingEvidence(view, "Scenario Result is unavailable") {
		t.Fatalf("non-regular Scenario Result report = %+v", view)
	}
}

func TestEvidenceReportMissingArtifactCannotPass(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if err := os.Remove(filepath.Join(bundleDir, "outputs", "result.txt")); err != nil {
		t.Fatal(err)
	}
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "artifact-failing" || !containsReportMissingEvidence(view, "artifact outputs/result.txt is missing") {
		t.Fatalf("missing-artifact report = %+v", view)
	}
}

func TestEvidenceReportScansMissingArtifactBeyondDisplayCap(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Outputs = nil
	for index := 0; index < maximumReportArtifacts+10; index++ {
		relative := fmt.Sprintf("outputs/item-%03d.txt", index)
		bundle.Outputs = append(bundle.Outputs, outputArtifact{Path: relative, MediaType: "text/plain"})
		if index < maximumReportArtifacts+9 {
			mustWriteFile(t, filepath.Join(bundleDir, filepath.FromSlash(relative)), "available\n")
		}
	}
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	if err := writeJSONFile(filepath.Join(bundleDir, evidenceBundleFileName), bundle); err != nil {
		t.Fatal(err)
	}
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("artifact outputs/item-%03d.txt is missing", maximumReportArtifacts+9)
	if view.Verdict != "artifact-failing" || !view.Truncated || !containsReportMissingEvidence(view, want) || view.Counts.Artifacts <= len(view.Artifacts) {
		t.Fatalf("post-cap missing artifact report = %+v", view)
	}
}

func TestEvidenceReportDetectsUnreadableAndZeroByteSizeMismatch(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	output := filepath.Join(bundleDir, "outputs", "result.txt")
	if err := os.Chmod(output, 0o000); err != nil {
		t.Fatal(err)
	}
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "artifact-failing" || !containsReportMissingEvidence(view, "artifact outputs/result.txt is unreadable") {
		if err := os.Chmod(output, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Skipf("test user can read mode-000 files; view = %+v", view)
	}
	if err := os.Chmod(output, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Artifacts = []evidenceArtifact{{Label: "outputs", Kind: "output", Path: "outputs/result.txt", MediaType: "text/plain", Size: 10}}
	bundle.LifecycleState, bundle.CleanupState = "completed", "completed"
	view, err = buildEvidenceReportView(bundleDir, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "artifact-failing" || !containsReportMissingEvidence(view, "artifact outputs/result.txt is size-mismatch") {
		t.Fatalf("zero-byte size mismatch report = %+v", view)
	}
}

func TestEvidenceReportRejectsTrailingAndOversizedScenarioResult(t *testing.T) {
	tests := []struct {
		name      string
		extra     string
		want      string
		truncated bool
	}{
		{name: "trailing", extra: `{"other":true}`, want: "Scenario Result is malformed"},
		{name: "oversized", extra: strings.Repeat(" ", maximumReportBytes), want: "Scenario Result exceeds the report input limit", truncated: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			bundleDir := writeReportFixture(t, reportFixtureOptions{})
			path := filepath.Join(bundleDir, evidenceScenarioResultFileName)
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.WriteString(test.extra); err != nil {
				_ = file.Close()
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
			view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
			if err != nil {
				t.Fatal(err)
			}
			if view.Verdict != "artifact-failing" || view.Truncated != test.truncated || !containsReportMissingEvidence(view, test.want) {
				t.Fatalf("invalid Scenario Result report = %+v", view)
			}
		})
	}
}

func TestEvidenceReportRejectsStructurallyInvalidScenarioResult(t *testing.T) {
	tests := []string{
		`null`,
		`{"status":"passed","assertions":[null]}`,
		`{"status":"passed","assertions":{"name":"wrong"}}`,
		`{"status":"passed","assertions":[{"name":"wrong","passed":"yes"}]}`,
		`{"status":"passed","steps":[{"status":1}]}`,
	}
	for index, document := range tests {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			bundleDir := writeReportFixture(t, reportFixtureOptions{})
			mustWriteFile(t, filepath.Join(bundleDir, evidenceScenarioResultFileName), document+"\n")
			view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
			if err != nil {
				t.Fatal(err)
			}
			if view.Verdict != "artifact-failing" || !containsReportMissingEvidence(view, "Scenario Result is malformed") {
				t.Fatalf("structurally invalid Scenario Result report = %+v", view)
			}
		})
	}
}

func TestEvidenceReportRejectsScenarioResultStatusMismatch(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	mustWriteFile(t, filepath.Join(bundleDir, evidenceScenarioResultFileName), `{"status":"failed","summary":"contradiction"}`+"\n")
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "artifact-failing" || !containsReportMissingEvidence(view, "Scenario Result status disagrees with Evidence Bundle status") {
		t.Fatalf("Scenario Result status mismatch report = %+v", view)
	}
}

func TestEvidenceReportRefreshesControlPlaneCleanupFailure(t *testing.T) {
	repoDir := t.TempDir()
	source := writeReportFixture(t, reportFixtureOptions{})
	bundleDir := filepath.Join(repoDir, "artifacts", "maya-stall", "run-report")
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, bundleDir); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	failure := &runFailureEvidence{FailedLayer: "cleanup", Diagnostic: "cleanup failed", CleanupState: "failed"}
	if err := writeControlPlaneCleanupFailureEvidence(repoDir, "run-report", failure); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	view, err := buildEvidenceReportView(bundleDir, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "cleanup-failed" || view.Lifecycle != "cleanup-failed" || view.Cleanup != "failed" {
		t.Fatalf("cleanup-failed report = %+v", view)
	}
}

func TestEvidenceReportMarksLaterStopLedgerFailure(t *testing.T) {
	repoDir := t.TempDir()
	source := writeReportFixture(t, reportFixtureOptions{})
	bundleDir := filepath.Join(repoDir, "artifacts", "maya-stall", "run-report")
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, bundleDir); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "kept", Cleanup: "retained"}); err != nil {
		t.Fatal(err)
	}
	ledgerErr := errors.New("durable ledger unavailable")
	if err := finalizeStoppedRunEvidenceReport(repoDir, "run-report", nil, ledgerErr); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	view, err := buildEvidenceReportView(bundleDir, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Status != resultStatusFailed || bundle.Failure == nil || !strings.Contains(bundle.Failure.Diagnostic, ledgerErr.Error()) {
		t.Fatalf("stop ledger failure bundle = %+v", bundle)
	}
	if view.Lifecycle != "failed" || view.Cleanup != "completed" || view.Verdict == "passed" {
		t.Fatalf("stop ledger failure report = %+v", view)
	}
}

func TestEvidenceReportPreservesCombinedStopAndLedgerFailures(t *testing.T) {
	repoDir := t.TempDir()
	source := writeReportFixture(t, reportFixtureOptions{})
	bundleDir := filepath.Join(repoDir, "artifacts", "maya-stall", "run-report")
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, bundleDir); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "kept", Cleanup: "retained"}); err != nil {
		t.Fatal(err)
	}
	stopErr := errors.New("session cleanup failed")
	ledgerErr := errors.New("durable ledger unavailable")
	if err := finalizeStoppedRunEvidenceReport(repoDir, "run-report", stopErr, ledgerErr); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Failure == nil || !strings.Contains(bundle.Failure.Diagnostic, stopErr.Error()) || !strings.Contains(bundle.Failure.Diagnostic, ledgerErr.Error()) || bundle.Failure.CleanupState != "failed" {
		t.Fatalf("combined stop and ledger failure bundle = %+v", bundle)
	}
}

func TestEvidenceReportPersistsLaterStopFailure(t *testing.T) {
	repoDir := t.TempDir()
	source := writeReportFixture(t, reportFixtureOptions{})
	bundleDir := filepath.Join(repoDir, "artifacts", "maya-stall", "run-report")
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, bundleDir); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "kept", Cleanup: "retained"}); err != nil {
		t.Fatal(err)
	}
	stopErr := errors.New("session cleanup failed")
	if err := finalizeStoppedRunEvidenceReport(repoDir, "run-report", stopErr, nil); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Status != resultStatusFailed || bundle.Failure == nil || !strings.Contains(bundle.Failure.Diagnostic, stopErr.Error()) || bundle.Failure.CleanupState != "failed" {
		t.Fatalf("stop failure bundle = %+v", bundle)
	}
}

func TestEvidenceReportInvalidatesKeptGenerationWhenLaterStopRefreshFails(t *testing.T) {
	repoDir := t.TempDir()
	source := writeReportFixture(t, reportFixtureOptions{})
	bundleDir := filepath.Join(repoDir, "artifacts", "maya-stall", "run-report")
	if err := os.MkdirAll(filepath.Dir(bundleDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(source, bundleDir); err != nil {
		t.Fatal(err)
	}
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "kept", Cleanup: "retained"}); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(bundleDir, evidenceManifestFileName)
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manifestPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := finalizeStoppedRunEvidenceReport(repoDir, "run-report", nil, nil); err == nil {
		t.Fatal("later stop report refresh unexpectedly succeeded")
	}
	if _, err := os.Stat(filepath.Join(bundleDir, evidenceReportFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale kept report remained after stop refresh failure: %v", err)
	}
}

func TestEvidenceReportOmitsUnsafeArtifactLinks(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{ArtifactPath: "../outside.txt"})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	content := string(mustReadReportFile(t, filepath.Join(bundleDir, evidenceReportFileName)))
	if strings.Contains(content, "outside.txt") || strings.Contains(content, `href="..`) {
		t.Fatalf("report exposed unsafe artifact path:\n%s", content)
	}
}

func TestEvidenceReportEnforcesSizeBudget(t *testing.T) {
	view := reportView{Version: reportViewVersion, RunID: "run-report", Summary: strings.Repeat("x", maximumReportBytes)}
	content, err := renderEvidenceReportHTML(view)
	if err != nil || len(content) >= maximumReportBytes || !bytes.Contains(content, []byte("[truncated]")) {
		t.Fatalf("bounded oversized report = bytes %d err %v", len(content), err)
	}
}

func TestEvidenceReportBoundsLargeStructuredInput(t *testing.T) {
	items := make([]reportAssertionView, maximumReportItems+20)
	for index := range items {
		items[index] = reportAssertionView{Name: strings.Repeat("n", maximumReportText+20), Summary: strings.Repeat("s", maximumReportText+20)}
	}
	view := boundEvidenceReportView(reportView{Version: reportViewVersion, RunID: "run-report", Assertions: items, Counts: reportCounts{Assertions: len(items)}})
	content, err := renderEvidenceReportHTML(view)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Truncated || len(view.Assertions) != maximumReportItems || len(content) >= maximumReportBytes {
		t.Fatalf("bounded report = truncated %t assertions %d bytes %d", view.Truncated, len(view.Assertions), len(content))
	}
}

func TestEvidenceReportAggregateBudgetTruncatesDeterministically(t *testing.T) {
	text := strings.Repeat("x", maximumReportText)
	view := reportView{Version: reportViewVersion, RunID: "run-report", Verdict: "passed"}
	for index := 0; index < maximumReportItems; index++ {
		view.Steps = append(view.Steps, reportStepView{ID: text, Name: text, Status: text, Summary: text})
		view.Assertions = append(view.Assertions, reportAssertionView{Name: text, Status: resultStatusPassed, Summary: text})
		view.Measurements = append(view.Measurements, reportMeasurementView{Name: text, Value: text, Unit: text, Threshold: text, Passed: text})
		view.Validators = append(view.Validators, validatorResult{Type: text, Status: resultStatusPassed, Message: text})
	}
	for index := 0; index < maximumReportArtifacts; index++ {
		view.Artifacts = append(view.Artifacts, reportArtifactView{Label: text, Kind: text, Path: text, Link: text, MediaType: text, SHA256: text, RecordedSHA256: text, State: "available"})
	}
	first := boundEvidenceReportView(view)
	second := boundEvidenceReportView(view)
	firstBytes, err := renderEvidenceReportHTML(first)
	if err != nil {
		t.Fatal(err)
	}
	secondBytes, err := renderEvidenceReportHTML(second)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Truncated || len(firstBytes) >= maximumReportBytes || !bytes.Equal(firstBytes, secondBytes) || len(first.Artifacts) == maximumReportArtifacts {
		t.Fatalf("aggregate budget = truncated %t artifacts %d bytes %d deterministic %t", first.Truncated, len(first.Artifacts), len(firstBytes), bytes.Equal(firstBytes, secondBytes))
	}
}

func TestEvidenceReportGoldenDOMContract(t *testing.T) {
	view := boundEvidenceReportView(reportView{
		Version: reportViewVersion, Verdict: "passed", Status: "passed", Scenario: "smoke", RunID: "run-report",
		Lifecycle: "completed", Cleanup: "completed", Confidentiality: "private", WorkflowState: "not-reached",
		Failure: reportFailureView{}, Counts: reportCounts{}, SchemaVersions: reportSchemaVersions{View: 1, Evidence: 1, Manifest: 1},
	})
	content, err := renderEvidenceReportHTML(view)
	if err != nil {
		t.Fatal(err)
	}
	document := string(content)
	for _, want := range []string{
		`<!doctype html>`, `<html lang="en">`, `<main><h1>`, `<section id="diagnosis"><h2>`,
		`<section id="workflow"><h2>`, `<section id="proof"><h2>`, `<section id="integrity"><h2>`,
		`<dl>`, `<table>`, `Verdict: passed`, `Named steps and checkpoints were not reached`,
	} {
		if !strings.Contains(document, want) {
			t.Fatalf("golden DOM missing %q:\n%s", want, document)
		}
	}
}

func TestEvidenceReportMissingFieldsRemainExplicit(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{NoResult: true})
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "failed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Revision != "not recorded" || view.MayaIdentity != "not recorded" || view.PluginIdentity != "not recorded" || view.Duration != "not recorded" {
		t.Fatalf("missing identity fields are ambiguous: %+v", view)
	}
}

func TestEvidenceReportUnknownAssertionIsNotFailedOrPassed(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	mustWriteFile(t, filepath.Join(bundleDir, evidenceScenarioResultFileName), `{"status":"passed","assertions":[{"name":"unknown assertion"}]}`+"\n")
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.Verdict != "artifact-failing" || view.Counts.UnknownAssertions != 1 || view.Counts.FailedAssertions != 0 || view.Assertions[0].Status != "unknown" {
		t.Fatalf("unknown assertion report = %+v", view)
	}
}

func containsReportMissingEvidence(view reportView, want string) bool {
	for _, item := range view.MissingEvidence {
		if item == want {
			return true
		}
	}
	return false
}

func TestEvidenceReportCommandDoesNotMutateVerifiedBundle(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	before := reportFixtureHashes(t, bundleDir)
	output := filepath.Join(t.TempDir(), "rendered.html")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"evidence", "report", "--output", output, bundleDir}, &stdout, &stderr, "", "test-version"); code != 0 {
		t.Fatalf("evidence report exit = %d; stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "verdict: passed") || !strings.Contains(stdout.String(), "report: "+output) {
		t.Fatalf("evidence report output = %s", stdout.String())
	}
	if !bytes.Equal(mustReadReportFile(t, output), mustReadReportFile(t, filepath.Join(bundleDir, evidenceReportFileName))) {
		t.Fatal("read-only render bytes differ from finalized report")
	}
	after := reportFixtureHashes(t, bundleDir)
	if before != after {
		t.Fatalf("read-only render changed bundle: before %v after %v", before, after)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"evidence", "report", "--output", filepath.Join(bundleDir, "copy.html"), bundleDir}, &stdout, &stderr, "", "test-version"); code != 1 || !strings.Contains(stderr.String(), "outside the verified Evidence Bundle") {
		t.Fatalf("in-bundle report exit = %d; stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
}

func TestEvidenceReportVerdictsCoverTerminalStates(t *testing.T) {
	tests := []struct {
		name   string
		bundle evidenceBundle
		view   reportView
		want   string
	}{
		{name: "passing", bundle: evidenceBundle{Status: "passed"}, view: reportView{Lifecycle: "completed", Cleanup: "completed"}, want: "passed"},
		{name: "product", bundle: evidenceBundle{Status: "failed", Failure: &runFailureEvidence{FailedLayer: "execution"}}, view: reportView{Lifecycle: "failed", Cleanup: "completed"}, want: "product-failing"},
		{name: "artifact", bundle: evidenceBundle{Status: "failed", Failure: &runFailureEvidence{FailedLayer: "run-state"}}, view: reportView{Lifecycle: "failed", Cleanup: "completed"}, want: "artifact-failing"},
		{name: "host", bundle: evidenceBundle{Status: "failed", Failure: &runFailureEvidence{FailedLayer: "host-selection"}}, view: reportView{Lifecycle: "failed", Cleanup: "completed"}, want: "host-failing"},
		{name: "transport", bundle: evidenceBundle{Status: "failed", Failure: &runFailureEvidence{FailedLayer: "remote-check"}}, view: reportView{Lifecycle: "failed", Cleanup: "completed"}, want: "transport-failing"},
		{name: "timeout", bundle: evidenceBundle{Status: "failed", Failure: &runFailureEvidence{FailedLayer: "execution", Diagnostic: "timed out"}}, view: reportView{Lifecycle: "failed", Cleanup: "completed"}, want: "timeout"},
		{name: "kept", bundle: evidenceBundle{Status: "failed"}, view: reportView{Lifecycle: "kept", Cleanup: "retained"}, want: "kept"},
		{name: "cleanup", bundle: evidenceBundle{Status: "passed"}, view: reportView{Lifecycle: "cleanup-failed", Cleanup: "failed"}, want: "cleanup-failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := reportVerdict(test.bundle, test.view); got != test.want {
				t.Fatalf("verdict = %q, want %q", got, test.want)
			}
		})
	}
}

func TestEvidenceReportMakesNotReachedAndTruncatedExplicit(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{NoResult: true, Events: `{"type":"events-truncated"}` + "\n"})
	view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "failed", Cleanup: "completed"})
	if err != nil {
		t.Fatal(err)
	}
	if view.WorkflowState != "not-reached" || !view.Truncated || len(view.MissingEvidence) == 0 {
		t.Fatalf("not-reached/truncated view = %+v", view)
	}
}

func TestEvidenceReportPublicationRejectsTamperedReport(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundleDir, evidenceReportFileName), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = buildPublishedArtifactManifest(bundleDir, bundle, "https://evidence.example.test")
	if err == nil || !strings.Contains(err.Error(), "report artifact") {
		t.Fatalf("tampered report verification error = %v", err)
	}
}

func TestEvidenceReportPublicationRejectsStaleCanonicalProjection(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Status = resultStatusFailed
	bundle.Report = nil
	if err := writeJSONFile(filepath.Join(bundleDir, evidenceBundleFileName), bundle); err != nil {
		t.Fatal(err)
	}
	bundle, err = readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildPublishedArtifactManifest(bundleDir, bundle, "https://proof.example.test"); err == nil || !strings.Contains(err.Error(), "does not project current canonical evidence") {
		t.Fatalf("stale canonical projection error = %v", err)
	}
}

func TestEvidenceReportPublicationRejectsUnmanifestedReport(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	mustWriteFile(t, filepath.Join(bundleDir, evidenceReportFileName), "<!doctype html><title>unverified</title>\n")
	if _, err := readEvidenceBundleFile(bundleDir); err == nil || !strings.Contains(err.Error(), "without manifest authority") {
		t.Fatalf("unmanifested report error = %v", err)
	}
}

func TestEvidenceReportRejectsNonRegularReportLeafAndSafelyInvalidatesSymlink(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	outside := filepath.Join(t.TempDir(), "outside.html")
	mustWriteFile(t, outside, "outside\n")
	if err := os.Symlink(outside, filepath.Join(bundleDir, evidenceReportFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvidenceBundleFile(bundleDir); err == nil || !strings.Contains(err.Error(), "must be a regular file") {
		t.Fatalf("non-regular report leaf error = %v", err)
	}
	if err := invalidateEvidenceReport(bundleDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(bundleDir, evidenceReportFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("report symlink remained after invalidation: %v", err)
	}
	if content, err := os.ReadFile(outside); err != nil || string(content) != "outside\n" {
		t.Fatalf("invalidation touched symlink target: content=%q err=%v", content, err)
	}
}

func TestEvidenceReportPublicationRejectsMutableReportKind(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	var manifest runManifest
	readJSONFile(t, filepath.Join(bundleDir, evidenceManifestFileName), &manifest)
	manifest.Report.Kind = "metadata"
	if err := writeJSONFile(filepath.Join(bundleDir, evidenceManifestFileName), manifest); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildPublishedArtifactManifest(bundleDir, bundle, "https://evidence.example.test"); err == nil || !strings.Contains(err.Error(), "report metadata is invalid") {
		t.Fatalf("mutable report kind error = %v", err)
	}
}

func TestEvidenceReportPublicationRejectsAdditionalReportKind(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(bundleDir, "outputs", "fake.html"), "not verified\n")
	bundle.Artifacts = append(bundle.Artifacts, evidenceArtifact{Label: "fake", Kind: "report", Path: "outputs/fake.html", MediaType: reportMediaType})
	if _, err := buildPublishedArtifactManifest(bundleDir, bundle, "https://evidence.example.test"); err == nil || !strings.Contains(err.Error(), "unverified report artifact") {
		t.Fatalf("additional report-kind error = %v", err)
	}
}

func TestEvidenceReportPublishedManifestOwnsPrimaryDetailPath(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := buildPublishedArtifactManifest(bundleDir, bundle, "https://evidence.example.test")
	if err != nil {
		t.Fatal(err)
	}
	markdown := renderReviewMarkdownFromManifest(manifest)
	if manifest.ReportPath != evidenceReportFileName || !strings.Contains(markdown, "details: [report.html]") {
		t.Fatalf("published report detail contract = manifest %+v markdown %s", manifest, markdown)
	}
}

func TestEvidenceReportRejectsMalformedAuthoritativeManifest(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(bundleDir, evidenceManifestFileName), "{malformed\n")
	if _, err := readEvidenceBundleFile(bundleDir); err == nil || !strings.Contains(err.Error(), "parse Evidence Bundle manifest") {
		t.Fatalf("malformed authoritative manifest error = %v", err)
	}
}

func TestEvidenceReportRejectsManifestWithoutRunIdentity(t *testing.T) {
	bundleDir := writeReportFixture(t, reportFixtureOptions{})
	if _, err := finalizeEvidenceReport(bundleDir, reportTerminalState{Lifecycle: "completed", Cleanup: "completed"}); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(bundleDir, evidenceManifestFileName), `{}`+"\n")
	if _, err := readEvidenceBundleFile(bundleDir); err == nil || !strings.Contains(err.Error(), "without manifest authority") {
		t.Fatalf("empty manifest identity error = %v", err)
	}
}

func TestEvidenceReportRejectsAlternateManifestPathAndIdentity(t *testing.T) {
	t.Run("alternate path", func(t *testing.T) {
		bundleDir := writeReportFixture(t, reportFixtureOptions{})
		bundle, err := readEvidenceBundleFile(bundleDir)
		if err != nil {
			t.Fatal(err)
		}
		bundle.Manifest = "alternate-manifest.json"
		if err := writeJSONFile(filepath.Join(bundleDir, evidenceBundleFileName), bundle); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(bundleDir, bundle.Manifest), `{"version":1,"runId":"run-report","scenario":"smoke"}`+"\n")
		if _, err := readEvidenceBundleFile(bundleDir); err == nil || !strings.Contains(err.Error(), "canonical manifest path") {
			t.Fatalf("alternate manifest path error = %v", err)
		}
	})
	t.Run("mismatched scenario", func(t *testing.T) {
		bundleDir := writeReportFixture(t, reportFixtureOptions{})
		var manifest runManifest
		readJSONFile(t, filepath.Join(bundleDir, evidenceManifestFileName), &manifest)
		manifest.Scenario = "other"
		if err := writeJSONFile(filepath.Join(bundleDir, evidenceManifestFileName), manifest); err != nil {
			t.Fatal(err)
		}
		if _, err := readEvidenceBundleFile(bundleDir); err == nil || !strings.Contains(err.Error(), "identifies another run") {
			t.Fatalf("mismatched manifest identity error = %v", err)
		}
	})
}

func TestRunJSONAndHTMLUseSameReportVerdictAndCounts(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"run", "--json", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit = %d; stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'})
	var terminal runCommandJSON
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.Report == nil {
		t.Fatal("terminal JSON omitted shared report view")
	}
	bundleDir := terminal.EvidenceDir
	if !filepath.IsAbs(bundleDir) {
		bundleDir = filepath.Join(dir, bundleDir)
	}
	bundle, err := readEvidenceBundleFile(bundleDir)
	if err != nil {
		t.Fatal(err)
	}
	view, err := buildEvidenceReportView(bundleDir, bundle)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Report.Verdict != view.Verdict || terminal.Report.Counts != view.Counts {
		t.Fatalf("terminal report = %+v, HTML view = %+v", terminal.Report, view)
	}
}

func TestRunFailsAndRemovesStaleHTMLWhenTerminalReportCannotFinalize(t *testing.T) {
	dir := writeRunConfigFixture(t)
	runtime := defaultRunRuntime()
	runtime.Broker = reportManifestBlockingBroker{fakeSessionBroker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "Scenario passed"}}}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--json", "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 {
		t.Fatalf("run exit = %d; stdout %s stderr %s", code, stdout.String(), stderr.String())
	}
	lines := bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte{'\n'})
	var terminal runCommandJSON
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil {
		t.Fatal(err)
	}
	if terminal.Status != resultStatusFailed || terminal.Report == nil || terminal.Report.Verdict == "passed" {
		t.Fatalf("terminal report finalization failure = %+v", terminal)
	}
	resultBytes := mustReadReportFile(t, filepath.Join(terminal.EvidenceDir, evidenceScenarioResultFileName))
	if !bytes.Contains(resultBytes, []byte(`"status": "passed"`)) {
		t.Fatalf("report artifact failure changed Scenario Result: %s", resultBytes)
	}
	if _, err := os.Stat(filepath.Join(terminal.EvidenceDir, evidenceReportFileName)); !os.IsNotExist(err) {
		t.Fatalf("stale report remained after persistent finalization failure: %v", err)
	}
}

type reportManifestBlockingBroker struct {
	fakeSessionBroker
}

func (broker reportManifestBlockingBroker) StopSession(context runContext, session brokerSessionIdentity) error {
	if err := broker.fakeSessionBroker.StopSession(context, session); err != nil {
		return err
	}
	path := filepath.Join(context.EvidenceDir, evidenceManifestFileName)
	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Mkdir(path, 0o700)
}

type reportFixtureOptions struct {
	Scenario     string
	Summary      string
	ArtifactPath string
	NoResult     bool
	Events       string
}

func writeReportFixture(t *testing.T, options reportFixtureOptions) string {
	t.Helper()
	dir := t.TempDir()
	if options.Scenario == "" {
		options.Scenario = "smoke"
	}
	if options.Summary == "" {
		options.Summary = "Scenario passed"
	}
	if options.ArtifactPath == "" {
		options.ArtifactPath = "outputs/result.txt"
	}
	if options.Events == "" {
		options.Events = `{"sequence":1,"type":"run.completed"}` + "\n"
	}
	manifestBytes, err := json.Marshal(runManifest{
		Version: 1, RunID: "run-report", Scenario: options.Scenario, TargetProfile: "default",
	})
	if err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, evidenceManifestFileName), string(manifestBytes)+"\n")
	mustWriteFile(t, filepath.Join(dir, evidenceEventsFileName), options.Events)
	mustWriteFile(t, filepath.Join(dir, evidenceLogPath), "bounded log\n")
	mustWriteFile(t, filepath.Join(dir, filepath.FromSlash(options.ArtifactPath)), "artifact\n")
	resultPath := ""
	if !options.NoResult {
		resultPath = evidenceScenarioResultFileName
		result := map[string]any{
			"status": "passed", "summary": options.Summary,
			"assertions":   []map[string]any{{"name": "plug-in loaded", "passed": true}},
			"measurements": []map[string]any{{"name": "duration", "value": 2.5, "unit": "s", "threshold": 3}},
		}
		content, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(dir, resultPath), string(content)+"\n")
	}
	bundle := evidenceBundle{
		Version: 1, RunID: "run-report", Scenario: options.Scenario, Status: "passed", TargetProfile: "default",
		Manifest: evidenceManifestFileName, Events: evidenceEventsFileName, Log: evidenceLogPath, ScenarioResult: resultPath,
		Outputs:    []outputArtifact{{Path: options.ArtifactPath, MediaType: "text/plain"}},
		Validators: []validatorResult{{Type: "scenarioResultStatus", Status: "passed", Message: "passed"}},
	}
	bundle.Artifacts = buildEvidenceBundleCatalog(bundle)
	if err := writeJSONFile(filepath.Join(dir, evidenceBundleFileName), bundle); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustReadReportFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func reportFixtureHashes(t *testing.T, bundleDir string) [4]string {
	t.Helper()
	var hashes [4]string
	for index, name := range []string{evidenceBundleFileName, evidenceManifestFileName, evidenceScenarioResultFileName, evidenceReportFileName} {
		hash, err := fileSHA256Hex(filepath.Join(bundleDir, name))
		if err != nil {
			t.Fatal(err)
		}
		hashes[index] = hash
	}
	return hashes
}
