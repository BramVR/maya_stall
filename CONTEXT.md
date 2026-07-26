# Maya Stall

Maya Stall is the shared release-qualification system and language for real Autodesk Maya desktop UI end-to-end testing from external repositories. It exists to keep Maya UI test orchestration separate from any one plug-in repo.

## Language

**Maya Stall**:
A release-qualification system for running and proving real Maya UI end-to-end checks across owned Windows Maya Hosts.
_Avoid_: Plugin test code, batch test runner, gg_klv_push test harness

**maya-stall**:
The command-line binary for Maya Stall.
_Avoid_: gg_maya_stall when referring to the command users run

**Consuming Repo**:
A repository that supplies Maya Stall with non-secret run configuration, test payloads, and build artifacts.
_Avoid_: Client, plugin repo when the repository might not be a plugin

**Target Profile**:
A named target environment for a Maya UI run, including the machine and runner identity implied by external credentials.
_Avoid_: Host, machine, profile when only one part of the target is meant

**Host Pool**:
A set of Maya Hosts that a Target Profile may choose from for a run.
_Avoid_: Target profile when referring to the individual selectable machines

**Control Plane**:
The shared Maya Stall service that records submitted runs, schedules them across Host Pools, owns shared Host Locks, and keeps run history.
_Avoid_: Session Broker, Maya Host

**Embedded Mode**:
The single-controller mode where the `maya-stall` CLI owns execution and stores the Run Ledger and Evidence Bundle in the Consuming Repo checkout.
_Avoid_: Control Plane mode, remote mode

**Configured Control Plane Mode**:
The shared mode selected with an external authenticated Control Plane URL, where the service owns the submitted run and durable records without changing Repo Run Config.
_Avoid_: Embedded mode, direct SSH mode

**Repo Run Config**:
Non-secret configuration supplied by a consuming repo for Maya Stall runs.
_Avoid_: Secrets config, user config

**Scenario**:
A named Maya end-to-end flow in Repo Run Config, with its own Run Payload, Expected Outputs, and evidence policy.
_Avoid_: Test when referring to the configured end-to-end flow

**Host Credentials**:
Secrets and identity material needed to reach or use a Maya Host.
_Avoid_: Target profile, repo config

**Windows Host Agent**:
The Maya Stall-owned service on a Maya Host that enforces the Host Lock, prepares run workspaces, monitors work, transfers evidence, and coordinates cleanup with the Session Broker.
_Avoid_: Session Broker when referring to Maya UI Session ownership

**Maya Host**:
A local, network, or virtual Windows machine with one isolated interactive desktop, Autodesk Maya, and the services needed to provide one Maya Stall execution slot.
_Avoid_: Cloud runner, headless worker

**Maya Version Requirement**:
The Autodesk Maya version a Scenario needs in order to run correctly.
_Avoid_: Host capability when referring specifically to Maya version compatibility

**Host Lock**:
A durable shared claim with a unique lock token, last heartbeat, idle deadline, and hard deadline that prevents more than one active or kept Maya UI run from using the same Maya Host at the same time.
_Avoid_: Repo-local lock, session lock when the machine-level run claim is meant

**Host Lock Heartbeat**:
A token-fenced sign of life from active Run progress or its Windows Host Agent that advances the Host Lock idle deadline without changing its hard lifetime.
_Avoid_: Agent process-session lease when the Maya Host reservation deadline is meant

**Host Health**:
The readiness of a Maya Host to accept a run, checked in layers such as SSH, workspace access, Session Broker, Maya UI, Visual Evidence, and Scenario inputs.
_Avoid_: Status when only the current lock or run state is meant

**Maya Host Capability Record**:
A versioned, timestamped Windows Host Agent report of the Maya builds, runtime features, and eligibility state used by planning and scheduling.
_Avoid_: Host Health when referring to compatibility rather than live readiness layers

**Maya UI Session**:
An interactive Autodesk Maya desktop process used for a run.
_Avoid_: Batch session, headless session

