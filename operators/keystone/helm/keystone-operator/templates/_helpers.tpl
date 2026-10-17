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

{{/*
API health-check egress for the shared NetworkPolicy (operator-library chart
hook). The operator GETs the Keystone API over its Service, and an enforcing
CNI judges that connection after the Service DNAT, at the API pod and its
port. The rule therefore selects the API pods in any namespace by the two
labels the operator sets on them (naming.CommonLabels in internal/common).
The KeystoneIdentityBackend controller dials the same Service. Port 5050 is
the OIDC sidecar the Service targets while federation is active.
*/}}
{{- define "operator-library.chart.networkPolicyEgress" -}}
- to:
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          app.kubernetes.io/name: keystone
          app.kubernetes.io/managed-by: keystone-operator
  ports:
    - protocol: TCP
      port: 5000
    - protocol: TCP
      port: 5050
{{- end }}
