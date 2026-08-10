package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type localWindowsHost struct {
	host mayaHostConfig
}

func (host localWindowsHost) ValidateTransportConfig() error {
	return validateLocalSessiondConfig(host.host)
}

func (host localWindowsHost) ProbeTransport(time.Duration) error {
	if runtime.GOOS != "windows" {
		return fmt.Errorf("local Windows Maya Host transport requires Windows")
	}
	return validateLocalSessiondConfig(host.host)
}

func (host localWindowsHost) StagePayload(context runContext, payload []manifestPayload) error {
	if err := host.ProbeTransport(0); err != nil {
		return err
	}
	if err := validatePayloadSnapshotForStage(context, payload); err != nil {
		return err
	}
	runRoot, err := localWindowsOwnedRunRoot(host.host, context.RunWorkspace.RunID())
	if err != nil {
		return err
	}
	if err := rejectLocalWindowsReparseAncestors(runRoot); err != nil {
		return err
	}
	if err := validateLocalWindowsPreStageRunRoot(runRoot); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.FromSlash(context.RunWorkspace.RemoteWorkspace()), 0o755); err != nil {
		return err
	}
	for _, item := range payload {
		destination := filepath.FromSlash(context.RunWorkspace.RemotePayloadPath(item))
		if err := requireLocalWindowsPathWithin(runRoot, destination); err != nil {
			return err
		}
		if err := copyPath(context.RunWorkspace.LocalPayloadPath(item), destination); err != nil {
			return fmt.Errorf("stage %s payload %q locally: %w", item.Kind, item.stageLabel(), err)
		}
	}
	return nil
}

func validateLocalWindowsPreStageRunRoot(runRoot string) error {
	info, err := os.Lstat(runRoot)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
		return fmt.Errorf("local Windows Run workspace is not a safe directory")
	}
	allowed := map[string]bool{
		".":                                    true,
		"workspace":                            true,
		"workspace/.maya-stall-maya-build.py":  true,
		"workspace/.maya-stall-maya-build.txt": true,
	}
	return filepath.WalkDir(runRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(runRoot, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if !allowed[relative] {
			return fmt.Errorf("local Windows Run workspace contains unexpected pre-stage path %q", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
			return fmt.Errorf("local Windows Run workspace must not use a symlink or reparse point")
		}
		return nil
	})
}

func (host localWindowsHost) CollectArtifacts(context runContext, scenario scenarioContract) error {
	return host.collectArtifacts(context, scenario, false)
}

func (host localWindowsHost) CollectFailureArtifacts(context runContext, scenario scenarioContract) error {
	return host.collectArtifacts(context, scenario, true)
}

func (host localWindowsHost) collectArtifacts(context runContext, scenario scenarioContract, optional bool) error {
	runRoot, err := localWindowsOwnedRunRoot(host.host, context.RunWorkspace.RunID())
	if err != nil {
		return err
	}
	if err := rejectLocalWindowsReparseAncestors(runRoot); err != nil {
		return err
	}
	seen := make(map[string]bool)
	for _, output := range scenario.Outputs {
		if output.Path == "" || seen[output.Path] {
			continue
		}
		seen[output.Path] = true
		if _, err := cleanRepoRelativePath(output.Path); err != nil {
			return err
		}
		source := filepath.FromSlash(context.RunWorkspace.RemoteOutputPath(output.Path))
		if err := requireLocalWindowsPathWithin(runRoot, source); err != nil {
			return err
		}
		if _, err := os.Lstat(source); err != nil {
			if os.IsNotExist(err) && (optional || output.Optional) {
				continue
			}
			return err
		}
		destination := filepath.Join(context.Workspace, filepath.FromSlash(output.Path))
		if err := ensureWorkspacePathHasNoSymlinkAncestor(context.Workspace, output.Path); err != nil {
			return err
		}
		if err := copyPath(source, destination); err != nil {
			return fmt.Errorf("collect declared output %q locally: %w", output.Path, err)
		}
	}
	return nil
}

func localWindowsOwnedRunRoot(host mayaHostConfig, runID string) (string, error) {
	if err := validateRunID(runID); err != nil {
		return "", err
	}
	runsRoot := filepath.Join(filepath.FromSlash(remotePath(host.WorkRoot)), "runs")
	runRoot := filepath.Join(runsRoot, runID)
	if err := requireLocalWindowsPathWithin(runsRoot, runRoot); err != nil {
		return "", err
	}
	return runRoot, nil
}

func requireLocalWindowsPathWithin(root string, candidate string) error {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(root, candidate)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("local Windows path %q is outside owned root", candidate)
	}
	return nil
}

func rejectLocalWindowsReparseAncestors(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	current := volume + string(filepath.Separator)
	for _, part := range strings.FieldsFunc(strings.TrimPrefix(absolute, volume), func(character rune) bool {
		return character == '/' || character == '\\'
	}) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
			return fmt.Errorf("local Windows runtime path must not use a symlink or reparse point")
		}
	}
	return nil
}

func validateLocalSessiondConfig(host mayaHostConfig) error {
	if !host.usesLocalWindows() {
		return fmt.Errorf("local gg_mayasessiond requires transport: local")
	}
	if host.usesRealSSH() {
		return fmt.Errorf("local Windows Maya Host must not configure SSH")
	}
	if strings.TrimSpace(host.WorkRoot) == "" {
		return fmt.Errorf("local Windows Maya Host requires workRoot")
	}
	for _, field := range []struct{ label, value string }{
		{"workRoot", host.WorkRoot},
		{"broker.stateDir", host.Broker.StateDir},
		{"broker.python", host.Broker.Python},
		{"broker.repo", host.Broker.Repo},
		{"broker.mcpSource", host.Broker.MCPSource},
		{"broker.mayaExe", host.Broker.MayaExe},
	} {
		label, value := field.label, field.value
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("local gg_mayasessiond requires %s", label)
		}
		_, _, absolute, traversesRoot := canonicalWindowsPathForComparison(value)
		if !absolute || traversesRoot || hasWindowsDevicePrefix(value) {
			return fmt.Errorf("local gg_mayasessiond %s must be an absolute non-device Windows path", label)
		}
	}
	if host.Broker.Port < 1 || host.Broker.Port > 65535 {
		return fmt.Errorf("local gg_mayasessiond requires broker.port between 1 and 65535")
	}
	if strings.TrimSpace(host.Broker.RecoveryTask) != "" {
		return fmt.Errorf("local gg_mayasessiond does not use broker.recoveryTask")
	}
	if trustedPluginArtifactsRoot(host) != "" {
		return fmt.Errorf("local gg_mayasessiond does not support trustedPluginArtifactsRoot")
	}
	if err := validateTrustedPluginArtifactsRoot(host); err != nil {
		return err
	}
	return nil
}
