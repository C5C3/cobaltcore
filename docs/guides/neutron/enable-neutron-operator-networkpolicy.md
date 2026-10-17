---
title: Enable the Neutron Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `neutron-system`; workload (Neutron, NeutronMetadataAgent) stays in `openstack`. -->

# How-to: Enable the Neutron Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level NetworkPolicy
that restricts the neutron-operator pod's egress and ingress to the minimum
required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the **operator
> pod itself**. For the per-CR policy that protects the Neutron API pods
> (`spec.networkPolicy` on a `Neutron`), see the
> [Neutron CRD API Reference](../../reference/neutron/neutron-crd.md). That
> policy restricts ingress to TCP 9696 from the sources you list (at least one
> is required, so an empty list is refused) and derives its egress from the CR:
> DNS, the database, the Keystone endpoint's port, the cache, the two OVN
> databases, and the broker port, with `additionalEgress` appended after them.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 7, so the neutron-operator is running (namespace `neutron-system`)
alongside the `controlplane-neutron` child it reconciles.
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
2. A running `neutron-operator` Helm release (namespace `neutron-system`).
3. The ControlPlane declares `spec.services.neutron`. Without it no
   `controlplane-neutron` child exists and the Step 2 verification has nothing
   to roll.

## Step 1 — Enable the policy

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

On the tutorial devstacks the `neutron-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. A raw
`helm upgrade` is reverted by the Flux helm-controller on its next reconcile;
outside Flux, the same values go through `helm upgrade --reuse-values`.
Substitute the CIDRs and ports from above; the single-CIDR list below stands in
for the kind case of one control-plane node:

```bash
kubectl patch helmrelease neutron-operator -n neutron-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/neutron-operator -n neutron-system \
  --for=condition=Ready --timeout=5m
```

The rendered policy allows egress to the kube-apiserver, to DNS and to the
Neutron API, and ingress to the webhook port. The chart adds the Neutron API
rule through the `operator-library.chart.networkPolicyEgress` hook, the hook a
chart uses to append an egress rule of its own: TCP 9696 to every pod labelled
`app.kubernetes.io/name: neutron` and
`app.kubernetes.io/managed-by: neutron-operator`, in any namespace. It needs no
value. The operator's health check GETs the Neutron API over the workload
Service, and the CNI judges that connection after the Service DNAT, at the API
pod and its port, so the rule selects the API pods by the labels the operator
sets on them. One deployment shape does need more: with a `Neutron` or
`NeutronMetadataAgent` placed on a registered target cluster, the operator talks
to that cluster's API server too, so add its address and port to the same
`kubeApiServer` lists.

The reverse direction needs nothing: the per-CR policy appends an ingress peer
selecting the operator's namespace to every policy it renders, so a workload
namespace running `spec.networkPolicy` already admits the probe.

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port (8080) at all, so a Prometheus that scraped the operator
before stops reaching it. Name the scraping namespace when you enable both this
policy and the ServiceMonitor from
[Enable the Neutron Operator Metrics Endpoint](./enable-neutron-operator-metrics.md):

```bash
kubectl patch helmrelease neutron-operator -n neutron-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

### Upgrading from the hand-written probe policy

neutron-operator charts before 0.6.0 rendered no rule for the health check, and
earlier versions of this guide had you apply a second policy,
`neutron-operator-api-probe`, beside the chart's. The chart covers that traffic
now, so delete the hand-written policy once the upgraded chart has rendered its
own:

```bash
kubectl delete networkpolicy neutron-operator-api-probe -n neutron-system \
  --ignore-not-found
```

## Step 2 — Verify

On the kind devstack this verifies the policy's **shape** and that enabling it
does **not break** reconciliation, not traffic enforcement, which the default
`kindnet` CNI does not apply (see the prerequisite above).

```bash
kubectl -n neutron-system get networkpolicy
kubectl -n neutron-system describe networkpolicy neutron-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `ControlPlane` CR and watching the projected network service roll out:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"neutron":{"api":{"replicas":2}}}}}'
kubectl rollout status deploy/controlplane-neutron -n openstack

# revert
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"neutron":{"api":{"replicas":1}}}}}'
```

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-neutron` child: the c5c3-operator re-asserts the child's
`spec.deployment.replicas` on every reconcile, so a direct edit of the child is
taken back.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`neutron-operator` pod restarts.

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

### Every Neutron and NeutronMetadataAgent write is rejected

**Symptom:** `kubectl apply` on either kind fails with `failed calling webhook`
plus `connection refused` or `no route to host`. The `ControlPlane` stalls too:
the c5c3-operator projects `controlplane-neutron` through the same admission
path.

**Diagnosis:** webhook ingress (9443) is blocked. The chart registers a mutating
and a validating webhook for each of the two kinds, all four with
`failurePolicy=Fail` on create and update, so an unreachable operator turns into
a cluster-wide rejection of every write to both kinds. Deletes pass: the rules
list `CREATE` and `UPDATE` only, which is what keeps a down operator from
blocking CR and namespace teardown. The usual cause is an API server that calls
webhooks from an IP that is **not** in `endpoints/kubernetes`, for example
because it sits behind a front-end proxy.
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease neutron-operator -n neutron-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and the
operator reverts to unrestricted pod networking without a pod restart.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The neutron chart's helm-unittest
(`operators/neutron/helm/neutron-operator/tests/networkpolicy_test.yaml`) pins
the egress rule the chart adds for its API health check. The keystone chart's
policy is exercised end-to-end on the CI e2e kind cluster by the chainsaw suite
below.

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```
