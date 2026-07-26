# Concepts

Read when:

- you encounter a Maya Stall term you do not recognize;
- you are writing docs, issues, or PRs and want consistent vocabulary;
- you need the product nouns in one place.

This page is a glossary. For deeper domain wording, see the root
[`CONTEXT.md`](../CONTEXT.md).

## Product Boundary

**Maya Stall** - release-qualification system for running and proving real
Autodesk Maya UI end-to-end checks across owned Windows Maya Hosts.

**maya-stall** - the command-line binary.

**Consuming Repo** - a repo that supplies non-secret Scenario config, Run
Payload paths, Maya Scripts, Plugin Artifacts, and Expected Outputs.

**Repo Run Config** - `.maya-stall.yaml` or `maya-stall.yaml`. Stores Scenario
definitions and must not store Host Credentials or private infrastructure.

**Crabbox** - upstream remote execution control plane used as a reference for
static SSH, target profiles, stop policy, visual evidence, artifacts, and review
publishing. Maya Stall is not a Crabbox fork.

## Targets And Hosts

**Target Profile** - a named target environment selected for a run. It maps to a
Host Pool in external host config.

**Host Pool** - a set of selectable Maya Hosts.

**Control Plane** - the shared Maya Stall service that records submitted runs,
schedules them across Host Pools, owns shared Host Locks, and keeps run history.

**Embedded Mode** - the default single-controller mode. The CLI executes the
Scenario and keeps its Run Ledger and Evidence Bundle in the Consuming Repo.

**Configured Control Plane Mode** - shared mode selected with an external
Control Plane HTTPS URL and bearer-token environment variable. The Control
Plane owns execution and durable run records; Repo Run Config does not change.

**Maya Host** - an owned physical or virtual Windows machine with one isolated
interactive desktop, Autodesk Maya, a writable work root, and the services
needed to provide one Maya Stall execution slot.

**Host Credentials** - secrets and identity material needed to reach or use a
Maya Host. Keep them outside Repo Run Config.

**Windows Host Agent** - the Maya Stall-owned service on a Maya Host that
enforces the Host Lock, prepares workspaces, monitors work, transfers evidence,
and coordinates cleanup with the Session Broker.

**Maya Version Requirement** - the Autodesk Maya version a Scenario needs.

**Host Health** - layered readiness checks for config, SSH, work root, Session
Broker, Maya version, Visual Evidence, Host Lock, and Scenario inputs.

**Maya Host Capability Record** - a versioned, timestamped Windows Host Agent
report covering Maya/Python/Session Broker versions and the Host features used
by planning and scheduling. Incomplete or stale records never qualify.

**Host Lock** - a durable shared claim with a unique lock token, heartbeat,
idle deadline, and hard deadline that prevents more than one active or kept
Maya UI run from using the same Maya Host at the same time.

**Host Lock Heartbeat** - a token-fenced active Run or Agent sign of life that
moves the idle deadline without extending the hard lifetime.

## Run Lifecycle

**Scenario** - a named Maya end-to-end flow in Repo Run Config.

**Fresh Run** - a run that starts from a clean Maya UI Session and clean
workspace.

**Run ID** - the stable identity created for a submitted Scenario before config
validation, host selection, or remote preflight.

**Run Ledger** - durable bounded metadata, ordered lifecycle events, and
retained logs for every accepted Run ID. It survives transient Run State
cleanup and does not replace Evidence Bundles.

**Maya UI Session** - an interactive Autodesk Maya desktop process used for a
run. Raw SSH-launched service-session Maya is not accepted as UI proof.

**Debug Attach** - deliberate reuse of an existing Maya UI Session for
investigation.

**Run Attach** - read-only access to a run's events and Session Broker log via
`maya-stall attach`.

**Kept Session** - a session intentionally left open after a run for debugging
until explicit stop or its durable deadline.

**Stop Policy** - the cleanup rule that decides whether Maya Stall stops or
keeps a Maya UI Session after a run.

## Payloads And Results

**Run Payload** - repo-owned inputs staged for a Scenario: Plugin Artifacts,
Maya Scripts, scenes, Expected Outputs, and include paths.

**Plugin Artifact** - a built Maya plugin file or related loadable binary.

**Maya Script** - repo-owned script that drives Maya behavior inside the UI
Session.

**Expected Output** - repo-owned artifact or value that defines successful
Scenario behavior.

**Scenario Result** - structured JSON written by a Scenario with status,
summary, assertions, measurements, and produced outputs.

**Validator** - reusable Maya Stall check that compares run outputs against
Expected Outputs.

## Evidence And Publishing

**Evidence Bundle** - logs, screenshots, recordings, metadata,
Scenario Result, and output files returned from a run.

**Visual Evidence** - screenshots or recordings captured from the Maya UI
Session.

**Evidence Store** - durable filesystem or network location where published
Evidence Bundles are kept.

**Review Comment** - published summary that links an Evidence Bundle into a code
review system such as GitHub or GitLab.

## Session Broker

**Session Broker** - Windows-side service that launches, owns, or attaches to
Maya UI Sessions for Maya Stall.

**Maya Session Daemon** - first Session Broker implementation used by Maya
Stall, currently represented by `gg_mayasessiond`.
