package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestCompatibleHostAgentCandidatesAreDeterministicWithinTargetProfile(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	newAgent := func(agentID string, hostID string, profiles ...string) *controlPlaneHostAgent {
		report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: hostID, Health: "healthy"}, now)
		report.Capabilities.SessionMayaBuild = report.Capabilities.MayaBuilds[0]
		report.TargetProfiles = profiles
		return &controlPlaneHostAgent{status: hostAgentStatusResponse{
			AgentID: agentID, HostID: hostID, State: "ready", Slots: 1, SessionID: "session", SessionBinding: true, DeadlineActions: true, Capabilities: report,
		}}
	}
	agents := map[string]*controlPlaneHostAgent{
		"agent-a": newAgent("agent-a", "maya-z", "ci"),
		"agent-z": newAgent("agent-z", "maya-a", "ci"),
		"agent-b": newAgent("agent-b", "maya-b", "other"),
	}

	candidates, reasons := compatibleHostAgentCandidates(agents, "ci", scenarioRequirements{}, now)
	if len(candidates) != 2 || candidates[0].status.HostID != "maya-a" || candidates[1].status.HostID != "maya-z" {
		t.Fatalf("ranked candidates = %+v", candidates)
	}
	if len(reasons) != 1 || reasons[0] != "Maya Host maya-b is not in Target Profile ci" {
		t.Fatalf("target-profile reasons = %+v", reasons)
	}
}

func TestIneligibleCapabilityReportsNeverQualify(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	base := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	base.Capabilities.SessionMayaBuild = base.Capabilities.MayaBuilds[0]
	tests := []struct {
		name   string
		mutate func(*mayaHostCapabilityRecord)
		want   string
	}{
		{name: "legacy schema", mutate: func(report *mayaHostCapabilityRecord) { report.Version = 1 }, want: "capability record version is 1; required version is 2"},
		{name: "incomplete", mutate: func(report *mayaHostCapabilityRecord) { report.Capabilities.GPU = nil }, want: "capability record is incomplete: missing GPU"},
		{name: "offline", mutate: func(report *mayaHostCapabilityRecord) { report.Online = false }, want: "Maya Host is offline"},
		{name: "unhealthy", mutate: func(report *mayaHostCapabilityRecord) { report.Health = "unhealthy" }, want: "Maya Host health is unhealthy"},
		{name: "maintenance", mutate: func(report *mayaHostCapabilityRecord) { report.Maintenance = true }, want: "Maya Host is under maintenance"},
		{name: "quarantined", mutate: func(report *mayaHostCapabilityRecord) { report.Quarantined = true }, want: "Maya Host is quarantined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := base
			test.mutate(&report)
			decision := decideMayaHostCompatibility(scenarioRequirements{}, report, now)
			if decision.Compatible || !containsString(decision.Reasons, test.want) {
				t.Fatalf("eligibility decision = %+v, want %q", decision, test.want)
			}
		})
	}
}

func TestVersionTwoCapabilityRequiresEveryTargetProfileHostPool(t *testing.T) {
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, time.Now())
	report.TargetProfiles = []string{"default", "render"}
	report.TargetProfileHostPools = map[string]string{"default": "windows-maya"}
	if completeTargetProfileHostPoolMapping(report) {
		t.Fatal("version 2 capability accepted an incomplete Host Pool mapping")
	}
	report.TargetProfileHostPools["render"] = "render-pool"
	if !completeTargetProfileHostPoolMapping(report) {
		t.Fatal("version 2 capability rejected a complete Host Pool mapping")
	}
}

func TestMinimumVersionMatchesAcceptNewerBuilds(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.Capabilities.MayaBuilds = []string{"2025.3"}
	report.Capabilities.SessionMayaBuild = "2025.3"
	report.Capabilities.Python = "3.11.9"
	report.Capabilities.SessionBroker.Version = "2.2"
	decision := decideMayaHostCompatibility(scenarioRequirements{
		Maya: versionRequirement{Minimum: "2025.2"}, Python: versionRequirement{Minimum: "3.11"},
		SessionBroker: sessionBrokerRequirement{Minimum: "2.1"},
	}, report, now)
	if !decision.Compatible {
		t.Fatalf("minimum version decision = %+v", decision)
	}
	if decision.SelectedMayaBuild != "2025.3" {
		t.Fatalf("selected Maya build = %q", decision.SelectedMayaBuild)
	}
}

func TestCompatibilitySelectsReportedSessionMayaBuild(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.Capabilities.MayaBuilds = []string{"2026", "2025.4", "2025.3"}
	report.Capabilities.SessionMayaBuild = "2026"
	decision := decideMayaHostCompatibility(scenarioRequirements{Maya: versionRequirement{Minimum: "2025.2"}}, report, now)
	if !decision.Compatible || decision.SelectedMayaBuild != "2026" {
		t.Fatalf("concrete Maya build decision = %+v", decision)
	}
}

