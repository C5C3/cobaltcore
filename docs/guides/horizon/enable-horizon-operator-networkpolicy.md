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
> [reconciler reference](../../reference/horizon/horizon-reconciler.md).

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the horizon-operator
is running (namespace `horizon-system`) alongside the projected
`controlplane-horizon` dashboard.
:::

A running `horizon-operator` Helm release (namespace `horizon-system`) is required.

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
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next
reconcile. Substitute the CIDRs and ports from above:

```bash
kubectl patch helmrelease horizon-operator -n horizon-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["10.96.0.1/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/horizon-operator -n horizon-system \
  --for=condition=Ready --timeout=5m
```

The chart-level policy allows what the operator needs: egress to the
kube-apiserver, to DNS and to the dashboard, ingress to the webhook and metrics
ports. The dashboard rule admits TCP 8080 to every pod labelled
`app.kubernetes.io/name: horizon` and
`app.kubernetes.io/managed-by: horizon-operator`, in any namespace, and needs no
value. The operator's health check GETs the dashboard over the Horizon Service,
and the CNI judges that connection after the Service DNAT, at the dashboard pod
and its port, so the rule selects the dashboard pods by the labels the operator
sets on them. When the workload namespace itself runs a per-CR NetworkPolicy,
the operator's namespace is admitted automatically by the sub-reconciler (an
operator-namespace ingress peer is appended to every rendered policy).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade`. The fail-closed guard requires
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

On kind v0.32 and later this checks the policy's shape and reconciliation
under enforcement. These commands do not attempt traffic outside the allow-list.
Restart the operator to test new connections; existing connections can remain
open after a policy change:

```bash
kubectl -n horizon-system rollout restart deploy/horizon-operator
kubectl -n horizon-system rollout status deploy/horizon-operator --timeout=5m
```

If the rollout times out, inspect the operator logs and compare the API-server
endpoint addresses and ports with the policy.

```bash
kubectl -n horizon-system get networkpolicy
kubectl -n horizon-system describe networkpolicy horizon-operator
```

Then confirm reconciliation still works end-to-end by driving a change
through the `ControlPlane` CR and watching the projected dashboard roll out:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"horizon":{"api":{"replicas":2}}}}}'
kubectl rollout status deploy/controlplane-horizon -n openstack

# revert
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"horizon":{"api":{"replicas":1}}}}}'
```

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-horizon` child: the c5c3-operator re-asserts the child's
`spec.deployment.replicas` on every reconcile, so a direct edit of the child is
reverted. If the operator loses apiserver connectivity after enabling the policy,
your CNI may map the apiserver behind a different port. Compare the policy's egress
ports with `kubectl get endpoints kubernetes -n default`.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The horizon chart's helm-unittest
(`operators/horizon/helm/horizon-operator/tests/networkpolicy_test.yaml`) pins
the egress rule the chart adds for its API health check. These tests check
rendering only; they do not exercise packet enforcement.

The Horizon-specific Chainsaw suite adds runtime enforcement coverage on the
CI kind cluster. Use the same tools, disposable devstack, image-loading and
path-resolution requirements as the
[Keystone suite](../keystone/enable-keystone-operator-networkpolicy.md#tested-by),
with the primary Horizon operator and its workload dependencies running.
From the repository root, load the Horizon `:dev` image and build its chart
dependency before running:

```bash
make image OPERATOR=horizon IMG=ghcr.io/c5c3/horizon-operator:dev
kind load docker-image ghcr.io/c5c3/horizon-operator:dev --name cobaltcore
helm dependency build --skip-refresh operators/horizon/helm/horizon-operator
chainsaw test --test-dir tests/e2e/horizon-operator/network-policy-egress
```

The suite checks that the chart renders the policy and that a Horizon CR
reaches `Ready=True` while it is present. The primary operator release competes
for the same leader lease, so a last step runs a probe pod the policy selects:
its connection to the MariaDB port must be dropped, and the dashboard must answer
`GET /auth/login/` through its Service on 8080. The step waits for the drop
first and fails when it never comes, so it passes only on a cluster that
enforces the policy, as the CI kind cluster does.

This suite covers only the Horizon chart. It does not validate other operators.
