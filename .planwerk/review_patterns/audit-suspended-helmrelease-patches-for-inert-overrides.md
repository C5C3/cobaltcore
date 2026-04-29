# Review Pattern: Audit suspended HelmRelease patches for inert overrides

**Review-Area**: architecture
**Detection-Hint**: When kind/base kustomization suspends a HelmRelease, any subsequent patches to that HelmRelease (e.g. enabling features via values) are durable but never applied. Look for patches that target suspended releases.
**Severity**: WARNING
**Occurrences**: 1

## What to check

Identify HelmRelease resources marked suspended in kustomize bases. For each, check if downstream overlays patch values on the suspended release. Such patches are dead code unless the suspend is lifted on the relevant path.

## Why it matters

Patches against suspended HelmReleases give the appearance of configuration but Flux never reconciles them. The actual deployment (helm install in a script) must carry the same overrides, or the feature silently fails.

## Examples from external reviews

### CC-0100 — berendt
- **Feedback**: The kind/base kustomization suspends HelmRelease/keystone-operator so the patch in `enable_keystone_operator_servicemonitor` is durable but inert.
- **What was missed**: Identify HelmRelease resources marked suspended in kustomize bases. For each, check if downstream overlays patch values on the suspended release. Such patches are dead code unless the suspend is lifted on the relevant path.
- **Fix**: Threaded the flag through the helm install invocation directly rather than relying on the suspended-release patch.
