# plan

`maya-stall plan` inspects a named Scenario without touching a Maya Host.

```sh
maya-stall plan smoke
maya-stall plan --json smoke
maya-stall plan --host-config ci-hosts.yaml smoke
maya-stall plan --input scene=C:\scenes\character.ma local-smoke
```

## Behavior

The command loads Repo Run Config and uses the same Scenario normalization and
Run Payload manifest builder as `maya-stall run`. It reports each declared
payload's normalized source, staged destination, kind, byte size, SHA-256 hash,
and readiness. Missing and unsafe inputs are retained in the report with a
concrete reason.

For a directory declaration, `size` is the sum of regular-file bytes. Its hash
is a deterministic tree digest over each sorted repo-relative file path, a NUL
separator, the decimal byte size, another NUL separator, and the file bytes.
Path changes and content changes therefore both change the digest.

A Scenario may declare required named file slots under
`payload.runtimeInputs`. Bind each slot with a repeated
`--input name=absolute-file`. Planning validates one regular non-symlink,
non-reparse-point file, its allowlisted extension, size, and SHA-256. The
runtime entry reports its name, `runtimeInput:file` kind, deterministic
`payload/runtimeInputs/<destination>` path, size, hash, and status. It never
reports the user's absolute source path. Directories, implicit sibling files,
scripts, credentials, undeclared bindings, and duplicate bindings fail closed.

```yaml
payload:
  runtimeInputs:
    scene:
      kind: file
      extensions: [.ma, .mb]
      destination: scenes/input.ma
```

Requirements include exact or minimum Maya, Python, and Session Broker
versions; required Session Broker, capture, control, renderer, GPU, display,
and licensing values; trusted Plugin Artifact support; and known capabilities
such as `script.execute`, `screenshot.capture`, `recording.capture`, and
`visual-evidence.required` for a required Visual Evidence Validator. Every
Scenario implicitly requires the Session Broker `script.execute` feature;
enabled screenshot or recording evidence implicitly requires the matching
capture capability. A required Visual Evidence validator implicitly requires
the `visual-evidence` capture capability.

When `--host-config` is present, `plan` reads every Target Profile, Host Pool,
and Maya Host from that local file. It reports compatible and incompatible
hosts using configured health, runtime shape, declared capabilities, and
Visual Evidence support. It uses the same compatibility decision and mismatch
wording as Control Plane scheduling. This is static planning information, not
an Agent heartbeat or live Host Health proof; run `doctor` for live checks.

Example Scenario requirements:

```yaml
requirements:
  maya:
    minimum: "2025.2"
  python:
    exact: "3.11.9"
  sessionBroker:
    minimum: "2.1"
    features: [script.execute, status.observe]
  capture: [screenshot, recording]
  control: [coordinate]
  renderers: [arnold]
  gpu: [nvidia]
  display: [console]
  licensing: [available]
  trustedPluginArtifacts: true
```

Each version requirement accepts `exact` or `minimum`, never both. The legacy
`mayaVersion` field remains an exact requirement and cannot be combined with
`requirements.maya`. Matching uses the Host's declared Session Broker Maya
build, not an arbitrary installed build; configured Agent execution binds the
fresh session durably and then verifies that build before payload staging.

`plan` never acquires a Host Lock, creates Run State or an Evidence Bundle,
opens SSH, calls the Session Broker, reads an Evidence Store, or mutates an
external system. The final `host-contact: none` line makes that boundary
explicit in human output.

## JSON

`--json` emits one stable `scenario-plan` document. Its top-level fields are:

- `version`, `kind`, `scenario`, `configPath`, and `ready`;
- `requirements`, including normalized `hostCapabilities` exact/minimum and
  feature requirements;
- `payload` entries with `name` when declared, `kind`, optional repo-owned
  `source`, `destination`, `size`, `sha256`, and `status`; Runtime Inputs omit
  the private absolute source;
- `issues` with source-specific reasons;
- `targetProfiles`, containing Host Pool and Maya Host compatibility.

The command exits `0` when local inputs are ready and, if host config was
provided, at least one configured Maya Host is compatible. It exits `1` for a
valid but blocked plan and `2` for command usage errors.
