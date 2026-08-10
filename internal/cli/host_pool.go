package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultFakeHostID = "fake-local"

var hostIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
var errHostLockOwnershipChanged = errors.New("Host Lock ownership changed") //nolint:staticcheck // Host Lock is a product term.
var fakeSSHHostSideLockDirHook func(mayaHostConfig) (string, bool)

type hostRuntime struct {
	TargetProfile string
	HostID        string
	Config        mayaHostConfig
	release       func() error
	renew         func() error
	markActive    func(string) error
	markKept      func(string) error
	bindSession   func(string) error
}

type hostValidationError struct {
	err          error
	cleanupState string
}

func (err *hostValidationError) Error() string { return err.err.Error() }
func (err *hostValidationError) Unwrap() error { return err.err }

type runHostLock struct {
	release     func() error
	renew       func() error
	markActive  func(string) error
	markKept    func(string) error
	bindSession func(string) error
}

type hostLockOwner struct {
	ClientMachine  string
	ClientPid      string
	LockToken      string
	ActiveRun      string
	KeptRun        string
	CreatedAt      string
	LeaseExpiresAt string
	BrokerStateDir string
	BrokerPython   string
	BrokerRepo     string
	BrokerSession  string
	Authoritative  bool
	LeaseExpired   bool
	HostClockLease bool
}

type hostSideLock struct {
	mu           sync.Mutex
	expected     string
	replaceOwner func(string, string) error
	remove       func(string) error
}

func (lock *hostSideLock) release() error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	return lock.remove(lock.expected)
}

func (lock *hostSideLock) renew(hostID string) error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	owner := parseHostLockOwner(lock.expected)
	if owner.KeptRun != "" {
		return nil
	}
	content := hostSideLockOwnerContent(hostID, owner, owner.ActiveRun, "", true)
	if err := lock.replaceOwner(lock.expected, content); err != nil {
		return err
	}
	lock.expected = content
	return nil
}

func (lock *hostSideLock) markActive(hostID string, runID string) error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	owner := parseHostLockOwner(lock.expected)
	content := hostSideLockOwnerContent(hostID, owner, runID, "", true)
	if err := lock.replaceOwner(lock.expected, content); err != nil {
		return err
	}
	lock.expected = content
	return nil
}

func (lock *hostSideLock) markKept(hostID string, runID string) error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	owner := parseHostLockOwner(lock.expected)
	content := hostSideLockOwnerContent(hostID, owner, "", runID, false)
	if err := lock.replaceOwner(lock.expected, content); err != nil {
		return err
	}
	lock.expected = content
	return nil
}

func (lock *hostSideLock) bindSession(hostID string, sessionID string) error {
	lock.mu.Lock()
	defer lock.mu.Unlock()
	if strings.TrimSpace(sessionID) == "" || strings.ContainsAny(sessionID, "\x00\r\n") {
		return fmt.Errorf("broker session id is invalid")
	}
	owner := parseHostLockOwner(lock.expected)
	owner.BrokerSession = sessionID
	content := hostSideLockOwnerContent(hostID, owner, owner.ActiveRun, owner.KeptRun, owner.KeptRun == "")
	if err := lock.replaceOwner(lock.expected, content); err != nil {
		return err
	}
	lock.expected = content
	return nil
}

type resolvedRuntime struct {
	Host     runHost
	Broker   sessionBroker
	Metadata runtimeMetadata
}

type runtimeMetadata struct {
	Profile            string `json:"profile"`
	HostAdapter        string `json:"hostAdapter"`
	BrokerAdapter      string `json:"brokerAdapter"`
	BrokerConfigSource string `json:"brokerConfigSource"`
	LiveProofEligible  bool   `json:"liveProofEligible"`
}

type userHostConfig struct {
	Version        int                            `yaml:"version"`
	TargetProfiles map[string]targetProfileConfig `yaml:"targetProfiles"`
	HostPools      map[string]hostPoolConfig      `yaml:"hostPools"`
}

type targetProfileConfig struct {
	HostPool string `yaml:"hostPool"`
}

type hostPoolConfig struct {
	Hosts []mayaHostConfig `yaml:"hosts"`
}

type mayaHostConfig struct {
	ID                         string                `yaml:"id"`
	Health                     string                `yaml:"health"`
	Transport                  string                `yaml:"transport"`
	SSH                        sshConfig             `yaml:"ssh"`
	WorkRoot                   string                `yaml:"workRoot"`
	TrustedPluginArtifactsRoot string                `yaml:"trustedPluginArtifactsRoot"`
	Broker                     brokerConfig          `yaml:"broker"`
	MayaVersions               []string              `yaml:"mayaVersions"`
	FakeInstalledMayaVersions  []string              `yaml:"fakeInstalledMayaVersions"`
	VisualEvidence             *bool                 `yaml:"visualEvidence"`
	Capabilities               *mayaHostCapabilities `yaml:"capabilities"`
}

