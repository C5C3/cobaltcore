---
title: Overview
---

# CobaltCore

CobaltCore (C5C3) is a Kubernetes-native OpenStack distribution for operating
Hosted Control Planes. The project website at
[cobaltcore.dev](https://cobaltcore.dev) introduces CobaltCore as a whole.
This documentation covers the
[c5c3/cobaltcore](https://github.com/C5C3/cobaltcore) repository, which
delivers everything needed for a self-contained OpenStack control-plane stack:
from the declarative infrastructure manifests, through the service operators,
to the c5c3-operator orchestration layer — built with the Operator SDK (Go),
controller-runtime, and Kubebuilder. The [Architecture](./architecture/)
section shows how the pieces fit together, from the implemented
management/target-cluster topology to the multi-cluster picture of the
original design.

The Keystone operator is the reference implementation that establishes the
patterns — CRD layout, sub-reconciler chain, webhooks, finalizers,
instrumentation — replicated by every other service operator. Horizon, Glance,
Placement, Barbican, Cinder, Nova, and Neutron with its OVN layer are onboarded
on the same scaffolding. The c5c3-operator ties the services together into a
single ControlPlane resource, which projects Nova through `services.nova`. The
compute nodes that join it are the follow-on
[#1013](https://github.com/C5C3/cobaltcore/issues/1013).

## Start here

- **[Quick Start](./quick-start.md)** — from `git clone` to an authenticated
  Keystone API call on a local kind cluster.
- **[Quick Start (Extended)](./quick-start-extended.md)** — UI tours, the
  local-build path, the production HelmRelease, E2E, and Tempest.
- **[Quick Start (ControlPlane)](./quick-start-controlplane.md)** — bring up a
  full ControlPlane through the c5c3-operator.

## What's inside

- **Operators.** Service operators following a shared sub-reconciler pattern,
  with [Keystone](./reference/keystone/) as the reference implementation and the
  [c5c3-operator](./reference/c5c3/controlplane-crd.md) as the ControlPlane
  orchestration layer. [Horizon](./reference/horizon/),
  [Glance](./reference/glance/), [Placement](./reference/placement/),
  [Barbican](./reference/barbican/), [Cinder](./reference/cinder/),
  [Nova](./reference/nova/), and [Neutron](./reference/neutron/) are onboarded
  on the same conventions; the [OVN operator](./reference/ovn/) runs the SDN
  layer Neutron programs.
- **Shared library.** Common types, conditions, config rendering, and
  Kubernetes helpers in `internal/common/`, plus the Helm chart, operator
  packaging, and rotation scripts. See the
  [Backend reference](./reference/backend/helm-values-schema.md).
- **Infrastructure stack.** Declarative FluxCD manifests for OpenBao HA,
  External Secrets Operator, MariaDB, Memcached, the Envoy Gateway, and the
  RabbitMQ Cluster Operator behind the message bus. See
  [Infrastructure Manifests](./reference/infrastructure/infrastructure-manifests.md).
- **CI/CD & container images.** GitHub Actions for CI and image builds, plus
  multi-stage builds for the OpenStack service images, Tempest, and the
  python-base / venv-builder layers. See the
  [CI Workflow](./reference/ci-cd/ci-workflow.md).
- **Test suites.** Unit, envtest integration, Chainsaw E2E, Tempest, and Chaos
  Mesh coverage across the stack — see the
  [Testing reference](./reference/testing/keystone-e2e-tests.md).

## Go deeper

- **[Architecture](./architecture/)** — the implemented topology, the layering
  from Flux stack to ControlPlane orchestration, and the
  [Core Components](./architecture/core-components.md) catalog.
- **[Guides](./guides/observability.md)** — day-2 operations, key rotation,
  multi-tenant deployment, identity and storage backends, target clusters, and
  advanced configuration.
- **[Reference](./reference/keystone/)** — CRDs, reconciler architecture,
  controller events, metrics, and the infrastructure, CI/CD, and testing
  internals.
- **[Future](./future/)** — idea sketches for where the operators could go next.
- **[Contributing](./contributing/adding-a-new-operator.md)** — onboard a new
  operator or release, follow the guide conventions, and set up the development
  environment.

For an AI-assisted tour of the codebase, browse the repository on
[DeepWiki](https://deepwiki.com/C5C3/cobaltcore).
