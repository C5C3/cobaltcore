---
title: Keystone Operator NetworkPolicy
quadrant: operator
feature: CC-0090
---

# Keystone Operator NetworkPolicy

Reference documentation for the chart-level NetworkPolicy that restricts the
egress and ingress of the keystone-operator pod itself (CC-0090). This is
distinct from the per-CR NetworkPolicy emitted by
[`reconcileNetworkPolicy`](./keystone-reconciler.md#sub-reconciler-contracts)
(CC-0039), which protects Keystone API pods rendered *from* a `Keystone` CR.

- **Scope:** the operator Deployment pod selected by
  `keystone-operator.selectorLabels`.
- **Chart:** `operators/keystone/helm/keystone-operator`.
- **Template:** `templates/networkpolicy.yaml`.
- **Values schema:** the authoritative contract for all tunables is
  [`values.schema.json`](https://github.com/c5c3/forge/blob/main/operators/keystone/helm/keystone-operator/values.schema.json).

## Overview

When `networkPolicy.enabled=true`, the chart renders one `networking.k8s.io/v1`
`NetworkPolicy` that:

1. Default-denies **both** directions for the operator pod by listing
   `Ingress` and `Egress` in `policyTypes` without a catch-all rule.
2. Opens explicit egress to the kube-apiserver (required for all
   controller-runtime clients and leader election).
3. Opens explicit egress to cluster DNS (required to resolve Service DNS
   names used by ESO, MariaDB, and any hostname-based client config).
4. When `webhook.enabled=true`, opens explicit ingress on TCP 9443 from the
   API-server CIDRs so admission webhooks are reachable.
5. When `networkPolicy.allowMetricsFrom` is non-empty, opens explicit
   ingress on the metrics port from the listed peers (opt-in).

Two failure modes are explicitly refused by the template at render time
(fail-closed — REQ-006): an empty `kubeApiServer.cidrs` or an empty
`kubeApiServer.ports` list while `enabled=true`. Either condition triggers
`{{ fail }}` rather than rendering a NetworkPolicy that would block all
controller traffic or open every port.

## Default-off posture

`networkPolicy.enabled` defaults to `false`. Rationale:

- Many production clusters run CNIs that do not enforce NetworkPolicy
  (kindnet without extension, Flannel without `kube-router`, etc.). Opting
  in by default would silently provide no protection on those clusters
  while adding surface area on clusters that do enforce it.
- Both `kubeApiServer.cidrs` and `kubeApiServer.ports` are
  cluster-specific. There is no safe default that works across kind, GKE,
  EKS, AKS, and on-prem kubeadm installations.
- Operators upgrading from earlier chart versions keep working with no
  change in values.

Operators must **explicitly opt in** by setting `networkPolicy.enabled=true`
and populating `kubeApiServer.cidrs` / `kubeApiServer.ports`. The
[how-to guide](../../guides/enable-keystone-operator-networkpolicy.md) walks
through the enablement steps.

## Rules rendered

All rules below are emitted on a single `NetworkPolicy` object named after
`include "keystone-operator.fullname" .` in the release namespace, with
`spec.podSelector` matching the operator Deployment's selector labels.

### Egress

| Direction | Protocol / Port | Peer | Values key | Default | Gated by |
| --- | --- | --- | --- | --- | --- |
| Egress | UDP 53 + TCP 53 | `namespaceSelector` + `podSelector` | `networkPolicy.dns.namespaceSelector`, `networkPolicy.dns.podSelector` | `kubernetes.io/metadata.name: kube-system` + `k8s-app: kube-dns` | `networkPolicy.dns.enabled` (default `true`) |
| Egress | TCP `<ports[*]>` | `ipBlock[*]` | `networkPolicy.kubeApiServer.cidrs`, `networkPolicy.kubeApiServer.ports` | `[]` (must be set when `enabled=true`) | Always when `networkPolicy.enabled=true` |

> **One rule, N×M tuples.** The kube-apiserver rule emits a **single**
> `egress` entry with all CIDRs under `to:` and all ports under `ports:`. By
> NetworkPolicy semantics this permits every (cidr, port) combination — one
> rule with three CIDRs and two ports covers six tuples. Do not expand the
> list into one rule per tuple (REQ-004).

### Ingress

| Direction | Protocol / Port | Peer | Values key | Default | Gated by |
| --- | --- | --- | --- | --- | --- |
| Ingress | TCP 9443 | `ipBlock[*]` | `networkPolicy.webhookClients.cidrs` (falls back to `networkPolicy.kubeApiServer.cidrs` when empty) | fallback to `kubeApiServer.cidrs` | `webhook.enabled` (default `true`) |
| Ingress | TCP `<metrics.port>` | each entry from `allowMetricsFrom` rendered verbatim as a `NetworkPolicyPeer` | `networkPolicy.allowMetricsFrom` | `[]` | Non-empty `allowMetricsFrom` |

### Not covered: health probes (port 8081)

The operator exposes liveness and readiness probes on TCP 8081. These probes
are called by the **kubelet** from the node's host network namespace, which
is not subject to `NetworkPolicy` in the standard CNIs
([Calico](https://docs.tigera.io/calico/latest/network-policy/policy-rules/kubernetes),
[Cilium](https://docs.cilium.io/en/stable/network/kubernetes/policy/), Antrea)
— see the upstream
[Kubernetes NetworkPolicy "what you can't do" list](https://kubernetes.io/docs/concepts/services-networking/network-policies/#what-you-can-t-do-with-network-policies-at-least-not-yet).
Therefore the template renders **no ingress rule for 8081**, and adding one
is unnecessary (REQ-009). Probes continue to work with the default-deny
ingress posture.

## Values snippet: kind cluster

The following snippet enables the policy on a local
[kind](https://kind.sigs.k8s.io/) cluster, whose API server endpoint is
reachable at `10.96.0.1:6443` via the built-in `kubernetes` Service:

```yaml
networkPolicy:
  enabled: true
  kubeApiServer:
    cidrs:
      - 10.96.0.1/32
    ports:
      - 6443
  # dns, allowMetricsFrom, webhookClients left at defaults
```

This same snippet is the fixture used by the chart-level E2E test at
`tests/e2e/keystone-operator/network-policy-egress/00-install-operator.yaml`
(CC-0090, REQ-010) and is the minimum viable configuration on kind.

### Production example

Production clusters typically have the API server behind a VIP or NLB
outside the cluster CIDR. Discover the endpoint with:

```bash
kubectl get endpoints kubernetes -o json \
  | jq -r '.subsets[] | .addresses[].ip as $ip | .ports[].port as $p | "\($ip) \($p)"'
```

and set `cidrs`/`ports` accordingly. Metrics scraping peers are added
explicitly:

```yaml
networkPolicy:
  enabled: true
  kubeApiServer:
    cidrs: ["10.0.0.10/32", "10.0.0.11/32", "10.0.0.12/32"]
    ports: [6443]
  allowMetricsFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
      podSelector:
        matchLabels:
          app.kubernetes.io/name: prometheus
```

## Fail-closed guards (REQ-006)

The template mirrors the defensive guard convention from the per-CR
NetworkPolicy sub-reconciler
(`operators/keystone/internal/controller/reconcile_networkpolicy.go`, lines
~61-63). If `networkPolicy.enabled=true` but either `kubeApiServer.cidrs`
or `kubeApiServer.ports` is empty, Helm render aborts with one of:

```
Error: execution error at (keystone-operator/templates/networkpolicy.yaml):
networkPolicy.kubeApiServer.cidrs must not be empty when networkPolicy.enabled=true:
refusing to render a NetworkPolicy that would block all kube-apiserver egress
(CC-0090, REQ-006)
```

```
Error: execution error at (keystone-operator/templates/networkpolicy.yaml):
networkPolicy.kubeApiServer.ports must not be empty when networkPolicy.enabled=true:
refusing to render a NetworkPolicy that would open all ports to kube-apiserver
(CC-0090, REQ-006)
```

The JSON schema (`values.schema.json`) also enforces `minItems: 1` on both
lists when `enabled=true`, but schema validation can be bypassed with
`helm --skip-schema-validation`. The template guard is the
defense-in-depth backstop that catches that case.

## Testing

| File | Scope |
| --- | --- |
| `operators/keystone/helm/keystone-operator/tests/networkpolicy_test.yaml` | helm-unittest suite: default-off, enabled-on, DNS rule, kube-apiserver rule, webhook ingress, metrics ingress, no 8081 rule, fail-closed guards (CC-0090, REQ-001..REQ-009) |
| `operators/keystone/helm/keystone-operator/tests/schema_validation_test.yaml` | Schema tests for `networkPolicy` (invalid CIDR, out-of-range port, non-boolean `enabled`) (CC-0090, REQ-005) |
| `tests/e2e/keystone-operator/network-policy-egress/chainsaw-test.yaml` | Chainsaw E2E: operator installed with `networkPolicy.enabled=true` on kind still reconciles a minimal Keystone CR to `Ready=True` (CC-0090, REQ-010) |

Run the helm-unittest suite locally with:

```bash
helm unittest operators/keystone/helm/keystone-operator \
  -f 'tests/networkpolicy_test.yaml'
```

## Related

- [How to enable the keystone-operator NetworkPolicy](../../guides/enable-keystone-operator-networkpolicy.md) —
  step-by-step enablement, verification, and troubleshooting.
- [Keystone Reconciler Architecture](./keystone-reconciler.md) —
  including the per-CR
  [`reconcileNetworkPolicy`](./keystone-reconciler.md#sub-reconciler-contracts)
  sub-reconciler (CC-0039), which protects Keystone API pods (not the
  operator itself).
- Upstream:
  [Kubernetes NetworkPolicy concepts](https://kubernetes.io/docs/concepts/services-networking/network-policies/).
