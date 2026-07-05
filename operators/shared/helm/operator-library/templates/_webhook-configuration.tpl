{{/*
Shared Mutating/ValidatingWebhookConfiguration template for the operator
charts. The manifests are structurally identical across operators — only the
webhook names, admission paths, API group, and resource differ — so the shape
is defined once here and each operator chart's
templates/webhook-configuration.yaml passes its four varying facts via a dict:

  root                  the consuming chart's root context
  mutatingWebhookName   e.g. "mkeystone.kb.io"
  validatingWebhookName e.g. "vkeystone.kb.io"
  mutatePath            the mutating admission path served by the operator
  validatePath          the validating admission path served by the operator
  apiGroup              the CR's API group
  resource              the CR's plural resource name
*/}}
{{- define "operator-library.webhookConfiguration" -}}
{{- $root := .root -}}
{{- if $root.Values.webhook.enabled -}}
apiVersion: admissionregistration.k8s.io/v1
kind: MutatingWebhookConfiguration
metadata:
  name: {{ include "operator-library.fullname" $root }}-mutating
  labels:
    {{- include "operator-library.labels" $root | nindent 4 }}
  annotations:
    cert-manager.io/inject-ca-from: {{ $root.Release.Namespace }}/{{ include "operator-library.fullname" $root }}-webhook
webhooks:
  - name: {{ .mutatingWebhookName }}
    admissionReviewVersions:
      - v1
    clientConfig:
      service:
        name: {{ include "operator-library.fullname" $root }}
        namespace: {{ $root.Release.Namespace }}
        path: {{ .mutatePath }}
    failurePolicy: Fail
    sideEffects: None
    rules:
      - apiGroups:
          - {{ .apiGroup }}
        apiVersions:
          - v1alpha1
        operations:
          - CREATE
          - UPDATE
        resources:
          - {{ .resource }}
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingWebhookConfiguration
metadata:
  name: {{ include "operator-library.fullname" $root }}-validating
  labels:
    {{- include "operator-library.labels" $root | nindent 4 }}
  annotations:
    cert-manager.io/inject-ca-from: {{ $root.Release.Namespace }}/{{ include "operator-library.fullname" $root }}-webhook
webhooks:
  - name: {{ .validatingWebhookName }}
    admissionReviewVersions:
      - v1
    clientConfig:
      service:
        name: {{ include "operator-library.fullname" $root }}
        namespace: {{ $root.Release.Namespace }}
        path: {{ .validatePath }}
    failurePolicy: Fail
    sideEffects: None
    rules:
      - apiGroups:
          - {{ .apiGroup }}
        apiVersions:
          - v1alpha1
        # No DELETE: the webhook is served in-process by the operator, so with
        # failurePolicy=Fail a DELETE rule would let a down operator block CR
        # and namespace deletion. ValidateDelete is a no-op anyway.
        operations:
          - CREATE
          - UPDATE
        resources:
          - {{ .resource }}
{{- end }}
{{- end }}
