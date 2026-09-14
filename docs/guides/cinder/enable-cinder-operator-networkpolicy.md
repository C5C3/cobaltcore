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

Follow that tutorial through the block-storage block of Step 3 and the
**Create a first volume** check in Step 6, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Every resource
name in the examples below is one that devstack produces.
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

The rendered policy allows egress to the kube-apiserver and to DNS, and ingress
to the webhook port. The chart defines no
`operator-library.chart.networkPolicyEgress` override, the hook a chart uses to
append an egress rule of its own (the barbican chart appends its OpenBao port),
so the shared set is the whole set. One deployment shape does need more: with a
`Cinder` placed on a registered target cluster, the operator talks to that
cluster's API server too, so add its address and port to the same
`kubeApiServer` lists.

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

### Not covered: the API health probe (port 8776)

The operator's health-check step GETs the Cinder API's oslo healthcheck endpoint
over the workload Service,
`http://{name}.{namespace}.svc.cluster.local:8776/healthcheck`, from the
operator pod in `cinder-system`. The shared library template renders egress to
DNS, to the `kubeApiServer` CIDRs, and to whatever the
`operator-library.chart.networkPolicyEgress` hook holds, and the cinder chart
leaves that hook at the library's empty default. **The chart renders no egress
rule for 8776.** On an enforcing CNI the probe is therefore blocked, and
`CinderAPIReady` goes `False` with `HealthCheckTimeout` ("health check timed
out") once the 10-second probe deadline expires, since a default-deny policy
drops the packet and never answers it. A peer that answers with a TCP reset
yields `ConnectionFailed`. No other condition is affected, so a `Cinder` whose
`DeploymentReady` is `True` while `CinderAPIReady` times out is the fingerprint
of this gap.

NetworkPolicies on the same pod are unioned, so an additive object beside the
chart's covers the gap with no chart change. Apply it in the operator namespace:

```yaml
# cinder-operator-api-probe.yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: cinder-operator-api-probe
  namespace: cinder-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: cinder-operator
      app.kubernetes.io/instance: cinder-operator
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: openstack
      ports:
        - protocol: TCP
          port: 8776
```

```bash
kubectl apply -f cinder-operator-api-probe.yaml
```

::: warning Apply this only after the chart policy is on
Policies union, but this object is not additive on its own: it declares
`policyTypes: [Egress]` and selects the operator pod, so applying it before the
Step 1 patch reconciles turns unrestricted egress into egress to 8776 alone. The
operator then loses the kube-apiserver, its leader-election lease stops
renewing, the pod restarts into the same wall, and every `Cinder`,
`CinderBackend` and `CinderBackupBackend` stops reconciling. The chart's
fail-closed guard on `kubeApiServer.cidrs` does not catch this: it protects the
chart's own template, not an object you applied by hand.
:::

The two selector labels are the chart's own
(`operator-library.selectorLabels`), where the instance value is the Helm
release name. Change `openstack` to the namespace your `Cinder` runs in, and add
one `to` peer per namespace when more than one carries a `Cinder`.

The reverse direction needs nothing: the per-CR policy appends an ingress peer
selecting the operator's namespace to every policy it renders, so a workload
namespace running `spec.networkPolicy` already admits the probe.

## Step 2 — Verify

On the kind devstack this verifies the policy's **shape** and that enabling it
does **not break** reconciliation, not traffic enforcement, which the default
`kindnet` CNI does not apply (see the prerequisite above).

```bash
kubectl -n cinder-system get networkpolicy
kubectl -n cinder-system describe networkpolicy cinder-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `ControlPlane` CR and watching the projected block-storage API roll out.
Read the current replica count first, so you can put it back:

```bash
kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.spec.services.cinder.replicas}'

kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"cinder":{"replicas":2}}}}'
kubectl rollout status deploy/controlplane-cinder -n openstack

# revert to the count you read above
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"cinder":{"replicas":1}}}}'
```

`services.cinder.replicas` sizes the API Deployment alone. The scheduler, the
volume services and the backup service are pinned to one replica by the
projection, so this patch rolls `controlplane-cinder` and leaves the storage
path untouched.

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
the same patch shape **and** delete the additive object above, if you applied
it:

```bash
kubectl delete networkpolicy cinder-operator-api-probe -n cinder-system \
  --ignore-not-found
```

Both have to go. Either one left selecting the pod keeps restricting it, and
`cinder-operator-api-probe` alone is the narrower of the two: it allows 8776 and
nothing else. With no policy selecting it the operator reverts to unrestricted
pod networking without a pod restart.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library. The keystone chart's copy is exercised end-to-end
on the CI e2e kind cluster by the borrowed chainsaw suite below; there is no
cinder twin of it. The cinder chart's own helm-unittests cover the ClusterRole,
the Role, the Deployment, the values schema and the webhook configurations, and
carry no networkpolicy test, so the rendered policy every chart gets is pinned by
the library's own helm-unittest,
`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`
(default-off posture, rule shape, both fail-closed guards, and the absence of an
ingress rule for the health-probe port).

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

A second suite, `tests/e2e/cinder/network-policy`, covers the other policy: the
one the operator projects around the Cinder pods from `spec.networkPolicy` on a
`Cinder` CR. That is a different subject from this guide's, and the
[Cinder CRD API Reference](../../reference/cinder/cinder-crd.md#spec) documents
its field.
