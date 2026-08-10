package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var mayaVersionPattern = regexp.MustCompile(`^[0-9]{4}(?:\.[0-9]+)?$`)

type hostHealthReport struct {
	TargetProfile string
	HostPool      string
	HostID        string
	Scenario      string
	Runtime       runtimeMetadata
	Layers        []hostHealthLayer
	Healthy       bool
	Warnings      []string
}

type hostHealthLayer struct {
	ID                 string
	Layer              string
	Status             string
	Detail             string
	Hint               string
	Source             string
	State              string
	BlockedBy          string
	ActiveRun          string
	KeptRun            string
	InteractiveDesktop bool
}

type doctorCheck = hostHealthLayer

func runDoctor(repoDir string, options doctorOptions) hostHealthReport {
	report := hostHealthReport{TargetProfile: options.TargetProfile, Scenario: options.ScenarioName, Healthy: true}
	add := func(check hostHealthLayer) {
		if check.Status == "fail" {
			report.Healthy = false
		}
		report.Layers = append(report.Layers, check)
	}

	config, configPath, err := loadRepoRunConfig(repoDir)
	if err != nil {
		add(failedCheck("local-config", err.Error(), "Run maya-stall init or fix the repo config schema."))
		return report
	}
	add(okCheck("local-config", repoRelativePath(repoDir, configPath)))

	var scenario scenarioContract
	scenarioInputsValid := true
	if options.ScenarioName != "" {
		selected, err := resolveScenarioContract(config, options.ScenarioName)
		if err != nil {
			scenarioInputsValid = false
			var userErr *usageError
			if errors.As(err, &userErr) {
				add(failedCheck("scenario-inputs", err.Error(), "Choose a configured Scenario or add it to the repo config. See docs/setup/windows-maya-host.md#scenario-inputs."))
			} else {
				add(failedCheck("scenario-inputs", err.Error(), "Fix the Scenario payload paths, expectedOutputs, and Validators in repo config. See docs/setup/windows-maya-host.md#scenario-inputs."))
			}
		} else if err := validateScenarioInputs(repoDir, selected); err != nil {
			scenarioInputsValid = false
			add(failedCheck("scenario-inputs", err.Error(), "Fix the Scenario payload paths, expectedOutputs, and Validators in repo config. See docs/setup/windows-maya-host.md#scenario-inputs."))
		} else {
			scenario = selected
			add(okCheck("scenario-inputs", options.ScenarioName))
		}
	}
	if options.RepairTrustedPluginAllowlist && !scenarioInputsValid {
		return report
	}

	hostConfig, err := loadUserHostConfig(options.HostConfig)
	if err != nil {
		add(failedCheck("target-profile", err.Error(), "Point --host-config at a valid user host config or omit it for the fake default. See docs/setup/windows-maya-host.md#target-profile-and-host-pool."))
		return report
	}
	profile, ok := hostConfig.TargetProfiles[options.TargetProfile]
	if !ok {
		add(failedCheck("target-profile", fmt.Sprintf("unknown Target Profile %q", options.TargetProfile), "Choose a configured Target Profile or add it to the host config. See docs/setup/windows-maya-host.md#target-profile-and-host-pool."))
		return report
	}
	add(okCheck("target-profile", options.TargetProfile))
	report.HostPool = profile.HostPool

	pool, ok := hostConfig.HostPools[profile.HostPool]
	if !ok {
		add(failedCheck("host-pool", fmt.Sprintf("unknown Host Pool %q", profile.HostPool), "Fix the Target Profile hostPool reference in the host config. See docs/setup/windows-maya-host.md#target-profile-and-host-pool."))
		return report
	}
	if len(pool.Hosts) == 0 {
		add(failedCheck("host-pool", fmt.Sprintf("%s has no Maya Hosts", profile.HostPool), "Add at least one Maya Host to the Host Pool. See docs/setup/windows-maya-host.md#target-profile-and-host-pool."))
		return report
	}
	for _, host := range pool.Hosts {
		if err := validateHostID(host.ID); err != nil {
			add(failedCheck("host-pool", err.Error(), "Fix Maya Host ids in the Host Pool config. See docs/setup/windows-maya-host.md#target-profile-and-host-pool."))
			return report
		}
	}
	if !hostPoolHasHealthyHost(pool.Hosts) {
		add(failedCheck("host-pool", fmt.Sprintf("%s has no healthy Maya Hosts", profile.HostPool), "Mark a ready Maya Host healthy or repair one host before running. See docs/setup/windows-maya-host.md#target-profile-and-host-pool."))
		return report
	}
	add(okCheck("host-pool", profile.HostPool))

	if options.HostPin == "" && options.ScenarioName == "" && !hostPoolHasRealSSHHost(pool.Hosts) {
		return report
	}
	host, err := selectDoctorHost(pool.Hosts, options.HostPin)
	if err != nil {
		add(failedCheck("host", err.Error(), "Choose a Maya Host from the selected Target Profile or repair Host Pool config. See docs/setup/windows-maya-host.md#target-profile-and-host-pool."))
		return report
	}
	if options.HostPin == "" && options.ScenarioName == "" {
		if realSSHHost, ok := firstHealthyRealSSHHost(pool.Hosts); ok {
			host = realSSHHost
		}
	}
	report.Warnings = append(report.Warnings, sweepKeptSessions(repoDir, host.ID, time.Now())...)
	add(okCheck("host", host.ID))
	report.HostID = host.ID
	var repairRequiredPaths []string
	if options.RepairTrustedPluginAllowlist {
		var check *doctorCheck
		repairRequiredPaths, check = trustedPluginAllowlistLocalConfigCheck(repoDir, host, scenario)
		if check != nil {
			add(*check)
			return report
		}
	}
	checkHostLayers(repoDir, options, host, scenario, repairRequiredPaths, &report, add)

	return report
}

