{{/*
Shared servicemonitor template for the operator charts. Defined once here and included
by each operator chart's templates/servicemonitor.yaml with the consuming chart's root
context, so .Chart/.Release/.Values resolve against the operator chart.
*/}}
{{- define "operator-library.servicemonitor" -}}
{{- /*
ServiceMonitor for Prometheus scraping of the operator metrics
endpoint. Rendered only when monitoring.serviceMonitor.enabled
is true; relies on prometheus-operator CRDs (monitoring.coreos.com/v1) being
installed in the cluster.
*/ -}}
{{- if .Values.monitoring.serviceMonitor.enabled }}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  selector:
    matchLabels:
      {{- include "operator-library.selectorLabels" . | nindent 6 }}
  endpoints:
    - port: metrics
      path: /metrics
      interval: {{ .Values.monitoring.serviceMonitor.interval | quote }}
{{- end }}
{{- end }}
