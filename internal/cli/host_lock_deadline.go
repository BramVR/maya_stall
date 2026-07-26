package cli

import (
	"fmt"
	"strings"
	"time"
)

const defaultHostLockIdleTimeout = 30 * time.Minute
const defaultHostLockHardLifetime = 6 * time.Hour
const minimumHostLockIdleTimeout = 2 * hostAgentHeartbeatInterval

type hostLockDeadlinePolicy struct {
	IdleTimeout  time.Duration
	HardLifetime time.Duration
}

type hostLockDeadlines struct {
	LastHeartbeatAt string `json:"lastHeartbeatAt"`
	IdleDeadline    string `json:"idleDeadline"`
	HardDeadline    string `json:"hardDeadline"`
	KeepDeadline    string `json:"keepDeadline,omitempty"`
	ExtensionCount  int    `json:"extensionCount,omitempty"`
}

func defaultHostLockDeadlinePolicy() hostLockDeadlinePolicy {
	return hostLockDeadlinePolicy{
		IdleTimeout:  defaultHostLockIdleTimeout,
		HardLifetime: defaultHostLockHardLifetime,
	}
}

func newHostLockDeadlines(now time.Time, policy hostLockDeadlinePolicy) hostLockDeadlines {
	now = now.UTC()
	return hostLockDeadlines{
		LastHeartbeatAt: now.Format(time.RFC3339Nano),
		IdleDeadline:    now.Add(policy.IdleTimeout).Format(time.RFC3339Nano),
		HardDeadline:    now.Add(policy.HardLifetime).Format(time.RFC3339Nano),
	}
}

func (deadlines *hostLockDeadlines) recordHeartbeat(now time.Time, policy hostLockDeadlinePolicy) error {
	hard, err := parseHostLockDeadline(deadlines.HardDeadline, "hard")
	if err != nil {
		return err
	}
	now = now.UTC()
	if !now.Before(hard) {
		return fmt.Errorf("Host Lock hard deadline has expired") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	idle, err := parseHostLockDeadline(deadlines.IdleDeadline, "idle")
	if err != nil {
		return err
	}
	if !now.Before(idle) {
		return fmt.Errorf("Host Lock idle deadline has expired") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	nextIdle := now.Add(policy.IdleTimeout)
	if nextIdle.After(hard) {
		nextIdle = hard
	}
	deadlines.LastHeartbeatAt = now.Format(time.RFC3339Nano)
	deadlines.IdleDeadline = nextIdle.Format(time.RFC3339Nano)
	return nil
}

func (deadlines *hostLockDeadlines) markKept(now time.Time, keepTTL time.Duration, policy hostLockDeadlinePolicy) error {
	if keepTTL <= 0 {
		return fmt.Errorf("kept-session TTL must be positive")
	}
	if err := deadlines.recordHeartbeat(now, policy); err != nil {
		return err
	}
	hard, err := parseHostLockDeadline(deadlines.HardDeadline, "hard")
	if err != nil {
		return err
	}
	keep := now.UTC().Add(keepTTL)
	if keep.After(hard) {
		keep = hard
	}
	deadlines.KeepDeadline = keep.Format(time.RFC3339Nano)
	return nil
}

func (deadlines *hostLockDeadlines) extendKept(now time.Time, extension time.Duration, policy hostLockDeadlinePolicy) error {
	if extension <= 0 {
		return fmt.Errorf("kept-session extension must be positive")
	}
	keep, err := parseHostLockDeadline(deadlines.KeepDeadline, "kept-session")
	if err != nil {
		return err
	}
	hard, err := parseHostLockDeadline(deadlines.HardDeadline, "hard")
	if err != nil {
		return err
	}
	now = now.UTC()
	if !now.Before(keep) {
		return fmt.Errorf("Kept Session deadline has expired") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	extended := keep.Add(extension)
	if extended.After(hard) {
		return fmt.Errorf("Kept Session extension exceeds Host Lock hard deadline %s", deadlines.HardDeadline) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if !now.Before(hard) {
		return fmt.Errorf("Host Lock hard deadline has expired") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	deadlines.KeepDeadline = extended.Format(time.RFC3339Nano)
	deadlines.ExtensionCount++
	return nil
}

func (deadlines hostLockDeadlines) expiryReason(now time.Time) string {
	now = now.UTC()
	if deadlineReached(deadlines.HardDeadline, now) {
		return "hard-lifetime"
	}
	if deadlineReached(deadlines.KeepDeadline, now) {
		return "kept-session"
	}
	if deadlineReached(deadlines.IdleDeadline, now) {
		return "idle"
	}
	return ""
}

func (deadlines hostLockDeadlines) validate() error {
	lastHeartbeat, err := parseHostLockDeadline(deadlines.LastHeartbeatAt, "last heartbeat")
	if err != nil {
		return err
	}
	idle, err := parseHostLockDeadline(deadlines.IdleDeadline, "idle")
	if err != nil {
		return err
	}
	hard, err := parseHostLockDeadline(deadlines.HardDeadline, "hard")
	if err != nil {
		return err
	}
	if idle.Before(lastHeartbeat) || hard.Before(idle) || deadlines.ExtensionCount < 0 {
		return fmt.Errorf("Host Lock deadline order is invalid") //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	if deadlines.KeepDeadline != "" {
		keep, err := parseHostLockDeadline(deadlines.KeepDeadline, "kept-session")
		if err != nil {
			return err
		}
		if keep.After(hard) {
			return fmt.Errorf("Host Lock kept-session deadline exceeds its hard deadline") //nolint:staticcheck // Product term starts the user-facing diagnostic.
		}
	}
	return nil
}

func parseHostLockDeadline(value string, label string) (time.Time, error) {
	if value == "" {
		return time.Time{}, fmt.Errorf("Host Lock %s deadline is missing", label) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("Host Lock %s deadline is invalid: %w", label, err) //nolint:staticcheck // Product term starts the user-facing diagnostic.
	}
	return deadline, nil
}

func deadlineReached(value string, now time.Time) bool {
	if value == "" {
		return false
	}
	deadline, err := time.Parse(time.RFC3339Nano, value)
	return err != nil || !now.Before(deadline)
}

func hostLockDeadlineRemaining(value string, now time.Time) string {
	deadline, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return "invalid"
	}
	remaining := deadline.Sub(now.UTC())
	if remaining <= 0 {
		return "expired"
	}
	if remaining < time.Minute {
		return "<1m left"
	}
	rounded := remaining.Round(time.Minute).String()
	return strings.TrimSuffix(rounded, "0s") + " left"
}