type brokerConfig struct {
	FakeStatus   string `yaml:"-"`
	Structured   bool   `yaml:"-"`
	Type         string `yaml:"type"`
	StateDir     string `yaml:"stateDir"`
	Python       string `yaml:"python"`
	Repo         string `yaml:"repo"`
	MCPSource    string `yaml:"mcpSource"`
	MCPPython    string `yaml:"mcpPython"`
	MayaExe      string `yaml:"mayaExe"`
	Port         int    `yaml:"port"`
	RecoveryTask string `yaml:"recoveryTask"`
}

func (config *brokerConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*config = brokerConfig{FakeStatus: value.Value}
		return nil
	}
	if err := rejectUnknownYAMLMappingFields(value, brokerConfigYAMLFields, "cli.brokerConfig"); err != nil {
		return err
	}
	var decoded struct {
		Type         string `yaml:"type"`
		StateDir     string `yaml:"stateDir"`
		Python       string `yaml:"python"`
		Repo         string `yaml:"repo"`
		MCPSource    string `yaml:"mcpSource"`
		MCPPython    string `yaml:"mcpPython"`
		MayaExe      string `yaml:"mayaExe"`
		Port         int    `yaml:"port"`
		RecoveryTask string `yaml:"recoveryTask"`
	}
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*config = brokerConfig{
		Structured:   true,
		Type:         decoded.Type,
		StateDir:     decoded.StateDir,
		Python:       decoded.Python,
		Repo:         decoded.Repo,
		MCPSource:    decoded.MCPSource,
		MCPPython:    decoded.MCPPython,
		MayaExe:      decoded.MayaExe,
		Port:         decoded.Port,
		RecoveryTask: decoded.RecoveryTask,
	}
	return nil
}

func (config brokerConfig) isGGMayaSessiond() bool {
	return strings.EqualFold(strings.TrimSpace(config.Type), "gg-mayasessiond")
}

func (config brokerConfig) fakeStatus() string {
	return config.FakeStatus
}

func (config brokerConfig) isLegacyFakeStatus() bool {
	switch strings.ToLower(strings.TrimSpace(config.FakeStatus)) {
	case "", "ok", "healthy", "reachable":
		return true
	default:
		return false
	}
}

func (config brokerConfig) missingStructuredType() bool {
	return config.Structured && strings.TrimSpace(config.Type) == ""
}

func (config brokerConfig) invalidReason() string {
	if config.isGGMayaSessiond() {
		return ""
	}
	if config.missingStructuredType() {
		return "broker.type is required for structured broker config"
	}
	if strings.TrimSpace(config.Type) != "" {
		return fmt.Sprintf("unknown broker.type %q", config.Type)
	}
	if !config.isLegacyFakeStatus() {
		return fmt.Sprintf("broker status %q is not usable for runs", config.FakeStatus)
	}
	return ""
}

type sshConfig struct {
	FakeStatus   string `yaml:"-"`
	Host         string `yaml:"host"`
	User         string `yaml:"user"`
	Port         int    `yaml:"port"`
	IdentityFile string `yaml:"identityFile"`
	Binary       string `yaml:"binary"`
	SFTPBinary   string `yaml:"sftpBinary"`
	SFTPTimeout  string `yaml:"sftpTimeout"`
}

func (config *sshConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		*config = sshConfig{FakeStatus: value.Value}
		return nil
	}
	if err := rejectUnknownYAMLMappingFields(value, sshConfigYAMLFields, "cli.sshConfig"); err != nil {
		return err
	}
	var decoded struct {
		Host         string `yaml:"host"`
		User         string `yaml:"user"`
		Port         int    `yaml:"port"`
		IdentityFile string `yaml:"identityFile"`
		Binary       string `yaml:"binary"`
		SFTPBinary   string `yaml:"sftpBinary"`
		SFTPTimeout  string `yaml:"sftpTimeout"`
	}
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*config = sshConfig{
		Host:         decoded.Host,
		User:         decoded.User,
		Port:         decoded.Port,
		IdentityFile: decoded.IdentityFile,
		Binary:       decoded.Binary,
		SFTPBinary:   decoded.SFTPBinary,
		SFTPTimeout:  decoded.SFTPTimeout,
	}
	return nil
}

var brokerConfigYAMLFields = map[string]struct{}{
	"type":         {},
	"stateDir":     {},
	"python":       {},
	"repo":         {},
	"mcpSource":    {},
	"mcpPython":    {},
	"mayaExe":      {},
	"port":         {},
	"recoveryTask": {},
}

var sshConfigYAMLFields = map[string]struct{}{
	"host":         {},
	"user":         {},
	"port":         {},
	"identityFile": {},
	"binary":       {},
	"sftpBinary":   {},
	"sftpTimeout":  {},
}

func rejectUnknownYAMLMappingFields(value *yaml.Node, known map[string]struct{}, typeName string) error {
	return rejectUnknownYAMLMappingFieldsWithStack(value, known, typeName, make(map[*yaml.Node]bool), make(map[*yaml.Node]bool))
}

