# Review Pattern: Eliminate duplicated source-of-truth between CI and build system

**Review-Area**: architecture
**Detection-Hint**: When a CI workflow file contains hardcoded lists (paths, modules, targets) that also exist in a Makefile or build script, flag the duplication. Search for the same values appearing in both .github/workflows/*.yml and Makefile/build configs.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Are CI job steps duplicating logic or lists already defined in the build system (Makefile, scripts)? Could the CI job call the existing build target instead of re-implementing it inline?

## Why it matters

Duplicated module/path lists between CI and Makefile will silently drift when one is updated but not the other, causing CI to test a stale set of targets while the Makefile tests the correct set — or vice versa.

## Examples from external reviews

### CC-0052 — sourcery-ai[bot]
- **Feedback**: The list of modules under race testing is duplicated between the Makefile (`internal/common`, `$(OPERATORS)`) and the `test-race` CI job (explicit paths), which risks drift over time; consider deriving the CI paths from the same source (e.g., calling `make test-race` or using `OPERATORS`) so they stay in sync.
- **What was missed**: Are CI job steps duplicating logic or lists already defined in the build system (Makefile, scripts)? Could the CI job call the existing build target instead of re-implementing it inline?
- **Fix**: Changed the CI test-race job from inline `go test` commands with hardcoded paths to `make test-race RACE_FLAGS="-count=1"`, delegating to the Makefile as single source of truth. Added a `RACE_FLAGS` variable to the Makefile so CI can pass additional flags.
