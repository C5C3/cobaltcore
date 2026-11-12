# Existing aggregate deployment entry points

This directory retains the broad composition used by existing overlays and
scripts. For a new deployment repository, use the
[public base catalog](../bases/README.md) to select separate lifecycle stages.

| Entry point | Contents and prerequisites |
| --- | --- |
| [`./`](.) | Namespaces, FluxInstance, chart/Git/OCI sources, operator reconciliation objects, shared OpenBao and the c5c3 operator |
| [`infrastructure/`](infrastructure/) | CRD-dependent TLS, static MariaDB/MaxScale, Memcached, the development/CI Garage reference, and the shared ESO store |
| [`infrastructure/tls/`](infrastructure/tls/) | Issuers and certificates for staged consumers |
| [`releases/openbao/`](releases/openbao/) | Shared OpenBao release, retaining the aggregate's storage default |
| [`releases/c5c3-operator/`](releases/c5c3-operator/) | The c5c3 operator release |

The aggregate requires flux-operator and Flux APIs before its resources can be
applied. Infrastructure requires the relevant operators, CRDs and admission
paths. Shared OpenBao additionally needs initialization/unseal and configuration.
The deployment script applies an early TLS subset before later infrastructure
phases; preserve that ordering rather than treating this tree as one recursive
apply.

“Production” in older comments distinguishes the unscaled reference from kind
overrides. This aggregate still includes environment assumptions such as a
storage-class default and CI infrastructure; it is not a complete production
profile. See the [infrastructure reference](../../docs/reference/infrastructure/infrastructure-manifests.md)
for its resource contracts.

Sources and releases remain at their established file paths for scripts,
automation and raw-file consumers. The `releases/` directory also contains Flux
Kustomizations for Git-sourced operators, not only HelmReleases. Its operator-only
Kustomization and the source Kustomization support direct composition; shared
OpenBao and c5c3 retain separate release roots. The public operators base still
uses a bounded compatibility adapter to preserve the existing staged output.
Do not reconcile this aggregate alongside the staged bases over the same objects.

Existing kind/lab workflows continue through `make deploy-infra`. See
[deployment entry points](../README.md) and [migration notes](../MIGRATION.md).
