---
name: verify-run-ledger
description: "Check changes to the Run Ledger: event identity (sequence numbers, retention gap markers, Windows Host Agent progress and terminal merges) via Go property tests, and multi-file writers (crash points, withRunLedgerLock discipline, local-events sync, transition-store Replace, follow-stream Refresh) via the TLA+ model in verification/run-ledger. Use when a diff touches run_ledger*.go, mergeHostAgent*Events, acceptHostAgentCompletion, serveHostAgentFail, quarantineHostAgentAssignmentLocked, hostAgentTransitionStore.apply, or serveControlPlaneEventStream refresh."
---

# Verify Run Ledger event identity

A Windows Host Agent sends bounded snapshots of its run's events. The Control
Plane merges each one into the stored Run Ledger (`serveHostAgentProgress`), and
the terminal events too (`acceptHostAgentCompletion`). Clients stream the stored
events by sequence number (ADR 0048). `verification/run-ledger/` runs the real Go
merge and bounding code over every honest Agent run up to a bound, and over
hostile or buggy checkpoints, and checks the stored stream after each merge.

## What it promises

For every stream the Control Plane stores after accepting a merge:
- `WellFormed`: sequences start at 1 and rise by one, except across at most one
  `run-ledger.events.truncated` marker whose `firstOmittedSequence` and
  `lastOmittedSequence` name exactly the sequences it stands for.
  Harm if broken: the next checkpoint and every reader reject the stream; the
  run's history is unreadable.
- `IdentityStable`: an acknowledged sequence keeps its payload.
  Harm if broken: a reconnecting client sees different content for the same
  event identity.
- `NoRestore`: a sequence inside a published gap never reappears.
  Harm if broken: clients that skipped the gap never see those events, and the
  history disagrees with what was streamed.
- `Monotone`: the last event sequence never goes back. With `WellFormed` this
  also means an acknowledged sequence can only vanish into the gap marker.
  Harm if broken: a stale snapshot erases events clients already have.
- `HonestAccepted` (honest replay only): a well-behaved Agent's checkpoints and
  terminal transfer are never rejected.
  Harm if broken: a healthy run fails with "Windows Host Agent progress changed
  event identity" or a failed completion.

## Does my change touch it?

Yes if the diff changes a function named in the description, the gap-marker
JSON shape, `hostAgentProgressLedgerPolicy`, `resolveRunLedgerPolicy` limits, or
how events are appended (`appendEventRecord`, `newRunEventRecord`). List covered
changes since the last sync:

```sh
git diff --stat 5c955db -- internal/cli/host_agent.go internal/cli/run_ledger.go internal/cli/run_ledger_store.go internal/cli/run.go
```

Then read the hunks: only changes to the functions above matter.

## Run the check

```sh
verification/run-ledger/check.sh              # ~60-80s: properties + mutants
verification/run-ledger/check.sh --props      # ~12s: properties only
verification/run-ledger/check.sh --fuzz 60s   # also fuzz arbitrary incoming bytes
```

Needs the repo's Go toolchain and rsync. The test file is build-tagged
(`ledgerverify`) and sits outside `internal/cli`. The script copies the working
tree to a temp dir, drops the test into `internal/cli` there, and runs
`go test -tags ledgerverify`. Your checkout is never edited. Uncommitted changes
are checked as they are. Without the tag, `go vet ./...` and `go test ./...` see
no buildable files in `verification/run-ledger` and skip it. Never pass
`-tags ledgerverify` to a repo-wide `go vet`/`go test`: the file declares
`package cli` and only builds inside `internal/cli`.

Output:
- `PASS properties` lists scenario counts and canaries. Each canary (retention
  gap stored, gap widened, acknowledged event absorbed into a gap, oversized
  event replaced, new events appended, terminal transfer merged) must be
  non-zero; the test fails on any that is zero, because the pass would mean little.
- `ok mutant:<name>`: a scratch copy with that guard removed fails the named
  property. `substitution matched nothing` means the Go text moved; update the
  perl expression in `check.sh` so it removes the same guard.
