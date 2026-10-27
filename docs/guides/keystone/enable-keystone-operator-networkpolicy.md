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
[Keystone Operator NetworkPolicy](../../reference/keystone/keystone-operator-networkpolicy.md).

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects
> Keystone API pods, see the
> [reconcileNetworkPolicy sub-reconciler](../../reference/keystone/keystone-reconciler.md#sub-reconciler-contracts).

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the keystone-operator
is running and a `keystone` CR is `Ready` in `openstack`.
:::

1. **Permission to read the `kubernetes` default Service's endpoints** in
   order to discover the API server CIDR:

   ```bash
   kubectl auth can-i get endpoints/kubernetes -n default
   ```

2. **Operator deployment whose chart values you can set.** This feature is
   gated by chart values. On the tutorial devstacks the operator is a Flux
   `HelmRelease`, so you set the values on its `spec.values` (section 2); a
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

Record every endpoint IP. The rule must cover all of them, or the operator
will lose quorum on leader-election renewals whenever it happens to be
talking to an excluded replica.

---

## 2. Update and apply chart values

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

On the tutorial devstacks the `keystone-operator` release is owned by Flux (a
`HelmRelease`). Apply these values by patching its `spec.values`:

```bash
kubectl patch helmrelease keystone-operator -n keystone-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["10.96.0.1/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/keystone-operator -n keystone-system \
  --for=condition=Ready --timeout=5m
```

Replace the example CIDR and port in both representations with every endpoint
from step 1. Flux's helm-controller reverts out-of-band `helm upgrade` changes,
so keep devstack values on the HelmRelease.

Flux reconciles the HelmRelease and creates the NetworkPolicy. Enabling the
policy alone does not restart the operator. Restart it in step 3 to check new
connections under the policy.

With the policy on, the chart also renders the egress rule the operator needs
for the Keystone API: TCP 5000 and 5050 to every pod labelled
`app.kubernetes.io/name: keystone` and
`app.kubernetes.io/managed-by: keystone-operator`, in any namespace. It needs no
value. The operator's health check and its `KeystoneIdentityBackend` controller
reach the API over the Keystone Service on 5000, and the CNI judges each
connection after the Service DNAT, at the API pod and its port: 5000, or 5050
when federation puts the OIDC sidecar in front of the API.

### Optional overrides

Merge any of these overrides into the same `spec.values.networkPolicy` patch.

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
  Override only if you operate a non-standard webhook architecture, for
  example, an API server front-end that calls webhooks from a different
  CIDR than it advertises in `endpoints/kubernetes`:

  ```yaml
  networkPolicy:
    webhookClients:
      cidrs: ["10.1.0.0/24"]
  ```

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

## 3. Verify

Restart the operator so its API-server connections open under the policy.
Existing connections can remain open after a policy change and hide a missing
API-server rule:

```bash
kubectl -n keystone-system rollout restart deploy/keystone-operator
kubectl -n keystone-system rollout status deploy/keystone-operator --timeout=5m
```

If the rollout times out, inspect the operator logs and the API-server rules
using the troubleshooting steps below.

### 3.1 NetworkPolicy exists and matches the pod

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

A zero-pod match is a symptom of mismatched labels, usually a stale
`fullnameOverride` or a custom `nameOverride`. The NetworkPolicy will exist
but protect no pod.

### 3.2 Operator still reconciles

Create (or update) a `Keystone` CR and watch its `Ready` condition progress
to `True`:

```bash
kubectl -n openstack get keystone keystone \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")]}{"\n"}'
```

If `Ready` stays `False` for more than a couple of reconcile cycles, jump to
**Troubleshooting** below.

### 3.3 Controller-runtime is talking to the API server

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
current API server endpoint IPs. Re-run step 1: an HA control plane may
have added a new replica, or a control-plane node may have been replaced
with a different IP.

**Fix:** update `networkPolicy.kubeApiServer.cidrs` to include every IP
returned by `kubectl get endpoints kubernetes -n default -o json`, then
reapply the values using step 2.

### Webhook TLS failures during admission

**Symptom:** `kubectl apply` on a `Keystone` CR fails with a message
containing `failed calling webhook` and `connection refused` or
`no route to host`, and the API-server audit log shows the webhook call
timed out.

**Diagnosis:** webhook ingress is blocked. Usually the API server calls
webhooks from an IP that is **not** present in
`endpoints/kubernetes`, for example, because it sits behind a front-end
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
match, then reapply the values using step 2.

### Helm render aborts with "must not be empty"

**Symptom:** `helm upgrade` fails with

```
Error: execution error at (keystone-operator/templates/networkpolicy.yaml):
networkPolicy.kubeApiServer.cidrs must not be empty ...
```

**Diagnosis:** you set `networkPolicy.enabled=true` but left `cidrs` or
`ports` empty. This is the fail-closed guard: the
template refuses to render a policy that would break the operator.

**Fix:** populate both lists from step 1 and re-run the upgrade.

### Metrics scraping suddenly returns `connection refused`

**Symptom:** Prometheus (or another scraper) logs `connection refused` on
the metrics port after the upgrade.

**Diagnosis:** the metrics port is default-denied. The previous behaviour
(no NetworkPolicy) allowed any peer; the new policy requires explicit
opt-in via `networkPolicy.allowMetricsFrom`.

**Fix:** add the scraper namespace/pod selectors to
`networkPolicy.allowMetricsFrom` as shown in step 2, then reapply those values.

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

- [Keystone Operator NetworkPolicy](../../reference/keystone/keystone-operator-networkpolicy.md) —
  authoritative rule table and schema contract.
- [Keystone Reconciler Architecture](../../reference/keystone/keystone-reconciler.md) —
  the CR-scoped `reconcileNetworkPolicy` sub-reconciler for the
  Keystone API pod NetworkPolicy (different scope from this guide).
- [Kubernetes NetworkPolicy concepts](https://kubernetes.io/docs/concepts/services-networking/network-policies/).

## Tested by

Helm unit tests cover rendering only: the shared rules are tested in
`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`,
and Keystone-specific egress is tested in
`operators/keystone/helm/keystone-operator/tests/networkpolicy_test.yaml`.

The `keystone-operator-network-policy-egress` Chainsaw suite adds runtime
coverage for the Keystone operator chart. CI includes it in the Keystone
`e2e-operator` job on kind v0.33.0 with the default kindnet CNI.

Run the following from the repository root against a disposable devstack with
infrastructure and the primary Keystone operator already running. Install the
tools pinned by `hack/install-test-deps.sh`; the script step needs Git, Helm,
kubectl, and jq as well as Chainsaw. Image loading also needs kind and Docker.
Building the image with `make image` requires
[Nix with flakes](../../reference/ci-cd/nix-image-build.md).
The fixture uses
`ghcr.io/c5c3/keystone-operator:dev` with `image.pullPolicy=Never`, so load that
image into the target kind cluster first:

```bash
make image OPERATOR=keystone IMG=ghcr.io/c5c3/keystone-operator:dev
kind load docker-image ghcr.io/c5c3/keystone-operator:dev --name cobaltcore
helm dependency build --skip-refresh operators/keystone/helm/keystone-operator
chainsaw test --test-dir tests/e2e/keystone-operator/network-policy-egress
```

Replace `cobaltcore` if your cluster has another name. Skip `make image` if the
`:dev` operator image is already built. The
[E2E prerequisites](../../reference/testing/keystone-e2e-tests.md#prerequisites)
list the required backing services and synced Secrets.
The service images required by the fixtures must also be available in the
cluster. Chainsaw runs scripts from the test directory; the install step
resolves chart and values paths from `GITHUB_WORKSPACE` in CI and from
`git rev-parse --show-toplevel` locally. No `GITHUB_WORKSPACE` export is needed
in a local Git checkout. If already set, it must point to this repository root.

The suite checks that the chart renders the policy and that a Keystone CR
reaches `Ready=True` while it is present. The primary operator release competes
for the same leader lease, so a last step runs a probe pod the policy selects:
its connection to the MariaDB port must be dropped, and the Keystone API must
answer it through the Keystone Service on 5000. The step waits for the drop
first and fails when it never comes, so it requires enforcement. This is
Keystone-specific runtime coverage; it does not establish that another
operator's chart works.
