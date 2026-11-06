---
title: Run E2E Tests
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Run E2E Tests

Use end-to-end tests to check that CobaltCore creates working OpenStack
services and handles changes to them. Chainsaw tests exercise Kubernetes
resources and their controllers. Tempest tests exercise the OpenStack APIs.

Run these tests on a disposable cluster. Suites create, change, and delete
resources; some change shared infrastructure or stop workloads.

## Prepare a test cluster

For a first operator test, follow
[Deploy Keystone Locally](../quick-start-extended.md) on a fresh kind cluster.
It installs the standalone infrastructure and Keystone operator needed by
the example below. Keep it separate from the ControlPlane deployment in the
Quick Start.

Run commands from the repository root. `make install-test-deps` installs
the pinned Chainsaw binary alongside kind and kubectl:

```bash
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"
kubectl config current-context
kubectl get pods -n keystone-system
```

Confirm that the context names your test cluster and the operator is running.

## Verify Keystone reconciliation

Run one suite:

```bash
mkdir -p _output/reports
chainsaw test \
  --config tests/e2e/chainsaw-config.yaml \
  tests/e2e/keystone/basic-deployment/
```

This creates a Keystone instance, waits for its conditions to become ready,
checks its generated resources, and calls the identity API. A successful run
ends with no failed tests. Chainsaw removes its test resources afterward.

JUnit XML reports are written to `_output/reports/`. On a failure, keep the
terminal output and inspect the failing resource using
[Observability and Diagnostics](../guides/observability.md).

## Test other behavior

Choose a suite whose prerequisites match your cluster:

| Goal | Suite or target | Preparation |
| --- | --- | --- |
| Check scaling, key rotation, or recovery | `tests/e2e/keystone/` | [Keystone test reference](../reference/testing/keystone-e2e-tests.md) |
| Check a ControlPlane and its identity resources | `make e2e-controlplane` | [ControlPlane test reference](../reference/testing/controlplane-e2e-tests.md) |
| Check Prometheus integration | `make e2e-prometheus` | Deploy with `WITH_PROMETHEUS=true` |
| Check recovery from failures | `make e2e-chaos` | [Chaos test prerequisites](../reference/testing/chaos-e2e-tests.md), including `WITH_CHAOS_MESH=true` |
| Check an operator upgrade | `make e2e-operator-upgrade` | [Upgrade test reference](../reference/testing/operator-upgrade-e2e-tests.md); the suite installs its own operator |

`make e2e` discovers every suite under `tests/e2e/`, with up to four running
in parallel. It does not install their operators or optional dependencies.
Use the service references and CI setup when preparing a cluster for that
full run; a basic Keystone installation is insufficient.

The `gateway-quick-start*` suites use the fixed host URL
`https://keystone.127-0-0-1.nip.io/v3` on port `443`. To run them, create a
fresh test cluster with that host port and a runtime allowed to bind it.
The Quick Start's `8443` mapping and a port-forward to the Keystone Service
do not exercise that path.

## Verify the OpenStack identity API with Tempest {#run-tempest}

On the standalone Keystone cluster, use the published test image:

```bash
BUILD_IMAGE=false \
TEMPEST_IMAGE=ghcr.io/c5c3/tempest:2026.1 \
SERVICE=keystone \
hack/run-tempest.sh
```

The runner needs Docker, kubectl, yq, curl, and `timeout` on `PATH`. On
macOS, install GNU coreutils and make its `timeout` command available.
It reads the admin password from `openstack/keystone-admin`, forwards the
Keystone Service to port `5000`, and runs the configured identity tests.
Leave port `5000` free so it reaches the intended service.

The command above targets the standalone Service named `keystone`. For a
ControlPlane, first identify its Keystone Service and admin Secret, then
set `SERVICE_NAME` and `ADMIN_SECRET` accordingly. See the
[Tempest reference](../reference/testing/tempest-test-infrastructure.md)
for service-specific configurations.

Results are stored in `_output/tempest/`:

| File | Use |
| --- | --- |
| `tempest-results.xml` | Review JUnit results in an IDE or CI viewer |
| `tempest.subunit` | Inspect the raw test-result stream |

Building the test image locally requires Nix on Linux. Use
[Build and Test Locally](../contributing/build-and-test-locally.md)
when changing an operator or service image.
