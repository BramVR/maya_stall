#!/usr/bin/env bash
# Model-check the local file Host Lock (repo lock + withLocalHostSideMutex).
#
#   verification/local-host-lock/check.sh                # everything (~15s)
#   verification/local-host-lock/check.sh --only-safety  # ssh + local-fixed bounds
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
failures=0

# copy_model <dir>: fresh copy of the model and configs into a temp dir.
copy_model() {
  mkdir -p "$1"
  cp "$here"/*.tla "$here"/*.cfg "$1/"
}

tlc() {
  local dir="$1" cfg="$2" log="$3"
  (cd "$dir" && java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -workers auto -deadlock \
    -cleanup -metadir "$work/meta.$RANDOM" -config "$cfg" MC.tla) >"$log" 2>&1
}

safety() {
  local cfg="$1" log="$work/safety.$1.log"
  if tlc "$here" "MC.$cfg.cfg" "$log"; then
    echo "PASS safety [$cfg]: $(grep -E 'distinct states found' "$log" | tail -1)"
  else
    echo "FAIL safety [$cfg]: see counterexample below"
    sed -n '/Error:/,$p' "$log"
    failures=$((failures + 1))
  fi
}

# expect_violation <label> <dir> <base cfg> <invariant>
expect_violation() {
  local label="$1" dir="$2" base="$3" inv="$4" log="$work/${1//[:\/]/_}.log"
  sed -e '/^INVARIANTS/,$d' "$dir/MC.$base.cfg" >"$dir/probe.cfg"
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

# mutant <name> <base cfg> <invariant> <perl substitution>
mutant() {
  local name="$1" base="$2" inv="$3" expr="$4" dir="$work/mutant.$1"
  copy_model "$dir"
  perl -0pe "$expr" "$here/LocalHostLock.tla" >"$dir/LocalHostLock.tla"
  if cmp -s "$here/LocalHostLock.tla" "$dir/LocalHostLock.tla"; then
    echo "FAIL mutant $name: substitution matched nothing; update check.sh"
    failures=$((failures + 1))
    return
  fi
  expect_violation "mutant:$name" "$dir" "$base" "$inv"
}

case "${1:-all}" in
  --only-safety)
    for cfg in ssh ssh-full local-fixed local-fixed-full; do safety "$cfg"; done
    ;;
  all)
    for cfg in ssh ssh-full local-fixed local-fixed-full; do safety "$cfg"; done

    # Known finding: with the local file lock as the only Host Lock, one crash
    # inside withLocalHostSideMutex lets two processes break the stale mutex
    # and the stale lock at once (read-stale, then os.Remove(path)).
    known="$work/known"
    copy_model "$known"
    expect_violation "finding:stale-break-toctou" "$known" local HolderFileIntact

    canaries="$work/canaries"
    copy_model "$canaries"
    for canary in CanaryNoKeptLeftBehind CanaryNoStaleLockTakeover CanaryNoStaleMutexBreak \
      CanaryNoKeptStop CanaryNoHostReuse; do
      expect_violation "canary" "$canaries" ssh "$canary"
    done

    # acquireRunHostLock takes the local lock without the host-side lock first
    mutant local-before-remote ssh HolderFileIntact \
      's/(Start\(p\) ==\n.*?\n\s+\/\\ IF )Mode = "ssh"/$1FALSE/s'
    # isStaleHostLock treats a keptRun lock as stale
    mutant kept-is-stale local-fixed LockExclusive \
      's/IF lockKind # "kept" \/\\ ~alive\[lockPid\]/IF lockKind = "kept" \\\/ ~alive[lockPid]/'
    # isStaleHostLock ignores whether the owner pid is alive
    mutant live-pid-is-stale local-fixed HolderFileIntact \
      's/IF lockKind # "kept" \/\\ ~alive\[lockPid\]/IF lockKind # "kept"/'
    # releaseHostSideLock frees the host-side kept lock but stop never checks the local owner
    mutant stop-without-owner-check local-fixed-full HolderFileIntact \
      's/IF lockRun = target\[p\] THEN ClearLock/IF TRUE THEN ClearLock/'
    ;;
  *) echo "usage: $0 [--only-safety]" >&2; exit 2 ;;
esac

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
