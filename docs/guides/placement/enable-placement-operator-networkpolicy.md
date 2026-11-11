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
> egress from the CR: DNS, the database, the Keystone endpoint's port, and the
> cache, with `additionalEgress` appended after it.

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

## Step 1: Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which carry the real API server addresses. The
`10.96.0.1` Service VIP that `kubectl get service kubernetes` reports is the
wrong source. An enforcing CNI
(Calico, Cilium, and kindnet on kind v0.32 and later) evaluates policy after the
Service DNAT has rewritten a packet aimed at the VIP to one of those endpoint
IPs, so a rule naming the VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints. A kind
devstack reports the one control-plane node's address on the kind bridge
(`172.18.0.x`), but an HA control plane reports one per API server replica:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
```

The rule must cover all of them, or the operator loses its leader-election
lease whenever it happens to be talking to an excluded replica.

On the tutorial devstacks the `placement-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next
reconcile. Substitute the CIDRs and ports from above. The single-CIDR list
below stands in for the kind case (one control-plane node), so replace
`172.18.0.2/32` with the address you recorded and extend the list to every IP
an HA control plane prints:

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/placement-operator -n placement-system \
  --for=condition=Ready --timeout=5m
```

The chart-level policy allows what the operator needs: egress to the
kube-apiserver, to DNS and to the Placement API, ingress to the webhook and
metrics ports. The Placement API rule admits TCP 8778 to every pod labelled
`app.kubernetes.io/name: placement` and
`app.kubernetes.io/managed-by: placement-operator`, in any namespace, and needs
no value. The operator's health check GETs the API over the Placement Service,
and the CNI judges that connection after the Service DNAT, at the API pod and
its port, so the rule selects the API pods by the labels the operator sets on
them. When the workload namespace itself runs a per-CR NetworkPolicy, the
operator's namespace is admitted automatically by the sub-reconciler (an
operator-namespace ingress peer is appended to every rendered policy).

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port at all, so a Prometheus that scraped the operator before
stops reaching it. Name the scraping namespace when you enable both this policy
and the ServiceMonitor from
[Enable the Placement Operator Metrics Endpoint](./enable-placement-operator-metrics.md):

```bash
kubectl patch helmrelease placement-operator -n placement-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

::: details Helm-managed installations (non-Flux)
If you installed the operator directly with Helm (not through Flux), set the
values with a rolling `helm upgrade`. The fail-closed guard still requires
`kubeApiServer.cidrs` and `ports`:

```bash
helm upgrade placement-operator oci://ghcr.io/c5c3/charts/placement-operator \
  --namespace placement-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

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

What the restart shows depends on the CNI. A CNI that isolates a new pod before
its containers start, as the
[Kubernetes NetworkPolicy contract](https://kubernetes.io/docs/concepts/services-networking/network-policies/#pod-lifecycle)
requires, judges the new pod's first API server connection: under a wrong
`kubeApiServer` rule the rollout does not finish within the timeout and the new
pod never turns `Ready`. kindnet adds a new pod to its policy set a moment after
the pod starts. The operator opens its API server connection in that moment and
keeps it, so on the kind devstack the rollout finishes and the pod turns `Ready`
under a wrong rule too. On kind, only the fresh-connection check at the end of
this step judges the rule.

When the rollout times out, see
[Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

Then check the policy:

```bash
kubectl -n placement-system get networkpolicy
kubectl -n placement-system describe networkpolicy placement-operator
```

Then confirm reconciliation still works end-to-end by driving a change
through the `ControlPlane` CR and watching the projected resource-tracking
service roll out. The block records the ControlPlane's current count, which is
empty while the ControlPlane leaves it at the default, scales the API one
replica above the size it runs at, and puts the count back:

```bash
if CUR=$(kubectl get controlplane controlplane -n openstack \
     -o jsonpath='{.spec.sizing.placement.api.replicas}') &&
   RUN=$(kubectl get deploy/controlplane-placement -n openstack \
     -o jsonpath='{.spec.replicas}'); then
  NEW=$((RUN + 1))

  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"placement\":{\"api\":{\"replicas\":$NEW}}}}}"
  kubectl wait deploy/controlplane-placement -n openstack --timeout=5m \
    --for=jsonpath='{.spec.replicas}'="$NEW"
  kubectl rollout status deploy/controlplane-placement -n openstack --timeout=5m

  # revert to the recorded count; null drops the override again
  kubectl patch controlplane controlplane -n openstack --type merge \
    -p "{\"spec\":{\"sizing\":{\"placement\":{\"api\":{\"replicas\":${CUR:-null}}}}}}"
else
  echo "could not read the replica counts; nothing was patched" >&2
