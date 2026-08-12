package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func validateHostSideLockSSHConfig(host mayaHostConfig) error {
	if reason := host.Broker.invalidReason(); reason != "" {
		return errors.New(reason)
	}
	if !host.Broker.isGGMayaSessiond() {
		return fmt.Errorf("SSH Maya Host requires broker.type: gg-mayasessiond")
	}
	if err := (ggMayaSessiondBroker{host: host}).validate(); err != nil {
		return err
	}
	return validateRealSSHConfig(host)
}

func remoteHostLockPath(host mayaHostConfig) (string, error) {
	if strings.TrimSpace(host.WorkRoot) == "" {
		return "", fmt.Errorf("workRoot is required for host-side Host Lock")
	}
	if err := rejectSFTPBatchUnsafePath(host.WorkRoot); err != nil {
		return "", fmt.Errorf("workRoot %w", err)
	}
	// One work root represents one Maya desktop resource. Keep the
	// authoritative filename independent of controller-local host aliases so
	// every checkout that targets this work root contends for the same lease.
	return remoteJoin(host.WorkRoot, "state", "locks", "host.lock"), nil
}

type remoteHostLockResult struct {
	Acquired       bool   `json:"acquired,omitempty"`
	Locked         bool   `json:"locked,omitempty"`
	State          string `json:"state,omitempty"`
	ActiveRun      string `json:"activeRun,omitempty"`
	KeptRun        string `json:"keptRun,omitempty"`
	ClientMachine  string `json:"clientMachine,omitempty"`
	ClientPid      string `json:"clientPid,omitempty"`
	CreatedAt      string `json:"createdAt,omitempty"`
	LeaseExpiresAt string `json:"leaseExpiresAt,omitempty"`
	BrokerStateDir string `json:"brokerStateDir,omitempty"`
	BrokerPython   string `json:"brokerPython,omitempty"`
	BrokerRepo     string `json:"brokerRepo,omitempty"`
	LeaseExpired   bool   `json:"leaseExpired,omitempty"`
	LeaseVersion   string `json:"leaseVersion,omitempty"`
	LockToken      string `json:"lockToken,omitempty"`
	Raw            string `json:"raw,omitempty"`
	ContentRead    bool   `json:"contentRead,omitempty"`
	Error          string `json:"error,omitempty"`
}

func (result remoteHostLockResult) owner() hostLockOwner {
	return hostLockOwner{
		ClientMachine:  result.ClientMachine,
		ClientPid:      result.ClientPid,
		KeptRun:        result.KeptRun,
		CreatedAt:      result.CreatedAt,
		LeaseExpiresAt: result.LeaseExpiresAt,
		BrokerStateDir: result.BrokerStateDir,
		BrokerPython:   result.BrokerPython,
		BrokerRepo:     result.BrokerRepo,
		LeaseExpired:   result.LeaseExpired,
		HostClockLease: true,
		LockToken:      result.LockToken,
	}
}

const maxRemoteHostLockReclaimAttempts = 2
const maxRemoteHostLockTimeoutAttempts = 2

var remoteHostLockRetryDelay = 250 * time.Millisecond

func acquireRemoteHostLock(host mayaHostConfig, content string) (*hostSideLock, bool, error) {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return nil, false, err
	}
	result, err := runRemoteHostLockScript(host, remoteHostLockAcquireScript(lockPath, content))
	for attempt := 0; ; attempt++ {
		if err != nil {
			state, stateErr := readRemoteHostLockState(host, lockPath)
			if stateErr == nil && state.Raw == content {
				return newRemoteHostSideLock(host, content), false, nil
			}
			return nil, false, errors.Join(err, stateErr)
		}
		if result.Error != "" {
			return nil, false, errors.New(result.Error)
		}
		if !result.Locked {
			return newRemoteHostSideLock(host, content), false, nil
		}
		// The staleness decision happens on the client. Expired active leases
		// can be reclaimed by any controller; kept locks remain durable. Losing the
		// reclaim race surfaces as locked, never as corruption, because the
		// reclaim script only deletes the exact stale content it observed.
		if attempt >= maxRemoteHostLockReclaimAttempts || !isStaleHostSideOwner(result.owner()) {
			return nil, true, nil
		}
		sessionInactive, statusErr := remoteHostLockSessionInactive(host, result)
		if statusErr != nil {
			return nil, true, nil
		}
		if !remoteHostLockReclaimAllowed(result, sessionInactive) {
			return nil, true, nil
		}
		result, err = runRemoteHostLockScript(host, remoteHostLockReclaimScript(lockPath, result.Raw, result.LeaseVersion, content))
	}
}