func validateScenarioInputs(repoDir string, scenario scenarioContract) error {
	for _, item := range scenario.Payload {
		if err := validatePayloadPathForTransport(repoDir, item.Source); err != nil {
			return fmt.Errorf("stage %s payload %s: %w", item.Kind, item.Source, err)
		}
	}
	return nil
}

func validateScenarioRemotePaths(scenario scenarioConfig) error {
	if err := rejectSFTPRepoPath(scenario.ExpectedOutputs.ScenarioResult); err != nil {
		return err
	}
	for _, path := range scenario.ExpectedOutputs.Files {
		if err := rejectSFTPRepoPath(path); err != nil {
			return err
		}
	}
	for _, validator := range scenario.Validators {
		if validator.Path == "" {
			continue
		}
		if err := rejectSFTPRepoPath(validator.Path); err != nil {
			return err
		}
	}
	return nil
}

func hostPoolHasHealthyHost(hosts []mayaHostConfig) bool {
	for _, host := range hosts {
		if isHealthyHost(host) {
			return true
		}
	}
	return false
}

func hostPoolHasRealSSHHost(hosts []mayaHostConfig) bool {
	_, ok := firstHealthyRealSSHHost(hosts)
	return ok
}

func firstHealthyRealSSHHost(hosts []mayaHostConfig) (mayaHostConfig, bool) {
	for _, host := range hosts {
		if isHealthyHost(host) && host.usesRealSSH() {
			return host, true
		}
	}
	return mayaHostConfig{}, false
}

func selectDoctorHost(hosts []mayaHostConfig, hostPin string) (mayaHostConfig, error) {
	for _, host := range hosts {
		if err := validateHostID(host.ID); err != nil {
			return mayaHostConfig{}, err
		}
		if hostPin != "" {
			if host.ID == hostPin {
				return host, nil
			}
			continue
		}
		if isHealthyHost(host) {
			return host, nil
		}
	}
	if hostPin != "" {
		return mayaHostConfig{}, fmt.Errorf("pinned Maya Host %q is not in Target Profile", hostPin)
	}
	return mayaHostConfig{}, fmt.Errorf("no healthy Maya Host available")
}