fi
```

The `if` stops the block before the first patch when either read fails. A
failed read prints the same empty string as a ControlPlane without an override,
so without the guard the revert would drop an override the ControlPlane
carries, and a failed Deployment read would set `NEW` to 1 and shrink the API
instead of growing it.

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

Finally, check the `kubeApiServer` rule on a fresh connection, once the
restarted pod has run for at least one minute. The block adds an ephemeral
container to the operator pod. The container shares the pod's network, so the
policy judges its connections as it judges the operator's. It runs the image
of the Keystone API Deployment, which ships `python3` and which the devstack's
single node already holds, and opens one new TCP connection to every address
and port of the `kubernetes` Endpoints. The block waits up to three minutes
for the check to end, then prints the state of the check container and its
log:

```bash
CHECK='
import socket, sys
for ep in sys.argv[1:]:
    host, port = ep.rsplit(":", 1)
    try:
        socket.create_connection((host, int(port)), timeout=5).close()
        print(ep, "connected")
    except socket.timeout:
        print(ep, "timed out")
    except OSError as exc:
        print(ep, exc)
print("endpoints checked:", len(sys.argv) - 1)
'
if POD=$(kubectl -n placement-system get pods \
     -l app.kubernetes.io/name=placement-operator,app.kubernetes.io/instance=placement-operator \
     -o jsonpath='{.items[0].metadata.name}') &&
   KS=$(kubectl -n openstack get keystones -o jsonpath='{.items[0].metadata.name}') &&
   IMG=$(kubectl -n openstack get deploy "${KS}" \
     -o jsonpath='{.spec.template.spec.containers[?(@.name=="keystone")].image}') &&
   EPS=$(kubectl get endpoints kubernetes -n default -o json \
     | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip):\($p)"') &&
   [ -n "${POD}" ] && [ -n "${IMG}" ] && [ -n "${EPS}" ]; then
  NAME="apiserver-check-$(date +%s)"
  kubectl -n placement-system debug "pod/${POD}" --image="${IMG}" --profile=restricted \
    --container="${NAME}" -- python3 -u -c "${CHECK}" ${EPS} &&
  for _ in $(seq 90); do
    DONE=$(kubectl -n placement-system get pod "${POD}" \
      -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state.terminated.reason}")
    [ -n "${DONE}" ] && break
    sleep 2
  done &&
  kubectl -n placement-system get pod "${POD}" \
    -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state}{'\n'}" &&
  kubectl -n placement-system logs "pod/${POD}" -c "${NAME}"
else
  echo "could not read the operator pod, the Keystone image or the endpoints; nothing was added" >&2
fi
```

The first line must show a `terminated` state with reason `Completed`. The
last line must read `endpoints checked: N`, and every line between them must
end in `connected`, as on the kind devstack:

```text
{"terminated":{"containerID":"containerd://d0af3b80a47d77c57535cd38a77f062d8eeaeb1b0210bf2f3d89ec554b3905bc","exitCode":0,"finishedAt":"2026-11-06T15:47:48Z","reason":"Completed","startedAt":"2026-11-06T15:47:48Z"}}
172.18.0.2:6443 connected
endpoints checked: 1
```

A `running` state, or a `waiting` state with reason `ContainerCreating`,
means the check has not ended yet: run the block's last two `kubectl`
commands again. Any other `waiting` reason, such as `ImagePullBackOff` or
`RunContainerError`, means the node could not pull or start the image, and
the state's `message` names the cause. A `terminated` state with a log that
lacks the last line means the check failed: the state's `reason` and
`exitCode` and the log say why. An empty first line means the node has
not reported the check container yet: check that the node is `Ready`
with `kubectl get nodes`, then run the block's last two `kubectl`
commands again.

A line that ends in `timed out` means the policy drops new connections to that
endpoint: the rule misses its address or its port. Any other message, such as
`[Errno 111] Connection refused`, means something answered or the route is
broken; kindnet drops a denied connection without an answer. The ephemeral
container exits after the check and stays in the pod's spec until the next
rollout replaces the pod. Adding it needs the `patch` verb on
`pods/ephemeralcontainers` in the operator's namespace. That verb lets its
holder mount the operator's ServiceAccount token and act with the operator's
rights, so run the check as a cluster admin and do not grant the verb to a
separate verification role. `--profile=restricted` lets it start in a
namespace that enforces the restricted Pod Security Standard.

When a line ends in `timed out`, see
[Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`placement-operator` pod restarts.
On the kind devstack the first sign can instead be a `timed out` line from
the fresh-connection check of the Verify step.

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

### Every Placement write is rejected

**Symptom:** `kubectl apply` on a `Placement` CR fails with
`failed calling webhook` plus `connection refused` or `no route to host`. The
`ControlPlane` stalls too: the c5c3-operator projects `controlplane-placement`
through the same admission path, so its reconcile fails on the same error.

**Diagnosis:** webhook ingress (9443) is blocked. Both placement webhooks
register `failurePolicy=Fail` on create and update, so an unreachable operator
turns into a cluster-wide rejection of every write to the kind. The usual cause
is an API server that calls webhooks from an IP that is **not** in
`endpoints/kubernetes`, for example because it sits behind a front-end proxy.
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
