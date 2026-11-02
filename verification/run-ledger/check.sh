#!/usr/bin/env bash
# Property-check Run Ledger event identity across Windows Host Agent
# checkpoints, against the real Go code.
#
#   verification/run-ledger/check.sh              # properties + mutants
#   verification/run-ledger/check.sh --props      # properties only
#   verification/run-ledger/check.sh --fuzz 60s   # also fuzz arbitrary incoming bytes
#
# The test file is build-tagged and lives outside internal/cli, so the script
# copies the working tree to a scratch dir, drops the test into internal/cli
# there, and runs `go test -tags ledgerverify`. The checkout is never edited.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

snapshot() {
  local dest="$1"
  mkdir -p "$dest"
  rsync -a --exclude .git --exclude artifacts --exclude bin --exclude dist \
    --exclude .maya-stall --exclude node_modules \
    --exclude verification --exclude .agents --exclude .claude "$repo/" "$dest/"
  cp "$here/ledger_verify_test.go" "$dest/internal/cli/zz_ledger_verify_test.go"
}

# run_props <dir> <log>: properties on the code in <dir>.
run_props() {
  (cd "$1" && go test -tags ledgerverify -count=1 -run "${LEDGER_VERIFY_RUN:-TestLedgerVerify}" -v ./internal/cli) >"$2" 2>&1
}

failures=0

properties() {
  snapshot "$work/base"
  if run_props "$work/base" "$work/base.log"; then
    echo "PASS properties: $(grep -hE 'scenarios checked|merges checked|canary reached' "$work/base.log" | sed 's/^ *//' | tr '\n' ' ')"
  else
    echo "FAIL properties:"
    grep -vE '^(=== RUN|PASS|ok|FAIL\s+github)' "$work/base.log" | head -80
    failures=$((failures + 1))
  fi
}

# mutant <name> <property> <file> <perl substitution>: break one guard in a
# scratch copy of the Go code; the named property must fail.
mutant() {
  local name="$1" prop="$2" file="$3" expr="$4" dir="$work/mutant.$1"
  snapshot "$dir"
  perl -0pi -e "$expr" "$dir/internal/cli/$file"
  if cmp -s "$repo/internal/cli/$file" "$dir/internal/cli/$file"; then
    echo "FAIL mutant $name: substitution matched nothing; update check.sh"
    failures=$((failures + 1))
    return
  fi
  # Mutants run the clean tests only, so a known finding cannot satisfy them.
  if LEDGER_VERIFY_RUN='TestLedgerVerify(HonestAgent|AdversarialCheckpoint)$' run_props "$dir" "$dir.log"; then
    echo "FAIL mutant $name: properties still pass"
    failures=$((failures + 1))
  elif grep -q "$prop" "$dir.log"; then
    echo "ok   mutant:$name: $prop fails as expected"
  else
    echo "FAIL mutant $name: failed without $prop"
    grep -vE '^(=== RUN)' "$dir.log" | head -20
    failures=$((failures + 1))
  fi
}

mode="${1:-all}"
case "$mode" in
  --props) properties ;;
  --fuzz)
    properties
    (cd "$work/base" && go test -tags ledgerverify -run '^$' -fuzz FuzzLedgerVerifyMerge -fuzztime "${2:-60s}" ./internal/cli) ||
      failures=$((failures + 1))
    ;;
  all)
    properties
    # mergeHostAgentProgressEvents stops comparing payloads of acknowledged sequences
    mutant skip-payload-compare IdentityStable host_agent.go \
      's/existing\[event.sequence\]; ok && event.eventType/existing[event.sequence]; false \&\& ok \&\& event.eventType/'
    # mergeHostAgentProgressEvents stops checking that sequences rise by one
    mutant skip-order-check WellFormed host_agent.go \
      's/err != nil \|\| event.sequence != expectedSequence/err != nil || false \&\& event.sequence != expectedSequence/'
    # mergeHostAgentProgressEvents lets a snapshot restore sequences inside a published gap
    mutant allow-gap-restore NoRestore host_agent.go \
      's/(event.sequence >= currentGapFirst && event.sequence <= currentGapLast && currentGapFirst > 0)/false \&\& $1/'
    # mergeHostAgentProgressEvents accepts a stale snapshot
    mutant accept-stale Monotone host_agent.go \
      's/(lastActualSequence\(incomingEvents\) < lastActualSequence\(currentEvents\))/false \&\& $1/'
    # mergeHostAgentTerminalEvents keeps the narrower gap instead of the widest
    mutant terminal-narrow-gap HonestAccepted host_agent.go \
      's/if incomingGapLast > gapLast \{/if false {/'
    # copyBoundedLedgerEvents forgets earlier omissions when re-bounding
    mutant rebound-forgets-gap HonestAccepted run_ledger.go \
      's/total \+= previouslyOmitted/total += 0*previouslyOmitted + 1/'
    ;;
  *) echo "usage: $0 [--props | --fuzz <duration>]" >&2; exit 2 ;;
esac

if [[ "$failures" -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
