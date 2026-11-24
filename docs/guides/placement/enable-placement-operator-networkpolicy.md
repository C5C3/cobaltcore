---
title: Enable the Placement Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `placement-system`; workload (Placement CR) stays in `openstack`. -->

# How-to: Enable the Placement Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the placement-operator pod's egress and ingress
to the minimum required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the
> **operator pod itself**. For the per-CR NetworkPolicy that protects the
> Placement API pods (`spec.networkPolicy` on a Placement CR), see the
> [Placement CRD API Reference](../../reference/placement/placement-crd.md).
> That per-CR policy restricts ingress to TCP 8778 from the sources you list
> (at least one is required, so an empty list is refused) and derives its
> egress for DNS, the database, and the Keystone endpoint's port. It adds
> cache egress when a cache is configured, then appends `additionalEgress`.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the placement-operator
is running (namespace `placement-system`) alongside the projected
`controlplane-placement` resource-tracking service.
:::

1. A running `placement-operator` Helm release (namespace `placement-system`).
2. The ControlPlane declares `spec.services.placement`. Without it no
   `controlplane-placement` child exists and the Step 2 verification has
   nothing to roll.
3. The cluster's CNI (container network interface) plugin enforces Kubernetes
   NetworkPolicy. Otherwise the policy object does not restrict traffic.
4. The commands require `kubectl` and `jq`; direct Helm installations also
   require `helm`. You need permission to list EndpointSlices in `default`;
   check with `kubectl auth can-i list endpointslices.discovery.k8s.io -n default`.
5. You need permission to patch the HelmRelease and ControlPlane and restart
   the operator Deployment. The fresh-connection check in Step 2 requires
   `patch` on `pods/ephemeralcontainers`; run it as a cluster admin, as
   described in that step.