func remoteHostLockReclaimAllowed(result remoteHostLockResult, sessionInactive bool) bool {
	return result.Locked && result.State != "kept" && result.LeaseExpired && result.LeaseVersion != "" && result.LockToken != "" && result.BrokerStateDir != "" && result.BrokerPython != "" && result.BrokerRepo != "" && sessionInactive
}

func remoteHostLockSessionInactive(host mayaHostConfig, owner remoteHostLockResult) (bool, error) {
	if strings.TrimSpace(owner.BrokerStateDir) == "" || strings.TrimSpace(owner.BrokerPython) == "" || strings.TrimSpace(owner.BrokerRepo) == "" {
		return false, fmt.Errorf("host lock does not identify its complete owning Session Broker configuration")
	}
	if !remoteHostLockBrokerConfigMatches(host, owner) {
		return false, fmt.Errorf("host lock Session Broker configuration does not match trusted Maya Host configuration")
	}
	if err := validateSessionBrokerStateDir(owner.BrokerStateDir); err != nil {
		return false, fmt.Errorf("host lock Session Broker state directory: %w", err)
	}
	broker := ggMayaSessiondBroker{host: host}
	if err := broker.validate(); err != nil {
		return false, err
	}
	status, err := broker.status()
	if err != nil {
		return false, err
	}
	return sessiondStatusProvesInactive(status), nil
}

func remoteHostLockBrokerConfigMatches(host mayaHostConfig, owner remoteHostLockResult) bool {
	trustedRepo, trustedRepoOK := canonicalHostLockBrokerPath(host.Broker.Repo, "", false)
	ownerRepo, ownerRepoOK := canonicalHostLockBrokerPath(owner.BrokerRepo, "", false)
	trustedPython, trustedPythonOK := canonicalHostLockBrokerPath(host.Broker.Python, host.Broker.Repo, true)
	ownerPython, ownerPythonOK := canonicalHostLockBrokerPath(owner.BrokerPython, owner.BrokerRepo, true)
	trustedState, trustedStateOK := canonicalHostLockBrokerPath(host.Broker.StateDir, host.Broker.Repo, true)
	ownerState, ownerStateOK := canonicalHostLockBrokerPath(owner.BrokerStateDir, owner.BrokerRepo, true)
	return trustedRepoOK && ownerRepoOK && trustedPythonOK && ownerPythonOK && trustedStateOK && ownerStateOK &&
		trustedRepo == ownerRepo && trustedPython == ownerPython && trustedState == ownerState
}

func canonicalHostLockBrokerPath(value string, repo string, allowRelative bool) (string, bool) {
	clean, _, absolute, traversesRoot := canonicalWindowsPathForComparison(strings.TrimSpace(value))
	if traversesRoot {
		return "", false
	}
	if !absolute && allowRelative {
		clean, _, absolute, traversesRoot = canonicalWindowsPathForComparison(remoteJoin(repo, strings.TrimSpace(value)))
	}
	return clean, absolute && !traversesRoot
}

