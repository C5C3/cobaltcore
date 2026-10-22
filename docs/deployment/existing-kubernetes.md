---
title: Deploy on an Existing Kubernetes Cluster
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Deploy on an Existing Kubernetes Cluster

Install CobaltCore's operators and a small ControlPlane on a Kubernetes
cluster you already manage. This walkthrough uses the same Keystone and
Horizon services as the [Quick Start](../quick-start.md), reached through a
local port-forward.

The repository ships a lab overlay for Gardener on metal-stack. Other
environments need an overlay reviewed for their storage, networking, and
platform controllers. This is an evaluation deployment with single-instance
backing services and lab certificates; production planning is outside this
walkthrough.

For KVM compute, Ceph volumes, and a complete Gardener lab, use
[Deploy on Bare Metal](../quick-start-metal-stack.md).

## Check the cluster

Use a dedicated test cluster. The installer creates cluster-wide CRDs,
controllers, webhooks, and namespaces, and bootstraps OpenBao. Check for
existing Flux, cert-manager, Gateway API, and backing-service installations
before applying an overlay that manages them.

The workstation needs `git`, `make`, `kubectl`, `jq`, and Mike Farah's `yq`
v4.40.1 or newer. Docker and kind are not required in external-cluster mode.

```bash
git clone https://github.com/c5c3/cobaltcore.git
cd cobaltcore
export KUBECONFIG=/absolute/path/to/your/kubeconfig
kubectl config current-context
kubectl get nodes
kubectl get storageclass
```

All later commands use this context. `EXTERNAL_CLUSTER=true` does not create
a cluster or switch contexts.

Before installation, the deployment script requires:

- A reachable API server and at least one `Ready` node.
- A default StorageClass with capacity for the stack's persistent volumes.
- No `kube-system/node-local-dns` DaemonSet. The supplied OpenBao network
  policy does not configure host-networked DNS endpoints.
- Credentials allowed to install and inspect the cluster-wide resources.

Nodes also need outbound access to the referenced image and chart registries.
Review available capacity against your chosen
[sizing profile](../reference/c5c3/controlplane-crd.md#built-in-profiles).

## Prepare the cluster configuration

An external overlay must contain `base/kustomization.yaml` and
`infrastructure/kustomization.yaml`. The first installs controllers and Flux
resources; the second adds resources that need their CRDs.

For a Gardener metal-stack evaluation, use the supplied overlay:

```bash
export EXTERNAL_OVERLAY=deploy/lab/metal-stack
```

For another environment, start a sibling overlay and review it before
deployment:

```bash
mkdir -p deploy/lab/my-cluster
cp -R deploy/lab/metal-stack/base deploy/lab/my-cluster/
cp -R deploy/lab/metal-stack/infrastructure deploy/lab/my-cluster/
export EXTERNAL_OVERLAY=deploy/lab/my-cluster
```

The copied overlay inherits lab defaults from `deploy/kind`. Review the
Gardener namespace labels, network policies and DNS behavior, storage class
selection, certificates, and the Headlamp and Flux demo UIs. Adapt these
settings to your platform. Render both layers to inspect the resources:

```bash
kubectl kustomize "$EXTERNAL_OVERLAY/base" > /tmp/cobaltcore-base.yaml
kubectl kustomize "$EXTERNAL_OVERLAY/infrastructure" > /tmp/cobaltcore-infrastructure.yaml
```

Keep the overlay beside `deploy/lab/metal-stack` so its relative references
resolve. See the [overlay reference](../reference/infrastructure/e2e-deployment.md#lab-overlay-patches)
for the supplied platform adaptations.

## Deploy the ControlPlane

```bash
EXTERNAL_CLUSTER=true \
WITH_CONTROLPLANE=true \
WITH_CONTROLPLANE_CR=true \
make deploy-infra

kubectl wait controlplane/controlplane -n openstack \
  --for=condition=Ready --timeout=10m
kubectl get controlplane,keystone,horizon -n openstack
```

`WITH_CONTROLPLANE_CR=true` installs the bundled minimal ControlPlane and
sets its public URLs to port `8443`. It does not install the full
metal-stack ControlPlane or its Ceph and hypervisor resources. Leaving the
flag unset installs the operator stack and leaves applying a ControlPlane
to you.

External mode rejects `WITH_VPA`, `WITH_METRICS_SERVER`,
`WITH_REGISTRY_CACHE`, and `WITH_OVN_KERNEL_MODULES`. Optional NFS, Ceph,
monitoring, and chaos components require matching directories and
prerequisites in your overlay. See
[deployment settings](../reference/infrastructure/e2e-deployment.md#environment-variables).

## Connect and verify

Keep the following port-forward running in a separate terminal:

```bash
export KUBECONFIG=/absolute/path/to/your/kubeconfig
kubectl config current-context
ENVOY_SERVICE=$(kubectl get svc -n envoy-gateway-system \
  -l gateway.envoyproxy.io/owning-gateway-name=openstack-gw \
  -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n envoy-gateway-system "svc/${ENVOY_SERVICE}" 8443:443
```

Check the identity API:

```bash
curl -fk https://keystone.127-0-0-1.nip.io:8443/v3
```

The response identifies API version `v3`. Complete the authentication and
Horizon checks in the [Quick Start](../quick-start.md). Those URLs use the
same hostnames and port as this port-forward.

If readiness fails, inspect the conditions and events with
[Observability and Diagnostics](../guides/observability.md) before rerunning
the installer with the same settings.

## Remove the evaluation deployment

After saving any data you need, confirm the context and use external mode
for cleanup:

```bash
kubectl config current-context
EXTERNAL_CLUSTER=true make teardown-infra
```

Keep `EXTERNAL_OVERLAY` set to the overlay used for deployment. Cleanup
removes the installed stack and its data while retaining the Kubernetes
cluster. Review the [teardown scope](../reference/infrastructure/e2e-deployment.md#make-teardown-infra)
before using it on a cluster with other workloads.
