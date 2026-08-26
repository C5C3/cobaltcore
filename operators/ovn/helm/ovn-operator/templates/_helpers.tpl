{{/*
The naming, label, and service-account helpers (.name, .fullname, .chart,
.labels, .selectorLabels, .serviceAccountName) live in the operator-library
library chart so both operator charts share one definition. Templates reference
them as "operator-library.<helper>". Only chart-specific helpers stay here.
*/}}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role.
Extracted into a named template to prevent drift when rules change. These rules
are ovn-specific, so they stay in this chart rather than the library.
They mirror the union of the kubebuilder RBAC markers in
operators/ovn/internal/controller/ovncentral_controller.go and
ovnchassis_controller.go; the leases rule is the one addition, required by the
manager's leader election. Markers that differ only in the resource are folded
into one rule, so the two CR kinds share a rule per verb set.
*/}}
{{- define "ovn-operator.rbacRules" -}}
# ovn.openstack.c5c3.io - ovncentrals, ovnchassis
# No create and no delete: both CR kinds are written by whoever deploys the
# control plane. The operator reads them, stamps their status and manages their
# finalizers, and never brings one into being or takes it away.
- apiGroups:
    - ovn.openstack.c5c3.io
  resources:
    - ovncentrals
    - ovnchassis
  verbs:
    - get
    - list
    - watch
    - update
    - patch
# ovn.openstack.c5c3.io - ovncentrals/status, ovnchassis/status
- apiGroups:
    - ovn.openstack.c5c3.io
  resources:
    - ovncentrals/status
    - ovnchassis/status
  verbs:
    - get
    - update
    - patch
# ovn.openstack.c5c3.io - ovncentrals/finalizers, ovnchassis/finalizers
- apiGroups:
    - ovn.openstack.c5c3.io
  resources:
    - ovncentrals/finalizers
    - ovnchassis/finalizers
  verbs:
    - update
# core - nodes, pods, secrets (read-only)
# The three inputs the operator reads and never writes: nodes and pods for the
# addresses the endpoint step publishes to the chassis layer, secrets for the
# certificate material cert-manager writes.
- apiGroups:
    - ""
  resources:
    - nodes
    - pods
    - secrets
  verbs:
    - get
    - list
    - watch
# core - services, configmaps, persistentvolumeclaims
# persistentvolumeclaims covers the backup volume; the database volumes come
# from the StatefulSet volumeClaimTemplates.
- apiGroups:
    - ""
  resources:
    - services
    - configmaps
    - persistentvolumeclaims
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - events
- apiGroups:
    - ""
  resources:
    - events
  verbs:
    - create
    - patch
# apps - statefulsets, deployments, daemonsets
# statefulsets carry the northbound and southbound databases, deployments the
# northd and relay tiers, daemonsets the per-node chassis pods.
- apiGroups:
    - apps
  resources:
    - statefulsets
    - deployments
    - daemonsets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# batch - jobs, cronjobs
# jobs covers the chassis maintenance Jobs; cronjobs covers the recurring
# database backup.
- apiGroups:
    - batch
  resources:
    - jobs
    - cronjobs
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# cert-manager.io - certificates
# The operator issues the OVN client and server certificates through
# cert-manager and reads back the Secrets they write.
- apiGroups:
    - cert-manager.io
  resources:
    - certificates
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# coordination.k8s.io - leases for leader election
- apiGroups:
    - coordination.k8s.io
  resources:
    - leases
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
{{- end }}