## Step 1: Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
EndpointSlices for the `kubernetes` Service in `default`. Kubernetes leaves
the order of Service DNAT (rewriting a Service IP to a pod IP) and
NetworkPolicy evaluation to the CNI. The Placement e2e suite confirms that
kindnet on kind v0.33 evaluates this rule after DNAT, so use the EndpointSlice
addresses on the tutorial devstack. Verify your CNI's behavior before applying
the same assumption to another cluster; see the
[Kubernetes NetworkPolicy behavior](https://kubernetes.io/docs/concepts/services-networking/network-policies/#behavior-of-to-and-from-selectors).

```bash
kubectl get endpointslices -n default \
  -l kubernetes.io/service-name=kubernetes -o json \
  | jq -r '
      [
        .items[] as $slice
        | $slice.endpoints[]?
        | select(.conditions.ready != false)
        | .addresses[]? as $ip
        | $slice.ports[]?.port as $p
        | select($p != null)
        | "\($ip)\(if ($ip | contains(":")) then "/128" else "/32" end) port=\($p)"
      ] | unique[]'
```

Record every unique CIDR and port pair. IPv4 addresses use `/32`; IPv6
addresses use `/128`. A kind devstack reports the one control-plane node's
address on the kind bridge (for example, `172.18.0.2/32 port=6443`). An HA
control plane reports one address per API server replica, and a dual-stack
cluster can report both address families:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
fd00::10/128 port=6443
```

If the command prints no addresses or ports, check the EndpointSlices before
continuing.

The chart combines the CIDRs and ports in one rule, so each listed port is
allowed to each listed address. The rule must cover all API server endpoints,
or the operator can lose its leader-election lease when it talks to an
excluded replica.

On Cilium, CIDR selectors do not match cluster node addresses by default
([Cilium CIDR matching](https://docs.cilium.io/en/stable/security/policy/layer3/#cidr-select-nodes)).
If the API server runs on a node, the chart's API-server egress CIDR may not
match even when it lists every EndpointSlice address. Before enabling the
policy, either set `--policy-cidr-match-mode=nodes` (the Cilium Helm value is
`policyCIDRMatchMode: nodes`) or add a CiliumNetworkPolicy that selects the
operator pod and permits egress to the `kube-apiserver` entity on the API
server ports
([Cilium's kube-apiserver entity](https://docs.cilium.io/en/stable/security/policy/layer3/#access-to-from-kube-apiserver)).
The chart's webhook ingress rule also uses an `ipBlock`; if webhook requests
arrive from a node address, allow ingress through Cilium policy on TCP 9443.
Cilium notes that the `kube-apiserver` entity may not work for ingress on some
managed distributions, so verify the caller path there. The cluster-level
option changes CIDR matching cluster-wide and causes Cilium to allocate node
identities; review affected CIDR policies before enabling it.

On the tutorial devstacks the `placement-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next reconcile. Check
whether the HelmRelease is suspended:

```bash
kubectl get helmrelease placement-operator -n placement-system \
  -o jsonpath='{.spec.suspend}{"\n"}'
```

If the result is `true`, resume the release through its deployment owner before
continuing. If Prometheus already scrapes the operator, add a metrics peer
before enabling the default-deny policy:

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Once the patch succeeds, check the release:

```bash
kubectl get helmrelease placement-operator -n placement-system -o yaml
```

Check that `status.observedGeneration` matches `metadata.generation` and the
`Ready` condition is `True`. Repeat the command if Flux has not reconciled the
patch yet.

Enable the policy after the metrics peer is in place, if needed. Substitute the
CIDRs and ports from above. The single-CIDR list below stands in for the kind
case (one control-plane node), so replace `172.18.0.2/32` with the address you
recorded and extend the list to every IP an HA control plane prints:

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'
```

Once the patch succeeds, check that Flux has reconciled the current generation
and the release is `Ready`, as above.

The shared chart policy allows egress to the kube-apiserver and DNS. It omits
ingress TCP 8081 for health probes: standard CNIs do not apply
NetworkPolicy to kubelet probes from a node's host network
([Kubernetes NetworkPolicy limits](https://kubernetes.io/docs/concepts/services-networking/network-policies/#what-you-cant-do-with-network-policies-at-least-not-yet)).
The Placement chart adds egress to TCP 8778 for pods labelled
`app.kubernetes.io/name: placement` and
`app.kubernetes.io/managed-by: placement-operator`, in any namespace. The
operator's health check calls the Placement Service; on the tested kindnet path,
policy evaluation after Service DNAT matches the API pods by these labels.
When the workload namespace has a per-CR NetworkPolicy, its sub-reconciler
appends an ingress peer for the operator namespace to every rendered policy.

Webhook ingress is rendered only when the webhook is enabled; its allowed CIDRs
come from `networkPolicy.webhookClients.cidrs`, falling back to
`networkPolicy.kubeApiServer.cidrs`. Metrics ingress is opt-in:
`networkPolicy.allowMetricsFrom` is empty by default, so the policy blocks
Prometheus from reaching the metrics port unless you add a peer. Each peer is
rendered as a `NetworkPolicyPeer`; a `podSelector` can narrow access within the
selected namespace. See [Enable the Placement Operator Metrics
Endpoint](./enable-placement-operator-metrics.md) for the ServiceMonitor setup.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade`. The fail-closed guard still requires
`kubeApiServer.cidrs` and `ports`:

```bash
CHART_VERSION="$(helm list -n placement-system --filter '^placement-operator$' -o json \
  | jq -r '.[0].chart // "" | sub("^placement-operator-"; "")')"

helm upgrade placement-operator oci://ghcr.io/c5c3/charts/placement-operator \
  --namespace placement-system --reuse-values --version "${CHART_VERSION}" \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

The command uses one IPv4 CIDR and port as an example. Add every API server
address at another CIDR index, and add each distinct API server port to the
ports list. If Prometheus already scrapes the operator, put the peer shown
above under `networkPolicy.allowMetricsFrom` in a values file and pass it to
this upgrade with `--values`.

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2: Verify

On the kind devstack this step checks the policy's shape, that the operator
still reconciles under the enforced policy, and that a fresh connection from
the operator pod reaches the API server. The step sends no traffic outside the
allow-list.

Restart the operator. A connection opened before the policy existed stays open,
so the restart makes the operator open its connections again:

```bash
kubectl -n placement-system rollout restart deploy/placement-operator
kubectl -n placement-system rollout status deploy/placement-operator --timeout=5m
```

`rollout status` waits for the pod's readiness probe. The `/readyz` endpoint
checks process health and webhook startup, not API-server connectivity, so a
successful rollout does not prove the egress rule works. CNI enforcement timing
also varies. Once the network plugin has processed a policy, Kubernetes
requires it to isolate new selected pods before their containers start
([Pod lifecycle](https://kubernetes.io/docs/concepts/services-networking/network-policies/#pod-lifecycle)).
kindnet can add a new pod to its policy set after startup; a connection opened
before enforcement stays open. Use the fresh-connection check below to test the
rule. If the rollout itself times out, inspect the pod events and operator logs.
If the logs show API timeouts, see
[Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

Then check the policy:

```bash
kubectl -n placement-system get networkpolicy
kubectl -n placement-system describe networkpolicy placement-operator
```

Then confirm reconciliation still works end-to-end by scaling the
resource-tracking service through the `ControlPlane` CR. Record the current
ControlPlane override and Deployment count:

```bash
CUR="$(kubectl get controlplane controlplane -n openstack \
  -o jsonpath='{.spec.sizing.placement.api.replicas}')"
RUN="$(kubectl get deploy/controlplane-placement -n openstack \
  -o jsonpath='{.spec.replicas}')"
echo "ControlPlane override: ${CUR:-unset}; Deployment replicas: ${RUN}"
```

If either read fails, or the Deployment count is empty or non-numeric, stop
before patching. An unset ControlPlane override is expected when sizing comes
from the default or a profile. Keep these values in the same shell for the
restore. Increase the count by one and wait for the Deployment:

```bash
NEW=$((RUN + 1))
kubectl patch controlplane controlplane -n openstack --type merge \
  -p "{\"spec\":{\"sizing\":{\"placement\":{\"api\":{\"replicas\":${NEW}}}}}}"
kubectl wait deploy/controlplane-placement -n openstack --timeout=5m \
  --for=jsonpath='{.spec.replicas}'="${NEW}"
kubectl rollout status deploy/controlplane-placement -n openstack --timeout=5m
```

Restore the original override after the check, even if a wait or rollout
fails. When `CUR` is empty, `null` removes the temporary override so the
default or profile applies again:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p "{\"spec\":{\"sizing\":{\"placement\":{\"api\":{\"replicas\":${CUR:-null}}}}}}"
kubectl wait deploy/controlplane-placement -n openstack --timeout=5m \
  --for=jsonpath='{.spec.replicas}'="${RUN}"
kubectl rollout status deploy/controlplane-placement -n openstack --timeout=5m
```

If restoration fails, inspect the ControlPlane and Deployment and restore the
recorded setting before continuing.

The `kubectl wait` is the check that proves the operator still reconciles: the
patch travels from the ControlPlane to the `Placement` CR to the Deployment,
and the restarted operator writes nothing until it holds the leader lease.
Until the placement operator writes the new count the Deployment is still fully
rolled out at its old size, so a `rollout status` alone would pass at once. The
new count is always one above the running one, so the patch changes the
Deployment whatever size the API already runs at.

Set the replica count on the `ControlPlane` CR. The c5c3-operator re-asserts the
child's `spec.deployment.replicas` on every reconcile, so a direct child edit
does not persist.

Finally, check the `kubeApiServer` rule on fresh connections, once the
restarted pod has run for at least one minute. An ephemeral container shares
the operator pod's network, so the policy judges its connections as it judges
the operator's. Use the Keystone API image, which ships `python3` and which
the devstack's single node already holds:

```bash
POD="$(kubectl -n placement-system get pods \
  -l app.kubernetes.io/name=placement-operator,app.kubernetes.io/instance=placement-operator \
  -o jsonpath='{.items[0].metadata.name}')"
KS="$(kubectl -n openstack get keystones -o jsonpath='{.items[0].metadata.name}')"
IMG="$(kubectl -n openstack get deploy "${KS}" \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="keystone")].image}')"
echo "Operator pod: ${POD}; Keystone image: ${IMG}"
```

If a lookup fails or the pod or image is empty, resolve it before continuing.
Choose an address and port from Step 1, without the CIDR suffix, and start the
check:

```bash
API_ADDRESS=172.18.0.2
API_PORT=6443
NAME="apiserver-check-$(date +%s)"
kubectl -n placement-system debug "pod/${POD}" --image="${IMG}" --profile=restricted \
  --container="${NAME}" -- python3 -u -c \
  'import socket, sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=5).close(); print("connected")' \
  "${API_ADDRESS}" "${API_PORT}"
```

Check the container state and log:

```bash
kubectl -n placement-system get pod "${POD}" \
  -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state}{'\n'}"
kubectl -n placement-system logs "pod/${POD}" -c "${NAME}"
```

The state must be `terminated` with reason `Completed`, and the log must say
`connected`. If the container is still starting or running, repeat these two
commands. If it cannot start, inspect the state's `reason` and `message`. If
the state is empty, check that the node is `Ready` with `kubectl get nodes`.
Repeat the check with each ready address and port from Step 1, using a new
container name each time.

A `TimeoutError` in the log can mean the policy drops connections to that
endpoint because the rule misses its address or port. Other errors, such as
`Connection refused`, can indicate a problem with the endpoint or route.
kindnet drops a denied connection without an answer. The ephemeral
container exits after the check and stays in the pod's spec until the next
rollout replaces the pod. Adding it needs the `patch` verb on
`pods/ephemeralcontainers` in the operator's namespace. That verb lets its
holder mount the operator's ServiceAccount token and act with the operator's
rights, so run the check as a cluster admin and do not grant the verb to a
separate verification role. `--profile=restricted` lets it start in a
namespace that enforces the restricted Pod Security Standard.

When a connection times out, see
[Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`placement-operator` pod restarts.
On the kind devstack the first sign can instead be a `TimeoutError` from
the fresh-connection check of the Verify step.

**Diagnosis:** the egress allow-list does not match the API server the operator
actually dials. Either `kubeApiServer.cidrs` is missing one or more of the
current endpoint IPs (an HA control plane may have added a replica, or a
control-plane node may have been replaced with a different IP), or your CNI maps
the API server behind a port that is not in `kubeApiServer.ports`.

**Fix:** re-run the EndpointSlice discovery command in
[Step 1](#step-1-enable-the-policy) and update
`networkPolicy.kubeApiServer.cidrs` and `ports` with every ready address and
port it returns.

### Every Placement write is rejected

**Symptom:** `kubectl apply` on a `Placement` CR fails with
`failed calling webhook` plus `connection refused` or `no route to host`. The
`ControlPlane` stalls too: the c5c3-operator projects `controlplane-placement`
through the same admission path, so its reconcile fails on the same error.

**Diagnosis:** webhook ingress (9443) is blocked. Both placement webhooks
register `failurePolicy=Fail` on create and update, so an unreachable operator
turns into a cluster-wide rejection of every write to the kind. The usual cause
is an API server that calls webhooks from an IP that is **not** in the
`kubernetes` EndpointSlices, for example because it sits behind a front-end
proxy.
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and the
operator reverts to unrestricted pod networking without a pod restart.

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The placement chart's helm-unittest
(`operators/placement/helm/placement-operator/tests/networkpolicy_test.yaml`)
pins the egress rule the chart adds for its API health check. These tests check
rendering only; they do not exercise packet enforcement.

The `placement-operator-network-policy-egress` Chainsaw suite adds runtime
coverage for the Placement operator chart. CI runs it in the placement
`e2e-operator` job on kind v0.33.0 with the default kindnet CNI. The suite
installs a second release, `placement-operator-netpol`, with
`networkPolicy.enabled=true` and the API server addresses of EndpointSlice
`default/kubernetes`. It checks that the policy selects the pod of that release
and leaves the primary `placement-operator` pod alone, and that this policy is
the only NetworkPolicy in `placement-system`. The operator pod must start
without a restart and log that it detected the Gateway API.

kindnet adds a new pod to its policy set a moment after the pod starts, so these
startup checks show only that the operator started with the policy present. The
proof comes from a probe pod the policy selects, on fresh connections:

1. It is dropped on a sink port that a control pod outside the policy reaches.
   The suite waits for this drop and fails with `was never dropped` on a cluster
   that does not enforce NetworkPolicy.
2. It resolves a cluster name and reaches the API server endpoint and the
   `kubernetes` Service VIP.
3. It reaches a stand-in for the Placement API on TCP 8778. It is dropped on the
   stand-in's TCP 9999 and on the sink's TCP 8778, so the rule admits the port
   only to its peer.

A stand-in replaces the Placement API pods: a bare pod that carries the two
labels of the chart's rule (`app.kubernetes.io/name: placement` and
`app.kubernetes.io/managed-by: placement-operator`) and listens on TCP 8778. The
suite applies no Placement CR.
`operators/placement/internal/controller/chart_networkpolicy_test.go` checks
that the rule admits the labels the operator sets on the real API pods and the
port their Service targets.

Run the following from the repository root against a disposable devstack with
infrastructure and the primary `placement-operator` release in
`placement-system`. Install the tools pinned by `hack/install-test-deps.sh`; the
script steps need Git, Helm, kubectl and jq as well as Chainsaw, and image
loading needs kind and Docker. Building the image with `make image` requires
[Nix with flakes](../../reference/ci-cd/nix-image-build.md). The release runs
`ghcr.io/c5c3/placement-operator:dev` with `image.pullPolicy=Never`, so load
that image into the kind cluster first:

```bash
make image OPERATOR=placement IMG=ghcr.io/c5c3/placement-operator:dev
kind load docker-image ghcr.io/c5c3/placement-operator:dev --name cobaltcore
helm dependency build --skip-refresh operators/placement/helm/placement-operator
chainsaw test --test-dir tests/e2e/placement-operator/network-policy-egress
```

Replace `cobaltcore` if your cluster has another name. The probe pods run
`ghcr.io/c5c3/placement:2026.1`, which the kind node pulls when the cluster does
not hold it yet. Chainsaw runs scripts from the test directory, and the install
step resolves the chart and values paths from `GITHUB_WORKSPACE` in CI and from
`git rev-parse --show-toplevel` locally. No `GITHUB_WORKSPACE` export is needed
in a local Git checkout.

The per-CR suite `tests/e2e/placement/network-policy` covers the Placement workload
policy, a different scope from this guide.
