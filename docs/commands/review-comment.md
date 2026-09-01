# review-comment

`maya-stall review-comment` creates or updates one marked Review Comment from a
published Evidence Bundle.

## GitHub

```sh
maya-stall review-comment github \
  --repo owner/repo \
  --pr 123 \
  /mnt/evidence/maya-stall/<run-id>
```

GitHub reads `GITHUB_TOKEN` by default. Use `--token-env <name>` to read a
different exact environment variable and `--api-url <url>` for GitHub
Enterprise.

## GitLab

```sh
maya-stall review-comment gitlab \
  --project group/project \
  --merge-request 123 \
  /mnt/evidence/maya-stall/<run-id>
```

GitLab reads `GITLAB_TOKEN` by default. Use `--token-env <name>` to read a
different exact environment variable and `--base-url <url>` for self-managed
GitLab.

## Dry Run

Use `--dry-run` to render locally without credentials or network writes:

```sh
maya-stall review-comment github \
  --repo owner/repo \
  --pr 123 \
  --dry-run \
  /mnt/evidence/maya-stall/<run-id>
```

The command rewrites `review-comment.md` from `artifact-manifest.json` before
posting so the comment matches the published bundle. When the bundle includes a
verified `report.html`, the comment links it as the primary detail page before
the artifact inventory.
