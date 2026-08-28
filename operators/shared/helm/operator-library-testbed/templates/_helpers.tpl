{{/*
The operator charts generate their RBAC rules from kubebuilder markers
(make sync-helm-rbac); this test consumer has no Go module, so it carries a
small hand-written rule set under the same "<chart name>.rbacRules" name the
operator-library ClusterRole and Role templates resolve via .Chart.Name.

No "operator-library.chart.*" hook is overridden here on purpose: the suites
in tests/ pin the library's default (hook-less) rendering; the operator charts
that override a hook pin that in their own suites.
*/}}
{{- define "operator-library-testbed.rbacRules" -}}
- apiGroups:
  - testbed.c5c3.io
  resources:
  - testbeds
  verbs:
  - get
  - list
  - watch
- apiGroups:
  - coordination.k8s.io
  resources:
  - leases
  verbs:
  - create
  - delete
  - get
  - list
  - patch
  - update
  - watch
{{- end }}
