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

Follow that tutorial through the `nova` block of Step 3 and the
**Boot a first server** catalog check in Step 6, so the projected
`controlplane-nova` child is `Ready` in `openstack`. Every resource name in the
examples below is one that devstack produces.
:::

1. **A CNI that enforces `networking.k8s.io/v1` NetworkPolicy (required for
   real enforcement).** Confirm with your platform team (Calico, Cilium, and
   Antrea enforce).

   ::: warning Enforcement cannot be verified on the default devstack CNI
   The ControlPlane Quick Start kind devstack uses the default `kindnet` CNI,
   which **silently ignores** NetworkPolicy objects, and kind fixes the CNI at
   cluster creation so it cannot be swapped in afterwards. The policy object is
   still created and the operator keeps reconciling, so Step 2 below confirms
   only the policy's **shape** and that enabling it does **not break**
   reconciliation. It does **not** prove that packets outside the allow-list are
   dropped. Real enforcement requires a cluster whose CNI enforces
   NetworkPolicy, typically your production platform.
   :::
2. A running `nova-operator` Helm release (namespace `nova-system`).
3. The ControlPlane declares `spec.services.nova`. Without it no
   `controlplane-nova` child exists and the Step 2 verification has nothing to
   roll.

## Step 1: Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which carry the real API server addresses. The
`10.96.0.1` Service VIP that `kubectl get service kubernetes` reports is the
wrong source: an enforcing CNI (Calico, Cilium) DNATs a packet aimed at the VIP
to one of those endpoint IPs before it evaluates policy, so a rule naming the
VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints, not just the first. A kind
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

The rendered policy allows egress to the kube-apiserver and to DNS, and ingress
to the webhook port. The chart defines no
`operator-library.chart.networkPolicyEgress` override, the hook a chart uses to
append an egress rule of its own, so the shared set is the whole set. One
deployment shape does need more: with a `Nova` placed on a registered target
cluster, the operator talks to that cluster's API server too, so add its address
and port to the same `kubeApiServer` lists.

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

### Not covered: the API health probe (port 8774)

The operator's health-check step GETs the root of the compute API over the
workload Service, from the operator pod in `nova-system`. On the devstack that
is `http://controlplane-nova.openstack.svc.cluster.local:8774/`. The compute API
ships no `/healthcheck` route, so the version document at `/` is what the probe
reads. The shared library template renders egress to DNS, to the
`kubeApiServer` CIDRs, and to whatever the
`operator-library.chart.networkPolicyEgress` hook holds, and the nova chart
leaves that hook at the library's empty default. **The chart renders no egress
rule for 8774.** On an enforcing CNI the probe is therefore blocked, and
`NovaAPIReady` goes `False` with `HealthCheckTimeout` once the 10-second probe
deadline expires, since a default-deny policy drops the packet and never answers
it. A peer that answers with a TCP reset yields `ConnectionFailed`. No other
condition is affected, so a `Nova` whose `DeploymentReady` is `True` while
`NovaAPIReady` times out is the fingerprint of this gap. On the ControlPlane it
surfaces as `NovaReady=False/WaitingForNova`, and the child's own conditions
name the cause:

```bash
kubectl get nova controlplane-nova -n openstack \
  -o jsonpath='{.status.conditions[?(@.type=="NovaAPIReady")].reason}{"\n"}'
```

NetworkPolicies on the same pod are unioned, so an additive object beside the
chart's covers the gap with no chart change. Apply it in the operator namespace:

```yaml
# nova-operator-api-probe.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: nova-operator-api-probe
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
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: openstack
      ports:
        - protocol: TCP
          port: 8774
```

```bash
kubectl apply -f nova-operator-api-probe.yaml
```

::: warning Apply this only after the chart policy is on
Policies union, but this object is not additive on its own: it declares
`policyTypes: [Egress]` and selects the operator pod, so applying it before the
Step 1 patch reconciles turns unrestricted egress into egress to 8774 alone. The
operator then loses the kube-apiserver, its leader-election lease stops
renewing, the pod restarts into the same wall, and every `Nova` stops
reconciling. The chart's fail-closed guard on `kubeApiServer.cidrs` does not
catch this: it protects the chart's own template, not an object you applied by
hand.
:::

The two selector labels are the chart's own
(`operator-library.selectorLabels`), where the instance value is the Helm
release name. Change `openstack` to the namespace your `Nova` runs in, and add
one `to` peer per namespace when more than one carries a `Nova`.

The reverse direction needs nothing: the per-CR policy appends an ingress peer
selecting the operator's namespace to every policy it renders, so a workload
namespace running `spec.networkPolicy` already admits the probe.

## Step 2: Verify

On the kind devstack this verifies the policy's **shape** and that enabling it
does **not break** reconciliation, not traffic enforcement, which the default
`kindnet` CNI does not apply (see the prerequisite above).

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
the same patch shape **and** delete the additive object above, if you applied
it:

```bash
kubectl delete networkpolicy nova-operator-api-probe -n nova-system \
  --ignore-not-found
```

Both have to go. Either one left selecting the pod keeps restricting it, and
`nova-operator-api-probe` alone is the narrower of the two: it allows 8774 and
nothing else. With no policy selecting it the operator reverts to unrestricted
pod networking without a pod restart.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library. The keystone chart's copy is exercised end-to-end
on the CI e2e kind cluster by the borrowed chainsaw suite below; there is no
nova twin of it. The nova chart's own helm-unittests cover the ClusterRole, the
Role, the Deployment, the values schema and the webhook configurations, and
carry no networkpolicy test, so the rendered policy every chart gets is pinned by
the library's own helm-unittest,
`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`
(default-off posture, rule shape, both fail-closed guards, and the absence of an
ingress rule for the health-probe port).

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

A second suite, `tests/e2e/nova/network-policy`, covers the other policies: the
ones the operator projects around the Nova pods from `spec.networkPolicy` on a
`Nova` CR. That is a different subject from this guide's, and the
[Nova CRD reference](../../reference/nova/nova-crd.md#network-policy) documents
its field.
