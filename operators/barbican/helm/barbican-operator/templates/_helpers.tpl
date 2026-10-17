{{/*
Chart-specific helpers only. The naming, label and service-account helpers, the
shared manifests (Deployment, RBAC, NetworkPolicy, certificate, service, PDB,
ServiceMonitor, webhook configurations) and the "operator-library.chart.*" hooks
live in the operator-library library chart; the RBAC rules are generated into
_rbac-rules.tpl from the kubebuilder markers (make sync-helm-rbac). A chart
overrides a library hook by defining a template of the same name here.
*/}}


{{/*
OpenBao API egress for the shared NetworkPolicy (operator-library chart hook).
The BarbicanSecretStore controller dials the OpenBao server of every store it
reconciles. Brownfield OpenBao servers are user-supplied, so the destination
cannot be narrowed: the rule is ports-only (TCP 8200) and carries no `to` peer.
It can be disabled entirely when no store uses a non-standard posture.

API health-check egress. The operator GETs the Barbican API over its Service,
and an enforcing CNI judges that connection after the Service DNAT, at the API
pod and its port. The rule therefore selects the API pods in any namespace by
the two labels the operator sets on them (naming.CommonLabels in
internal/common). With OpenBao off the output starts with a newline, which the
library's trim removes.
*/}}
{{- define "operator-library.chart.networkPolicyEgress" -}}
{{- if .Values.networkPolicy.openBao.enabled }}
- ports:
    - protocol: TCP
      port: 8200
{{- end }}
- to:
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          app.kubernetes.io/name: barbican
          app.kubernetes.io/managed-by: barbican-operator
  ports:
    - protocol: TCP
      port: 9311
{{- end }}
