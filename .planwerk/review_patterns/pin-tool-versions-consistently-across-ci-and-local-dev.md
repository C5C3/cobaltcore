# Review Pattern: Pin tool versions consistently across CI and local dev

**Review-Area**: architecture
**Detection-Hint**: When a PR introduces a pinned tool version in CI (e.g., an env var like `TOOL_VERSION`), check whether the same version is referenced in local developer tooling such as Makefiles, setup scripts, or contributing docs.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Is a tool version pinned in CI but not in any local development entry point (Makefile, script, devcontainer)? Could a contributor run a different version locally and get different results than CI?

## Why it matters

Version drift between CI and local tooling causes contributors to pass local checks but fail in CI (or vice versa), wasting debugging time and creating friction. A single source of truth for tool versions prevents silent divergence.

## Examples from external reviews

### CC-0053 — sourcery-ai[bot]
- **Feedback**: You've added `GOFUMPT_VERSION` as a top-level CI env var; to keep local development consistent with CI, it may be worth wiring this same version into any existing local tooling (e.g. a `make fmt` or developer setup script) so contributors don't unknowingly use a different gofumpt version.
- **What was missed**: Is a tool version pinned in CI but not in any local development entry point (Makefile, script, devcontainer)? Could a contributor run a different version locally and get different results than CI?
- **Fix**: Added Makefile targets (`make fmt`, `make format-check`, `make install-gofumpt`) using `GOFUMPT_VERSION ?= v0.9.2` to match the CI-pinned version, providing a single source of truth.
