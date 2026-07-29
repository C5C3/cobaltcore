---
title: Overview
---

# CobaltCore Forge

CobaltCore Forge helps teams run OpenStack control planes on Kubernetes.

This repository includes deployment manifests, Kubernetes operators, and tested
workflows for running the stack locally and in CI.

You can complete the main quick start and make an authenticated Keystone API
call on a local kind cluster.

## Who this is for

- Platform engineers and site reliability engineers (SREs) managing
  Kubernetes-based control planes.
- OpenStack operators.
- Developers contributing to Kubernetes operators.

## Start here

If you open only one page first, open **[Quick Start](./quick-start.md)**.

- **[Quick Start](./quick-start.md)** — Path from clone to an
  authenticated Keystone API call on a local kind cluster.
- **[Quick Start (ControlPlane)](./quick-start-controlplane.md)** — bring up a
  full ControlPlane through the c5c3-operator after the main quick start.
- **[Quick Start (Extended)](./quick-start-extended.md)** *(optional)* — UI
  tours, local-build path, production HelmRelease, E2E, and Tempest.

## Architecture at a glance

CobaltCore (C5C3) operates hosted control planes across a multi-cluster
topology: Management, Control Plane, Hypervisor, and Storage.

The architecture follows a Keystone-first strategy. CobaltCore delivers a
self-contained deployment stack, from declarative infrastructure
manifests through service operators to c5c3-operator orchestration, built with
Operator SDK (Go), controller-runtime, and Kubebuilder.

- **Operators.** Service operators following a shared sub-reconciler pattern,
  with [Keystone](./reference/keystone/) as the reference implementation and the
  [c5c3-operator](./reference/c5c3/controlplane-crd.md) as the ControlPlane
  orchestration layer. [Horizon](./reference/horizon/) and
  [Glance](./reference/glance/) are onboarded on the same conventions.
- **Operator pattern details.** The Keystone reference pattern includes CRD
  layout, sub-reconciler chain, webhooks, finalizers, and instrumentation,
  replicated by subsequent service operators.
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
