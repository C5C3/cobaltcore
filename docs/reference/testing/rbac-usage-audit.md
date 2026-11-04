---
title: RBAC Usage Audit
quadrant: operator
---

# RBAC Usage Audit

Every operator's ClusterRole comes from the `+kubebuilder:rbac` markers in its
Go sources: `make manifests` renders them into
`operators/<op>/config/rbac/role.yaml`, and `make sync-helm-rbac` writes the
rules into the chart's `templates/_rbac-rules.tpl`
([packaging](../backend/keystone-operator-packaging.md#shared-library-subchart)).
The markers are written by hand, so nothing in that chain shows whether an
operator still issues each verb it is granted. The RBAC usage audit answers
that from the API server's side. Two CI jobs run their kind cluster with a
kube-apiserver audit log, and `hack/audit-rbac-usage.py` compares the requests
each operator's ServiceAccount made with the ClusterRole its markers render.

A granted verb that no request used is removed from its marker unless the
operator's code has a call site for it. A verb the suites never exercise but
the code calls keeps its marker, with a comment naming the call site.

## How the audit works

### The audited cluster

`hack/kind-config-audit.yaml` is `hack/kind-config.yaml` with a kubeadm patch
that turns the audit log on. The patch passes four flags to the API server and
mounts the policy file from the repository:

| Flag | Value |
| --- | --- |
| `--audit-policy-file` | `/etc/kubernetes/policies/audit-policy.yaml`, mounted from `hack/audit-policy.yaml` |
| `--audit-log-path` | `/var/log/kubernetes/kube-apiserver-audit.log` on the control-plane node |
| `--audit-log-maxsize` | `100` (MB per file) |
| `--audit-log-maxbackup` | `10` rotated files, so at most about 1.1 GB on the node |

The port mappings are the same as in `hack/kind-config.yaml`, so the Gateway
answers on `https://*.127-0-0-1.nip.io` as on every other kind cluster.
`tests/unit/hack/kind_config_audit_test.sh` keeps both files in step. Only the
`e2e-operator` and `e2e-controlplane` jobs create their cluster from it; the
other jobs keep `hack/kind-config.yaml`.

### The audit policy

`hack/audit-policy.yaml` has two rules:

| Rule | Level | Requests |
| --- | --- | --- |
| 1 | `Metadata`, without managed fields | every user in the group `system:serviceaccounts` |
| 2 | `None` | everything else, including the suites' own requests as `kubernetes-admin` |

`Metadata` records the user, the verb, the object reference and the response
code, and never a request or response body. The policy omits the
`RequestReceived` stage, which carries nothing the diff needs and would double
the number of events.

### The diff

The operator's identity is the ServiceAccount `<op>-operator` in `<op>-system`,
which is what `hack/ci-deploy-operator.sh` installs. Each audit event of that
user with an object reference becomes one triple: the API group, the resource
(with `/<subresource>` when the request named one), and the verb. Discovery
requests (`/api`, `/apis`) carry no object reference and are not counted.

The granted triples come from the `<op>-operator` ClusterRole in
`operators/<op>/config/rbac/role.yaml`, plus the leases rule
`hack/gen-helm-rbac-rules.py` appends for leader election. A `*` in a rule
matches every value, as RBAC matches it.

Three rules map events onto verbs the audit log does not name:

- A `patch` or `update` answered with `201` created the object. The API server
  authorizes that creation as `create`, so the event uses `create` as well.
  Server-Side Apply of an object that does not exist yet is the common case:
  `apply.EnsureObject` in `internal/common/apply/apply.go` creates children
  this way.
- A `watch` with `sendInitialEvents=true` is an informer's initial list. The
  client-go reflector streams it over a watch where the API server supports
  it, which the Kubernetes 1.37 kind node does, and falls back to a plain
  `list` request where it does not. The event uses `list` as well, so a kind
  the operator caches does not show `list` as unused.
- An unexercised `get` is not reported while `list` on the same kind is
  granted. A `list` returns every object a `get` could read, so removing the
  `get` takes no capability away. The operators read through the cached client,
  whose informers issue `list` and `watch` and never `get`. The same holds for
  `get` on the kind's `status` subresource, which returns the same object.

## Reading the report

`hack/audit-rbac-usage.py` prints three sections per operator, then the counts:

| Section | Content | Expected |
| --- | --- | --- |
| `USED, NOT GRANTED` | triples the operator used that no rule of its ClusterRole covers, with the API server's authorization reason | empty |
| `UNUSED` | per rule and per kind of it, the granted verbs no request used; a rule whose verbs are `*` reports nothing | what the audit trims or justifies |
| `GET IMPLIED BY LIST` | the kinds whose unexercised `get` the second mapping rule keeps out of `UNUSED` | informational |

A row under `USED, NOT GRANTED` is either a grant another binding supplies
(barbican's `tokenrequest-rbac.yaml` Role, or a default ClusterRole the
authorization reason names) or a request the mapping rules above do not cover
yet. With `--json <file>` the script writes the same data as
`{"<op>": {"used", "unused", "used_not_granted", "get_implied_by_list", "events", "skipped_lines"}}`.

| Exit code | Meaning |
| --- | --- |
| `0` | a report, also one with findings |
| `1` | `--fail-on-unused` and at least one `UNUSED` row |
| `2` | an audit log cannot be read, no event carries the operator's username, or `role.yaml` has no `<op>-operator` ClusterRole |

## In CI

After the diagnostics dumps, the `Report RBAC usage` step of `e2e-operator`
and `e2e-controlplane` runs `hack/ci-rbac-usage-report.sh` under
`continue-on-error`. The step never fails the job: a code path the suites skip
is no reason to fail correct code. It copies the audit log out of the
control-plane node, keeps the operator's events, runs the diff, and prints a
`::notice::` with the counts and a `::warning::` when a triple was used but not
granted. A second warning appears when the log rotated ten times, because the
oldest events, such as an informer's first `list`, may then be gone.

| Job | `OPERATOR` | Artifact |
| --- | --- | --- |
| `e2e-operator` | the matrix operator | `rbac-usage-<operator>`, `rbac-usage-nova-<shard>` on a nova shard |
| `e2e-controlplane` | `c5c3` | `rbac-usage-e2e-controlplane` |

Each artifact holds `audit.jsonl` (the operator's events) and
`<operator>.json` (the findings). The c5c3-operator appears in two artifacts,
`rbac-usage-c5c3` and `rbac-usage-e2e-controlplane`, and the nova-operator in
one per shard. A verb used in any of them counts as used, so diff them
together:

```bash
gh run download <run-id> --pattern 'rbac-usage-*' --dir _output/rbac-usage-run
python3 hack/audit-rbac-usage.py --operator c5c3 \
  --audit-log _output/rbac-usage-run/rbac-usage-c5c3/audit.jsonl \
  --audit-log _output/rbac-usage-run/rbac-usage-e2e-controlplane/audit.jsonl
```

## Running it locally

1. Create the audited cluster from the repository root, so the relative
   `hostPath` of the policy mount resolves:

   ```bash
   KIND_CONFIG=hack/kind-config-audit.yaml CLUSTER_NAME=cobaltcore hack/ci-create-kind-cluster.sh
   ```

2. Deploy the stack onto it. `SKIP_KIND_CREATE=true` keeps
   `hack/deploy-infra.sh` from creating a cluster of its own:

   ```bash
   SKIP_KIND_CREATE=true make deploy-infra
   ```

3. Build and deploy the operator as the `e2e-operator` job does:

   ```bash
   make image OPERATOR=keystone IMG=ghcr.io/c5c3/keystone-operator:dev
   kind load docker-image ghcr.io/c5c3/keystone-operator:dev --name cobaltcore
   OPERATOR=keystone IMAGE_REPO=ghcr.io/c5c3/keystone-operator NAMESPACE=keystone-system \
     hack/ci-deploy-operator.sh
   ```

4. Run the suites whose requests you want in the log, for example:

   ```bash
   mkdir -p _output/reports
   chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/basic-deployment/
   ```

5. Run the report. The target passes `OPERATOR` to
   `hack/ci-rbac-usage-report.sh`, which also reads `KIND_CLUSTER`,
   `OPERATOR_NAMESPACE` and `OUT_DIR`:

   ```bash
   make audit-rbac-usage OPERATOR=keystone
   ```

On a cluster created from `hack/kind-config.yaml` the script exits 1 with
`no audit event: was the cluster created with hack/kind-config-audit.yaml?`.

## Acting on an UNUSED row

The suites do not reach every code path, so an `UNUSED` verb is a candidate,
not a verdict. For each one, search `operators/<op>/` and `internal/common/`:

- `list` and `watch` on a kind stay when the operator reads the kind through
  the cached client, owns or watches it in a controller builder (`For`, `Owns`,
  `Watches`), or indexes it.
- `create`, `update`, `patch`, `delete` and `deletecollection` stay when the
  code calls `.Create(`, `.Update(`, `.Patch(`, `.Delete(` or `.DeleteAllOf(`
  with the kind's Go type, also through `apply.EnsureObject`,
  `apply.EnsureUnownedObject` or `reconcile.DeleteOrphanedChildFunc`.
- `get` on a kind whose `list` leaves the marker no longer follows `list`. It
  stays when the code reads the kind through an uncached reader
  (`mgr.GetAPIReader()`, or an unstructured object, which the manager client
  fetches uncached).

Some verbs are authorized by the API server without being a request's verb, so
the audit log never shows them. They stay by the code that needs them:

- The roles support the `OwnerReferencesPermissionEnforcement` admission
  plugin, which kind does not enable. Under it, a child created with an owner
  reference that sets `blockOwnerDeletion` needs `update` on the owner kind's
  `finalizers` subresource. A change of the owner reference on an existing child,
  such as after the owner is recreated or its API version changes, needs
  `delete` on the child. So `<kind>/finalizers` keeps `update` while the
  operator sets owner references with an owner of that kind, and a child kind
  keeps `delete` while the operator applies it with an owner reference
  (`apply.EnsureObject`), even without a `.Delete(` call. The
  target-cluster-access chart grants the `finalizers` subresource for the same
  plugin.
- An operator that writes a Role may grant in it only verbs it holds itself,
  and it may bind only roles whose verbs it holds or on which it holds `bind`.
  The keystone-operator's rotation Role grants `patch` on a Secret, so the
  keystone-operator keeps `patch` on Secrets. The c5c3-operator's Role for a
  dedicated OpenBao grants `create` on `serviceaccounts/token`, and its
  ClusterRoleBinding references `system:auth-delegator`, for which it holds
  `bind` on that one ClusterRole.
- client-go's event recorder patches an event when the same event repeats,
  so `events` keeps `patch` beside `create` wherever an operator records events.
- The leases rule that `hack/gen-helm-rbac-rules.py` appends for leader election
  has no marker and no call site in the operators. client-go's lease lock issues
  only `get`, `create` and `update` on its lease, so the rule grants those
  three.

A verb that stays without a trace gets a comment line beside its marker that
names the call site:

```go
// Unexercised by the e2e suites; used by <file>:<line> (<function>) to <verb> <resource>.
```

A verb with no such call site leaves the marker. After the edit, `make
manifests`, `make sync-helm-rbac` and `make verify-helm-rbac` regenerate and
check the chart rules, and the chart's version rises by a patch with an
`artifacthub.io/changes` entry.

## First audit

The first audit ran on the `ci:full` run of pull request #1505, before any
marker changed. Every operator's ClusterRole was trimmed by its result.

| Item | Value |
| --- | --- |
| Run | [39396147282](https://github.com/C5C3/cobaltcore/actions/runs/39396147282) on 2026-10-20 |
| Measured commit | `e2322b8ec5f2214ee3bc8f3ea1d66779a1442ded` |
| Artifacts | one `rbac-usage-<operator>` per `e2e-operator` job, both nova shards, and the `e2e-controlplane` report, which that run still uploaded as a second `rbac-usage-c5c3` |
| Diff | the nova shards and the two c5c3 reports diffed together, each against the ClusterRole of the measured commit |

Each table lists every `UNUSED` row of its operator, one line per kind,
outcome and site. A removed verb names the marker that grants the kind's
remaining verbs, or says the marker went away. A kept verb names the call site
its marker comment names. The four lease verbs `list`, `watch`, `patch` and
`delete` were unused on every operator and left the generator's leases rule.
No operator used a triple its role does not grant, apart from barbican's
TokenRequest.

### keystone

30832 events and 105 used triples. Of 49 unused verbs, 32 were removed and 17 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `update`, `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:408` |
| `secrets` | `patch` | kept | `internal/common/rotation/rotation.go:248` (`EnsureRBAC`) |
| `serviceaccounts` | `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:412` |
| `serviceaccounts` | `update` | kept | `internal/common/rotation/rotation.go:241` (`EnsureRBAC`) |
| `services` | `delete` | kept | `operators/keystone/internal/controller/reconcile_deployment.go:149` (`reconcileDeployment`) |
| `services` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:407` |
| `pods` | `list` | kept | `operators/keystone/internal/controller/reconcile_policyvalidation.go:129` (`getValidationErrorMessage`) |
| `apps/deployments` | `delete` | kept | `operators/keystone/internal/controller/reconcile_deployment.go:105` (`reconcileDeployment`) |
| `apps/deployments` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:405` |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:447` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/vpa_flow.go:190` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `delete` | kept | `internal/common/deployment/vpa_flow.go:213` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/keystone/internal/controller/keystone_controller.go:1177` (`setupWithOptions`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:451` |
| `batch/cronjobs` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:421` |
| `batch/jobs` | `update`, `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:420` |
| `cert-manager.io/certificates` | `create`, `update` | kept | `operators/keystone/internal/controller/reconcile_databasetls.go:146` (`reconcileDatabaseTLS`) |
| `cert-manager.io/certificates` | `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:430` |
| `external-secrets.io/externalsecrets` | `create`, `update`, `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:433` |
| `external-secrets.io/pushsecrets` | `update` | kept | `operators/keystone/internal/controller/reconcile_secrets.go:295` (`finalizeOpenBaoSecrets`) |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:454` |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:422` |
| `k8s.mariadb.com/grants` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:422` |
| `k8s.mariadb.com/users` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:422` |
| `keystone.openstack.c5c3.io/keystoneidentitybackends/finalizers` | `update` | removed | marker removed |
| `keystone.openstack.c5c3.io/keystones/finalizers` | `update` | kept | `operators/keystone/internal/controller/reconcile_fernet.go:214` (`createKeysSecret`) |
| `keystone.openstack.c5c3.io/keystones/status` | `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:397` |
| `keystone.openstack.c5c3.io/keystones` | `create`, `delete` | removed | `operators/keystone/internal/controller/keystone_controller.go:396` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:452` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/keystone/internal/controller/reconcile_deployment.go:154` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/keystone/internal/controller/keystone_controller.go:446` |
| `rbac.authorization.k8s.io/rolebindings` | `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:460` |
| `rbac.authorization.k8s.io/rolebindings` | `update` | kept | `internal/common/rotation/rotation.go:269` (`EnsureRBAC`) |
| `rbac.authorization.k8s.io/roles` | `patch` | removed | `operators/keystone/internal/controller/keystone_controller.go:460` |
| `rbac.authorization.k8s.io/roles` | `update` | kept | `internal/common/rotation/rotation.go:248` (`EnsureRBAC`) |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/keystone/internal/controller/keystone_controller.go:463` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

### c5c3

29677 events and 258 used triples. Of 112 unused verbs, 65 were removed and 47 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `barbican.openstack.c5c3.io/barbicans` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:739` (`deleteOrphanedBarbican`) |
| `barbican.openstack.c5c3.io/barbicans` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:343` |
| `barbican.openstack.c5c3.io/barbicansecretstores` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:652` (`reconcileBarbicanSecretStore`) |
| `barbican.openstack.c5c3.io/barbicansecretstores` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:345` |
| `c5c3.io/controlplanes` | `create`, `patch`, `delete` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:275` |
| `c5c3.io/credentialrotations` | `create`, `update`, `patch`, `delete` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:279` |
| `c5c3.io/keystoneservices` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_delete.go:1678` (`deleteProjectedRegistrations`) |
| `c5c3.io/controlplanes/finalizers` | `update` | kept | `operators/c5c3/internal/controller/reconcile_korc.go:368` (`reconcileKORC`) |
| `c5c3.io/keystoneapplicationcredentials/finalizers` | `update` | kept | `operators/c5c3/internal/controller/keystoneapplicationcredential_delivery.go:166` (`deliverCredential`) |
| `c5c3.io/keystonecatalogentries/finalizers` | `update` | removed | marker removed |
| `c5c3.io/keystoneprojects/finalizers` | `update` | removed | marker removed |
| `c5c3.io/keystoneroleassignments/finalizers` | `update` | removed | marker removed |
| `c5c3.io/keystoneservices/finalizers` | `update` | kept | `operators/c5c3/internal/controller/keystoneservice_controller.go:678` (`claimKeystoneServiceChild`) |
| `c5c3.io/keystoneusers/finalizers` | `update` | kept | `operators/c5c3/internal/controller/keystoneuser_delivery.go:163` (`deliverCredentials`) |
| `c5c3.io/mariadbdatabases/finalizers` | `update` | kept | `operators/c5c3/internal/controller/mariadbdatabase_delivery.go:149` (`deliverCredentials`) |
| `c5c3.io/rabbitmqvhosts/finalizers` | `update` | kept | `operators/c5c3/internal/controller/rabbitmqvhost_delivery.go:186` (`deliverCredentials`) |
| `c5c3.io/controlplanes/status` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:276` |
| `c5c3.io/credentialrotations/status` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:280` |
| `c5c3.io/keystoneapplicationcredentials/status` | `patch` | removed | `operators/c5c3/internal/controller/keystoneapplicationcredential_controller.go:144` |
| `c5c3.io/keystonecatalogentries/status` | `patch` | removed | `operators/c5c3/internal/controller/keystonecatalogentry_controller.go:87` |
| `c5c3.io/keystoneprojects/status` | `patch` | removed | `operators/c5c3/internal/controller/keystoneproject_controller.go:106` |
| `c5c3.io/keystoneroleassignments/status` | `patch` | removed | `operators/c5c3/internal/controller/keystoneroleassignment_controller.go:118` |
| `c5c3.io/keystoneservices/status` | `patch` | removed | `operators/c5c3/internal/controller/keystoneservice_controller.go:140` |
| `c5c3.io/keystoneusers/status` | `patch` | removed | `operators/c5c3/internal/controller/keystoneuser_controller.go:140` |
| `c5c3.io/mariadbdatabases/status` | `patch` | removed | `operators/c5c3/internal/controller/mariadbdatabase_controller.go:240` |
| `c5c3.io/rabbitmqvhosts/status` | `patch` | removed | `operators/c5c3/internal/controller/rabbitmqvhost_controller.go:179` |
| `c5c3.io/keystoneapplicationcredentials` | `patch` | removed | `operators/c5c3/internal/controller/keystoneapplicationcredential_controller.go:143` |
| `c5c3.io/keystonecatalogentries` | `patch` | removed | `operators/c5c3/internal/controller/keystonecatalogentry_controller.go:86` |
| `c5c3.io/keystoneprojects` | `patch` | removed | `operators/c5c3/internal/controller/keystoneproject_controller.go:105` |
| `c5c3.io/keystoneroleassignments` | `patch` | removed | `operators/c5c3/internal/controller/keystoneproject_controller.go:107` |
| `c5c3.io/keystoneusers` | `patch` | removed | `operators/c5c3/internal/controller/keystoneuser_controller.go:139` |
| `c5c3.io/mariadbdatabases` | `patch` | removed | `operators/c5c3/internal/controller/mariadbdatabase_controller.go:239` |
| `c5c3.io/rabbitmqvhosts` | `patch` | removed | `operators/c5c3/internal/controller/rabbitmqvhost_controller.go:178` |
| `c5c3.io/secretaggregates` | `list`, `watch` | removed | marker removed |
| `cinder.openstack.c5c3.io/cinderbackends` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_cinder.go:283` (`reconcileCinderBackends`) |
| `cinder.openstack.c5c3.io/cinderbackends` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:358` |
| `cinder.openstack.c5c3.io/cinderbackupbackends` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_cinder.go:323` (`reconcileCinderBackupBackend`) |
| `cinder.openstack.c5c3.io/cinderbackupbackends` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:358` |
| `cinder.openstack.c5c3.io/cinders` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_cinder.go:790` (`deleteOrphanedCinder`) |
| `cinder.openstack.c5c3.io/cinders` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:358` |
| `glance.openstack.c5c3.io/glancebackends` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_glance.go:566` (`reconcileGlanceBackends`) |
| `glance.openstack.c5c3.io/glancebackends` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:333` |
| `glance.openstack.c5c3.io/glances` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_glance.go:456` (`deleteOrphanedGlance`) |
| `glance.openstack.c5c3.io/glances` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:331` |
| `horizon.openstack.c5c3.io/horizons` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_horizon.go:541` (`deleteOrphanedHorizon`) |
| `horizon.openstack.c5c3.io/horizons` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:325` |
| `horizon.openstack.c5c3.io/horizons` | `update` | kept | `operators/c5c3/internal/controller/reconcile_horizon.go:209` (`reconcileHorizon`) |
| `keystone.openstack.c5c3.io/keystones` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:316` |
| `memcached.c5c3.io/memcacheds` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:309` |
| `neutron.openstack.c5c3.io/neutrons` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_neutron.go:601` (`deleteOrphanedNeutron`) |
| `neutron.openstack.c5c3.io/neutrons` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:349` |
| `nova.openstack.c5c3.io/novas` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_nova.go:936` (`deleteOrphanedNova`) |
| `nova.openstack.c5c3.io/novas` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:360` |
| `placement.openstack.c5c3.io/placements` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_placement.go:404` (`deleteOrphanedPlacement`) |
| `placement.openstack.c5c3.io/placements` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:337` |
| `serviceaccounts` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:458` |
| `serviceaccounts/token` | `create` | kept | `operators/c5c3/internal/controller/reconcile_barbican_openbao.go:198` (`ensureBarbicanOpenBao`) |
| `cert-manager.io/certificates` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:453` |
| `cert-manager.io/certificates` | `update` | kept | `operators/c5c3/internal/controller/reconcile_dbcredentials.go:747` (`ensureDynamicDBCredentialObjects`) |
| `external-secrets.io/secretstores` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:441` |
| `generators.external-secrets.io/passwords` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_nova_metadata_secret.go:275` (`deleteGeneratedNovaMetadataSecret`) |
| `generators.external-secrets.io/passwords` | `list`, `watch`, `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:449` |
| `generators.external-secrets.io/vaultdynamicsecrets` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:446` |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/c5c3/internal/controller/mariadbdatabase_controller.go:243` |
| `k8s.mariadb.com/mariadbs` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_delete.go:1541` (`sweepExternalNamespaceResidue`) |
| `k8s.mariadb.com/mariadbs` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:306` |
| `k8s.mariadb.com/mariadbs` | `update` | kept | `operators/c5c3/internal/controller/reconcile_infrastructure.go:771` (`ensureMariaDB`) |
| `openbao.org/openbaoclusters` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:766` (`deleteOrphanedBarbican`) |
| `openbao.org/openbaoclusters` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:376` |
| `openbao.org/openbaotenants` | `create` | kept | `operators/c5c3/internal/controller/reconcile_barbican_openbao.go:264` (`ensureBarbicanOpenBaoTenant`) |
| `openbao.org/openbaotenants` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:828` (`deleteOwnedEnsembleObject`) |
| `openbao.org/openbaotenants` | `update`, `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:379` |
| `openstack.k-orc.cloud/applicationcredentials` | `update` | kept | `operators/c5c3/internal/controller/reconcile_credentialrotation.go:520` (`clearPasswordHashAnnotation`) |
| `openstack.k-orc.cloud/domains` | `update` | kept | `operators/c5c3/internal/controller/reconcile_delete.go:2228` (`forceRemoveKORCFinalizers`) |
| `openstack.k-orc.cloud/endpoints` | `update` | kept | `operators/c5c3/internal/controller/reconcile_delete.go:2228` (`forceRemoveKORCFinalizers`) |
| `openstack.k-orc.cloud/projects` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:426` |
| `openstack.k-orc.cloud/regions` | `update` | kept | `operators/c5c3/internal/controller/reconcile_delete.go:2228` (`forceRemoveKORCFinalizers`) |
| `openstack.k-orc.cloud/roleassignments` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:426` |
| `openstack.k-orc.cloud/roles` | `update` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:426` |
| `openstack.k-orc.cloud/services` | `update` | kept | `operators/c5c3/internal/controller/reconcile_delete.go:2228` (`forceRemoveKORCFinalizers`) |
| `openstack.k-orc.cloud/applicationcredentials/status` | `patch` | kept | `operators/c5c3/internal/controller/reconcile_korc.go:389` (`reconcileKORC`) |
| `openstack.k-orc.cloud/domains/status` | `patch` | kept | `operators/c5c3/internal/controller/reconcile_korc.go:232` (`reconcileKORC`) |
| `openstack.k-orc.cloud/endpoints/status` | `patch` | kept | `operators/c5c3/internal/controller/reconcile_catalog.go:195` (`reconcileCatalog`) |
| `openstack.k-orc.cloud/regions/status` | `patch` | kept | `operators/c5c3/internal/controller/reconcile_catalog.go:195` (`reconcileCatalog`) |
| `openstack.k-orc.cloud/roleassignments/status` | `patch` | kept | `operators/c5c3/internal/controller/registration_projection.go:819` (`applyAccountRole`) |
| `openstack.k-orc.cloud/roles/status` | `patch` | kept | `operators/c5c3/internal/controller/registration_projection.go:819` (`applyAccountRole`) |
| `openstack.k-orc.cloud/services/status` | `patch` | kept | `operators/c5c3/internal/controller/reconcile_catalog.go:195` (`reconcileCatalog`) |
| `openstack.k-orc.cloud/users/status` | `patch` | kept | `operators/c5c3/internal/controller/keystoneuser_provision.go:165` (`provisionUser`) |
| `rabbitmq.com/permissions` | `update` | removed | `operators/c5c3/internal/controller/rabbitmqvhost_controller.go:182` |
| `rabbitmq.com/rabbitmqclusters` | `patch` | removed | `operators/c5c3/internal/controller/controlplane_controller.go:314` |
| `rabbitmq.com/rabbitmqclusters` | `update` | kept | `operators/c5c3/internal/controller/reconcile_infrastructure.go:1077` (`ensureRabbitMQ`) |
| `rabbitmq.com/users` | `update` | removed | `operators/c5c3/internal/controller/rabbitmqvhost_controller.go:182` |
| `rabbitmq.com/vhosts` | `update` | removed | `operators/c5c3/internal/controller/rabbitmqvhost_controller.go:182` |
| `rbac.authorization.k8s.io/clusterrolebindings` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_delete.go:1208` (`deleteBarbicanAuthDelegatorBinding`) |
| `rbac.authorization.k8s.io/rolebindings` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:828` (`deleteOwnedEnsembleObject`) |
| `rbac.authorization.k8s.io/rolebindings` | `get` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:818` (`deleteOwnedEnsembleObject`) |
| `rbac.authorization.k8s.io/roles` | `delete` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:828` (`deleteOwnedEnsembleObject`) |
| `rbac.authorization.k8s.io/roles` | `get` | kept | `operators/c5c3/internal/controller/reconcile_barbican.go:818` (`deleteOwnedEnsembleObject`) |
| `rbac.authorization.k8s.io/clusterroles` | `bind` | kept | `operators/c5c3/internal/controller/reconcile_barbican_openbao.go:195` (`ensureBarbicanOpenBao`) |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

With `list` removed, `get` no longer follows it: `generators.external-secrets.io/passwords` keeps `get` for `operators/c5c3/internal/controller/reconcile_delete.go:1530` (`sweepExternalNamespaceResidue`); `c5c3.io/secretaggregates` loses `get` as well, since nothing reads it.

### horizon

642 events and 42 used triples. Of 35 unused verbs, 19 were removed and 16 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `delete` | kept | `internal/common/config/config.go:517` (`pruneImmutableObjects`) |
| `configmaps` | `update`, `patch` | removed | `operators/horizon/internal/controller/horizon_controller.go:216` |
| `services` | `delete` | kept | `operators/horizon/internal/controller/reconcile_deployment.go:125` (`reconcileDeployment`) |
| `services` | `update` | removed | `operators/horizon/internal/controller/horizon_controller.go:215` |
| `events` | `patch` | kept | `internal/common/config/ownership.go:154` (`RecordExtraConfigHealth`) |
| `apps/deployments` | `delete` | kept | `internal/common/deployment/deployment.go:53` (`EnsureDeployment`) |
| `apps/deployments` | `update` | removed | `operators/horizon/internal/controller/horizon_controller.go:213` |
| `autoscaling/horizontalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/deployment.go:174` (`EnsureHPA`) |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/horizon/internal/controller/horizon_controller.go:224` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/vpa_flow.go:190` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `delete` | kept | `internal/common/deployment/vpa_flow.go:213` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/horizon/internal/controller/horizon_controller.go:590` (`SetupWithManager`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/horizon/internal/controller/horizon_controller.go:228` |
| `external-secrets.io/externalsecrets` | `list`, `watch` | kept | `internal/common/secrets/secrets.go:68` (`WaitForExternalSecret`) |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/horizon/internal/controller/horizon_controller.go:231` |
| `horizon.openstack.c5c3.io/horizons` | `create`, `patch`, `delete` | removed | `operators/horizon/internal/controller/horizon_controller.go:208` |
| `horizon.openstack.c5c3.io/horizons` | `update` | kept | `internal/common/reconcile/finalizer.go:25` (`EnsureFinalizer`) |
| `horizon.openstack.c5c3.io/horizons/finalizers` | `update` | kept | `internal/common/apply/apply.go:53` (`EnsureObject`) |
| `horizon.openstack.c5c3.io/horizons/status` | `patch` | removed | `operators/horizon/internal/controller/horizon_controller.go:209` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/horizon/internal/controller/horizon_controller.go:229` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/horizon/internal/controller/reconcile_deployment.go:130` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/horizon/internal/controller/horizon_controller.go:223` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/horizon/internal/controller/horizon_controller.go:248` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

With `list` removed, `get` no longer follows it: `scheduling.k8s.io/priorityclasses` keeps `get` for `internal/common/validation/validation.go:312` (`PriorityClassExists`).

### glance

3756 events and 74 used triples. Of 43 unused verbs, 26 were removed and 17 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `delete` | kept | `operators/glance/internal/controller/reconcile_config.go:181` (`reconcileConfig`) |
| `configmaps` | `update`, `patch` | removed | `operators/glance/internal/controller/glance_controller.go:340` |
| `secrets` | `delete` | kept | `operators/glance/internal/controller/reconcile_backends.go:192` (`reconcileBackends`) |
| `secrets` | `patch` | removed | `operators/glance/internal/controller/glance_controller.go:343` |
| `secrets` | `update` | kept | `operators/glance/internal/controller/reconcile_dbconnection_secret.go:49` (`reconcileDBConnectionSecret`) |
| `services` | `delete` | kept | `operators/glance/internal/controller/reconcile_deployment.go:210` (`reconcileDeployment`) |
| `services` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:338` |
| `apps/deployments` | `delete` | kept | `operators/glance/internal/controller/reconcile_deployment.go:166` (`reconcileDeployment`) |
| `apps/deployments` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:336` |
| `autoscaling/horizontalpodautoscalers` | `create`, `patch` | kept | `operators/glance/internal/controller/reconcile_hpa.go:26` (`reconcileHPA`) |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:366` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/vpa_flow.go:190` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `delete` | kept | `internal/common/deployment/vpa_flow.go:213` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/glance/internal/controller/glance_controller.go:826` (`SetupWithManager`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:370` |
| `batch/cronjobs` | `delete` | kept | `operators/glance/internal/controller/reconcile_dbpurge.go:154` (`reconcileDBPurge`) |
| `batch/cronjobs` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:347` |
| `batch/jobs` | `update`, `patch` | removed | `operators/glance/internal/controller/glance_controller.go:345` |
| `external-secrets.io/externalsecrets` | `list`, `watch` | kept | `internal/common/secrets/secrets.go:68` (`WaitForExternalSecret`) |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:373` |
| `glance.openstack.c5c3.io/glancebackends/status` | `patch` | removed | `operators/glance/internal/controller/glance_controller.go:334` |
| `glance.openstack.c5c3.io/glances/status` | `patch` | removed | `operators/glance/internal/controller/glance_controller.go:328` |
| `glance.openstack.c5c3.io/glances` | `create`, `delete` | removed | `operators/glance/internal/controller/glance_controller.go:327` |
| `glance.openstack.c5c3.io/glances/finalizers` | `update` | kept | `operators/glance/internal/controller/reconcile_deployment.go:166` (`reconcileDeployment`) |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:348` |
| `k8s.mariadb.com/grants` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:348` |
| `k8s.mariadb.com/users` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:348` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:371` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/glance/internal/controller/reconcile_deployment.go:215` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/glance/internal/controller/glance_controller.go:364` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/glance/internal/controller/glance_controller.go:380` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

With `list` removed, `get` no longer follows it: `scheduling.k8s.io/priorityclasses` keeps `get` for `internal/common/validation/validation.go:312` (`PriorityClassExists`).

### placement

1786 events and 67 used triples. Of 40 unused verbs, 25 were removed and 15 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `delete` | kept | `internal/common/config/config.go:517` (`pruneImmutableObjects`) |
| `configmaps` | `update`, `patch` | removed | `operators/placement/internal/controller/placement_controller.go:258` |
| `secrets` | `patch`, `delete` | removed | `operators/placement/internal/controller/placement_controller.go:260` |
| `secrets` | `update` | kept | `internal/common/database/connection.go:342` (`ReconcileConnectionSecret`) |
| `services` | `delete` | kept | `operators/placement/internal/controller/reconcile_deployment.go:176` (`reconcileDeployment`) |
| `services` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:256` |
| `apps/deployments` | `delete` | kept | `internal/common/deployment/deployment.go:53` (`EnsureDeployment`) |
| `apps/deployments` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:254` |
| `autoscaling/horizontalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/deployment.go:174` (`EnsureHPA`) |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:281` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/vpa_flow.go:190` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `delete` | kept | `internal/common/deployment/vpa_flow.go:213` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/placement/internal/controller/placement_controller.go:697` (`SetupWithManager`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:285` |
| `batch/jobs` | `update`, `patch` | removed | `operators/placement/internal/controller/placement_controller.go:262` |
| `external-secrets.io/externalsecrets` | `list`, `watch` | kept | `internal/common/secrets/secrets.go:68` (`WaitForExternalSecret`) |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:288` |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:263` |
| `k8s.mariadb.com/grants` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:263` |
| `k8s.mariadb.com/users` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:263` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:286` |
| `placement.openstack.c5c3.io/placements` | `create`, `delete` | removed | `operators/placement/internal/controller/placement_controller.go:249` |
| `placement.openstack.c5c3.io/placements/finalizers` | `update` | kept | `internal/common/apply/apply.go:53` (`EnsureObject`) |
| `placement.openstack.c5c3.io/placements/status` | `patch` | removed | `operators/placement/internal/controller/placement_controller.go:250` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/placement/internal/controller/reconcile_deployment.go:181` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/placement/internal/controller/placement_controller.go:279` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/placement/internal/controller/placement_controller.go:295` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

With `list` removed, `get` no longer follows it: `scheduling.k8s.io/priorityclasses` keeps `get` for `internal/common/validation/validation.go:312` (`PriorityClassExists`).

### barbican

2950 events and 75 used triples. Of 48 unused verbs, 28 were removed and 20 kept, and one used triple had no grant in the ClusterRole: `serviceaccounts/token` `create`, which the RoleBinding `barbican-operator-tokenrequest` of the chart's `templates/tokenrequest-rbac.yaml` grants in the namespace of a managed `BarbicanSecretStore`. The API server's authorization reason names that binding, and the ClusterRole must not carry the grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `create`, `update`, `patch`, `delete` | removed | `operators/barbican/internal/controller/barbican_controller.go:322` |
| `configmaps` | `list`, `watch` | kept | `internal/common/policy/policy.go:30` (`LoadPolicyFromConfigMap`) |
| `secrets` | `delete` | kept | `operators/barbican/internal/controller/reconcile_config.go:147` (`reconcileConfig`) |
| `secrets` | `patch` | removed | `operators/barbican/internal/controller/barbican_controller.go:325` |
| `secrets` | `update` | kept | `operators/barbican/internal/controller/barbicansecretstore_controller.go:845` (`mintCredentials`) |
| `services` | `delete` | kept | `operators/barbican/internal/controller/reconcile_deployment.go:211` (`reconcileDeployment`) |
| `services` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:320` |
| `apps/deployments` | `delete` | kept | `operators/barbican/internal/controller/reconcile_deployment.go:167` (`reconcileDeployment`) |
| `apps/deployments` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:318` |
| `autoscaling/horizontalpodautoscalers` | `create`, `patch` | kept | `operators/barbican/internal/controller/reconcile_hpa.go:26` (`reconcileHPA`) |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:358` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/vpa_flow.go:190` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `delete` | kept | `internal/common/deployment/vpa_flow.go:213` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/barbican/internal/controller/barbican_controller.go:814` (`SetupWithManager`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:362` |
| `barbican.openstack.c5c3.io/barbicans` | `create`, `delete` | removed | `operators/barbican/internal/controller/barbican_controller.go:305` |
| `barbican.openstack.c5c3.io/barbicans/finalizers` | `update` | kept | `operators/barbican/internal/controller/reconcile_deployment.go:167` (`reconcileDeployment`) |
| `barbican.openstack.c5c3.io/barbicansecretstores/finalizers` | `update` | kept | `operators/barbican/internal/controller/barbicansecretstore_controller.go:860` (`mintCredentials`) |
| `barbican.openstack.c5c3.io/barbicans/status` | `patch` | removed | `operators/barbican/internal/controller/barbican_controller.go:306` |
| `barbican.openstack.c5c3.io/barbicansecretstores/status` | `patch` | removed | `operators/barbican/internal/controller/barbicansecretstore_controller.go:302` |
| `barbican.openstack.c5c3.io/barbicansecretstores` | `update` | kept | `operators/barbican/internal/controller/barbicansecretstore_controller.go:475` (`ensureRemoteTeardownMarks`) |
| `batch/cronjobs` | `delete` | kept | `operators/barbican/internal/controller/reconcile_dbclean.go:182` (`reconcileDBClean`) |
| `batch/cronjobs` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:334` |
| `batch/jobs` | `update`, `patch` | removed | `operators/barbican/internal/controller/barbican_controller.go:332` |
| `external-secrets.io/externalsecrets` | `list`, `watch` | kept | `internal/common/secrets/secrets.go:68` (`WaitForExternalSecret`) |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:365` |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:335` |
| `k8s.mariadb.com/grants` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:335` |
| `k8s.mariadb.com/users` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:335` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:363` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/barbican/internal/controller/reconcile_deployment.go:216` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/barbican/internal/controller/barbican_controller.go:356` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/barbican/internal/controller/barbican_controller.go:371` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

### ovn

1330 events and 57 used triples. Of 45 unused verbs, 22 were removed and 23 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `delete` | kept | `operators/ovn/internal/controller/reconcile_database.go:201` (`reconcileRaftDatabase`) |
| `configmaps` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:221` |
| `persistentvolumeclaims` | `delete` | kept | `operators/ovn/internal/controller/reconcile_backup.go:214` (`reconcileBackup`) |
| `persistentvolumeclaims` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:221` |
| `secrets` | `create` | kept | `operators/ovn/internal/controller/reconcile_client_secret.go:151` (`writeChassisClientSecret`) |
| `secrets` | `patch`, `delete` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:224` |
| `secrets` | `update` | kept | `operators/ovn/internal/controller/reconcile_client_secret.go:172` (`writeChassisClientSecret`) |
| `services` | `delete` | kept | `operators/ovn/internal/controller/reconcile_relay.go:237` (`deleteRelay`) |
| `services` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:221` |
| `events` | `patch` | kept | `operators/ovn/internal/controller/reconcile_maintenance.go:449` (`runMaintenanceJob`) |
| `apps/daemonsets` | `delete` | kept | `operators/ovn/internal/controller/reconcile_ovs.go:107` (`reconcileOVS`) |
| `apps/daemonsets` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:232` |
| `apps/deployments` | `delete` | kept | `operators/ovn/internal/controller/reconcile_relay.go:234` (`deleteRelay`) |
| `apps/deployments` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:232` |
| `apps/statefulsets` | `delete` | kept | `operators/ovn/internal/controller/reconcile_database.go:221` (`reconcileRaftDatabase`) |
| `apps/statefulsets` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:232` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/vpa_flow.go:190` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `delete` | kept | `internal/common/deployment/vpa_flow.go:213` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/ovn/internal/controller/ovncentral_controller.go:594` (`setupWithOptions`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:252` |
| `batch/cronjobs` | `delete` | kept | `operators/ovn/internal/controller/reconcile_backup.go:238` (`reconcileBackup`) |
| `batch/cronjobs` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:238` |
| `batch/jobs` | `delete` | kept | `internal/common/job/job.go:205` (`recreateStaleJob`) |
| `batch/jobs` | `update`, `patch` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:236` |
| `cert-manager.io/certificates` | `delete` | kept | `operators/ovn/internal/controller/reconcile_relay.go:240` (`deleteRelay`) |
| `cert-manager.io/certificates` | `patch` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:246` |
| `cert-manager.io/certificates` | `update` | kept | `internal/common/tls/tls.go:65` (`EnsureCertificate`) |
| `ovn.openstack.c5c3.io/ovncentrals` | `update` | kept | `operators/ovn/internal/controller/ovncentral_controller.go:345` (`Reconcile`) |
| `ovn.openstack.c5c3.io/ovnchassis` | `update` | kept | `operators/ovn/internal/controller/ovnchassis_controller.go:249` (`Reconcile`) |
| `ovn.openstack.c5c3.io/ovncentrals/finalizers` | `update` | kept | `operators/ovn/internal/controller/reconcile_database.go:201` (`reconcileRaftDatabase`) |
| `ovn.openstack.c5c3.io/ovnchassis/finalizers` | `update` | kept | `operators/ovn/internal/controller/reconcile_ovs.go:107` (`reconcileOVS`) |
| `ovn.openstack.c5c3.io/ovncentrals/status` | `patch` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:205` |
| `ovn.openstack.c5c3.io/ovnchassis/status` | `patch` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:206` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/ovn/internal/controller/reconcile_database.go:225` (`reconcileRaftDatabase`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:241` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/ovn/internal/controller/ovncentral_controller.go:255` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

### neutron

3688 events and 84 used triples. Of 49 unused verbs, 32 were removed and 17 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `delete` | kept | `operators/neutron/internal/controller/reconcile_config.go:177` (`reconcileConfig`) |
| `configmaps` | `update`, `patch` | removed | `operators/neutron/internal/controller/neutron_controller.go:302` |
| `secrets` | `delete` | kept | `internal/common/messaging/messaging.go:338` (`ReconcileTransportURLSecret`) |
| `secrets` | `patch` | removed | `operators/neutron/internal/controller/neutron_controller.go:305` |
| `secrets` | `update` | kept | `operators/neutron/internal/controller/reconcile_ovn.go:240` (`writeOVNClientSecret`) |
| `services` | `delete` | kept | `operators/neutron/internal/controller/reconcile_deployment.go:168` (`reconcileDeployment`) |
| `services` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:300` |
| `apps/daemonsets` | `delete` | kept | `operators/neutron/internal/controller/reconcile_daemonset.go:155` (`reconcileDaemonSet`) |
| `apps/daemonsets` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:311` |
| `apps/deployments` | `delete` | kept | `operators/neutron/internal/controller/reconcile_deployment.go:157` (`reconcileDeployment`) |
| `apps/deployments` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:311` |
| `autoscaling/horizontalpodautoscalers` | `create`, `patch` | kept | `operators/neutron/internal/controller/reconcile_hpa.go:30` (`reconcileHPA`) |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:338` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch`, `delete` | kept | `operators/neutron/internal/controller/reconcile_vpa.go:21` (`reconcileVPA`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/neutron/internal/controller/neutron_controller.go:814` (`setupWithOptions`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:341` |
| `batch/cronjobs` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:315` |
| `batch/jobs` | `update`, `patch` | removed | `operators/neutron/internal/controller/neutron_controller.go:314` |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:345` |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:316` |
| `k8s.mariadb.com/grants` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:316` |
| `k8s.mariadb.com/users` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:316` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:342` |
| `neutron.openstack.c5c3.io/neutronmetadataagents` | `create`, `patch`, `delete` | removed | `operators/neutron/internal/controller/neutron_controller.go:290` |
| `neutron.openstack.c5c3.io/neutronmetadataagents` | `update` | kept | `operators/neutron/internal/controller/neutronmetadataagent_controller.go:363` (`Reconcile`) |
| `neutron.openstack.c5c3.io/neutrons` | `create`, `delete` | removed | `operators/neutron/internal/controller/neutron_controller.go:285` |
| `neutron.openstack.c5c3.io/neutronmetadataagents/finalizers` | `update` | kept | `operators/neutron/internal/controller/reconcile_daemonset.go:155` (`reconcileDaemonSet`) |
| `neutron.openstack.c5c3.io/neutrons/finalizers` | `update` | kept | `operators/neutron/internal/controller/reconcile_deployment.go:157` (`reconcileDeployment`) |
| `neutron.openstack.c5c3.io/neutronmetadataagents/status` | `patch` | removed | `operators/neutron/internal/controller/neutron_controller.go:291` |
| `neutron.openstack.c5c3.io/neutrons/status` | `patch` | removed | `operators/neutron/internal/controller/neutron_controller.go:286` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/neutron/internal/controller/reconcile_deployment.go:173` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/neutron/internal/controller/neutron_controller.go:336` |
| `rabbitmq.com/rabbitmqclusters` | `list`, `watch` | removed | `operators/neutron/internal/controller/neutron_controller.go:325` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/neutron/internal/controller/neutron_controller.go:352` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

With `list` removed, `get` no longer follows it: `scheduling.k8s.io/priorityclasses` keeps `get` for `internal/common/validation/validation.go:312` (`PriorityClassExists`); `rabbitmq.com/rabbitmqclusters` keeps `get` for `internal/common/messaging/messaging.go:382` (`resolveManaged`).

### cinder

7443 events and 82 used triples. Of 41 unused verbs, 28 were removed and 13 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `delete` | kept | `operators/cinder/internal/controller/reconcile_config.go:209` (`reconcileConfig`) |
| `configmaps` | `update`, `patch` | removed | `operators/cinder/internal/controller/cinder_controller.go:268` |
| `secrets` | `patch` | removed | `operators/cinder/internal/controller/cinder_controller.go:270` |
| `secrets` | `update` | kept | `operators/cinder/internal/controller/reconcile_messaging.go:41` (`reconcileTransportURLSecret`) |
| `services` | `delete` | kept | `operators/cinder/internal/controller/reconcile_deployment.go:173` (`reconcileDeployment`) |
| `services` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:266` |
| `apps/deployments` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:279` |
| `autoscaling/horizontalpodautoscalers` | `create`, `patch` | kept | `operators/cinder/internal/controller/reconcile_hpa.go:32` (`reconcileHPA`) |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:308` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch`, `delete` | kept | `operators/cinder/internal/controller/reconcile_vpa.go:24` (`reconcileVPA`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/cinder/internal/controller/cinder_controller.go:870` (`setupWithOptions`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:311` |
| `batch/cronjobs` | `delete` | kept | `operators/cinder/internal/controller/reconcile_dbpurge.go:134` (`reconcileDBPurge`) |
| `batch/cronjobs` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:285` |
| `batch/jobs` | `update`, `patch` | removed | `operators/cinder/internal/controller/cinder_controller.go:283` |
| `cinder.openstack.c5c3.io/cinderbackupbackends` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:263` |
| `cinder.openstack.c5c3.io/cinderbackends/status` | `patch` | removed | `operators/cinder/internal/controller/cinder_controller.go:264` |
| `cinder.openstack.c5c3.io/cinderbackupbackends/status` | `patch` | removed | `operators/cinder/internal/controller/cinder_controller.go:264` |
| `cinder.openstack.c5c3.io/cinders/status` | `patch` | removed | `operators/cinder/internal/controller/cinder_controller.go:256` |
| `cinder.openstack.c5c3.io/cinders/finalizers` | `update` | kept | `operators/cinder/internal/controller/reconcile_dbpurge.go:134` (`reconcileDBPurge`) |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:315` |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:286` |
| `k8s.mariadb.com/grants` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:286` |
| `k8s.mariadb.com/users` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:286` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:312` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/cinder/internal/controller/reconcile_deployment.go:178` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/cinder/internal/controller/cinder_controller.go:306` |
| `rabbitmq.com/rabbitmqclusters` | `list`, `watch` | removed | `operators/cinder/internal/controller/cinder_controller.go:295` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/cinder/internal/controller/cinder_controller.go:322` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

With `list` removed, `get` no longer follows it: `scheduling.k8s.io/priorityclasses` keeps `get` for `internal/common/validation/validation.go:312` (`PriorityClassExists`); `rabbitmq.com/rabbitmqclusters` keeps `get` for `internal/common/messaging/messaging.go:382` (`resolveManaged`).

### nova

12210 events and 87 used triples. Of 45 unused verbs, 28 were removed and 17 kept, and every used triple had a grant.

| Kind | Verbs | Outcome | Marker or call site |
| --- | --- | --- | --- |
| `configmaps` | `delete` | kept | `operators/nova/internal/controller/reconcile_config.go:267` (`reconcileConfig`) |
| `configmaps` | `update`, `patch` | removed | `operators/nova/internal/controller/nova_controller.go:251` |
| `secrets` | `update` | kept | `internal/common/messaging/messaging.go:338` (`ReconcileTransportURLSecret`) |
| `services` | `delete` | kept | `operators/nova/internal/controller/reconcile_consoleproxy.go:136` (`deleteConsoleProxy`) |
| `services` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:249` |
| `nodes` | `watch` | removed | `operators/nova/internal/controller/novacompute_controller.go:268` |
| `apps/daemonsets` | `update` | removed | `operators/nova/internal/controller/novacompute_controller.go:264` |
| `apps/deployments` | `delete` | kept | `operators/nova/internal/controller/reconcile_consoleproxy.go:129` (`deleteConsoleProxy`) |
| `apps/deployments` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:262` |
| `autoscaling/horizontalpodautoscalers` | `create`, `patch` | kept | `operators/nova/internal/controller/reconcile_hpa.go:32` (`reconcileHPA`) |
| `autoscaling/horizontalpodautoscalers` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:291` |
| `autoscaling.k8s.io/verticalpodautoscalers` | `create`, `patch` | kept | `internal/common/deployment/vpa_flow.go:190` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `delete` | kept | `internal/common/deployment/vpa_flow.go:213` (`ReconcileVPAs`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `list`, `watch` | kept | `operators/nova/internal/controller/nova_controller.go:842` (`setupWithOptions`) |
| `autoscaling.k8s.io/verticalpodautoscalers` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:295` |
| `batch/cronjobs` | `delete` | kept | `operators/nova/internal/controller/reconcile_dbarchive.go:203` (`reconcileDBArchive`) |
| `batch/cronjobs` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:267` |
| `batch/jobs` | `update`, `patch` | removed | `operators/nova/internal/controller/nova_controller.go:265` |
| `external-secrets.io/externalsecrets` | `list`, `watch` | kept | `internal/common/secrets/secrets.go:68` (`WaitForExternalSecret`) |
| `gateway.networking.k8s.io/httproutes` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:299` |
| `k8s.mariadb.com/databases` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:268` |
| `k8s.mariadb.com/grants` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:268` |
| `k8s.mariadb.com/users` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:268` |
| `networking.k8s.io/networkpolicies` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:296` |
| `nova.openstack.c5c3.io/novacomputes` | `patch` | removed | `operators/nova/internal/controller/novacompute_controller.go:259` |
| `nova.openstack.c5c3.io/novacomputes/finalizers` | `update` | kept | `operators/nova/internal/controller/reconcile_novacompute_daemonset.go:141` (`reconcileNovaComputeDaemonSet`) |
| `nova.openstack.c5c3.io/novas/finalizers` | `update` | kept | `operators/nova/internal/controller/reconcile_deployment.go:187` (`reconcileDeployment`) |
| `nova.openstack.c5c3.io/novacomputes/status` | `patch` | removed | `operators/nova/internal/controller/novacompute_controller.go:260` |
| `nova.openstack.c5c3.io/novas/status` | `patch` | removed | `operators/nova/internal/controller/nova_controller.go:243` |
| `policy/poddisruptionbudgets` | `delete` | kept | `operators/nova/internal/controller/reconcile_deployment.go:198` (`reconcileDeployment`) |
| `policy/poddisruptionbudgets` | `update` | removed | `operators/nova/internal/controller/nova_controller.go:289` |
| `rabbitmq.com/rabbitmqclusters` | `list`, `watch` | removed | `operators/nova/internal/controller/nova_controller.go:277` |
| `scheduling.k8s.io/priorityclasses` | `list`, `watch` | removed | `operators/nova/internal/controller/nova_controller.go:306` |
| `coordination.k8s.io/leases` | `list`, `watch`, `patch`, `delete` | removed | `hack/gen-helm-rbac-rules.py` (`LEADER_ELECTION_RULE`) |

With `list` removed, `get` no longer follows it: `scheduling.k8s.io/priorityclasses` keeps `get` for `internal/common/validation/validation.go:312` (`PriorityClassExists`); `rabbitmq.com/rabbitmqclusters` keeps `get` for `internal/common/messaging/messaging.go:382` (`resolveManaged`).
