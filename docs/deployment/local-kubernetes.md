---
title: Deploy on Local Kubernetes (kind)
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Deploy on Local Kubernetes (kind)

Run CobaltCore on your workstation to explore its APIs and dashboard. The
[Quick Start](../quick-start.md) creates a kind cluster with a small
ControlPlane, Keystone identity service, and Horizon dashboard. Start there
for your first deployment.

Use this guide to adapt the local runtime, ports, or capacity. For the full
set of OpenStack services, continue with
[Deploy a ControlPlane](../quick-start-controlplane.md).

## Prepare your workstation

Use a running Docker daemon, with Docker Desktop on macOS. Allocate at least
4 CPU cores, 16 GB RAM, and 10 GB free disk for the ControlPlane walkthrough.
The standalone [Deploy Keystone Locally](../quick-start-extended.md) guide
needs at least 2 CPU cores and 8 GB RAM.

Install `git`, `make`, `jq`, and Mike Farah's `yq` v4.40.1 or newer. From the
repository, install the pinned kind, kubectl, and Chainsaw binaries:

```bash
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"
```

The installer does not install Docker, Helm, jq, or yq. Helm is needed when
installing a local chart; the Quick Start uses Flux to install published
charts. The Flux CLI is optional. Contributors can use the
[Nix development environment](../contributing/nix-dev-environment.md).

### Use Podman

kind can use Podman when you set:

```bash
export KIND_EXPERIMENTAL_PROVIDER=podman
```

The repository's deployment scripts also call `docker`, including
`docker info` and node-container commands. A Podman setup therefore needs a
working Docker-compatible command and endpoint for the same runtime that kind
uses. Setting the provider variable alone is insufficient. Use Docker for
the documented default path.

## Choose the host port

The Quick Start sets `KIND_HOST_PORT=8443`. This avoids the privileged port
binding required by the repository default, `443`. Both settings forward to
the Envoy proxy's NodePort `31443` inside the cluster.

```bash
export KIND_HOST_PORT=8443
```

Set the variable before creating the cluster. Changing it later does not
change an existing kind port mapping. Port `8428` must also be free: the
kind configuration reserves it for the optional load-testing stack.

With `WITH_CONTROLPLANE_CR=true`, deployment adds the selected port to the
bundled ControlPlane's public URLs. When writing your own manifest, include
the port in each service's `publicEndpoint`; keep the Gateway hostname free
of a port suffix.

### Resolve the local endpoint names

The names under `127-0-0-1.nip.io` resolve to your workstation. If your
resolver blocks that address, bypass DNS for one API check:

```bash
curl -fk --resolve keystone.127-0-0-1.nip.io:8443:127.0.0.1 \
  https://keystone.127-0-0-1.nip.io:8443/v3
```

If this succeeds while a normal request cannot resolve the hostname, add
these entries to your workstation's `/etc/hosts` with administrator access:

```text
127.0.0.1 keystone.127-0-0-1.nip.io
127.0.0.1 horizon.127-0-0-1.nip.io
```

Then retry the normal API URL and dashboard. Add further service hostnames
only when you deploy those services.

## Keep deployment modes separate

A ControlPlane manages its own database and cache. A standalone Keystone
deployment uses infrastructure created directly by the deployment script.
Switching between them requires a fresh cluster.

To replace a disposable local deployment, confirm the cluster name and then
remove it:

```bash
kind get clusters
EXTERNAL_CLUSTER=false make teardown-infra
```

This deletes the `cobaltcore` kind cluster and its data. Then follow either
the [Quick Start](../quick-start.md) for a ControlPlane or
[Deploy Keystone Locally](../quick-start-extended.md) for a standalone service.
Do not run the standalone guide over the Quick Start cluster.

## Add capacity or optional services

The bundled ControlPlane uses the `Minimal` sizing profile. Keep that profile
for a laptop. The `Standard` profile requests a larger database and more
replicas; see [Sizing](../reference/c5c3/controlplane-crd.md#built-in-profiles) before changing it.

For several Kubernetes nodes, create a fresh cluster with
`KIND_CONFIG=hack/kind-config-multinode.yaml` when running the deployment
command. A new config does not modify an existing cluster.

Optional services are enabled with environment variables passed to
`make deploy-infra`:

| Goal | Setting |
| --- | --- |
| Inspect operator metrics in Grafana | `WITH_PROMETHEUS=true` |
| Test horizontal autoscaling | `WITH_METRICS_SERVER=true` |
| Test vertical autoscaling | `WITH_VPA=true` (also installs metrics-server) |
| Run failure-injection tests | `WITH_CHAOS_MESH=true` |
| Reuse downloaded images across local clusters | `WITH_REGISTRY_CACHE=true` on a fresh cluster |

Preserve the settings used for the original deployment when rerunning the
command. Removing a flag does not uninstall its components. The
[deployment reference](../reference/infrastructure/e2e-deployment.md#environment-variables)
lists all settings and timeouts.

## Verify your changes

Use the authentication and dashboard checks in the
[Quick Start](../quick-start.md). For operator tests or Tempest API checks,
follow [Run E2E Tests](../tutorials/run-e2e-tests.md) on a disposable cluster.
