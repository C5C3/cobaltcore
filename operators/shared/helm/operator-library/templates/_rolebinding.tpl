{{/*
Shared rolebinding template for the operator charts. Defined once here and included
by each operator chart's templates/rolebinding.yaml with the consuming chart's root
context, so .Chart/.Release/.Values resolve against the operator chart.
*/}}
{{- define "operator-library.rolebinding" -}}
{{- if .Values.rbac.namespaceScoped }}
# Namespace-scoped RoleBinding (replaces ClusterRoleBinding).
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "operator-library.fullname" . }}
subjects:
  - kind: ServiceAccount
    name: {{ include "operator-library.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
{{- end }}
{{- end }}
