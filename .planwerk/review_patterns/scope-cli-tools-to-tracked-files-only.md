# Review Pattern: Scope CLI tools to tracked files only

**Review-Area**: architecture
**Detection-Hint**: When a CI job runs a linter or formatter with a recursive directory scan (e.g., `tool .` or `tool ./...`), check whether the repo contains generated, vendored, or third-party code that should be excluded from that tool's scope.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Does the CI command operate on the entire directory tree (`.` or `./...`) instead of scoping to tracked source files? Could generated code, vendor directories, or tooling artifacts cause false positives?

## Why it matters

Running formatters or linters over the entire repo can cause spurious CI failures on files the team does not own or maintain (vendored dependencies, generated protobuf/mock code, third-party tooling). This creates flaky pipelines and erodes trust in CI signals.

## Examples from external reviews

### CC-0053 — sourcery-ai[bot]
- **Feedback**: The `format-check` job currently runs `gofumpt -l .` over the entire repo; consider restricting this to tracked Go source files (e.g. via `git ls-files '*.go' | xargs gofumpt -l`) to avoid unexpected failures on generated, vendored, or tooling code.
- **What was missed**: Does the CI command operate on the entire directory tree (`.` or `./...`) instead of scoping to tracked source files? Could generated code, vendor directories, or tooling artifacts cause false positives?
- **Fix**: Changed from `gofumpt -l .` to `git ls-files '*.go' | xargs -r gofumpt -l` so only version-controlled Go source files are checked.