- A failure prints the property name, a count by property, and the shortest
  failing scenario: policy, prefix length, checkpoints (each is "Agent has
  events 1..k"), and the stored, incoming, and merged streams.

Bounds via env: `LEDGER_VERIFY_MAX_EVENTS` (default 9) and
`LEDGER_VERIFY_MAX_PREFIX` (default 3). `LEDGER_VERIFY_RUN` selects tests (a
`go test -run` pattern). `--fuzz` can find more inputs that break the same
properties; keep any new failing input as a named case in
`TestLedgerVerifyKeyCaseIdentity` or a new test.

## Known finding (open at sync commit)

Filed as https://github.com/BramVR/maya_stall/issues/173 (open; still
reproduces at `5c955db`). Expected `check.sh` result until it is fixed: exit 1,
`FAIL properties:` whose only failing test is `TestLedgerVerifyKeyCaseIdentity`
(`6 scenarios failed map[WellFormed:6]`, shortest `case-only type key`;
`TestLedgerVerifyHonestAgent` and `TestLedgerVerifyAdversarialCheckpoint` show
`--- PASS` with all six canaries non-zero), every `ok   mutant:` line present,
and the final `1 check(s) failed`. Any other failure, or a second failed check,
is a real failure. `--props` behaves the same way.
`mergeHostAgentProgressEvents` (host_agent.go:1505-1510 at `5c955db`, `parse`
inside it) reads `sequence`/`type` through struct tags. Go's
encoding/json matches those keys case-insensitively and keeps the last match.
Readers use the exact keys (`ledgerEventSequence`,
`readControlPlaneEvents`). A checkpoint line `{"sequence":2,"Sequence":4,...}`
is validated as 4 and stored, but readers see 2. Every later checkpoint is then
rejected (`invalid current event order`). `{"sEquence":4,"tYpe":"x"}` is stored
and invisible to readers. Only an authenticated Agent can send this. The honest
path writes exact keys. When it is fixed, this test passes; leave it in as a
regression check, and remove this section.

Mutants run only `TestLedgerVerifyHonestAgent` and
`TestLedgerVerifyAdversarialCheckpoint`, so the open finding can't satisfy them.

## Real bug or test error?

Re-read the failing scenario against the code path it mirrors:
- Honest replay (`lvReplay`): initial ledger = the assigned prefix bounded with
  the Run Ledger policy; Agent raw events = that prefix exactly as
  `installAssignedEventPrefix` writes it, then unsequenced appends; each
  checkpoint = `copyBoundedLedgerEvents` twice with the progress policy
  (`buildHostAgentProgress`, then `boundSanitizedHostAgentProgressArtifacts`);
  terminal = bounded once with the Run Ledger policy (`SyncArtifacts`), then
  `mergeHostAgentTerminalEvents`.
- If the failure needs something real code cannot produce, it is a test error.
  Examples: a progress policy larger than the Run Ledger policy
  (`hostAgentProgressLedgerPolicy` caps it), or a gapped assigned prefix
  (`installAssignedEventPrefix` rejects it). Fix the generator and say why in a
  comment.
- Otherwise it is a finding. Report it with the scenario retold as Agent
  checkpoints and Control Plane responses, with file:line. Do not weaken
  `lvCheckTransition` or narrow the generator to make it pass. Do not change
  product code unless asked.

`lvViewOf`/`lvCheckTransition` encode ADR 0048 independently of the merge code.
Change them only when the ADR's contract changes.

## Update for an intended change

1. If the event or gap-marker shape changes, update `lvViewOf` and the ADR together.
2. If the Agent or Control Plane bounding path changes, update `lvAgentCheckpoint`,
   `lvAgentRaw`, or `lvTerminalStaged` to mirror it.
3. Run `check.sh`. Fix mutant expressions that no longer match.
4. Confirm the generator still mirrors the code at the new commit, then move
   the sync commit here and in the `git diff` command above. Move it only after
   that confirmation, never just because the check passed.

## Coverage record

Sync commit: `5c955db`. Every function below is byte-identical between the
previous sync (`fc665f9`) and `5c955db`; the test compiles and runs unchanged.

Real code exercised (`internal/cli/`):
- host_agent.go: `mergeHostAgentProgressEvents`, `mergeHostAgentTerminalEvents`, `validHostAgentEventGap`
- run_ledger.go: `copyBoundedLedgerEvents`, `readBoundedLedgerEvent`, `normalizeRunLedgerEvent`, `runLedgerOmittedEventCount`, `marshalTruncatedRunLedgerEvent`, `ledgerEventSequence`
Mirrored (not executed): `buildHostAgentProgress`, `boundSanitizedHostAgentProgressArtifacts` (sanitizer treated as identity), `acceptHostAgentCompletion` event path, `installAssignedEventPrefix`, `hostAgentProgressLedgerPolicy`.

Bounds: up to 9 raw events, assigned prefix 1..3, 0..3 checkpoints with
non-decreasing event counts, six reachable policy pairs (maxEvents 3..6, one
byte-bounded at 1,200 bytes with oversized events). Adversarial: honest
checkpoints with one line dropped, duplicated, payload or timestamp rewritten,
sequence shifted, gap widened or narrowed, or bounded with another policy.

Not covered here: multi-file writers and crashes (see the writers model below),
the client stream loop's byte limits, Embedded Mode ledger writes, the path sanitizer.

