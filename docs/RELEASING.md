# Releasing

Read this when preparing a Maya Stall release. This checklist is for future
release execution; adding or editing this file does not authorize a release.

## Current Recommendation

Use `v0.1.0` for the first release unless Bram explicitly chooses a stronger
compatibility promise. The codebase implements the v1 product shape, but no tag
or GitHub Release exists yet, and a first public SemVer tag should leave room
for early CLI, config, packaging, and proof-contract adjustment.

Use `v1.0.0` only after Bram confirms:

- command and config compatibility should be treated as stable;
- release notes can promise the current proof and artifact contract;
- the exact release head has the required local, CI, and live Maya proof.

## Release Prep Checklist

1. Start from a clean `main`.
2. Fetch and fast-forward to `origin/main`.
3. Confirm the intended release head with `git rev-parse HEAD`.
4. Confirm no release already exists with `git tag --list` and `gh release list --repo BramVR/maya_stall`.
5. Check open release blockers with `gh issue list --repo BramVR/maya_stall --state open` and `gh pr list --repo BramVR/maya_stall --state open`.
6. Recheck dependency freshness. Do not accept dependency churn only to make the graph look newer.
7. Choose the release version and update `CHANGELOG.md`: move the proposed notes under the chosen version, add the release date, and open a fresh top `Unreleased` section.
8. Do not change code version metadata, package metadata, tags, environments, or release assets unless that exact release execution has been approved.

## Local Gates

Run the default local gates from a clean checkout:

```sh
go test ./...
go vet ./...
scripts/check-docs.sh
node --test scripts/proof/*.test.mjs scripts/windows/*.test.mjs
```

For release-only docs changes, live Maya proof is not required by the changed
paths. For a release candidate that touches runtime behavior, live workflow
behavior, proof policy, Visual Evidence, Review Comments, host config, SSH,
Session Broker behavior, or Evidence Bundle layout, fake/local tests are not
enough.

## CI And Live Proof

Before tagging, verify the exact release head has green GitHub Actions:

- CI: `.github/workflows/ci-required.yml`; `CI / Required` is the stable required check.

If the proof policy requires live Maya proof, the protected Live Maya Proof job must
pass against a configured owned Windows Maya Host. Skipped, missing, fake-only,
or fork-withheld live proof blocks the release.

For a first release that claims live Windows Maya proof, use a fresh green main
Proof run at the exact release head. Confirm the run produced the
`live-visual-evidence-proof` artifact and that the proof manifest reports the
live Maya gate as passed.

## Public Artifact Confidentiality

Release notes, proof summaries, and uploaded proof artifacts must remain public
safe. Do not publish private host config, Host Credentials, SSH keys, Windows
credentials, tokens, private hostnames, local user paths, license details, raw
logs with secrets, or unreviewed desktop media.

Before copying release notes into a public GitHub body, write them to a temp file
and scan that file:

```sh
mkdir -p /tmp/maya-stall-release
cp /path/to/release-notes.md /tmp/maya-stall-release/release-notes.md
node scripts/proof/assert-public-artifact-confidentiality.mjs --path /tmp/maya-stall-release
```

Before publishing live proof artifacts, rely on the workflow confidentiality
gate. It must scan the proof artifact directory and the proof manifest before
the `live-visual-evidence-proof` artifact is uploaded.

## Release Notes Draft

Prepare the public body from `CHANGELOG.md`. Include:

- version and date;
- 2-5 highlights;
- ordered changelog entries with full canonical PR URLs;
- exact release commit;
- local gates and CI run URLs;
- live Maya proof state and artifact name when applicable;
- dependency-freshness result;
- any known release blockers or hold conditions.

Keep the body in a temp file and scan it with the public artifact
confidentiality gate. Do not run `gh release create` during prep-only work.

## Tag And Publish

Only after approval, create and push the annotated tag, then create the draft
GitHub Release from that existing tag:

```sh
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
gh release create v0.1.0 \
  --repo BramVR/maya_stall \
  --verify-tag \
  --draft \
  --title "v0.1.0" \
  --notes-file /tmp/maya-stall-release/release-notes.md
```

Publish the draft only after the final release view matches the intended
release notes, target commit, proof links, and assets:

```sh
gh release edit v0.1.0 --repo BramVR/maya_stall --draft=false
```

If release assets are added later, build them from the tag, publish checksums,
and verify downloads before marking the release ready. Do not publish registry
artifacts unless that registry lane has its own approval and verification
checklist.

## Post-Release Verification

Verify:

- `git ls-remote --tags origin v0.1.0` returns the pushed tag;
- `gh release view v0.1.0 --repo BramVR/maya_stall` shows the expected title, body, target commit, and assets;
- release notes link the changelog section, CI runs, live proof run, and proof artifact when applicable;
- a clean checkout at the tag builds and passes the chosen smoke command;
- no generated release note or public proof text failed the confidentiality gate.

Then open the next `Unreleased` changelog section and commit that metadata-only
change separately.

## Hold Conditions

Hold the release if any of these are true:

- Bram has not chosen release execution;
- exact-head Lint or Proof is red, missing, or stale;
- live Maya proof is required but missing, skipped, fake-only, or blocked by the protected environment;
- public artifact confidentiality scan fails;
- release notes contain private host details, credentials, local machine paths, or unreviewed media;
- dependency freshness finds a safe required update that has not been handled;
- the release would imply a compatibility promise Bram has not approved.
