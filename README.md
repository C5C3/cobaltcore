# CobaltCore

Kubernetes-native operators and deployment stack for OpenStack Hosted Control Planes.

**Project website:** [cobaltcore.dev](https://cobaltcore.dev) ·
**Documentation:** [c5c3.github.io/cobaltcore](https://c5c3.github.io/cobaltcore/) ·
**Issues:** [github.com/c5c3/cobaltcore/issues](https://github.com/c5c3/cobaltcore/issues)

## Overview

CobaltCore (C5C3) is a Kubernetes-native OpenStack distribution for operating Hosted Control Planes. The
project website at [cobaltcore.dev](https://cobaltcore.dev) introduces CobaltCore as a whole. This repository
holds its control-plane stack: the declarative infrastructure manifests, one operator per OpenStack service,
and the c5c3-operator, which orchestrates the service operators as children of a single `ControlPlane`
resource. The operators are written in Go with the Operator SDK, controller-runtime, and Kubebuilder.

The Keystone operator is the reference implementation. Its CRD layout, sub-reconciler chain, webhooks,
finalizers, and instrumentation are the patterns every other service operator follows. How the pieces fit
together, including the implemented management/target-cluster topology, is described in
[the architecture documentation](docs/architecture/index.md).

## Services

| Operator | OpenStack service | Managed by the `ControlPlane` |
| --- | --- | --- |
| [Keystone](docs/reference/keystone/index.md) | Identity (reference implementation) | yes |
| [Horizon](docs/reference/horizon/index.md) | Dashboard | yes |
| [Glance](docs/reference/glance/index.md) | Image | yes |
| [Placement](docs/reference/placement/index.md) | Placement | yes |
| [Barbican](docs/reference/barbican/index.md) | Key manager | yes |
| [Neutron](docs/reference/neutron/index.md) | Networking (ML2/OVN) | yes |
| [OVN](docs/reference/ovn/index.md) | The SDN layer Neutron programs | referenced from the Neutron block |
| [Cinder](docs/reference/cinder/index.md) | Block storage | yes |
| [Nova](docs/reference/nova/index.md) | Compute control plane | not yet, tracked in [#1019](https://github.com/c5c3/cobaltcore/issues/1019) |

The [c5c3-operator](docs/reference/c5c3/controlplane-crd.md) reconciles the `ControlPlane` and projects each
enabled service block onto a child CR of the matching service operator.

The service images are built for every OpenStack release defined under [`releases/`](releases/), currently
2025.2 and 2026.1.

## Getting started

The quick starts run on a local kind cluster and need Docker Desktop or Podman:

- [Quick Start](docs/quick-start.md): from `git clone` to an authenticated Keystone API call.
- [Quick Start (Extended)](docs/quick-start-extended.md): UI tours, the local-build path, the production
  HelmRelease, E2E, and Tempest.
- [Quick Start (ControlPlane)](docs/quick-start-controlplane.md): a full ControlPlane through the
  c5c3-operator.

The short path to the infrastructure stack:

```bash
git clone https://github.com/c5c3/cobaltcore.git
cd cobaltcore
make install-test-deps
export PATH="${HOME}/.local/bin:${PATH}"
make deploy-infra
```

`make deploy-infra` creates the `cobaltcore` kind cluster and installs the infrastructure stack, including
Flux, cert-manager, OpenBao, the MariaDB and Memcached operators, External Secrets, and the Envoy Gateway.
`make teardown-infra` deletes the cluster again.

## Repository layout

| Path | Contents |
| --- | --- |
| [`operators/`](operators/) | One Go module per operator, each with its API types, controllers, webhooks, and Helm chart, plus the shared `operator-library` chart |
| [`internal/common/`](internal/common/) | The shared library: common types, conditions, config rendering, Kubernetes helpers |
| [`images/`](images/) | Multi-stage builds for the OpenStack service images, Tempest, and the python-base / venv-builder layers |
| [`releases/`](releases/) | Per-release source refs, upper constraints, extra packages, and Tempest excludes |
| [`patches/`](patches/) | Patches applied to the upstream OpenStack sources at image build time |
| [`deploy/`](deploy/) | FluxCD HelmReleases and kind overlays for the infrastructure stack and the operators |
| [`tests/`](tests/) | Chainsaw E2E, chaos, multi-cluster, operator-upgrade, and Tempest suites |
| [`docs/`](docs/) | The VitePress sources of the [documentation site](https://c5c3.github.io/cobaltcore/) |
| [`hack/`](hack/) | Scripts behind the Makefile targets and the CI workflows |

The operators and `internal/common/` form one Go workspace ([`go.work`](go.work)). `make test` runs the unit
tests, `make test-integration` the envtest suites, and `make lint` the linters.

## Documentation

The documentation is published at [c5c3.github.io/cobaltcore](https://c5c3.github.io/cobaltcore/) from the
sources under [`docs/`](docs/):

- [Architecture](docs/architecture/index.md): the implemented topology and the
  [core components](docs/architecture/core-components.md).
- [Guides](docs/guides/observability.md): day-2 operations, key rotation, multi-tenant deployment, identity
  backends, storage backends, and target clusters.
- [Reference](docs/reference/keystone/index.md): CRDs, reconcilers, events, metrics, and the infrastructure,
  CI/CD, and testing internals.
- [Future](docs/future/index.md): idea sketches for where the operators could go next.

## Contributing

The [contributing section](docs/contributing/adding-a-new-operator.md) covers onboarding a new operator or
OpenStack release, dependency management, the guide conventions, and the
[Nix development environment](docs/contributing/nix-dev-environment.md). Documentation prose
follows [STYLE_GUIDE.md](STYLE_GUIDE.md).

## Roadmap

Outstanding work is tracked in [GitHub Issues](https://github.com/c5c3/cobaltcore/issues). The issue tracker
is the single source of truth for planned features, production-hardening gaps, and release milestones.

## Security

Found a vulnerability? Please report it privately through GitHub Private Vulnerability Reporting rather than
opening a public issue. See [SECURITY.md](SECURITY.md) for the reporting process, scope, and response
expectations.

## License

CobaltCore is licensed under the [Apache License 2.0](LICENSE). Source files carry SPDX license headers.