# Writers model: crash points and ledger-lock discipline

The property tests above check the merge functions one call at a time. The
TLA+ model `verification/run-ledger/RunLedgerWriters.tla` checks how the
*writers* of one Host-Agent-backed run's Control Plane ledger interleave and
crash. Each file write is atomic (`writeRunLedgerBytes`: temp, fsync, rename),
but a multi-file update is not, and some writers skip `withRunLedgerLock`.

Files modelled: `E` ledger `events.jsonl`, `R` ledger `run.json` (active or
terminal), `L` the Control Plane's local Run State `events.jsonl` that
`syncRunLedgerArtifacts` copies from. Event payloads are abstracted to their
origin (`P` prefix, `A` Agent progress, `T` Agent terminal transfer, `Fail`
Control Plane `run.failed`).

## What it promises

- `IdentityStable`: whenever the ledger lock is free (a reader could look; a
  crash frees it), every event sequence a client already received still has
  the same payload (ADR 0048).
  Harm if broken: `attach`/reconnecting clients and `history` disagree about
  what happened; an Agent event a client saw is replaced by a different one.
- `CompletedTerminal`: once the assignment completed and left
  `handler.assignments`, the on-disk `run.json` is terminal.
  Harm if broken: `readControlPlaneRunRecord` falls back to disk, so status,
  history, and `attach` show the run as active forever, and follow streams
  never get `stream-end`.

## Run it

```sh
verification/run-ledger/check-writers.sh   # ~11s wall
```

Same pinned `tla2tools.jar` v1.7.4 as `verification/host-lock`.

- `KNOWN finding reproduces: <property>`: the as-coded model
  (`Writers.code.cfg`) still breaks it. Expected until the findings below are
  fixed. If a line says `NOTE ... now holds`, the code was fixed: confirm, then
  update this section.
- `PASS fixed model [fixed|full]`: the model with both candidate fixes
  (`FixLock`, `FixSync`) passes. Mutants and canaries run on this config.
- `ok canary`: reachable situations (Agent event streamed, completion after a
  fail, completion via terminal transfer, quarantine committed after a fail,
  follow-stream refresh across finishing, crash mid-fail, retry after reset).
- `ok mutant:<name>`: removing that guard breaks the named property.

Read every TLC result with the pstack `/statechart` skill: it retells a
violation as its shortest path and charts the passing model.

## Open findings (sync commit)

Both still reproduce at `5c955db` (re-checked against main's Go, not just the
model); `check-writers.sh` exits 0 with both `KNOWN finding reproduces` lines.
Filed as https://github.com/BramVR/maya_stall/issues/174 (`CompletedTerminal`) and https://github.com/BramVR/maya_stall/issues/175 (`IdentityStable`).

1. `CompletedTerminal`: `hostAgentTransitionStore.apply` (host_agent_transition_store.go)
   and `resetHostAgentFinishing` write `run.json` through
   `runLedgerStore.Replace` without the ledger lock. A follow stream opened
   while the run was queued keeps `refreshTransient` for its whole life
   (`serveControlPlaneEventStream`, control_plane.go:750-795;
   `shouldRefreshControlPlaneEventStream`, control_plane.go:950). Replace
   calls: host_agent_transition_store.go:134,145 and host_agent.go:2002. When
   `finishEarlyFailure` appends to L, its `refreshControlPlaneEventSource` runs
   `refreshRunLedgerArtifacts` (locked read, sync, write). If that read lands
   after `FinalizeAcknowledgedFailure` restored the active record and its write
   lands after `finishHostAgentAssignment`'s Commit(completed), `run.json` goes
   back to `submitted` after the assignment is gone. Reproduced against the real
   functions with an injected pause between read and write.
   Fix: write `run.json` in the transition store under `withRunLedgerLock`, or
   make Refresh compare-and-write; and stop refreshing once the run is
   assignment-backed.
2. `IdentityStable`: `syncRunLedgerArtifacts` copies L over E whenever L's
   line count exceeds E's last sequence (`selectRunLedgerArtifactSource`). For
   an assignment-backed run L only holds the prefix plus Control Plane appends
   (`run.failed` per fail attempt or quarantine), so once fail attempts that
   reset outnumber the acknowledged Agent events, any Refresh (follow stream
   with `refreshTransient`, or `finalizeRunLedgerUnlocked` inside
   `FinalizeAcknowledgedFailure` followed by a crash) replaces acknowledged
   events with local ones. At `fc665f9` the Control Plane deadline sweep
   (`sweepHostAgentDeadlines`, appending `host-lock-expired` and refreshing) was
   another route; main has no such sweep. Shortest trace at `5c955db` (12
   steps): fail attempt, reset, quarantine (`quarantineHostAgentAssignmentLocked`
   Snapshot, then `ensureAcceptedFailureEvidence` appends a second `run.failed`
   to L), follow refresh copies L over E, a client streams sequence 3,
   `preserveAcknowledgedFailure` (run_ledger_store.go:262) writes the older
   Snapshot back and sequence 3 disappears. Longer traces replace an
   acknowledged `A` event the same way.
   Fix: never sync an assignment-backed ledger from L (FixSync in the model).

