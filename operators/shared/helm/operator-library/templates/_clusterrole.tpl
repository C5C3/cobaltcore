{{/*
Shared ClusterRole template for the operator charts. Defined once here and
included by each operator chart's templates/clusterrole.yaml with the consuming
chart's root context.

The rules come from the chart's "<chart name>.rbacRules" named template
(templates/_rbac-rules.tpl in the operator chart), which the library resolves
via .Chart.Name — so the ClusterRole and the namespace-scoped Role always
render the same rule set.
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
  {{- include (printf "%s.rbacRules" .Chart.Name) . | nindent 2 }}
{{- end }}
{{- end }}
