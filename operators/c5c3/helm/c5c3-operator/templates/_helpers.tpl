{{/*
Chart-specific helpers only. The naming, label and service-account helpers, the
shared manifests (Deployment, RBAC, NetworkPolicy, certificate, service, PDB,
ServiceMonitor, webhook configurations) and the "operator-library.chart.*" hooks
live in the operator-library library chart; the RBAC rules are generated into
_rbac-rules.tpl from the kubebuilder markers (make sync-helm-rbac). A chart
overrides a library hook by defining a template of the same name here.
*/}}

{{/*
Environment of the manager container (operator-library chart hook). The identity
of the barbican-operator, rendered as BARBICAN_OPERATOR_NAMESPACE and
BARBICAN_OPERATOR_SERVICE_ACCOUNT, is the subject of the TokenRequest grant this
operator projects next to every dedicated OpenBao instance. The namespace of the
openbao-operator, rendered as OPENBAO_OPERATOR_NAMESPACE, is where it creates the
OpenBaoTenant that admits that instance's namespace.
*/}}
{{- define "operator-library.chart.env" -}}
{{- with .Values.barbicanOperator }}
- name: BARBICAN_OPERATOR_NAMESPACE
  value: {{ .namespace | quote }}
- name: BARBICAN_OPERATOR_SERVICE_ACCOUNT
  value: {{ .serviceAccount | quote }}
{{- end }}
{{- with .Values.openbaoOperator }}
- name: OPENBAO_OPERATOR_NAMESPACE
  value: {{ .namespace | quote }}
{{- end }}
{{- end }}

{{/*
Why rbac.namespaceScoped=true fails the render (operator-library chart hook).
*/}}
{{- define "operator-library.chart.namespaceScopedUnsupported" -}}
the ControlPlane controller watches cluster-scoped Namespaces, SizingProfiles and ClusterSecretStores and creates ClusterRoleBindings, none of which a namespaced Role can grant, and the manager fails its cache sync at startup
{{- end }}
