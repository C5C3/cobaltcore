{{/*
Chart-specific helpers only. The naming, label and service-account helpers, the
shared manifests (Deployment, RBAC, NetworkPolicy, certificate, service, PDB,
ServiceMonitor, webhook configurations) and the "operator-library.chart.*" hooks
live in the operator-library library chart; the RBAC rules are generated into
_rbac-rules.tpl from the kubebuilder markers (make sync-helm-rbac). A chart
overrides a library hook by defining a template of the same name here.
*/}}

{{/*
Federation metadata allowlist (operator-library chart hook): renders
--federation-metadata-allow-cidrs from federation.metadataAllowCidrs, omitted
when the list is empty so the operator keeps its hardened default.
*/}}
{{- define "operator-library.chart.args" -}}
{{- with .Values.federation }}
{{- with .metadataAllowCidrs }}
- --federation-metadata-allow-cidrs={{ join "," . }}
{{- end }}
{{- end }}
{{- end }}
