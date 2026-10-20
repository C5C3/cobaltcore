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

A verb that stays gets a comment line beside its marker:

```go
// Unexercised by the e2e suites; used by <file>:<line> (<function>).
```

A verb with no such call site leaves the marker. After the edit, `make
manifests`, `make sync-helm-rbac` and `make verify-helm-rbac` regenerate and
check the chart rules, and the chart's version rises by a patch with an
`artifacthub.io/changes` entry.