func sessiondStatusProvesInactive(status sessiondStatusResult) bool {
	stopped := status.HasState && strings.EqualFold(status.DerivedStatus, "stopped") && strings.EqualFold(status.State.Status, "stopped")
	if !stopped {
		return false
	}
	if status.State.MayaAlive || status.State.MCPAlive {
		return false
	}
	for _, process := range []string{"daemon", "maya", "mcp"} {
		alive, reported := status.ProcessAlive[process]
		if !reported || alive {
			return false
		}
	}
	return true
}

func newRemoteHostSideLock(host mayaHostConfig, content string) *hostSideLock {
	return &hostSideLock{
		expected: content,
		replaceOwner: func(expected string, replacement string) error {
			return replaceRemoteHostLockOwner(host, expected, replacement)
		},
		remove: func(expected string) error {
			return removeRemoteHostLock(host, expected)
		},
	}
}

func replaceRemoteHostLockOwner(host mayaHostConfig, expected string, content string) error {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return err
	}
	return replaceRemoteHostLockOwnerWithTransport(host, lockPath, expected, content, runRemoteHostLockScript, readRemoteHostLockState)
}

func replaceRemoteHostLockOwnerWithTransport(
	host mayaHostConfig,
	lockPath string,
	expected string,
	content string,
	run func(mayaHostConfig, string) (remoteHostLockResult, error),
	read func(mayaHostConfig, string) (remoteHostLockResult, error),
) error {
	for attempt := 0; ; attempt++ {
		result, err := run(host, remoteHostLockReplaceScript(lockPath, expected, content))
		if err != nil {
			state, stateErr := read(host, lockPath)
			if stateErr == nil && state.Raw == content {
				return nil
			}
			if stateErr == nil && (state.State == "unlocked" || (state.ContentRead && state.Raw != expected)) {
				return fmt.Errorf("%w on Maya Host", errHostLockOwnershipChanged)
			}
			// A remote PowerShell failure can surface as SSH exit 255 even though
			// the connection reached the Host. Retry only after an independent
			// state read proves this controller still owns the exact prior lock.
			if stateErr == nil && state.Raw == expected && attempt+1 < maxRemoteHostLockTimeoutAttempts {
				time.Sleep(remoteHostLockRetryDelay)
				continue
			}
			return errors.Join(err, stateErr)
		}
		if result.Error != "" {
			return remoteHostLockError(result.Error)
		}
		return nil
	}
}

func removeRemoteHostLock(host mayaHostConfig, expected string) error {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return err
	}
	return removeRemoteHostLockWithTransport(host, lockPath, expected, runRemoteHostLockScript, readRemoteHostLockState)
}

func removeRemoteHostLockWithTransport(
	host mayaHostConfig,
	lockPath string,
	expected string,
	run func(mayaHostConfig, string) (remoteHostLockResult, error),
	read func(mayaHostConfig, string) (remoteHostLockResult, error),
) error {
	for attempt := 0; ; attempt++ {
		result, err := run(host, remoteHostLockRemoveScript(lockPath, expected))
		if err != nil {
			state, stateErr := read(host, lockPath)
			if stateErr == nil && state.State == "unlocked" {
				return nil
			}
			if stateErr == nil && state.Raw != "" && state.Raw != expected {
				return fmt.Errorf("%w on Maya Host", errHostLockOwnershipChanged)
			}
			if stateErr == nil && state.Raw == expected && attempt+1 < maxRemoteHostLockTimeoutAttempts {
				time.Sleep(remoteHostLockRetryDelay)
				continue
			}
			return errors.Join(err, stateErr)
		}
		if result.Error != "" {
			return remoteHostLockError(result.Error)
		}
		return nil
	}
}

func isRemoteHostLockTimeout(err error) bool {
	return err != nil && strings.Contains(err.Error(), "timed out")
}

