# Review Pattern: Cross-check all instances of a claimed fix across files

**Review-Area**: validation
**Detection-Hint**: When a PR description claims to fix or rename a value, search the entire diff for every occurrence of both the old and new value. Confirm the source-of-truth file was updated, not just downstream consumers or documentation.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Grep the repo for the old value (e.g., `libldap-2.5-0`). If it still appears in the canonical data file while docs and Dockerfiles show the corrected value, the source-of-truth was missed. Similarly, if docs define a schema field (e.g., `pip_packages: []`), verify the actual data file includes it.

## Why it matters

Fixing a value in docs and downstream files but not in the source-of-truth YAML means the fix is cosmetic. Once the parameterization is properly wired, the build will fail on the stale incorrect value from the unfixed source file.

## Examples from external reviews

### CC-0027 — greptile-apps[bot]
- **Feedback**: The `docs/reference/container-images.md` diff correctly shows the fix, and the Dockerfile's hardcoded list already uses `libldap2`, but the actual source-of-truth YAML file was never updated. Once `EXTRA_APT_PACKAGES` is properly wired into the Dockerfile, this stale entry will cause `apt-get install` to fail at build time.
- **What was missed**: Grep the repo for the old value (e.g., `libldap-2.5-0`). If it still appears in the canonical data file while docs and Dockerfiles show the corrected value, the source-of-truth was missed. Similarly, if docs define a schema field (e.g., `pip_packages: []`), verify the actual data file includes it.
- **Fix**: Changed `libldap-2.5-0` to `libldap2` in `releases/2025.2/extra-packages.yaml` and added the missing `pip_packages: []` field to match the documented schema.
