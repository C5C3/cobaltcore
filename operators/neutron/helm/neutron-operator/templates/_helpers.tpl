{{/*
Chart-specific helpers only. The naming, label and service-account helpers, the
shared manifests (Deployment, RBAC, NetworkPolicy, certificate, service, PDB,
ServiceMonitor, webhook configurations) and the "operator-library.chart.*" hooks
live in the operator-library library chart; the RBAC rules are generated into
_rbac-rules.tpl from the kubebuilder markers (make sync-helm-rbac). A chart
overrides a library hook by defining a template of the same name here.
*/}}


{{/*
Why rbac.namespaceScoped=true fails the render (operator-library chart hook).
*/}}
{{- define "operator-library.chart.namespaceScopedUnsupported" -}}
a NeutronMetadataAgent lives beside its OVNChassis in a privileged namespace the Neutron API namespace must not share, and the operator reads OVNCentral status across namespaces
{{- end }}

{{/*
API health-check egress for the shared NetworkPolicy (operator-library chart
hook). The operator GETs the Neutron API over its Service, and an enforcing
CNI judges that connection after the Service DNAT, at the API pod and its
port. The rule therefore selects the API pods in any namespace by the two
labels the operator sets on them (naming.CommonLabels in internal/common).
*/}}
{{- define "operator-library.chart.networkPolicyEgress" -}}
- to:
    - namespaceSelector: {}
      podSelector:
        matchLabels:
          app.kubernetes.io/name: neutron
          app.kubernetes.io/managed-by: neutron-operator
  ports:
    - protocol: TCP
      port: 9696
{{- end }}
