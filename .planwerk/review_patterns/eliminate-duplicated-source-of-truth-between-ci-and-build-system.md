# Review Pattern: Eliminate duplicated source-of-truth between CI and build system

**Review-Area**: architecture
**Detection-Hint**: When a CI workflow file contains hardcoded lists (paths, modules, targets) that also exist in a Makefile or build script, flag the duplication. Search for the same values appearing in both .github/workflows/*.yml and Makefile/build configs.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Are CI job steps duplicating logic or lists already defined in the build system (Makefile, scripts)? Could the CI job call the existing build target instead of re-implementing it inline?

## Why it matters

Duplicated module/path lists between CI and Makefile will silently drift when one is updated but not the other, causing CI to test a stale set of targets while the Makefile tests the correct set — or vice versa.

## Examples from external reviews

### CC-0052 — sourcery-ai[bot]
- **Feedback**: The list of modules under race testing is duplicated between the Makefile (`internal/common`, `$(OPERATORS)`) and the `test-race` CI job (explicit paths), which risks drift over time; consider deriving the CI paths from the same source (e.g., calling `make test-race` or using `OPERATORS`) so they stay in sync.
- **What was missed**: Are CI job steps duplicating logic or lists already defined in the build system (Makefile, scripts)? Could the CI job call the existing build target instead of re-implementing it inline?
- **Fix**: Changed the CI test-race job from inline `go test` commands with hardcoded paths to `make test-race RACE_FLAGS="-count=1"`, delegating to the Makefile as single source of truth. Added a `RACE_FLAGS` variable to the Makefile so CI can pass additional flags.

### CC-0061 — berendt
- **Feedback**: The three module paths are hardcoded in the workflow, duplicating the go.work use directives and the OPERATORS variable in the Makefile. Adding a new Go module to go.work requires a separate manual edit to this step. The sibling test-race job avoids this problem by delegating to make test-race, which iterates over $(OPERATORS).
- **What was missed**: Does the new CI step hardcode values (module paths, directory lists, package names) that are already the canonical source of truth in another file? Are there existing CI jobs that solve this same problem by delegating to a build tool?
- **Fix**: Added a `make govulncheck` Makefile target that iterates over `$(OPERATORS)`, matching the existing `test-race` pattern, and replaced the hardcoded CI `run` block with `run: make govulncheck`.