func rejectUnknownYAMLMappingFieldsWithStack(value *yaml.Node, known map[string]struct{}, typeName string, visiting, validated map[*yaml.Node]bool) error {
	if value.Kind != yaml.MappingNode {
		return nil
	}
	if validated[value] {
		return nil
	}
	if visiting[value] {
		return fmt.Errorf("line %d: cyclic YAML merge in type %s", value.Line, typeName)
	}
	visiting[value] = true
	defer delete(visiting, value)
	for i := 0; i < len(value.Content); i += 2 {
		key := value.Content[i]
		if isYAMLMergeKey(key) {
			if err := rejectUnknownYAMLMergeFields(value.Content[i+1], known, typeName, visiting, validated); err != nil {
				return err
			}
			continue
		}
		if _, ok := known[key.Value]; !ok {
			return fmt.Errorf("line %d: field %s not found in type %s", key.Line, key.Value, typeName)
		}
	}
	validated[value] = true
	return nil
}

func isYAMLMergeKey(key *yaml.Node) bool {
	return key.Kind == yaml.ScalarNode && key.Value == "<<" &&
		(key.Tag == "" || key.Tag == "!" || key.ShortTag() == "!!merge")
}

func rejectUnknownYAMLMergeFields(value *yaml.Node, known map[string]struct{}, typeName string, visiting, validated map[*yaml.Node]bool) error {
	if value.Kind == yaml.AliasNode {
		value = value.Alias
	}
	if value == nil {
		return nil
	}
	if value.Kind == yaml.SequenceNode {
		if validated[value] {
			return nil
		}
		if visiting[value] {
			return fmt.Errorf("line %d: cyclic YAML merge in type %s", value.Line, typeName)
		}
		visiting[value] = true
		defer delete(visiting, value)
		for _, item := range value.Content {
			if err := rejectUnknownYAMLMergeFields(item, known, typeName, visiting, validated); err != nil {
				return err
			}
		}
		validated[value] = true
		return nil
	}
	return rejectUnknownYAMLMappingFieldsWithStack(value, known, typeName, visiting, validated)
}

func selectHostForRun(repoDir string, options runOptions) (hostRuntime, error) {
	return selectHostForRunValidated(repoDir, options, nil)
}

func selectHostForRunValidated(repoDir string, options runOptions, validate func(mayaHostConfig) error) (hostRuntime, error) {
	config, err := loadUserHostConfig(options.HostConfig)
	if err != nil {
		return hostRuntime{}, err
	}
	if options.SharedFakeWorkRoot != "" {
		for poolName, pool := range config.HostPools {
			for index := range pool.Hosts {
				if !pool.Hosts[index].usesRealSSH() {
					pool.Hosts[index].WorkRoot = options.SharedFakeWorkRoot
				}
			}
			config.HostPools[poolName] = pool
		}
	}
	if options.TargetProfile == "" {
		options.TargetProfile = "default"
	}
	candidates, err := hostCandidates(config, options.TargetProfile, options.HostPin)
	if err != nil {
		return hostRuntime{}, err
	}

	deadline := time.Now().Add(options.HostLockWait)
	retryDelay := 25 * time.Millisecond
	for _, candidate := range candidates {
		if candidate.usesRealSSH() {
			retryDelay = 250 * time.Millisecond
			break
		}
	}
	for {
		var sawLocked bool
		for _, candidate := range candidates {
			if !isHealthyHost(candidate) {
				continue
			}
			if options.BeforeHostLock != nil {
				options.BeforeHostLock(candidate)
			}
			lock, locked, err := acquireRunHostLock(repoDir, candidate)
			if err != nil {
				return hostRuntime{}, err
			}
			if locked {
				sawLocked = true
				continue
			}
			if validate != nil {
				if err := validate(candidate); err != nil {
					cleanupState := "completed"
					if releaseErr := lock.release(); releaseErr != nil {
						cleanupState = "failed"
						err = errors.Join(err, releaseErr)
					}
					return hostRuntime{}, &hostValidationError{err: err, cleanupState: cleanupState}
				}
			}
			return hostRuntime{
				TargetProfile: options.TargetProfile,
				HostID:        candidate.ID,
				Config:        candidate,
				release:       lock.release,
				renew:         lock.renew,
				markActive:    lock.markActive,
				markKept:      lock.markKept,
				bindSession:   lock.bindSession,
			}, nil
		}

		if options.HostLockWait <= 0 || time.Now().After(deadline) {
			if sawLocked {
				return hostRuntime{}, fmt.Errorf("no healthy unlocked Maya Host available in Target Profile %q", options.TargetProfile)
			}
			if options.HostPin != "" {
				return hostRuntime{}, fmt.Errorf("pinned Maya Host %q is not healthy in Target Profile %q", options.HostPin, options.TargetProfile)
			}
			return hostRuntime{}, fmt.Errorf("no healthy Maya Host available in Target Profile %q", options.TargetProfile)
		}
		remaining := time.Until(deadline)
		if remaining < retryDelay {
			time.Sleep(remaining)
		} else {
			time.Sleep(retryDelay)
		}
		if retryDelay >= 250*time.Millisecond && retryDelay < 2*time.Second {
			retryDelay *= 2
			if retryDelay > 2*time.Second {
				retryDelay = 2 * time.Second
			}
		}
	}
}

