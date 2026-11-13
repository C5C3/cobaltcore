---
title: Enable the Cinder Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `cinder-system`; workload (Cinder, CinderBackend, CinderBackupBackend) stays in `openstack`. -->

# How-to: Enable the Cinder Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level NetworkPolicy
that restricts the cinder-operator pod's egress and ingress to the minimum
required for correct reconciliation.

> **Scope.** This guide covers the NetworkPolicy that protects the **operator
> pod itself**. For the per-CR policy that protects the Cinder pods
> (`spec.networkPolicy` on a `Cinder`), see the
> [Cinder CRD API Reference](../../reference/cinder/cinder-crd.md#spec). That
> policy restricts ingress to TCP 8776 from the sources you list (at least one
> is required, so an empty list is refused) and derives egress from the CR: DNS,
> the database, cache, Keystone, Glance and Barbican endpoints, the broker port,
> NFS exports, and the Ceph monitor and OSD ports to the networks configured on
> RBD volume and backup backends, with `additionalEgress` appended.
> One policy covers the API pods, the scheduler, every volume service, the
> backup service and the Job pods, because its selector carries no component
> key.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_NFS=true make deploy-infra
```

Follow that tutorial through the block-storage block of Step 4 and the
**Create a first volume** check in Step 7, so the projected `controlplane-cinder`
child is `Ready` in `openstack` with `nfs1` and `nfsbk` attached. Every resource
name in the examples below is one that devstack produces.
:::

Use a workstation whose `kubectl` context targets the cluster and with `jq`
installed for the endpoint-discovery and connection-check commands; direct
Helm installs also require `helm`. The cluster's container network interface
(CNI) must enforce Kubernetes NetworkPolicy. Confirm that the `cinder-operator`
Helm release runs in `cinder-system` and that the ControlPlane declares
`spec.services.cinder`; otherwise no `controlplane-cinder` child exists for the
Step 2 verification.

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which carry the real API server addresses. The
devstack's kindnet evaluates egress after Service DNAT, so the `kubernetes`
Service VIP (`10.96.0.1` on this devstack) is rewritten to an endpoint IP before
policy evaluation; a rule for the VIP does not match. Kubernetes leaves the
ordering of Service translation and NetworkPolicy evaluation to each CNI; see
the [NetworkPolicy documentation](https://kubernetes.io/docs/concepts/services-networking/network-policies/#behavior-of-to-and-from-selectors).
On other CNIs, check which destination IP their policy evaluates and configure
those CIDRs.

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '
      .subsets[]
      | (.addresses[].ip) as $ip
      | (.ports[].port) as $p
      | ($ip | if contains(":") then "/128" else "/32" end) as $mask
      | "\($ip)\($mask) port=\($p)"
    '
```

Record **every** endpoint IP the command prints. A kind
devstack reports the one control-plane node's address on the kind bridge
(`172.18.0.x`), but an HA control plane reports one per API server replica. The
rule must cover all of them, or the operator loses its leader-election lease
whenever it happens to be talking to an excluded replica.

On the tutorial devstacks the `cinder-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. A raw
`helm upgrade` is reverted by the Flux helm-controller on its next reconcile;
outside Flux, the same values go through `helm upgrade --reuse-values`.
Substitute the CIDRs and ports from above; the single-CIDR list below stands in
for the kind case of one control-plane node:

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/cinder-operator -n cinder-system \
  --for=condition=Ready --timeout=5m
```

::: details Helm-managed installations (non-Flux)
For a direct Helm installation, substitute the CIDRs and ports from the
discovery command. This example uses one IPv4 endpoint:

```bash
helm upgrade cinder-operator oci://ghcr.io/c5c3/charts/cinder-operator \
  --namespace cinder-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443' \
  --wait --timeout 5m
```

Add an indexed `--set` entry for every additional CIDR or port.
:::

The rendered policy allows egress to the kube-apiserver, to DNS and to the
Cinder API, and ingress to the webhook port. The chart adds the Cinder API rule
through the `operator-library.chart.networkPolicyEgress` hook, the hook a chart
uses to append an egress rule of its own: TCP 8776 to every pod labelled
`app.kubernetes.io/name: cinder` and
`app.kubernetes.io/managed-by: cinder-operator`, in any namespace. It needs no
value. The operator's health check GETs the Cinder API over the workload
Service, and the CNI judges that connection after the Service DNAT, at the API
pod and its port, so the rule selects the API pods by the labels the operator
sets on them. One deployment shape does need more: with a `Cinder` placed on a
registered target cluster, the operator talks to that cluster's API server too,
so add its address and port to the same `kubeApiServer` lists. For a local
`Cinder`, the Cinder reconciler adds the operator namespace as an ingress peer
to the per-CR policy, which admits the local health check. A `Cinder` placed on
a target cluster is probed through that cluster's API-server Service proxy.
Verify that cluster's CNI permits the resulting traffic; the local
operator-namespace peer does not represent that path.

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port (8080) at all, so a Prometheus that scraped the operator
before stops reaching it. Name the scraping namespace when you enable both this
policy and the ServiceMonitor from
[Enable the Cinder Operator Metrics Endpoint](./enable-cinder-operator-metrics.md):

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

### Upgrading from the hand-written probe policy

cinder-operator charts before 0.5.0 rendered no rule for the health check, and
earlier versions of this guide had you apply a second policy,
`cinder-operator-api-probe`, beside the chart's. The chart covers that traffic
now, so delete the hand-written policy once the upgraded chart has rendered its
own:

```bash
kubectl delete networkpolicy cinder-operator-api-probe -n cinder-system \
  --ignore-not-found
```

## Step 2 — Verify

On the kind devstack this step checks the policy's shape, that the operator
still reconciles under the enforced policy, and that a fresh connection from
the operator pod reaches the API server. The step sends no traffic outside the
allow-list.

Restart the operator. A connection opened before the policy existed stays open,
so the restart makes the operator open its connections again:

```bash
kubectl -n cinder-system rollout restart deploy/cinder-operator
kubectl -n cinder-system rollout status deploy/cinder-operator --timeout=5m
```

What the restart shows depends on the CNI. After the CNI has processed the
NetworkPolicy, a conformant implementation isolates newly created selected
pods before their containers start, as the
[Kubernetes NetworkPolicy contract](https://kubernetes.io/docs/concepts/services-networking/network-policies/#pod-lifecycle)
requires. Under a wrong `kubeApiServer` rule, the new pod's first API server
connection then fails, so the rollout does not finish within the timeout. A pod
created before the CNI has processed the policy may start unprotected. kindnet
adds a new pod to its policy set a moment after the pod starts. The operator
opens its API server connection in that moment and keeps it, so on the kind
devstack the rollout finishes and the pod turns `Ready` under a wrong rule too.
On kind, only the fresh-connection check at the end of this step judges the rule.

When the rollout times out, see
[Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

Then check the policy:

```bash
kubectl -n cinder-system get networkpolicy
kubectl -n cinder-system describe networkpolicy cinder-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `ControlPlane` CR and watching the projected block-storage API roll out.
The block records the ControlPlane's current count, which is empty while the
ControlPlane leaves it at the default, scales the API one replica above the size
it runs at, and puts the count back:

```bash
(
  set -e
  if CUR=$(kubectl get controlplane controlplane -n openstack \
       -o jsonpath='{.spec.sizing.cinder.api.replicas}') &&
     RUN=$(kubectl get deploy/controlplane-cinder -n openstack \
       -o jsonpath='{.spec.replicas}'); then
    NEW=$((RUN + 1))

    restore_replicas() {
      # null drops the override again when the ControlPlane used its default.
      kubectl patch controlplane controlplane -n openstack --type merge \
        -p "{\"spec\":{\"sizing\":{\"cinder\":{\"api\":{\"replicas\":${CUR:-null}}}}}}"
    }
    trap restore_replicas EXIT

    kubectl patch controlplane controlplane -n openstack --type merge \
      -p "{\"spec\":{\"sizing\":{\"cinder\":{\"api\":{\"replicas\":$NEW}}}}}"
    kubectl wait deploy/controlplane-cinder -n openstack --timeout=5m \
      --for=jsonpath='{.spec.replicas}'="$NEW"
    kubectl rollout status deploy/controlplane-cinder -n openstack --timeout=5m

    restore_replicas
    trap - EXIT
  else
    echo "could not read the replica counts; nothing was patched" >&2
    exit 1
  fi
)
```

The two reads finish before the first patch. If either fails, the block prints
an error, exits nonzero and leaves the ControlPlane unchanged. If the patch,
wait or rollout fails after scaling, the exit trap restores the recorded count.
An unset replica override reads as an empty string, so the restore uses `null`
to remove that override again.

The `kubectl wait` is the check that proves the operator still reconciles: the
patch travels from the ControlPlane to the `Cinder` CR to the Deployment, and
the restarted operator writes nothing until it holds the leader lease. Until
the cinder operator writes the new count the Deployment is still fully rolled
out at its old size, so a `rollout status` alone would pass at once. The new
count is always one above the running one, so the patch changes the Deployment
whatever size the API already runs at.

`spec.sizing.cinder.api.replicas` sizes the API Deployment alone. The scheduler
takes a count of its own under `spec.sizing.cinder.scheduler`, and the volume
services and the backup service are pinned to one replica by the projection, so
this patch rolls `controlplane-cinder` and leaves the storage path untouched.

Set the replica count on the `ControlPlane` CR, not on the projected
`controlplane-cinder` child: the c5c3-operator re-asserts the child's
`spec.api.deployment.replicas` on every reconcile, so a direct edit of the child
is taken back.

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
if POD=$(kubectl -n cinder-system get pods \
     -l app.kubernetes.io/name=cinder-operator,app.kubernetes.io/instance=cinder-operator \
     -o jsonpath='{.items[0].metadata.name}') &&
   KS=$(kubectl -n openstack get keystones -o jsonpath='{.items[0].metadata.name}') &&
   IMG=$(kubectl -n openstack get deploy "${KS}" \
     -o jsonpath='{.spec.template.spec.containers[?(@.name=="keystone")].image}') &&
   EPS=$(kubectl get endpoints kubernetes -n default -o json \
     | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip):\($p)"') &&
   [ -n "${POD}" ] && [ -n "${IMG}" ] && [ -n "${EPS}" ]; then
  NAME="apiserver-check-$(date +%s)"
  kubectl -n cinder-system debug "pod/${POD}" --image="${IMG}" --profile=restricted \
    --container="${NAME}" -- python3 -u -c "${CHECK}" ${EPS} &&
  for _ in $(seq 90); do
    DONE=$(kubectl -n cinder-system get pod "${POD}" \
      -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state.terminated.reason}")
    [ -n "${DONE}" ] && break
    sleep 2
  done &&
  kubectl -n cinder-system get pod "${POD}" \
    -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state}{'\n'}" &&
  kubectl -n cinder-system logs "pod/${POD}" -c "${NAME}"
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
`cinder-operator` pod restarts.
On the kind devstack the first sign can instead be a `timed out` line from
the fresh-connection check of the Verify step.

**Diagnosis:** the egress allow-list does not match the API server the operator
actually dials. Either `kubeApiServer.cidrs` is missing one or more of the
current endpoint IPs (an HA control plane may have added a replica, or a
control-plane node may have been replaced with a different IP), or your CNI maps
the API server behind a port that is not in `kubeApiServer.ports`. A target
cluster registered since the policy went on is the same failure with a different
address.

**Fix:** re-run the discovery command from Step 1 and update
`networkPolicy.kubeApiServer.cidrs` to include **every** IP it returns, plus
every port.

### Every Cinder, CinderBackend and CinderBackupBackend write is rejected

**Symptom:** `kubectl apply` on any of the three kinds fails with
`failed calling webhook` plus `connection refused` or `no route to host`. The
`ControlPlane` stalls too: the c5c3-operator projects `controlplane-cinder` and
its `nfs1` / `nfsbk` satellites through the same admission path.

**Diagnosis:** webhook ingress (9443) is blocked. The chart renders one
MutatingWebhookConfiguration and one ValidatingWebhookConfiguration carrying six
webhooks between them (`mcinder.kb.io`, `mcinderbackend.kb.io`,
`mcinderbackupbackend.kb.io`, `vcinder.kb.io`, `vcinderbackend.kb.io`,
`vcinderbackupbackend.kb.io`), all with `failurePolicy: Fail` on create and
update, so an unreachable operator turns into a cluster-wide rejection of every
write to all three kinds. The admission rules do not match `DELETE`, so the
webhook cannot reject delete requests. Finalizers still gate removal: the Cinder
controller releases the database finalizer after issuing cleanup and handles a
remote-children finalizer for target-cluster CRs. If the controller is
unavailable, a `Cinder` can stay `Terminating` and hold up namespace deletion.
The usual cause is an API server that calls webhooks from an IP outside
`endpoints/kubernetes`, for example because it sits behind a front-end proxy.
`networkPolicy.webhookClients.cidrs` falls back to
`kubeApiServer.cidrs` when empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the policy blocks recovery, disable it using the release's management
method. For a Flux-owned release:

```bash
kubectl patch helmrelease cinder-operator -n cinder-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":false}}}}'
```

For a direct Helm installation:

```bash
helm upgrade cinder-operator oci://ghcr.io/c5c3/charts/cinder-operator \
  --namespace cinder-system --reuse-values \
  --set networkPolicy.enabled=false
```

The policy object is removed on the next reconcile, and the operator reverts to
unrestricted pod networking without a pod restart unless a
`cinder-operator-api-probe` policy from an earlier version of this guide still
selects the pod. That policy allows 8776 alone, so delete it as well:

```bash
kubectl delete networkpolicy cinder-operator-api-probe -n cinder-system \
  --ignore-not-found
```

## Tested by

Every operator chart includes the same chart-level NetworkPolicy template from
the shared operator-library, whose helm-unittest
(`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`)
pins the shared rules. The cinder chart's helm-unittest
(`operators/cinder/helm/cinder-operator/tests/networkpolicy_test.yaml`) pins the
egress rule the chart adds for its API health check. These tests cover rendering
only; they do not exercise packet enforcement.

The `cinder-operator-network-policy-egress` Chainsaw suite adds runtime coverage
for the Cinder operator chart. CI runs it in the cinder `e2e-operator` job on
kind v0.33.0 with the default kindnet CNI. The suite installs a second release,
`cinder-operator-netpol`, with `networkPolicy.enabled=true` and the API server
addresses of EndpointSlice `default/kubernetes`. It checks that the policy
selects the pod of that release and leaves the primary `cinder-operator` pod
alone, and that this policy is the only NetworkPolicy in `cinder-system`. The
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
3. It reaches a stand-in for the Cinder API on TCP 8776. It is dropped on the
   stand-in's TCP 9999 and on the sink's TCP 8776, so the rule admits the port
   only to its peer.

A stand-in replaces the Cinder API pods: a bare pod that carries the two labels
of the chart's rule (`app.kubernetes.io/name: cinder` and
`app.kubernetes.io/managed-by: cinder-operator`) and listens on TCP 8776. The
suite applies no Cinder CR.
`operators/cinder/internal/controller/chart_networkpolicy_test.go` checks that
the rule admits the labels the operator sets on the real API pods and the port
their Service targets.

Run the following from the repository root against a disposable devstack with
infrastructure and the primary `cinder-operator` release in `cinder-system`.
Install the tools pinned by `hack/install-test-deps.sh`; the script steps need
Git, Helm, kubectl and jq as well as Chainsaw, and image loading needs kind and
Docker. Building the image with `make image` requires
[Nix with flakes](../../reference/ci-cd/nix-image-build.md). The release runs
`ghcr.io/c5c3/cinder-operator:dev` with `image.pullPolicy=Never`, so load that
image into the kind cluster first:

```bash
make image OPERATOR=cinder IMG=ghcr.io/c5c3/cinder-operator:dev
kind load docker-image ghcr.io/c5c3/cinder-operator:dev --name cobaltcore
helm dependency build --skip-refresh operators/cinder/helm/cinder-operator
chainsaw test --test-dir tests/e2e/cinder-operator/network-policy-egress
```

Replace `cobaltcore` if your cluster has another name. The probe pods run
`ghcr.io/c5c3/cinder:2026.1`, which the kind node pulls when the cluster does
not hold it yet. Chainsaw runs scripts from the test directory, and the install
step resolves the chart and values paths from `GITHUB_WORKSPACE` in CI and from
`git rev-parse --show-toplevel` locally. No `GITHUB_WORKSPACE` export is needed
in a local Git checkout.

A second suite, `tests/e2e/cinder/network-policy`, covers the other policy: the
one the operator projects around the Cinder pods from `spec.networkPolicy` on a
`Cinder` CR. That is a different subject from this guide's, and the
[Cinder CRD API Reference](../../reference/cinder/cinder-crd.md#spec) documents
its field.
