# Review Pattern: Verify hardcoded paths match actual repository layout

**Review-Area**: validation
**Detection-Hint**: When a script assigns a file path to a variable, trace that path from the expected working directory and confirm the file actually exists at that location in the repo tree.
**Severity**: BLOCKING
**Occurrences**: 3

## What to check

Compare every hardcoded or constructed file path in scripts against the real directory structure. Pay special attention when one path variable (e.g., OVERRIDES) uses a subdirectory prefix like 'releases/${RELEASE}/' but a sibling variable (e.g., CONSTRAINTS) uses a bare filename — the inconsistency signals a bug.

## Why it matters

A wrong path causes the script to fail immediately on every invocation with a misleading error message, making the entire script non-functional. This is a ship-blocking defect that any manual test run would have caught.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: `CONSTRAINTS` is set to the bare filename `upper-constraints.txt`, but the actual file lives at `releases/${RELEASE}/upper-constraints.txt` in the repository. When the script is invoked from the repo root as documented, the pre-flight check will immediately fail.
- **What was missed**: Compare every hardcoded or constructed file path in scripts against the real directory structure. Pay special attention when one path variable (e.g., OVERRIDES) uses a subdirectory prefix like 'releases/${RELEASE}/' but a sibling variable (e.g., CONSTRAINTS) uses a bare filename — the inconsistency signals a bug.
- **Fix**: Changed CONSTRAINTS from bare filename to 'releases/${RELEASE}/upper-constraints.txt' to match the OVERRIDES pattern and the actual repo layout.

### CC-0032 — greptile-apps[bot]
- **Feedback**: Whether this works depends on `anchore/scan-action@v7` treating `""` as "not provided" (a truthy check in JavaScript). This is an **implicit contract with the action's internal implementation** rather than the documented interface, which shows `sbom` and `image` as mutually exclusive inputs.
- **What was missed**: When calling external actions (e.g., anchore/scan-action), verify that only the documented input is provided rather than passing both mutually-exclusive inputs with one as an empty string. GitHub Actions passes empty strings as actual values, not as absent inputs.
- **Fix**: Split into separate steps or use conditional step-level `if:` guards so that only the appropriate input is provided to each invocation, rather than passing both inputs with one as empty string.

### CC-0009 — greptile-apps[bot]
- **Feedback**: `setup-auth.sh` enables the `kubernetes/management` auth mount and creates the `eso-management` role, but never calls `bao write auth/kubernetes/management/config` to tell OpenBao how to validate service account tokens from the management cluster.
- **What was missed**: When the same file sets up the default kubernetes auth mount with enable + config + role, but the management kubernetes auth mount only has enable + role (skipping config), that gap should be flagged. Also verify that documentation claiming something is 'fully configured' matches the actual code.
- **Fix**: Added explicit `bao write auth/kubernetes/management/config kubernetes_host=... ca_cert=...` call mirroring the default mount configuration, and updated documentation to accurately describe the configuration and its RBAC prerequisite.
