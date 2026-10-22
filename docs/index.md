---
title: Overview
---

# CobaltCore

CobaltCore runs OpenStack on Kubernetes. Use it to provide a cloud with
identity, a dashboard, images, networking, compute, and block storage, managed
through Kubernetes resources.

Declare a `ControlPlane` with the OpenStack services you want. CobaltCore
deploys those services, connects them to their databases and credentials, and
keeps the running configuration aligned with your declaration.

Kubernetes hosts the control plane: the APIs and supporting services that
manage your cloud. Compute hosts provide the capacity to run virtual machines.
You can evaluate the APIs locally before connecting real compute infrastructure.

## Getting started

**[Quick Start: deploy CobaltCore and verify it works](./quick-start.md)**
creates a local cloud with identity and a dashboard. Follow one path from
installation to a successful sign-in.

## Deployment guides

Choose a guide when you need to configure the deployment environment.

| Environment | Guide |
| --- | --- |
| Your computer | [Deploy on Local Kubernetes (kind)](./deployment/local-kubernetes.md) |
| A Kubernetes cluster you already manage | [Deploy on an Existing Kubernetes Cluster](./deployment/existing-kubernetes.md) |
| KVM workers on a Gardener / metal-stack cluster | [Deploy on Bare Metal](./quick-start-metal-stack.md) |

## Tutorials

- [Deploy a ControlPlane](./quick-start-controlplane.md): enable and verify more OpenStack services.
- [Provision Resources](./quick-start-orders.md): give a service its own identity, database, and messaging credentials. CobaltCore calls these resource requests **Orders**.
- [Run E2E Tests](./tutorials/run-e2e-tests.md): check deployment behavior with the test suites.
- [Configure Your Deployment](./guides/advanced-configuration.md): adjust sizing, connect existing infrastructure, and configure service behavior.

## Operate your cloud

- [Diagnose a Deployment](./guides/observability.md): inspect readiness, logs, and metrics.
- [Maintain Your Cloud](./guides/day-2-operations.md): scale services, upgrade, and rotate credentials.
- [Connect a Compute Cluster](./guides/nova/connect-a-compute-cluster.md): attach compute capacity.

Read the [architecture](./architecture/) to understand how CobaltCore manages
services across clusters. The [API reference](./reference/c5c3/controlplane-crd.md)
describes the available configuration fields.

## Contribute to CobaltCore

For operator development, start with [Build and Test Locally](./contributing/build-and-test-locally.md)
or [Add an Operator](./contributing/adding-a-new-operator.md).
Product background is available at [cobaltcore.dev](https://cobaltcore.dev).
