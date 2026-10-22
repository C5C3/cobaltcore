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
This guide is written against the **[Deploy a ControlPlane](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 7, so the ovn-operator is running (namespace `ovn-system`) alongside the
`controlplane-ovn` central it reconciles.
:::

1. **A CNI that enforces `networking.k8s.io/v1` NetworkPolicy (required for
   real enforcement).** Confirm with your platform team (Calico, Cilium, and
   Antrea enforce).

   ::: warning The devstack enforces this policy
   The ControlPlane Quick Start kind devstack runs kind's default `kindnet`
   CNI. On kind v0.32 and later, kindnet enforces NetworkPolicy and evaluates a
   connection to a Service after the Service DNAT, so a `kubeApiServer` rule
   that misses the API server's endpoint address cuts the operator off on the
   devstack too. Step 2 below confirms the policy's **shape** and that the
   operator still reconciles under it. It sends no traffic outside the
   allow-list, so it does **not** prove that such traffic is dropped.
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
wrong source: an enforcing CNI
(Calico, Cilium, and kindnet on kind v0.32 and later) evaluates policy after the
Service DNAT has rewritten a packet aimed at the VIP to one of those endpoint
IPs, so a rule naming the VIP never matches:

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

On the kind devstack this verifies the policy's **shape** and that the operator
still reconciles under the enforced policy. It sends no traffic outside the
allow-list (see the prerequisite above).

Restart the operator first. NetworkPolicy enforcement is stateful: a connection
opened before the policy existed stays open, so a wrong `kubeApiServer` rule
shows only after the next restart:

```bash
kubectl -n ovn-system rollout restart deploy/ovn-operator
kubectl -n ovn-system rollout status deploy/ovn-operator --timeout=5m
```

A rollout that does not finish within the timeout, with the new pod never
`Ready`, means the egress rule does not admit the API server.
See [Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

Then check the policy:

```bash
kubectl -n ovn-system get networkpolicy
kubectl -n ovn-system describe networkpolicy ovn-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `OVNCentral` and watching northd roll out. The central is a CR you own on
this devstack: the ControlPlane references it and projects nothing onto it, so
the edit stands until you revert it yourself. The block records the central's
current northd count, scales northd one replica above the size it runs at, and
puts the count back.

The patch passes through the ovn-operator's own admission webhooks. In the
first seconds after the restart, the webhook Service can still route a request
to the old pod, which no longer serves, and the patch fails with
`failed calling webhook`. That happens with the policy disabled too, so the
block tries the patch up to ten times, two seconds apart. A refused call fails
at once and the ten tries take about 20 seconds. A call the CNI drops, as
kindnet does when the webhook ingress rule misses the API server, fails only
after the API server's 10-second webhook timeout (the chart sets no
`timeoutSeconds`) with `context deadline exceeded`, and the ten tries take about
two minutes. When every try fails, the block prints a message and skips the
wait and the revert. That failure is the one under
[Every OVNCentral and OVNChassis write is rejected](#every-ovncentral-and-ovnchassis-write-is-rejected):

```bash
if CUR=$(kubectl get ovncentral controlplane-ovn -n openstack \
     -o jsonpath='{.spec.northd.deployment.replicas}') &&
   RUN=$(kubectl get deploy/controlplane-ovn-northd -n openstack \
     -o jsonpath='{.spec.replicas}'); then
  NEW=$((RUN + 1))

  patched=
  for _ in $(seq 10); do
    if kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
         -p "{\"spec\":{\"northd\":{\"deployment\":{\"replicas\":$NEW}}}}"; then
      patched=1
      break
    fi
    sleep 2
  done

  if [ -n "$patched" ]; then
    kubectl wait deploy/controlplane-ovn-northd -n openstack --timeout=5m \
      --for=jsonpath='{.spec.replicas}'="$NEW"
    kubectl rollout status deploy/controlplane-ovn-northd -n openstack --timeout=5m

    # revert to the recorded count; null drops the field again
    kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
      -p "{\"spec\":{\"northd\":{\"deployment\":{\"replicas\":${CUR:-null}}}}}"
  else
    echo "every OVNCentral patch was rejected; nothing was changed" >&2
  fi
else
  echo "could not read the replica counts; nothing was patched" >&2
fi
```

The outer `if` stops the block before the first patch when either read fails.
A failed read prints the same empty string as a central that leaves the count
unset, so without the guard the revert would drop a count the central carries,
and a failed Deployment read would set `NEW` to 1 and shrink northd instead of
growing it.

The `kubectl wait` is the check that proves the operator still reconciles. The
restarted operator writes nothing until it holds the leader lease, and until it
writes the new count the northd Deployment is still fully rolled out at its old
size, so a `rollout status` alone would pass at once. The new count is always
one above the running one, so the patch changes the Deployment whatever size
northd already runs at.

The extra replica is idle by design: one northd holds the Southbound lock and
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
plus `connection refused` or `no route to host`. A CNI that drops the packets
instead of refusing them, such as kindnet, makes the call fail with
`context deadline exceeded` once the API server's 10-second webhook timeout
expires.

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
