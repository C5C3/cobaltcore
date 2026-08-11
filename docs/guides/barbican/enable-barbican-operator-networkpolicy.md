---
title: Enable the Barbican Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `barbican-system`; workload (Barbican CR) stays in `openstack`. -->

# How-to: Enable the Barbican Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level NetworkPolicy
that restricts the barbican-operator pod's egress and ingress to the minimum
required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects the
> Barbican API pods (`spec.networkPolicy` on a Barbican CR), see the
> [Barbican CRD API Reference](../../reference/barbican/barbican-crd.md).
> That per-CR policy restricts ingress to TCP 9311 from the sources you list
> (at least one is required, so an empty list is refused) and derives its egress
> from the CR: DNS, the database, the Keystone endpoint's port, the cache, and
> the OpenBao servers of its attached secret stores, with `additionalEgress`
> appended after it.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the barbican-operator
is running (namespace `barbican-system`) alongside the projected
`controlplane-barbican` key manager.
:::

1. **A CNI that enforces `networking.k8s.io/v1` NetworkPolicy (required for
   real enforcement).** Confirm with your platform team (Calico, Cilium, and
   Antrea enforce).

   ::: warning Enforcement cannot be verified on the default devstack CNI
   The ControlPlane Quick Start kind devstack uses the default `kindnet` CNI,
   which **silently ignores** NetworkPolicy objects, and kind fixes the CNI at
   cluster creation so it cannot be swapped in afterwards. The policy object
   is still created and the operator keeps reconciling, so Step 2 below
   confirms only the policy's **shape** and that enabling it does **not
   break** reconciliation. It does **not** prove that packets outside the
   allow-list are dropped. Real enforcement requires a cluster whose CNI
   enforces NetworkPolicy, typically your production platform.
   :::
2. A running `barbican-operator` Helm release (namespace `barbican-system`).
3. The ControlPlane declares `spec.services.barbican`. Without it no
   `controlplane-barbican` child exists and the Step 2 verification has nothing
   to roll. See
   [Run Barbican on a Dedicated OpenBao](./barbican-dedicated-openbao.md).

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, the real API server addresses, not the `10.96.0.1`
Service VIP that `kubectl get service kubernetes` reports. An enforcing CNI
(Calico, Cilium) DNATs a packet aimed at the VIP to one of those endpoint IPs
before it evaluates policy, so a rule naming the VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints, not just the first. A kind
devstack reports the one control-plane node's address on the kind bridge
(`172.18.0.x`), but an HA control plane reports one per API server replica:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
```

The rule must cover all of them, or the operator loses its leader-election lease
whenever it happens to be talking to an excluded replica.

On the tutorial devstacks the `barbican-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`, not with a raw
`helm upgrade`, which the Flux helm-controller reverts on its next reconcile.
Substitute the CIDRs and ports from above. The single-CIDR list below stands in
for the kind case (one control-plane node), so replace `172.18.0.2/32` with the
address you recorded and extend the list to every IP an HA control plane prints:

```bash
kubectl patch helmrelease barbican-operator -n barbican-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/barbican-operator -n barbican-system \
  --for=condition=Ready --timeout=5m
```

The rendered policy declares `policyTypes: [Ingress, Egress]`, so both directions
default-deny and only the rules the chart emits get through: egress to the
kube-apiserver, to DNS, and to OpenBao on TCP 8200, plus ingress to the webhook
and metrics ports.

### API egress is not in the chart

The barbican-operator probes `/healthcheck` on the Barbican Service over TCP 9311
in the workload namespace to set `BarbicanAPIReady`. The chart emits **no** egress
rule for that port and offers no value to add one, so on an enforcing CNI the
probe is dropped and every Barbican parks on `BarbicanAPIReady=False` /
`APIUnhealthy` while the API itself is serving normally. Cover it with a second
NetworkPolicy of your own selecting the operator pod — policies are additive, so
its egress rule joins the chart's:

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: barbican-operator-api-egress
  namespace: barbican-system
spec:
  podSelector:
    matchLabels:
      # The chart's own selector, so this policy attaches to the same pod.
      app.kubernetes.io/name: barbican-operator
      app.kubernetes.io/instance: barbican-operator
  policyTypes:
    - Egress
  egress:
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: openstack
      ports:
        - protocol: TCP
          port: 9311
```

The ingress leg of that same probe needs nothing from you: when the workload
namespace runs a per-CR NetworkPolicy, the sub-reconciler appends an
operator-namespace ingress peer to every policy it renders.

### OpenBao egress

The barbican-operator dials the OpenBao server of every store it reconciles, so
the chart carries a rule of its own for it, on by default:

```yaml
networkPolicy:
  openBao:
    enabled: true
```

