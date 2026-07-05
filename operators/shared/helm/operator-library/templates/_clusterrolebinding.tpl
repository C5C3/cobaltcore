{{/*
Shared clusterrolebinding template for the operator charts. Defined once here and included
by each operator chart's templates/clusterrolebinding.yaml with the consuming chart's root
context, so .Chart/.Release/.Values resolve against the operator chart.
*/}}
{{- define "operator-library.clusterrolebinding" -}}
{{- if not .Values.rbac.namespaceScoped }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "operator-library.fullname" . }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ include "operator-library.fullname" . }}
subjects:
  - kind: ServiceAccount
    name: {{ include "operator-library.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
{{- end }}
{{- end }}
