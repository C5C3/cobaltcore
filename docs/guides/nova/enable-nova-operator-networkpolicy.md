---
title: Enable the Nova Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `nova-system`; workload (Nova) stays in `openstack`. -->

# How-to: Enable the Nova Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level NetworkPolicy
that restricts the nova-operator pod's egress and ingress to the minimum
required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the **operator
> pod itself**. For the per-CR policies that protect the Nova pods
> (`spec.networkPolicy` on a `Nova`), see
> [Network policy](../../reference/nova/nova-crd.md#network-policy) in the Nova
> CRD reference. Those restrict ingress to TCP 8774, 8775 and 6080 from the
> sources you list and derive their egress from the CR, with a second policy,
> `{name}-novncproxy`, opening the console proxy's VNC egress. The ControlPlane
> does not project `spec.networkPolicy`, so on the devstack below no per-CR
> policy exists.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through the `nova` block of Step 4 and the
**Boot a first server** catalog check in Step 7, so the projected
`controlplane-nova` child is `Ready` in `openstack`. Every resource name in the
examples below is one that devstack produces.
:::

1. A running `nova-operator` Helm release (namespace `nova-system`).
2. The ControlPlane declares `spec.services.nova`. Without it no
   `controlplane-nova` child exists and the Step 2 verification has nothing to
   roll.

## Step 1: Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which carry the real API server addresses. The
`10.96.0.1` Service VIP that `kubectl get service kubernetes` reports is the
wrong source: an enforcing CNI
(Calico, Cilium, and kindnet on kind v0.32 and later) evaluates policy after the
Service DNAT has rewritten a packet aimed at the VIP to one of those endpoint
IPs, so a rule naming the VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints. A kind
devstack reports the one control-plane node's address on the kind bridge
(`172.18.0.x`), but an HA control plane reports one per API server replica. The
rule must cover all of them, or the operator loses its leader-election lease
whenever it happens to be talking to an excluded replica.

On the tutorial devstacks the `nova-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. A raw
`helm upgrade` is reverted by the Flux helm-controller on its next reconcile;
outside Flux, the same values go through `helm upgrade --reuse-values`.
Substitute the CIDRs and ports from above; the single-CIDR list below stands in
for the kind case of one control-plane node:

```bash
kubectl patch helmrelease nova-operator -n nova-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/nova-operator -n nova-system \
  --for=condition=Ready --timeout=5m
```

The rendered policy allows egress to the kube-apiserver, to DNS, to the Nova
API, to the Keystone API and to the Placement API, and ingress to the webhook
port. The chart adds the last three through the
`operator-library.chart.networkPolicyEgress` hook, the hook a chart uses to
append egress rules of its own: TCP 8774 to every pod labelled
`app.kubernetes.io/name: nova` and
`app.kubernetes.io/managed-by: nova-operator`, TCP 5000 and 5050 to every pod
labelled `app.kubernetes.io/name: keystone` and
`app.kubernetes.io/managed-by: keystone-operator`, and TCP 8778 to every pod
labelled `app.kubernetes.io/name: placement` and
`app.kubernetes.io/managed-by: placement-operator`, all in any namespace. None
needs a value. The operator's health check GETs the Nova API. A `NovaCompute`
pool authenticates against Keystone, calls the Nova API to manage its compute
services and aggregates, and calls the Placement API to create and delete its
custom traits. Each call goes through a Service, and the CNI judges the
connection after the Service DNAT, at the pod and its port, so each rule selects
the pods by the labels their operator sets on them. The Keystone and Placement
rules therefore admit a `NovaCompute` call only when its URL reaches those pods
in this cluster;
[NovaCompute calls to Keystone or Placement time out](#novacompute-calls-to-keystone-or-placement-time-out)
covers the other topologies. Keystone answers on 5050
when federation puts its OIDC sidecar in front of the API. One deployment shape
does need more: with a `Nova` placed on a registered target cluster, the
operator talks to that cluster's API server too, so add its address and port to
the same `kubeApiServer` lists.

The reverse direction needs nothing: the per-CR policy appends an ingress peer
selecting the operator's namespace to every policy it renders, so a workload
namespace running `spec.networkPolicy` already admits the probe.

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port (8080) at all, so a Prometheus that scraped the operator
before stops reaching it. Name the scraping namespace when you enable both this
policy and the ServiceMonitor from
[Enable the Nova Operator Metrics Endpoint](./enable-nova-operator-metrics.md):

```bash
kubectl patch helmrelease nova-operator -n nova-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

### Upgrading from the hand-written probe policy

nova-operator charts before 0.5.0 rendered no rule for the health check, and
earlier versions of this guide had you apply a second policy,
`nova-operator-api-probe`, beside the chart's. The chart covers that traffic
now, so delete the hand-written policy once the upgraded chart has rendered its
own:

```bash
kubectl delete networkpolicy nova-operator-api-probe -n nova-system \
  --ignore-not-found
```

## Step 2: Verify

On the kind devstack this verifies the policy's **shape** and that the operator
still reconciles under the enforced policy. It sends no traffic outside the
allow-list.

Restart the operator first. NetworkPolicy enforcement is stateful: a connection
opened before the policy existed stays open, so a wrong `kubeApiServer` rule
shows only after the next restart:

```bash
kubectl -n nova-system rollout restart deploy/nova-operator
kubectl -n nova-system rollout status deploy/nova-operator --timeout=5m
```

A rollout that does not finish within the timeout, with the new pod never
`Ready`, means the egress rule does not admit the API server.
See [Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

Then check the policy:

```bash
kubectl -n nova-system get networkpolicy
kubectl -n nova-system describe networkpolicy nova-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `ControlPlane` CR and watching the projected compute API roll out. The
block records the ControlPlane's current count, which is empty while the
ControlPlane leaves it at the default, scales the API one replica above the size
it runs at, and puts the count back:

```bash
if CUR=$(kubectl get controlplane controlplane -n openstack \
     -o jsonpath='{.spec.sizing.nova.api.replicas}') &&
   RUN=$(kubectl get deploy/controlplane-nova -n openstack \
     -o jsonpath='{.spec.replicas}'); then
  NEW=$((RUN + 1))

  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"nova\":{\"api\":{\"replicas\":$NEW}}}}}"
  kubectl wait deploy/controlplane-nova -n openstack --timeout=5m \
    --for=jsonpath='{.spec.replicas}'="$NEW"
  kubectl rollout status deploy/controlplane-nova -n openstack --timeout=5m

  # revert to the recorded count; null drops the override again
  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"nova\":{\"api\":{\"replicas\":${CUR:-null}}}}}}"
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
patch travels from the ControlPlane to the `Nova` CR to the Deployment, and
until the nova operator writes the new count the Deployment is still fully
rolled out at its old size, so a `rollout status` alone would pass at once. An
operator that cannot reach the API server never writes the count, and the wait
times out. The new count is always one above the running one, so the patch
changes the Deployment whatever size the API already runs at.

`spec.sizing.nova.api.replicas` sizes the API Deployment alone. The metadata
API, the scheduler, the conductor and the console proxy carry counts of their
own under `spec.sizing.nova`, so this patch rolls `controlplane-nova` and leaves
the other four untouched.

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-nova` child: the c5c3-operator re-asserts the child's
`spec.api.deployment.replicas` on every reconcile, so a direct edit of the child
is taken back.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`nova-operator` pod restarts.

**Diagnosis:** the egress allow-list does not match the API server the operator
actually dials. Either `kubeApiServer.cidrs` is missing one or more of the
current endpoint IPs (an HA control plane may have added a replica, or a
control-plane node may have been replaced with a different IP), or your CNI maps
the API server behind a port that is not in `kubeApiServer.ports`. A target
cluster registered since the policy went on is the same failure with a different
address.

**Fix:** re-run the discovery command from Step 1 and update
`networkPolicy.kubeApiServer.cidrs` to include **every** IP it returns, plus
every port.

### NovaCompute calls to Keystone or Placement time out

**Symptom:** a `NovaCompute` reports `ServicesReady`, `AggregatesReady` or
`TraitsReady` as `False` with `ComputeAPIError`, and the message names the
Keystone token request (`POST /v3/auth/tokens`) or a Placement call with
`context deadline exceeded`, while its `Nova` stays `Ready`.

**Diagnosis:** the operator pod dials a Keystone or Placement address the chart
rules do not select. They admit the keystone and placement pods of this cluster,
but a `Nova` without `spec.targetClusterRef` is called from the operator pod
itself, at the URLs it names: `spec.keystoneEndpoint`, and
`spec.endpoints.placement.override` or else the catalog's internal Placement
endpoint. Those resolve elsewhere when the ControlPlane places Keystone or
Placement on another cluster (the URL is then the public one, such as the
Gateway hostname), when the override names another host, or when the `Nova`
points at an external Keystone. A placed `Nova` is reached through its cluster's
API server instead, which the `kubeApiServer` lists cover.

**Fix:** no chart value adds a destination. Apply a second NetworkPolicy of your
own beside the chart's; policies on the same pod are unioned, so its egress rule
joins the chart's. Name the address the URL resolves to and its port:

```yaml
# nova-operator-keystone-egress.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nova-operator-keystone-egress
  namespace: nova-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: nova-operator
      app.kubernetes.io/instance: nova-operator
  policyTypes:
    - Egress
  egress:
    - to:
        - ipBlock:
            cidr: 203.0.113.10/32
      ports:
        - protocol: TCP
          port: 443
```

```bash
kubectl apply -f nova-operator-keystone-egress.yaml
```

The two selector labels are the chart's own (`operator-library.selectorLabels`),
where the instance value is the Helm release name. Add a rule per address when
Keystone and Placement resolve to different hosts. When an address is the
LoadBalancer IP of a Gateway in this cluster, most CNIs judge the connection
after the DNAT, as they do for a Service, so select the Gateway's data-plane
pods and their container port in place of the `ipBlock`.

Apply this object only while the chart policy is on. It declares
`policyTypes: [Egress]` and selects the operator pod, so on its own it turns
unrestricted egress into egress to this one address: the operator loses the
kube-apiserver, its leader-election lease stops renewing, and every `Nova` stops
reconciling. Delete it whenever you set `networkPolicy.enabled=false`.

### Every Nova write is rejected

**Symptom:** `kubectl apply` on a `Nova` fails with `failed calling webhook`
plus `connection refused` or `no route to host`. The `ControlPlane` stalls too:
the c5c3-operator projects `controlplane-nova` through the same admission path,
and `NovaReady` reads `False/NovaError`.

**Diagnosis:** webhook ingress (9443) is blocked. The chart renders one
MutatingWebhookConfiguration and one ValidatingWebhookConfiguration carrying the
webhooks `mnova.kb.io` and `vnova.kb.io`, both with `failurePolicy: Fail` on
create and update, so an unreachable operator turns into a cluster-wide
rejection of every write to the kind. Deletes pass: the rules list `CREATE` and
`UPDATE` only, which is what keeps a down operator from blocking CR and
namespace teardown. The usual cause is an API server that calls webhooks from an
IP that is **not** in `endpoints/kubernetes`, for example because it sits behind
a front-end proxy. `networkPolicy.webhookClients.cidrs` falls back to
`kubeApiServer.cidrs` when empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease nova-operator -n nova-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile, and
the operator reverts to unrestricted pod networking without a pod restart unless
a policy of your own still selects the pod: `nova-operator-api-probe` from an
earlier version of this guide, which allows 8774 alone, or the one from
[NovaCompute calls to Keystone or Placement time out](#novacompute-calls-to-keystone-or-placement-time-out).
Delete those as well:

```bash
kubectl delete networkpolicy nova-operator-api-probe \
  nova-operator-keystone-egress -n nova-system --ignore-not-found
```

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The nova chart's helm-unittest
(`operators/nova/helm/nova-operator/tests/networkpolicy_test.yaml`) pins the
three egress rules the chart adds, for its API health check, for the Keystone
API and for the Placement API. These tests cover rendering only; they do not
exercise packet enforcement.

The [Keystone operator suite](../keystone/enable-keystone-operator-networkpolicy.md#tested-by)
provides runtime enforcement coverage for the Keystone chart using the shared
template. It does not install the Nova chart or test its Nova, Keystone or
Placement egress rules at runtime. There is no Nova operator
`network-policy-egress` suite.

With the linked suite's prerequisites satisfied, run its Keystone-only example
from the repository root:

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

A second suite, `tests/e2e/nova/network-policy`, covers the other policies: the
ones the operator projects around the Nova pods from `spec.networkPolicy` on a
`Nova` CR. That is a different subject from this guide's, and the
[Nova CRD reference](../../reference/nova/nova-crd.md#network-policy) documents
its field.
