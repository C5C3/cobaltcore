# Review Pattern: Enforce documented constraints as template-level guards

**Review-Area**: validation
**Detection-Hint**: When a PR documents that configuration X requires configuration Y (e.g., 'you must set webhook.enabled=false when rbac.namespaceScoped=true'), check whether the templates actually enforce that constraint with a {{- fail }} guard or equivalent. Search for 'must', 'requires', 'only if' in added docs and verify matching validation exists in code.
**Severity**: BLOCKING
**Occurrences**: 1

## What to check

Every documented configuration constraint or incompatibility must have a corresponding programmatic enforcement (e.g., Helm fail, admission webhook, startup check). If documentation says 'A requires B', the system must reject the invalid combination with a clear error rather than silently producing a broken deployment.

## Why it matters

Without enforcement, users who follow the values.yaml defaults or miss a documentation footnote get a deployment that appears to install successfully but fails at runtime with cryptic 403 errors deep in the reconcile loop. This turns a preventable misconfiguration into an hours-long debugging session.

## Examples from external reviews

### CC-0043 — berendt
- **Feedback**: The documentation states users must set webhook.enabled: false when rbac.namespaceScoped: true, but the Helm templates do not enforce this. A user who sets rbac.namespaceScoped=true without disabling webhooks will get a Role (no ClusterRole) alongside cluster-scoped MutatingWebhookConfiguration and ValidatingWebhookConfiguration resources. The operator pod will start but fail at runtime.
- **What was missed**: Every documented configuration constraint or incompatibility must have a corresponding programmatic enforcement (e.g., Helm fail, admission webhook, startup check). If documentation says 'A requires B', the system must reject the invalid combination with a clear error rather than silently producing a broken deployment.
- **Fix**: Added a Helm fail guard at the top of role.yaml: {{- if and .Values.rbac.namespaceScoped .Values.webhook.enabled }}{{- fail "rbac.namespaceScoped=true requires webhook.enabled=false" }}{{- end }}
