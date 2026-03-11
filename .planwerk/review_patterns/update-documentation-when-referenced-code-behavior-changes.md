# Review Pattern: Update documentation when referenced code behavior changes

**Review-Area**: documentation
**Detection-Hint**: When a PR modifies script logic (e.g., file paths, arguments, invocation requirements), search the docs/ directory for references to that script and verify descriptions still match the implementation.
**Severity**: WARNING
**Occurrences**: 2

## What to check

Check that documentation describing a script's behavior (file paths read, expected working directory, arguments) matches the actual script implementation, especially after the script was modified in a prior or current PR.

## Why it matters

Stale documentation misleads users into invoking tools incorrectly, leading to silent failures or confusing errors when file paths or preconditions no longer match reality.

## Examples from external reviews

### CC-0006 — greptile-apps[bot]
- **Feedback**: This sentence says the script reads `upper-constraints.txt` "from the current directory", which was accurate for the original implementation but is no longer correct. The script now resolves `CONSTRAINTS="releases/${RELEASE}/upper-constraints.txt"`
- **What was missed**: Check that documentation describing a script's behavior (file paths read, expected working directory, arguments) matches the actual script implementation, especially after the script was modified in a prior or current PR.
- **Fix**: Updated docs/reference/container-images.md lines 307-309 to reference `releases/<release>/upper-constraints.txt` relative to repository root, matching the actual script logic.

### CC-0008 — greptile-apps[bot]
- **Feedback**: The reference docs acknowledge this but attribute incorrect FluxCD behaviour (docs/reference/infrastructure-manifests.md line 111–112 says 'the HelmRelease installs CRDs first, making the ClusterIssuer valid' — this is not true for `kubectl apply -k`; the HelmRelease install is asynchronous).
- **What was missed**: Check that any documentation statement explaining *why* a deployment pattern works is accurate for the exact command documented. For example, if docs say 'the HelmRelease installs CRDs first, making resource X valid', verify whether that is true for `kubectl apply -k` (it is not — HelmRelease processing is asynchronous) versus FluxCD's Kustomization controller with `dependsOn` (where it can be true).
- **Fix**: The reference documentation (docs/reference/infrastructure-manifests.md) was substantially rewritten to reflect the two-phase deployment, correct the previously inaccurate FluxCD behavior claims, and document the new directory layout.
