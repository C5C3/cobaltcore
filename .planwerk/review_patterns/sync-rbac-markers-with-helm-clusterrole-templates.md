# Review Pattern: Sync RBAC markers with Helm ClusterRole templates

**Review-Area**: security
**Detection-Hint**: When a PR adds or modifies `+kubebuilder:rbac` markers in Go controller files, check whether the corresponding Helm `clusterrole.yaml` template has matching apiGroups/resources/verbs entries.
**Severity**: BLOCKING
**Occurrences**: 2

## What to check

For every `+kubebuilder:rbac` annotation in the diff, verify a corresponding rule exists in the Helm ClusterRole template. Compare apiGroups, resources, and verbs between the two sources.

## Why it matters

Kubebuilder markers generate RBAC for `make manifests` but Helm-deployed operators use the Helm template. A mismatch means the operator gets 403 Forbidden at runtime, silently breaking reconciliation loops with no build-time or test-time signal.

## Examples from external reviews

### CC-0037 — berendt
- **Feedback**: The kubebuilder RBAC marker was added to the controller for policy/poddisruptionbudgets, but the Helm ClusterRole template has no policy apiGroup rule. The operator will receive 403 Forbidden errors at runtime.
- **What was missed**: For every `+kubebuilder:rbac` annotation in the diff, verify a corresponding rule exists in the Helm ClusterRole template. Compare apiGroups, resources, and verbs between the two sources.
- **Fix**: Add the missing RBAC rule to clusterrole.yaml with apiGroups: [policy], resources: [poddisruptionbudgets], verbs: [get, list, watch, create, update, patch, delete].

### CC-0038 — berendt
- **Feedback**: The controller declares kubebuilder RBAC markers for autoscaling resources and SetupWithManager owns HorizontalPodAutoscaler, but the Helm ClusterRole template has no autoscaling apiGroup entry. When deployed via Helm, the operator will receive 403 Forbidden on every HPA create/get/update/delete call, silently breaking reconciliation.
- **What was missed**: Diff any controller file for new `+kubebuilder:rbac` markers or new `Owns()`/`Watches()` calls. Then open the Helm ClusterRole template and verify every declared API group and resource has a corresponding rule. The two sources of RBAC truth (kubebuilder markers → `config/rbac` and Helm templates) must stay in sync.
- **Fix**: Added an autoscaling RBAC rule (apiGroups: autoscaling, resources: horizontalpodautoscalers, verbs: get/list/watch/create/update/patch/delete) to the Helm ClusterRole template.
