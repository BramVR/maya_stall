# Static Evidence Bundle report rationale

## Problem

An Evidence Bundle has the canonical Scenario Result, evidence metadata, events,
logs, artifacts, and cleanup facts, but callers currently assemble their own
small projections. The static report must stay deterministic and offline, avoid
making HTML authoritative, and work for old bundles with missing additive
fields. Finalization must also hash the report without asking the report to
contain its own digest.

## Usage (caller's view)

The run finalizer supplies the terminal lifecycle facts after cleanup settles:

```go
view, err := finalizeEvidenceReport(bundleDir, reportTerminalState{
	Lifecycle: "completed",
	Cleanup:   "completed",
	Next:      "maya-stall result " + runID,
})
```

The terminal command renders the same projection instead of deriving another
verdict:

```go
view := reportViewFromOutcome(outcome)
printRunOutcome(stdout, outcome, view)
```

An existing bundle can be rendered elsewhere without changing the bundle:

```sh
maya-stall evidence report --output /tmp/report.html artifacts/maya-stall/<run-id>
```

## Shape

`report.go` owns `reportView`, its version-tolerant loader, failure
classification, bounded excerpts, safe artifact links, and deterministic HTML.
Callers pass either a terminal `runOutcome` or an Evidence Bundle directory and
receive the same typed view. The renderer accepts only that view.

Evidence finalization writes canonical JSON first, builds the view, writes and
hashes `report.html`, then adds the report's path, media type, size, and hash to
the bundle and run manifest. The view deliberately ignores the report artifact
when building its own inventory. This keeps the digest one-way and makes a later
read-only render byte-identical.

The module is deep enough to justify its three entry points. It hides schema
tolerance, path checks, escaping, size limits, deterministic ordering, and
failure classification. Callers do not coordinate those steps.

## Synthesis decision

Candidate A, the bundle-owned projection module, is the base. It keeps report
policy beside Evidence Bundle knowledge and gives terminal output a small
adapter into the same view.

Candidate B started from Run Ledger events and joined the Scenario Result,
Evidence Bundle, manifest, cleanup record, and publication record at each call
site. That shape exposed storage order to callers, repeated missing-field rules,
and split one report operation into load, merge, classify, and render modules.
It was rejected for information leakage, temporal decomposition, and
pass-through methods.

## Tradeoffs accepted

- We accept additive lifecycle fields in `evidence.json` in exchange for making
  retained and cleanup-failed states explicit in old and new readers.
- We accept one compact HTML template in Go in exchange for deterministic,
  dependency-free offline output.
- We accept omission of the report's own digest from report content in exchange
  for a non-circular manifest.

## Alternatives considered

- Event-first aggregation exposed five record formats to every caller and made
  a future timeline simple at the cost of making today's finalizer and CLI
  understand storage internals.
- A separate HTML result model hid template details but duplicated verdict,
  failure, and count rules already needed by terminal JSON.

## Open questions and risks

- Which future issue should expand the current run manifest into the complete
  offline-verification contract from issue 124?
- Which Scenario Result step schema will issue 128 choose beyond the tolerant
  optional fields consumed here?

## Next implementation step

Add the typed projection and malicious-input tests before wiring finalization.
