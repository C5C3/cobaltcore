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

> **Scope.** Neither `OVNCentral` nor `OVNChassis` carries a `spec.networkPolicy`
> field, so there is no per-CR policy to pair this one with. The OVN workload
> exposes two OVSDB databases that authenticate every connection with a
> certificate the `OVNCentral` issues, and a chassis layer that runs in the
> node's own network namespace, where a pod-selector policy has nothing to
> match. This guide covers the operator pod alone.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Deploy a ControlPlane](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to the **Create a first network** check in its
Step 7, so the ovn-operator is running (namespace `ovn-system`) alongside the
`controlplane-ovn` central it reconciles.
:::

1. A running `ovn-operator` Helm release (namespace `ovn-system`).
2. The `OVNCentral` `controlplane-ovn`, which the quick start applies in its
   Step 3. Without it the Step 2 verification has nothing to roll.

## Step 1 — Enable the policy

The chart guards `networkPolicy.enabled=true` with a fail-closed check:
`networkPolicy.kubeApiServer.cidrs` and `ports` must both be non-empty, or the
template refuses to render. Gather the API server CIDRs and ports from the
`kubernetes` **Endpoints**, which carry the real API server addresses. The
`10.96.0.1` Service VIP that `kubectl get service kubernetes` reports is the
wrong source: an enforcing CNI
(Calico, Cilium, and kindnet on kind v0.32 and later) evaluates policy after the
Service DNAT has rewritten a packet aimed at the VIP to one of those endpoint
IPs, so a rule naming the VIP never matches:

```bash
kubectl get endpoints kubernetes -n default -o json \
  | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip)/32 port=\($p)"'
```

Record **every** endpoint IP the command prints, not just the first. A kind
devstack reports the one control-plane node's address on the kind bridge
(`172.18.0.x`), but an HA control plane reports one per API server replica. The
rule must cover all of them, or the operator loses its leader-election lease
whenever it happens to be talking to an excluded replica.

On the tutorial devstacks the `ovn-operator` release is owned by Flux (a
`HelmRelease`), so set the values by patching its `spec.values`. A raw
`helm upgrade` is reverted by the Flux helm-controller on its next reconcile.
Substitute the CIDRs and ports from above; the single-CIDR list below stands in
for the kind case of one control-plane node:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"enabled":true,"kubeApiServer":{"cidrs":["172.18.0.2/32"],"ports":[6443]}}}}}'

kubectl wait helmrelease/ovn-operator -n ovn-system \
  --for=condition=Ready --timeout=5m
```

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
`kubeApiServer` lists.

Metrics ingress is opt-in and separate. `networkPolicy.allowMetricsFrom` is
empty by default, and with the policy on, an empty list means no ingress rule
for the metrics port (8080) at all, so a Prometheus that scraped the operator
before stops reaching it. Name the scraping namespace when you enable both this
policy and the ServiceMonitor from
[Enable the OVN Operator Metrics Endpoint](./enable-ovn-operator-metrics.md):

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"allowMetricsFrom":[{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"monitoring"}}}]}}}}'
```

Each entry is rendered verbatim as a `NetworkPolicyPeer`, so a `podSelector`
narrows it further to the Prometheus pods.

## Step 2 — Verify

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
the edit stands until you revert it yourself. The block records the central's
current northd count, scales northd one replica above the size it runs at, and
puts the count back.

