package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunExpiresOverdueKeptSessionBeforeHostLock(t *testing.T) {
	dir := writeRunConfigFixture(t)
	keptAt := time.Date(2026, time.July, 22, 9, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return keptAt }
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: resultStatusFailed, Summary: "keep for inspection"}}

	var stdout, stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--keep-on-failure", "smoke"}, &stdout, &stderr, dir, "test-version", runtime); code != 1 {
		t.Fatalf("kept run exit code = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	keptRunID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID, "run-record.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read kept Run Record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse kept Run Record: %v", err)
	}
	record["keepTTL"] = "90m0s"
	record["keepDeadline"] = keptAt.Add(-time.Minute).Format(time.RFC3339Nano)
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("make kept Run Record overdue: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	runtime.Now = func() time.Time { return keptAt.Add(time.Hour) }
	runtime.Broker = fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "successor completed"}}
	if code := RunWithRuntime([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version", runtime); code != 0 {
		t.Fatalf("successor run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID)); !os.IsNotExist(err) {
		t.Fatalf("expired kept Run State = %v, want removed", err)
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, keptRunID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read expired run ledger events: %v", err)
	}
	for _, want := range []string{
		`"type":"kept-session-expired"`,
		`"runId":"` + keptRunID + `"`,
		`"host":"fake-local"`,
		`"deadline":"` + keptAt.Add(-time.Minute).Format(time.RFC3339Nano) + `"`,
		`"outcome":"broker-cleaned"`,
	} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("expired run ledger events missing %s:\n%s", want, events)
		}
	}
}

func TestDoctorExpiresOverdueKeptSessionBeforeHostChecks(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	keptRunID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID, "run-record.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read kept Run Record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse kept Run Record: %v", err)
	}
	record["keepDeadline"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("make kept Run Record overdue: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"doctor", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("doctor exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID)); !os.IsNotExist(err) {
		t.Fatalf("expired kept Run State = %v, want removed", err)
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(dir, keptRunID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read expired run ledger events: %v", err)
	}
	if !strings.Contains(string(events), `"type":"kept-session-expired"`) {
		t.Fatalf("expired run ledger events missing kept-session-expired:\n%s", events)
	}
}

func TestKeptSessionWithFutureDeadlineIsUntouched(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	keptRunID := runIDFromOutput(t, stdout.String())
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID)

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"doctor", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 1 {
		t.Fatalf("doctor exit code = %d, want locked-host failure; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("future kept Run State changed: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(stateDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read future kept run events: %v", err)
	}
	if strings.Contains(string(events), "kept-session-expired") {
		t.Fatalf("future kept run was expired:\n%s", events)
	}
}

func TestLegacyKeptSessionGetsGraceThenExpiresOnLaterContact(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	keptRunID := runIDFromOutput(t, stdout.String())
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID)
	recordPath := filepath.Join(stateDir, "run-record.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read kept Run Record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse kept Run Record: %v", err)
	}
	delete(record, "keepTTL")
	delete(record, "keepDeadline")
	delete(record, "hardDeadline")
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("write legacy kept Run Record: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"doctor", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 1 {
		t.Fatalf("first doctor exit code = %d, want kept-host failure; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stamped := readRunRetentionRecordFile(t, recordPath)
	if stamped.KeepTTL != defaultKeepTTL.String() || stamped.KeepDeadline == "" || stamped.HardDeadline == "" {
		t.Fatalf("legacy grace stamp = ttl %q deadline %q hard deadline %q", stamped.KeepTTL, stamped.KeepDeadline, stamped.HardDeadline)
	}
	events, err := os.ReadFile(filepath.Join(stateDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read grace-stamped events: %v", err)
	}
	if !strings.Contains(string(events), `"type":"kept-session-grace-stamped"`) {
		t.Fatalf("grace-stamped event missing:\n%s", events)
	}
	contactAt, err := time.Parse(time.RFC3339Nano, stamped.KeepDeadline)
	if err != nil {
		t.Fatalf("parse grace deadline: %v", err)
	}
	contactAt = contactAt.Add(-defaultKeepTTL)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return contactAt }
	if _, err := extendRunThroughMode(dir, extendOptions{RunID: keptRunID, By: time.Minute}, runtime); err != nil {
		t.Fatalf("extend grace-stamped legacy session: %v", err)
	}

	content, err = os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read grace-stamped Run Record: %v", err)
	}
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse grace-stamped Run Record: %v", err)
	}
	record["keepDeadline"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("advance grace deadline: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"doctor", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("later doctor exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("expired legacy Run State = %v, want removed", err)
	}
}

