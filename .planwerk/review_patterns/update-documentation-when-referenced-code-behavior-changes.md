# Review Pattern: Update documentation when referenced code behavior changes
**Review-Area**: documentation
**Detection-Hint**: When a PR modifies script logic (e.g., file paths, arguments, invocation requirements), search the docs/ directory for references to that script and verify descriptions still match the implementation.
**Severity**: WARNING
**Occurrences**: 5

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

### CC-0009 — greptile-apps[bot]
- **Feedback**: Stale documentation — describes discarded exit-code approach, not the current JSON parsing implementation. The claim that 'Exit code `2` indicates sealed (already initialized)' is exactly the ambiguity that the JSON parsing fix was introduced to resolve.
- **What was missed**: After any in-PR code revision, grep the PR's documentation files for terms related to the old approach (e.g., 'exit code', old function names, old flags). Verify all behavioral descriptions match the final implementation.
- **Fix**: Updated Behavior, Idempotency sections, and the idempotency summary table to describe `bao status -format=json` with `jq -e '.initialized == true'` instead of exit-code checking.

### CC-0046 — berendt
- **Feedback**: the documentation now contradicts itself by saying 'Most HelmReleases' instead of 'All HelmReleases'.
- **What was missed**: Does the PR weaken existing documentation statements (e.g., 'All' → 'Most', 'Always' → 'Usually') to justify a deviation in the new code? If so, question whether the code should conform to the documented standard rather than the docs being diluted.
- **Fix**: Documentation reverted from 'Most' back to 'All' and the chaos-mesh exception paragraph removed, after making the code conform to the documented standard.

### CC-0040 — berendt
- **Feedback**: The documentation claims the E2E test patches with processes: 8, threads: 4 and asserts --processes 8 --threads 4, but commit 384af29 reduced these to processes: 3, threads: 3 in the actual test files without updating the docs.
- **What was missed**: Grep docs/ for the old literal values being changed in test files (e.g., grep for 'processes: 8' or 'threads: 4'). Any documentation table or example that references specific test parameters must match the actual test files.
- **Fix**: Updated the documentation table rows to reflect the actual test values of processes: 3 and threads: 3.
