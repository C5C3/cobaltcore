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
