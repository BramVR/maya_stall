package cli

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRemoteHostLockReclaimRequiresExpiredLeaseAndInactiveBroker(t *testing.T) {
	expired := remoteHostLockResult{Locked: true, State: "active", LeaseExpired: true, LeaseVersion: "1", LockToken: "owner-token", BrokerStateDir: "C:/maya-stall/sessiond-ui", BrokerPython: "C:/maya-stall/python.exe", BrokerRepo: "C:/maya-stall/broker"}
	if remoteHostLockReclaimAllowed(expired, false) {
		t.Fatal("expired Host Lock was reclaimable while Session Broker remained active")
	}
	if !remoteHostLockReclaimAllowed(expired, true) {
		t.Fatal("expired Host Lock was not reclaimable after Session Broker proved inactive")
	}
	fresh := expired
	fresh.LeaseExpired = false
	if remoteHostLockReclaimAllowed(fresh, true) {
		t.Fatal("live Host Lock lease was reclaimable")
	}
	missingBrokerOwner := expired
	missingBrokerOwner.BrokerStateDir = ""
	if remoteHostLockReclaimAllowed(missingBrokerOwner, true) {
		t.Fatal("Host Lock without its owning Session Broker state directory was reclaimable")
	}
}

func TestRemoteHostLockRecoveryRequiresTrustedBrokerConfiguration(t *testing.T) {
	host := mayaHostConfig{Broker: brokerConfig{
		StateDir: "C:/maya-stall/sessiond-ui",
		Python:   "C:/maya-stall/python.exe",
		Repo:     "C:/maya-stall/broker",
	}}
	owner := remoteHostLockResult{
		BrokerStateDir: host.Broker.StateDir,
		BrokerPython:   host.Broker.Python,
		BrokerRepo:     host.Broker.Repo,
	}
	if !remoteHostLockBrokerConfigMatches(host, owner) {
		t.Fatal("matching trusted broker configuration was rejected")
	}
	variant := owner
	variant.BrokerStateDir = `c:\MAYA-STALL\SESSIOND-UI`
	variant.BrokerPython = `c:\MAYA-STALL\PYTHON.EXE`
	variant.BrokerRepo = `c:\MAYA-STALL\BROKER`
	if !remoteHostLockBrokerConfigMatches(host, variant) {
		t.Fatal("equivalent Windows broker paths were rejected")
	}
	relativeStateHost := host
	relativeStateHost.Broker.StateDir = "sessiond-ui"
	relativeStateOwner := owner
	relativeStateOwner.BrokerStateDir = `c:\maya-stall\broker\sessiond-ui`
	if !remoteHostLockBrokerConfigMatches(relativeStateHost, relativeStateOwner) {
		t.Fatal("relative trusted broker state directory did not match its absolute owner path")
	}
	commandPythonHost := host
	commandPythonHost.Broker.Python = "python"
	commandPythonOwner := owner
	commandPythonOwner.BrokerPython = "PYTHON"
	if !remoteHostLockBrokerConfigMatches(commandPythonHost, commandPythonOwner) {
		t.Fatal("trusted non-absolute broker Python command was rejected")
	}
	owner.BrokerPython = "C:/maya-stall/untrusted/python.exe"
	if remoteHostLockBrokerConfigMatches(host, owner) {
		t.Fatal("host-writable lock selected an untrusted broker executable")
	}
}

func TestHostLockContentionSmokeHeartbeatPrecedesLeaseExpiry(t *testing.T) {
	lease := 2 * time.Second
	interval := hostLockContentionHeartbeatInterval(lease)
	if interval <= 0 || interval >= lease {
		t.Fatalf("contention heartbeat interval = %s, want within lease %s", interval, lease)
	}
}

func TestRetryRemoteHostLockTimeout(t *testing.T) {
	oldDelay := remoteHostLockRetryDelay
	remoteHostLockRetryDelay = 0
	t.Cleanup(func() { remoteHostLockRetryDelay = oldDelay })
	calls := 0
	result, err := retryRemoteHostLockTimeout(func() (remoteHostLockResult, error) {
		calls++
		if calls == 1 {
			return remoteHostLockResult{}, errors.New("ssh command timed out after 30s")
		}
		return remoteHostLockResult{State: "unlocked"}, nil
	})
	if err != nil || result.State != "unlocked" || calls != 2 {
		t.Fatalf("retry result = %+v, calls=%d, err=%v", result, calls, err)
	}

	calls = 0
	_, err = retryRemoteHostLockTimeout(func() (remoteHostLockResult, error) {
		calls++
		return remoteHostLockResult{}, errors.New("permission denied")
	})
	if err == nil || calls != 1 {
		t.Fatalf("non-timeout retry calls=%d err=%v", calls, err)
	}
}

