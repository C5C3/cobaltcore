---
title: Quick Start
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Quick Start

Deploy CobaltCore on your computer, sign in to OpenStack, and open its dashboard.
This walkthrough uses Docker, a local kind Kubernetes cluster, and port `8443`.

CobaltCore manages OpenStack services through a Kubernetes resource called a
`ControlPlane`. You declare the services you need; CobaltCore creates and
maintains them and their supporting infrastructure.

This first deployment enables Keystone for identity and Horizon for the
dashboard. It uses the `Minimal` sizing profile for a single-node evaluation.
Add networking, images, and compute with [Deploy a ControlPlane](./quick-start-controlplane.md)
after verifying this deployment.

## Before you start

- Run Docker Desktop on macOS or Docker Engine on Linux.
- Allow at least 4 CPU cores, 16 GB of memory, and 10 GB of free disk for the lab.
- Install Git, Make, `jq`, [Mike Farah's `yq`](https://github.com/mikefarah/yq)
  v4.40.1 or newer, and the [OpenStack CLI](https://docs.openstack.org/python-openstackclient/latest/).
- Keep local ports `8443` and `8428` free and allow internet access for image downloads.

Start with no existing kind cluster named `cobaltcore`. If you already have a
local deployment, follow [Deploy on Local Kubernetes](./deployment/local-kubernetes.md)
before changing its mode. This walkthrough creates a local evaluation cloud;
running virtual machines requires compute hosts, covered by
[Deploy on Bare Metal](./quick-start-metal-stack.md).

## 1. Get CobaltCore

```bash
git clone https://github.com/c5c3/cobaltcore.git
cd cobaltcore
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"
```

The installer supplies pinned versions of kind, kubectl, and Chainsaw.
Run the remaining commands from this repository directory.

## 2. Deploy

```bash
EXTERNAL_CLUSTER=false KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true WITH_CONTROLPLANE_CR=true make deploy-infra
kubectl config use-context kind-cobaltcore
kubectl wait controlplane/controlplane -n openstack \
  --for=condition=Ready --timeout=15m
```

The command creates Kubernetes, installs CobaltCore and its dependencies, and
applies the bundled `ControlPlane`. It also configures credentials and the
public API address. Initial downloads and startup can take several minutes.

If a download is interrupted after cluster creation, run
`kubectl config use-context kind-cobaltcore` before repeating the deployment
command with the same flags. For a readiness failure, inspect the resource
before retrying:

```bash
kubectl describe controlplane controlplane -n openstack
kubectl get pods -n openstack
```

See [Diagnose a Deployment](./guides/observability.md) for further checks.

## 3. Verify API access

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret controlplane-keystone-admin-credentials \
  -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
openstack --insecure token issue
```

Success returns a token with `expires`, `id`, `project_id`, and `user_id` fields.
This verifies authentication through the public API. `--insecure` accepts the
self-signed certificate used by this local lab.

The `*.127-0-0-1.nip.io` hostnames must resolve to `127.0.0.1`. If your network
blocks them, use the [local DNS troubleshooting steps](./deployment/local-kubernetes.md).

## 4. Open the dashboard

Open [Horizon](https://horizon.127-0-0-1.nip.io:8443/), accept the lab certificate,
and sign in with domain `Default`, user `admin`, and the password held in
`OS_PASSWORD`. Retrieve that password when needed:

```bash
printf '%s\n' "$OS_PASSWORD"
```

The dashboard and successful token request confirm your first CobaltCore
deployment works. Compute, networking, images, and volumes become available
as you enable those services.

## Next steps

- [Deploy a ControlPlane](./quick-start-controlplane.md): add OpenStack services to this deployment.
- [Provision Resources](./quick-start-orders.md): request service identities, databases, and credentials.
- [Deploy on an Existing Kubernetes Cluster](./deployment/existing-kubernetes.md): prepare a cluster outside your workstation.

## Remove the local deployment

This deletes the `cobaltcore` kind cluster and all data in it:

```bash
EXTERNAL_CLUSTER=false make teardown-infra
```
