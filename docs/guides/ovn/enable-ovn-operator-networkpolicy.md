---
title: Enable the OVN Operator NetworkPolicy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

<!-- operator namespace is `ovn-system`; workload (OVNCentral, OVNChassis) stays in `openstack`. -->

# How-to: Enable the OVN Operator NetworkPolicy

This guide walks an operator through opting in to the chart-level NetworkPolicy
that restricts the ovn-operator pod's egress and ingress to the minimum required
for correct reconciliation.

Kubernetes NetworkPolicies combine allowed connections additively. Another
policy that selects the operator pod may allow additional traffic, so inspect
existing policies in `ovn-system` when assessing the effective allow-list
([Kubernetes NetworkPolicy semantics](https://kubernetes.io/docs/concepts/services-networking/network-policies/#the-two-sorts-of-pod-isolation)).

> **Scope.** Neither `OVNCentral` nor `OVNChassis` carries a `spec.networkPolicy`
> field, so there is no per-CR policy to pair this one with. The OVN workload
> exposes two OVSDB databases that authenticate every connection with a
> certificate the `OVNCentral` issues, and a chassis layer that runs in the
> node's own network namespace, where a pod-selector policy has nothing to
> match. This guide covers the operator pod alone.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 7, so the ovn-operator is running (namespace `ovn-system`) alongside the
`controlplane-ovn` central it reconciles.
:::

The `ovn-operator` Helm release must be running in `ovn-system`. The quick
start applies `OVNCentral` `controlplane-ovn` in Step 3; without it, the Step 2
verification has nothing to roll. The commands require `kubectl` and `jq`; the
Helm-managed path below also requires Helm. You need permission to list
EndpointSlices in `default` (check with
`kubectl auth can-i list endpointslices.discovery.k8s.io -n default`), patch
the `ovn-operator` HelmRelease for the Flux path, patch the `OVNCentral`, restart
the operator Deployment, read the operator pod and NetworkPolicy, check the
northd Deployment status, and create an ephemeral container in `ovn-system` for
the fresh-connection check (check with
`kubectl auth can-i patch pods/ephemeralcontainers -n ovn-system`). That
ephemeral-container permission is privileged: its holder can mount the
operator ServiceAccount token into an ephemeral container, so grant it only to
trusted administrators.

## Step 1: Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from ready
endpoints in the EndpointSlices for the `kubernetes` Service in `default`.
Kubernetes leaves the order of Service destination network address translation
(DNAT) and NetworkPolicy evaluation to the network plugin. The OVN e2e suite
confirms that kindnet on kind v0.33 evaluates this rule after DNAT, so the
Service VIP does not match there. Use the endpoint addresses for this devstack
and verify your CNI's behavior before applying the same assumption elsewhere
([Kubernetes NetworkPolicy behavior](https://kubernetes.io/docs/concepts/services-networking/network-policies/#behavior-of-to-and-from-selectors)):

```bash
kubectl get endpointslices -n default \
  -l kubernetes.io/service-name=kubernetes -o json \
  | jq -r '
      [
        .items[] as $slice
        | $slice.endpoints[]?
        | select(.conditions.ready != false)
        | .addresses[]? as $ip
        | $slice.ports[]?.port as $port
        | select($port != null)
        | "\($ip)\(if ($ip | contains(":")) then "/128" else "/32" end) port=\($port)"
      ] | unique[]'
```

Record every unique CIDR and port the command prints. IPv4 addresses use
`/32`; IPv6 addresses use `/128`. A kind devstack reports the control-plane
node's address on the kind bridge, for example
`172.18.0.2/32 port=6443`. An HA control plane reports an address for each API
server replica, and a dual-stack cluster can report both address families. The
command should print at least one address and port; if it does not, check the
EndpointSlices before continuing. The chart puts all CIDRs and ports into one
rule, so each listed port is allowed to each listed address. The rule must
cover every address and port, or the operator can lose its leader-election
lease when it talks to an excluded replica.

On Cilium, CIDR selectors do not match cluster node addresses by default
([Cilium CIDR matching](https://docs.cilium.io/en/stable/security/policy/layer3/#cidr-select-nodes)).
If the API server runs on a node, the chart's API-server egress CIDR may not
match even when it lists every EndpointSlice address. Before enabling the
policy, add a CiliumNetworkPolicy that selects the operator pod and allows
egress to `kube-apiserver` on the API server ports discovered above
([Cilium's kube-apiserver entity](https://docs.cilium.io/en/stable/security/policy/layer3/#access-to-from-kube-apiserver)).
The chart's webhook ingress rule also uses an `ipBlock`; if webhook requests
arrive from a node address, add Cilium ingress for `kube-apiserver` on TCP
port 9443. Verify that entity on managed distributions, where it may not
identify the webhook caller.

Setting `--policy-cidr-match-mode=nodes` (Helm value
`policyCIDRMatchMode: nodes`) is another option. It lets CIDR selectors match
node addresses cluster-wide and allocates identities for nodes. Cilium
recommends leaving CIDR match mode disabled for most deployments; review the
identity and memory impact and all affected CIDR policies before enabling it.

On the tutorial devstacks the `ovn-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. A raw
`helm upgrade` is reverted by the Flux helm-controller on its next reconcile.
Only patch an active HelmRelease; a suspended release stores the values but
does not apply them. Check `spec.suspend` with
`kubectl get helmrelease ovn-operator -n ovn-system -o jsonpath='{.spec.suspend}'`.
Resume Flux only when it should own this release; otherwise use the
Helm-managed path below.

Metrics ingress is opt-in. `networkPolicy.allowMetricsFrom` is empty by
default, and with the policy on, an empty list means no ingress rule for the
metrics port (8080). If Prometheus already scrapes the operator, add its peer
before enabling the policy to avoid interrupting scrapes. For Flux, patch the
peer while the HelmRelease is active:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Once the patch succeeds, check the release:

```bash
kubectl get helmrelease ovn-operator -n ovn-system -o yaml
```

Check that `status.observedGeneration` matches `metadata.generation` and the
`Ready` condition is `True` before enabling the policy. Repeat the command if
Flux has not reconciled the patch yet.

For direct Helm installations, set the same value in the values supplied to
`helm upgrade`. Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a
`podSelector` narrows it further to the Prometheus pods.

Enable the policy after adding the metrics peer, if needed:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'
```

Substitute the CIDRs and ports from the discovery command; the single-CIDR
list stands in for the kind case of one control-plane node. Once the patch
succeeds, check that Flux has reconciled the current generation and the release
is `Ready`, as above.

::: details Helm-managed installations (non-Flux)
For a release managed directly by Helm, enable the same values with a rolling
`helm upgrade`. Find the installed chart version:

```bash
helm list -n ovn-system --filter '^ovn-operator$'
```

Use the version from the `CHART` column to pin the upgrade. For example, if
the column reads `ovn-operator-0.7.0`:

```bash
helm upgrade ovn-operator oci://ghcr.io/c5c3/charts/ovn-operator \
  --namespace ovn-system --version 0.7.0 --reuse-values \
  --set networkPolicy.enabled=true \
  --set 'networkPolicy.kubeApiServer.cidrs[0]=172.18.0.2/32' \
  --set 'networkPolicy.kubeApiServer.ports[0]=6443'
```

Replace the example CIDR and port with values from the EndpointSlice command.
For an HA or dual-stack control plane, add an indexed `--set` entry for each
additional CIDR and distinct port. If Prometheus scrapes the operator, add
`networkPolicy.allowMetricsFrom` to a values file and pass it with `--values`
to this command. Also set `networkPolicy.webhookClients.cidrs` if the webhook
caller address differs from the API server endpoints. Do not run this on
tutorial devstacks: Flux owns that release and reverts out-of-band Helm
revisions on its next reconcile.
:::

The rendered policy allows what the operator needs: egress to the kube-apiserver
and to DNS, ingress to the webhook port. Nothing else belongs on the list. The
ovn-operator opens no connection of its own to OVN. It writes StatefulSets,
Deployments, DaemonSets, ConfigMaps and Jobs, and the OVSDB traffic is theirs,
from the workload namespace. The chart also defines no
`operator-library.chart.networkPolicyEgress` override, the hook a chart uses to
append an egress rule of its own (the barbican chart appends its OpenBao port),
so the shared set is the whole set. One deployment shape does need more: with an
`OVNCentral` or `OVNChassis` placed on a registered target cluster, the operator
talks to that cluster's API server too, so add its address and port to the same
`kubeApiServer` lists. The `allowMetricsFrom` peer also permits scraping when
you enable the ServiceMonitor from
[Enable the OVN Operator Metrics Endpoint](./enable-ovn-operator-metrics.md).

## Step 2: Verify

On the kind devstack this step checks the policy's shape, that the operator
still reconciles under the enforced policy, and that a fresh connection from
the operator pod reaches the API server. The step sends no traffic outside the
allow-list.

Restart the operator. A connection opened before the policy existed stays open,
so the restart makes the operator open its connections again:

```bash
kubectl -n ovn-system rollout restart deploy/ovn-operator
kubectl -n ovn-system rollout status deploy/ovn-operator --timeout=5m
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
kubectl -n ovn-system get networkpolicy
kubectl -n ovn-system describe networkpolicy ovn-operator
```

Then confirm reconciliation still works end-to-end by driving a change through
the `OVNCentral` and watching northd roll out. The central is a CR you own on
this devstack: the ControlPlane references it and projects nothing onto it, so
it will not overwrite the test change. First, record the central's configured
count and the Deployment's current count:

```bash
CUR="$(kubectl get ovncentral controlplane-ovn -n openstack \
  -o jsonpath='{.spec.northd.deployment.replicas}')"
RUN="$(kubectl get deploy/controlplane-ovn-northd -n openstack \
  -o jsonpath='{.spec.replicas}')"
echo "Configured: ${CUR:-unset}; running: $RUN"
```

Check that both reads succeeded and the running count is a number. Keep this
shell open: the commands below use these values to scale northd up by one and
restore the original configuration.

The patch passes through the ovn-operator's own admission webhooks. In the
first seconds after the restart, the webhook Service can still route a request
to the old pod, which no longer serves, and the patch fails with
`failed calling webhook`. If this happens immediately after the restart, retry
the patch. A persistent failure is described under
[Every OVNCentral and OVNChassis write is rejected](#every-ovncentral-and-ovnchassis-write-is-rejected):

```bash
NEW=$((RUN + 1))
kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
  -p "{\"spec\":{\"northd\":{\"deployment\":{\"replicas\":$NEW}}}}"
```

Once the patch succeeds, wait for the operator to update the Deployment and
for the rollout to finish:

```bash
kubectl wait deploy/controlplane-ovn-northd -n openstack --timeout=5m \
  --for=jsonpath='{.spec.replicas}'="$NEW"
kubectl rollout status deploy/controlplane-ovn-northd -n openstack --timeout=5m
```

Restore the original count, even if either check fails. If the count was unset,
`null` removes the field again:

```bash
kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
  -p "{\"spec\":{\"northd\":{\"deployment\":{\"replicas\":${CUR:-null}}}}}"
```

If the restore fails, retry it after resolving the reported error.

The `kubectl wait` is the check that proves the operator still reconciles. The
restarted operator writes nothing until it holds the leader lease, and until it
writes the new count the northd Deployment is still fully rolled out at its old
size, so a `rollout status` alone would pass at once. The new count is always
one above the running one, so the patch changes the Deployment whatever size
northd already runs at.

The extra replica is idle by design: one northd holds the Southbound lock and
the rest wait on it, so the rollout proves the write path without changing what
the control plane computes.

Finally, check the `kubeApiServer` rule on a fresh connection, once the
restarted pod has run for at least one minute. The block adds an ephemeral
container to the operator pod. The container shares the pod's network, so the
policy judges its connections as it judges the operator's. It runs the image
of the Keystone API Deployment, which ships `python3` and which the devstack's
single node already holds. Find the operator pod and the image:

```bash
POD="$(kubectl -n ovn-system get pods \
  -l app.kubernetes.io/name=ovn-operator,app.kubernetes.io/instance=ovn-operator \
  -o jsonpath='{.items[0].metadata.name}')"
KS="$(kubectl -n openstack get keystones -o jsonpath='{.items[0].metadata.name}')"
IMG="$(kubectl -n openstack get deploy "$KS" \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="keystone")].image}')"
echo "Pod: $POD; image: $IMG"
```

Check that the pod and image are present. Start an ephemeral container that
opens a fresh connection to an API server endpoint. Replace `172.18.0.2` and
`6443` with an address and port from Step 1:

```bash
NAME="apiserver-check-$(date +%s)"
kubectl -n ovn-system debug "pod/$POD" --image="$IMG" --profile=restricted \
  --container="$NAME" -- python3 -u -c \
  'import socket; socket.create_connection(("172.18.0.2", 6443), timeout=5).close(); print("connected")'
```

Check the container's state and log:

```bash
kubectl -n ovn-system get pod "${POD}" \
  -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state}{'\n'}"
kubectl -n ovn-system logs "pod/${POD}" -c "${NAME}"
```

On success, the state is `terminated` with reason `Completed` and exit code
`0`, and the log says `connected`:

```text
{"terminated":{"containerID":"containerd://d0af3b80a47d77c57535cd38a77f062d8eeaeb1b0210bf2f3d89ec554b3905bc","exitCode":0,"finishedAt":"2026-11-06T15:47:48Z","reason":"Completed","startedAt":"2026-11-06T15:47:48Z"}}
connected
```

A `running` state, or a `waiting` state with reason `ContainerCreating`,
means the check has not ended yet: run the state and log commands again. Any
other `waiting` reason, such as `ImagePullBackOff` or
`RunContainerError`, means the node could not pull or start the image, and
the state's `message` names the cause. A terminated state with a nonzero exit
code means the connection failed; the state's `reason` and `exitCode` and the
log say why. An empty state means the node has not reported the check
container yet: check that the node is `Ready` with `kubectl get nodes`, then
run the state and log commands again.

A `TimeoutError` means the connection was dropped; check whether the policy
includes the address and port. Other errors, such as
`[Errno 111] Connection refused`, can indicate a different connectivity
problem. Repeat the check for every address and port from Step 1, using a new
container name each time. The ephemeral container exits after the check and
stays in the pod's spec until the next rollout replaces the pod. Adding it
needs the `patch` verb on
`pods/ephemeralcontainers` in the operator's namespace. That verb lets its
holder mount the operator's ServiceAccount token and act with the operator's
rights, so run the check as a cluster admin and do not grant the verb to a
separate verification role. `--profile=restricted` lets it start in a
namespace that enforces the restricted Pod Security Standard.

When the check reports a timeout, see
[Reconcile timeouts / leader-election churn](#reconcile-timeouts-leader-election-churn).

## Troubleshooting

### Reconcile timeouts / leader-election churn

**Symptom:** operator logs show `Get https://<kube-apiserver>: i/o timeout` or
leader-election lease renewals fail with `context deadline exceeded`, and the
`ovn-operator` pod restarts.
On the kind devstack the first sign can instead be a timeout from
the fresh-connection check of the Verify step.

**Diagnosis:** the egress allow-list does not match the destination address or
port as the CNI evaluates it. On the tested kindnet path, the rule must include
every current EndpointSlice address and port. An HA control plane may have
added a replica, or a control-plane node may have a different IP. If the
EndpointSlice values are complete, verify whether your CNI evaluates policy
before or after Service DNAT. On Cilium, CIDR selectors do not match node
addresses by default; see the Cilium note in Step 1. A target cluster registered
since the policy went on is the same failure with a different address.

**Fix:** re-run the discovery command from Step 1 and update
`networkPolicy.kubeApiServer.cidrs` and `ports` to include every value it
returns. Confirm that the CNI evaluates the rule against those endpoint
addresses and ports.

### Every OVNCentral and OVNChassis write is rejected

**Symptom:** `kubectl apply` on either kind fails with `failed calling webhook`
plus `connection refused` or `no route to host`. A CNI that drops the packets
instead of refusing them, such as kindnet, makes the call fail with
`context deadline exceeded` once the API server's 10-second webhook timeout
expires.

**Diagnosis:** webhook ingress (9443) is blocked. The chart registers a mutating
and a validating webhook for each of the two kinds, all four with
`failurePolicy=Fail` on create and update, so an unreachable operator turns into
a cluster-wide rejection of every write to both kinds. The usual cause is an API
server that calls webhooks from an IP that is **not** in the `kubernetes`
EndpointSlices, for example because it sits behind a front-end proxy.
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly. For an active Flux release:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

Replace the example CIDR with the caller address. Once the patch succeeds,
check that Flux has reconciled the current generation and the release is
`Ready`, as in Step 1.

For a directly managed release, set the same value with the pinned Helm
upgrade from Step 1.

If blocked webhook calls prevent normal recovery, set
`networkPolicy.enabled=false`. For an active Flux release, use the same patch
shape and check the release status. For a directly managed release, use the
pinned Helm upgrade from Step 1 with `networkPolicy.enabled=false`. The policy
object is removed on the next reconcile and the operator returns to
unrestricted pod networking without a pod restart. To disable it on a Flux
release:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":false}}}}'
```

Once the patch succeeds, check that Flux has reconciled the current generation
and the release is `Ready`, as in Step 1.

## Tested by

Every operator chart includes the chart-level NetworkPolicy template from the
shared operator-library. The OVN chart ships no NetworkPolicy unit test of its
own. The library's helm-unittest checks the rendered shared policy in
`operators/shared/helm/operator-library-testbed/tests/networkpolicy_test.yaml`
(default-off posture, rule shape, both fail-closed guards, and the absence of an
ingress rule for the health-probe port). These tests cover rendering only; they
do not exercise packet enforcement.

The `ovn-operator-network-policy-egress` Chainsaw suite adds runtime coverage
for the OVN operator chart. CI runs it in the ovn `e2e-operator` job on kind
v0.33.0 with the default kindnet CNI. The suite installs a second release,
`ovn-operator-netpol`, with `networkPolicy.enabled=true` and the API server
addresses of EndpointSlice `default/kubernetes`. It checks that the policy
selects the pod of that release and leaves the primary `ovn-operator` pod alone,
and that this policy is the only NetworkPolicy in `ovn-system`. The operator pod
must start without a restart and log that it detected cert-manager.

kindnet adds a new pod to its policy set a moment after the pod starts, so these
startup checks show only that the operator started with the policy present. The
proof comes from a probe pod the policy selects, on fresh connections:

1. It is dropped on a sink port that a control pod outside the policy reaches.
   The suite waits for this drop and fails with `was never dropped` on a cluster
   that does not enforce NetworkPolicy.
2. It resolves a cluster name and reaches the API server endpoint and the
   `kubernetes` Service VIP.

The OVN chart adds no egress rule of its own, so the suite runs no stand-in. It
applies no OVN CR, and the OVN operator has no `chart_networkpolicy_test.go`.

Run the following from the repository root against a disposable devstack with
infrastructure and the primary `ovn-operator` release in `ovn-system`. Install
the tools pinned by `hack/install-test-deps.sh`; the script steps need Git,
Helm, kubectl and jq as well as Chainsaw, and image loading needs kind and
Docker. Building the image with `make image` requires
[Nix with flakes](../../reference/ci-cd/nix-image-build.md). The release runs
`ghcr.io/c5c3/ovn-operator:dev` with `image.pullPolicy=Never`, so load that
image into the kind cluster first:

```bash
make image OPERATOR=ovn IMG=ghcr.io/c5c3/ovn-operator:dev
kind load docker-image ghcr.io/c5c3/ovn-operator:dev --name cobaltcore
helm dependency build --skip-refresh operators/ovn/helm/ovn-operator
chainsaw test --test-dir tests/e2e/ovn-operator/network-policy-egress
```

Replace `cobaltcore` if your cluster has another name. The probe pods run
`ghcr.io/c5c3/keystone:2026.1`, because the OVN image ships no `python3`. The
kind node pulls it when the cluster does not hold it yet. Chainsaw runs scripts
from the test directory, and the install step resolves the chart and values
paths from `GITHUB_WORKSPACE` in CI and from `git rev-parse --show-toplevel`
locally. No `GITHUB_WORKSPACE` export is needed in a local Git checkout.
