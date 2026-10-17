{{/*
Chart-specific helpers only. The naming, label and service-account helpers, the
shared manifests (Deployment, RBAC, NetworkPolicy, certificate, service, PDB,
ServiceMonitor, webhook configurations) and the "operator-library.chart.*" hooks
live in the operator-library library chart; the RBAC rules are generated into
_rbac-rules.tpl from the kubebuilder markers (make sync-helm-rbac). A chart
overrides a library hook by defining a template of the same name here.
*/}}

{{/*
API egress for the shared NetworkPolicy (operator-library chart hook). The
operator GETs the Nova API for its health check, and a NovaCompute pool
authenticates against Keystone, calls the Nova API for its compute services
and aggregates, and calls the Placement API to create and delete its custom
traits. Each call goes through a Service, and an enforcing CNI judges the
connection after the Service DNAT, at the pod and its port, so each rule
selects the pods in any namespace by the two labels their operator sets on
them (naming.CommonLabels in internal/common). Port 5050 is the OIDC sidecar
the Keystone Service targets while federation is active.
*/}}
{{- define "operator-library.chart.networkPolicyEgress" -}}
- to:
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          app.kubernetes.io/name: nova
          app.kubernetes.io/managed-by: nova-operator
  ports:
    - protocol: TCP
      port: 8774
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
