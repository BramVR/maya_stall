#!/usr/bin/env bash
# Model-check the Run Ledger writers of one Host-Agent-backed run (crash points,
# ledger lock discipline, local-events sync). See RunLedgerWriters.tla.
#
#   verification/run-ledger/check-writers.sh      # findings, fixed model, canaries, mutants (~20s)
#
# Exit 0 only when every line is as expected. While the two open findings are
# unfixed, "KNOWN finding reproduces" lines are expected and do not fail.
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

# stage <dir> [perl expr]: copy the model into dir, optionally mutated.
stage() {
  mkdir -p "$1"
  cp "$here/MCWriters.tla" "$here"/Writers.*.cfg "$1/"
  if [[ -n "${2:-}" ]]; then
    perl -0pe "$2" "$here/RunLedgerWriters.tla" >"$1/RunLedgerWriters.tla"
  else
    cp "$here/RunLedgerWriters.tla" "$1/"
  fi
}

# tlc <dir> <cfg> <log>
tlc() {
  (cd "$1" && java -XX:+UseParallelGC -cp "$jar" tlc2.TLC -workers auto -deadlock \
    -cleanup -metadir "$work/meta.$RANDOM" -config "$2" MCWriters.tla) >"$3" 2>&1
}

# probe <dir> <base cfg> <invariant> <log>: run base config with one invariant.
probe() {
  sed -e '/^INVARIANTS/,$d' "$1/$2" >"$1/probe.cfg"
  printf 'INVARIANTS\n  %s\n' "$3" >>"$1/probe.cfg"
  tlc "$1" probe.cfg "$4"
}

base="$work/base"
stage "$base"

# Open findings on the model of the code at the sync commit.
for inv in IdentityStable CompletedTerminal; do
  log="$work/known.$inv.log"
  if probe "$base" Writers.code.cfg "$inv" "$log"; then
    echo "NOTE $inv now holds on the as-coded model: finding fixed? update the skill"
  elif grep -q "Invariant $inv is violated" "$log"; then
    echo "KNOWN finding reproduces: $inv ($(grep -c '^State ' "$log") steps)"
  else
    echo "FAIL code model: TLC error"; tail -20 "$log"; failures=$((failures + 1))
  fi
done

# Candidate fixes (FixLock, FixSync) must pass at both bounds.
for bound in fixed full; do
  log="$work/safety.$bound.log"
  if tlc "$base" "Writers.$bound.cfg" "$log"; then
    echo "PASS fixed model [$bound]: $(grep -E 'distinct states found' "$log" | tail -1)"
  else
    echo "FAIL fixed model [$bound]"; sed -n '/Error:/,$p' "$log"; failures=$((failures + 1))
  fi
done

expect_violation() {
  local label="$1" dir="$2" inv="$3" log="$work/$1.$3.log"
  if probe "$dir" Writers.fixed.cfg "$inv" "$log"; then
    echo "FAIL $label: $inv was never violated"; failures=$((failures + 1))
  elif grep -q "Invariant $inv is violated" "$log"; then
    echo "ok   $label: $inv violated as expected"
  else
    echo "FAIL $label: TLC error"; tail -20 "$log"; failures=$((failures + 1))
  fi
}

for canary in CanaryNoAgentEventStreamed CanaryNoCompletionAfterFail CanaryNoCompletionViaTransfer \
  CanaryNoQuarantineCommitted CanaryNoFollowRefreshDuringFinish CanaryNoCrashMidFail CanaryNoRetryAfterReset; do
  expect_violation canary "$base" "$canary"
done

mutant() {
  local name="$1" inv="$2" expr="$3" dir="$work/mutant.$1"
  stage "$dir" "$expr"
  if cmp -s "$here/RunLedgerWriters.tla" "$dir/RunLedgerWriters.tla"; then
    echo "FAIL mutant $name: substitution matched nothing; update check-writers.sh"
    failures=$((failures + 1)); return
  fi
  expect_violation "mutant:$name" "$dir" "$inv"
}

# preserveAcknowledgedFailure ignores the acknowledged Snapshot
mutant preserve-drops-acknowledged IdentityStable \
  's/Preserve\(a\) == IF HasFail\(a\) THEN a ELSE Append\(a, "Fail"\)/Preserve(a) == <<"P", "Fail">>/'
# syncRunLedgerArtifacts copies local Run State events over an Agent-backed ledger
mutant sync-from-local IdentityStable \
  's/Len\(l\) > Len\(e\) \/\\ ~FixSync THEN/Len(l) > Len(e) THEN/'
# refreshRunLedgerArtifacts drops withRunLedgerLock between read and write
mutant refresh-releases-lock CompletedTerminal \
  's/lock\x27 = "refresh" \/\\ rr\x27 = R/lock\x27 = "none" \/\\ rr\x27 = R/'
# transition-store Replace of run.json stays outside the ledger lock
mutant commit-unlocked CompletedTerminal \
  's/UnlockedWriteOK == FixLock => lock = "none"/UnlockedWriteOK == TRUE/'

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"; exit 1
fi
echo "all checks as expected (open findings: see .agents/skills/verify-run-ledger)"
