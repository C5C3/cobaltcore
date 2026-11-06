---
title: Enable the Glance Operator NetworkPolicy
quadrant: operator
---

<!-- operator namespace is `glance-system`; workload (Glance CR) stays in `openstack`. -->

# How-to: Enable the Glance Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the glance-operator pod's egress and ingress
to the minimum required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects the
> Glance API pods (`spec.networkPolicy` on a Glance CR), see the
> [Glance CRD API Reference](../../reference/glance/glance-crd.md). That per-CR
> policy is also the containment layer for `web-download` image imports: its
> auto-derived egress covers DNS, the database, the cache, and the S3 backends
> only, so an import needs an explicit `spec.networkPolicy.additionalEgress`
> rule naming the mirrors; nothing else stays reachable. Enable it whenever
> `spec.importFiltering` widens the operator's HTTPS-on-443 default, because the
> remaining `disallowedHosts` filter matches host strings literally and cannot
> cover every spelling of an address.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the glance-operator
is running (namespace `glance-system`) alongside the projected
`controlplane-glance` image service.
:::

A running `glance-operator` Helm release (namespace `glance-system`) is required.

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which hold the real API server addresses. Do not
use the `10.96.0.1` Service VIP that `kubectl get service kubernetes` reports:
kindnet (kind v0.32 and later) and enforcing CNIs such as Calico and Cilium
evaluate policy after the Service DNAT, so a rule naming the VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints. A kind devstack reports the
one control-plane node's address on the kind bridge, for example
`172.18.0.2/32`, but an HA control plane reports one per API server replica:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
```

The rule must cover all of them, or the operator loses its leader-election
lease whenever it happens to be talking to an excluded replica.

On Cilium, a CIDR selector, and so an `ipBlock` peer, matches no cluster node
address by default. When the API server runs on a cluster node, as on a
kubeadm control plane, Cilium drops the operator's API server traffic even
with every endpoint address in the rule. Before you enable the policy, add a
CiliumNetworkPolicy in the operator's namespace that selects the operator pod
and allows egress `toEntities: [kube-apiserver]`. Starting the Cilium agents
with `--policy-cidr-match-mode=nodes` (Helm value `policyCIDRMatchMode: nodes`)
also works, but it is cluster-wide: every CIDR selector in every
NetworkPolicy, CiliumNetworkPolicy, and CiliumClusterwideNetworkPolicy, in
both directions (`ipBlock`, `fromCIDR`, `fromCIDRSet`, `toCIDR`, `toCIDRSet`,
and CIDR deny rules), then matches node addresses too. Audit those rules
before you turn it on.

On the tutorial devstacks the `glance-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next
reconcile. Substitute the CIDRs and ports from above. The single-CIDR list
below is the kind case, one control-plane node, so replace `172.18.0.2/32`
with the address the discovery command printed and extend the list to every
IP an HA control plane prints:

```bash
kubectl patch helmrelease glance-operator -n glance-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/glance-operator -n glance-system \
  --for=condition=Ready --timeout=5m
```

