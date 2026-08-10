package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type planOptions struct {
	ScenarioName  string
	HostConfig    string
	JSON          bool
	InputBindings map[string]string
}

type scenarioPlan struct {
	Version        int                 `json:"version"`
	Kind           string              `json:"kind"`
	Scenario       string              `json:"scenario"`
	ConfigPath     string              `json:"configPath"`
	Ready          bool                `json:"ready"`
	Requirements   planRequirements    `json:"requirements"`
	Payload        []planPayload       `json:"payload"`
	Issues         []planIssue         `json:"issues"`
	TargetProfiles []planTargetProfile `json:"targetProfiles"`
}

type planRequirements struct {
	MayaVersion      string               `json:"mayaVersion,omitempty"`
	SessionBroker    bool                 `json:"sessionBroker"`
	Capabilities     []string             `json:"capabilities"`
	HostCapabilities scenarioRequirements `json:"hostCapabilities"`
}

type planPayload struct {
	Name        string `json:"name,omitempty"`
	Kind        string `json:"kind"`
	Source      string `json:"source,omitempty"`
	Destination string `json:"destination"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	Status      string `json:"status"`
}

type planIssue struct {
	Source string `json:"source,omitempty"`
	Reason string `json:"reason"`
}

type planTargetProfile struct {
	Name       string     `json:"name"`
	HostPool   string     `json:"hostPool"`
	Compatible bool       `json:"compatible"`
	Reasons    []string   `json:"reasons"`
	Hosts      []planHost `json:"hosts"`
}

type planHost struct {
	ID         string   `json:"id"`
	Compatible bool     `json:"compatible"`
	Reasons    []string `json:"reasons"`
}

func parsePlanArgs(args []string) (planOptions, error) {
	options := planOptions{InputBindings: make(map[string]string)}
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--json":
			options.JSON = true
		case "--host-config":
			i++
			if i >= len(args) || args[i] == "" || strings.HasPrefix(args[i], "--") {
				return options, newUsageError("--host-config needs a path")
			}
			options.HostConfig = args[i]
		case "--input":
			i++
			if i >= len(args) || args[i] == "" || strings.HasPrefix(args[i], "--") {
				return options, newUsageError("--input needs name=absolute-file")
			}
			name, source, err := parseRuntimeInputBinding(args[i])
			if err != nil {
				return options, err
			}
			if _, exists := options.InputBindings[name]; exists {
				return options, newUsageError("runtime input %q is bound more than once", name)
			}
			options.InputBindings[name] = source
		default:
			if strings.HasPrefix(arg, "-") {
				return options, newUsageError("unknown plan option %q", arg)
			}
			if options.ScenarioName != "" {
				return options, newUsageError("expected one Scenario name")
			}
			options.ScenarioName = arg
		}
	}
	if options.ScenarioName == "" {
		return options, newUsageError("expected Scenario name")
	}
	return options, nil
}

func buildScenarioPlan(repoDir string, options planOptions) (scenarioPlan, error) {
	config, configPath, err := loadRepoRunConfig(repoDir)
	if err != nil {
		return scenarioPlan{}, err
	}
	relConfigPath, err := filepath.Rel(repoDir, configPath)
	if err != nil {
		return scenarioPlan{}, err
	}
	rawScenario, ok := config.Scenarios[options.ScenarioName]
	if !ok {
		return scenarioPlan{}, newUsageError("unknown Scenario %q", options.ScenarioName)
	}
	plan := scenarioPlan{
		Version:    1,
		Kind:       "scenario-plan",
		Scenario:   options.ScenarioName,
		ConfigPath: filepath.ToSlash(relConfigPath),
		Requirements: planRequirements{
			MayaVersion:      rawScenario.MayaVersion,
			SessionBroker:    true,
			Capabilities:     scenarioPlanCapabilities(rawScenario),
			HostCapabilities: normalizedScenarioRequirements(rawScenario),
		},
		Payload:        make([]planPayload, 0),
		Issues:         []planIssue{},
		TargetProfiles: []planTargetProfile{},
	}
	scenario, err := resolveScenarioContract(config, options.ScenarioName)
	if err != nil {
		validPayload := make([]manifestPayload, 0)
		for _, declaration := range manifestPayloadDeclarations(rawScenario.Payload) {
			item, itemErr := buildManifestPayloadItem(declaration.Kind, declaration.Source)
			if itemErr != nil {
				plan.Payload = append(plan.Payload, planPayload{Kind: declaration.Kind, Source: declaration.Source, Status: "unsafe"})
				plan.Issues = append(plan.Issues, planIssue{Source: declaration.Source, Reason: itemErr.Error()})
				continue
			}
			validPayload = append(validPayload, item)
			entry, issue := inspectPlanPayload(repoDir, item)
			plan.Payload = append(plan.Payload, entry)
			if issue != nil {
				plan.Issues = append(plan.Issues, *issue)
			}
		}
		runtimeDeclarations, runtimeErr := normalizeRuntimeInputDeclarations(rawScenario.Payload.RuntimeInputs)
		if runtimeErr != nil {
			plan.Issues = append(plan.Issues, planIssue{Reason: runtimeErr.Error()})
		} else {
			runtimePayload, runtimeIssues := inspectPlanRuntimeInputs(runtimeDeclarations, options.InputBindings)
			plan.Payload = append(plan.Payload, runtimePayload...)
			plan.Issues = append(plan.Issues, runtimeIssues...)
		}
		if remoteErr := validateScenarioRemotePaths(rawScenario); remoteErr != nil && !planHasIssueReason(plan.Issues, remoteErr.Error()) {
			plan.Issues = append(plan.Issues, planIssue{Reason: remoteErr.Error()})
		}
		if !planHasIssueReason(plan.Issues, err.Error()) {
			plan.Issues = append(plan.Issues, planIssue{Reason: err.Error()})
		}
		applyPlanHostConfig(&plan, options, repoDir, rawScenario, validPayload)
		return plan, nil
	}
	plan.Payload = make([]planPayload, 0, len(scenario.Payload))
	for _, item := range scenario.Payload {
		entry, issue := inspectPlanPayload(repoDir, item)
		plan.Payload = append(plan.Payload, entry)
		if issue != nil {
			plan.Issues = append(plan.Issues, *issue)
		}
	}
	runtimePayload, runtimeIssues := inspectPlanRuntimeInputs(scenario.Config.Payload.RuntimeInputs, options.InputBindings)
	plan.Payload = append(plan.Payload, runtimePayload...)
	plan.Issues = append(plan.Issues, runtimeIssues...)
	if err := validateScenarioRemotePaths(scenario.Config); err != nil {
		plan.Issues = append(plan.Issues, planIssue{Reason: err.Error()})
	}
	plan.Ready = len(plan.Issues) == 0
	applyPlanHostConfig(&plan, options, repoDir, scenario.Config, scenario.Payload)
	return plan, nil
}

func applyPlanHostConfig(plan *scenarioPlan, options planOptions, repoDir string, scenario scenarioConfig, payload []manifestPayload) {
	if options.HostConfig == "" {
		return
	}
	hostConfig, err := loadUserHostConfig(options.HostConfig)
	if err != nil {
		plan.Ready = false
		plan.Issues = append(plan.Issues, planIssue{Reason: fmt.Sprintf("host config %q: %v", options.HostConfig, err)})
		return
	}
	plan.TargetProfiles = planTargetCompatibility(repoDir, hostConfig, scenario, payload)
	compatible := false
	for _, profile := range plan.TargetProfiles {
		compatible = compatible || profile.Compatible
	}
	if !compatible {
		plan.Ready = false
		plan.Issues = append(plan.Issues, planIssue{Reason: "host config has no compatible Maya Host"})
	}
}

func planHasIssueReason(issues []planIssue, reason string) bool {
	for _, issue := range issues {
		if issue.Reason == reason {
			return true
		}
	}
	return false
}

func inspectPlanPayload(repoDir string, item manifestPayload) (planPayload, *planIssue) {
	entry := planPayload{
		Kind:        item.Kind,
		Source:      item.Source,
		Destination: filepath.ToSlash(item.Staged),
		Status:      "ready",
	}
	if err := rejectSFTPRepoPath(item.Source); err != nil {
		entry.Status = "invalid"
		return entry, &planIssue{Source: item.Source, Reason: err.Error()}
	}
	if err := validatePayloadPathForTransport(repoDir, item.Source); err != nil {
		entry.Status = "invalid"
		if os.IsNotExist(err) {
			entry.Status = "missing"
		}
		return entry, &planIssue{Source: item.Source, Reason: stablePlanIssueReason(repoDir, item.Source, err)}
	}
	size, hash, err := summarizePlanPayload(filepath.Join(repoDir, item.Source))
	if err != nil {
		entry.Status = "invalid"
		return entry, &planIssue{Source: item.Source, Reason: stablePlanIssueReason(repoDir, item.Source, err)}
	}
	entry.Size = size
	entry.SHA256 = hash
	return entry, nil
}

func stablePlanIssueReason(repoDir string, source string, err error) string {
	if os.IsNotExist(err) {
		return fmt.Sprintf("payload path %q does not exist", source)
	}
	reason := strings.ReplaceAll(err.Error(), repoDir+string(filepath.Separator), "")
	return filepath.ToSlash(reason)
}

func planTargetCompatibility(repoDir string, config userHostConfig, scenario scenarioConfig, payload []manifestPayload) []planTargetProfile {
	names := make([]string, 0, len(config.TargetProfiles))
	for name := range config.TargetProfiles {
		names = append(names, name)
	}
	sort.Strings(names)
	profiles := make([]planTargetProfile, 0, len(names))
	for _, name := range names {
		configured := config.TargetProfiles[name]
		profile := planTargetProfile{Name: name, HostPool: configured.HostPool, Reasons: []string{}, Hosts: []planHost{}}
		pool, ok := config.HostPools[configured.HostPool]
		if !ok {
			profile.Reasons = append(profile.Reasons, fmt.Sprintf("references unknown Host Pool %q", configured.HostPool))
			profiles = append(profiles, profile)
			continue
		}
		_, structuralErr := hostCandidates(config, name, "")
		if structuralErr != nil {
			profile.Reasons = append(profile.Reasons, structuralErr.Error())
		}
		compatibleHost := false
		for _, host := range pool.Hosts {
			planned := planHostCompatibility(repoDir, host, scenario, payload)
			profile.Hosts = append(profile.Hosts, planned)
			compatibleHost = compatibleHost || planned.Compatible
		}
		profile.Compatible = structuralErr == nil && compatibleHost
		if !profile.Compatible && len(profile.Reasons) == 0 {
			profile.Reasons = append(profile.Reasons, "no compatible Maya Host")
		}
		profiles = append(profiles, profile)
	}
	return profiles
}

func planHostCompatibility(repoDir string, host mayaHostConfig, scenario scenarioConfig, payload []manifestPayload) planHost {
	planned := planHost{ID: host.ID, Reasons: []string{}}
	if err := validateHostID(host.ID); err != nil {
		planned.Reasons = append(planned.Reasons, err.Error())
	}
	decision := decideMayaHostCompatibility(normalizedScenarioRequirements(scenario), configuredMayaHostCapabilityRecord(host, time.Now()), time.Now())
	planned.Reasons = append(planned.Reasons, decision.Reasons...)
	if _, err := resolveRuntimeForHost(host); err != nil {
		planned.Reasons = append(planned.Reasons, err.Error())
	}
	if host.usesRealSSH() {
		if err := validateRealSSHConfig(host); err != nil {
			planned.Reasons = append(planned.Reasons, err.Error())
		}
	} else {
		if err := validateTrustedPluginArtifactsRoot(host); err != nil {
			planned.Reasons = append(planned.Reasons, err.Error())
		}
		if err := validateFakeTransportStatus(host.SSH.FakeStatus); err != nil {
			planned.Reasons = append(planned.Reasons, err.Error())
		}
	}
	if scenarioRequiresVisualEvidence(scenario) && host.VisualEvidence != nil && !*host.VisualEvidence {
		planned.Reasons = append(planned.Reasons, "Visual Evidence is disabled")
	}
	if err := validatePlanTrustedPluginConfig(repoDir, host, scenario, payload); err != nil {
		planned.Reasons = append(planned.Reasons, err.Error())
	}
	planned.Compatible = len(planned.Reasons) == 0
	return planned
}

func validatePlanTrustedPluginConfig(repoDir string, host mayaHostConfig, scenario scenarioConfig, payload []manifestPayload) error {
	if trustedPluginArtifactsRoot(host) == "" || !host.usesRealSSH() || !manifestHasPluginArtifacts(payload) {
		return nil
	}
	if _, err := trustedPluginAllowlistRequiredPaths(repoDir, host, payload); err != nil {
		return err
	}
	versions := trustedPluginAllowlistMayaVersions(host, scenario)
	if len(versions) == 0 {
		return fmt.Errorf("maya version is required to locate TrustCenter preferences; set host mayaVersions or Scenario mayaVersion")
	}
	for _, version := range versions {
		if err := validateMayaPrefsVersion(strings.TrimSpace(version)); err != nil {
			return err
		}
	}
	return nil
}

func staticMayaVersions(host mayaHostConfig) []string {
	if host.Capabilities != nil && len(host.Capabilities.MayaBuilds) > 0 {
		return normalizeMayaVersions(host.Capabilities.MayaBuilds)
	}
	if host.usesRealSSH() {
		return normalizeMayaVersions(host.MayaVersions)
	}
	if host.FakeInstalledMayaVersions != nil {
		return normalizeMayaVersions(host.FakeInstalledMayaVersions)
	}
	if len(host.MayaVersions) > 0 {
		return normalizeMayaVersions(host.MayaVersions)
	}
	return []string{"2025"}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func scenarioPlanCapabilities(scenario scenarioConfig) []string {
	capabilities := []string{"script.execute"}
	if scenario.Evidence.Screenshots.Enabled {
		capabilities = append(capabilities, "screenshot.capture")
	}
	if scenario.Evidence.Recording.Enabled {
		capabilities = append(capabilities, "recording.capture")
	}
	for _, validator := range scenario.Validators {
		if validator.Type == "visualEvidence" && (validator.Required == nil || *validator.Required) {
			capabilities = append(capabilities, "visual-evidence.required")
			break
		}
	}
	return capabilities
}

func scenarioRequiresVisualEvidence(scenario scenarioConfig) bool {
	if scenario.Evidence.Screenshots.Enabled || scenario.Evidence.Recording.Enabled {
		return true
	}
	for _, validator := range scenario.Validators {
		if validator.Type == "visualEvidence" && (validator.Required == nil || *validator.Required) {
			return true
		}
	}
	return false
}

func summarizePlanPayload(path string) (int64, string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, "", err
	}
	if info.Mode().IsRegular() {
		file, err := os.Open(path)
		if err != nil {
			return 0, "", err
		}
		digest := sha256.New()
		size, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return 0, "", copyErr
		}
		if closeErr != nil {
			return 0, "", closeErr
		}
		return size, hex.EncodeToString(digest.Sum(nil)), nil
	}
	if !info.IsDir() {
		return 0, "", fmt.Errorf("payload path %s is not a regular file or directory", path)
	}
	digest := sha256.New()
	var size int64
	err = filepath.WalkDir(path, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(path, current)
		if err != nil {
			return err
		}
		_, _ = digest.Write([]byte(filepath.ToSlash(relative)))
		_, _ = digest.Write([]byte{0})
		_, _ = digest.Write([]byte(strconv.FormatInt(info.Size(), 10)))
		_, _ = digest.Write([]byte{0})
		file, err := os.Open(current)
		if err != nil {
			return err
		}
		copied, copyErr := io.Copy(digest, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		size += copied
		return nil
	})
	if err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(digest.Sum(nil)), nil
}

func printScenarioPlan(writer io.Writer, plan scenarioPlan, jsonOutput bool) {
	if jsonOutput {
		_ = json.NewEncoder(writer).Encode(plan)
		return
	}
	status := "ready"
	if !plan.Ready {
		status = "blocked"
	}
	_, _ = fmt.Fprintf(writer, "Scenario Plan: %s\nstatus: %s\n", planHumanText(plan.Scenario), status)
	mayaVersion := plan.Requirements.MayaVersion
	if requirement := plan.Requirements.HostCapabilities.Maya; requirement.Exact != "" {
		mayaVersion = "exact " + requirement.Exact
	} else if requirement.Minimum != "" {
		mayaVersion = "minimum " + requirement.Minimum
	}
	if mayaVersion == "" {
		mayaVersion = "any"
	}
	_, _ = fmt.Fprintf(writer, "maya-version: %s\nsession-broker: required\n", planHumanText(mayaVersion))
	printPlanVersionRequirement(writer, "python-version", plan.Requirements.HostCapabilities.Python)
	printPlanVersionRequirement(writer, "session-broker-version", versionRequirement{
		Exact: plan.Requirements.HostCapabilities.SessionBroker.Exact, Minimum: plan.Requirements.HostCapabilities.SessionBroker.Minimum,
	})
	for _, capability := range plan.Requirements.Capabilities {
		_, _ = fmt.Fprintf(writer, "capability: %s\n", planHumanText(capability))
	}
	for _, required := range []struct {
		label  string
		values []string
	}{
		{label: "session-broker-feature", values: plan.Requirements.HostCapabilities.SessionBroker.Features},
		{label: "capture", values: plan.Requirements.HostCapabilities.Capture},
		{label: "control", values: plan.Requirements.HostCapabilities.Control},
		{label: "renderer", values: plan.Requirements.HostCapabilities.Renderers},
		{label: "gpu", values: plan.Requirements.HostCapabilities.GPU},
		{label: "display", values: plan.Requirements.HostCapabilities.Display},
		{label: "licensing", values: plan.Requirements.HostCapabilities.Licensing},
	} {
		for _, value := range required.values {
			_, _ = fmt.Fprintf(writer, "required-%s: %s\n", required.label, planHumanText(value))
		}
	}
	if required := plan.Requirements.HostCapabilities.TrustedPluginArtifacts; required != nil {
		_, _ = fmt.Fprintf(writer, "required-trusted-plugin-artifacts: %t\n", *required)
	}
	for _, item := range plan.Payload {
		label := item.Source
		if label == "" {
			label = item.Name
		}
		_, _ = fmt.Fprintf(writer, "payload: %s %s -> %s (%d bytes, sha256 %s) [%s]\n", planHumanText(item.Kind), planHumanText(label), planHumanText(item.Destination), item.Size, planHumanText(item.SHA256), planHumanText(item.Status))
	}
	for _, profile := range plan.TargetProfiles {
		_, _ = fmt.Fprintf(writer, "target-profile: %s (Host Pool %s) [%s]\n", planHumanText(profile.Name), planHumanText(profile.HostPool), planHumanText(planCompatibilityLabel(profile.Compatible, profile.Reasons)))
		for _, host := range profile.Hosts {
			_, _ = fmt.Fprintf(writer, "host: %s [%s]\n", planHumanText(host.ID), planHumanText(planCompatibilityLabel(host.Compatible, host.Reasons)))
		}
	}
	for _, issue := range plan.Issues {
		if issue.Source == "" {
			_, _ = fmt.Fprintf(writer, "issue: %s\n", planHumanText(issue.Reason))
			continue
		}
		_, _ = fmt.Fprintf(writer, "issue: %s: %s\n", planHumanText(issue.Source), planHumanText(issue.Reason))
	}
	_, _ = fmt.Fprintln(writer, "host-contact: none")
}

func printPlanVersionRequirement(writer io.Writer, label string, requirement versionRequirement) {
	if requirement.Exact != "" {
		_, _ = fmt.Fprintf(writer, "%s: exact %s\n", label, planHumanText(requirement.Exact))
	} else if requirement.Minimum != "" {
		_, _ = fmt.Fprintf(writer, "%s: minimum %s\n", label, planHumanText(requirement.Minimum))
	}
}

func planHumanText(value string) string {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return strconv.QuoteToGraphic(value)
	}
	return value
}

func planCompatibilityLabel(compatible bool, reasons []string) string {
	if compatible {
		return "compatible"
	}
	if len(reasons) == 0 {
		return "incompatible"
	}
	return "incompatible: " + strings.Join(reasons, "; ")
}
