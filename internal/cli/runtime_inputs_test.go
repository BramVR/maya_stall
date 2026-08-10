package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestManifestPayloadStageLabelPrefersRuntimeInputName(t *testing.T) {
	item := manifestPayload{Name: "character", Source: "private/source.ma"}
	if got := item.stageLabel(); got != "character" {
		t.Fatalf("stage label = %q, want runtime input name", got)
	}
	item.Name = ""
	if got := item.stageLabel(); got != "private/source.ma" {
		t.Fatalf("stage label = %q, want repository payload source", got)
	}
}

func TestRuntimeInputSnapshotErrorsUseStablePathPrivateDiagnostics(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "missing", err: fs.ErrNotExist, want: `runtime input "character" file disappeared before it could be snapshotted`},
		{name: "permission", err: fs.ErrPermission, want: `runtime input "character" file could not be read or snapshotted due to permissions`},
		{name: "destination exists", err: fs.ErrExist, want: `runtime input "character" snapshot destination already exists`},
		{name: "other", err: fs.ErrInvalid, want: `runtime input "character" could not be snapshotted`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &os.PathError{Op: "copy", Path: `C:\private\customer\character.ma`, Err: tt.err}
			got := stableRuntimeInputSnapshotError("character", err)
			if got != tt.want {
				t.Fatalf("snapshot error = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "private") || strings.Contains(got, "customer") {
				t.Fatalf("snapshot error exposed private source path: %q", got)
			}
		})
	}
}

func TestPlanNormalizesDeclaredRuntimeFileInputWithoutCreatingRunState(t *testing.T) {
	dir := t.TempDir()
	inputDir := t.TempDir()
	input := filepath.Join(inputDir, "character.ma")
	content := []byte("// selected Maya scene\n")
	if err := os.WriteFile(input, content, 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      runtimeInputs:
        character:
          kind: file
          extensions: [.ma, .mb]
          destination: scenes/character.ma
    expectedOutputs:
      scenarioResult: outputs/result.json
`)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"plan", "--json", "--input", "character=" + input, "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("plan exit code = %d, stderr = %q", code, stderr.String())
	}
	var plan scenarioPlan
	if err := json.Unmarshal(stdout.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Payload) != 1 {
		t.Fatalf("plan payload = %+v", plan.Payload)
	}
	wantHash := sha256.Sum256(content)
	want := planPayload{
		Name:        "character",
		Kind:        "runtimeInput:file",
		Destination: "payload/runtimeInputs/scenes/character.ma",
		Size:        int64(len(content)),
		SHA256:      hex.EncodeToString(wantHash[:]),
		Status:      "ready",
	}
	if plan.Payload[0] != want {
		t.Fatalf("runtime input plan = %+v, want %+v", plan.Payload[0], want)
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run([]string{"plan", "--input", "character=" + input, "smoke"}, &stdout, &stderr, dir, "test-version"); code != 0 || !strings.Contains(stdout.String(), "payload: runtimeInput:file character -> payload/runtimeInputs/scenes/character.ma") {
		t.Fatalf("human plan exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".maya-stall")); !os.IsNotExist(err) {
		t.Fatalf("plan created run state: %v", err)
	}
}

func TestRuntimeInputRejectsDirectoriesLinksScriptsAndCredentials(t *testing.T) {
	for _, extension := range []string{".py", ".ps1", ".pem", ".pfx", ".ppk", ".kdbx"} {
		_, err := normalizeRuntimeInputDeclarations(map[string]runtimeInputDeclaration{
			"selected": {Kind: "file", Extensions: []string{extension}, Destination: "selected" + extension},
		})
		if err == nil || !strings.Contains(err.Error(), "is not allowed for runtime files") {
			t.Fatalf("extension %s error = %v", extension, err)
		}
	}
	if _, err := normalizeRuntimeInputDeclarations(map[string]runtimeInputDeclaration{
		"selected": {Kind: "file", Extensions: []string{".ma"}, Destination: "selected.py"},
	}); err == nil || !strings.Contains(err.Error(), "destination extension") {
		t.Fatalf("unsafe destination extension error = %v", err)
	}

	declaration := runtimeInputDeclaration{Kind: "file", Extensions: []string{".ma"}, Destination: "scene.ma"}
	if err := validateRuntimeInputFile(t.TempDir(), declaration); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(realDir, "scene.ma")
	if err := os.WriteFile(file, []byte("scene\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "linked")
	if err := os.Symlink(realDir, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a test reparse-point link is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	if err := validateRuntimeInputFile(filepath.Join(link, "scene.ma"), declaration); err == nil || !strings.Contains(err.Error(), "symlink or reparse point") {
		t.Fatalf("linked input error = %v", err)
	}
}

func TestRuntimeInputFailuresUseStablePathPrivateDiagnostics(t *testing.T) {
	dir := t.TempDir()
	inputDir := t.TempDir()
	unsupported := filepath.Join(inputDir, "character.txt")
	if err := os.WriteFile(unsupported, []byte("not a scene\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      runtimeInputs:
        character:
          kind: file
          extensions: [.ma]
          destination: scenes/character.ma
    expectedOutputs:
      scenarioResult: outputs/result.json
`)

	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{name: "missing", args: []string{"plan", "--json", "smoke"}, code: 1, want: `runtime input "character" is required`},
		{name: "undeclared", args: []string{"plan", "--json", "--input", "extra=" + unsupported, "smoke"}, code: 1, want: `runtime input "extra" is not declared`},
		{name: "unsupported", args: []string{"plan", "--json", "--input", "character=" + unsupported, "smoke"}, code: 1, want: `extension ".txt" is not allowed`},
		{name: "duplicate", args: []string{"plan", "--json", "--input", "character=" + unsupported, "--input", "character=" + unsupported, "smoke"}, code: 2, want: `runtime input "character" is bound more than once`},
		{name: "relative", args: []string{"plan", "--json", "--input", "character=relative.ma", "smoke"}, code: 2, want: `must bind an absolute file path`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(tt.args, &stdout, &stderr, dir, "test-version")
			combined := strings.ReplaceAll(stdout.String()+stderr.String(), `\"`, `"`)
			if code != tt.code || !strings.Contains(combined, tt.want) {
				t.Fatalf("code=%d stdout=%q stderr=%q, want code=%d containing %q", code, stdout.String(), stderr.String(), tt.code, tt.want)
			}
			if strings.Contains(stdout.String(), inputDir) {
				t.Fatalf("JSON output exposed absolute runtime input path: %s", stdout.String())
			}
		})
	}
}