func loadUserHostConfig(path string) (userHostConfig, error) {
	if path == "" {
		return userHostConfig{
			Version: 1,
			TargetProfiles: map[string]targetProfileConfig{
				"default": {HostPool: "default"},
			},
			HostPools: map[string]hostPoolConfig{
				"default": {Hosts: []mayaHostConfig{{ID: defaultFakeHostID, Health: "healthy"}}},
			},
		}, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return userHostConfig{}, err
	}
	var config userHostConfig
	if err := decodeKnownYAMLFields(content, &config); err != nil {
		return userHostConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if config.Version != 1 {
		return userHostConfig{}, fmt.Errorf("unsupported host config version %d", config.Version)
	}
	return config, nil
}

func hostCandidates(config userHostConfig, targetProfile string, hostPin string) ([]mayaHostConfig, error) {
	profile, ok := config.TargetProfiles[targetProfile]
	if !ok {
		return nil, newUsageError("unknown Target Profile %q", targetProfile)
	}
	pool, ok := config.HostPools[profile.HostPool]
	if !ok {
		return nil, fmt.Errorf("Target Profile %q references unknown Host Pool %q", targetProfile, profile.HostPool)
	}
	if len(pool.Hosts) == 0 {
		return nil, fmt.Errorf("Host Pool %q has no Maya Hosts", profile.HostPool)
	}
	for _, host := range pool.Hosts {
		if err := validateHostID(host.ID); err != nil {
			return nil, err
		}
	}
	if hostPin == "" {
		return pool.Hosts, nil
	}
	for _, host := range pool.Hosts {
		if host.ID == hostPin {
			return []mayaHostConfig{host}, nil
		}
	}
	return nil, newUsageError("pinned Maya Host %q is not in Target Profile %q", hostPin, targetProfile)
}

func validateHostID(id string) error {
	if id == "" {
		return fmt.Errorf("Maya Host id must not be empty")
	}
	if !hostIDPattern.MatchString(id) {
		return fmt.Errorf("Maya Host id %q must contain only letters, numbers, dots, underscores, or dashes", id)
	}
	return nil
}

func isHealthyHost(host mayaHostConfig) bool {
	health := strings.ToLower(strings.TrimSpace(host.Health))
	return health == "" || health == "healthy"
}

func (host mayaHostConfig) usesRealSSH() bool {
	return strings.EqualFold(strings.TrimSpace(host.Transport), "ssh") || host.SSH.Host != ""
}

func (host mayaHostConfig) usesLocalWindows() bool {
	return strings.EqualFold(strings.TrimSpace(host.Transport), "local")
}

func resolveRuntimeForHost(host mayaHostConfig) (resolvedRuntime, error) {
	if host.usesLocalWindows() {
		if !host.Broker.isGGMayaSessiond() {
			if reason := host.Broker.invalidReason(); reason != "" {
				return resolvedRuntime{}, fmt.Errorf("%s", reason)
			}
			return resolvedRuntime{}, fmt.Errorf("local Windows Maya Host requires broker.type: gg-mayasessiond")
		}
		broker := ggMayaSessiondBroker{host: host}
		if err := broker.validate(); err != nil {
			return resolvedRuntime{}, err
		}
		return resolvedRuntime{
			Host:   localWindowsHost{host: host},
			Broker: broker,
			Metadata: runtimeMetadata{
				Profile:            "local-sessiond",
				HostAdapter:        "local-windows",
				BrokerAdapter:      "gg-mayasessiond",
				BrokerConfigSource: "host config transport=local broker.type=gg-mayasessiond",
				LiveProofEligible:  true,
			},
		}, nil
	}
	if host.usesRealSSH() {
		if !host.Broker.isGGMayaSessiond() {
			if reason := host.Broker.invalidReason(); reason != "" {
				return resolvedRuntime{}, fmt.Errorf("%s", reason)
			}
			return resolvedRuntime{}, fmt.Errorf("SSH Maya Host requires broker.type: gg-mayasessiond")
		}
		broker := ggMayaSessiondBroker{host: host}
		if err := broker.validate(); err != nil {
			return resolvedRuntime{}, err
		}
		return resolvedRuntime{
			Host:   realSSHHost{host: host},
			Broker: broker,
			Metadata: runtimeMetadata{
				Profile:            "ssh-sessiond",
				HostAdapter:        "ssh",
				BrokerAdapter:      "gg-mayasessiond",
				BrokerConfigSource: "host config broker.type=gg-mayasessiond",
				LiveProofEligible:  true,
			},
		}, nil
	}
	if transport := strings.TrimSpace(host.Transport); transport != "" {
		return resolvedRuntime{}, fmt.Errorf("unknown Maya Host transport %q", transport)
	}

	if host.Broker.isGGMayaSessiond() {
		return resolvedRuntime{}, fmt.Errorf("fake Maya Host cannot use gg_mayasessiond Session Broker")
	}
	if reason := host.Broker.invalidReason(); reason != "" {
		return resolvedRuntime{}, fmt.Errorf("%s", reason)
	}
	return resolvedRuntime{
		Host:   fakeHost{SSHStatus: host.SSH.FakeStatus},
		Broker: fakeSessionBroker{Result: ScenarioResult{Status: resultStatusPassed, Summary: "fake Scenario completed"}},
		Metadata: runtimeMetadata{
			Profile:            "fake-local",
			HostAdapter:        "fake",
			BrokerAdapter:      "fake",
			BrokerConfigSource: "default fake host config",
			LiveProofEligible:  false,
		},
	}, nil
}

func acquireHostLock(repoDir string, hostID string) (func() error, bool, error) {
	if err := validateHostID(hostID); err != nil {
		return nil, false, err
	}
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return nil, false, err
	}
	lockDir := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts")
	var release func() error
	var locked bool
	err := withLocalHostSideMutex(lockDir, hostID, func() error {
		var acquireErr error
		release, locked, acquireErr = acquireHostLockAtDir(lockDir, hostID)
		return acquireErr
	})
	if err != nil && release != nil {
		cleanupErr := release()
		return nil, false, errors.Join(err, cleanupErr)
	}
	return release, locked, err
}

