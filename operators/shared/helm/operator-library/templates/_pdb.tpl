{{/*
Shared pdb template for the operator charts. Defined once here and included
by each operator chart's templates/pdb.yaml with the consuming chart's root
context, so .Chart/.Release/.Values resolve against the operator chart.
*/}}
{{- define "operator-library.pdb" -}}
{{/*
  PodDisruptionBudget for the operator Deployment.

  Keeps at least one replica — and with it the in-process admission webhook
  (failurePolicy=Fail) — available during voluntary disruptions such as node
  drains and cluster upgrades. Rendered only when replicas > 1: with a single
  replica, minAvailable: 1 would block every voluntary disruption and wedge
  node drains.
*/}}
{{- if gt (int .Values.replicas) 1 }}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  minAvailable: 1
  selector:
    matchLabels:
      {{- include "operator-library.selectorLabels" . | nindent 6 }}
{{- end }}
{{- end }}
