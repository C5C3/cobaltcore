---
title: Enable the Placement Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `placement-system`; workload (Placement CR) stays in `openstack`. -->

# How-to: Enable the Placement Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the placement-operator pod's egress and ingress
to the minimum required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects the
> Placement API pods (`spec.networkPolicy` on a Placement CR), see the
> [Placement CRD API Reference](../../reference/placement/placement-crd.md).
> That per-CR policy restricts ingress to TCP 8778 from the sources you list
> (at least one is required, so an empty list is refused) and derives its
> egress from the CR: DNS, the database, the Keystone endpoint's port, and the
> cache, with `additionalEgress` appended after it.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the placement-operator
is running (namespace `placement-system`) alongside the projected
`controlplane-placement` resource-tracking service.
:::

1. A running `placement-operator` Helm release (namespace `placement-system`).
2. The ControlPlane declares `spec.services.placement`. Without it no
   `controlplane-placement` child exists and the Step 2 verification has
   nothing to roll.

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which carry the real API server addresses. The
`10.96.0.1` Service VIP that `kubectl get service kubernetes` reports is the
wrong source. An enforcing CNI
(Calico, Cilium, and kindnet on kind v0.32 and later) evaluates policy after the
Service DNAT has rewritten a packet aimed at the VIP to one of those endpoint
IPs, so a rule naming the VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints. A kind
devstack reports the one control-plane node's address on the kind bridge
(`172.18.0.x`), but an HA control plane reports one per API server replica:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
```

The rule must cover all of them, or the operator loses its leader-election
lease whenever it happens to be talking to an excluded replica.

On the tutorial devstacks the `placement-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next
reconcile. Substitute the CIDRs and ports from above. The single-CIDR list
below stands in for the kind case (one control-plane node), so replace
`172.18.0.2/32` with the address you recorded and extend the list to every IP
an HA control plane prints:

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/placement-operator -n placement-system \
  --for=condition=Ready --timeout=5m
```

The chart-level policy allows what the operator needs: egress to the
kube-apiserver, to DNS and to the Placement API, ingress to the webhook and
metrics ports. The Placement API rule admits TCP 8778 to every pod labelled
`app.kubernetes.io/name: placement` and
`app.kubernetes.io/managed-by: placement-operator`, in any namespace, and needs
no value. The operator's health check GETs the API over the Placement Service,
and the CNI judges that connection after the Service DNAT, at the API pod and
its port, so the rule selects the API pods by the labels the operator sets on
them. When the workload namespace itself runs a per-CR NetworkPolicy, the
operator's namespace is admitted automatically by the sub-reconciler (an
operator-namespace ingress peer is appended to every rendered policy).

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port at all, so a Prometheus that scraped the operator before
stops reaching it. Name the scraping namespace when you enable both this policy
and the ServiceMonitor from
[Enable the Placement Operator Metrics Endpoint](./enable-placement-operator-metrics.md):

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade`. The fail-closed guard still requires
`kubeApiServer.cidrs` and `ports`:

```bash
helm upgrade placement-operator oci://ghcr.io/c5c3/charts/placement-operator \
  --namespace placement-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2 — Verify

On the kind devstack this verifies the policy's **shape** and that the operator
still reconciles under the enforced policy. It sends no traffic outside the
allow-list.

Restart the operator first. NetworkPolicy enforcement is stateful: a connection
opened before the policy existed stays open, so a wrong `kubeApiServer` rule
shows only after the next restart:

```bash
kubectl -n placement-system rollout restart deploy/placement-operator
kubectl -n placement-system rollout status deploy/placement-operator --timeout=5m
```

A rollout that does not finish within the timeout, with the new pod never
`Ready`, means the egress rule does not admit the API server.
See [Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

Then check the policy:

```bash
kubectl -n placement-system get networkpolicy
kubectl -n placement-system describe networkpolicy placement-operator
```

Then confirm reconciliation still works end-to-end by driving a change
through the `ControlPlane` CR and watching the projected resource-tracking
service roll out. The block records the ControlPlane's current count, which is
empty while the ControlPlane leaves it at the default, scales the API one
replica above the size it runs at, and puts the count back:

```bash
if CUR=$(kubectl get controlplane controlplane -n openstack \
     -o jsonpath='{.spec.sizing.placement.api.replicas}') &&
   RUN=$(kubectl get deploy/controlplane-placement -n openstack \
     -o jsonpath='{.spec.replicas}'); then
  NEW=$((RUN + 1))

  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"placement\":{\"api\":{\"replicas\":$NEW}}}}}"
  kubectl wait deploy/controlplane-placement -n openstack --timeout=5m \
    --for=jsonpath='{.spec.replicas}'="$NEW"
  kubectl rollout status deploy/controlplane-placement -n openstack --timeout=5m

  # revert to the recorded count; null drops the override again
  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"placement\":{\"api\":{\"replicas\":${CUR:-null}}}}}}"
else
  echo "could not read the replica counts; nothing was patched" >&2
fi
```

The `if` stops the block before the first patch when either read fails. A
failed read prints the same empty string as a ControlPlane without an override,
so without the guard the revert would drop an override the ControlPlane
carries, and a failed Deployment read would set `NEW` to 1 and shrink the API
instead of growing it.

The `kubectl wait` is the check that proves the operator still reconciles: the
patch travels from the ControlPlane to the `Placement` CR to the Deployment,
and the restarted operator writes nothing until it holds the leader lease.
Until the placement operator writes the new count the Deployment is still fully
rolled out at its old size, so a `rollout status` alone would pass at once. The
new count is always one above the running one, so the patch changes the
Deployment whatever size the API already runs at.

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-placement` child: the c5c3-operator re-asserts the child's
`spec.deployment.replicas` on every reconcile, so a direct edit of the child is
reverted.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`placement-operator` pod restarts.

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

### Every Placement write is rejected

**Symptom:** `kubectl apply` on a `Placement` CR fails with
`failed calling webhook` plus `connection refused` or `no route to host`. The
`ControlPlane` stalls too: the c5c3-operator projects `controlplane-placement`
through the same admission path, so its reconcile fails on the same error.

**Diagnosis:** webhook ingress (9443) is blocked. Both placement webhooks
register `failurePolicy=Fail` on create and update, so an unreachable operator
turns into a cluster-wide rejection of every write to the kind. The usual cause
is an API server that calls webhooks from an IP that is **not** in
`endpoints/kubernetes`, for example because it sits behind a front-end proxy.
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and the
operator reverts to unrestricted pod networking without a pod restart.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The placement chart's helm-unittest
(`operators/placement/helm/placement-operator/tests/networkpolicy_test.yaml`)
pins the egress rule the chart adds for its API health check. These tests check
rendering only; they do not exercise packet enforcement.

The [Keystone operator suite](../keystone/enable-keystone-operator-networkpolicy.md#tested-by)
provides runtime enforcement coverage for the Keystone chart using the shared
template. It does not install the Placement chart or test Placement-specific
runtime egress. There is no Placement operator `network-policy-egress` suite.
With the linked suite's prerequisites satisfied, run its Keystone-only example
from the repository root:

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

The per-CR suite `tests/e2e/placement/network-policy` covers the Placement workload
policy, a different scope from this guide.