func acquireRunHostLock(repoDir string, host mayaHostConfig) (runHostLock, bool, error) {
	hostSideLock, locked, err := acquireHostSideLock(host)
	if err != nil || locked {
		return runHostLock{}, locked, err
	}
	localRelease, locked, err := acquireHostLock(repoDir, host.ID)
	if err != nil || locked {
		releaseErr := hostSideLock.release()
		if releaseErr != nil {
			return runHostLock{}, false, &hostValidationError{err: errors.Join(err, releaseErr), cleanupState: "failed"}
		}
		if err != nil {
			return runHostLock{}, false, &hostValidationError{err: err, cleanupState: "completed"}
		}
		return runHostLock{}, locked, nil
	}
	ownedRunID := ""
	return runHostLock{
		release: func() error {
			localReleaseErr := error(nil)
			if ownedRunID == "" {
				localReleaseErr = localRelease()
			} else {
				localReleaseErr = removeRepoHostLockForRun(repoDir, host.ID, ownedRunID)
			}
			return errors.Join(hostSideLock.release(), localReleaseErr)
		},
		renew: func() error {
			return hostSideLock.renew(host.ID)
		},
		markActive: func(runID string) error {
			if err := hostSideLock.markActive(host.ID, runID); err != nil {
				return err
			}
			if err := markHostLockActiveWithAuthority(repoDir, host.ID, runID, hasAuthoritativeHostSideLock(host)); err != nil {
				return err
			}
			ownedRunID = runID
			return nil
		},
		markKept: func(runID string) error {
			if err := hostSideLock.markKept(host.ID, runID); err != nil {
				return err
			}
			if err := markHostLockKeptWithAuthority(repoDir, host.ID, runID, hasAuthoritativeHostSideLock(host)); err != nil {
				return err
			}
			ownedRunID = runID
			return nil
		},
		bindSession: func(sessionID string) error {
			return hostSideLock.bindSession(host.ID, sessionID)
		},
	}, false, nil
}

func fakeHostSideLockDir(host mayaHostConfig) (string, bool) {
	if host.usesRealSSH() || host.WorkRoot == "" || !filepath.IsAbs(host.WorkRoot) {
		return "", false
	}
	return filepath.Join(host.WorkRoot, "state", "locks", "hosts"), true
}

func localHostSideLockID(host mayaHostConfig) string {
	if host.usesLocalWindows() {
		return "host"
	}
	return host.ID
}

func fakeSSHHostSideLockDir(host mayaHostConfig) (string, bool) {
	if fakeSSHHostSideLockDirHook == nil {
		return "", false
	}
	return fakeSSHHostSideLockDirHook(host)
}

func hasAuthoritativeHostSideLock(host mayaHostConfig) bool {
	if host.usesRealSSH() {
		return true
	}
	_, ok := fakeHostSideLockDir(host)
	return ok
}

func releaseHostSideLock(host mayaHostConfig, runID string) error {
	if host.usesRealSSH() {
		if lockDir, ok := fakeSSHHostSideLockDir(host); ok {
			return removeHostSideLockForRunAtDir(lockDir, "host", runID)
		}
		return removeRemoteHostLockForRun(host, runID)
	}
	if lockDir, ok := fakeHostSideLockDir(host); ok {
		return removeHostSideLockForRunAtDir(lockDir, localHostSideLockID(host), runID)
	}
	return nil
}

