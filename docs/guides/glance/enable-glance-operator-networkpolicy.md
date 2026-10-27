---
title: Enable the Glance Operator NetworkPolicy
quadrant: operator
---

<!-- operator namespace is `glance-system`; workload (Glance CR) stays in `openstack`. -->

# How-to: Enable the Glance Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the glance-operator pod's egress and ingress
to the minimum required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects the
> Glance API pods (`spec.networkPolicy` on a Glance CR), see the
> [Glance CRD API Reference](../../reference/glance/glance-crd.md). That per-CR
> policy is also the containment layer for `web-download` image imports: its
> auto-derived egress covers DNS, the database, the cache, and the S3 backends
> only, so an import needs an explicit `spec.networkPolicy.additionalEgress`
> rule naming the mirrors; nothing else stays reachable. Enable it whenever
> `spec.importFiltering` widens the operator's HTTPS-on-443 default, because the
> remaining `disallowedHosts` filter matches host strings literally and cannot
> cover every spelling of an address.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the glance-operator
is running (namespace `glance-system`) alongside the projected
`controlplane-glance` image service.
:::

A running `glance-operator` Helm release (namespace `glance-system`) is required.

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDR and port from the
in-cluster `kubernetes` Service (on kind this is `10.96.0.1/32` port `6443`):

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints. A kind devstack reports a
single `10.96.0.1/32`, but an HA control plane reports one
per API server replica:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
```

The rule must cover all of them, or the operator loses its leader-election
lease whenever it happens to be talking to an excluded replica.

On the tutorial devstacks the `glance-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next
reconcile. Substitute the CIDRs and ports from above; the single-CIDR list
below is the kind case, so extend it to every IP you recorded:

```bash
kubectl patch helmrelease glance-operator -n glance-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["10.96.0.1/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/glance-operator -n glance-system \
  --for=condition=Ready --timeout=5m
```

The chart-level policy allows what the operator needs: egress to the
kube-apiserver, to DNS and to the Glance API, ingress to the webhook and metrics
ports. The Glance API rule admits TCP 9292 to every pod labelled
`app.kubernetes.io/name: glance` and
`app.kubernetes.io/managed-by: glance-operator`, in any namespace, and needs no
value. The operator's health check GETs the API over the Glance Service, and the
CNI judges that connection after the Service DNAT, at the API pod and its port,
so the rule selects the API pods by the labels the operator sets on them. When
the workload namespace itself runs a per-CR NetworkPolicy, the operator's
namespace is admitted automatically by the sub-reconciler (an operator-namespace
ingress peer is appended to every rendered policy).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade`. The fail-closed guard requires
`kubeApiServer.cidrs` and `ports`:

```bash
helm upgrade glance-operator oci://ghcr.io/c5c3/charts/glance-operator \
  --namespace glance-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=10.96.0.1/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2 — Verify

On kind v0.32 and later this checks the policy's shape and reconciliation
under enforcement. These commands do not attempt traffic outside the allow-list.
Restart the operator to test new connections; existing connections can remain
open after a policy change:

```bash
kubectl -n glance-system rollout restart deploy/glance-operator
kubectl -n glance-system rollout status deploy/glance-operator --timeout=5m
```

If the rollout times out, inspect the operator logs and the API-server rules
using the troubleshooting steps below.

```bash
kubectl -n glance-system get networkpolicy
kubectl -n glance-system describe networkpolicy glance-operator
```

Then confirm reconciliation still works end-to-end by driving a change
through the `ControlPlane` CR and watching the projected image service roll out:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"glance":{"api":{"replicas":2}}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack

# revert
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"glance":{"api":{"replicas":1}}}}}'
```

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-glance` child: the c5c3-operator re-asserts the child's
`spec.deployment.replicas` on every reconcile, so a direct edit of the child is
reverted.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`glance-operator` pod restarts.

**Diagnosis:** the egress allow-list does not match the API server the operator
actually dials. Either `kubeApiServer.cidrs` is missing one or more of the
current endpoint IPs (an HA control plane may have added a replica, or a
control-plane node may have been replaced with a different IP), or your CNI maps
the API server behind a port that is not in `kubeApiServer.ports`.

**Fix:** re-run the discovery command from Step 1 and update
`networkPolicy.kubeApiServer.cidrs` to include **every** IP it returns, plus
every port:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

### Every Glance and GlanceBackend write is rejected

**Symptom:** `kubectl apply` on a `Glance` or `GlanceBackend` CR fails with
`failed calling webhook` plus `connection refused` or `no route to host`. The
`ControlPlane` stalls too: the c5c3-operator projects `controlplane-glance` and
its backend children through the same admission path, so its reconcile fails on
the same error.

**Diagnosis:** webhook ingress (9443) is blocked. Both glance webhooks register
`failurePolicy=Fail` on create and update, so an unreachable operator turns into
a cluster-wide rejection of every write to either kind. The usual cause is an API
server that calls webhooks from an IP that is **not** in `endpoints/kubernetes`
(for example, because it sits behind a front-end proxy).
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease glance-operator -n glance-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and
the operator reverts to unrestricted pod networking without a pod restart.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The glance chart's helm-unittest
(`operators/glance/helm/glance-operator/tests/networkpolicy_test.yaml`) pins the
egress rule the chart adds for its API health check. These tests cover rendering
only; they do not exercise packet enforcement.

The [Keystone operator suite](../keystone/enable-keystone-operator-networkpolicy.md#tested-by)
provides runtime enforcement coverage for the Keystone chart using the shared
template. It does not install the Glance chart or test Glance-specific runtime
egress. There is no Glance operator `network-policy-egress` suite.
With the linked suite's prerequisites satisfied, run its Keystone-only example
from the repository root:

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

The per-CR suite `tests/e2e/glance/network-policy` covers the Glance workload
policy, a different scope from this guide.
