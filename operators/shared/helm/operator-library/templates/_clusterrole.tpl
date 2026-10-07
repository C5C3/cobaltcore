{{/*
Shared ClusterRole template for the operator charts. Defined once here and
included by each operator chart's templates/clusterrole.yaml with the consuming
chart's root context.

The rules come from the chart's "<chart name>.rbacRules" named template
(templates/_rbac-rules.tpl in the operator chart), which the library resolves
via .Chart.Name — so the ClusterRole and the namespace-scoped Role always
render the same rule set. Under webhook.standalone they come from the chart's
"<chart name>.webhookRbacRules" template (templates/_webhook-rbac-rules.tpl)
instead: that release runs no controller, so its ServiceAccount needs only
what the admission webhooks read.
*/}}
{{- define "operator-library.clusterrole" -}}
{{- if not .Values.rbac.namespaceScoped }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: {{ include "operator-library.fullname" . }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
rules:
  {{- if .Values.webhook.standalone }}
  {{- include (printf "%s.webhookRbacRules" .Chart.Name) . | nindent 2 }}
  {{- else }}
  {{- include (printf "%s.rbacRules" .Chart.Name) . | nindent 2 }}
  {{- end }}
{{- end }}
{{- end }}