func removeRemoteHostLockForRun(host mayaHostConfig, runID string) error {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return err
	}
	result, err := runRemoteHostLockScript(host, remoteHostLockRemoveForRunScript(lockPath, runID))
	if err != nil {
		state, stateErr := readRemoteHostLockState(host, lockPath)
		if stateErr == nil && state.State == "unlocked" {
			return nil
		}
		if stateErr == nil && state.Raw != "" && state.KeptRun != runID && state.ActiveRun != runID {
			return fmt.Errorf("%w on Maya Host", errHostLockOwnershipChanged)
		}
		return errors.Join(err, stateErr)
	}
	if result.Error != "" {
		return remoteHostLockError(result.Error)
	}
	if result.State == "unlocked" {
		return nil
	}
	return errHostLockOwnershipChanged
}

func readRemoteHostLockState(host mayaHostConfig, lockPath string) (remoteHostLockResult, error) {
	return retryRemoteHostLockTimeout(func() (remoteHostLockResult, error) {
		return runRemoteHostLockScript(host, remoteHostLockStateScript(lockPath))
	})
}

func retryRemoteHostLockTimeout(action func() (remoteHostLockResult, error)) (remoteHostLockResult, error) {
	for attempt := 0; ; attempt++ {
		result, err := action()
		if err == nil || !isRemoteHostLockTimeout(err) || attempt+1 >= maxRemoteHostLockTimeoutAttempts {
			return result, err
		}
		time.Sleep(remoteHostLockRetryDelay)
	}
}

func verifyRemoteHostLockForRun(host mayaHostConfig, runID string) error {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return err
	}
	result, err := readRemoteHostLockState(host, lockPath)
	if err != nil {
		return err
	}
	if result.Error != "" {
		return remoteHostLockError(result.Error)
	}
	if result.KeptRun != runID && (result.ActiveRun != runID || result.LeaseExpired) {
		return fmt.Errorf("%w on Maya Host", errHostLockOwnershipChanged)
	}
	return nil
}

func verifyCleanupRemoteHostLockForRun(host mayaHostConfig, runID string) error {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return err
	}
	result, err := readRemoteHostLockState(host, lockPath)
	if err != nil {
		return err
	}
	if result.Error != "" {
		return remoteHostLockError(result.Error)
	}
	if result.KeptRun != runID && result.ActiveRun != runID {
		return fmt.Errorf("%w on Maya Host", errHostLockOwnershipChanged)
	}
	return nil
}

func verifyKeptRemoteHostLockForRun(host mayaHostConfig, runID string) error {
	lockPath, err := remoteHostLockPath(host)
	if err != nil {
		return err
	}
	result, err := readRemoteHostLockState(host, lockPath)
	if err != nil {
		return err
	}
	if result.Error != "" {
		return remoteHostLockError(result.Error)
	}
	if result.KeptRun != runID {
		return fmt.Errorf("%w on Maya Host", errHostLockOwnershipChanged)
	}
	return nil
}

func remoteHostLockError(message string) error {
	if strings.Contains(message, "Host Lock ownership changed") {
		return fmt.Errorf("%w on Maya Host", errHostLockOwnershipChanged)
	}
	return errors.New(message)
}

func runRemoteHostLockScript(host mayaHostConfig, script string) (remoteHostLockResult, error) {
	raw, err := runSSHCommandOutputWithInput(host, encodedPowerShellCommand(remoteHostLockStdinRunnerPS), script+"\n", sshCommandTimeout)
	if err != nil {
		return remoteHostLockResult{}, err
	}
	var result remoteHostLockResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &result); err != nil {
		return remoteHostLockResult{}, fmt.Errorf("parse host-side Host Lock JSON: %w", err)
	}
	return result, nil
}

const remoteHostLockStdinRunnerPS = `$script = [Console]::In.ReadToEnd()
& ([ScriptBlock]::Create($script))`

const hostSideLockMutexStaleAfter = 10 * time.Minute

