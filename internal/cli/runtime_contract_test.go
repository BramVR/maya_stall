package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveRuntimeForHostAllowsOnlySupportedProfiles(t *testing.T) {
	tests := []struct {
		name              string
		host              mayaHostConfig
		wantProfile       string
		wantHostAdapter   string
		wantBrokerAdapter string
		wantProofEligible bool
		wantErr           string
	}{
		{
			name:              "fake local",
			host:              mayaHostConfig{ID: "fake-local"},
			wantProfile:       "fake-local",
			wantHostAdapter:   "fake",
			wantBrokerAdapter: "fake",
			wantProofEligible: false,
		},
		{
			name: "ssh sessiond",
			host: mayaHostConfig{
				ID:        "alpha",
				Transport: "ssh",
				SSH:       sshConfig{Host: "maya-win-01"},
				WorkRoot:  "C:/maya-stall",
				Broker: brokerConfig{
					Structured: true,
					Type:       "gg-mayasessiond",
					StateDir:   "C:/maya-stall/sessiond-ui",
					Python:     "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
					Repo:       "C:/maya-stall/tools/GG_MayaSessiond",
				},
			},
			wantProfile:       "ssh-sessiond",
			wantHostAdapter:   "ssh",
			wantBrokerAdapter: "gg-mayasessiond",
			wantProofEligible: true,
		},
		{
			name: "local Windows sessiond",
			host: mayaHostConfig{
				ID:        "local-workstation",
				Transport: "local",
				WorkRoot:  `C:\maya-stall-local`,
				Broker: brokerConfig{
					Structured: true,
					Type:       "gg-mayasessiond",
					StateDir:   `C:\maya-stall-local\sessiond`,
					Python:     `C:\Python311\python.exe`,
					Repo:       `C:\src\GG_MayaSessiond`,
					MCPSource:  `C:\src\GG_MayaMCP`,
					MayaExe:    `C:\Program Files\Autodesk\Maya2025\bin\maya.exe`,
					Port:       7123,
				},
			},
			wantProfile:       "local-sessiond",
			wantHostAdapter:   "local-windows",
			wantBrokerAdapter: "gg-mayasessiond",
			wantProofEligible: true,
		},
		{
			name:    "ssh without broker",
			host:    mayaHostConfig{ID: "alpha", Transport: "ssh", SSH: sshConfig{Host: "maya-win-01"}, WorkRoot: "C:/maya-stall"},
			wantErr: "SSH Maya Host requires broker.type: gg-mayasessiond",
		},
		{
			name: "fake with real broker",
			host: mayaHostConfig{ID: "fake-local", Broker: brokerConfig{
				Structured: true,
				Type:       "gg-mayasessiond",
				StateDir:   "C:/maya-stall/sessiond-ui",
				Python:     "C:/maya-stall/sessiond-venv311/Scripts/python.exe",
				Repo:       "C:/maya-stall/tools/GG_MayaSessiond",
			}},
			wantErr: "fake Maya Host cannot use gg_mayasessiond Session Broker",
		},
		{
			name:    "ssh scalar fake broker",
			host:    mayaHostConfig{ID: "alpha", Transport: "ssh", SSH: sshConfig{Host: "maya-win-01"}, WorkRoot: "C:/maya-stall", Broker: brokerConfig{FakeStatus: "ok"}},
			wantErr: "SSH Maya Host requires broker.type: gg-mayasessiond",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime, err := resolveRuntimeForHost(tt.host)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("resolveRuntimeForHost error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRuntimeForHost returned error: %v", err)
			}
			if runtime.Metadata.Profile != tt.wantProfile || runtime.Metadata.HostAdapter != tt.wantHostAdapter || runtime.Metadata.BrokerAdapter != tt.wantBrokerAdapter || runtime.Metadata.LiveProofEligible != tt.wantProofEligible {
				t.Fatalf("resolved runtime metadata = %+v", runtime.Metadata)
			}
		})
	}
}

func TestRunScenarioEvidenceRecordsResolvedRuntime(t *testing.T) {
	dir := writeRunConfigFixture(t)
	var stdout, stderr bytes.Buffer

	code := Run([]string{"run", "--stop-after", "never", "smoke"}, &stdout, &stderr, dir, "test-version")
	if code != 0 {
		t.Fatalf("run exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	evidence := onlyRunDir(t, filepath.Join(dir, "artifacts", "maya-stall"))
	bundle := readEvidenceBundle(t, evidence)
	if bundle.Runtime.Profile != "fake-local" || bundle.Runtime.HostAdapter != "fake" || bundle.Runtime.BrokerAdapter != "fake" || bundle.Runtime.LiveProofEligible {
		t.Fatalf("Evidence Bundle runtime = %+v", bundle.Runtime)
	}
}

func TestRunScenarioRejectsInjectedFakeBrokerForSSHSessiond(t *testing.T) {
	dir := writeRunConfigFixture(t)
	hostConfigPath := filepath.Join(dir, "ci-hosts.yaml")
	sshBinary := filepath.Join(dir, "fake-ssh-runtime-contract")
	mustWriteFile(t, hostConfigPath, `version: 1
targetProfiles:
  ci:
    hostPool: windows-maya
hostPools:
  windows-maya:
    hosts:
      - id: alpha
        transport: ssh
        ssh:
          host: maya-win-01
          binary: `+sshBinary+`
        workRoot: C:/maya-stall
        broker:
          type: gg-mayasessiond
          stateDir: C:/maya-stall/sessiond-ui
          python: C:/maya-stall/sessiond-venv311/Scripts/python.exe
          repo: C:/maya-stall/tools/GG_MayaSessiond
`)
	runtimeConfig := defaultRunRuntime()
	runtimeConfig.Broker = fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "fake broker result"}}
	var stdout, stderr bytes.Buffer

	code := RunWithRuntime([]string{"run", "--host-config", hostConfigPath, "--target-profile", "ci", "smoke"}, &stdout, &stderr, dir, "test-version", runtimeConfig)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "ssh-sessiond runtime requires gg_mayasessiond Session Broker adapter") {
		t.Fatalf("run error did not reject injected fake broker: %s", stderr.String())
	}
}