func TestLegacyGraceIgnoresSuccessorKeepTTL(t *testing.T) {
	dir := writeRunConfigFixture(t)
	keptAt := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return keptAt }
	var stdout, stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runtime); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	keptRunID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID, "run-record.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read kept Run Record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse kept Run Record: %v", err)
	}
	delete(record, "keepTTL")
	delete(record, "keepDeadline")
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("write legacy kept Run Record: %v", err)
	}

	contactAt := keptAt.Add(time.Hour)
	runtime.Now = func() time.Time { return contactAt }
	stdout.Reset()
	stderr.Reset()
	if code := RunWithRuntime([]string{"run", "--keep-ttl", "1m", "smoke"}, &stdout, &stderr, dir, "test-version", runtime); code != 1 {
		t.Fatalf("successor run exit code = %d, want locked-host failure; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stamped := readRunRetentionRecordFile(t, recordPath)
	if stamped.KeepTTL != defaultKeepTTL.String() || stamped.KeepDeadline != contactAt.Add(defaultKeepTTL).Format(time.RFC3339Nano) {
		t.Fatalf("legacy grace inherited successor TTL: ttl=%q deadline=%q", stamped.KeepTTL, stamped.KeepDeadline)
	}
}

func TestLegacyKeepDeadlineIsClampedToMigratedHardLifetime(t *testing.T) {
	dir := writeRunConfigFixture(t)
	keptAt := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return keptAt }
	var stdout, stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runtime); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")
	record := readRunRetentionRecordFile(t, recordPath)
	contactAt := keptAt.Add(time.Hour)
	record.HardDeadline = ""
	record.KeepDeadline = contactAt.Add(24 * time.Hour).Format(time.RFC3339Nano)
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy Run Record: %v", err)
	}
	if err := writeRunLedgerBytes(recordPath, append(content, '\n')); err != nil {
		t.Fatalf("write legacy Run Record: %v", err)
	}

	if warnings := sweepKeptSessions(dir, record.Host, contactAt); len(warnings) != 0 {
		t.Fatalf("migrate legacy hard lifetime warnings = %v", warnings)
	}
	migrated := readRunRetentionRecordFile(t, recordPath)
	wantHard := contactAt.Add(defaultHostLockHardLifetime).Format(time.RFC3339Nano)
	if migrated.HardDeadline != wantHard || migrated.KeepDeadline != wantHard {
		t.Fatalf("migrated deadlines = keep %q hard %q, want %q", migrated.KeepDeadline, migrated.HardDeadline, wantHard)
	}
}

