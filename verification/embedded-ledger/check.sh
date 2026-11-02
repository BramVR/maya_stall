#!/usr/bin/env bash
# Model-check Embedded Mode Run Ledger writers, stop, sweep, and prune.
#
#   verification/embedded-ledger/check.sh               # everything (~1 min)
#   verification/embedded-ledger/check.sh --only-safety # code + fixed configs only
#
# Exit 0 when every expectation below holds. Open findings are expected
# violations of the code config, reported as KNOWN: crashed runs that `stop`
# cannot finish (https://github.com/BramVR/maya_stall/issues/177), Finalize
# overwriting a concurrent stop's terminal ledger with kept (TerminalStable),
# and markKept overwriting another run's Host Lock (LiveRunOwnsLock). Once
# fixed, move the property into MC.code.cfg and drop its KNOWN probe.
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
if [[ "$(shasum -a 256 "$jar" | cut -d' ' -f1)" != "$tla_sha256" ]]; then
  echo "tla2tools.jar sha256 mismatch" >&2
  exit 2
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
failures=0

tlc() { # tlc <dir> <cfg> <log>
  (cd "$1" && java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -workers auto -deadlock \
    -cleanup -metadir "$work/meta.$RANDOM$RANDOM" -config "$2" MC.tla) >"$3" 2>&1
}

copy_model() { # copy_model <dir> [spec-file]
  mkdir -p "$1"
  cp "$here/MC.tla" "$1/"
  cp "${2:-$here/EmbeddedLedger.tla}" "$1/EmbeddedLedger.tla"
}

pass() { # pass <cfg>
  local log="$work/pass.$1.log"
  if tlc "$here" "MC.$1.cfg" "$log"; then
    echo "PASS $1: $(grep -E 'distinct states found' "$log" | tail -1)"
  else
    echo "FAIL $1:"
    sed -n '/Error:/,$p' "$log" | grep -E 'Error|^State [0-9]+:' | sed 's/ line.*//'
    failures=$((failures + 1))
  fi
}

# probe <label> <dir> <base-cfg> <INVARIANT|PROPERTY> <name> [known]
probe() {
  local label="$1" dir="$2" base="$3" kind="$4" name="$5" known="${6:-}" log="$work/$1.$5.log"
  sed -e '/^INVARIANTS/,$d' "$here/MC.$base.cfg" >"$dir/probe.cfg"
  if [[ "$kind" == PROPERTY ]]; then
    printf 'PROPERTIES\n  %s\n' "$name" >>"$dir/probe.cfg"
  else
    printf 'INVARIANTS\n  %s\n' "$name" >>"$dir/probe.cfg"
  fi
  if tlc "$dir" probe.cfg "$log"; then
    echo "FAIL $label: $name was never violated"
    failures=$((failures + 1))
  elif grep -q "$name is violated" "$log"; then
    if [[ -n "$known" ]]; then
      echo "KNOWN $label: $name violated (open finding). Trace:"
      grep -E '^State [0-9]+:' "$log" | sed -e 's/ line.*//' -e 's/^/    /'
    else
      echo "ok   $label: $name violated as expected"
    fi
  else
    echo "FAIL $label: TLC error other than $name"
    tail -20 "$log"
    failures=$((failures + 1))
  fi
}

mutant() { # mutant <name> <base-cfg> <INVARIANT|PROPERTY> <prop> <perl-expr>
  local name="$1" dir="$work/mutant.$1"
  mkdir -p "$dir"
  perl -0pe "$5" "$here/EmbeddedLedger.tla" >"$dir/spec.tla"
  if cmp -s "$here/EmbeddedLedger.tla" "$dir/spec.tla"; then
    echo "FAIL mutant $name: substitution matched nothing; update check.sh"
    failures=$((failures + 1))
    return
  fi
  copy_model "$dir" "$dir/spec.tla"
  probe "mutant:$name" "$dir" "$2" "$3" "$4"
}

pass code
pass fixed
if [[ "${1:-}" != --only-safety ]]; then
  pass fixed-full

  known="$work/known"
  copy_model "$known"
  probe "finding" "$known" orphan INVARIANT CrashedRunRecoverable known
  probe "finding" "$known" orphan INVARIANT CrashedRunRecoverableWithRecord known
  probe "finding" "$known" orphan INVARIANT CrashedRunRecoverableAfterStop known
  probe "finding" "$known" orphan PROPERTY TerminalStable known
  probe "finding" "$known" orphan INVARIANT LiveRunOwnsLock known

  canaries="$work/canaries"
  copy_model "$canaries"
  for canary in CanaryNoCompletedRun CanaryNoPrune CanaryNoCrashRecoveryStop \
    CanaryNoLockTakeover CanaryNoSweepRecovery CanaryNoStopDuringFinish; do
    probe "canary" "$canaries" code INVARIANT "$canary"
  done

  # pruneRunLedgerUnlocked expires cleanup-failed records too
  mutant prune-expires-cleanup-failed code INVARIANT PruneKeepsUnfinished \
    's/ledger\[y\] = "terminal" THEN "none"/ledger[y] \\in {"terminal", "cleanup"} THEN "none"/'
  # reconcileSubmittedRunLedgerForStop and stopKeptRunAfterBrokerStop skip
  # requireStaleSubmittedRunController
  mutant stop-skips-controller-check code INVARIANT LiveRunIntact \
    's/(lockKind = "active") \/\\ Dead\(x\)/$1/g'
  # sweepKeptSessions expires a kept run before its keep deadline
  mutant sweep-ignores-deadline code INVARIANT SweepOnlyExpired \
    's/(rs\[y\] \\in \{"running", "kept"\}) \/\\ due\[y\]/$1/'
  # updateRunLedgerAfterStopUnlocked drops its kept/cleanup-failed guard
  mutant stop-failure-overwrites-terminal fixed PROPERTY TerminalStable \
    's/IF ledger\[x\] = "terminal" THEN "terminal" ELSE "cleanup"/"cleanup"/'
fi

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
