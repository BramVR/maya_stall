package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	visualEvidenceOriginBrokerCapture     = "broker-capture"
	visualEvidenceOriginFakeBrokerCapture = "fake-broker-capture"
	visualEvidenceOriginDiscovered        = "discovered"
)

type visualEvidenceOptions struct {
	HostConfig    string
	TargetProfile string
	HostPin       string
}

type visualEvidenceCapturePlan struct {
	Kind     string
	Name     string
	Duration time.Duration
	FPS      int
}

func parseVisualEvidenceArgs(args []string) (visualEvidenceOptions, error) {
	options := visualEvidenceOptions{TargetProfile: "default"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--host-config":
			i++
			if i >= len(args) || args[i] == "" {
				return visualEvidenceOptions{}, newUsageError("--host-config needs a path")
			}
			options.HostConfig = args[i]
		case "--target-profile":
			i++
			if i >= len(args) || args[i] == "" {
				return visualEvidenceOptions{}, newUsageError("--target-profile needs a name")
			}
			options.TargetProfile = args[i]
		case "--host":
			i++
			if i >= len(args) || args[i] == "" {
				return visualEvidenceOptions{}, newUsageError("--host needs a Maya Host id")
			}
			options.HostPin = args[i]
		default:
			return visualEvidenceOptions{}, newUsageError("unknown visual evidence option %q", arg)
		}
	}
	return options, nil
}

func captureStandaloneVisualEvidence(repoDir string, options visualEvidenceOptions, runtime runRuntime, kind string) (outcome runOutcome, artifact visualEvidenceArtifact, err error) {
	plan, err := planStandaloneVisualEvidence(kind)
	if err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	if runtime.Now == nil {
		runtime.Now = time.Now
	}
	host, err := selectHostForRun(repoDir, runOptions{
		HostConfig:    options.HostConfig,
		TargetProfile: options.TargetProfile,
		HostPin:       options.HostPin,
	})
	if err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	defer func() {
		if host.release != nil {
			if releaseErr := host.release(); releaseErr != nil {
				err = errors.Join(err, fmt.Errorf("release Host Lock for %s: %w", host.HostID, releaseErr))
			}
		}
	}()
	resolved, err := resolveRuntimeForHost(host.Config)
	if err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	if err := rejectMismatchedRuntimeOverride(resolved, runtime); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	if runtime.Broker == nil {
		runtime.Broker = resolved.Broker
	}

	runID := runtime.Now().UTC().Format("20060102T150405.000000000Z")
	workspace, err := newRunWorkspace(repoDir, runID, host.Config.WorkRoot, evidenceStandaloneResultName)
	if err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	context := runContext{
		RepoDir:      repoDir,
		RunWorkspace: workspace,
		StateDir:     workspace.StateDir(),
		EvidenceDir:  workspace.EvidenceDir(),
		Workspace:    workspace.LocalWorkspace(),
		EventsPath:   workspace.EventsPath(),
		LogPath:      workspace.LogPath(),
	}
	if err := createCleanRunDirs(context); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	cleanupState := true
	cleanupEvidence := true
	defer func() {
		if cleanupState {
			_ = cleanupRunState(repoDir, runID)
			if err != nil && cleanupEvidence {
				_ = os.RemoveAll(context.EvidenceDir)
			}
		}
	}()

	manifest := runManifest{
		Version:       evidenceSchemaVersion,
		RunID:         runID,
		Scenario:      evidenceStandaloneScenarioPrefix + kind,
		TargetProfile: host.TargetProfile,
		Host:          host.HostID,
		Runtime:       resolved.Metadata,
	}
	if configPath, err := DiscoverConfig(repoDir); err == nil {
		manifest.ConfigPath = repoRelativePath(repoDir, configPath)
	}
	if err := writeJSONFile(filepath.Join(context.StateDir, "manifest.json"), manifest); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	if err := appendEvent(context.EventsPath, "visual-evidence.started", kind); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	brokerName := sessionBrokerDisplayName(runtime.Broker)
	if err := os.WriteFile(context.LogPath, []byte(brokerName+" captured Visual Evidence\n"), 0o644); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}

	artifacts, err := capturePlannedVisualEvidence(runtime.Broker, context, plan)
	if err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	if len(artifacts) != 1 {
		return runOutcome{}, visualEvidenceArtifact{}, fmt.Errorf("Visual Evidence plan produced %d artifacts, want 1", len(artifacts))
	}
	artifact = artifacts[0]
	artifact.TargetProfile = host.TargetProfile
	artifact.Host = host.HostID
	result := ScenarioResult{Status: resultStatusPassed, Summary: brokerName + " Visual Evidence captured"}
	if err := writeJSONFile(filepath.Join(context.EvidenceDir, evidenceScenarioResultFileName), result); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	if err := appendEvent(context.EventsPath, "visual-evidence.completed", artifact.Path); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	if err := writeEvidenceBundle(context, manifest, scenarioContract{}, result, []visualEvidenceArtifact{artifact}, nil); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	cleanupEvidence = false
	if err := cleanupRunState(repoDir, runID); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	cleanupState = false
	outcome = runOutcome{
		RunID:         runID,
		Scenario:      manifest.Scenario,
		TargetProfile: host.TargetProfile,
		Host:          host.HostID,
		StateDir:      context.StateDir,
		EvidenceDir:   context.EvidenceDir,
		Result:        result,
		StopPolicy:    "stopped",
	}
	if _, err := finalizeEvidenceReport(context.EvidenceDir, reportTerminalStateForOutcome(outcome)); err != nil {
		return runOutcome{}, visualEvidenceArtifact{}, err
	}
	return outcome, artifact, nil
}