func TestEmbeddedKeepTTLRejectsHardLifetimeOverflow(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--stop-after", "never", "--keep-ttl", "7h", "smoke"}, &stdout, &stderr, dir, "test-version", defaultRunRuntime())
	if code != 2 || !strings.Contains(stderr.String(), "exceeds embedded Kept Session retention limit 6h0m0s") {
		t.Fatalf("embedded over-cap keep TTL = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = RunWithRuntime([]string{"run", "--stop-after", "always", "--keep-ttl", "7h", "smoke"}, &stdout, &stderr, dir, "test-version", defaultRunRuntime())
	if code != 0 {
		t.Fatalf("cleanup-guaranteed embedded run rejected irrelevant keep TTL = code %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

func TestRejectedLegacyExtensionStillPersistsFirstContactHardDeadline(t *testing.T) {
	dir := writeRunConfigFixture(t)
	start := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return start }
	var stdout, stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runtime); code != 0 {
		t.Fatalf("kept run exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")
	record := readRunRetentionRecordFile(t, recordPath)
	contactAt := start.Add(time.Hour)
	record.HardDeadline = ""
	record.KeepDeadline = contactAt.Add(24 * time.Hour).Format(time.RFC3339Nano)
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy Run Record: %v", err)
	}
	if err := writeRunLedgerBytes(recordPath, append(content, '\n')); err != nil {
		t.Fatalf("write legacy Run Record: %v", err)
	}
	runtime.Now = func() time.Time { return contactAt }
	if _, err := extendRunThroughMode(dir, extendOptions{RunID: runID, By: defaultHostLockHardLifetime}, runtime); err == nil {
		t.Fatal("over-policy legacy extension succeeded")
	}
	first := readRunRetentionRecordFile(t, recordPath)
	wantHard := contactAt.Add(defaultHostLockHardLifetime).Format(time.RFC3339Nano)
	if first.HardDeadline != wantHard || first.KeepDeadline != wantHard {
		t.Fatalf("first-contact deadlines = keep %q hard %q, want %q", first.KeepDeadline, first.HardDeadline, wantHard)
	}
	runtime.Now = func() time.Time { return contactAt.Add(time.Hour) }
	if _, err := extendRunThroughMode(dir, extendOptions{RunID: runID, By: defaultHostLockHardLifetime}, runtime); err == nil {
		t.Fatal("later over-policy legacy extension succeeded")
	}
	if later := readRunRetentionRecordFile(t, recordPath); later.HardDeadline != wantHard {
		t.Fatalf("legacy hard deadline moved from %q to %q", wantHard, later.HardDeadline)
	}
}

func TestConcurrentEmbeddedExtensionsAreSerialized(t *testing.T) {
	dir := writeRunConfigFixture(t)
	now := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return now }
	var stdout, stderr bytes.Buffer
	if code := RunWithRuntime([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version", runtime); code != 0 {
		t.Fatalf("kept run exit code = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")
	before := readRunRetentionRecordFile(t, recordPath)
	initial, err := time.Parse(time.RFC3339Nano, before.KeepDeadline)
	if err != nil {
		t.Fatalf("parse initial keep deadline: %v", err)
	}
	var group sync.WaitGroup
	errorsByAttempt := make(chan error, 2)
	for attempt := 0; attempt < 2; attempt++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := extendRunThroughMode(dir, extendOptions{RunID: runID, By: time.Minute}, runtime)
			errorsByAttempt <- err
		}()
	}
	group.Wait()
	close(errorsByAttempt)
	for err := range errorsByAttempt {
		if err != nil {
			t.Fatalf("concurrent extension: %v", err)
		}
	}
	after := readRunRetentionRecordFile(t, recordPath)
	if want := initial.Add(2 * time.Minute).Format(time.RFC3339Nano); after.KeepDeadline != want {
		t.Fatalf("serialized keep deadline = %q, want %q", after.KeepDeadline, want)
	}
}

func TestInvalidKeepDeadlineWarnsAndRecordsEvent(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", runID)
	recordPath := filepath.Join(stateDir, "run-record.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read kept Run Record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse kept Run Record: %v", err)
	}
	record["keepDeadline"] = "not-rfc3339"
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("write invalid kept deadline: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"doctor", "--scenario", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 1 {
		t.Fatalf("doctor exit code = %d, want locked-host failure; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `invalid keep deadline "not-rfc3339"`) {
		t.Fatalf("invalid deadline warning missing: %s", stderr.String())
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("invalid-deadline retained Run State changed: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(stateDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read invalid-deadline events: %v", err)
	}
	for _, want := range []string{`"type":"kept-session-expired"`, `"outcome":"invalid-deadline"`, `"deadline":"not-rfc3339"`} {
		if !strings.Contains(string(events), want) {
			t.Fatalf("invalid-deadline events missing %s:\n%s", want, events)
		}
	}
}

func TestRunSweepsConfiguredControlPlaneRetentionRepos(t *testing.T) {
	dataDir := t.TempDir()
	runsRoot := filepath.Join(dataDir, "runs")
	oldRepo := filepath.Join(runsRoot, "old-run", "repo")
	newRepo := filepath.Join(runsRoot, "new-run", "repo")
	moveRunConfigFixture(t, oldRepo)
	moveRunConfigFixture(t, newRepo)
	sharedFakeWorkRoot := filepath.Join(dataDir, "fake-host")
	keptAt := time.Date(2026, time.July, 22, 14, 0, 0, 0, time.UTC)
	runtime := defaultRunRuntime()
	runtime.Now = func() time.Time { return keptAt }

	kept, err := newFreshRun(oldRepo, runOptions{
		ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterNever,
		AssignedRunID: "20260722T140000.000000000Z", SharedFakeWorkRoot: sharedFakeWorkRoot,
		KeptSessionRepoRoot: runsRoot,
	}, runtime).Run()
	if err != nil {
		t.Fatalf("create configured kept run: %v", err)
	}
	recordPath := filepath.Join(kept.StateDir, "run-record.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read configured kept Run Record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse configured kept Run Record: %v", err)
	}
	record["keepDeadline"] = keptAt.Add(-time.Minute).Format(time.RFC3339Nano)
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("make configured kept Run Record overdue: %v", err)
	}

	runtime.Now = func() time.Time { return keptAt.Add(time.Hour) }
	successor, err := newFreshRun(newRepo, runOptions{
		ScenarioName: "smoke", TargetProfile: "default", StopAfter: stopAfterAlways,
		AssignedRunID: "20260722T150000.000000000Z", SharedFakeWorkRoot: sharedFakeWorkRoot,
		KeptSessionRepoRoot: runsRoot,
	}, runtime).Run()
	if err != nil || successor.Result.Status != resultStatusPassed {
		t.Fatalf("configured successor = %+v, err=%v", successor, err)
	}
	if _, err := os.Stat(kept.StateDir); !os.IsNotExist(err) {
		t.Fatalf("configured expired Run State = %v, want removed", err)
	}
	events, err := os.ReadFile(filepath.Join(runLedgerDir(oldRepo, kept.RunID), runLedgerEventsFileName))
	if err != nil {
		t.Fatalf("read configured expired run events: %v", err)
	}
	if !strings.Contains(string(events), `"outcome":"broker-cleaned"`) {
		t.Fatalf("configured expiry event missing:\n%s", events)
	}
}

