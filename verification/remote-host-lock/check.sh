#!/usr/bin/env bash
# Model-check the Embedded Mode host-side Host Lock over SSH (host_lock_ssh.go).
#
#   verification/remote-host-lock/check.sh          # safety (3 configs), canaries, mutants (~25s)
#   verification/remote-host-lock/check.sh --quick  # skip the 3-controller bound
#   verification/remote-host-lock/check.sh --only-safety [pause|nopause|unfenced]
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
  local cfg="$work/probe.$RANDOM.cfg"
  sed -e '/^INVARIANTS/,$d' "$here/${4:-MC.pause.cfg}" >"$cfg"
  printf 'INVARIANTS\n  %s\n' "$inv" >>"$cfg"
  if tlc "$dir" "$cfg" "$log"; then
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

# mutant <name> <invariant> <perl substitution> [cfg]: break one guard, expect a violation.
mutant() {
  local name="$1" inv="$2" expr="$3" cfg="${4:-MC.pause.cfg}" dir="$work/mutant.$1"
  mkdir -p "$dir"
  cp "$here/MC.tla" "$dir/"
  perl -0pe "$expr" "$here/RemoteHostLock.tla" >"$dir/RemoteHostLock.tla"
  if cmp -s "$here/RemoteHostLock.tla" "$dir/RemoteHostLock.tla"; then
    echo "FAIL mutant $name: substitution matched nothing; update check.sh"
    failures=$((failures + 1))
    return
  fi
  expect_violation "mutant:$name" "$dir" "$inv" "$cfg"
}

mode="${1:-all}"
case "$mode" in
  --only-safety) safety "${2:-pause}" ;;
  all | --quick)
    safety pause
    [[ "$mode" == all ]] && safety nopause

    # Without the renew-to-broker-call timing assumption: the Host Lock
    # fence on broker calls (issue 170) must carry safety on its own.
    safety unfenced

    # Canaries: the model reaches each situation (no vacuous pass).
    for canary in CanaryNoSession CanaryNoReclaim CanaryNoKept CanaryNoDelayedCreate CanaryNoHostReuse; do
      expect_violation "canary" "$here" "$canary"
    done

    # Mutants: each removes one guard of the Go/PowerShell code; the matching
    # property must fail.
    # remoteHostLockSessionInactive / sessiondStatusProvesInactive skipped
    mutant reclaim-ignores-broker NoReclaimOverLiveSession \
      's/attempts\[c\] < MaxReclaim \/\\ obsExp\[c\] \/\\ sess = None/attempts[c] < MaxReclaim \/\\ obsExp[c]/'
    # Locked-Result keptRun => leaseExpired=false and reclaimAllowed State != kept dropped
    mutant reclaim-kept KeptNeverReclaimed \
      's/\[\] OTHER +-> FALSE/[] OTHER -> idleExp/'
    # remoteHostLockReclaimScript deletes without the exact content+leaseVersion check
    mutant reclaim-without-cas NoReclaimOverLiveSession \
      's/IF lockOwner = None \\\/ writeSeq = obsSeq\[c\]/IF TRUE/'
    # remoteHostLockRemoveScript deletes without the exact-content compare
    mutant release-without-cas SessionUnderOwnersLock \
      's/(Release\(c\) ==\n.*?\n)  \/\\ IF Mine\(c\) THEN RemoveLock/$1  \/\\ IF TRUE THEN RemoveLock/s'
    # ggMayaSessiondBroker.StopSession's session-id check is not a mutant: with
    # the fence, a foreign live session cannot coexist with our lock here. Go
    # keeps it for what the model omits (standing session, Maya restarts).
    # StartFreshSession stops and starts without the Host Lock fence
    mutant start-without-fence SessionUnderOwnersLock \
      's/(StartSession\(c\) ==\n.*?)IF FenceOK\(c\)/$1IF TRUE/s' MC.unfenced.cfg
    # RunScenario drives Maya without the Host Lock fence
    mutant step-without-fence NoForeignSessionTouch \
      's/(ScenarioStep\(c\) ==\n.*?)IF FenceOK\(c\)/$1IF TRUE/s' MC.unfenced.cfg
    # StopSession stops after its status check without the Host Lock fence
    mutant stop-without-fence NoForeignSessionTouch \
      's/(StopFenced\(c\) ==\n.*?)IF FenceOK\(c\)/$1IF TRUE/s' MC.unfenced.cfg
    ;;
  *) echo "usage: $0 [--quick | --only-safety [pause|nopause|unfenced]]" >&2; exit 2 ;;
esac

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
