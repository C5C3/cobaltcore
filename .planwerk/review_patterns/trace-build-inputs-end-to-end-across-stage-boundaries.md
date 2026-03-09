# Review Pattern: Trace build inputs end-to-end across stage boundaries

**Review-Area**: validation
**Detection-Hint**: When a CI workflow or build system passes a parameter (e.g., --build-arg), trace it from the caller to where it is consumed. In multi-stage Dockerfiles, confirm the ARG is re-declared after each FROM where it is needed, since ARGs do not cross stage boundaries.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For every build arg passed by the CI workflow, verify that (1) the Dockerfile declares it in the correct stage, and (2) it is actually referenced in a RUN or similar instruction. A declared-but-unused or passed-but-undeclared arg means the feature is silently broken.

## Why it matters

The entire point of the PR was to eliminate drift by parameterizing packages via build args. If the arg never reaches the consumer, the feature is completely inert and the stated goal is unmet — hardcoded values still govern the build.

## Examples from external reviews

### CC-0027 — greptile-apps[bot]
- **Feedback**: The CI workflow resolves and passes `EXTRA_APT_PACKAGES` as a build arg, but the Dockerfile never declares `ARG EXTRA_APT_PACKAGES` in Stage 2 nor uses it. The apt packages remain hardcoded, meaning `apt_packages` entries in `extra-packages.yaml` are completely ignored at build time.
- **What was missed**: For every build arg passed by the CI workflow, verify that (1) the Dockerfile declares it in the correct stage, and (2) it is actually referenced in a RUN or similar instruction. A declared-but-unused or passed-but-undeclared arg means the feature is silently broken.
- **Fix**: Added `ARG EXTRA_APT_PACKAGES=""` after the Stage 2 `FROM` line and replaced the hardcoded apt package list with `$EXTRA_APT_PACKAGES` in the `apt-get install` command.