// remoteHostLockCommonPS parses lock content into fields the client uses to
// decide staleness; the scripts never decide staleness on the Maya Host.
var remoteHostLockCommonPS = fmt.Sprintf(`function Out-Result($value) {
  $json = ($value | ConvertTo-Json -Compress) + [Environment]::NewLine
  $bytes = [Text.Encoding]::UTF8.GetBytes($json)
  $stdout = [Console]::OpenStandardOutput()
  $stdout.Write($bytes, 0, $bytes.Length)
  $stdout.Flush()
}
function Invoke-WithLockMutex($path, [scriptblock]$action) {
  [System.IO.Directory]::CreateDirectory([System.IO.Path]::GetDirectoryName($path)) | Out-Null
  $mutexPath = $path + '.mutex'
  $deadline = (Get-Date).ToUniversalTime().AddSeconds(30)
  $mutexToken = [Guid]::NewGuid().ToString('N')
  $mutex = $null
  while ($null -eq $mutex) {
    try {
      $mutex = [System.IO.File]::Open($mutexPath, [System.IO.FileMode]::CreateNew, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
      $tokenBytes = [System.Text.Encoding]::ASCII.GetBytes($mutexToken)
      $mutex.Write($tokenBytes, 0, $tokenBytes.Length)
      $mutex.Flush($true)
    } catch {
      if (!(Test-Path -LiteralPath $mutexPath)) { throw }
      try {
        $age = ((Get-Date).ToUniversalTime() - (Get-Item -LiteralPath $mutexPath -ErrorAction Stop).LastWriteTimeUtc).TotalSeconds
        if ($age -ge %d) { Remove-Item -LiteralPath $mutexPath -Force -ErrorAction Stop }
      } catch { }
      if ((Get-Date).ToUniversalTime() -ge $deadline) { throw 'timed out waiting for Host Lock mutex file' }
      Start-Sleep -Milliseconds 50
    }
  }
  try {
    & $action
  } finally {
    $mutex.Close()
    try {
      $currentToken = [string](Get-Content -LiteralPath $mutexPath -Raw -ErrorAction Stop)
      if ($currentToken -ceq $mutexToken) {
        Remove-Item -LiteralPath $mutexPath -Force -ErrorAction Stop
      }
    } catch { }
  }
}
function Lock-Fields($raw) {
  $fields = @{state='active'; raw=$raw}
  foreach ($line in ($raw -split '\r?\n')) {
    $parts = $line.Split(':', 2)
    if ($parts.Length -ne 2) { continue }
    $key = $parts[0].Trim()
    $value = $parts[1].Trim()
    if ($value -eq '') { continue }
    switch ($key) {
      'keptRun' { $fields.state = 'kept'; $fields.keptRun = $value }
      'activeRun' { $fields.activeRun = $value }
      'clientMachine' { $fields.clientMachine = $value }
      'clientPid' { $fields.clientPid = $value }
      'createdAt' { $fields.createdAt = $value }
      'brokerStateDir' { $fields.brokerStateDir = $value }
      'brokerPython' { $fields.brokerPython = $value }
      'brokerRepo' { $fields.brokerRepo = $value }
      'leaseExpiresAt' { $fields.leaseExpiresAt = $value }
      'leaseDurationSeconds' { $fields.leaseDurationSeconds = $value }
      'lockToken' { $fields.lockToken = $value }
    }
  }
  return $fields
}
function Locked-Result($path) {
  try {
    $raw = [string](Get-Content -LiteralPath $path -Raw -ErrorAction Stop)
  } catch {
    return @{locked=$true; state='active'}
  }
  $fields = Lock-Fields $raw
  $fields.contentRead = $true
  $leaseSeconds = %d
  if ($fields.leaseDurationSeconds) { $leaseSeconds = [double]$fields.leaseDurationSeconds }
  $modified = (Get-Item -LiteralPath $path -ErrorAction Stop).LastWriteTimeUtc
  $age = ((Get-Date).ToUniversalTime() - $modified).TotalSeconds
  $fields.leaseExpired = ($age -ge $leaseSeconds)
  $fields.leaseVersion = $modified.Ticks.ToString([System.Globalization.CultureInfo]::InvariantCulture)
  $fields.locked = $true
  return $fields
}
function Clear-OwnedLiveLockMutex($path, $expected) {
  $mutexPath = $path + '.mutex'
  if (!(Test-Path -LiteralPath $path) -or !(Test-Path -LiteralPath $mutexPath)) { return }
  $current = [string](Get-Content -LiteralPath $path -Raw -ErrorAction Stop)
  if ($current -cne $expected) { return }
  $fields = Lock-Fields $current
  $leaseSeconds = %d
  if ($fields.leaseDurationSeconds) { $leaseSeconds = [double]$fields.leaseDurationSeconds }
  $age = ((Get-Date).ToUniversalTime() - (Get-Item -LiteralPath $path -ErrorAction Stop).LastWriteTimeUtc).TotalSeconds
  if ($age -lt $leaseSeconds) {
    Remove-Item -LiteralPath $mutexPath -Force -ErrorAction SilentlyContinue
  }
}
function New-LockFile($path, $content) {
  $stream = [System.IO.File]::Open($path, [System.IO.FileMode]::CreateNew, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
  try {
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($content)
    $stream.Write($bytes, 0, $bytes.Length)
  } finally {
    $stream.Close()
  }
}
function Replace-LockFile($path, $content) {
  $temp = $path + '.' + [System.Guid]::NewGuid().ToString('N') + '.tmp'
  try {
    [System.IO.File]::WriteAllText($temp, $content, [System.Text.Encoding]::UTF8)
    try {
      [System.IO.File]::Replace($temp, $path, $null)
    } catch {
      Move-Item -LiteralPath $temp -Destination $path -Force -ErrorAction Stop
    }
  } finally {
    Remove-Item -LiteralPath $temp -Force -ErrorAction SilentlyContinue
  }
}`, int(hostSideLockMutexStaleAfter/time.Second), int(hostSideLockLeaseDuration/time.Second), int(hostSideLockLeaseDuration/time.Second))