func TestExpiryEventRunsAfterBrokerCleanupCheckpoint(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())
	now := time.Now()
	if err := prepareRunLedgerForStop(dir, runID, now); err != nil {
		t.Fatalf("prepare expiry stop: %v", err)
	}
	callbackRan := false
	err := stopKeptRunAfterBrokerStop(dir, runID, now, func(stateDir string) {
		callbackRan = true
		manifest := readRunManifestFile(t, filepath.Join(stateDir, "manifest.json"))
		record, err := readRunRetentionRecord(dir, stateDir, manifest)
		if err != nil {
			t.Errorf("read broker-cleaned Run Record: %v", err)
		} else if record.StopPhase != "broker-cleaned" {
			t.Errorf("Run Record stop phase during expiry event = %q, want broker-cleaned", record.StopPhase)
		}
		ledger, err := newRunLedgerStore(dir).Read(runID)
		if err != nil {
			t.Errorf("read broker-cleaned Run Ledger: %v", err)
		} else if ledger.StopPhase != "broker-cleaned" {
			t.Errorf("Run Ledger stop phase during expiry event = %q, want broker-cleaned", ledger.StopPhase)
		}
	})
	if err != nil {
		t.Fatalf("stop kept run through expiry path: %v", err)
	}
	if !callbackRan {
		t.Fatal("expiry event callback did not run")
	}
}

func moveRunConfigFixture(t *testing.T, destination string) {
	t.Helper()
	source := writeRunConfigFixture(t)
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		t.Fatalf("create configured repo parent: %v", err)
	}
	if err := os.Rename(source, destination); err != nil {
		t.Fatalf("move configured repo fixture: %v", err)
	}
}

func TestStatusShowsKeptSessionTTL(t *testing.T) {
	tests := []struct {
		name              string
		deadline          string
		wantDeadline      string
		wantRemainingPart string
	}{
		{name: "remaining", deadline: time.Now().UTC().Add(42 * time.Minute).Format(time.RFC3339Nano), wantRemainingPart: "42m left"},
		{name: "expired", deadline: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), wantRemainingPart: "expired"},
		{name: "unstamped", wantDeadline: "unstamped", wantRemainingPart: "unstamped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			var stdout, stderr bytes.Buffer
			if code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
				t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			runID := runIDFromOutput(t, stdout.String())
			recordPath := filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json")
			content, err := os.ReadFile(recordPath)
			if err != nil {
				t.Fatalf("read kept Run Record: %v", err)
			}
			var record map[string]any
			if err := json.Unmarshal(content, &record); err != nil {
				t.Fatalf("parse kept Run Record: %v", err)
			}
			if tt.deadline == "" {
				delete(record, "keepDeadline")
			} else {
				record["keepDeadline"] = tt.deadline
				tt.wantDeadline = tt.deadline
			}
			if err := writeJSONFile(recordPath, record); err != nil {
				t.Fatalf("set status deadline: %v", err)
			}

			stdout.Reset()
			stderr.Reset()
			if code := Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version"); code != 0 {
				t.Fatalf("status exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			for _, want := range []string{"keepDeadline: " + tt.wantDeadline, "keepRemaining: " + tt.wantRemainingPart} {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("status missing %q:\n%s", want, stdout.String())
				}
			}

			stdout.Reset()
			stderr.Reset()
			if code := Run([]string{"status", "--json", "--run", runID}, &stdout, &stderr, dir, "test-version"); code != 0 {
				t.Fatalf("JSON status exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			var status map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
				t.Fatalf("parse JSON status: %v", err)
			}
			if status["keepDeadline"] != tt.wantDeadline || !strings.Contains(status["keepRemaining"].(string), tt.wantRemainingPart) {
				t.Fatalf("JSON kept TTL status = %#v", status)
			}
		})
	}
}

