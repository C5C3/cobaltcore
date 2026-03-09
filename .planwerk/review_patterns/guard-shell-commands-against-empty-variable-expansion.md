# Review Pattern: Guard shell commands against empty variable expansion

**Review-Area**: error-handling
**Detection-Hint**: When reviewing Dockerfiles or shell scripts, check every ARG/ENV with a default of empty string. Trace how it is used: if it becomes a positional argument to a command (apt-get install, pip install, etc.), verify the command handles zero arguments gracefully. If not, require a conditional guard.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Any RUN instruction that passes a build ARG or ENV variable as arguments to a package manager (apt-get, pip, npm, etc.) must be wrapped in a conditional `if [ -n "$VAR" ]` when the variable can legitimately be empty. Check the ARG default value and whether all callers (CI and local builds) are guaranteed to provide a non-empty value.

## Why it matters

CI may always supply the value, but local developer builds following documented instructions can omit the build-arg, producing a cryptic failure (`E: No packages specified`) instead of a clean no-op. This wastes developer time debugging a Dockerfile issue that has nothing to do with their actual work.

## Examples from external reviews

### CC-0027 — greptile-apps[bot]
- **Feedback**: `apt-get install` fails with default empty `EXTRA_APT_PACKAGES` — a developer following the local build instructions who omits `--build-arg EXTRA_APT_PACKAGES=...` gets a confusing failure. A conditional guard would make this more resilient.
- **What was missed**: Any RUN instruction that passes a build ARG or ENV variable as arguments to a package manager (apt-get, pip, npm, etc.) must be wrapped in a conditional `if [ -n "$VAR" ]` when the variable can legitimately be empty. Check the ARG default value and whether all callers (CI and local builds) are guaranteed to provide a non-empty value.
- **Fix**: Wrapped the apt-get install block in `if [ -n "${EXTRA_APT_PACKAGES}" ]; then ... fi` so an empty value is a silent no-op instead of a build failure.