The rule names TCP 8200 and no peer. A brownfield store points at a user-supplied
server whose address the chart cannot know, so restricting the destination would
mean guessing it. Set `openBao.enabled=false` when every store in the cluster is
reached over a different posture, a non-standard port or an egress proxy already
covered by another policy, and add the matching rule yourself. With the policy on
and this rule off, the store controller cannot reach any server and every store
parks on `CredentialsReady=False` with reason `OpenBaoUnreachable`.

The per-CR policy derives its OpenBao egress differently, so do not read one as
the model for the other. It emits a **single** destination-unrestricted egress
rule carrying one port entry per distinct port across the store hosts of that
Barbican, deduplicated and sorted ascending, and only when at least one
credential-ready store yields a usable port. Ten stores on ten hosts that all
listen on 8200 therefore produce one rule with one port.

### Metrics ingress

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is empty
by default, and with the policy on, an empty list means no ingress rule for the
metrics port at all, so a Prometheus that scraped the operator before stops
reaching it. Name the scraping namespace when you enable both this policy and the
ServiceMonitor from
[Enable the Barbican Operator Metrics Endpoint](./enable-barbican-operator-metrics.md):

```bash
kubectl patch helmrelease barbican-operator -n barbican-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade`. The fail-closed guard still requires
`kubeApiServer.cidrs` and `ports`:

```bash
helm upgrade barbican-operator oci://ghcr.io/c5c3/charts/barbican-operator \
  --namespace barbican-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

Do **not** run this on the tutorial devstacks: there the release is Flux-owned,
and the helm-controller reverts out-of-band revisions on its next reconcile. Use
the HelmRelease patch above instead.
:::

## Step 2 — Verify

On the kind devstack this verifies the policy's **shape** and that enabling it
does **not break** reconciliation, not traffic enforcement, which the default
`kindnet` CNI does not apply (see the prerequisite above).

```bash
kubectl -n barbican-system get networkpolicy
kubectl -n barbican-system describe networkpolicy barbican-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `ControlPlane` CR and watching the projected key manager roll out:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"barbican":{"replicas":2}}}}'
kubectl rollout status deploy/controlplane-barbican -n openstack

# revert
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"barbican":{"replicas":1}}}}'
```

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-barbican` child: the c5c3-operator re-asserts the child's
`spec.deployment.replicas` on every reconcile, so a direct edit of the child is
reverted.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`barbican-operator` pod restarts.

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

### Every secret store reports OpenBaoUnreachable

**Symptom:** every `BarbicanSecretStore` in the cluster flips to
`CredentialsReady=False` / `OpenBaoUnreachable` with `context deadline exceeded`,
including stores that were Ready before the policy went on.

**Diagnosis:** the store controller's egress to the OpenBao API is blocked. A
dropped packet has no refusal to report, so the client waits out its own deadline
and reports a timeout rather than a connection error. The usual causes are
`networkPolicy.openBao.enabled=false` with no replacement rule, or a server that
listens on a port other than 8200.

**Fix:** re-enable the chart rule. Its port is fixed at 8200 and the chart offers
no override, so a server on another port needs a second NetworkPolicy of your own
selecting the operator pod; policies are additive, so its egress rule joins the
chart's. Verify from the rendered object rather than from the values:

```bash
kubectl -n barbican-system get networkpolicy barbican-operator \
  -o jsonpath='{.spec.egress[*].ports}'
```

### Every Barbican write is rejected

**Symptom:** `kubectl apply` on a `Barbican` or `BarbicanSecretStore` CR fails
with `failed calling webhook` plus `connection refused` or `no route to host`.
The `ControlPlane` stalls too: the c5c3-operator projects `controlplane-barbican`
and `controlplane-barbican-store` through the same admission path, so its
reconcile fails on the same error.

**Diagnosis:** webhook ingress (9443) is blocked. Both barbican webhooks register
`failurePolicy=Fail` on create and update, so an unreachable operator turns into
a cluster-wide rejection of every write to either kind. The usual cause is an API
server that calls webhooks from an IP that is **not** in `endpoints/kubernetes`,
for example because it sits behind a front-end proxy.
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease barbican-operator -n barbican-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and the
operator reverts to unrestricted pod networking without a pod restart.

## Tested by

Every operator chart carries an equivalent chart-level NetworkPolicy template
(each chart ships its own copy, not a shared operator-library helper). The
keystone chart's copy is exercised end-to-end on the CI e2e kind cluster by the
chainsaw suite below; the barbican chart's copy, including its OpenBao egress
rule, is pinned by its helm-unittest
(`operators/barbican/helm/barbican-operator/tests/networkpolicy_test.yaml`).

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```
