# Review Pattern: Expose new configuration through all existing config surfaces

**Review-Area**: architecture
**Detection-Hint**: When a PR adds a new runtime configuration option (CLI flag, env var), check whether the project also has a programmatic configuration struct (e.g., ManagerConfig, Options). If it does, verify the new option is also exposed there.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Identify all configuration entry points (CLI flags, config structs, env vars). When a new option is added to one entry point, verify it is also added to the others so that all consumers (CLI users, library callers, tests) can use it consistently.

## Why it matters

If a configuration option is only available via CLI flag parsing but not via the programmatic config struct, callers who construct the manager programmatically (tests, embedding applications, custom controllers) cannot use the feature without reaching into global flag state, breaking encapsulation and testability.

## Examples from external reviews

### CC-0043 — sourcery-ai[bot]
- **Feedback**: The new `--namespace` CLI flag is only configured via flag parsing in `Run`; if `ManagerConfig` is used to construct managers programmatically, consider adding a `Namespace` field there so callers can opt into namespace scoping without going through global flags.
- **What was missed**: Identify all configuration entry points (CLI flags, config structs, env vars). When a new option is added to one entry point, verify it is also added to the others so that all consumers (CLI users, library callers, tests) can use it consistently.
- **Fix**: Add a `Namespace string` field to `ManagerConfig` and wire the CLI flag to populate it, so both CLI and programmatic callers have access to the namespace-scoping configuration.
