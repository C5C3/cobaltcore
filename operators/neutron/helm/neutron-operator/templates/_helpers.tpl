{{/*
The naming, label, and service-account helpers (.name, .fullname, .chart,
.labels, .selectorLabels, .serviceAccountName) live in the operator-library
library chart so all operator charts share one definition. Templates reference
them as "operator-library.<helper>". Only chart-specific helpers stay here.
*/}}

{{/*
RBAC rules shared between ClusterRole and namespace-scoped Role.
Extracted into a named template to prevent drift when rules change. These rules
are neutron-specific, so they stay in this chart rather than the library.
They mirror the kubebuilder RBAC markers at the top of
operators/neutron/internal/controller/neutron_controller.go, which
neutronmetadataagent_controller.go repeats verbatim; the leases rule is the one
addition, required by the manager's leader election. One rule per marker, in
marker order.
*/}}
{{- define "neutron-operator.rbacRules" -}}
# neutron.openstack.c5c3.io - neutrons
- apiGroups:
    - neutron.openstack.c5c3.io
  resources:
    - neutrons
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# neutron.openstack.c5c3.io - neutrons/status
- apiGroups:
    - neutron.openstack.c5c3.io
  resources:
    - neutrons/status
  verbs:
    - get
    - update
    - patch
# neutron.openstack.c5c3.io - neutrons/finalizers
- apiGroups:
    - neutron.openstack.c5c3.io
  resources:
    - neutrons/finalizers
  verbs:
    - update
# neutron.openstack.c5c3.io - neutronmetadataagents
- apiGroups:
    - neutron.openstack.c5c3.io
  resources:
    - neutronmetadataagents
  verbs:
    - get
    - list
    - watch
    - create
    - update
    - patch
    - delete
# neutron.openstack.c5c3.io - neutronmetadataagents/status
- apiGroups:
    - neutron.openstack.c5c3.io
  resources:
    - neutronmetadataagents/status
  verbs:
    - get
    - update
    - patch
# neutron.openstack.c5c3.io - neutronmetadataagents/finalizers
- apiGroups:
    - neutron.openstack.c5c3.io
  resources:
    - neutronmetadataagents/finalizers
  verbs:
    - update
# ovn.openstack.c5c3.io - ovncentrals, ovnchassis (read-only)
# Both kinds belong to ovn-operator. The Neutron OVN step reads the OVNCentral
# for the two connection strings and the client certificate the [ovn] config
# section needs; the agent chassis step reads the OVNChassis a
# NeutronMetadataAgent runs beside. Neither is written.
- apiGroups:
    - ovn.openstack.c5c3.io
  resources:
    - ovncentrals
    - ovnchassis
  verbs:
    - get
    - list
    - watch
# core - services, configmaps, secrets
- apiGroups:
    - ""
  resources:
    - services
    - configmaps
    - secrets
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
# apps - deployments, daemonsets
# deployments carry the Neutron API server, daemonsets the per-node metadata
# agent.
- apiGroups:
    - apps
  resources:
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
# jobs covers the db-sync Job that runs neutron-db-manage upgrade head;
# cronjobs covers the recurring ovn-db-sync run.
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
# rabbitmq.com - rabbitmqclusters (read-only)
# In managed messaging mode the transport URL is derived from the named
# RabbitmqCluster instead of read from a Secret, so the operator reads and
# watches the cluster.
- apiGroups:
    - rabbitmq.com
  resources:
    - rabbitmqclusters
  verbs:
    - get
    - list
    - watch
# external-secrets.io - externalsecrets (read-only)
# The database, service-user and messaging credential Secrets are ESO-managed;
# the operator only reads the ExternalSecrets to attribute a not-synced Secret
# in SecretsReady messages.
- apiGroups:
    - external-secrets.io
  resources:
    - externalsecrets
  verbs:
    - get
    - list
    - watch
# external-secrets.io - clustersecretstores, secretstores (read-only)
# Required so the operator can observe the selected store's Ready condition and
# reflect upstream secret-backend outages in SecretsReady. A CR selects either
# the shared cluster-scoped ClusterSecretStore (default) or a namespaced
# SecretStore, so both kinds must be watchable.
- apiGroups:
    - external-secrets.io
  resources:
    - clustersecretstores
    - secretstores
  verbs:
    - get
    - list
    - watch
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
# Required to create/update/delete HTTPRoutes that expose the Neutron API
# externally.
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
