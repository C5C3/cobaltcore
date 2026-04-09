# Review Pattern: Cross-reference new security helpers against the specification they claim to implement

**Review-Area**: security
**Detection-Hint**: When a function's doc comment or name references a specification (e.g., 'Pod Security Standards restricted profile'), open that specification and verify every REQUIRED field is present. Compare against existing compliant templates in the same repo (e.g., Helm charts).
**Severity**: WARNING
**Occurrences**: 1

## What to check

Every field required by the claimed security standard is included. In this case, the Kubernetes PSS Restricted profile mandates Capabilities.Drop=['ALL'], but the helper omitted it while the project's own Helm deployment template already had it.

## Why it matters

An incomplete restricted SecurityContext will be silently rejected by clusters enforcing Pod Security Standards via admission policies, causing runtime failures that are hard to diagnose. The codebase already had a correct reference implementation in the Helm chart, making this inconsistency fully detectable at review time.

## Examples from external reviews

### CC-0045 — berendt
- **Feedback**: The restrictedSecurityContext helper is missing Capabilities: { Drop: ["ALL"] }, which is required by the Pod Security Standards Restricted profile. The project's own Helm operator deployment template already sets and tests for this field.
- **What was missed**: Every field required by the claimed security standard is included. In this case, the Kubernetes PSS Restricted profile mandates Capabilities.Drop=['ALL'], but the helper omitted it while the project's own Helm deployment template already had it.
- **Fix**: Added `Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}` to the helper and a corresponding assertion to `expectRestrictedSecurityContext`.