func verifyHostSideLockForRun(host mayaHostConfig, runID string) error {
	if host.usesRealSSH() {
		if lockDir, ok := fakeSSHHostSideLockDir(host); ok {
			return verifyHostSideLockForRunAtDir(lockDir, "host", runID, false, false)
		}
		return verifyRemoteHostLockForRun(host, runID)
	}
	if lockDir, ok := fakeHostSideLockDir(host); ok {
		return verifyHostSideLockForRunAtDir(lockDir, localHostSideLockID(host), runID, false, false)
	}
	return nil
}

func verifyCleanupHostSideLockForRun(host mayaHostConfig, runID string) error {
	if host.usesRealSSH() {
		if lockDir, ok := fakeSSHHostSideLockDir(host); ok {
			return verifyHostSideLockForRunAtDir(lockDir, "host", runID, false, true)
		}
		return verifyCleanupRemoteHostLockForRun(host, runID)
	}
	if lockDir, ok := fakeHostSideLockDir(host); ok {
		return verifyHostSideLockForRunAtDir(lockDir, localHostSideLockID(host), runID, false, true)
	}
	return nil
}

func verifyKeptHostSideLockForRun(host mayaHostConfig, runID string) error {
	if host.usesRealSSH() {
		if lockDir, ok := fakeSSHHostSideLockDir(host); ok {
			return verifyHostSideLockForRunAtDir(lockDir, "host", runID, true, false)
		}
		return verifyKeptRemoteHostLockForRun(host, runID)
	}
	if lockDir, ok := fakeHostSideLockDir(host); ok {
		return verifyHostSideLockForRunAtDir(lockDir, localHostSideLockID(host), runID, true, false)
	}
	return nil
}

func verifyHostSideLockForRunAtDir(lockDir string, hostID string, runID string, requireKept bool, allowStaleActive bool) error {
	return withLocalHostSideMutex(lockDir, hostID, func() error {
		lockPath := filepath.Join(lockDir, hostID+".lock")
		content, err := os.ReadFile(lockPath)
		if err != nil {
			return err
		}
		owner := parseHostLockOwner(string(content))
		if owner.KeptRun == runID {
			return nil
		}
		if requireKept || owner.ActiveRun != runID || !allowStaleActive && isStaleHostSideOwner(owner) {
			return fmt.Errorf("%w for %s", errHostLockOwnershipChanged, hostID)
		}
		return nil
	})
}