func checkHostLayers(repoDir string, options doctorOptions, host mayaHostConfig, scenario scenarioContract, repairRequiredPaths []string, report *hostHealthReport, add func(hostHealthLayer)) {
	sshOK := true
	if host.usesRealSSH() {
		sshCheck := realSSHLayer(host)
		add(sshCheck)
		workRootCheck := realWorkRootLayer(host)
		add(workRootCheck)
		sshOK = sshCheck.Status == "ok" && workRootCheck.Status == "ok"
	} else {
		add(statusLayer("fake-ssh", host.SSH.FakeStatus, "reachable", []string{"", "ok", "healthy", "reachable"}, "Fix SSH reachability for this Maya Host. See docs/setup/windows-maya-host.md#openssh-reachability."))
		add(statusLayer("work-root", host.WorkRoot, "writable", []string{"", "ok", "writable"}, "Fix the host work root path or permissions. See docs/setup/windows-maya-host.md#work-root."))
	}
	lockCheck := hostLockLayer(repoDir, host)
	add(lockCheck)
	resolved, runtimeErr := resolveRuntimeForHost(host)
	if runtimeErr != nil {
		add(failedCheck("runtime", runtimeErr.Error(), "Choose a supported runtime profile: fake-local or ssh-sessiond. See docs/setup/windows-maya-host.md#session-broker."))
	} else {
		report.Runtime = resolved.Metadata
		add(withSource(okCheck("runtime", resolved.Metadata.Profile), resolved.Metadata.Profile))
	}
	probeLockDetail := ""
	needsProbeLock := runtimeErr == nil && host.Broker.isGGMayaSessiond()
	if options.RepairTrustedPluginAllowlist && host.usesRealSSH() {
		needsProbeLock = true
	}
	if needsProbeLock && lockCheck.Status == "ok" {
		lock, locked, err := acquireRunHostLock(repoDir, host)
		if err != nil {
			probeLockDetail = err.Error()
		} else if locked {
			probeLockDetail = fmt.Sprintf("%s locked", host.ID)
		} else {
			defer func() {
				if err := lock.release(); err != nil {
					add(failedCheck("host-lock-release", fmt.Sprintf("release doctor probe lock for %s: %v", host.ID, err), "Inspect the Host Lock state and permissions. See docs/setup/windows-maya-host.md#host-lock-and-retention."))
				}
			}()
		}
	}
	if options.RepairTrustedPluginAllowlist {
		if host.usesRealSSH() && (lockCheck.Status != "ok" || probeLockDetail != "") {
			add(withBlockedBy(failedCheck("maya-version", "skipped because Host Lock is not clear", "Wait for the active Fresh Run or clear the stale Host Lock before probing installed Maya versions. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock"))
		} else {
			add(mayaVersionLayer(options, host, scenario.Config))
		}
		add(trustedPluginAllowlistDoctorLayer(repoDir, host, scenario, repairRequiredPaths, true, sshOK, lockCheck.Status == "ok" && probeLockDetail == ""))
		add(withSource(okCheck("session-broker", "skipped during TrustCenter repair"), "repair"))
		add(withSource(okCheck("visual-evidence", "skipped during TrustCenter repair"), "repair"))
		add(withSource(okCheck("desktop-control", "skipped during TrustCenter repair"), "repair"))
		return
	}
	brokerInvalidReason := host.Broker.invalidReason()
	if runtimeErr != nil {
		brokerInvalidReason = runtimeErr.Error()
	}
	brokerLayerInvalidReason := ""
	if runtimeErr != nil || host.Broker.missingStructuredType() || strings.TrimSpace(host.Broker.Type) != "" {
		brokerLayerInvalidReason = brokerInvalidReason
	}
	sessionBrokerOK := false
	if runtimeErr == nil && host.Broker.isGGMayaSessiond() {
		broker := resolved.Broker.(ggMayaSessiondBroker)
		var check hostHealthLayer
		if err := broker.validate(); err != nil {
			check = failedCheck("session-broker", err.Error(), "Configure gg_mayasessiond paths in host config. See docs/setup/windows-maya-host.md#session-broker.")
		} else if probeLockDetail != "" {
			check = withBlockedBy(failedCheck("session-broker", "skipped because Host Lock is not clear: "+probeLockDetail, "Wait for the active Fresh Run or clear the stale Host Lock before probing gg_mayasessiond. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock")
		} else if lockCheck.Status == "ok" {
			check = realSessionBrokerLayer(host)
		} else {
			check = withBlockedBy(failedCheck("session-broker", "skipped because Host Lock is not clear", "Wait for the active Fresh Run or clear the stale Host Lock before probing gg_mayasessiond. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock")
		}
		sessionBrokerOK = check.Status == "ok"
		add(check)
	} else if brokerLayerInvalidReason != "" {
		add(failedCheck("session-broker", brokerLayerInvalidReason, "Use broker.type: gg-mayasessiond or a legacy fake broker status. See docs/setup/windows-maya-host.md#session-broker."))
	} else {
		add(withSource(statusLayer("session-broker", host.Broker.fakeStatus(), "reachable", []string{"", "ok", "healthy", "reachable"}, "Start or repair the Session Broker on this Maya Host. See docs/setup/windows-maya-host.md#session-broker."), "fake"))
	}
	if host.usesRealSSH() && probeLockDetail != "" {
		add(withBlockedBy(failedCheck("maya-version", "skipped because Host Lock is not clear: "+probeLockDetail, "Wait for the active Fresh Run or clear the stale Host Lock before probing installed Maya versions. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock"))
	} else if host.usesRealSSH() && lockCheck.Status != "ok" {
		add(withBlockedBy(failedCheck("maya-version", "skipped because Host Lock is not clear", "Wait for the active Fresh Run or clear the stale Host Lock before probing installed Maya versions. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock"))
	} else {
		add(mayaVersionLayer(options, host, scenario.Config))
	}
	add(trustedPluginAllowlistDoctorLayer(repoDir, host, scenario, nil, false, sshOK, lockCheck.Status == "ok"))
	if host.VisualEvidence != nil && !*host.VisualEvidence {
		add(withSource(failedCheck("visual-evidence", "unavailable", "Enable screenshot capture through the Session Broker. See docs/setup/windows-maya-host.md#visual-evidence."), "config"))
	} else if brokerInvalidReason != "" {
		add(withBlockedBy(failedCheck("visual-evidence", "unavailable: "+brokerInvalidReason, "Use a valid Session Broker before checking screenshot capture. See docs/setup/windows-maya-host.md#visual-evidence."), "session-broker"))
	} else if runtimeErr == nil && host.Broker.isGGMayaSessiond() {
		broker := resolved.Broker.(ggMayaSessiondBroker)
		if err := broker.validate(); err != nil {
			add(withBlockedBy(failedCheck("visual-evidence", "unavailable: "+err.Error(), "Configure gg_mayasessiond paths before checking screenshot capture. See docs/setup/windows-maya-host.md#visual-evidence."), "session-broker"))
		} else if probeLockDetail != "" {
			add(withBlockedBy(failedCheck("visual-evidence", "skipped because Host Lock is not clear: "+probeLockDetail, "Wait for the active Fresh Run or clear the stale Host Lock before probing viewport.capture. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock"))
		} else if lockCheck.Status != "ok" {
			add(withBlockedBy(failedCheck("visual-evidence", "skipped because Host Lock is not clear", "Wait for the active Fresh Run or clear the stale Host Lock before probing viewport.capture. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock"))
		} else if !sessionBrokerOK {
			add(withBlockedBy(failedCheck("visual-evidence", "skipped because session-broker is not healthy", "Repair the gg_mayasessiond session-broker layer before probing viewport.capture. See docs/setup/windows-maya-host.md#visual-evidence."), "session-broker"))
		} else {
			add(realSessionBrokerVisualEvidenceLayer(host))
		}
	} else {
		add(withSource(okCheck("visual-evidence", "available"), "fake"))
	}
	if brokerInvalidReason != "" {
		add(withBlockedBy(failedCheck("desktop-control", "unavailable: "+brokerInvalidReason, "Use a valid Session Broker before checking desktop control. See docs/setup/windows-maya-host.md#desktop-control."), "session-broker"))
	} else if runtimeErr == nil && host.Broker.isGGMayaSessiond() {
		broker := resolved.Broker.(ggMayaSessiondBroker)
		if err := broker.validate(); err != nil {
			add(withBlockedBy(failedCheck("desktop-control", "unavailable: "+err.Error(), "Configure gg_mayasessiond paths before checking desktop control. See docs/setup/windows-maya-host.md#desktop-control."), "session-broker"))
		} else if probeLockDetail != "" {
			add(withBlockedBy(failedCheck("desktop-control", "skipped because Host Lock is not clear: "+probeLockDetail, "Wait for the active Fresh Run or clear the stale Host Lock before using desktop control. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock"))
		} else if lockCheck.Status != "ok" {
			add(withBlockedBy(failedCheck("desktop-control", "skipped because Host Lock is not clear", "Wait for the active Fresh Run or clear the stale Host Lock before using desktop control. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "host-lock"))
		} else if !sessionBrokerOK {
			add(withBlockedBy(failedCheck("desktop-control", "skipped because session-broker is not healthy", "Repair the gg_mayasessiond session-broker layer before using desktop control. See docs/setup/windows-maya-host.md#desktop-control."), "session-broker"))
		} else {
			add(realSessionBrokerDesktopControlLayer(resolved.Broker))
		}
	} else if runtimeErr == nil {
		add(fakeDesktopControlLayer(resolved.Broker))
	}
}

type sessiondDoctorOutput struct {
	OK     bool `json:"ok"`
	Checks []struct {
		ID string `json:"id"`
		OK bool   `json:"ok"`
	} `json:"checks"`
}

type sessiondStatusOutput struct {
	State struct {
		Status          string `json:"status"`
		MayaAlive       bool   `json:"maya_alive"`
		MCPAlive        bool   `json:"mcp_alive"`
		CallServerReady bool   `json:"call_server_ready"`
	} `json:"state"`
	DerivedStatus string `json:"derived_status"`
}

type windowsProcessSession struct {
	ProcessID   int    `json:"ProcessId"`
	SessionID   int    `json:"SessionId"`
	SessionName string `json:"SessionName"`
	UserName    string `json:"UserName"`
	Name        string `json:"Name"`
}

func realSessionBrokerLayer(host mayaHostConfig) doctorCheck {
	broker := ggMayaSessiondBroker{host: host}
	health, err := broker.ensureReady()
	if err != nil {
		return withSource(failedCheck("session-broker", err.Error(), "Start or repair gg_mayasessiond on this Maya Host. See docs/setup/windows-maya-host.md#session-broker."), "gg-mayasessiond")
	}
	if !health.OK {
		check := withSource(withState(failedCheck("session-broker", health.Detail, health.Hint), health.Reason), "gg-mayasessiond")
		return check
	}
	detail := health.Detail
	if health.Recovered {
		detail = "recovered gg_mayasessiond commandPort health; Maya UI is interactive"
	}
	check := withSource(okCheck("session-broker", detail), "gg-mayasessiond")
	if health.Recovered {
		check = withState(check, "recovered")
	}
	check.InteractiveDesktop = health.InteractiveDesktop
	return check
}

func failingSessiondDoctorChecks(doctor sessiondDoctorOutput) []string {
	var failing []string
	for _, check := range doctor.Checks {
		if !check.OK && check.ID != "" {
			failing = append(failing, check.ID)
		}
	}
	return failing
}

func realSessionBrokerVisualEvidenceLayer(host mayaHostConfig) doctorCheck {
	broker := ggMayaSessiondBroker{host: host}
	result, err := broker.callCapture()
	if err != nil {
		return withSource(failedCheck("visual-evidence", err.Error(), "Repair viewport.capture in gg_mayasessiond. See docs/setup/windows-maya-host.md#visual-evidence."), "session-broker")
	}
	if !result.OK {
		return withSource(failedCheck("visual-evidence", result.Error, "Repair viewport.capture in gg_mayasessiond. See docs/setup/windows-maya-host.md#visual-evidence."), "session-broker")
	}
	if _, _, err := captureImageData(result); err != nil {
		return withSource(failedCheck("visual-evidence", err.Error(), "Repair viewport.capture in gg_mayasessiond. See docs/setup/windows-maya-host.md#visual-evidence."), "session-broker")
	}
	if err := broker.probeDesktopRecordingReadiness(); err != nil {
		return withSource(failedCheck("visual-evidence", "viewport.capture available; desktop recording unavailable: "+err.Error(), "Install ffmpeg locally and repair Windows desktop recording prerequisites. See docs/setup/windows-maya-host.md#visual-evidence."), "session-broker")
	}
	return withSource(okCheck("visual-evidence", "viewport.capture available; desktop recording available"), "session-broker")
}

func realSessionBrokerDesktopControlLayer(broker sessionBroker) doctorCheck {
	if _, ok := broker.(desktopClicker); !ok {
		return withSource(failedCheck("desktop-control", "desktop click unavailable", "Use a Session Broker adapter that supports explicit desktop control. See docs/setup/windows-maya-host.md#desktop-control."), "session-broker")
	}
	return withSource(okCheck("desktop-control", "desktop click available"), "session-broker")
}

func fakeDesktopControlLayer(broker sessionBroker) doctorCheck {
	if _, ok := broker.(desktopClicker); !ok {
		return withSource(failedCheck("desktop-control", "desktop click unavailable", "Use a Session Broker adapter that supports explicit desktop control. See docs/setup/windows-maya-host.md#desktop-control."), "fake")
	}
	return withSource(okCheck("desktop-control", "available"), "fake")
}

func sessiondDoctorArgs(host mayaHostConfig) []string {
	args := []string{"doctor", "--state-dir", host.Broker.StateDir}
	if host.Broker.MCPSource != "" {
		args = append(args, "--mcp-src", host.Broker.MCPSource)
	}
	args = append(args, "--json")
	return args
}

func mayaProcessSessions(host mayaHostConfig) ([]windowsProcessSession, error) {
	script := `$ErrorActionPreference = 'Stop'
Get-CimInstance Win32_Process -Filter "Name = 'maya.exe'" | Select-Object ProcessId,SessionId,Name | ConvertTo-Json -Compress`
	raw, err := runSSHCommandOutput(host, encodedPowerShellCommand(script), sessiondCommandTimeout)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var processes []windowsProcessSession
		if err := json.Unmarshal([]byte(trimmed), &processes); err != nil {
			return nil, fmt.Errorf("parse maya.exe process JSON: %w", err)
		}
		return processes, nil
	}
	var process windowsProcessSession
	if err := json.Unmarshal([]byte(trimmed), &process); err != nil {
		return nil, fmt.Errorf("parse maya.exe process JSON: %w", err)
	}
	return []windowsProcessSession{process}, nil
}

func statusLayer(layer string, value string, okDetail string, okValues []string, hint string) doctorCheck {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, okValue := range okValues {
		if normalized == okValue {
			return okCheck(layer, okDetail)
		}
	}
	if normalized == "" {
		normalized = "unknown"
	}
	return failedCheck(layer, normalized, hint)
}

func mayaVersionLayer(options doctorOptions, host mayaHostConfig, scenario scenarioConfig) doctorCheck {
	versions, err := installedMayaVersionsForDoctor(host)
	if err != nil {
		return failedCheck("maya-version", "could not determine installed Maya versions: "+err.Error(), mayaVersionHint())
	}
	if len(versions) == 0 {
		return failedCheck("maya-version", "no Autodesk Maya installation found on host"+mayaVersionDriftDetail(host.MayaVersions, versions), mayaVersionHint())
	}
	installed := strings.Join(versions, ",")
	drift := mayaVersionDriftDetail(host.MayaVersions, versions)
	requirement := normalizedScenarioRequirements(scenario).Maya
	if options.ScenarioName == "" || (requirement.Exact == "" && requirement.Minimum == "") {
		return okCheck("maya-version", installed+drift)
	}
	for _, version := range versions {
		if requirement.Exact != "" && sameMayaBuildVersion(version, requirement.Exact) {
			return okCheck("maya-version", fmt.Sprintf("%s satisfies Scenario %s%s", version, options.ScenarioName, drift))
		}
		if requirement.Minimum != "" && versionAtLeast(version, requirement.Minimum) {
			return okCheck("maya-version", fmt.Sprintf("%s satisfies Scenario %s%s", version, options.ScenarioName, drift))
		}
	}
	required := requirement.Exact
	description := required
	if scenario.MayaVersion == "" && requirement.Exact != "" {
		description = "exact " + requirement.Exact
	}
	if requirement.Minimum != "" {
		required = requirement.Minimum
		description = "minimum " + requirement.Minimum
	}
	return failedCheck("maya-version", fmt.Sprintf("Scenario %s needs %s; host has %s%s", options.ScenarioName, description, installed, drift), mayaVersionMismatchHint(required, installed))
}

func installedMayaVersionsForDoctor(host mayaHostConfig) ([]string, error) {
	if host.usesRealSSH() {
		return probeInstalledMayaVersions(host)
	}
	if host.FakeInstalledMayaVersions != nil {
		return normalizeMayaVersions(host.FakeInstalledMayaVersions), nil
	}
	if len(host.MayaVersions) > 0 {
		return normalizeMayaVersions(host.MayaVersions), nil
	}
	return []string{"2025"}, nil
}

func mayaVersionDriftDetail(configured []string, installed []string) string {
	configured = normalizeMayaVersions(configured)
	if len(configured) == 0 || sameStringSet(configured, installed) {
		return ""
	}
	return "; config declares " + strings.Join(configured, ",") + " (drift)"
}

func normalizeMayaVersions(versions []string) []string {
	seen := map[string]bool{}
	normalized := make([]string, 0, len(versions))
	for _, version := range versions {
		version = strings.TrimSpace(version)
		if !mayaVersionPattern.MatchString(version) || seen[version] {
			continue
		}
		seen[version] = true
		normalized = append(normalized, version)
	}
	sort.Strings(normalized)
	return normalized
}

func sameStringSet(left []string, right []string) bool {
	leftSet := map[string]bool{}
	rightSet := map[string]bool{}
	for _, value := range left {
		leftSet[value] = true
	}
	for _, value := range right {
		rightSet[value] = true
	}
	if len(leftSet) != len(rightSet) {
		return false
	}
	for value := range leftSet {
		if !rightSet[value] {
			return false
		}
	}
	return true
}

func mayaVersionHint() string {
	return "Install a compatible Autodesk Maya version or choose another Maya Host. See docs/setup/windows-maya-host.md#autodesk-maya."
}

func mayaVersionMismatchHint(required string, installed string) string {
	return fmt.Sprintf("Install Autodesk Maya %s or choose a Maya Host with %s installed; detected %s. See docs/setup/windows-maya-host.md#autodesk-maya.", required, required, installed)
}

func hostLockLayer(repoDir string, host mayaHostConfig) doctorCheck {
	if lockDir, ok := fakeHostSideLockDir(host); ok {
		isStale := isStaleHostSideLock
		if host.usesLocalWindows() {
			isStale = func(lockPath string) (bool, error) {
				return isRecoverableLocalSessiondHostLock(host, lockPath)
			}
		}
		check := withSource(localHostLockLayer(filepath.Join(lockDir, localHostSideLockID(host)+".lock"), host.ID, isStale), "maya-host")
		return hostLockLayerWithLegacyLocal(repoDir, host.ID, check)
	}
	if host.usesRealSSH() {
		if lockDir, ok := fakeSSHHostSideLockDir(host); ok {
			check := withSource(localHostLockLayer(filepath.Join(lockDir, "host.lock"), host.ID, isStaleHostSideLock), "maya-host")
			return hostLockLayerWithLegacyLocal(repoDir, host.ID, check)
		}
		return hostLockLayerWithLegacyLocal(repoDir, host.ID, withSource(remoteHostLockLayer(host), "maya-host"))
	}
	lockPath := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts", host.ID+".lock")
	return withSource(localHostLockLayer(lockPath, host.ID, isStaleHostLock), "local")
}

func hostLockLayerWithLegacyLocal(repoDir string, hostID string, hostSide doctorCheck) doctorCheck {
	if hostSide.Status != "ok" {
		return hostSide
	}
	localPath := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts", hostID+".lock")
	local := withSource(localHostLockLayer(localPath, hostID, isStaleHostLock), "local")
	if local.Status != "ok" {
		return local
	}
	return hostSide
}

func localHostLockLayer(lockPath string, hostID string, isStale func(string) (bool, error)) doctorCheck {
	_, err := os.Stat(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return withState(okCheck("host-lock", "unlocked"), "unlocked")
	}
	if err != nil {
		return failedCheck("host-lock", err.Error(), "Inspect the Host Lock state directory and permissions. See docs/setup/windows-maya-host.md#host-lock-and-retention.")
	}
	if keptRun, found, err := readHostLockKeptRun(lockPath); err != nil {
		return failedCheck("host-lock", err.Error(), "Inspect or remove the unreadable Host Lock file. See docs/setup/windows-maya-host.md#host-lock-and-retention.")
	} else if found {
		check := withState(failedCheck("host-lock", fmt.Sprintf("%s locked", hostID), "Stop the Kept Session or clear the Host Lock after verifying no run is active. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "kept")
		check.KeptRun = keptRun
		return check
	}
	stale, err := isStale(lockPath)
	if err != nil {
		return failedCheck("host-lock", err.Error(), "Inspect or remove the unreadable Host Lock file. See docs/setup/windows-maya-host.md#host-lock-and-retention.")
	}
	if stale {
		return withState(okCheck("host-lock", "unlocked"), "stale")
	}
	content, err := os.ReadFile(lockPath)
	if err != nil {
		return failedCheck("host-lock", err.Error(), "Inspect or remove the unreadable Host Lock file. See docs/setup/windows-maya-host.md#host-lock-and-retention.")
	}
	activeRun := parseHostLockOwner(string(content)).ActiveRun
	detail := fmt.Sprintf("%s locked", hostID)
	if activeRun != "" {
		detail += " by active run " + activeRun
	}
	check := withState(failedCheck("host-lock", detail, "Wait for the active Fresh Run or clear the stale Host Lock after verifying no run is active. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "active")
	check.ActiveRun = activeRun
	return check
}

func remoteHostLockLayer(host mayaHostConfig) doctorCheck {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return failedCheck("host-lock", err.Error(), "Fix the Maya Host work root before checking Host Lock state. See docs/setup/windows-maya-host.md#host-lock-and-retention.")
	}
	result, err := readRemoteHostLockState(host, lockPath)
	if err != nil {
		return failedCheck("host-lock", err.Error(), "Inspect the Host Lock under the Maya Host work root. See docs/setup/windows-maya-host.md#host-lock-and-retention.")
	}
	if result.Error != "" {
		return failedCheck("host-lock", result.Error, "Inspect the Host Lock under the Maya Host work root. See docs/setup/windows-maya-host.md#host-lock-and-retention.")
	}
	switch result.State {
	case "", "unlocked":
		return withState(okCheck("host-lock", "unlocked"), "unlocked")
	case "kept":
		check := withState(failedCheck("host-lock", fmt.Sprintf("%s locked", host.ID), "Stop the Kept Session before reusing this Maya Host. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "kept")
		check.KeptRun = result.KeptRun
		return check
	}
	if result.LeaseExpired {
		inactive, statusErr := remoteHostLockSessionInactive(host, result)
		if statusErr != nil {
			check := withState(failedCheck("host-lock", "expired lease; Session Broker state unavailable", "Repair Session Broker status and verify the Maya UI Session before recovering this Host Lock. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "stale-unverified")
			check.ActiveRun = result.ActiveRun
			return check
		}
		if inactive {
			check := withState(okCheck("host-lock", "expired lease; Session Broker inactive"), "stale")
			check.ActiveRun = result.ActiveRun
			return check
		}
		check := withState(failedCheck("host-lock", "expired lease but Session Broker remains active", "Stop and verify the owned Maya UI Session before recovering this Host Lock. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "active")
		check.ActiveRun = result.ActiveRun
		return check
	}
	detail := fmt.Sprintf("%s locked", host.ID)
	if result.ActiveRun != "" {
		detail += " by active run " + result.ActiveRun
	}
	check := withState(failedCheck("host-lock", detail, "Wait for the active Fresh Run. See docs/setup/windows-maya-host.md#host-lock-and-retention."), "active")
	check.ActiveRun = result.ActiveRun
	return check
}

func okCheck(layer string, detail string) doctorCheck {
	return doctorCheck{ID: layer, Layer: layer, Status: "ok", Detail: detail}
}

func failedCheck(layer string, detail string, hint string) doctorCheck {
	return doctorCheck{ID: layer, Layer: layer, Status: "fail", Detail: detail, Hint: hint}
}

func withSource(check hostHealthLayer, source string) hostHealthLayer {
	check.Source = source
	return check
}

func withState(check hostHealthLayer, state string) hostHealthLayer {
	check.State = state
	return check
}

func withBlockedBy(check hostHealthLayer, blockedBy string) hostHealthLayer {
	check.BlockedBy = blockedBy
	return check
}

func printHostHealthReport(stdout io.Writer, report hostHealthReport) {
	for _, check := range report.Layers {
		fmt.Fprintf(stdout, "%s: %s - %s\n", check.ID, check.Status, check.Detail)
		if check.Hint != "" {
			fmt.Fprintf(stdout, "hint: %s\n", check.Hint)
		}
	}
}

func formatHostHealthReport(report hostHealthReport) string {
	parts := make([]string, 0, len(report.Layers))
	for _, check := range report.Layers {
		part := fmt.Sprintf("%s=%s", check.ID, check.Status)
		if check.Source != "" {
			part += " source:" + check.Source
		}
		if check.State != "" {
			part += " state:" + check.State
		}
		if check.ActiveRun != "" {
			part += " activeRun:" + check.ActiveRun
		}
		if check.KeptRun != "" {
			part += " keptRun:" + check.KeptRun
		}
		if check.InteractiveDesktop {
			part += " interactiveDesktop:true"
		}
		if check.BlockedBy != "" {
			part += " blockedBy:" + check.BlockedBy
		}
		if check.Detail != "" {
			part += " detail:" + check.Detail
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}
