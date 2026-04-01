# Review Pattern: Sync RBAC markers with Helm ClusterRole templates

**Review-Area**: security
**Detection-Hint**: When a PR adds or modifies `+kubebuilder:rbac` markers in Go controller files, check whether the corresponding Helm `clusterrole.yaml` template has matching apiGroups/resources/verbs entries.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

For every `+kubebuilder:rbac` annotation in the diff, verify a corresponding rule exists in the Helm ClusterRole template. Compare apiGroups, resources, and verbs between the two sources.

## Why it matters

Kubebuilder markers generate RBAC for `make manifests` but Helm-deployed operators use the Helm template. A mismatch means the operator gets 403 Forbidden at runtime, silently breaking reconciliation loops with no build-time or test-time signal.

## Examples from external reviews

### CC-0037 — berendt
- **Feedback**: The kubebuilder RBAC marker was added to the controller for policy/poddisruptionbudgets, but the Helm ClusterRole template has no policy apiGroup rule. The operator will receive 403 Forbidden errors at runtime.
- **What was missed**: For every `+kubebuilder:rbac` annotation in the diff, verify a corresponding rule exists in the Helm ClusterRole template. Compare apiGroups, resources, and verbs between the two sources.
- **Fix**: Add the missing RBAC rule to clusterrole.yaml with apiGroups: [policy], resources: [poddisruptionbudgets], verbs: [get, list, watch, create, update, patch, delete].
