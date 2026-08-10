package cli

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type repoRunConfig struct {
	Version   int                       `yaml:"version"`
	RunLedger runLedgerConfig           `yaml:"runLedger"`
	Scenarios map[string]scenarioConfig `yaml:"scenarios"`
}

type scenarioConfig struct {
	Description       string               `yaml:"description"`
	MayaVersion       string               `yaml:"mayaVersion"`
	Requirements      scenarioRequirements `yaml:"requirements"`
	SelectedMayaBuild string               `yaml:"-"`
	Payload           runPayload           `yaml:"payload"`
	ExpectedOutputs   expectedOutputs      `yaml:"expectedOutputs"`
	Evidence          evidenceConfig       `yaml:"evidence"`
	Validators        []validatorConfig    `yaml:"validators"`
}

type runPayload struct {
	MayaScripts     []string                           `yaml:"mayaScripts"`
	Scripts         []string                           `yaml:"scripts"`
	Scenes          []string                           `yaml:"scenes"`
	PluginArtifacts []string                           `yaml:"pluginArtifacts"`
	ExpectedOutputs []string                           `yaml:"expectedOutputs"`
	IncludePaths    []string                           `yaml:"includePaths"`
	RuntimeInputs   map[string]runtimeInputDeclaration `yaml:"runtimeInputs"`
}

type runtimeInputDeclaration struct {
	Kind        string   `yaml:"kind"`
	Extensions  []string `yaml:"extensions"`
	Destination string   `yaml:"destination"`
}

type expectedOutputs struct {
	Files          []string `yaml:"files"`
	ScenarioResult string   `yaml:"scenarioResult"`
}

type evidenceConfig struct {
	Screenshots evidenceToggle `yaml:"screenshots"`
	Recording   evidenceToggle `yaml:"recording"`
}

type evidenceToggle struct {
	Enabled bool `yaml:"enabled"`
}

type validatorConfig struct {
	Type      string  `yaml:"type"`
	Status    string  `yaml:"status"`
	Path      string  `yaml:"path"`
	JSONPath  string  `yaml:"jsonPath"`
	Equals    any     `yaml:"equals"`
	Tolerance float64 `yaml:"tolerance"`
	SHA256    string  `yaml:"sha256"`
	Required  *bool   `yaml:"required"`
}

func loadRepoRunConfig(dir string) (repoRunConfig, string, error) {
	path, err := DiscoverConfig(dir)
	if err != nil {
		return repoRunConfig{}, "", err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return repoRunConfig{}, "", err
	}
	var config repoRunConfig
	if err := decodeKnownYAMLFields(content, &config); err != nil {
		return repoRunConfig{}, "", fmt.Errorf("parse %s: %w", path, err)
	}
	if config.Version != 1 {
		return repoRunConfig{}, "", fmt.Errorf("unsupported repo config version %d", config.Version)
	}
	if len(config.Scenarios) == 0 {
		return repoRunConfig{}, "", fmt.Errorf("repo config has no Scenarios")
	}
	return config, path, nil
}

func decodeKnownYAMLFields(content []byte, out any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	return decoder.Decode(out)
}
