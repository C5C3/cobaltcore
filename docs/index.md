---
title: Overview
---

# CobaltCore Forge

CobaltCore (C5C3) is a Kubernetes-native OpenStack distribution for operating
Hosted Control Planes across a multi-cluster topology — Management, Control
Plane, Hypervisor, and Storage. This repository delivers everything needed for
a self-contained Keystone deployment stack: from the declarative infrastructure
manifests, through the service operators, to the c5c3-operator orchestration
layer — built with the Operator SDK (Go), controller-runtime, and Kubebuilder.

The implementation follows a Keystone-first strategy. The Keystone operator is
the reference implementation that establishes the patterns — CRD layout,
sub-reconciler chain, webhooks, finalizers, instrumentation — replicated by
every subsequent service operator. Horizon, Glance, and Placement are already
onboarded on the same scaffolding, and the c5c3-operator ties the services
together into a single ControlPlane resource.

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
  [Glance](./reference/glance/), and [Placement](./reference/placement/) are
  onboarded on the same conventions.
- **Shared library.** Common types, conditions, config rendering, and
  Kubernetes helpers in `internal/common/`, plus the Helm chart, operator
  packaging, and rotation scripts. See the
  [Backend reference](./reference/backend/helm-values-schema.md).
- **Infrastructure stack.** Declarative FluxCD HelmReleases for OpenBao HA,
  External Secrets Operator, MariaDB, Memcached, and the Envoy Gateway. See
  [Infrastructure Manifests](./reference/infrastructure/infrastructure-manifests.md).
- **CI/CD & container images.** GitHub Actions for CI and image builds, plus
  multi-stage builds for the OpenStack service images, Tempest, and the
  python-base / venv-builder layers. See the
  [CI Workflow](./reference/ci-cd/ci-workflow.md).
- **Test suites.** Unit, envtest integration, Chainsaw E2E, Tempest, and Chaos
  Mesh coverage across the stack — see the
  [Testing reference](./reference/testing/keystone-e2e-tests.md).

## Go deeper

- **[Guides](./guides/observability.md)** — day-2 operations, key rotation,
  multi-tenant deployment, identity backends, and advanced configuration.
- **[Reference](./reference/keystone/)** — CRDs, reconciler architecture,
  controller events, metrics, and the infrastructure and CI/CD internals.
- **[Future](./future/)** — idea sketches for where the operators could go next.
- **[Contributing](./contributing/adding-a-new-operator.md)** — onboard a new
  operator or release, follow the guide conventions, and set up the development
  environment.

For an AI-assisted tour of the codebase, browse the repository on
[DeepWiki](https://deepwiki.com/C5C3/forge).