func removeHostSideLockForRunAtDir(lockDir string, hostID string, runID string) error {
	return withLocalHostSideMutex(lockDir, hostID, func() error {
		lockPath := filepath.Join(lockDir, hostID+".lock")
		content, err := os.ReadFile(lockPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		owner := parseHostLockOwner(string(content))
		if owner.KeptRun != runID && owner.ActiveRun != runID {
			return fmt.Errorf("%w for %s", errHostLockOwnershipChanged, hostID)
		}
		return os.Remove(lockPath)
	})
}

func acquireHostSideLock(host mayaHostConfig) (*hostSideLock, bool, error) {
	content, err := hostSideLockContent(host.ID, host.Broker.StateDir, host.Broker.Python, host.Broker.Repo, "")
	if err != nil {
		return nil, false, err
	}
	if host.usesRealSSH() {
		if err := validateHostSideLockSSHConfig(host); err != nil {
			return nil, false, err
		}
		if lockDir, ok := fakeSSHHostSideLockDir(host); ok {
			return acquireLocalHostSideLock(lockDir, "host", content, isStaleHostSideLock)
		}
		return acquireRemoteHostLock(host, content)
	}
	if lockDir, ok := fakeHostSideLockDir(host); ok {
		isStale := isStaleHostSideLock
		if host.usesLocalWindows() {
			isStale = func(lockPath string) (bool, error) {
				return isRecoverableLocalSessiondHostLock(host, lockPath)
			}
		}
		return acquireLocalHostSideLock(lockDir, localHostSideLockID(host), content, isStale)
	}
	return &hostSideLock{
		replaceOwner: func(string, string) error { return nil },
		remove:       func(string) error { return nil },
	}, false, nil
}

func acquireLocalHostSideLock(lockDir string, hostID string, content string, isStale func(string) (bool, error)) (*hostSideLock, bool, error) {
	var locked bool
	err := withLocalHostSideMutex(lockDir, hostID, func() error {
		_, acquiredLocked, acquireErr := acquireHostLockAtDirWithContent(lockDir, hostID, content, isStale)
		locked = acquiredLocked
		return acquireErr
	})
	if err != nil || locked {
		return nil, locked, err
	}
	lockPath := filepath.Join(lockDir, hostID+".lock")
	return &hostSideLock{
		expected: content,
		replaceOwner: func(expected string, replacement string) error {
			return withLocalHostSideMutex(lockDir, hostID, func() error {
				current, err := os.ReadFile(lockPath)
				if err != nil {
					return err
				}
				if string(current) != expected {
					return fmt.Errorf("%w for %s", errHostLockOwnershipChanged, hostID)
				}
				return replaceHostLockOwnerAtDir(lockDir, hostID, replacement)
			})
		},
		remove: func(expected string) error {
			return withLocalHostSideMutex(lockDir, hostID, func() error {
				current, err := os.ReadFile(lockPath)
				if errors.Is(err, os.ErrNotExist) {
					return nil
				}
				if err != nil {
					return err
				}
				if string(current) != expected {
					return fmt.Errorf("%w for %s", errHostLockOwnershipChanged, hostID)
				}
				return os.Remove(lockPath)
			})
		},
	}, false, nil
}

func isRecoverableLocalSessiondHostLock(host mayaHostConfig, lockPath string) (bool, error) {
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	owner := parseHostLockOwner(string(content))
	if !isStaleHostSideOwner(owner) {
		return false, nil
	}
	result := remoteHostLockResult{
		BrokerStateDir: owner.BrokerStateDir,
		BrokerPython:   owner.BrokerPython,
		BrokerRepo:     owner.BrokerRepo,
	}
	inactive, err := remoteHostLockSessionInactive(host, result)
	if err != nil {
		return false, fmt.Errorf("verify expired local Host Lock Session Broker inactivity: %w", err)
	}
	return inactive, nil
}

func withLocalHostSideMutex(lockDir string, hostID string, action func() error) error {
	deadline := time.Now().Add(5 * time.Second)
	mutexDir := filepath.Join(lockDir, ".mutexes")
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(mutexDir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(mutexDir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("host lock mutex path %s must be a directory, not a symlink", mutexDir)
	}
	for {
		release, locked, err := acquireHostLockAtDir(mutexDir, hostID)
		if err != nil {
			return err
		}
		if !locked {
			return errors.Join(action(), release())
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("time out waiting for local Host Lock mutex for %s", hostID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func acquireHostLockAtDir(lockDir string, hostID string) (func() error, bool, error) {
	content := fmt.Sprintf("host: %s\npid: %d\n", hostID, os.Getpid())
	return acquireHostLockAtDirWithContent(lockDir, hostID, content, isStaleHostLock)
}

func acquireHostLockAtDirWithContent(lockDir string, hostID string, content string, isStale func(string) (bool, error)) (func() error, bool, error) {
	if err := os.MkdirAll(lockDir, 0o755); err != nil {
		return nil, false, err
	}
	lockPath := filepath.Join(lockDir, hostID+".lock")
	tempFile, err := os.CreateTemp(lockDir, hostID+".*.tmp")
	if err != nil {
		return nil, false, err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.WriteString(content); err != nil {
		tempFile.Close()
		return nil, false, err
	}
	if err := tempFile.Close(); err != nil {
		return nil, false, err
	}
	for {
		err := os.Link(tempPath, lockPath)
		if errors.Is(err, os.ErrExist) {
			stale, err := isStale(lockPath)
			if err != nil {
				return nil, false, err
			}
			if !stale {
				return nil, true, nil
			}
			if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, false, err
			}
			continue
		}
		if err != nil {
			return nil, false, err
		}
		return func() error {
			return os.Remove(lockPath)
		}, false, nil
	}
}

func markHostLockKept(repoDir string, hostID string, runID string) error {
	return markHostLockKeptWithAuthority(repoDir, hostID, runID, false)
}

func markHostLockKeptWithAuthority(repoDir string, hostID string, runID string, authoritative bool) error {
	if err := validateHostID(hostID); err != nil {
		return err
	}
	return replaceHostLockOwner(repoDir, hostID, fmt.Sprintf("host: %s\nkeptRun: %s\nauthoritativeHostLock: %t\n", hostID, runID, authoritative))
}

func markHostLockActive(repoDir string, hostID string, runID string) error {
	return markHostLockActiveWithAuthority(repoDir, hostID, runID, false)
}

func markHostLockActiveWithAuthority(repoDir string, hostID string, runID string, authoritative bool) error {
	if err := validateHostID(hostID); err != nil {
		return err
	}
	if err := validateRunID(runID); err != nil {
		return err
	}
	return replaceHostLockOwner(repoDir, hostID, fmt.Sprintf("host: %s\npid: %d\nactiveRun: %s\nauthoritativeHostLock: %t\n", hostID, os.Getpid(), runID, authoritative))
}

func replaceHostLockOwner(repoDir string, hostID string, content string) error {
	lockDir := filepath.Join(repoDir, ".maya-stall", "state", "locks", "hosts")
	if err := ensureOutputPathHasNoSymlinkParent(repoDir, filepath.Join(".maya-stall", "state", "locks", "hosts")); err != nil {
		return err
	}
	return withLocalHostSideMutex(lockDir, hostID, func() error {
		return replaceHostLockOwnerAtDir(lockDir, hostID, content)
	})
}

func replaceHostLockOwnerAtDir(lockDir string, hostID string, content string) error {
	lockPath := filepath.Join(lockDir, hostID+".lock")
	if info, err := os.Lstat(lockPath); err != nil {
		return err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("Host Lock %s must not be a symlink", lockPath)
	}
	tempFile, err := os.CreateTemp(lockDir, hostID+".kept.*.tmp")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	if _, err := tempFile.WriteString(content); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, lockPath)
}

func isStaleHostLock(lockPath string) (bool, error) {
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) != "pid" {
			if ok && strings.TrimSpace(key) == "keptRun" && strings.TrimSpace(value) != "" {
				return false, nil
			}
			continue
		}
		pid, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil {
			return true, nil
		}
		return !processExists(pid), nil
	}
	return true, nil
}

var hostSideLockLeaseDuration = time.Hour
var hostSideLockHeartbeatInterval = time.Minute

func hostSideLockContent(hostID string, brokerStateDir string, brokerPython string, brokerRepo string, extra string) (string, error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", fmt.Errorf("create Host Lock token: %w", err)
	}
	now := time.Now().UTC()
	machine, _ := os.Hostname()
	return fmt.Sprintf(
		"host: %s\nclientMachine: %s\nclientPid: %d\nlockToken: %s\nbrokerStateDir: %s\nbrokerPython: %s\nbrokerRepo: %s\n%screatedAt: %s\nleaseExpiresAt: %s\nleaseDurationSeconds: %d\n",
		hostID,
		strings.TrimSpace(machine),
		os.Getpid(),
		hex.EncodeToString(tokenBytes),
		strings.TrimSpace(brokerStateDir),
		strings.TrimSpace(brokerPython),
		strings.TrimSpace(brokerRepo),
		extra,
		now.Format(time.RFC3339Nano),
		now.Add(hostSideLockLeaseDuration).Format(time.RFC3339Nano),
		int(hostSideLockLeaseDuration/time.Second),
	), nil
}

func hostSideLockOwnerContent(hostID string, owner hostLockOwner, activeRun string, keptRun string, leased bool) string {
	now := time.Now().UTC()
	createdAt := owner.CreatedAt
	if createdAt == "" {
		createdAt = now.Format(time.RFC3339Nano)
	}
	content := fmt.Sprintf(
		"host: %s\nclientMachine: %s\nclientPid: %s\nlockToken: %s\nbrokerStateDir: %s\nbrokerPython: %s\nbrokerRepo: %s\ncreatedAt: %s\n",
		hostID,
		owner.ClientMachine,
		owner.ClientPid,
		owner.LockToken,
		owner.BrokerStateDir,
		owner.BrokerPython,
		owner.BrokerRepo,
		createdAt,
	)
	if owner.BrokerSession != "" {
		content += "brokerSessionId: " + owner.BrokerSession + "\n"
	}
	if activeRun != "" {
		content += "activeRun: " + activeRun + "\n"
	}
	if keptRun != "" {
		content += "keptRun: " + keptRun + "\n"
	}
	if leased {
		content += fmt.Sprintf("leaseExpiresAt: %s\nleaseDurationSeconds: %d\n", now.Add(hostSideLockLeaseDuration).Format(time.RFC3339Nano), int(hostSideLockLeaseDuration/time.Second))
	}
	return content
}

func parseHostLockOwner(content string) hostLockOwner {
	var owner hostLockOwner
	for _, line := range strings.Split(content, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "clientMachine":
			owner.ClientMachine = value
		case "clientPid":
			owner.ClientPid = value
		case "lockToken":
			owner.LockToken = value
		case "activeRun":
			owner.ActiveRun = value
		case "keptRun":
			owner.KeptRun = value
		case "createdAt":
			owner.CreatedAt = value
		case "leaseExpiresAt":
			owner.LeaseExpiresAt = value
		case "brokerStateDir":
			owner.BrokerStateDir = value
		case "brokerPython":
			owner.BrokerPython = value
		case "brokerRepo":
			owner.BrokerRepo = value
		case "brokerSessionId":
			owner.BrokerSession = value
		case "authoritativeHostLock":
			owner.Authoritative = strings.EqualFold(value, "true")
		}
	}
	return owner
}

func isStaleHostSideOwner(owner hostLockOwner) bool {
	if owner.KeptRun != "" || owner.LockToken == "" {
		return false
	}
	if owner.HostClockLease {
		return owner.LeaseExpired
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, owner.LeaseExpiresAt)
	return err == nil && !time.Now().UTC().Before(expiresAt)
}

func isStaleHostSideLock(lockPath string) (bool, error) {
	content, err := os.ReadFile(lockPath)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return isStaleHostSideOwner(parseHostLockOwner(string(content))), nil
}