func TestStatusOmitsKeptSessionTTLForNonKeptRuns(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	runID := runIDFromOutput(t, stdout.String())

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"status", "--run", runID}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("status exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "keepDeadline") {
		t.Fatalf("non-kept status must omit keepDeadline:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"status", "--json", "--run", runID}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("JSON status exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var status map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		t.Fatalf("parse JSON status: %v", err)
	}
	if _, found := status["keepDeadline"]; found {
		t.Fatalf("non-kept JSON status must omit keepDeadline: %#v", status)
	}
}

func TestRunWarnsAndContinuesWhenExpiredBrokerCannotStopRetainedSession(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        health: healthy
      - id: beta
        health: healthy
`)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "--host", "alpha", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	keptRunID := runIDFromOutput(t, stdout.String())
	stateDir := filepath.Join(dir, ".maya-stall", "state", "runs", keptRunID)
	recordPath := filepath.Join(stateDir, "run-record.json")
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read kept Run Record: %v", err)
	}
	var record map[string]any
	if err := json.Unmarshal(content, &record); err != nil {
		t.Fatalf("parse kept Run Record: %v", err)
	}
	record["keepDeadline"] = time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano)
	capabilities := record["brokerCapabilities"].(map[string]any)
	capabilities["stopRetainedSession"] = false
	if err := writeJSONFile(recordPath, record); err != nil {
		t.Fatalf("remove retained stop capability: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 {
		t.Fatalf("successor run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "host: beta") {
		t.Fatalf("successor did not skip locked alpha host:\n%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "does not support stop retained session") {
		t.Fatalf("successor warning missing unsupported stop capability: %s", stderr.String())
	}
	if _, err := os.Stat(stateDir); err != nil {
		t.Fatalf("unsupported retained Run State changed: %v", err)
	}
	events, err := os.ReadFile(filepath.Join(stateDir, "events.jsonl"))
	if err != nil {
		t.Fatalf("read unsupported retained run events: %v", err)
	}
	if !strings.Contains(string(events), `"outcome":"unsupported"`) {
		t.Fatalf("unsupported expiry event missing:\n%s", events)
	}
}

func TestRunStampsConfiguredKeepTTL(t *testing.T) {
	keptAt := time.Date(2026, time.July, 22, 11, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		args    []string
		wantTTL time.Duration
	}{
		{name: "default", args: []string{"run", "--stop-after", "never", "smoke"}, wantTTL: defaultKeepTTL},
		{name: "flag", args: []string{"run", "--keep-ttl", "2h", "--stop-after", "never", "smoke"}, wantTTL: 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeRunConfigFixture(t)
			runtime := defaultRunRuntime()
			runtime.Now = func() time.Time { return keptAt }
			var stdout, stderr bytes.Buffer
			if code := RunWithRuntime(tt.args, &stdout, &stderr, dir, "test-version", runtime); code != 0 {
				t.Fatalf("kept run exit code = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			runID := runIDFromOutput(t, stdout.String())
			record := readRunRetentionRecordFile(t, filepath.Join(dir, ".maya-stall", "state", "runs", runID, "run-record.json"))
			if record.KeepTTL != tt.wantTTL.String() {
				t.Fatalf("keep TTL = %q, want %q", record.KeepTTL, tt.wantTTL)
			}
			if record.KeepDeadline != keptAt.Add(tt.wantTTL).Format(time.RFC3339Nano) {
				t.Fatalf("keep deadline = %q, want %q", record.KeepDeadline, keptAt.Add(tt.wantTTL).Format(time.RFC3339Nano))
			}
		})
	}
}

func TestRunRejectsInvalidKeepTTL(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"run", "--keep-ttl", "0s", "smoke"}, &stdout, &stderr, dir, "test-version"); code != 2 {
		t.Fatalf("invalid keep TTL exit code = %d, want usage failure 2; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `invalid --keep-ttl duration "0s"`) {
		t.Fatalf("invalid keep TTL error missing: %s", stderr.String())
	}
}