func TestPlanDuplicateRuntimeInputHasStableJSONUsageDiagnostic(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(t.TempDir(), "scene.ma")
	if err := os.WriteFile(source, []byte("scene\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  runtime-input:
    payload:
      runtimeInputs:
        scene: {kind: file, extensions: [.ma], destination: scene.ma}
    expectedOutputs: {scenarioResult: result.json}
`)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"plan", "--json", "--input", "scene=" + source, "--input", "scene=" + source, "runtime-input"}, &stdout, &stderr, dir, "test-version")
	if code != 2 || stderr.Len() != 0 {
		t.Fatalf("duplicate plan input exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var result runCommandJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Kind != "usage-error" || result.Error != `runtime input "scene" is bound more than once` {
		t.Fatalf("duplicate input JSON = %+v", result)
	}
}

func TestRunRejectsRuntimeInputChangedDuringStagingBeforeMayaStarts(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(t.TempDir(), "character.ma")
	if err := os.WriteFile(input, []byte("first bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      runtimeInputs:
        character:
          kind: file
          extensions: [.ma]
          destination: scenes/character.ma
    expectedOutputs:
      scenarioResult: outputs/result.json
`)
	broker := &countingSessionBroker{}
	runtime := defaultRunRuntime()
	runtime.Broker = broker
	runtime.BeforeRuntimeInputCopy = func(_, source string) {
		if err := os.WriteFile(source, []byte("changed bytes\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--json", "--input", "character=" + input, "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 1 || !strings.Contains(stdout.String()+stderr.String(), `runtime input "character" changed during staging`) {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if broker.starts != 0 {
		t.Fatalf("Maya Session Broker starts = %d, want 0", broker.starts)
	}
}

type countingSessionBroker struct {
	fakeSessionBroker
	starts int
}

func (broker *countingSessionBroker) StartFreshSession(context runContext, scenario scenarioConfig) (brokerSessionIdentity, error) {
	broker.starts++
	return broker.fakeSessionBroker.StartFreshSession(context, scenario)
}

func TestRunSnapshotsRuntimeInputAndRecordsIdentityWithoutAbsoluteSource(t *testing.T) {
	dir := t.TempDir()
	inputDir := t.TempDir()
	input := filepath.Join(inputDir, "character.ma")
	content := []byte("// immutable selected scene\n")
	if err := os.WriteFile(input, content, 0o644); err != nil {
		t.Fatal(err)
	}
	mustWriteFile(t, filepath.Join(dir, ".maya-stall.yaml"), `version: 1
scenarios:
  smoke:
    payload:
      runtimeInputs:
        character:
          kind: file
          extensions: [.ma]
          destination: scenes/character.ma
    expectedOutputs:
      scenarioResult: outputs/result.json
`)
	broker := &runtimeInputMutatingBroker{source: input, want: content}
	runtime := defaultRunRuntime()
	runtime.Broker = broker
	var stdout, stderr bytes.Buffer
	code := RunWithRuntime([]string{"run", "--input", "character=" + input, "smoke"}, &stdout, &stderr, dir, "test-version", runtime)
	if code != 0 {
		t.Fatalf("run exit code = %d, stdout = %q, stderr = %q", code, stdout.String(), stderr.String())
	}
	if !broker.ran {
		t.Fatal("Scenario did not receive the runtime input snapshot")
	}
	unchanged, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unchanged, content) {
		t.Fatalf("original runtime input changed to %q", unchanged)
	}
	evidenceDir := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	manifestBytes, err := os.ReadFile(filepath.Join(evidenceDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(manifestBytes, []byte(input)) || bytes.Contains(manifestBytes, []byte(inputDir)) {
		t.Fatalf("manifest exposed absolute runtime input source: %s", manifestBytes)
	}
	var manifest runManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Payload) != 1 {
		t.Fatalf("manifest payload = %+v", manifest.Payload)
	}
	wantHash := sha256.Sum256(content)
	item := manifest.Payload[0]
	if item.Name != "character" || item.Kind != "runtimeInput:file" || item.Source != "" || filepath.ToSlash(item.Staged) != "payload/runtimeInputs/scenes/character.ma" || item.Size != int64(len(content)) || item.SHA256 != hex.EncodeToString(wantHash[:]) {
		t.Fatalf("runtime input manifest identity = %+v", item)
	}
}

type runtimeInputMutatingBroker struct {
	fakeSessionLifecycle
	source string
	want   []byte
	ran    bool
}

func (broker *runtimeInputMutatingBroker) RunScenario(context runContext, _ scenarioConfig) (ScenarioResult, error) {
	if err := os.WriteFile(context.LogPath, []byte("runtime input snapshot used\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	var inputs map[string]string
	if err := json.Unmarshal([]byte(context.Environment[runtimeInputsEnvVar]), &inputs); err != nil {
		return ScenarioResult{}, err
	}
	staged := inputs["character"]
	if staged == "" || filepath.Clean(staged) == filepath.Clean(broker.source) {
		return ScenarioResult{}, os.ErrInvalid
	}
	content, err := os.ReadFile(staged)
	if err != nil {
		return ScenarioResult{}, err
	}
	if !bytes.Equal(content, broker.want) {
		return ScenarioResult{}, os.ErrInvalid
	}
	if err := os.WriteFile(staged, []byte("Scenario changed only the staged snapshot\n"), 0o644); err != nil {
		return ScenarioResult{}, err
	}
	broker.ran = true
	return ScenarioResult{Status: resultStatusPassed, Summary: "runtime input snapshot used"}, nil
}