When a fix lands, move the matching switch's behaviour into the model body,
drop the switch, and make the as-coded config pass.

## Real bug or modelling error?

Retell the trace with the coverage list below. If each step's guard matches the
Go code (and steps the model splits really run in separate lock sections), it
is a finding: report it, keep the property. If a step is impossible in Go
(e.g. a follow-stream refresh after a crash: connections die with the process,
and a reconnect to an assignment-backed run has `refreshTransient = false`),
fix the action. A state judged while the ledger lock is held is invisible to
readers unless a crash follows; that is why `IdentityStable` is guarded by
`lock = "none"`.

## Update for an intended change

Edit the action for the changed function so its guard, lock use, and file
writes match Go; one action = one atomic file write or one locked section that
readers cannot see into. Rerun `check-writers.sh`; fix mutant expressions that
no longer match. Move the sync commit only after confirming the model matches
the code at the new commit.

## Coverage record (writers model)

Sync commit: `5c955db`. Covered changes since then:

```sh
git diff --stat 5c955db -- internal/cli/host_agent.go internal/cli/host_agent_transition_store.go internal/cli/run_ledger.go internal/cli/run_ledger_store.go internal/cli/run_ledger_lock.go internal/cli/control_plane.go internal/cli/fresh_run.go
```

Resync from `fc665f9`: removed `QAppend`/`QRefresh` (the deadline sweep's
`host-lock-expired` append and refresh; `sweepHostAgentDeadlines` is absent on
main) and the `Exp` payload; `QCommit` now ends the quarantine. Canary
`CanaryNoSweepRefresh` became `CanaryNoQuarantineCommitted`. All other actions
match main: their Go functions are unchanged, and the lock functions main
dropped (`withRunDeadlineLock`, `withDeadlineAuthorityLock`) were never modelled.

Actions -> Go (`internal/cli/`):
- `Progress`: `serveHostAgentProgress` -> `runLedgerStore.UpdateSnapshot`
- `FailStart`, `FailSync`, `FailPreserve`, `FailRestore`: `serveHostAgentFail`, `finishEarlyFailure` (fresh_run.go), `runLedgerStore.FinalizeAcknowledgedFailure`, `finalizeRunLedgerUnlocked`, `syncRunLedgerArtifacts`, `preserveAcknowledgedFailure`
- `CompleteCopy`: `acceptHostAgentCompletion` -> `copyHostAgentResultFiles`
- `FinishCommit`, `FinishReset`, `FinishCleanup`, `FinishCompleted`: `finishHostAgentAssignment`, `resetHostAgentFinishing`, `cleanupRunState`, `hostAgentTransitionStore.apply`
- `QStart`, `QSync`, `QPreserve`, `QCommit`: `quarantineHostAgentAssignment` (Agent fail with `Quarantine`, process-session lease expiry, process disappeared, restart quarantine marker) -> `quarantineHostAgentAssignmentLocked`, `ensureAcceptedFailureEvidence`
- `FollowRefreshRead`, `FollowRefreshWrite`: `serveControlPlaneEventStream`, `shouldRefreshControlPlaneEventStream`, `refreshControlPlaneEventSource`, `refreshRunLedgerArtifactsUnlocked`
- `Read`: `runLedgerStore.ReadEvents` / `readControlPlaneEvents`
- `Crash`: process death (flock released, connections dropped), `loadHostAgentState` finishing completion
- File atomicity: `writeRunLedgerBytes`, `syncRunLedgerDirectory`; lock: `withRunLedgerLock`, `withRunLedgerFileLock`

Bounds: fixed = 2 progress events, 3 handler attempts, 1 crash (26k distinct
states); full = 3, 4, 2 (296k distinct states). One run, one Control Plane.

Not covered: Embedded Mode writers (`run`, `stop`, `doctor`, retention sweeps),
`initializeRunLedger` (single directory rename, atomic), `pruneRunLedger` and
`cleanupRunLedgerRecord` (root lock), log content, bounding/truncation (the
property tests above cover it), Windows lock semantics (`run_ledger_lock_windows.go`),
`UpdateSnapshot` crash between events and `run.json` (only record metadata goes
stale; the next write repairs it).
