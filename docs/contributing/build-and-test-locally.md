---
title: Build and Test Locally
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Build and Test Locally

Build a changed Keystone operator or service image and run it on a local
kind cluster. Image builds require Nix with flakes on Linux. On macOS, use
the published images in [Deploy Keystone Locally](../quick-start-extended.md)
or build on a Linux host.

Start with the infrastructure from Steps 1 to 3 of that guide, before
installing the published Keystone operator. The local Helm installation
below must own the release; do not also enable Flux management of it.

The [Nix development environment](./nix-dev-environment.md) provides the
pinned development tools. A running Docker daemon is still required.

## Run a changed operator {#build-operator}

Build the image and load it into the `cobaltcore` cluster:

```bash
make image OPERATOR=keystone IMG=ghcr.io/c5c3/keystone-operator:dev
kind load docker-image ghcr.io/c5c3/keystone-operator:dev --name cobaltcore
```

Prepare the chart dependencies and register the CRDs before starting the
operator:

```bash
helm dependency build --skip-refresh operators/keystone/helm/keystone-operator/
kubectl apply -f operators/keystone/helm/keystone-operator/crds/
kubectl wait crd --all --for=condition=Established --timeout=60s

helm install keystone-operator \
  operators/keystone/helm/keystone-operator/ \
  -n keystone-system --create-namespace \
  --set image.repository=ghcr.io/c5c3/keystone-operator \
  --set image.tag=dev \
  --set image.pullPolicy=Never \
  --wait --timeout=120s
```

Return to [choose the service image](../quick-start-extended.md#step-6-—-prepare-and-load-the-keystone-service-image)
and create the Keystone resource.

## Run a changed service image {#build-service-image}

Build the release used by your Keystone resource:

```bash
RELEASE=2026.1
LOCAL_TAG="${RELEASE}-dev-$(date +%s)"
nix build ".#keystone-${RELEASE//./_}-image"
./result | docker load
docker tag "keystone:${RELEASE}" "ghcr.io/c5c3/keystone:${LOCAL_TAG}"
kind load docker-image "ghcr.io/c5c3/keystone:${LOCAL_TAG}" --name cobaltcore
printf 'Local service image tag: %s\n' "$LOCAL_TAG"
```

In the Keystone manifest from Step 7 of the standalone guide, set
`spec.image.tag` to the printed local tag and add `spec.image.pullPolicy: Never`.
The operator otherwise defaults tagged service images to `Always`, which pulls
from the registry even after `kind load`. The unique tag also makes each rebuild
change the workload template and trigger a rollout when you reapply the manifest.
Load the image on every kind node before applying it. The
[image build reference](../reference/ci-cd/nix-image-build.md)
describes the release attributes and image-loading process.

## Check your changes

Run the operator's unit tests:

```bash
make test-operator OPERATOR=keystone
```

Then follow [Run E2E Tests](../tutorials/run-e2e-tests.md) to verify
reconciliation and API behavior on the test cluster.

To build the Tempest image locally before running the identity API tests:

```bash
SERVICE=keystone hack/run-tempest.sh
```

Subsequent runs can reuse that image with `BUILD_IMAGE=false`. Published
images and test outputs are covered in the E2E tutorial.
