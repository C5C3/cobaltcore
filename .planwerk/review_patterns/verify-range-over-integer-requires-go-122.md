# Review Pattern: Verify range-over-integer requires Go 1.22+

**Review-Area**: type-safety
**Detection-Hint**: When you see `for i := range <integer>`, check the go.mod minimum Go version. This syntax was introduced in Go 1.22. If the project targets or may be built with older toolchains, flag it.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Any use of `for i := range N` where N is an integer. Confirm the project's go.mod declares Go >= 1.22 and that all CI/build environments use that version.

## Why it matters

The `range` over integer syntax only compiles on Go 1.22+. If anyone builds with an older toolchain—or a downstream consumer pins an older Go version—the code won't compile at all. Even when it does compile, the idiom is unfamiliar enough that it slows comprehension for many reviewers.

## Examples from external reviews

### CC-0036 — sourcery-ai[bot]
- **Feedback**: In `createCredentialKeysSecret` you're using `for i := range numKeys` on an `int`; consider switching to a conventional `for i := 0; i < numKeys; i++ {}` loop for clearer intent and compatibility with older Go toolchains.
- **What was missed**: Any use of `for i := range N` where N is an integer. Confirm the project's go.mod declares Go >= 1.22 and that all CI/build environments use that version.
- **Fix**: All `for i := range <int>` loops in both production and test code were replaced with `for i := 0; i < N; i++ {}` style loops.
