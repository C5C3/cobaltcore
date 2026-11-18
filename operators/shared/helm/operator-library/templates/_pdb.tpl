{{/*
Shared pdb template for the operator charts. Defined once here and included
by each operator chart's templates/pdb.yaml with the consuming chart's root
context, so .Chart/.Release/.Values resolve against the operator chart.
*/}}
{{- define "operator-library.pdb" -}}
{{/*
  PodDisruptionBudget for the operator Deployment.

  Rendered when replicas > 1 and podDisruptionBudget.enabled is not false.
  With a single replica, a budget of minAvailable: 1 would block every
  voluntary disruption and wedge node drains. maxUnavailable wins over
  minAvailable; the values schema rejects setting both. With neither set the
  budget keeps one replica, and with it the in-process admission webhook
  (failurePolicy=Fail), available. hasKey, not truthiness, decides: 0 is a
  value. A missing block or a missing enabled key means enabled.
*/}}
{{- $pdb := .Values.podDisruptionBudget | default dict }}
{{- if and (gt (int .Values.replicas) 1) (ne (toString (get $pdb "enabled")) "false") }}
apiVersion: policy/v1
kind: PodDisruptionBudget
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  {{- if hasKey $pdb "maxUnavailable" }}
  maxUnavailable: {{ toJson $pdb.maxUnavailable }}
  {{- else if hasKey $pdb "minAvailable" }}
  minAvailable: {{ toJson $pdb.minAvailable }}
  {{- else }}
  minAvailable: 1
  {{- end }}
  selector:
    matchLabels:
      {{- include "operator-library.selectorLabels" . | nindent 6 }}
{{- end }}
{{- end }}
