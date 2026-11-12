# Lab environments

[`metal-stack/`](metal-stack/README.md) adapts the development stack to an
existing metal-stack/Gardener cluster. It has explicit storage, networking,
gateway, observability and hypervisor assumptions; it is not a generic
production profile.

For a different deployment environment, start with the
[reusable base catalog](../bases/README.md). For an overlay consumed by
`EXTERNAL_CLUSTER=true make deploy-infra`, preserve the existing
`EXTERNAL_OVERLAY` interface described in the
[metal-stack README](metal-stack/README.md) and
[deployment script](../../hack/deploy-infra.sh).
