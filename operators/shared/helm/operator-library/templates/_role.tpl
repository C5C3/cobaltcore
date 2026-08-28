{{/*
Shared namespace-scoped Role template for the operator charts. Defined once
here and included by each operator chart's templates/role.yaml with the
consuming chart's root context. Rendered only for rbac.namespaceScoped=true;
the rules are the same "<chart name>.rbacRules" set the ClusterRole renders.

Two guards refuse the mode:
  - webhook.enabled=true — a namespace-scoped operator cannot manage the
    cluster-scoped webhook configurations.
  - a non-empty "operator-library.chart.namespaceScopedUnsupported" hook — a
    chart whose operator cannot run namespace-scoped at all (ovn-operator
    watches cluster-scoped Nodes; neutron-operator reads across namespaces)
    overrides the hook with the reason, and the render fails with it.
*/}}
{{- define "operator-library.role" -}}
{{- if and .Values.rbac.namespaceScoped .Values.webhook.enabled }}
{{- fail "rbac.namespaceScoped=true requires webhook.enabled=false — namespace-scoped operators cannot manage cluster-scoped webhook configurations" }}
{{- end }}
{{- if .Values.rbac.namespaceScoped }}
{{- with include "operator-library.chart.namespaceScopedUnsupported" . }}
{{- fail (printf "rbac.namespaceScoped=true is not supported by %s — %s" $.Chart.Name .) }}
{{- end }}
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
rules:
  {{- include (printf "%s.rbacRules" .Chart.Name) . | nindent 2 }}
{{- end }}
{{- end }}
