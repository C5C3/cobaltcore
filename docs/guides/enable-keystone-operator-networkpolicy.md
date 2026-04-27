---
title: Enable the Keystone Operator NetworkPolicy
quadrant: operator
feature: CC-0090
---

# How-to: Enable the Keystone Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the keystone-operator pod's egress and ingress
to the minimum required for correct reconciliation (CC-0090). For the
authoritative rule table, failure modes, and schema contract, see
[Keystone Operator NetworkPolicy](../reference/keystone/keystone-operator-networkpolicy.md).

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects
> Keystone API pods (CC-0039), see the
> [reconcileNetworkPolicy sub-reconciler](../reference/keystone/keystone-reconciler.md#sub-reconciler-contracts).

---

## Prerequisites

1. **A CNI that enforces `networking.k8s.io/v1` NetworkPolicy.** Confirm with
   your platform team. Common CNIs that enforce:

   - [Calico](https://docs.tigera.io/calico/latest/network-policy/policy-rules/kubernetes)
   - [Cilium](https://docs.cilium.io/en/stable/network/kubernetes/policy/)
   - [Antrea](https://antrea.io/docs/main/docs/network-policy/)

   On kind, the default `kindnet` CNI does **not** enforce NetworkPolicy.
   Install a supported CNI (Calico or Cilium) before proceeding — otherwise
   the policy is rendered but has no effect.

2. **Permission to read the `kubernetes` default Service's endpoints** in
   order to discover the API server CIDR:

   ```bash
   kubectl auth can-i get endpoints/kubernetes -n default
   ```

3. **Existing operator deployment managed by Helm.** If you installed the
   operator via `hack/ci-deploy-operator.sh` or a raw manifest, first
   migrate to Helm — this feature is gated by chart values.

---

## 1. Gather the kube-apiserver CIDR and port

The operator reaches the API server through the in-cluster `kubernetes`
Service. Its backing endpoints give you the IPs and ports to encode in the
NetworkPolicy:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Example output on a kind cluster:

```
10.96.0.1/32 port=6443
```

Example output on a kubeadm cluster with an HA control plane:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
```

> **CIDR, not DNS.** `NetworkPolicy.spec.egress[].to[].ipBlock.cidr`
> accepts CIDR notation only. Use `/32` for single IPs. DNS names are not
> valid here.

Record every endpoint IP — the rule must cover all of them, or the operator
will lose quorum on leader-election renewals whenever it happens to be
talking to an excluded replica.

---

## 2. Update chart values

Add the following block to your Helm values file (minimum viable
configuration):

```yaml
networkPolicy:
  enabled: true
  kubeApiServer:
    cidrs:
      - 10.96.0.1/32      # replace with the CIDRs from step 1
    ports:
      - 6443              # replace with the ports from step 1
```

Optional overrides:

- **DNS peer.** If your cluster uses [CoreDNS in a non-default namespace or
  with a non-default label](https://github.com/coredns/coredns/blob/master/README.md),
  override the selectors:

  ```yaml
  networkPolicy:
    dns:
      namespaceSelector:
        kubernetes.io/metadata.name: custom-dns
      podSelector:
        app.kubernetes.io/name: coredns
  ```

- **Metrics scraping.** The metrics port is default-denied; opt in with
  explicit peers:

  ```yaml
  networkPolicy:
    allowMetricsFrom:
      - namespaceSelector:
          matchLabels:
            kubernetes.io/metadata.name: monitoring
        podSelector:
          matchLabels:
            app.kubernetes.io/name: prometheus
  ```

- **Webhook clients.** By default, webhook ingress (9443) falls back to
  `kubeApiServer.cidrs`, which is correct for the vast majority of
  clusters (the API server is the only caller of admission webhooks).
  Override only if you operate a non-standard webhook architecture — for
  example, an API server front-end that calls webhooks from a different
  CIDR than it advertises in `endpoints/kubernetes`:

  ```yaml
  networkPolicy:
    webhookClients:
      cidrs: ["10.1.0.0/24"]
  ```

---

## 3. Roll out

Apply the values change with a rolling `helm upgrade`:

```bash
helm upgrade keystone-operator oci://ghcr.io/c5c3/charts/keystone-operator \
  --namespace keystone-operator-system \
  --version <chart-version> \
  -f values.yaml
```

The upgrade creates the `NetworkPolicy` object and rolls the operator
Deployment. Existing reconciliations queue during the rollout and resume
once the new pod reaches `Ready`.

---

## Verification

### 4.1 NetworkPolicy exists and matches the pod

```bash
kubectl -n keystone-operator-system describe networkpolicy \
  $(kubectl -n keystone-operator-system get networkpolicy -o name \
    | grep keystone-operator)
```

Expected: one NetworkPolicy whose `PodSelector` matches the operator pod's
labels, `PolicyTypes: Ingress, Egress`, and egress rules for DNS and the
API server.

Confirm the selector actually matches exactly one pod:

```bash
kubectl -n keystone-operator-system get pods \
  -l app.kubernetes.io/name=keystone-operator,app.kubernetes.io/instance=keystone-operator
```

A zero-pod match is a symptom of mismatched labels — usually a stale
`fullnameOverride` or a custom `nameOverride`. The NetworkPolicy will exist
but protect no pod.

### 4.2 Operator still reconciles

Create (or update) a `Keystone` CR and watch its `Ready` condition progress
to `True`:

```bash
kubectl -n openstack get keystone <name> \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
```

If `Ready` stays `False` for more than one reconcile budget, jump to
**Troubleshooting** below.

### 4.3 Controller-runtime is talking to the API server

Tail operator logs and confirm there are no `i/o timeout`,
`connection refused`, or `context deadline exceeded` errors when the
controller-runtime client dials the API:

```bash
kubectl -n keystone-operator-system logs deploy/keystone-operator -f \
  | grep -Ei 'apiserver|leader|timeout|refused|deadline'
```

### 4.4 Chart-level E2E guardrail

The repository ships a Chainsaw E2E that exercises this exact code path —
operator installed with `networkPolicy.enabled=true`, one Keystone CR
reaches `Ready=True`:

```bash
chainsaw test tests/e2e/keystone-operator/network-policy-egress
```

This should be green on any CI environment that provisions a kind cluster
with a NetworkPolicy-enforcing CNI.

---

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout`
or leader-election lease renewals fail with `context deadline exceeded`,
and the operator pod restarts.

**Diagnosis:** the `kubeApiServer.cidrs` list is missing one or more of the
current API server endpoint IPs. Re-run step 1 — an HA control plane may
have added a new replica, or a control-plane node may have been replaced
with a different IP.

**Fix:** update `networkPolicy.kubeApiServer.cidrs` to include every IP
returned by `kubectl get endpoints kubernetes -o json`, then
`helm upgrade` again.

### Webhook TLS failures during admission

**Symptom:** `kubectl apply` on a `Keystone` CR fails with a message
containing `failed calling webhook` and `connection refused` or
`no route to host`, and the API-server audit log shows the webhook call
timed out.

**Diagnosis:** webhook ingress is blocked. Usually the API server calls
webhooks from an IP that is **not** present in
`endpoints/kubernetes` — for example, because it sits behind a front-end
proxy. `networkPolicy.webhookClients.cidrs` falls back to
`kubeApiServer.cidrs` when empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check API-server audit logs or the
`kube-apiserver` Pod's own `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly.

### DNS resolution fails (no Service reachable by name)

**Symptom:** operator logs show `dial tcp: lookup foo.bar.svc.cluster.local:
no such host` or `i/o timeout` on any client talking to a cluster Service
by name.

**Diagnosis:** the DNS egress peer selectors do not match your cluster's
DNS Pods. Check the DNS Pods:

```bash
kubectl -n kube-system get pods -l k8s-app=kube-dns
```

If the label set does not match the defaults
(`namespaceSelector: kubernetes.io/metadata.name=kube-system`,
`podSelector: k8s-app=kube-dns`), update
`networkPolicy.dns.namespaceSelector` / `networkPolicy.dns.podSelector` to
match, then `helm upgrade`.

### Helm render aborts with "must not be empty"

**Symptom:** `helm upgrade` fails with

```
Error: execution error at (keystone-operator/templates/networkpolicy.yaml):
networkPolicy.kubeApiServer.cidrs must not be empty ...
```

**Diagnosis:** you set `networkPolicy.enabled=true` but left `cidrs` or
`ports` empty. This is the fail-closed guard (CC-0090, REQ-006) — the
template refuses to render a policy that would break the operator.

**Fix:** populate both lists from step 1 and re-run the upgrade.

### Metrics scraping suddenly returns `connection refused`

**Symptom:** Prometheus (or another scraper) logs `connection refused` on
the metrics port after the upgrade.

**Diagnosis:** the metrics port is default-denied. The previous behaviour
(no NetworkPolicy) allowed any peer; the new policy requires explicit
opt-in via `networkPolicy.allowMetricsFrom`.

**Fix:** add the scraper namespace/pod selectors to
`networkPolicy.allowMetricsFrom` as shown in step 2, then `helm upgrade`.

---

## Rolling back

If enablement causes production regressions that cannot be resolved
immediately, roll back by setting `networkPolicy.enabled=false` and
re-running `helm upgrade`. The NetworkPolicy object is removed on the
next reconcile and the operator reverts to unrestricted pod networking
without a pod restart.

---

## See also

- [Keystone Operator NetworkPolicy](../reference/keystone/keystone-operator-networkpolicy.md) —
  authoritative rule table and schema contract (CC-0090).
- [Keystone Reconciler Architecture](../reference/keystone/keystone-reconciler.md) —
  the CR-scoped `reconcileNetworkPolicy` sub-reconciler (CC-0039) for the
  Keystone API pod NetworkPolicy (different scope from this guide).
- [Kubernetes NetworkPolicy concepts](https://kubernetes.io/docs/concepts/services-networking/network-policies/).
