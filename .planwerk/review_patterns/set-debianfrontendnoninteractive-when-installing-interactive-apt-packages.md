# Review Pattern: Set DEBIAN_FRONTEND=noninteractive when installing interactive apt packages

**Review-Area**: security
**Detection-Hint**: When reviewing a Dockerfile that runs `apt-get install`, check if any listed package is known to trigger interactive prompts (tzdata, keyboard-configuration, console-setup, locales). If so, verify DEBIAN_FRONTEND=noninteractive is set.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Check that any Dockerfile `RUN apt-get install` step that includes `tzdata` (or other debconf-interactive packages) sets `DEBIAN_FRONTEND=noninteractive`, either as an `ENV` or inline for the RUN step.

## Why it matters

Without this setting, tzdata and similar packages may present interactive prompts that hang or silently fail in CI environments without a TTY, causing non-reproducible build failures that are difficult to diagnose.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `tzdata` is known to present interactive prompts for geographic area and timezone when installed via `apt-get` if `DEBIAN_FRONTEND` is not set. Docker builds have no TTY, so `tzdata` usually defaults to UTC without hanging, but the behaviour is environment-dependent and has caused silent build failures on some CI runners.
- **What was missed**: Check that any Dockerfile `RUN apt-get install` step that includes `tzdata` (or other debconf-interactive packages) sets `DEBIAN_FRONTEND=noninteractive`, either as an `ENV` or inline for the RUN step.
- **Fix**: Changed `RUN apt-get update && apt-get install -y` to `RUN DEBIAN_FRONTEND=noninteractive apt-get update && apt-get install -y` in images/python-base/Dockerfile.
