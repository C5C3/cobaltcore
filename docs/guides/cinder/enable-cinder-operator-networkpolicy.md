---
title: Enable the Cinder Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `cinder-system`; workload (Cinder, CinderBackend, CinderBackupBackend) stays in `openstack`. -->

# How-to: Enable the Cinder Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level NetworkPolicy
that restricts the cinder-operator pod's egress and ingress to the minimum
required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the **operator
> pod itself**. For the per-CR policy that protects the Cinder pods
> (`spec.networkPolicy` on a `Cinder`), see the
> [Cinder CRD API Reference](../../reference/cinder/cinder-crd.md#spec). That
> policy restricts ingress to TCP 8776 from the sources you list (at least one
> is required, so an empty list is refused) and derives its egress from the CR:
> DNS, the database, the cache, the Keystone, Glance and Barbican endpoints, the
> broker port, and the NFS exports, with `additionalEgress` appended after them.
> One policy covers the API pods, the scheduler, every volume service, the
> backup service and the Job pods, because its selector carries no component
> key.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

Follow that tutorial through the block-storage block of Step 4 and the
**Create a first volume** check in Step 7, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Every resource
name in the examples below is one that devstack produces.
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
2. A running `cinder-operator` Helm release (namespace `cinder-system`).
3. The ControlPlane declares `spec.services.cinder`. Without it no
   `controlplane-cinder` child exists and the Step 2 verification has nothing to
   roll.

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

On the tutorial devstacks the `cinder-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. A raw
`helm upgrade` is reverted by the Flux helm-controller on its next reconcile;
outside Flux, the same values go through `helm upgrade --reuse-values`.
Substitute the CIDRs and ports from above; the single-CIDR list below stands in
for the kind case of one control-plane node:

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/cinder-operator -n cinder-system \
  --for=condition=Ready --timeout=5m
```

The rendered policy allows egress to the kube-apiserver, to DNS and to the
Cinder API, and ingress to the webhook port. The chart adds the Cinder API rule
through the `operator-library.chart.networkPolicyEgress` hook, the hook a chart
uses to append an egress rule of its own: TCP 8776 to every pod labelled
`app.kubernetes.io/name: cinder` and
`app.kubernetes.io/managed-by: cinder-operator`, in any namespace. It needs no
value. The operator's health check GETs the Cinder API over the workload
Service, and the CNI judges that connection after the Service DNAT, at the API
pod and its port, so the rule selects the API pods by the labels the operator
sets on them. One deployment shape does need more: with a `Cinder` placed on a
registered target cluster, the operator talks to that cluster's API server too,
so add its address and port to the same `kubeApiServer` lists.

The reverse direction needs nothing: the per-CR policy appends an ingress peer
selecting the operator's namespace to every policy it renders, so a workload
namespace running `spec.networkPolicy` already admits the probe.

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port (8080) at all, so a Prometheus that scraped the operator
before stops reaching it. Name the scraping namespace when you enable both this
policy and the ServiceMonitor from
[Enable the Cinder Operator Metrics Endpoint](./enable-cinder-operator-metrics.md):

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

### Upgrading from the hand-written probe policy

cinder-operator charts before 0.5.0 rendered no rule for the health check, and
earlier versions of this guide had you apply a second policy,
`cinder-operator-api-probe`, beside the chart's. The chart covers that traffic
now, so delete the hand-written policy once the upgraded chart has rendered its
own:

```bash
kubectl delete networkpolicy cinder-operator-api-probe -n cinder-system \
  --ignore-not-found
```

## Step 2 — Verify

On the kind devstack this verifies the policy's **shape** and that the operator
still reconciles under the enforced policy. It sends no traffic outside the
allow-list (see the prerequisite above).

Restart the operator first. NetworkPolicy enforcement is stateful: a connection
opened before the policy existed stays open, so a wrong `kubeApiServer` rule
shows only after the next restart:

```bash
kubectl -n cinder-system rollout restart deploy/cinder-operator
kubectl -n cinder-system rollout status deploy/cinder-operator --timeout=5m
```

A rollout that does not finish within the timeout, with the new pod never
`Ready`, means the egress rule does not admit the API server.
See [Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

Then check the policy:

```bash
kubectl -n cinder-system get networkpolicy
kubectl -n cinder-system describe networkpolicy cinder-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `ControlPlane` CR and watching the projected block-storage API roll out.
The block records the ControlPlane's current count, which is empty while the
ControlPlane leaves it at the default, scales the API one replica above the size
it runs at, and puts the count back:

```bash
if CUR=$(kubectl get controlplane controlplane -n openstack \
     -o jsonpath='{.spec.sizing.cinder.api.replicas}') &&
   RUN=$(kubectl get deploy/controlplane-cinder -n openstack \
     -o jsonpath='{.spec.replicas}'); then
  NEW=$((RUN + 1))

  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"cinder\":{\"api\":{\"replicas\":$NEW}}}}}"
  kubectl wait deploy/controlplane-cinder -n openstack --timeout=5m \
    --for=jsonpath='{.spec.replicas}'="$NEW"
  kubectl rollout status deploy/controlplane-cinder -n openstack --timeout=5m

  # revert to the recorded count; null drops the override again
  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"cinder\":{\"api\":{\"replicas\":${CUR:-null}}}}}}"
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
patch travels from the ControlPlane to the `Cinder` CR to the Deployment, and
the restarted operator writes nothing until it holds the leader lease. Until
the cinder operator writes the new count the Deployment is still fully rolled
out at its old size, so a `rollout status` alone would pass at once. The new
count is always one above the running one, so the patch changes the Deployment
whatever size the API already runs at.

`spec.sizing.cinder.api.replicas` sizes the API Deployment alone. The scheduler
takes a count of its own under `spec.sizing.cinder.scheduler`, and the volume
services and the backup service are pinned to one replica by the projection, so
this patch rolls `controlplane-cinder` and leaves the storage path untouched.

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-cinder` child: the c5c3-operator re-asserts the child's
`spec.api.deployment.replicas` on every reconcile, so a direct edit of the child
is taken back.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`cinder-operator` pod restarts.

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

### Every Cinder, CinderBackend and CinderBackupBackend write is rejected

**Symptom:** `kubectl apply` on any of the three kinds fails with
`failed calling webhook` plus `connection refused` or `no route to host`. The
`ControlPlane` stalls too: the c5c3-operator projects `controlplane-cinder` and
its `nfs1` / `nfsbk` satellites through the same admission path.

**Diagnosis:** webhook ingress (9443) is blocked. The chart renders one
MutatingWebhookConfiguration and one ValidatingWebhookConfiguration carrying six
webhooks between them (`mcinder.kb.io`, `mcinderbackend.kb.io`,
`mcinderbackupbackend.kb.io`, `vcinder.kb.io`, `vcinderbackend.kb.io`,
`vcinderbackupbackend.kb.io`), all with `failurePolicy: Fail` on create and
update, so an unreachable operator turns into a cluster-wide rejection of every
write to all three kinds. Deletes pass: the rules list `CREATE` and `UPDATE`
only, which is what keeps a down operator from blocking CR and namespace
teardown. The usual cause is an API server that calls webhooks from an IP that
is **not** in `endpoints/kubernetes`, for example because it sits behind a
front-end proxy. `networkPolicy.webhookClients.cidrs` falls back to
`kubeApiServer.cidrs` when empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile, and
the operator reverts to unrestricted pod networking without a pod restart unless
a `cinder-operator-api-probe` policy from an earlier version of this guide still
selects the pod. That policy allows 8776 alone, so delete it as well:

```bash
kubectl delete networkpolicy cinder-operator-api-probe -n cinder-system \
  --ignore-not-found
```

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The cinder chart's helm-unittest
(`operators/cinder/helm/cinder-operator/tests/networkpolicy_test.yaml`) pins the
egress rule the chart adds for its API health check. The keystone chart's policy
is exercised end-to-end on the CI e2e kind cluster by the chainsaw suite below.

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

A second suite, `tests/e2e/cinder/network-policy`, covers the other policy: the
one the operator projects around the Cinder pods from `spec.networkPolicy` on a
`Cinder` CR. That is a different subject from this guide's, and the
[Cinder CRD API Reference](../../reference/cinder/cinder-crd.md#spec) documents
its field.
