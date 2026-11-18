---
title: Enable the Glance Operator NetworkPolicy
quadrant: operator
---

<!-- operator namespace is `glance-system`; workload (Glance CR) stays in `openstack`. -->

# How-to: Enable the Glance Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level
NetworkPolicy that restricts the glance-operator pod's egress and ingress
to the rules required for reconciliation.

Kubernetes NetworkPolicies combine allowed connections additively. Another
NetworkPolicy that selects the same operator pod may allow additional traffic,
so inspect existing policies in `glance-system` when assessing the effective
allow-list ([Kubernetes NetworkPolicy semantics](https://kubernetes.io/docs/concepts/services-networking/network-policies/#the-two-sorts-of-pod-isolation)).

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

A running `glance-operator` Helm release (namespace `glance-system`) and
`kubectl` and `jq` on the workstation are required.

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`EndpointSlice` objects for the `kubernetes` Service in `default`. The
NetworkPolicy API leaves the order of Service DNAT and policy evaluation to
the network plugin. The Glance e2e suite confirms that kindnet on kind v0.33
evaluates this rule after DNAT, against the EndpointSlice addresses. Use those
addresses for the tutorial devstack; verify your CNI's behavior before
applying the same assumption to another cluster
([Kubernetes NetworkPolicy behavior](https://kubernetes.io/docs/concepts/services-networking/network-policies/#behavior-of-to-and-from-selectors)).

```bash
kubectl get endpointslices -n default \
  -l kubernetes.io/service-name=kubernetes -o json \
  | jq -r '
      .items[] as $slice
      | $slice.endpoints[]?.addresses[]? as $ip
      | $slice.ports[]?.port as $p
      | select($p != null)
      | "\($ip)\(if ($ip | contains(":")) then "/128" else "/32" end) port=\($p)"
    '
```

Record **every** endpoint IP the command prints. A kind devstack reports the
one control-plane node's address on the kind bridge, for example
`172.18.0.2/32`. IPv6 addresses use `/128`. An HA control plane reports one
address per API server replica:

```
10.0.0.10/32 port=6443
10.0.0.11/32 port=6443
10.0.0.12/32 port=6443
```

The rule must cover all of them, or the operator loses its leader-election
lease whenever it happens to be talking to an excluded replica.

On Cilium, CIDR selectors do not match cluster node addresses by default
([Cilium CIDR matching](https://docs.cilium.io/en/stable/security/policy/layer3/#cidr-select-nodes)).
If the API server runs on a node, the chart's API-server egress CIDR may not
match even when it lists every EndpointSlice address. Before enabling the
policy, add a CiliumNetworkPolicy that selects the operator pod and allows
egress to `kube-apiserver` on the API server ports you discovered above
([Cilium's kube-apiserver entity](https://docs.cilium.io/en/stable/security/policy/layer3/#access-to-from-kube-apiserver)).
The chart's webhook ingress rule also uses an `ipBlock`; if webhook requests
arrive from a node address, allow ingress from `kube-apiserver` on TCP 9443
with Cilium policy as well. Cilium notes that this entity may not work for
ingress on some managed distributions, so verify the caller path there.

Setting `--policy-cidr-match-mode=nodes` (Helm value
`policyCIDRMatchMode: nodes`) is another option. It lets CIDR selectors match
node addresses cluster-wide and causes Cilium to allocate node identities.
Review the affected CIDR policies before enabling it.

For Flux-managed releases, metrics ingress is separate and opt-in. The default
`networkPolicy.allowMetricsFrom: []` renders no ingress rule for the metrics
port (8080), so Prometheus cannot scrape the operator after this policy is enabled
unless you add a peer first. For Prometheus in the `monitoring` namespace,
patch the HelmRelease before enabling the policy:

```bash
kubectl patch helmrelease glance-operator -n glance-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

This namespace selector allows all pods in `monitoring`; add a `podSelector`
to narrow it if needed. This value only opens the network path. The
ServiceMonitor is controlled separately by
`monitoring.serviceMonitor.enabled`; see [Enable the Glance Operator Metrics Endpoint](./enable-glance-operator-metrics.md).
For a direct Helm install, set the same peer in the chart values.

On the tutorial devstacks the `glance-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. The Flux
helm-controller reverts a raw `helm upgrade` on its next
reconcile. Substitute the CIDRs and ports from above. The single-CIDR list
below is the kind case, one control-plane node, so replace `172.18.0.2/32`
with the address the discovery command printed and extend the list to every
IP an HA control plane prints. Add every distinct port from discovery to the
`ports` list as well:

```bash
kubectl patch helmrelease glance-operator -n glance-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/glance-operator -n glance-system \
  --for=condition=Ready --timeout=5m
```

The chart-level policy allows egress to the kube-apiserver, DNS, and the
Glance API. When the webhook is enabled, it allows ingress on TCP 9443 from
`networkPolicy.webhookClients.cidrs`, falling back to
`networkPolicy.kubeApiServer.cidrs` when that list is empty. Metrics ingress is
rendered only when `networkPolicy.allowMetricsFrom` is non-empty. The Glance
API rule admits TCP 9292 to every pod labelled
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
values with a rolling `helm upgrade`. Use every CIDR and port from the
discovery command; the address and port below are a single-IPv4 kind example.
For additional CIDRs or ports, add the corresponding indexed `--set` entries.
The fail-closed guard requires `kubeApiServer.cidrs` and `ports`:

```bash
helm upgrade glance-operator oci://ghcr.io/c5c3/charts/glance-operator \
  --namespace glance-system --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

This command leaves metrics ingress closed. If Prometheus scrapes the operator,
set `networkPolicy.allowMetricsFrom` to the appropriate peer in the Helm values
before the upgrade.

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
kubectl -n glance-system rollout restart deploy/glance-operator
kubectl -n glance-system rollout status deploy/glance-operator --timeout=5m
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

Finally, check the `kubeApiServer` rule on a fresh connection, once the
restarted pod has run for at least one minute. The block adds an ephemeral
container to the operator pod. The container shares the pod's network, so the
policy judges its connections as it judges the operator's. It runs the image
of the Keystone API Deployment, which ships `python3` and which the devstack's
single node already holds, and opens one new TCP connection to every address
and port of the `kubernetes` EndpointSlices. The block waits up to three minutes
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
if POD=$(kubectl -n glance-system get pods \
     -l app.kubernetes.io/name=glance-operator,app.kubernetes.io/instance=glance-operator \
     -o jsonpath='{.items[0].metadata.name}') &&
   KS=$(kubectl -n openstack get keystones -o jsonpath='{.items[0].metadata.name}') &&
   IMG=$(kubectl -n openstack get deploy "${KS}" \
     -o jsonpath='{.spec.template.spec.containers[?(@.name=="keystone")].image}') &&
   EPS=$(kubectl get endpointslices -n default \
     -l kubernetes.io/service-name=kubernetes -o json \
        | jq -r '
            .items[] as $slice
            | $slice.endpoints[]?.addresses[]? as $ip
            | $slice.ports[]?.port as $p
            | select($p != null)
            | "\($ip):\($p)"
          ') &&
   [ -n "${POD}" ] && [ -n "${IMG}" ] && [ -n "${EPS}" ]; then
  NAME="apiserver-check-$(date +%s)"
  kubectl -n glance-system debug "pod/${POD}" --image="${IMG}" --profile=restricted \
    --container="${NAME}" -- python3 -u -c "${CHECK}" ${EPS} &&
  for _ in $(seq 90); do
    DONE=$(kubectl -n glance-system get pod "${POD}" \
      -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state.terminated.reason}")
    [ -n "${DONE}" ] && break
    sleep 2
  done &&
  kubectl -n glance-system get pod "${POD}" \
    -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state}{'\n'}" &&
  kubectl -n glance-system logs "pod/${POD}" -c "${NAME}"
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
`glance-operator` pod restarts.
On the kind devstack the first sign can instead be a `timed out` line from
the fresh-connection check of the Verify step.

**Diagnosis:** the egress allow-list does not match the API server the operator
actually dials. Either `kubeApiServer.cidrs` is missing one or more of the
current EndpointSlice IPs (an HA control plane may have added a replica, or a
control-plane node may have been replaced with a different IP), or your CNI maps
the API server behind a port that is not in `kubeApiServer.ports`. A rule naming
the `10.96.0.1` Service VIP instead of an endpoint address fails on the tested
kindnet path after Service DNAT. On Cilium without
`--policy-cidr-match-mode=nodes`, a CIDR selector also does not match an API
server running on a cluster node; see the Cilium paragraph in Step 1.

**Fix:** re-run the discovery command from Step 1 and update
`networkPolicy.kubeApiServer.cidrs` to include **every** IP it returns, plus
every port:

```bash
kubectl get endpointslices -n default \
  -l kubernetes.io/service-name=kubernetes -o json \
  | jq -r '
      .items[] as $slice
      | $slice.endpoints[]?.addresses[]? as $ip
      | $slice.ports[]?.port as $p
      | select($p != null)
      | "\($ip)\(if ($ip | contains(":")) then "/128" else "/32" end) port=\($p)"
    '
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
server that calls webhooks from an IP that is **not** in the API server's
EndpointSlice address set (for example, because it sits behind a front-end proxy).
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
