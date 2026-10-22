# CobaltCore

Kubernetes-native operators and deployment stack for OpenStack Hosted Control Planes.

**Project website:** [cobaltcore.dev](https://cobaltcore.dev) ·
**Documentation:** [c5c3.github.io/cobaltcore](https://c5c3.github.io/cobaltcore/) ·
**Issues:** [github.com/c5c3/cobaltcore/issues](https://github.com/c5c3/cobaltcore/issues)

## Overview

CobaltCore runs OpenStack on Kubernetes. Declare a `ControlPlane` with the services you need;
CobaltCore deploys them, connects their databases and credentials, and maintains their configuration.

Start with identity and a dashboard on your computer, then add images, networking, compute, and storage.
Kubernetes hosts the APIs; compute hosts provide capacity for virtual machines. See the
[architecture documentation](docs/architecture/index.md) for how the components fit together.

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
| [Nova](docs/reference/nova/index.md) | Compute control plane | yes |

The [c5c3-operator](docs/reference/c5c3/controlplane-crd.md) reconciles the `ControlPlane` and projects each
enabled service block onto a child CR of the matching service operator.

The service images are built for every OpenStack release defined under [`releases/`](releases/), currently
2026.1 and 2026.2.

## Getting started

Follow the **[Quick Start](docs/quick-start.md)** to deploy a local CobaltCore cloud, verify API access,
and sign in to the dashboard. It uses Docker, kind, and the `Minimal` sizing profile.

### Deployment guides

- [Deploy on Local Kubernetes (kind)](docs/deployment/local-kubernetes.md)
- [Deploy on an Existing Kubernetes Cluster](docs/deployment/existing-kubernetes.md)
- [Deploy on Bare Metal (Gardener / metal-stack)](docs/quick-start-metal-stack.md)

### Tutorials

- [Deploy a ControlPlane](docs/quick-start-controlplane.md): enable more OpenStack services.
- [Provision Resources](docs/quick-start-orders.md): create service identities, databases, and credentials using Orders.
- [Run E2E Tests](docs/tutorials/run-e2e-tests.md): verify deployment behavior.
- [Configure Your Deployment](docs/guides/advanced-configuration.md): adjust sizing and connect existing infrastructure.

For contributor setup and local image builds, see [Build and Test Locally](docs/contributing/build-and-test-locally.md).

## Repository layout

| Path | Contents |
| --- | --- |
| [`operators/`](operators/) | One Go module per operator, each with its API types, controllers, webhooks, and Helm chart, plus the shared `operator-library` chart |
| [`internal/common/`](internal/common/) | The shared library: common types, conditions, config rendering, Kubernetes helpers |
| [`images/`](images/) | The Nix expressions of the OpenStack service images, nova-compute, libvirt, Tempest, the federation proxy, ovn, the backup shifter and the two upstream Go agents |
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