func collectScenarioVisualEvidence(broker sessionBroker, context runContext, scenarioName string, config evidenceConfig) ([]visualEvidenceArtifact, error) {
	plan, err := planScenarioVisualEvidence(scenarioName, config)
	if err != nil {
		return nil, err
	}
	return capturePlannedVisualEvidence(broker, context, plan)
}

func planStandaloneVisualEvidence(kind string) ([]visualEvidenceCapturePlan, error) {
	switch kind {
	case "screenshot":
		return []visualEvidenceCapturePlan{{Kind: "screenshot", Name: evidenceDefaultScreenshotName}}, nil
	case "recording":
		return []visualEvidenceCapturePlan{{Kind: "recording", Name: evidenceDefaultRecordingName, Duration: defaultRecordingDuration, FPS: defaultRecordingFPS}}, nil
	default:
		return nil, fmt.Errorf("unknown Visual Evidence kind %q", kind)
	}
}

func planScenarioVisualEvidence(scenarioName string, config evidenceConfig) ([]visualEvidenceCapturePlan, error) {
	var plan []visualEvidenceCapturePlan
	if config.Screenshots.Enabled {
		plan = append(plan, visualEvidenceCapturePlan{Kind: "screenshot", Name: visualEvidenceFileName(scenarioName, ".png")})
	}
	if config.Recording.Enabled {
		plan = append(plan, visualEvidenceCapturePlan{Kind: "recording", Name: visualEvidenceFileName(scenarioName, ".mp4"), Duration: defaultRecordingDuration, FPS: defaultRecordingFPS})
	}
	return plan, nil
}

func capturePlannedVisualEvidence(broker sessionBroker, context runContext, plan []visualEvidenceCapturePlan) ([]visualEvidenceArtifact, error) {
	artifacts := make([]visualEvidenceArtifact, 0, len(plan))
	for _, capture := range plan {
		switch capture.Kind {
		case "screenshot":
			capturer, ok := broker.(screenshotCapturer)
			if !ok {
				return artifacts, fmt.Errorf("session broker does not support screenshot capture")
			}
			artifact, err := capturer.CaptureScreenshot(context, screenshotRequest{Name: capture.Name})
			if err != nil {
				return artifacts, err
			}
			artifacts = append(artifacts, artifact)
		case "recording":
			capturer, ok := broker.(recordingCapturer)
			if !ok {
				return artifacts, fmt.Errorf("session broker does not support recording capture")
			}
			artifact, err := capturer.CaptureRecording(context, recordingRequest{Name: capture.Name, Duration: capture.Duration, FPS: capture.FPS})
			if err != nil {
				return artifacts, err
			}
			artifact.DurationSeconds = capture.Duration.Seconds()
			artifact.FPS = capture.FPS
			artifacts = append(artifacts, artifact)
		default:
			return artifacts, fmt.Errorf("unknown Visual Evidence kind %q", capture.Kind)
		}
	}
	return artifacts, nil
}

func visualEvidenceEventDetails(kind string, name string) (relative string, requestedEvent string, capturedEvent string) {
	name = filepath.Base(filepath.ToSlash(name))
	if name == "." || name == ".." || name == "" {
		switch kind {
		case "recording":
			name = evidenceDefaultRecordingName
		default:
			name = evidenceDefaultScreenshotName
		}
	}
	dir := evidenceScreenshotsDir
	requestedEvent = "broker.screenshot.capture-requested"
	capturedEvent = "broker.screenshot.captured"
	if kind == "recording" {
		dir = evidenceRecordingsDir
		requestedEvent = "broker.recording.capture-requested"
		capturedEvent = "broker.recording.captured"
	}
	relative = filepath.ToSlash(filepath.Join(dir, name))
	return relative, requestedEvent, capturedEvent
}

func appendVisualEvidenceCaptureRequested(context runContext, kind string, origin string, name string) error {
	relative, requestedEvent, _ := visualEvidenceEventDetails(kind, name)
	return appendEventRecord(context.EventsPath, map[string]string{
		"event":  requestedEvent,
		"detail": relative,
		"origin": origin,
	})
}

func registerVisualEvidenceBytes(context runContext, kind string, origin string, name string, mediaType string, content []byte) (visualEvidenceArtifact, error) {
	relative, _, capturedEvent := visualEvidenceEventDetails(kind, name)
	path := filepath.Join(context.EvidenceDir, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return visualEvidenceArtifact{}, err
	}
	if err := rejectExistingFileLeaf(path); err != nil {
		return visualEvidenceArtifact{}, err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return visualEvidenceArtifact{}, err
	}
	hash := sha256HexOfBytes(content)
	if err := appendEventRecord(context.EventsPath, map[string]string{
		"event":  capturedEvent,
		"detail": relative,
		"origin": origin,
		"sha256": hash,
	}); err != nil {
		return visualEvidenceArtifact{}, err
	}
	return visualEvidenceArtifact{Kind: kind, Path: relative, MediaType: mediaType, Origin: origin, SHA256: hash}, nil
}

func annotateVisualEvidenceTarget(artifacts []visualEvidenceArtifact, targetProfile string, host string) {
	for index := range artifacts {
		artifacts[index].TargetProfile = targetProfile
		artifacts[index].Host = host
	}
}

func visualEvidenceFileName(name string, extension string) string {
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	clean := strings.Trim(builder.String(), ".-")
	if clean == "" {
		clean = "scenario"
	}
	return clean + extension
}

func forceVisualEvidenceExtension(name string, extension string) string {
	base := strings.TrimSuffix(filepath.Base(filepath.ToSlash(name)), filepath.Ext(name))
	base = strings.Trim(base, ".-")
	if base == "" || base == ".." {
		switch extension {
		case ".mp4":
			return evidenceDefaultRecordingName
		default:
			return evidenceDefaultScreenshotName
		}
	}
	return base + extension
}
