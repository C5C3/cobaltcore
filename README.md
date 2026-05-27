# CobaltCore Forge

Prototyping ground for CobaltCore.

## Overview

CobaltCore (C5C3) is a Kubernetes-native OpenStack distribution for operating Hosted Control Planes across
a multi-cluster topology (Management, Control Plane, Hypervisor, Storage). This repository delivers everything
needed for a fully self-contained Keystone deployment stack — from infrastructure deployment manifests through
the Keystone Operator to the c5c3-operator orchestration layer — built with Operator SDK (Go), controller-runtime,
and Kubebuilder.

The implementation follows a Keystone-first strategy: the Keystone Operator serves as the reference implementation
establishing patterns for all subsequent operators. The c5c3-operator is implemented with Keystone-only orchestration,
ready to be extended for additional services later.

The architecture is organized as a Go Workspace monorepo with a shared library (`internal/common/`), individual
operator modules (`operators/keystone/`, `operators/c5c3/`), container image builds (`images/`), declarative
infrastructure deployment manifests (`deploy/`), and comprehensive tests at every level (unit, envtest integration,
Chainsaw E2E).

## Roadmap

Outstanding work is tracked in [GitHub Issues](https://github.com/c5c3/forge/issues) — the issue
tracker is the single source of truth for planned features (`CC-NNNN` labels), production-hardening
gaps, and release milestones. See the architecture handbook under [`architecture/docs/`](architecture/docs/)
for the design context behind individual feature IDs.