The patch passes through the ovn-operator's own admission webhooks. In the
first seconds after the restart, the webhook Service can still route a request
to the old pod, which no longer serves, and the patch fails with
`failed calling webhook`. That happens with the policy disabled too, so the
block tries the patch up to ten times, two seconds apart. A refused call fails
at once and the ten tries take about 20 seconds. A call the CNI drops, as
kindnet does when the webhook ingress rule misses the API server, fails only
after the API server's 10-second webhook timeout (the chart sets no
`timeoutSeconds`) with `context deadline exceeded`, and the ten tries take about
two minutes. When every try fails, the block prints a message and skips the
wait and the revert. That failure is the one under
[Every OVNCentral and OVNChassis write is rejected](#every-ovncentral-and-ovnchassis-write-is-rejected):

```bash
if CUR=$(kubectl get ovncentral controlplane-ovn -n openstack \
     -o jsonpath='{.spec.northd.deployment.replicas}') &&
   RUN=$(kubectl get deploy/controlplane-ovn-northd -n openstack \
     -o jsonpath='{.spec.replicas}'); then
  NEW=$((RUN + 1))

  patched=
  for _ in $(seq 10); do
    if kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
         -p "{\"spec\":{\"northd\":{\"deployment\":{\"replicas\":$NEW}}}}"; then
      patched=1
      break
    fi
    sleep 2
  done

  if [ -n "$patched" ]; then
    kubectl wait deploy/controlplane-ovn-northd -n openstack --timeout=5m \
      --for=jsonpath='{.spec.replicas}'="$NEW"
    kubectl rollout status deploy/controlplane-ovn-northd -n openstack --timeout=5m

    # revert to the recorded count; null drops the field again
    kubectl patch ovncentral controlplane-ovn -n openstack --type merge \
      -p "{\"spec\":{\"northd\":{\"deployment\":{\"replicas\":${CUR:-null}}}}}"
  else
    echo "every OVNCentral patch was rejected; nothing was changed" >&2
  fi
else
  echo "could not read the replica counts; nothing was patched" >&2
fi
```

The outer `if` stops the block before the first patch when either read fails.
A failed read prints the same empty string as a central that leaves the count
unset, so without the guard the revert would drop a count the central carries,
and a failed Deployment read would set `NEW` to 1 and shrink northd instead of
growing it.

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
if POD=$(kubectl -n ovn-system get pods \
     -l app.kubernetes.io/name=ovn-operator,app.kubernetes.io/instance=ovn-operator \
     -o jsonpath='{.items[0].metadata.name}') &&
   KS=$(kubectl -n openstack get keystones -o jsonpath='{.items[0].metadata.name}') &&
   IMG=$(kubectl -n openstack get deploy "${KS}" \
     -o jsonpath='{.spec.template.spec.containers[?(@.name=="keystone")].image}') &&
   EPS=$(kubectl get endpoints kubernetes -n default -o json \
     | jq -r '.subsets[] | (.addresses[].ip) as $ip | (.ports[].port) as $p | "\($ip):\($p)"') &&
   [ -n "${POD}" ] && [ -n "${IMG}" ] && [ -n "${EPS}" ]; then
  NAME="apiserver-check-$(date +%s)"
  kubectl -n ovn-system debug "pod/${POD}" --image="${IMG}" --profile=restricted \
    --container="${NAME}" -- python3 -u -c "${CHECK}" ${EPS} &&
  for _ in $(seq 90); do
    DONE=$(kubectl -n ovn-system get pod "${POD}" \
      -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state.terminated.reason}")
    [ -n "${DONE}" ] && break
    sleep 2
  done &&
  kubectl -n ovn-system get pod "${POD}" \
    -o jsonpath="{.status.ephemeralContainerStatuses[?(@.name==\"${NAME}\")].state}{'\n'}" &&
  kubectl -n ovn-system logs "pod/${POD}" -c "${NAME}"
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
`ovn-operator` pod restarts.
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
server that calls webhooks from an IP that is **not** in `endpoints/kubernetes`,
for example because it sits behind a front-end proxy.
`networkPolicy.webhookClients.cidrs` falls back to `kubeApiServer.cidrs` when
empty, which is wrong in that topology.

**Fix:** discover the actual caller IP (check the API-server audit log or the
`kube-apiserver` Pod's `--advertise-address`) and set
`networkPolicy.webhookClients.cidrs` explicitly:

```bash
kubectl patch helmrelease ovn-operator -n ovn-system --type=merge \
  -p '{"spec":{"values":{"networkPolicy":{"webhookClients":{"cidrs":["10.1.0.0/24"]}}}}}'
```

If the wedge blocks you from recovering, set `networkPolicy.enabled=false` with
the same patch shape. The policy object is removed on the next reconcile and the
operator reverts to unrestricted pod networking without a pod restart.

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
