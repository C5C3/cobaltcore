# Review Pattern: Flag global config changes that solve localized problems

**Review-Area**: architecture
**Detection-Hint**: When a project-wide configuration option is added or changed, ask: does this need to apply globally, or is the underlying problem limited to specific files? Look for config changes whose commit message or PR description references only a subset of files.
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

Check whether a global configuration change (e.g., framework-level settings in config files) is the narrowest possible fix. Verify that the change does not silently disable default behavior for unrelated parts of the project.

## Why it matters

A global setting that solves a problem in a few files can silently break functionality everywhere else. Future contributors won't get errors — features just quietly stop working, making the root cause hard to trace.

## Examples from external reviews

### CC-0033 — greptile-apps[bot]
- **Feedback**: Setting `delimiters: ['${{{{', '}}}}$']` applies globally to every page compiled by VitePress — not just the reference docs that contain GitHub Actions `${{ }}` expressions. Any future contributor who tries to use standard Vue template interpolation (e.g., a custom component or page that renders `{{ someVar }}`) will silently get no output rather than an error.
- **What was missed**: Check whether a global configuration change (e.g., framework-level settings in config files) is the narrowest possible fix. Verify that the change does not silently disable default behavior for unrelated parts of the project.
- **Fix**: Removed the global delimiter override from config.ts and wrapped only the affected content in individual reference doc files with `::: v-pre` / `:::` blocks.

### CC-0059 — berendt
- **Feedback**: W-002: Partial refactoring creates inconsistent access pattern — r.Client.Get → r.Get changes in reconcile_networkpolicy_test.go while 37 instances of r.Client.Get remain across the codebase.
- **What was missed**: When a diff replaces one idiomatic form with another (e.g. r.Client.Get → r.Get), count how many instances of the old form remain across the codebase. If the PR only converts a small fraction, flag it as a partial refactoring that should either be completed or reverted to keep consistency.
- **Fix**: Reverted all 4 r.Client.Get → r.Get changes in reconcile_networkpolicy_test.go back to r.Client.Get, restoring consistency with the remaining 37 instances across the codebase.