func remoteHostLockAcquireScript(lockPath string, content string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$lockPath = %s
$content = %s
%s
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $lockPath) | Out-Null
$result = Invoke-WithLockMutex $lockPath {
  if (Test-Path -LiteralPath $lockPath) {
    return (Locked-Result $lockPath)
  }
  try {
    New-LockFile $lockPath $content
    return @{acquired=$true; locked=$false; state='active'}
  } catch {
    if (Test-Path -LiteralPath $lockPath) {
      return (Locked-Result $lockPath)
    }
    throw
  }
}
Out-Result $result`, powerShellSingleQuoted(lockPath), powerShellSingleQuoted(content), remoteHostLockCommonPS)
}

// remoteHostLockReclaimScript removes a lock the client judged stale and
// re-attempts the CreateNew acquire. It only deletes the lock when its
// content still exactly matches the stale content the client observed;
// any concurrent change surfaces as locked.
func remoteHostLockReclaimScript(lockPath string, expectedContent string, expectedLeaseVersion string, content string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$lockPath = %s
$expected = %s
$expectedLeaseVersion = %s
$content = %s
%s
$result = Invoke-WithLockMutex $lockPath {
  if (Test-Path -LiteralPath $lockPath) {
    try {
      $raw = [string](Get-Content -LiteralPath $lockPath -Raw -ErrorAction Stop)
    } catch {
      return @{locked=$true; state='active'}
    }
    if ($raw -cne $expected) {
      $fields = Lock-Fields $raw
      $fields.locked = $true
      return $fields
    }
    $currentLeaseVersion = (Get-Item -LiteralPath $lockPath -ErrorAction Stop).LastWriteTimeUtc.Ticks.ToString([System.Globalization.CultureInfo]::InvariantCulture)
    if ($currentLeaseVersion -cne $expectedLeaseVersion) {
      return (Locked-Result $lockPath)
    }
    try {
      Remove-Item -LiteralPath $lockPath -Force -ErrorAction Stop
    } catch {
      return (Locked-Result $lockPath)
    }
  }
  try {
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $lockPath) | Out-Null
    New-LockFile $lockPath $content
    return @{acquired=$true; locked=$false; state='active'}
  } catch {
    if (Test-Path -LiteralPath $lockPath) {
      return (Locked-Result $lockPath)
    }
    throw
  }
}
Out-Result $result`, powerShellSingleQuoted(lockPath), powerShellSingleQuoted(expectedContent), powerShellSingleQuoted(expectedLeaseVersion), powerShellSingleQuoted(content), remoteHostLockCommonPS)
}

