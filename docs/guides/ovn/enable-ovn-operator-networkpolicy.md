---
title: Enable the OVN Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `ovn-system`; workload (OVNCentral, OVNChassis) stays in `openstack`. -->

# How-to: Enable the OVN Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level NetworkPolicy
that restricts the ovn-operator pod's egress and ingress to the minimum required
for correct reconciliation.

> **Scope.** Neither `OVNCentral` nor `OVNChassis` carries a `spec.networkPolicy`
> field, so there is no per-CR policy to pair this one with. The OVN workload
> exposes two OVSDB databases that authenticate every connection with a
> certificate the `OVNCentral` issues, and a chassis layer that runs in the
> node's own network namespace, where a pod-selector policy has nothing to
> match. This guide covers the operator pod alone.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 6, so the ovn-operator is running (namespace `ovn-system`) alongside the
`controlplane-ovn` central it reconciles.
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
2. A running `ovn-operator` Helm release (namespace `ovn-system`).
3. The `OVNCentral` `controlplane-ovn`, which the quick start applies in its
   Step 3. Without it the Step 2 verification has nothing to roll.

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

On the tutorial devstacks the `ovn-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. A raw
`helm upgrade` is reverted by the Flux helm-controller on its next reconcile.
Substitute the CIDRs and ports from above; the single-CIDR list below stands in
for the kind case of one control-plane node:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/ovn-operator -n ovn-system \
  --for=condition=Ready --timeout=5m
```

The rendered policy allows what the operator needs: egress to the kube-apiserver
and to DNS, ingress to the webhook port. Nothing else belongs on the list. The
ovn-operator opens no connection of its own to OVN. It writes StatefulSets,
Deployments, DaemonSets, ConfigMaps and Jobs, and the OVSDB traffic is theirs,
from the workload namespace. The chart also defines no
`operator-library.chart.networkPolicyEgress` override, the hook a chart uses to
append an egress rule of its own (the barbican chart appends its OpenBao port),
so the shared set is the whole set. One deployment shape does need more: with an
`OVNCentral` or `OVNChassis` placed on a registered target cluster, the operator
talks to that cluster's API server too, so add its address and port to the same
`kubeApiServer` lists.

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port (8080) at all, so a Prometheus that scraped the operator
before stops reaching it. Name the scraping namespace when you enable both this
policy and the ServiceMonitor from
[Enable the OVN Operator Metrics Endpoint](./enable-ovn-operator-metrics.md):

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

## Step 2 — Verify

On the kind devstack this verifies the policy's **shape** and that enabling it
does **not break** reconciliation, not traffic enforcement, which the default
`kindnet` CNI does not apply (see the prerequisite above).

```bash
kubectl -n ovn-system get networkpolicy
kubectl -n ovn-system describe networkpolicy ovn-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `OVNCentral` and watching northd roll out. The central is a CR you own on
this devstack: the ControlPlane references it and projects nothing onto it, so
the edit stands until you revert it yourself:

```bash
kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
  -p '{"spec":{"northd":{"deployment":{"replicas":2}}}}'
kubectl rollout status deploy/controlplane-ovn-northd -n openstack

# revert
kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
  -p '{"spec":{"northd":{"deployment":{"replicas":1}}}}'
```

The second replica is idle by design: one northd holds the Southbound lock and
the rest wait on it, so the rollout proves the write path without changing what
the control plane computes.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`ovn-operator` pod restarts.

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

### Every OVNCentral and OVNChassis write is rejected

**Symptom:** `kubectl apply` on either kind fails with `failed calling webhook`
plus `connection refused` or `no route to host`.

**Diagnosis:** webhook ingress (9443) is blocked. The chart registers a mutating
and a validating webhook for each of the two kinds, all four with
`failurePolicy=Fail` on create and update, so an unreachable operator turns into
a cluster-wide rejection of every write to both kinds. The usual cause is an API
server that calls webhooks from an IP that is **not** in `endpoints/kubernetes`,
for example because it sits behind a front-end proxy.
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and the
operator reverts to unrestricted pod networking without a pod restart.

## Tested by

Every operator chart carries an equivalent chart-level NetworkPolicy template,
included from the shared operator-library. The keystone chart's copy is
exercised end-to-end on the CI e2e kind cluster by the chainsaw suite below; the
ovn chart ships no NetworkPolicy unit test of its own, and the rendered policy
every chart gets is pinned by the library's own helm-unittest,
`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`
(default-off posture, rule shape, both fail-closed guards, and the absence of an
ingress rule for the health-probe port).

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```
