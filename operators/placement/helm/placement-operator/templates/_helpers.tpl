{{/*
Chart-specific helpers only. The naming, label and service-account helpers, the
shared manifests (Deployment, RBAC, NetworkPolicy, certificate, service, PDB,
ServiceMonitor, webhook configurations) and the "operator-library.chart.*" hooks
live in the operator-library library chart; the RBAC rules are generated into
_rbac-rules.tpl from the kubebuilder markers (make sync-helm-rbac). A chart
overrides a library hook by defining a template of the same name here.
*/}}

{{/*
API health-check egress for the shared NetworkPolicy (operator-library chart
hook). The operator GETs the Placement API over its Service, and an enforcing
CNI judges that connection after the Service DNAT, at the API pod and its
port. The rule therefore selects the API pods in any namespace by the two
labels the operator sets on them (naming.CommonLabels in internal/common).
*/}}
{{- define "operator-library.chart.networkPolicyEgress" -}}
- to:
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          app.kubernetes.io/name: placement
          app.kubernetes.io/managed-by: placement-operator
  ports:
    - protocol: TCP
      port: 8778
{{- end }}