**Fresh Run**:
A Maya Stall run that starts from a clean Maya UI Session.
_Avoid_: Reused session, warm session

**Run ID**:
The stable identity created for a submitted Scenario before config validation, host selection, or remote preflight.
_Avoid_: Maya process ID, workspace path

**Run Ledger**:
Durable bounded metadata, lifecycle events, and retained logs for every accepted Run ID, independent from transient Run State and Evidence Bundles.
_Avoid_: Run State, Evidence Store

**Debug Attach**:
A Maya Stall run that intentionally reuses an existing Maya UI Session for investigation.
_Avoid_: Fresh run, CI run

**Run Attach**:
A read-only connection to an active or kept run's events and logs.
_Avoid_: Debug Attach, UI viewer

**Kept Session**:
A Maya UI Session intentionally left open after a run for debugging until its durable keep deadline, explicit authorized stop, or Host Lock deadline.
_Avoid_: Leaked session, active run

**Stop Policy**:
The cleanup rule that decides whether Maya Stall stops or keeps a Maya UI Session after a run.
_Avoid_: Cleanup when only the run-retention rule is meant

**Run Payload**:
The repo-owned tests, scripts, scenes, expected outputs, and build artifacts that a consuming repo gives Maya Stall to execute.
_Avoid_: Test bundle unless packaging is specifically meant

**Plugin Artifact**:
A built Maya plugin file or related loadable binary supplied by a consuming repo.
_Avoid_: Package when the loadable file itself is meant

**Maya Script**:
A script supplied by a consuming repo to drive Maya behavior inside a Maya UI Session.
_Avoid_: Shell command, payload command

**Expected Output**:
A repo-owned artifact or value that defines what a successful Maya run should produce.
_Avoid_: Golden file unless file comparison is specifically meant

**Scenario Result**:
A structured result written by a Scenario to report status, assertions, measurements, and produced outputs.
_Avoid_: Evidence Bundle, log output

**Validator**:
A reusable Maya Stall check that compares run outputs against Expected Outputs.
_Avoid_: Test when the consuming repo owns the scenario logic

**Evidence Bundle**:
The logs, screenshots, scenes, output files, and other artifacts returned from a run to prove what happened.
_Avoid_: Results when only pass/fail status is meant

**Visual Evidence**:
Screenshots or screen recordings captured from the Maya UI Session during a run.
_Avoid_: Evidence Bundle when non-visual logs and files are also meant

**Visual Evidence Provenance**:
The recorded origin (Session Broker capture, fake-broker capture, or discovered file) and SHA256 content hash of each Visual Evidence artifact in an Evidence Bundle.
_Avoid_: Attestation, signature when only origin metadata and content hashes are meant

**Review Comment**:
A published summary that links an Evidence Bundle into a code review system.
_Avoid_: PR comment when the hosting platform might not be GitHub

**Evidence Store**:
A durable location where published Evidence Bundles are kept for reviewers.
_Avoid_: Evidence Bundle when referring to the storage location

**Session Broker**:
The Windows-side service that launches, owns, or attaches to Maya UI sessions for Maya Stall.
_Avoid_: Runner when the test execution layer is meant

**Maya Session Daemon**:
The first Session Broker implementation used by Maya Stall on a Maya Host.
_Avoid_: Session Broker when speaking about the abstract role

**Crabbox**:
An upstream remote execution control plane used as a reference for leases, target profiles, repo sync, remote command execution, desktop evidence, and artifact publishing.
_Avoid_: Maya Stall, Maya session broker

## Example Dialogue

Dev: "The consuming repo provides a target profile and a run payload."

Domain expert: "Right. Maya Stall uses the target profile to reach the Windows Maya environment, asks the session broker for a Maya UI session, runs the payload, then returns an evidence bundle."

Dev: "So pass/fail is not enough?"

Domain expert: "Correct. A run must leave enough evidence to debug UI failures later."
