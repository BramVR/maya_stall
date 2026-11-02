#!/usr/bin/env bash
# Model-check Embedded Mode run retention: Fresh Run controller, Kept Session
# sweep and explicit stop, and stop -> Host Lock release.
#
#   verification/retention/check.sh          # safety, unfenced-start, canaries, mutants
#   verification/retention/check.sh --quick  # skip the larger bound
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

tlc() {
  local dir="$1" cfg="$2" log="$3"
  (cd "$dir" && java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -workers auto -deadlock \
    -cleanup -metadir "$work/meta.$RANDOM" -config "$cfg" MC.tla) >"$log" 2>&1
}

failures=0

# The unfenced broker (issue 170, FenceStart = FALSE) must still violate
# MayaHeldByHostLock, so the fence in MC.code.cfg is what makes it pass.
unfenced_start() {
  local log="$work/unfenced.log" cfg="$work/unfenced.cfg"
  sed -e 's/FenceStart = TRUE/FenceStart = FALSE/' "$here/MC.code.cfg" >"$cfg"
  if tlc "$here" "$cfg" "$log"; then
    echo "FAIL unfenced-start: MayaHeldByHostLock was never violated"
    failures=$((failures + 1))
  elif grep -q "Invariant MayaHeldByHostLock is violated" "$log"; then
    echo "ok   unfenced-start: MayaHeldByHostLock violated as expected"
  else
    echo "FAIL unfenced-start: unexpected result"
    sed -n '/Error:/,$p' "$log" | head -60
    failures=$((failures + 1))
  fi
}

safety() {
  local cfg="$1" log="$work/$1.log"
  if tlc "$here" "MC.$cfg.cfg" "$log"; then
    echo "PASS safety [$cfg]: $(grep -E 'distinct states found' "$log" | tail -1)"
  else
    echo "FAIL safety [$cfg]: see counterexample below"
    sed -n '/Error:/,$p' "$log"
    failures=$((failures + 1))
  fi
}

expect_violation() {
  local label="$1" dir="$2" inv="$3" log="$work/$1.$3.log"
  sed -e '/^INVARIANTS/,$d' "$here/MC.code.cfg" >"$dir/probe.cfg"
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

mutant() {
  local name="$1" inv="$2" expr="$3" dir="$work/mutant.$1"
  mkdir -p "$dir"
  cp "$here/MC.tla" "$dir/"
  perl -0pe "$expr" "$here/RunRetention.tla" >"$dir/RunRetention.tla"
  if cmp -s "$here/RunRetention.tla" "$dir/RunRetention.tla"; then
    echo "FAIL mutant $name: substitution matched nothing; update check.sh"
    failures=$((failures + 1))
    return
  fi
  expect_violation "mutant:$name" "$dir" "$inv"
}

mode="${1:-all}"
case "$mode" in
  all | --quick) ;;
  *) echo "usage: $0 [--quick]" >&2; exit 2 ;;
esac

safety code
unfenced_start
[[ "$mode" == all ]] && safety full

canary_dir="$work/canary"
mkdir -p "$canary_dir"
cp "$here/MC.tla" "$here/RunRetention.tla" "$canary_dir/"
for canary in CanaryNoKept CanaryNoKeptStop CanaryNoCleanupPending CanaryNoBoundDeadStop \
  CanaryNoUnboundDeadStop CanaryNoReclaim CanaryHostNeverReused CanaryNoHeartbeatFailure \
  CanaryNoFenceRefusal CanaryNoStandingSession; do
  expect_violation canary "$canary_dir" "$canary"
done

# Each mutant removes one guard the Go code has; the named property must fail.
# StopRetainedRun stops the exact bound Kept Session before release
mutant release-without-broker-stop MayaHeldByHostLock \
  's/THEN \/\\ broker\x27 = NoRun\n(\s+\/\\ phase\x27 = \[phase EXCEPT !\[r\] = "broker-cleaned"\])/THEN \/\\ UNCHANGED broker\n$1/'
# finishStoppedRunCleanup releases through compare-and-swap (hostSideLock.release)
mutant cleanup-release-without-cas MayaHeldByHostLock \
  's/(CleanupRelease\(r\) ==\n.*?\n\s+\/\\ IF )Owns\(r\) \\\/ lockRun = NoRun/$1TRUE/s'
# remoteHostLockReclaimAllowed requires broker inactivity
mutant reclaim-ignores-broker MayaHeldByHostLock \
  's/lockKind = "active" \/\\ now >= lease \/\\ BrokerInactive/lockKind = "active" \/\\ now >= lease/'
# requireStaleSubmittedRunController refuses crash-recovery stop of a live controller
mutant stop-ignores-live-controller MayaHeldByHostLock \
  's/\/\\ mirror\[r\] = "active" \/\\ ~ControllerLive\(r\)/\/\\ mirror[r] = "active"/'


if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
