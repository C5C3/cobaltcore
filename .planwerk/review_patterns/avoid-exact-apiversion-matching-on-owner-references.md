# Review Pattern: Avoid exact APIVersion matching on owner references

**Review-Area**: architecture
**Detection-Hint**: Look for ownerRef.APIVersion == someExactVersion comparisons; flag if it doesn't tolerate group version bumps.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

When filtering owner references by API group, compare on the group only (e.g. via schema.ParseGroupVersion) rather than on the full APIVersion string, so existing owner references survive a CRD version bump.

## Why it matters

Tying logic to an exact APIVersion silently breaks existing resources when the CRD is promoted (e.g. v1alpha1 -> v1beta1), causing mappers/controllers to stop reconciling without any obvious error.

## Examples from external reviews

### CC-0087 — sourcery-ai[bot]
- **Feedback**: The updated owner-ref path in secretToKeystoneMapper now matches only on Kind and exact APIVersion, dropping the previous UID-based isOwnedBy check; consider either retaining a UID-based match or at least accepting any version in the keystone.openstack.c5c3.io group so existing Secrets continue to map correctly across version bumps.
- **What was missed**: When filtering owner references by API group, compare on the group only (e.g. via schema.ParseGroupVersion) rather than on the full APIVersion string, so existing owner references survive a CRD version bump.
- **Fix**: Replaced exact APIVersion equality with group-only matching via schema.ParseGroupVersion so any version in keystone.openstack.c5c3.io is accepted.
