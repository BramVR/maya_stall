#!/usr/bin/env bash
# Model-check the Control Plane durable Run queue (ADR 0050) across two Hosts.
#
#   verification/run-queue/check.sh                # safety, findings, canaries, mutants (~20-60s)
#   verification/run-queue/check.sh --only-safety  # just the safety pass (~10-30s)
#
# Needs Java 11+. Downloads the pinned tla2tools.jar once into
# ${XDG_CACHE_HOME:-~/.cache}/maya-stall-verification and verifies its sha256.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
tla_version="1.7.4"
tla_sha256="936a262061c914694dfd669a543be24573c45d5aa0ff20a8b96b23d01e050e88"
cache="${XDG_CACHE_HOME:-$HOME/.cache}/maya-stall-verification"
jar="$cache/tla2tools-$tla_version.jar"

if [[ ! -f "$jar" ]]; then
  mkdir -p "$cache"
  curl -fsSL -o "$jar.tmp" "https://github.com/tlaplus/tlaplus/releases/download/v$tla_version/tla2tools.jar"
  mv "$jar.tmp" "$jar"
fi
actual="$(shasum -a 256 "$jar" | cut -d' ' -f1)"
if [[ "$actual" != "$tla_sha256" ]]; then
  echo "tla2tools.jar sha256 mismatch: $actual" >&2
  exit 2
fi

work="$(mktemp -d)"
trap 'rm -rf "$work" "$here/probe.cfg"' EXIT

# tlc <dir> <cfg> <log>: runs TLC, returns its exit code, keeps the full log.
tlc() {
  local dir="$1" cfg="$2" log="$3"
  (cd "$dir" && java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -workers auto -deadlock \
    -cleanup -metadir "$work/meta.$RANDOM" -config "$cfg" MC.tla) >"$log" 2>&1
}

failures=0

safety() {
  local log="$work/safety.log"
  if tlc "$here" MC.quick.cfg "$log"; then
    echo "PASS safety: $(grep -E 'distinct states found' "$log" | tail -1)"
  else
    echo "FAIL safety: see counterexample below"
    sed -n '/Error:/,$p' "$log"
    failures=$((failures + 1))
  fi
}

# expect_violation <label> <dir> <INVARIANTS|PROPERTIES> <name> [base cfg]
expect_violation() {
  local label="$1" dir="$2" kind="$3" name="$4" base="${5:-MC.quick.cfg}" log="$work/$1.$4.$RANDOM.log"
  sed -e '/^INVARIANTS/,$d' "$here/$base" >"$dir/probe.cfg"
  printf '%s\n  %s\n' "$kind" "$name" >>"$dir/probe.cfg"
  if tlc "$dir" probe.cfg "$log"; then
    echo "FAIL $label: $name was never violated"
    failures=$((failures + 1))
  elif grep -qE "(Invariant|Action property) $name is violated" "$log"; then
    echo "ok   $label: $name violated as expected"
  else
    echo "FAIL $label: TLC error other than $name"
    tail -20 "$log"
    failures=$((failures + 1))
  fi
}

# mutant <name> <kind> <property> <perl substitution> [base cfg]: break one guard, expect a violation.
mutant() {
  local name="$1" kind="$2" prop="$3" expr="$4" base="${5:-MC.quick.cfg}" dir="$work/mutant.$1"
  mkdir -p "$dir"
  cp "$here/MC.tla" "$dir/"
  perl -0pe "$expr" "$here/ControlPlaneRunQueue.tla" >"$dir/ControlPlaneRunQueue.tla"
  if cmp -s "$here/ControlPlaneRunQueue.tla" "$dir/ControlPlaneRunQueue.tla"; then
    echo "FAIL mutant $name: substitution matched nothing; update check.sh"
    failures=$((failures + 1))
    return
  fi
  expect_violation "mutant:$name" "$dir" "$kind" "$prop" "$base"
}

mode="${1:-all}"
case "$mode" in
  --only-safety) safety ;;
  all)
    safety

    # Open findings: under file-write faults the current code violates these.
    # When a fix lands and the model matches it, the probe reports "never
    # violated": enable the fault in MC.quick.cfg and delete the probe line.
    # MC.findings.cfg adds the file-write faults these need.
    expect_violation "finding" "$here" INVARIANTS ReservationOwned MC.findings.cfg
    expect_violation "finding" "$here" PROPERTIES TerminalLedgerStable MC.findings.cfg
    expect_violation "finding" "$here" INVARIANTS RestartLoadable MC.findings.cfg

    # Canaries: the model reaches each situation (no vacuous pass).
    for canary in CanaryNoAssignment CanaryNoParallelAssignments CanaryNoCancel \
      CanaryNoContention CanaryNoRequeue CanaryNoRestartWithQueue CanaryNoReadmission \
      CanaryNoInertEntry; do
      expect_violation "canary" "$here" INVARIANTS "$canary"
    done
    # MC.bound.cfg has MaxQueued=1 so two Runs can overflow the queue
    expect_violation "canary" "$here" INVARIANTS CanaryNoQueueFull MC.bound.cfg

    # Mutants: each removes one guard the Go code has; the property must fail.
    # cancelQueuedRun: cancel a Run whose dispatch is marked
    mutant cancel-while-dispatching INVARIANTS CanceledNeverAssigned \
      's/(CancelStart\(r\) ==\n.*?inMap\[r\]) \/\\ ~dispatching\[r\]/$1/s'
    # loadControlPlaneQueue: a canceling intent goes back to the queue
    mutant restart-requeues-canceling INVARIANTS CanceledNeverAssigned \
      's/ELSE IF intent\[r\] = "canceling" THEN \[i \|-> "none", l \|-> "canceled", q \|-> FALSE, c \|-> FALSE\]\n//'
    # dispatchQueuedRuns: queueAcquireHostLock always succeeds (no shared Host Lock)
    mutant dispatch-ignores-shared-lock INVARIANTS HostExclusive \
      's/IF fakeLock\[h\] # NoRun\n/IF FALSE\n/'
    # queueHostAgentRun: skip the queue bound
    mutant no-queue-bound INVARIANTS QueueBound \
      's/IF Waiting \+ admissions >= MaxQueued\n/IF FALSE\n/' MC.bound.cfg
    # failure paths: remove the queue intent without a terminal Run Ledger
    mutant intent-removed-before-terminal INVARIANTS QueuedRecoverable \
      's/(AdmFail\(r\) ==\n.*?)ledger\x27 = \[ledger EXCEPT !\[r\] = "failed"\]/$1UNCHANGED ledger/s'
    # loadControlPlaneQueue: a queued intent beside a terminal ledger is an error
    mutant restart-rejects-terminal INVARIANTS RestartLoadable \
      's/(\\\/ intent\[r\] = "queued" \/\\ l \\notin \{"submitted", "queued"\}) \\cup Terminal/$1/'
    ;;
  *) echo "usage: $0 [--only-safety]" >&2; exit 2 ;;
esac

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
