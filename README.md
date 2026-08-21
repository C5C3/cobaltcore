# CobaltCore

Kubernetes-native operators and deployment stack for OpenStack Hosted Control Planes.

## Overview

CobaltCore (C5C3) is a Kubernetes-native OpenStack distribution for operating Hosted Control Planes. This
repository delivers everything needed for a fully self-contained OpenStack control-plane stack — from
infrastructure deployment manifests through the service operators to the c5c3-operator orchestration layer —
built with Operator SDK (Go), controller-runtime, and Kubebuilder. How the pieces fit together, including the
implemented management/target-cluster topology, is described in [the architecture documentation](docs/architecture/index.md).

The Keystone Operator is the reference implementation: it establishes the patterns every service operator
follows. Horizon, Glance, Placement, and Barbican are built on the same scaffolding, and the c5c3-operator
orchestrates all of them as children of one ControlPlane resource.

The architecture is organized as a Go Workspace monorepo with a shared library (`internal/common/`), individual
operator modules (`operators/keystone/`, `operators/horizon/`, `operators/glance/`, `operators/placement/`,
`operators/barbican/`, `operators/c5c3/`), container image builds (`images/`), declarative
infrastructure deployment manifests (`deploy/`), and comprehensive tests at every level (unit, envtest integration,
Chainsaw E2E).

## Roadmap

Outstanding work is tracked in [GitHub Issues](https://github.com/c5c3/cobaltcore/issues) — the issue
tracker is the single source of truth for planned features (`CC-NNNN` labels), production-hardening
gaps, and release milestones.

## Security

Found a vulnerability? Please report it privately through GitHub Private Vulnerability
Reporting rather than opening a public issue. See [SECURITY.md](SECURITY.md) for the
reporting process, scope, and response expectations.
