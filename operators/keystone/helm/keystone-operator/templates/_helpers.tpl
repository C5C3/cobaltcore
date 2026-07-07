{{/*
The naming, label, and service-account helpers (.name, .fullname, .chart,
.labels, .selectorLabels, .serviceAccountName) live in the operator-library
library chart so both operator charts share one definition. Templates reference
them as "operator-library.<helper>". Only chart-specific helpers stay here.
*/}}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role.
Extracted into a named template to prevent drift when rules change. These rules
are keystone-specific, so they stay in this chart rather than the library.
*/}}
{{- define "keystone-operator.rbacRules" -}}
# keystone.openstack.c5c3.io - keystones
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystones
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# keystone.openstack.c5c3.io - keystones/status
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystones/status
  verbs:
    - get
    - update
    - patch
# keystone.openstack.c5c3.io - keystones/finalizers
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystones/finalizers
  verbs:
    - update
# keystone.openstack.c5c3.io - keystoneidentitybackends
# create/delete are intentionally NOT granted: users create backend CRs; the
# operator only reads them and updates the object (finalizer add/remove).
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystoneidentitybackends
  verbs:
    - get
    - list
    - watch
    - update
# keystone.openstack.c5c3.io - keystoneidentitybackends/status
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystoneidentitybackends/status
  verbs:
    - get
    - update
    - patch
# keystone.openstack.c5c3.io - keystoneidentitybackends/finalizers
- apiGroups:
    - keystone.openstack.c5c3.io
  resources:
    - keystoneidentitybackends/finalizers
  verbs:
    - update
# apps - deployments
- apiGroups:
    - apps
  resources:
    - deployments
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# core - services, configmaps, secrets, serviceaccounts
- apiGroups:
    - ""
  resources:
    - services
    - configmaps
    - secrets
    - serviceaccounts
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
# core - pods (read-only)
# Required for getValidationErrorMessage to list pods of failed policy
# validation Jobs and extract error details from terminated container state.
- apiGroups:
    - ""
  resources:
    - pods
  verbs:
    - get
    - list
# batch - jobs, cronjobs
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
# k8s.mariadb.com - databases, users, grants
- apiGroups:
    - k8s.mariadb.com
  resources:
    - databases
    - users
    - grants
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# k8s.mariadb.com - mariadbs (read-only)
# Required for the operator to observe the referenced MariaDB cluster's
# Ready condition and reflect outages in DatabaseReady.
- apiGroups:
    - k8s.mariadb.com
  resources:
    - mariadbs
  verbs:
    - get
    - list
    - watch
# cert-manager.io - certificates
# Required so the operator can issue the per-Keystone database client
# Certificate (Spec.Database.TLS) and have cert-manager rotate the keypair
# Secret consumed by the Keystone workloads.
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
# external-secrets.io - externalsecrets
# delete is intentionally NOT granted: the operator only manages externalsecret
# lifecycles via owner-references, it never calls r.Delete on them directly.
- apiGroups:
    - external-secrets.io
  resources:
    - externalsecrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
# external-secrets.io - pushsecrets
# delete is required so the openbao-finalizer can tear down the fernet-keys
# and credential-keys backup PushSecrets on Keystone CR deletion and rely on
# ESO DeletionPolicy=Delete to purge the kv-v2 paths.
- apiGroups:
    - external-secrets.io
  resources:
    - pushsecrets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# external-secrets.io - clustersecretstores (read-only)
# Required so the operator can observe the ClusterSecretStore's Ready
# condition and reflect upstream secret-backend (OpenBao) outages in
# SecretsReady. ClusterSecretStore is cluster-scoped; the rule is only
# effective when rendered into the ClusterRole.
- apiGroups:
    - external-secrets.io
  resources:
    - clustersecretstores
  verbs:
    - get
    - list
    - watch
# rbac.authorization.k8s.io - roles, rolebindings
- apiGroups:
    - rbac.authorization.k8s.io
  resources:
    - roles
    - rolebindings
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# policy - poddisruptionbudgets
- apiGroups:
    - policy
  resources:
    - poddisruptionbudgets
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# autoscaling - horizontalpodautoscalers
- apiGroups:
    - autoscaling
  resources:
    - horizontalpodautoscalers
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# networking.k8s.io - networkpolicies
- apiGroups:
    - networking.k8s.io
  resources:
    - networkpolicies
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# gateway.networking.k8s.io - httproutes
# Required to create/update/delete HTTPRoutes that expose Keystone API externally.
- apiGroups:
    - gateway.networking.k8s.io
  resources:
    - httproutes
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# gateway.networking.k8s.io - httproutes/status (read-only)
# Required so the operator can observe the Accepted condition set by the
# upstream Gateway controller and reflect it in HTTPRouteReady.
- apiGroups:
    - gateway.networking.k8s.io
  resources:
    - httproutes/status
  verbs:
    - get
# scheduling.k8s.io - priorityclasses (read-only)
# Required for the webhook to validate that spec.priorityClassName references
# an existing PriorityClass at admission time.
- apiGroups:
    - scheduling.k8s.io
  resources:
    - priorityclasses
  verbs:
    - get
    - list
    - watch
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