func remoteHostLockReplaceScript(lockPath string, expected string, content string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$lockPath = %s
$expected = %s
$content = %s
%s
$result = Invoke-WithLockMutex $lockPath {
  if (!(Test-Path -LiteralPath $lockPath)) { return @{error='Host Lock missing on Maya Host'} }
  $current = [string](Get-Content -LiteralPath $lockPath -Raw -ErrorAction Stop)
  if ($current -ceq $content) { return @{state='updated'} }
  if ($current -cne $expected) { return @{error='Host Lock ownership changed on Maya Host'} }
  Replace-LockFile $lockPath $content
  return @{state='updated'}
}
Out-Result $result`, powerShellSingleQuoted(lockPath), powerShellSingleQuoted(expected), powerShellSingleQuoted(content), remoteHostLockCommonPS)
}

func remoteHostLockRemoveScript(lockPath string, expected string) string {
	// Release runs only after controller renewal has stopped and any owned Maya
	// session has settled. Recover a leftover mutex only while the authoritative
	// lock still exactly names this live lease, then reacquire the fence before
	// the compare-delete so a successor can never be removed.
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$lockPath = %s
$expected = %s
%s
Clear-OwnedLiveLockMutex $lockPath $expected
$result = Invoke-WithLockMutex $lockPath {
  if (!(Test-Path -LiteralPath $lockPath)) { return @{state='unlocked'} }
  if ([string](Get-Content -LiteralPath $lockPath -Raw -ErrorAction Stop) -cne $expected) { return @{error='Host Lock ownership changed on Maya Host'} }
  Remove-Item -LiteralPath $lockPath -Force -ErrorAction Stop
  return @{state='unlocked'}
}
Out-Result $result`, powerShellSingleQuoted(lockPath), powerShellSingleQuoted(expected), remoteHostLockCommonPS)
}

func remoteHostLockRemoveForRunScript(lockPath string, runID string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$lockPath = %s
$runID = %s
%s
$result = Invoke-WithLockMutex $lockPath {
  if (!(Test-Path -LiteralPath $lockPath)) { return @{state='unlocked'} }
  $fields = Lock-Fields ([string](Get-Content -LiteralPath $lockPath -Raw -ErrorAction Stop))
  if ($fields.keptRun -cne $runID -and $fields.activeRun -cne $runID) { return @{state='different'} }
  Remove-Item -LiteralPath $lockPath -Force -ErrorAction Stop
  return @{state='unlocked'}
}
Out-Result $result`, powerShellSingleQuoted(lockPath), powerShellSingleQuoted(runID), remoteHostLockCommonPS)
}

func remoteHostLockStateScript(lockPath string) string {
	return fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$lockPath = %s
%s
$result = Invoke-WithLockMutex $lockPath {
  if (!(Test-Path -LiteralPath $lockPath)) { return @{state='unlocked'} }
  return (Locked-Result $lockPath)
}
Out-Result $result`, powerShellSingleQuoted(lockPath), remoteHostLockCommonPS)
}
