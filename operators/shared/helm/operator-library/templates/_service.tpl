{{/*
Shared service template for the operator charts. Defined once here and included
by each operator chart's templates/service.yaml with the consuming chart's root
context, so .Chart/.Release/.Values resolve against the operator chart.
*/}}
{{- define "operator-library.service" -}}
apiVersion: v1
kind: Service
metadata:
  name: {{ include "operator-library.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  type: ClusterIP
  selector:
    {{- include "operator-library.selectorLabels" . | nindent 4 }}
  ports:
    {{- if .Values.webhook.enabled }}
    - name: webhook
      port: 443
      targetPort: 9443
      protocol: TCP
    {{- end }}
    - name: metrics
      port: {{ .Values.metrics.port }}
      targetPort: {{ .Values.metrics.port }}
      protocol: TCP
{{- end }}
