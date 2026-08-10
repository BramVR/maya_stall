package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var forbiddenRuntimeInputExtensions = map[string]bool{
	".bat": true, ".cmd": true, ".com": true, ".dll": true, ".exe": true,
	".js": true, ".kdbx": true, ".key": true, ".mel": true, ".mll": true,
	".p12": true, ".pem": true, ".pfx": true, ".ppk": true, ".ps1": true,
	".py": true, ".sh": true, ".so": true, ".vbs": true,
}

func forbiddenRuntimeInputExtension(extension string) bool {
	return forbiddenRuntimeInputExtensions[strings.ToLower(strings.TrimSpace(extension))]
}

func snapshotDeclaredRuntimeInputs(context runContext, declarations map[string]runtimeInputDeclaration, bindings map[string]string, beforeCopy func(string, string)) ([]manifestPayload, error) {
	planned, issues := inspectPlanRuntimeInputs(declarations, bindings)
	if len(issues) > 0 {
		return nil, fmt.Errorf("%s", issues[0].Reason)
	}
	manifest := make([]manifestPayload, 0, len(planned))
	for _, entry := range planned {
		source := bindings[entry.Name]
		if beforeCopy != nil {
			beforeCopy(entry.Name, source)
		}
		if err := validateRuntimeInputFile(source, declarations[entry.Name]); err != nil {
			return nil, fmt.Errorf("%s", stableRuntimeInputError(entry.Name, err))
		}
		item := manifestPayload{
			Name:   entry.Name,
			Kind:   entry.Kind,
			Staged: filepath.FromSlash(entry.Destination),
			Size:   entry.Size,
			SHA256: entry.SHA256,
		}
		destination := context.RunWorkspace.LocalPayloadPath(item)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return nil, err
		}
		if err := copyFile(source, destination); err != nil {
			return nil, fmt.Errorf("runtime input %q could not be snapshotted", entry.Name)
		}
		snapshotSize, snapshotHash, err := summarizePlanPayload(destination)
		if err != nil {
			return nil, fmt.Errorf("runtime input %q snapshot could not be verified", entry.Name)
		}
		sourceSize, sourceHash, err := summarizePlanPayload(source)
		if err != nil {
			return nil, fmt.Errorf("runtime input %q could not be verified after staging", entry.Name)
		}
		if snapshotSize != entry.Size || snapshotHash != entry.SHA256 || sourceSize != entry.Size || sourceHash != entry.SHA256 {
			return nil, fmt.Errorf("runtime input %q changed during staging", entry.Name)
		}
		manifest = append(manifest, item)
		if err := appendEvent(context.EventsPath, "runtime-input.snapshotted", entry.Name+" sha256="+entry.SHA256); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

func configureRuntimeInputEnvironment(context runContext, payload []manifestPayload, localSnapshot bool) error {
	inputs := make(map[string]string)
	for _, item := range payload {
		if !strings.HasPrefix(item.Kind, "runtimeInput:") {
			continue
		}
		path := context.RunWorkspace.RemotePayloadPath(item)
		if localSnapshot {
			path = context.RunWorkspace.LocalPayloadPath(item)
		}
		inputs[item.Name] = path
	}
	if len(inputs) == 0 {
		delete(context.Environment, runtimeInputsEnvVar)
		return nil
	}
	encoded, err := json.Marshal(inputs)
	if err != nil {
		return err
	}
	context.Environment[runtimeInputsEnvVar] = string(encoded)
	return nil
}

func validateRuntimeInputName(name string) error {
	if name == "" || !hostIDPattern.MatchString(name) {
		return fmt.Errorf("runtime input name %q must contain only letters, numbers, dots, underscores, or dashes", name)
	}
	return nil
}

func parseRuntimeInputBinding(value string) (string, string, error) {
	name, source, ok := strings.Cut(value, "=")
	name = strings.TrimSpace(name)
	source = strings.TrimSpace(source)
	if !ok || source == "" {
		return "", "", newUsageError("--input needs name=absolute-file")
	}
	if err := validateRuntimeInputName(name); err != nil {
		return "", "", newUsageError("%v", err)
	}
	if !filepath.IsAbs(source) {
		return "", "", newUsageError("runtime input %q must bind an absolute file path", name)
	}
	return name, filepath.Clean(source), nil
}

func inspectPlanRuntimeInputs(declarations map[string]runtimeInputDeclaration, bindings map[string]string) ([]planPayload, []planIssue) {
	names := make([]string, 0, len(declarations))
	for name := range declarations {
		names = append(names, name)
	}
	sort.Strings(names)
	payload := make([]planPayload, 0, len(names))
	issues := make([]planIssue, 0)
	for _, name := range names {
		declaration := declarations[name]
		entry := planPayload{
			Name:        name,
			Kind:        "runtimeInput:" + declaration.Kind,
			Destination: filepath.ToSlash(filepath.Join("payload", "runtimeInputs", declaration.Destination)),
			Status:      "ready",
		}
		source, found := bindings[name]
		if !found {
			entry.Status = "missing"
			payload = append(payload, entry)
			issues = append(issues, planIssue{Source: name, Reason: fmt.Sprintf("runtime input %q is required; bind it with --input %s=ABSOLUTE_FILE", name, name)})
			continue
		}
		if err := validateRuntimeInputFile(source, declaration); err != nil {
			entry.Status = "invalid"
			if os.IsNotExist(err) {
				entry.Status = "missing"
			}
			payload = append(payload, entry)
			issues = append(issues, planIssue{Source: name, Reason: stableRuntimeInputError(name, err)})
			continue
		}
		size, hash, err := summarizePlanPayload(source)
		if err != nil {
			entry.Status = "invalid"
			payload = append(payload, entry)
			issues = append(issues, planIssue{Source: name, Reason: stableRuntimeInputError(name, err)})
			continue
		}
		entry.Size = size
		entry.SHA256 = hash
		payload = append(payload, entry)
	}
	undeclared := make([]string, 0)
	for name := range bindings {
		if _, found := declarations[name]; !found {
			undeclared = append(undeclared, name)
		}
	}
	sort.Strings(undeclared)
	for _, name := range undeclared {
		issues = append(issues, planIssue{Source: name, Reason: fmt.Sprintf("runtime input %q is not declared by the Scenario", name)})
	}
	return payload, issues
}

func validateRuntimeInputFile(source string, declaration runtimeInputDeclaration) error {
	if err := validateRuntimeInputPath(source); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("must be a regular file and not a symlink or directory")
	}
	extension := strings.ToLower(filepath.Ext(source))
	for _, allowed := range declaration.Extensions {
		if extension == allowed {
			return nil
		}
	}
	return fmt.Errorf("extension %q is not allowed (expected one of %s)", extension, strings.Join(declaration.Extensions, ", "))
}

func validateRuntimeInputPath(source string) error {
	abs, err := filepath.Abs(source)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	remainder := strings.TrimPrefix(abs, volume)
	current := volume + string(filepath.Separator)
	for _, part := range strings.FieldsFunc(remainder, func(r rune) bool { return r == '/' || r == '\\' }) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
			return fmt.Errorf("must not use a symlink or reparse point")
		}
	}
	return nil
}

func stableRuntimeInputError(name string, err error) string {
	if os.IsNotExist(err) {
		return fmt.Sprintf("runtime input %q file does not exist", name)
	}
	if os.IsPermission(err) {
		return fmt.Sprintf("runtime input %q file cannot be read", name)
	}
	message := err.Error()
	for _, stable := range []string{
		"must be a regular file and not a symlink or directory",
		"must not use a symlink or reparse point",
	} {
		if strings.Contains(message, stable) {
			return fmt.Sprintf("runtime input %q %s", name, stable)
		}
	}
	if strings.HasPrefix(message, "extension ") {
		return fmt.Sprintf("runtime input %q %s", name, message)
	}
	return fmt.Sprintf("runtime input %q file could not be inspected", name)
}
