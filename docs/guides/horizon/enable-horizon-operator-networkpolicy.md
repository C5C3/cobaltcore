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
This guide is written against the **[Deploy a ControlPlane](../../quick-start-controlplane.md)** devstack. Stand it up first:

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
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which hold the real API server addresses. Do not
use the `10.96.0.1` Service VIP that `kubectl get service kubernetes` reports:
kindnet (kind v0.32 and later) and enforcing CNIs such as Calico and Cilium
evaluate policy after the Service DNAT, so a rule naming the VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record every endpoint IP the command prints. A kind devstack prints the one
control-plane node's address on the kind bridge, for example
`172.18.0.2/32 port=6443`; an HA control plane prints one line per API server
replica.

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

On the tutorial devstacks the `horizon-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next
reconcile. Substitute the CIDRs and ports from above. The single-CIDR list
below is the kind case, one control-plane node, so replace `172.18.0.2/32`
with the address the discovery command printed and extend the list to every
IP an HA control plane prints:

```bash
kubectl patch helmrelease horizon-operator -n horizon-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

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
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

Do **not** run this on the tutorial devstacks: there the release is
Flux-owned, and the helm-controller reverts out-of-band revisions on its next
reconcile. Use the HelmRelease patch above instead.
:::

## Step 2 — Verify

On the kind devstack this step checks the policy's shape, that the operator
still reconciles under the enforced policy, and that a fresh connection from
the operator pod reaches the API server. The step sends no traffic outside the
allow-list.

Restart the operator. A connection opened before the policy existed stays open,
so the restart makes the operator open its connections again:

```bash
kubectl -n horizon-system rollout restart deploy/horizon-operator
kubectl -n horizon-system rollout status deploy/horizon-operator --timeout=5m
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
reverted.

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
if POD=$(kubectl -n horizon-system get pods \
     -l app.kubernetes.io/name=horizon-operator,app.kubernetes.io/instance=horizon-operator \
     -o jsonpath='{.items[0].metadata.name}') &&
   KS=$(kubectl -n openstack get keystones -o jsonpath='{.items[0].metadata.name}') &&
   IMG=$(kubectl -n openstack get deploy "${KS}" \
     -o jsonpath='{.spec.template.spec.containers[?(@.name=="keystone")].image}') &&
   EPS=$(kubectl get endpoints kubernetes -n default -o json \
     | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip):\($p)"') &&
   [ -n "${POD}" ] && [ -n "${IMG}" ] && [ -n "${EPS}" ]; then
  NAME="apiserver-check-$(date +%s)"
  kubectl -n horizon-system debug "pod/${POD}" --image="${IMG}" --profile=restricted \
    --container="${NAME}" -- python3 -u -c "${CHECK}" ${EPS} &&
  for _ in $(seq 90); do
    DONE=$(kubectl -n horizon-system get pod "${POD}" \
      -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state.terminated.reason}")
    [ -n "${DONE}" ] && break
    sleep 2
  done &&
  kubectl -n horizon-system get pod "${POD}" \
    -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state}{'\n'}" &&
  kubectl -n horizon-system logs "pod/${POD}" -c "${NAME}"
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

If the restarted operator does not reach `Ready`, loses API server
connectivity, or the check prints `timed out`, the egress rule misses the
address or the port its connections reach after the Service DNAT. A rule
naming the `10.96.0.1` Service VIP fails this way, and so does a port missing
from `kubeApiServer.ports`. Compare the policy's egress CIDRs and ports with
`kubectl get endpoints kubernetes -n default`. On Cilium without
`--policy-cidr-match-mode=nodes`, no `ipBlock` matches an API server that runs
on a cluster node; see the Cilium paragraph in Step 1.

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

The suite also checks that the policy selects the pod of the
`horizon-operator-netpol` release and leaves the primary `horizon-operator` pod
alone, that it is the only NetworkPolicy in `horizon-system`, and that the
operator pod never restarted. Before the probe pod runs, a control pod that no
policy selects must reach the MariaDB port, which shows that the server listens.
After the drop, the probe pod must resolve a cluster name and reach the API
server endpoint and the `kubernetes` Service VIP on fresh connections, which only
the policy's DNS and API server rules admit.

This suite covers only the Horizon chart. It does not validate other operators.