func TestRemoteHostLockReplaceRetryAcceptsAlreadyUpdatedContent(t *testing.T) {
	script := remoteHostLockReplaceScript("C:/maya-stall/state/locks/host.lock", "old-owner", "new-owner")
	alreadyUpdated := strings.Index(script, "$current -ceq $content")
	ownershipChanged := strings.LastIndex(script, "$current -cne $expected")
	if alreadyUpdated < 0 || ownershipChanged < 0 || alreadyUpdated > ownershipChanged {
		t.Fatalf("replace script does not accept intended content before ownership mismatch:\n%s", script)
	}
}

func TestRemoteHostLockRenewalRetriesAfterUnchangedOwnershipOnRemoteFailure(t *testing.T) {
	oldDelay := remoteHostLockRetryDelay
	remoteHostLockRetryDelay = 0
	t.Cleanup(func() { remoteHostLockRetryDelay = oldDelay })
	expected := "host: maya-win\nactiveRun: run-owned\nlockToken: token-owned\n"
	replacement := expected + "leaseExpiresAt: later\n"
	mutations := 0
	run := func(mayaHostConfig, string) (remoteHostLockResult, error) {
		mutations++
		if mutations == 1 {
			return remoteHostLockResult{}, errors.New("remote PowerShell lock file temporarily unavailable")
		}
		return remoteHostLockResult{State: "updated"}, nil
	}
	reads := 0
	read := func(mayaHostConfig, string) (remoteHostLockResult, error) {
		reads++
		return remoteHostLockResult{Locked: true, State: "active", Raw: expected, ContentRead: true}, nil
	}

	if err := replaceRemoteHostLockOwnerWithTransport(mayaHostConfig{}, "host.lock", expected, replacement, run, read); err != nil {
		t.Fatalf("renew unchanged Host Lock after transient remote failure: %v", err)
	}
	if mutations != 2 || reads != 1 {
		t.Fatalf("renew calls = mutations %d reads %d; want failed replace, ownership read, successful reconnect", mutations, reads)
	}
}

func TestRemoteHostLockReleaseRetriesAfterUnchangedOwnershipOnRemoteFailure(t *testing.T) {
	oldDelay := remoteHostLockRetryDelay
	remoteHostLockRetryDelay = 0
	t.Cleanup(func() { remoteHostLockRetryDelay = oldDelay })
	expected := "host: maya-win\nactiveRun: run-owned\nlockToken: token-owned\n"
	mutations := 0
	run := func(mayaHostConfig, string) (remoteHostLockResult, error) {
		mutations++
		if mutations == 1 {
			return remoteHostLockResult{}, errors.New("remote PowerShell lock file temporarily unavailable")
		}
		return remoteHostLockResult{State: "unlocked"}, nil
	}
	reads := 0
	read := func(mayaHostConfig, string) (remoteHostLockResult, error) {
		reads++
		return remoteHostLockResult{Locked: true, State: "active", Raw: expected, ContentRead: true}, nil
	}

	if err := removeRemoteHostLockWithTransport(mayaHostConfig{}, "host.lock", expected, run, read); err != nil {
		t.Fatalf("release unchanged Host Lock after transient remote failure: %v", err)
	}
	if mutations != 2 || reads != 1 {
		t.Fatalf("release calls = mutations %d reads %d; want failed release, ownership read, successful reconnect", mutations, reads)
	}
}

func TestRemoteHostLockRenewalDoesNotRetryAfterSuccessorTakesOwnership(t *testing.T) {
	oldDelay := remoteHostLockRetryDelay
	remoteHostLockRetryDelay = 0
	t.Cleanup(func() { remoteHostLockRetryDelay = oldDelay })
	expected := "host: maya-win\nactiveRun: run-old\nlockToken: token-old\n"
	successor := "host: maya-win\nactiveRun: run-new\nlockToken: token-new\n"
	mutations := 0
	run := func(mayaHostConfig, string) (remoteHostLockResult, error) {
		mutations++
		return remoteHostLockResult{}, errors.New("remote PowerShell lock file temporarily unavailable")
	}
	reads := 0
	read := func(mayaHostConfig, string) (remoteHostLockResult, error) {
		reads++
		return remoteHostLockResult{Locked: true, State: "active", Raw: successor, ContentRead: true}, nil
	}

	err := replaceRemoteHostLockOwnerWithTransport(mayaHostConfig{}, "host.lock", expected, expected+"leaseExpiresAt: later\n", run, read)
	if !errors.Is(err, errHostLockOwnershipChanged) {
		t.Fatalf("renewal after successor ownership error = %v, want Host Lock ownership changed", err)
	}
	if mutations != 1 || reads != 1 {
		t.Fatalf("successor ownership calls = mutations %d reads %d; must prevent retry", mutations, reads)
	}
}

