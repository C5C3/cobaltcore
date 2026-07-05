{{/*
Shared certificate template for the operator charts. Defined once here and included
by each operator chart's templates/certificate.yaml with the consuming chart's root
context, so .Chart/.Release/.Values resolve against the operator chart.
*/}}
{{- define "operator-library.certificate" -}}
{{- if .Values.webhook.enabled -}}
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: {{ include "operator-library.fullname" . }}-selfsigned
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: {{ include "operator-library.fullname" . }}-webhook
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "operator-library.labels" . | nindent 4 }}
spec:
  dnsNames:
    - {{ include "operator-library.fullname" . }}.{{ .Release.Namespace }}.svc
    - {{ include "operator-library.fullname" . }}.{{ .Release.Namespace }}.svc.cluster.local
  issuerRef:
    kind: Issuer
    name: {{ include "operator-library.fullname" . }}-selfsigned
  secretName: {{ include "operator-library.fullname" . }}-webhook-cert
{{- end }}
{{- end }}