The chart-level policy allows what the operator needs: egress to the
kube-apiserver, to DNS and to the Glance API, ingress to the webhook and metrics
ports. The Glance API rule admits TCP 9292 to every pod labelled
`app.kubernetes.io/name: glance` and
`app.kubernetes.io/managed-by: glance-operator`, in any namespace, and needs no
value. The operator's health check GETs the API over the Glance Service, and the
CNI judges that connection after the Service DNAT, at the API pod and its port,
so the rule selects the API pods by the labels the operator sets on them. When
the workload namespace itself runs a per-CR NetworkPolicy, the operator's
namespace is admitted automatically by the sub-reconciler (an operator-namespace
ingress peer is appended to every rendered policy).

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade`. The fail-closed guard requires
`kubeApiServer.cidrs` and `ports`:

```bash
helm upgrade glance-operator oci://ghcr.io/c5c3/charts/glance-operator \
  --namespace glance-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
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
kubectl -n glance-system rollout restart deploy/glance-operator
kubectl -n glance-system rollout status deploy/glance-operator --timeout=5m
```

If the rollout times out, inspect the operator logs and the API-server rules
using the troubleshooting steps below.

```bash
kubectl -n glance-system get networkpolicy
kubectl -n glance-system describe networkpolicy glance-operator
```

Then confirm reconciliation still works end-to-end by driving a change
through the `ControlPlane` CR and watching the projected image service roll out:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"glance":{"api":{"replicas":2}}}}}'
kubectl rollout status deploy/controlplane-glance -n openstack

# revert
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"sizing":{"glance":{"api":{"replicas":1}}}}}'
```

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-glance` child: the c5c3-operator re-asserts the child's
`spec.deployment.replicas` on every reconcile, so a direct edit of the child is
reverted.

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`glance-operator` pod restarts.

**Diagnosis:** the egress allow-list does not match the API server the operator
actually dials. Either `kubeApiServer.cidrs` is missing one or more of the
current endpoint IPs (an HA control plane may have added a replica, or a
control-plane node may have been replaced with a different IP), or your CNI maps
the API server behind a port that is not in `kubeApiServer.ports`. A rule naming
the `10.96.0.1` Service VIP instead of an endpoint address fails the same way,
because the CNI evaluates the connection after the Service DNAT. On Cilium
without `--policy-cidr-match-mode=nodes`, no `ipBlock` matches an API server
that runs on a cluster node; see the Cilium paragraph in Step 1.

**Fix:** re-run the discovery command from Step 1 and update
`networkPolicy.kubeApiServer.cidrs` to include **every** IP it returns, plus
every port:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

### Every Glance and GlanceBackend write is rejected

**Symptom:** `kubectl apply` on a `Glance` or `GlanceBackend` CR fails with
`failed calling webhook` plus `connection refused` or `no route to host`. The
`ControlPlane` stalls too: the c5c3-operator projects `controlplane-glance` and
its backend children through the same admission path, so its reconcile fails on
the same error.

**Diagnosis:** webhook ingress (9443) is blocked. Both glance webhooks register
`failurePolicy=Fail` on create and update, so an unreachable operator turns into
a cluster-wide rejection of every write to either kind. The usual cause is an API
server that calls webhooks from an IP that is **not** in `endpoints/kubernetes`
(for example, because it sits behind a front-end proxy).
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease glance-operator -n glance-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and
the operator reverts to unrestricted pod networking without a pod restart.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The glance chart's helm-unittest
(`operators/glance/helm/glance-operator/tests/networkpolicy_test.yaml`) pins the
egress rule the chart adds for its API health check. These tests cover rendering
only; they do not exercise packet enforcement.

The `glance-operator-network-policy-egress` Chainsaw suite adds runtime coverage
for the Glance operator chart. CI runs it in the glance `e2e-operator` job on
kind v0.33.0 with the default kindnet CNI. The suite installs a second release,
`glance-operator-netpol`, with `networkPolicy.enabled=true` and the API server
addresses of EndpointSlice `default/kubernetes`. It checks that the policy
selects the pod of that release and leaves the primary `glance-operator` pod
alone, and that this policy is the only NetworkPolicy in `glance-system`. The
operator pod must start without a restart and log that it detected the Gateway
API.

kindnet adds a new pod to its policy set a moment after the pod starts, so these
startup checks show only that the operator started with the policy present. The
proof comes from a probe pod the policy selects, on fresh connections:

1. It is dropped on a sink port that a control pod outside the policy reaches.
   The suite waits for this drop and fails with `was never dropped` on a cluster
   that does not enforce NetworkPolicy.
2. It resolves a cluster name and reaches the API server endpoint and the
   `kubernetes` Service VIP.
3. It reaches a stand-in for the Glance API on TCP 9292. It is dropped on the
   stand-in's TCP 9999 and on the sink's TCP 9292, so the rule admits the port
   only to its peer.

A stand-in replaces the Glance API pods: a bare pod that carries the two labels
of the chart's rule (`app.kubernetes.io/name: glance` and
`app.kubernetes.io/managed-by: glance-operator`) and listens on TCP 9292. The
suite applies no Glance CR.
`operators/glance/internal/controller/chart_networkpolicy_test.go` checks that
the rule admits the labels the operator sets on the real API pods and the port
their Service targets.

Run the following from the repository root against a disposable devstack with
infrastructure and the primary `glance-operator` release in `glance-system`.
Install the tools pinned by `hack/install-test-deps.sh`; the script steps need
Git, Helm, kubectl and jq as well as Chainsaw, and image loading needs kind and
Docker. Building the image with `make image` requires
[Nix with flakes](../../reference/ci-cd/nix-image-build.md). The release runs
`ghcr.io/c5c3/glance-operator:dev` with `image.pullPolicy=Never`, so load that
image into the kind cluster first:

```bash
make image OPERATOR=glance IMG=ghcr.io/c5c3/glance-operator:dev
kind load docker-image ghcr.io/c5c3/glance-operator:dev --name cobaltcore
helm dependency build --skip-refresh operators/glance/helm/glance-operator
chainsaw test --test-dir tests/e2e/glance-operator/network-policy-egress
```

Replace `cobaltcore` if your cluster has another name. The probe pods run
`ghcr.io/c5c3/glance:2026.1`, which the kind node pulls when the cluster does
not hold it yet. Chainsaw runs scripts from the test directory, and the install
step resolves the chart and values paths from `GITHUB_WORKSPACE` in CI and from
`git rev-parse --show-toplevel` locally. No `GITHUB_WORKSPACE` export is needed
in a local Git checkout.

The per-CR suite `tests/e2e/glance/network-policy` covers the Glance workload
policy, a different scope from this guide.
