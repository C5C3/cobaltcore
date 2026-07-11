---
title: Enable the Horizon Operator NetworkPolicy
quadrant: operator
---

<!-- operator namespace is `horizon-system`; workload (Horizon CR) stays in `openstack`. -->

# How-to: Enable the Horizon Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the horizon-operator pod's egress and ingress
to the minimum required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects the
> dashboard pods (`spec.networkPolicy` on a Horizon CR), see the
> [reconciler reference](../reference/horizon/horizon-reconciler.md).

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the horizon-operator
is running (namespace `horizon-system`) alongside the projected
`controlplane-horizon` dashboard.
:::

1. **A CNI that enforces `networking.k8s.io/v1` NetworkPolicy — for real
   enforcement.** Confirm with your platform team (Calico, Cilium, and Antrea
   enforce).

   ::: warning Enforcement cannot be verified on the default devstack CNI
   The ControlPlane Quick Start kind devstack uses the default `kindnet` CNI,
   which **silently ignores** NetworkPolicy objects, and kind fixes the CNI at
   cluster creation so it cannot be swapped in afterwards. The policy object
   is still created and the operator keeps reconciling, so Step 2 below
   confirms only the policy's **shape** and that enabling it does **not
   break** reconciliation — it does **not** prove that packets outside the
   allow-list are dropped. Real enforcement requires a cluster whose CNI
   enforces NetworkPolicy — typically your production platform, not the kind
   devstack.
   :::
2. A running `horizon-operator` Helm release (namespace `horizon-system`).

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDR and port from the
in-cluster `kubernetes` Service (on kind this is `10.96.0.1/32` port `6443`):

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

On the tutorial devstacks the `horizon-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values` — not with a
raw `helm upgrade`, which the Flux helm-controller reverts on its next
reconcile. Substitute the CIDRs and ports from above:

```bash
kubectl patch helmrelease horizon-operator -n horizon-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["10.96.0.1/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/horizon-operator -n horizon-system \
  --for=condition=Ready --timeout=5m
```

The chart-level policy allows exactly what the operator needs: egress to the
kube-apiserver and DNS, ingress to the webhook and metrics ports. The
horizon-operator's health check reaches the dashboard Service on TCP 8080 in
the workload namespace; when the workload namespace itself runs a per-CR
NetworkPolicy, the operator's namespace is admitted automatically by the
sub-reconciler (an operator-namespace ingress peer is appended to every
rendered policy).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade` — remember the fail-closed guard requires
`kubeApiServer.cidrs` and `ports`:

```bash
helm upgrade horizon-operator oci://ghcr.io/c5c3/charts/horizon-operator \
  --namespace horizon-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=10.96.0.1/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2 — Verify

On the kind devstack this verifies the policy's **shape** and that enabling it
does **not break** reconciliation — not traffic enforcement, which the default
`kindnet` CNI does not apply (see the prerequisite above).

```bash
kubectl -n horizon-system get networkpolicy
kubectl -n horizon-system describe networkpolicy horizon-operator
```

Then confirm reconciliation still works end-to-end by driving a change
through the `ControlPlane` CR and watching the projected dashboard roll out:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"horizon":{"replicas":2}}}}'
kubectl rollout status deploy/controlplane-horizon -n openstack

# revert
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"horizon":{"replicas":1}}}}'
```

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-horizon` child: the c5c3-operator re-asserts the child's
`spec.deployment.replicas` on every reconcile, so a direct edit of the child is
reverted. If the operator loses apiserver connectivity after enabling the policy,
your CNI maps the apiserver behind a different port — compare the policy's egress
ports with `kubectl get endpoints kubernetes -n default`.
