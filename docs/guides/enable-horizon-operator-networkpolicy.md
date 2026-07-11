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

1. **A CNI that enforces `networking.k8s.io/v1` NetworkPolicy.** Confirm with
   your platform team — kindnet (the kind default) does NOT enforce policies,
   so on a kind cluster the object is created but has no effect.
2. A running `horizon-operator` Helm release (namespace `horizon-system`).

## Step 1 — Enable the policy

```bash
helm upgrade horizon-operator oci://ghcr.io/c5c3/charts/horizon-operator \
  --namespace horizon-system --reuse-values \
  --set networkPolicy.enabled=true
```

The chart-level policy allows exactly what the operator needs: egress to the
kube-apiserver and DNS, ingress to the webhook and metrics ports. The
horizon-operator's health check reaches the dashboard Service on TCP 8080 in
the workload namespace; when the workload namespace itself runs a per-CR
NetworkPolicy, the operator's namespace is admitted automatically by the
sub-reconciler (an operator-namespace ingress peer is appended to every
rendered policy).

## Step 2 — Verify

```bash
kubectl -n horizon-system get networkpolicy
kubectl -n horizon-system describe networkpolicy horizon-operator
```

Then confirm reconciliation still works end-to-end: patch any Horizon CR
(e.g. bump `spec.deployment.replicas`) and watch the change roll out. If the
operator loses apiserver connectivity after enabling the policy, your CNI
maps the apiserver behind a different port — compare the policy's egress
ports with `kubectl get endpoints kubernetes -n default`.
