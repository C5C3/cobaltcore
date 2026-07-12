---
title: Enable the Keystone Operator NetworkPolicy
quadrant: operator
---

<!-- operator namespace is `keystone-system`; workload (Keystone CR) stays in `openstack`. -->

# How-to: Enable the Keystone Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the keystone-operator pod's egress and ingress
to the minimum required for correct reconciliation. For the
authoritative rule table, failure modes, and schema contract, see
[Keystone Operator NetworkPolicy](../reference/keystone/keystone-operator-networkpolicy.md).

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects
> Keystone API pods, see the
> [reconcileNetworkPolicy sub-reconciler](../reference/keystone/keystone-reconciler.md#sub-reconciler-contracts).

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the keystone-operator
is running and a `keystone` CR is `Ready` in `openstack`. (The devstack's default
`kindnet` CNI does not enforce NetworkPolicy — item 1 below explains what you can
and cannot verify on kind.)
:::

1. **A CNI that enforces `networking.k8s.io/v1` NetworkPolicy — for real
   enforcement.** Confirm with your platform team. Common CNIs that enforce:

   - [Calico](https://docs.tigera.io/calico/latest/network-policy/policy-rules/kubernetes)
   - [Cilium](https://docs.cilium.io/en/stable/network/kubernetes/policy/)
   - [Antrea](https://antrea.io/docs/main/docs/network-policy/)

   ::: warning Enforcement cannot be verified on the default devstack CNI
   The Quick Start kind devstack uses the default `kindnet` CNI, which
   **silently ignores** NetworkPolicy objects. kind fixes the CNI at cluster
   creation, so it cannot be swapped in afterwards on a running devstack. The
   policy object is still rendered and the operator keeps reconciling
   normally, so this guide's verification steps (§4.1–4.3) confirm only that
   the policy has the right **shape** and that enabling it does **not break**
   the operator — they do **not** prove that packets outside the allow-list
   are dropped. Real enforcement requires a cluster whose CNI enforces
   NetworkPolicy (Calico, Cilium, Antrea) — typically your production
   platform, not the kind devstack.
   :::

2. **Permission to read the `kubernetes` default Service's endpoints** in
   order to discover the API server CIDR:

   ```bash
   kubectl auth can-i get endpoints/kubernetes -n default
   ```

3. **Operator deployment whose chart values you can set.** This feature is
   gated by chart values. On the tutorial devstacks the operator is a Flux
   `HelmRelease`, so you set the values on its `spec.values` (section 3); a
   directly Helm-managed install uses `helm upgrade`. If you installed the
   operator via `hack/ci-deploy-operator.sh` or a raw manifest, first migrate
   to one of those chart-value paths.

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

On the tutorial devstacks the `keystone-operator` release is owned by Flux (a
`HelmRelease`), so apply the values change by patching that HelmRelease's
`spec.values` — not with a raw `helm upgrade`, which the Flux helm-controller
reverts on its next reconcile. Substitute the CIDRs and ports you gathered in
step 1:

```bash
kubectl patch helmrelease keystone-operator -n keystone-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["10.96.0.1/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/keystone-operator -n keystone-system \
  --for=condition=Ready --timeout=5m
```

The optional overrides from section 2 (`dns`, `allowMetricsFrom`,
`webhookClients`) go into the same `spec.values.networkPolicy` patch — merge
them into the JSON above.

Flux reconciles the HelmRelease, which creates the `NetworkPolicy` object and
rolls the operator Deployment. Existing reconciliations queue during the
rollout and resume once the new pod reaches `Ready`.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), apply the
`values.yaml` from section 2 with a rolling `helm upgrade` instead:

```bash
helm upgrade keystone-operator oci://ghcr.io/c5c3/charts/keystone-operator \
  --namespace keystone-system \
  --version <chart-version> \
  -f values.yaml
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

---

## 4. Verify

### 4.1 NetworkPolicy exists and matches the pod

```bash
kubectl -n keystone-system describe networkpolicy \
  $(kubectl -n keystone-system get networkpolicy -o name \
    | grep keystone-operator)
```

Expected: one NetworkPolicy whose `PodSelector` matches the operator pod's
labels, `PolicyTypes: Ingress, Egress`, and egress rules for DNS and the
API server.

Confirm the selector actually matches exactly one pod:

```bash
kubectl -n keystone-system get pods \
  -l app.kubernetes.io/name=keystone-operator,app.kubernetes.io/instance=keystone-operator
```

A zero-pod match is a symptom of mismatched labels — usually a stale
`fullnameOverride` or a custom `nameOverride`. The NetworkPolicy will exist
but protect no pod.

### 4.2 Operator still reconciles

Create (or update) a `Keystone` CR and watch its `Ready` condition progress
to `True`:

```bash
kubectl -n openstack get keystone keystone \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
```

If `Ready` stays `False` for more than a couple of reconcile cycles, jump to
**Troubleshooting** below.

### 4.3 Controller-runtime is talking to the API server

Tail operator logs and confirm there are no `i/o timeout`,
`connection refused`, or `context deadline exceeded` errors when the
controller-runtime client dials the API:

```bash
kubectl -n keystone-system logs deploy/keystone-operator -f \
  | grep -Ei 'apiserver|leader|timeout|refused|deadline'
```

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
`ports` empty. This is the fail-closed guard — the
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
immediately, roll back by setting `networkPolicy.enabled=false`. On the Flux
devstacks patch the HelmRelease:

```bash
kubectl patch helmrelease keystone-operator -n keystone-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":false}}}}'
```

(On a directly Helm-managed install, re-run `helm upgrade` with
`networkPolicy.enabled=false` instead.) The NetworkPolicy object is removed on
the next reconcile and the operator reverts to unrestricted pod networking
without a pod restart.

---

## See also

- [Keystone Operator NetworkPolicy](../reference/keystone/keystone-operator-networkpolicy.md) —
  authoritative rule table and schema contract.
- [Keystone Reconciler Architecture](../reference/keystone/keystone-reconciler.md) —
  the CR-scoped `reconcileNetworkPolicy` sub-reconciler for the
  Keystone API pod NetworkPolicy (different scope from this guide).
- [Kubernetes NetworkPolicy concepts](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

## Tested by

The operator installed with `networkPolicy.enabled=true` and one Keystone CR
reaching `Ready=True` is exercised on the CI e2e kind cluster by this chainsaw
suite:

```bash
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

Like the verification steps above, this suite runs on the default `kindnet` CI
cluster, so it validates that the chart **renders** the policy and that the
operator **reconciles to Ready** while the (unenforced) policy is present — it
does **not** assert that blocked egress is actually dropped. Enforcement
coverage would require a CI job with a NetworkPolicy-enforcing CNI.
