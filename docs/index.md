---
layout: home
hero:
  name: CobaltCore Forge
  text: Kubernetes-native OpenStack
  tagline: Operator-driven OpenStack distribution for Hosted Control Planes
  actions:
    - theme: brand
      text: Quick Start
      link: /quick-start
    - theme: alt
      text: Reference Docs
      link: /reference/keystone/keystone-crd
features:
  - title: Operators
    details: Service operators following a shared sub-reconciler pattern, with Keystone as the reference implementation and c5c3-operator as the ControlPlane orchestration layer.
  - title: Backend & Shared Library
    details: Common types, conditions, config rendering, and Kubernetes helpers in internal/common/, plus Helm chart, operator packaging, and rotation scripts.
  - title: Infrastructure Stack
    details: Declarative FluxCD HelmReleases for OpenBao HA, External Secrets Operator, MariaDB, and Memcached.
  - title: CI/CD & Container Images
    details: GitHub Actions workflows for CI and image builds, plus multi-stage builds for OpenStack service images, Tempest, python-base, and the venv-builder.
  - title: Test Suites
    details: Unit, envtest integration, Chainsaw E2E, Tempest, and Chaos Mesh coverage across the stack.
  - title: Day 2 Operations
    details: Guides for observability, key rotation, multi-tenant deployment, and advanced configuration.
  - title: Maintenance & Dependencies
    details: <a href="/forge/contributing/dependency-management">Dependency Management</a> — Go version upgrades, library updates, and the Renovate workflow.
---
