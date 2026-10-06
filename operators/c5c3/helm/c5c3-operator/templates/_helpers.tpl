{{/*
Chart-specific helpers only. The naming, label and service-account helpers, the
shared manifests (Deployment, RBAC, NetworkPolicy, certificate, service, PDB,
ServiceMonitor, webhook configurations) and the "operator-library.chart.*" hooks
live in the operator-library library chart; the RBAC rules are generated into
_rbac-rules.tpl from the kubebuilder markers (make sync-helm-rbac). A chart
overrides a library hook by defining a template of the same name here.
*/}}

{{/*
Identity of the barbican-operator (operator-library chart hook), rendered as the
BARBICAN_OPERATOR_NAMESPACE and BARBICAN_OPERATOR_SERVICE_ACCOUNT environment
variables: the subject of the TokenRequest grant this operator projects next to
every dedicated OpenBao instance.
*/}}
{{- define "operator-library.chart.env" -}}
{{- with .Values.barbicanOperator }}
- name: BARBICAN_OPERATOR_NAMESPACE
  value: {{ .namespace | quote }}
- name: BARBICAN_OPERATOR_SERVICE_ACCOUNT
  value: {{ .serviceAccount | quote }}
{{- end }}
{{- end }}

{{/*
Why rbac.namespaceScoped=true fails the render (operator-library chart hook).
*/}}
{{- define "operator-library.chart.namespaceScopedUnsupported" -}}
the ControlPlane controller watches cluster-scoped Namespaces, SizingProfiles and ClusterSecretStores and creates ClusterRoleBindings, none of which a namespaced Role can grant, and the manager fails its cache sync at startup
{{- end }}