func TestHostAgentBuildsVersionedTimestampedCapabilityRecordFromHostConfig(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := dir + "/hosts.yaml"
	if err := os.WriteFile(path, []byte(`version: 1
targetProfiles:
  ci:
    hostPool: windows
  render:
    hostPool: windows
hostPools:
  windows:
    hosts:
      - id: maya-win-01
        health: healthy
        transport: ssh
        ssh:
          host: maya-win-01
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv/Scripts/python.exe
          repo: C:/maya-stall/GG_MayaSessiond
        capabilities:
          mayaBuilds: ["2025.3"]
          sessionMayaBuild: "2025.3"
          python: "3.11.9"
          sessionBroker:
            version: "2.2"
            features: ["script.execute", "status.observe"]
          capture: ["screenshot", "recording"]
          control: ["coordinate"]
          renderers: ["arnold"]
          gpu: ["nvidia"]
          display: ["console"]
          licensing: ["available"]
          trustedPluginArtifacts: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := hostAgentCapabilityRecord(hostAgentRunOnceOptions{HostID: "maya-win-01", HostConfig: path}, now)
	if err != nil {
		t.Fatal(err)
	}
	if report.Version != mayaHostCapabilityRecordVersion || report.ReportedAt != "2026-07-16T12:00:00Z" || !report.Online || report.Health != "healthy" {
		t.Fatalf("capability record identity = %+v", report)
	}
	if len(report.TargetProfiles) != 2 || report.TargetProfiles[0] != "ci" || report.TargetProfiles[1] != "render" || report.TargetProfileHostPools["ci"] != "windows" || report.TargetProfileHostPools["render"] != "windows" || report.Capabilities.Python != "3.11.9" || report.Capabilities.TrustedPluginArtifacts == nil || !*report.Capabilities.TrustedPluginArtifacts {
		t.Fatalf("capability record content = %+v", report)
	}
}

func TestLegacyMayaInventoryDoesNotInferExactSessionBuild(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy", Transport: "ssh", MayaVersions: []string{"2025"}}, now)
	if len(report.Capabilities.MayaBuilds) != 1 || report.Capabilities.MayaBuilds[0] != "2025" {
		t.Fatalf("reported Maya inventory = %v", report.Capabilities.MayaBuilds)
	}
	if report.Capabilities.SessionMayaBuild != "" {
		t.Fatalf("legacy Maya inventory inferred exact session build %q", report.Capabilities.SessionMayaBuild)
	}
	decision := decideMayaHostCompatibility(scenarioRequirements{}, report, now)
	if decision.Compatible || !containsString(decision.Reasons, "capability record is incomplete: missing session Maya build") {
		t.Fatalf("legacy inventory eligibility = %+v", decision)
	}
}

func TestStructuredMayaInventoryDoesNotInferExactSessionBuild(t *testing.T) {
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{Capabilities: &mayaHostCapabilities{MayaBuilds: []string{"2025.3"}}}, time.Now())
	if report.Capabilities.SessionMayaBuild != "" {
		t.Fatalf("structured Maya inventory inferred exact session build %q", report.Capabilities.SessionMayaBuild)
	}
}

func TestHostAgentCapabilityRecordRejectsConflictingDuplicateHostDefinitions(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hosts.yaml"
	if err := os.WriteFile(path, []byte(`version: 1
targetProfiles:
  ci:
    hostPool: ci-pool
  render:
    hostPool: render-pool
hostPools:
  ci-pool:
    hosts:
      - id: maya-win-01
        health: healthy
        mayaVersions: ["2025"]
  render-pool:
    hosts:
      - id: maya-win-01
        health: healthy
        mayaVersions: ["2026"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := hostAgentCapabilityRecord(hostAgentRunOnceOptions{HostID: "maya-win-01", HostConfig: path}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "conflicting definitions") {
		t.Fatalf("duplicate Host definition error = %v", err)
	}
}

func TestHostAgentCapabilityRecordRejectsUnusableRuntimeConfig(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/hosts.yaml"
	if err := os.WriteFile(path, []byte(`version: 1
targetProfiles:
  ci:
    hostPool: fake
hostPools:
  fake:
    hosts:
      - id: maya-win-01
        health: healthy
        mayaVersions: ["2025"]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := hostAgentCapabilityRecord(hostAgentRunOnceOptions{HostID: "maya-win-01", HostConfig: path}, time.Now())
	if err == nil || !strings.Contains(err.Error(), "live-proof-eligible Maya Host") {
		t.Fatalf("unusable Host runtime error = %v", err)
	}
}

func TestAssignedMayaBuildBecomesExactRuntimeRequirement(t *testing.T) {
	scenario := scenarioConfig{
		Requirements:      scenarioRequirements{Maya: versionRequirement{Minimum: "2025.2"}},
		SelectedMayaBuild: "2025.3",
	}
	requirement := normalizedScenarioRequirements(scenario).Maya
	if requirement.Exact != "2025.3" || requirement.Minimum != "" {
		t.Fatalf("assigned runtime Maya requirement = %+v", requirement)
	}
}

func TestHostAgentRejectsAssignmentWhoseSelectedMayaBuildWasNotMatched(t *testing.T) {
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, time.Now())
	report.Capabilities.MayaBuilds = []string{"2025.3"}
	report.Capabilities.SessionMayaBuild = "2025.3"
	assignment := hostAgentAssignmentResponse{
		Version: hostAgentAPIVersion, Kind: "host-agent-assignment", RunID: "20260716T120000.000000000Z",
		AgentID: "windows-agent-01", HostID: "maya-win-01", LockToken: "lock", Capabilities: report, SelectedMayaBuild: "2026",
		Submission: controlPlaneSubmission{Scenario: "smoke", Config: []byte(`version: 1
scenarios:
  smoke:
    requirements:
      maya:
        minimum: "2025.2"
    expectedOutputs:
      scenarioResult: "outputs/result.json"
`)},
	}
	err := validateHostAgentAssignment(hostAgentRunOnceOptions{AgentID: "windows-agent-01", HostID: "maya-win-01"}, assignment)
	if err == nil || !strings.Contains(err.Error(), "selected Maya build") {
		t.Fatalf("assignment Maya build validation error = %v", err)
	}
}

func TestMayaBuildProbeScriptReadsFreshSessionVersion(t *testing.T) {
	script := mayaBuildProbeScript("C:/maya-stall/runs/run-1/workspace/build.txt")
	for _, required := range []string{"cmds.about(majorVersion=True)", "cmds.about(minorVersion=True)", "cmds.about(patchVersion=True)", "C:/maya-stall/runs/run-1/workspace/build.txt", "write_text"} {
		if !strings.Contains(script, required) {
			t.Fatalf("Maya build probe script missing %q: %s", required, script)
		}
	}
}

func TestMayaBuildComparisonIgnoresTrailingZeroComponents(t *testing.T) {
	if !sameMayaBuildVersion("2025.3", "2025.3.0") {
		t.Fatal("numerically equivalent Maya builds did not match")
	}
}

func TestMayaMismatchNamesReportedSessionBuild(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.Capabilities.MayaBuilds = []string{"2025", "2026"}
	report.Capabilities.SessionMayaBuild = "2026"
	decision := decideMayaHostCompatibility(scenarioRequirements{Maya: versionRequirement{Exact: "2025"}}, report, now)
	if decision.Compatible || len(decision.Reasons) == 0 || !strings.Contains(decision.Reasons[0], "reported session build is 2026") {
		t.Fatalf("Maya mismatch decision = %+v", decision)
	}
}

func TestNormalizedScenarioRequirementsIncludeImplicitExecutionCapabilities(t *testing.T) {
	requirements := normalizedScenarioRequirements(scenarioConfig{Evidence: evidenceConfig{
		Screenshots: evidenceToggle{Enabled: true},
		Recording:   evidenceToggle{Enabled: true},
	}, Validators: []validatorConfig{{Type: "visualEvidence"}}})
	if !containsString(requirements.SessionBroker.Features, "script.execute") {
		t.Fatalf("Session Broker features = %v", requirements.SessionBroker.Features)
	}
	for _, required := range []string{"screenshot", "recording", "visual-evidence"} {
		if !containsString(requirements.Capture, required) {
			t.Fatalf("capture capabilities = %v, want %q", requirements.Capture, required)
		}
	}
}

func TestMayaHostCapabilityRecordSnapshotIsIndependent(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	report := configuredMayaHostCapabilityRecord(mayaHostConfig{ID: "maya-win-01", Health: "healthy"}, now)
	report.TargetProfiles = []string{"ci"}
	snapshot := snapshotMayaHostCapabilityRecord(report)

	report.TargetProfiles[0] = "other"
	report.Capabilities.MayaBuilds[0] = "2030"
	report.Capabilities.SessionBroker.Features[0] = "changed"
	*report.Capabilities.TrustedPluginArtifacts = !*report.Capabilities.TrustedPluginArtifacts

	if snapshot.TargetProfiles[0] != "ci" || snapshot.Capabilities.MayaBuilds[0] == "2030" || snapshot.Capabilities.SessionBroker.Features[0] != "script.execute" || *snapshot.Capabilities.TrustedPluginArtifacts == *report.Capabilities.TrustedPluginArtifacts {
		t.Fatalf("capability snapshot changed with source: %+v", snapshot)
	}
}

func TestDoctorMayaVersionLayerHonorsStructuredMinimum(t *testing.T) {
	check := mayaVersionLayer(
		doctorOptions{ScenarioName: "smoke"},
		mayaHostConfig{FakeInstalledMayaVersions: []string{"2025.3"}},
		scenarioConfig{Requirements: scenarioRequirements{Maya: versionRequirement{Minimum: "2026"}}},
	)
	if check.Status != "fail" || check.Detail != "Scenario smoke needs minimum 2026; host has 2025.3" {
		t.Fatalf("structured minimum check = %+v", check)
	}
}

func TestDoctorMayaVersionLayerTreatsTrailingZeroAsExact(t *testing.T) {
	check := mayaVersionLayer(
		doctorOptions{ScenarioName: "smoke"},
		mayaHostConfig{FakeInstalledMayaVersions: []string{"2025.3"}},
		scenarioConfig{Requirements: scenarioRequirements{Maya: versionRequirement{Exact: "2025.3.0"}}},
	)
	if check.Status != "ok" {
		t.Fatalf("semantic exact check = %+v", check)
	}
}

func TestScenarioRequirementsForSchedulingFailsClosed(t *testing.T) {
	if _, err := scenarioRequirementsForScheduling(t.TempDir(), "smoke"); err == nil {
		t.Fatal("missing repo config unexpectedly produced empty scheduling requirements")
	}
}

func TestLegacyHostAgentResponsesOmitCapabilityExtension(t *testing.T) {
	for _, response := range []any{
		hostAgentStatusResponse{Version: hostAgentAPIVersion, Kind: "host-agent-status"},
		hostAgentAssignmentResponse{Version: hostAgentAPIVersion, Kind: "host-agent-assignment"},
	} {
		encoded, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"capabilities"`) {
			t.Fatalf("legacy response exposes capability extension: %s", encoded)
		}
	}
}

func TestTrustedPluginAllowlistUsesStructuredMayaRequirementsAndCapabilities(t *testing.T) {
	exactHost := mayaHostConfig{Capabilities: &mayaHostCapabilities{MayaBuilds: []string{"2025.3", "2026"}, SessionMayaBuild: "2025.3"}}
	exact := trustedPluginAllowlistMayaVersions(exactHost, scenarioConfig{Requirements: scenarioRequirements{Maya: versionRequirement{Exact: "2025.3"}}})
	if len(exact) != 1 || exact[0] != "2025.3" {
		t.Fatalf("exact trusted versions = %v", exact)
	}
	exactConfiguredHost := exactHost
	exactConfiguredHost.MayaVersions = []string{"2025"}
	exactConfigured := trustedPluginAllowlistMayaVersions(exactConfiguredHost, scenarioConfig{Requirements: scenarioRequirements{Maya: versionRequirement{Exact: "2025.3"}}})
	if len(exactConfigured) != 1 || exactConfigured[0] != "2025" {
		t.Fatalf("configured exact trusted versions = %v, want configured Maya preferences family", exactConfigured)
	}
	minimumHost := mayaHostConfig{
		MayaVersions: []string{"2025"},
		Capabilities: &mayaHostCapabilities{MayaBuilds: []string{"2025.3", "2026"}, SessionMayaBuild: "2025.3"},
	}
	minimum := trustedPluginAllowlistMayaVersions(minimumHost, scenarioConfig{Requirements: scenarioRequirements{Maya: versionRequirement{Minimum: "2025"}}})
	if len(minimum) != 1 || minimum[0] != "2025" {
		t.Fatalf("minimum trusted versions = %v, want configured Maya preferences family", minimum)
	}
	unrelatedHost := mayaHostConfig{
		MayaVersions: []string{"2025"},
		Capabilities: &mayaHostCapabilities{MayaBuilds: []string{"2026"}, SessionMayaBuild: "2026"},
	}
	unrelated := trustedPluginAllowlistMayaVersions(unrelatedHost, scenarioConfig{Requirements: scenarioRequirements{Maya: versionRequirement{Minimum: "2025"}}})
	if len(unrelated) != 1 || unrelated[0] != "2026" {
		t.Fatalf("unrelated preferences family resolved as %v, want selected session build failure path", unrelated)
	}
	incompatibleHost := mayaHostConfig{
		MayaVersions: []string{"2025", "2026"},
		Capabilities: &mayaHostCapabilities{MayaBuilds: []string{"2025.3"}, SessionMayaBuild: "2025.3"},
	}
	incompatible := trustedPluginAllowlistMayaVersions(incompatibleHost, scenarioConfig{Requirements: scenarioRequirements{Maya: versionRequirement{Minimum: "2026"}}})
	if len(incompatible) != 0 {
		t.Fatalf("incompatible fixed session resolved preferences versions %v", incompatible)
	}
}