func TestRemoteHostLockMutationTimeoutDoesNotRetryWithoutOwnershipProof(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(func(mayaHostConfig, string) (remoteHostLockResult, error), func(mayaHostConfig, string) (remoteHostLockResult, error)) error
	}{
		{
			name: "renewal",
			mutate: func(run func(mayaHostConfig, string) (remoteHostLockResult, error), read func(mayaHostConfig, string) (remoteHostLockResult, error)) error {
				return replaceRemoteHostLockOwnerWithTransport(mayaHostConfig{}, "host.lock", "expected", "replacement", run, read)
			},
		},
		{
			name: "release",
			mutate: func(run func(mayaHostConfig, string) (remoteHostLockResult, error), read func(mayaHostConfig, string) (remoteHostLockResult, error)) error {
				return removeRemoteHostLockWithTransport(mayaHostConfig{}, "host.lock", "expected", run, read)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutations := 0
			run := func(mayaHostConfig, string) (remoteHostLockResult, error) {
				mutations++
				return remoteHostLockResult{}, errSSHCommandTimedOut
			}
			read := func(mayaHostConfig, string) (remoteHostLockResult, error) {
				return remoteHostLockResult{}, errors.New("Host Lock unreadable")
			}

			err := test.mutate(run, read)
			if !errors.Is(err, errSSHCommandTimedOut) || mutations != 1 {
				t.Fatalf("unproven timeout = err %v mutations %d; want one fail-closed attempt", err, mutations)
			}
		})
	}
}

func TestSessiondStatusMustExplicitlyProveEveryProcessInactive(t *testing.T) {
	stopped := sessiondStatusResult{HasState: true, DerivedStatus: "stopped", ProcessAlive: map[string]bool{"daemon": false, "maya": false, "mcp": false}}
	stopped.State.Status = "stopped"
	if !sessiondStatusProvesInactive(stopped) {
		t.Fatal("explicit stopped broker with dead processes was not accepted as inactive")
	}
	missing := sessiondStatusResult{DerivedStatus: "missing", ProcessAlive: map[string]bool{}}
	if sessiondStatusProvesInactive(missing) {
		t.Fatal("missing broker state was accepted as proof of inactivity")
	}
	missing.ProcessAlive["maya"] = true
	if sessiondStatusProvesInactive(missing) {
		t.Fatal("missing broker state with a live Maya process was accepted as inactive")
	}
	if sessiondStatusProvesInactive(sessiondStatusResult{}) {
		t.Fatal("incomplete broker status was accepted as inactive")
	}
	incompleteStopped := stopped
	delete(incompleteStopped.ProcessAlive, "daemon")
	if sessiondStatusProvesInactive(incompleteStopped) {
		t.Fatal("stopped broker without complete process liveness was accepted as inactive")
	}
}

func TestHostLockHeartbeatKeepsFirstRenewalFailure(t *testing.T) {
	oldInterval := hostSideLockHeartbeatInterval
	hostSideLockHeartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { hostSideLockHeartbeatInterval = oldInterval })
	want := errors.New("ownership lost")
	calls := 0
	stop, check := startHostLockHeartbeat(func() error {
		calls++
		if calls == 1 {
			return want
		}
		return nil
	})
	t.Cleanup(func() { _ = stop() })
	deadline := time.Now().Add(time.Second)
	for check() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(check(), want) {
		t.Fatalf("heartbeat error = %v, want %v", check(), want)
	}
	time.Sleep(15 * time.Millisecond)
	if !errors.Is(stop(), want) {
		t.Fatalf("stopped heartbeat error = %v, want sticky %v", stop(), want)
	}
}

func TestRenewHostLockDoesNotConvertKeptOwnerBackToLeased(t *testing.T) {
	const kept = "host: alpha\nlockToken: token\nkeptRun: kept-run\n"
	replaced := false
	lock := hostSideLock{
		expected: kept,
		replaceOwner: func(string, string) error {
			replaced = true
			return nil
		},
	}
	if err := lock.renew("alpha"); err != nil {
		t.Fatalf("renew kept Host Lock: %v", err)
	}
	if replaced {
		t.Fatal("renew rewrote a kept Host Lock")
	}
	if !strings.Contains(lock.expected, "keptRun: kept-run") {
		t.Fatalf("kept Host Lock changed: %q", lock.expected)
	}
}
