#!/usr/bin/env bash
# Model-check the Control Plane <-> Windows Host Agent Host Lock protocol.
#
#   verification/host-lock/check.sh          # safety, canaries, mutants (~15s)
#   verification/host-lock/check.sh --quick  # skip the larger bound
#   verification/host-lock/check.sh --only-safety [quick|full]
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
trap 'rm -rf "$work"' EXIT

# tlc <dir> <cfg> <log>: runs TLC, returns its exit code, keeps the full log.
tlc() {
  local dir="$1" cfg="$2" log="$3"
  (cd "$dir" && java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -workers auto -deadlock \
    -cleanup -metadir "$work/meta.$RANDOM" -config "$cfg" MC.tla) >"$log" 2>&1
}

failures=0

safety() {
  local bound="$1" log="$work/safety.$1.log"
  if tlc "$here" "MC.$bound.cfg" "$log"; then
    echo "PASS safety [$bound]: $(grep -E 'distinct states found' "$log" | tail -1)"
  else
    echo "FAIL safety [$bound]: see counterexample below"
    sed -n '/Error:/,$p' "$log"
    failures=$((failures + 1))
  fi
}

# expect_violation <label> <dir> <invariant>: TLC must report exactly this invariant.
expect_violation() {
  local label="$1" dir="$2" inv="$3" log="$work/$1.log"
  sed -e '/^INVARIANTS/,$d' "$here/MC.quick.cfg" >"$dir/probe.cfg"
  printf 'INVARIANTS\n  %s\n' "$inv" >>"$dir/probe.cfg"
  if tlc "$dir" probe.cfg "$log"; then
    echo "FAIL $label: $inv was never violated"
    failures=$((failures + 1))
  elif grep -q "Invariant $inv is violated" "$log"; then
    echo "ok   $label: $inv violated as expected"
  else
    echo "FAIL $label: TLC error other than $inv"
    tail -20 "$log"
    failures=$((failures + 1))
  fi
}

# mutant <name> <invariant> <perl substitution>: break one guard, expect a violation.
mutant() {
  local name="$1" inv="$2" expr="$3" dir="$work/mutant.$1"
  mkdir -p "$dir"
  cp "$here/MC.tla" "$dir/"
  perl -0pe "$expr" "$here/HostAgentHostLock.tla" >"$dir/HostAgentHostLock.tla"
  if cmp -s "$here/HostAgentHostLock.tla" "$dir/HostAgentHostLock.tla"; then
    echo "FAIL mutant $name: substitution matched nothing; update check.sh"
    failures=$((failures + 1))
    return
  fi
  expect_violation "mutant:$name" "$dir" "$inv"
}

mode="${1:-all}"
case "$mode" in
  --only-safety) safety "${2:-quick}" ;;
  all | --quick)
    safety quick
    [[ "$mode" == all ]] && safety full

    # Canaries: the model reaches each situation (no vacuous pass).
    canaries="$work/canaries"
    mkdir -p "$canaries"
    cp "$here/MC.tla" "$here/HostAgentHostLock.tla" "$canaries/"
    for canary in CanaryNoMaya CanaryHostNeverReused \
      CanaryNoQuarantineOverLiveMaya CanaryNoConfirmedAcrossRestart \
      CanaryNoTakeoverOfAssigned CanaryNoMarkerRecovery; do
      expect_violation "canary" "$canaries" "$canary"
    done

    # Mutants: each removes one guard from the Go code's model; the matching
    # property must fail.
    # serveHostAgentRegistration lets a new process take over a confirmed run
    mutant register-takeover OneMayaPerHost \
      's/(Register\(p\) ==.*?)\n\s+\/\\ ~RegisterBlockedByConfirmed/$1/s'
    # serveHostAgentConfirm skips the process-session fence
    mutant confirm-without-session OneMayaPerHost \
      's/(\/\\ pc\[p\] = "polled") \/\\ SessionOK\(p\)/$1/'
    # runHostAgentOnce reports complete instead of quarantine after an unresolved stop
    mutant unresolved-stop-completes MayaHeldByHostLock \
      's/(PostTerminal\(p\) ==\n.*?\n\s+\/\\ )pc\[p\] = "stopped"/$1pc[p] \\in {"stopped", "unresolved"}/s'
    # serveHostAgentRegistration replaces a live process session
    mutant register-steals-live-session OneMayaPerHost \
      's/\n\s+\/\\ ~\(sess # NoProc \/\\ leaseLive\)//'
    # runScenarioThroughHostAgent never calls removeQueuedRunLocked
    mutant accept-keeps-queue AssignOnce \
      's/dQueued\x27 = \[dQueued EXCEPT !\[r\] = FALSE\]/UNCHANGED dQueued/'
    # hostAgentTransitionStore.apply writes the assignment but not the Host Lock
    mutant commit-skips-host-lock RestartLoadable \
      's/(dAsg\x27 = \[dAsg EXCEPT !\[r\] = "confirmed"\]\n\s+\/\\ )lockRun\x27 = r \/\\ lockState\x27 = "confirmed"/$1UNCHANGED <<lockRun, lockState>>/'
    ;;
  *) echo "usage: $0 [--quick | --only-safety [quick|full]]" >&2; exit 2 ;;
esac

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
