---
title: Keystone Operator Packaging
quadrant: backend
---

# Keystone Operator Packaging

Reference documentation for the Keystone operator packaging artifacts. This covers
the operator image, Helm chart configuration, FluxCD HelmRelease integration,
dependency chain, and CRD installation behavior. These artifacts package the Keystone
operator for deployment into Kubernetes clusters via the GitOps pipeline.

## Directory Layout

```text
operators/keystone/
├── vendor.json                         Vendor hash of the operator image (make nix-lock)
├── config/rbac/role.yaml               controller-gen ClusterRole from the RBAC markers (make manifests)
├── helm/
│   └── keystone-operator/
│       ├── Chart.yaml                  Helm chart metadata
│       ├── Chart.lock                  Pinned operator-library dependency
│       ├── values.yaml                 Default configuration values
│       ├── values.schema.json          JSON Schema for values validation (generated)
│       ├── values.schema.extras.json   Chart-specific schema additions (federation)
│       ├── crds/
│       │   ├── keystone.openstack.c5c3.io_keystones.yaml   Keystone CRD (auto-installed by Helm)
│       │   └── keystone.openstack.c5c3.io_keystoneidentitybackends.yaml
│       │                                                   KeystoneIdentityBackend CRD
│       └── templates/
│           ├── _helpers.tpl            Chart-specific helpers: the operator-library.chart.* hook overrides
│           ├── _rbac-rules.tpl         RBAC rules, generated from config/rbac/role.yaml (make sync-helm-rbac)
│           ├── _webhook-rbac-rules.tpl RBAC rules of the standalone webhook, generated from the same file
│           ├── serviceaccount.yaml     ServiceAccount (conditional)
│           ├── clusterrole.yaml        ClusterRole (cluster-scoped RBAC, default)
│           ├── clusterrolebinding.yaml ClusterRoleBinding (default)
│           ├── role.yaml               Namespace-scoped Role (rbac.namespaceScoped)
│           ├── rolebinding.yaml        Namespace-scoped RoleBinding (rbac.namespaceScoped)
│           ├── deployment.yaml         Operator Deployment
│           ├── service.yaml            ClusterIP Service (webhook + metrics)
│           ├── pdb.yaml                PodDisruptionBudget (replicas > 1, podDisruptionBudget.enabled)
│           ├── certificate.yaml        Self-signed Issuer + webhook Certificate (conditional)
│           ├── networkpolicy.yaml      Operator pod NetworkPolicy (opt-in)
│           ├── servicemonitor.yaml     Prometheus ServiceMonitor (opt-in)
│           └── webhook-configuration.yaml  Mutating + Validating webhooks (conditional)
operators/shared/helm/
├── operator-library/                   Library chart: every manifest template above, defined once
└── operator-library-testbed/           Test consumer of the library; runs the shared helm-unittest suites
deploy/bases/operators/
├── kustomization.yaml                  Base kustomization (includes keystone-operator release)
└── releases/
    └── keystone-operator.yaml          FluxCD HelmRelease
```

## Image

**Location:** `nix/lib/operator-image.nix` (shared by all operators) and
`operators/keystone/vendor.json`, the vendor hash of the keystone module

The root flake builds the image as the attribute `keystone-operator-image`, a
`dockerTools.streamLayeredImage` around a statically linked operator binary
([Operator images](../ci-cd/nix-image-build.md#operator-images)).

### Source

The build runs the keystone module on its own with `GOWORK=off`, so
`operators/keystone/go.mod` and its `replace` directive for `internal/common`
decide the module graph. The source holds `internal/common` and
`operators/keystone`, without the `_test.go` files and the `helm/`,
`dashboards/` and `config/` directories, which no `go:embed` reads. The embedded
option catalogs under `api/v1alpha1/catalogs/` and the scripts under
`internal/controller/scripts/` are part of it. A commit to another operator or to
the chart leaves the image unchanged.

### Toolchain

`buildGo127Module` of the locked `nixpkgs-images` compiles the binary with
`CGO_ENABLED=0` and the ldflags `-s -w`. `proxyVendor` keeps the fixed-output
derivation to the module download, whose hash is the `vendorHash` of
`operators/keystone/vendor.json`; `make nix-lock` writes it when the `go.mod` or
`go.sum` files it derives from move. The unit tests run under `make test`, not in
the build.

### Runtime Image Properties

The image is `nix/lib/static-image.nix`, the stand-in for
`gcr.io/distroless/static:nonroot`:

| Property | Value |
| --- | --- |
| Binary | `/usr/bin/manager` |
| User | `65532:65532` (nonroot) |
| Working directory | `/home/nonroot` |
| Entrypoint | `["/usr/bin/manager"]` |
| Shell | None |
| Package manager | None |

### OCI Annotations

The image carries four static labels and no per-build label:

| Annotation | Value |
| --- | --- |
| `org.opencontainers.image.title` | `keystone-operator` |
| `org.opencontainers.image.description` | `CobaltCore OpenStack keystone operator` |
| `org.opencontainers.image.licenses` | `Apache-2.0` |
| `org.opencontainers.image.vendor` | `SAP SE` |

The `merge-operator-images` job of the CI workflow records the per-build
metadata on the published manifest list as index annotations: the commit
(`org.opencontainers.image.revision`), the version, the creation time, and the
source and URL of the repository.

### Local Build

```bash
# From the repository root, with Nix and flakes
make image OPERATOR=keystone IMG=keystone-operator:dev

# Verify: prints the flags of the manager
docker run --rm keystone-operator:dev --help
```

## Helm Chart

**Location:** `operators/keystone/helm/keystone-operator/`

### Chart Metadata

**File:** `Chart.yaml`

| Field | Value |
| --- | --- |
| `apiVersion` | `v2` |
| `name` | `keystone-operator` |
| `description` | `A Helm chart for deploying the Keystone OpenStack operator` |
| `type` | `application` |
| `version` | `0.15.0` |
| `appVersion` | `0.1.0` |

### Shared Library Subchart

The chart depends on the `operator-library` library chart
(`operators/shared/helm/operator-library`), declared in `Chart.yaml`:

```yaml
dependencies:
  - name: operator-library
    version: 0.12.0
    repository: "file://../../../shared/helm/operator-library"
```

The library holds, in one place, the naming and label helpers
(`operator-library.fullname`, `operator-library.labels`,
`operator-library.selectorLabels`, `operator-library.serviceAccountName`) and
every manifest template the operator charts render: the `Deployment`
skeleton, the ClusterRole and Role, the NetworkPolicy, certificate, Service,
ServiceAccount, bindings, PDB, ServiceMonitor and webhook configurations. Each
`templates/*.yaml` in this chart is a one-line `include`. What only this chart
knows goes through the `operator-library.chart.*` hooks: the library defines
each hook empty, and a chart overrides it by defining a template of the same
name in its `_helpers.tpl` (Helm loads the parent chart's templates last, so
its definition wins). The keystone-operator overrides
`operator-library.chart.args` to render `--federation-metadata-allow-cidrs`.

The RBAC rules are not written by hand: `make manifests` has controller-gen
assemble `config/rbac/role.yaml` from the `+kubebuilder:rbac` markers in the
Go sources, and `make sync-helm-rbac` copies its rules into
`templates/_rbac-rules.tpl` as the `keystone-operator.rbacRules` template the
library's ClusterRole and Role include (plus the leader-election leases rule).
`make verify-helm-rbac` fails on drift; the rationale for each grant lives as
a comment next to its marker. The operator-library's own helm-unittest suites
run once against `operators/shared/helm/operator-library-testbed`, a test
consumer that renders every shared template with the values contract the
operator charts share; this chart's suites cover only what it adds. A library
change is released to every consumer with `make helm-bump-library VERSION=x`.

`Chart.lock` pins the dependency and is committed; the vendored copy under
`charts/` is a build artifact (git-ignored). `helm dependency build` vendors it
from the local path. Run `make helm-deps` before `helm lint`/`template`/
`unittest`, and `make helm-package` does it automatically so the published
tarball is self-contained.

### Configuration Reference

**File:** `values.yaml`

All configurable parameters with their types, defaults, and descriptions:

#### Image

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `image.repository` | `string` | `ghcr.io/c5c3/keystone-operator` | Container image registry and repository |
| `image.tag` | `string` | `""` (appVersion) | Image tag. When empty, defaults to `appVersion` from `Chart.yaml` |
| `image.digest` | `string` | `""` | Optional `sha256:` digest with 64 lowercase hexadecimal characters, rendered as `repository:tag@digest`; Flux can inject it from a ConfigMap |
| `image.pullPolicy` | `string` | `IfNotPresent` | Kubernetes image pull policy (`Always`, `IfNotPresent`, `Never`) |

#### Replicas

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `replicas` | `integer` | `2` | Number of operator pod replicas. Use 2+ for high availability with leader election |

#### Pod Disruption Budget

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `podDisruptionBudget.enabled` | `boolean` | `true` | Render the `PodDisruptionBudget` when `replicas > 1`. `false` renders none at any replica count |
| `podDisruptionBudget.minAvailable` | `integer` or percentage `string` | unset | Pods that must stay available during a voluntary disruption. With neither count set the budget renders `minAvailable: 1` |
| `podDisruptionBudget.maxUnavailable` | `integer` or percentage `string` | unset | Pods that may be unavailable during a voluntary disruption. Wins over `minAvailable`; the schema rejects setting both |

The chart does not check either count against `replicas`. `minAvailable` equal
to `replicas` and `maxUnavailable: 0` render as given and block every node
drain. The [values schema reference](./helm-values-schema.md#poddisruptionbudget)
gives the constraints and the chart versions that carry the keys.

#### Resources

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `resources.limits.cpu` | `string` | `500m` | CPU limit per operator pod |
| `resources.limits.memory` | `string` | `128Mi` | Memory limit per operator pod |
| `resources.requests.cpu` | `string` | `10m` | CPU request per operator pod |
| `resources.requests.memory` | `string` | `64Mi` | Memory request per operator pod |

#### Scheduling

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `nodeSelector` | `object` | `{}` | Node labels the operator pods must match. Empty renders no `nodeSelector` |
| `tolerations` | `array` | `[]` | Taints the operator pods tolerate. Empty renders no `tolerations` |
| `priorityClassName` | `string` | `""` | PriorityClass of the operator pods; the class must exist. Empty renders none |

#### RBAC

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `rbac.namespaceScoped` | `boolean` | `false` | When `true`, renders a namespace-scoped Role/RoleBinding instead of the ClusterRole/ClusterRoleBinding, restricting the operator to its release namespace. See the [Multi-Tenant Deployment guide](../../guides/multi-tenant-deployment.md) |

#### Leader Election

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `leaderElection.enabled` | `boolean` | `true` | Enable leader election for controller manager. Required when running multiple replicas to ensure only one active controller |

When enabled, the `--leader-elect` flag is passed to the manager binary. When disabled,
the flag is omitted (not set to `false`), and all replicas process reconciliation events
concurrently. Disable only for single-replica development deployments.

#### Controller

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `controller.maxConcurrentReconciles` | `integer` | `2` | Maximum number of Keystone CRs reconciled concurrently; rendered as `--max-concurrent-reconciles` |
| `controller.defaultImagePullPolicy` | `string` | unset | Fallback pull policy for managed workload containers whose image references omit one; allowed values are `Always`, `IfNotPresent`, and `Never` |

When `controller.defaultImagePullPolicy` is unset, the operator uses `IfNotPresent`
for digest-pinned images and `Always` for tag-based images.

#### Webhook

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `webhook.enabled` | `boolean` | `true` | Enable admission webhooks for `Keystone` and `KeystoneIdentityBackend` CR validation and defaulting. Requires cert-manager for TLS certificate injection |
| `webhook.standalone` | `boolean` | `false` | Run the release as a standalone admission webhook for the namespace-scoped installs of the chart. Requires `webhook.enabled=true` and `rbac.namespaceScoped=false` |

When disabled, the webhook container port (9443) is omitted from the Deployment and no
webhook configuration resources are created. The operator continues to function without
webhook validation or defaulting for either CR kind. The API server continues to apply
the CRD schema's validation rules and defaults.
A namespace-scoped install sets `webhook.enabled=false`, which the values schema
requires beside `rbac.namespaceScoped=true`.

With `webhook.standalone=true` the release reconciles nothing. The Deployment
passes `--enable-controllers=false` and no `--leader-elect`, so the manager
registers no controller, elects no leader and serves only the admission
webhooks. The ClusterRole renders the
[standalone webhook rule set](#standalone-webhook-rule-set). The Service,
Issuer, Certificate and both webhook configurations render as with
`webhook.enabled=true`, and the configurations admit the `Keystone` and
`KeystoneIdentityBackend` CRs of every namespace. One such release per cluster
admits the CRs of every namespace-scoped install of the chart; see
[Multi-Tenant Deployment → Admission webhooks](../../guides/multi-tenant-deployment.md#admission-webhooks-the-standalone-webhook-release).

#### Metrics

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `metrics.port` | `integer` | `8080` | Port for the Prometheus metrics endpoint. Exposed via both the container port and the Service |

#### Logging

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `logging.development` | `boolean` | `false` | When `true`, passes `--zap-devel` (console encoder, debug verbosity). Leave `false` for the production profile (JSON encoder, info level) |
| `logging.level` | `string` | `""` | `--zap-log-level`: `debug`, `info`, `error`, `panic`, or a positive integer. Empty uses the mode default |
| `logging.encoder` | `string` | `""` | `--zap-encoder`: `json` or `console`. Empty uses the mode default |

Each value maps to a controller-runtime `--zap-*` flag and is omitted from the
operator args when left at its default.

#### Additional Manager Inputs

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `extraArgs` | array of strings | `[]` | Append manager arguments after the chart-rendered flags |
| `extraEnv` | array of `EnvVar` objects | `[]` | Append environment variables to the manager container |
| `extraVolumes` | array of `Volume` objects | `[]` | Append pod volumes after the webhook certificate volume |
| `extraVolumeMounts` | array of `VolumeMount` objects | `[]` | Append manager volume mounts after the webhook certificate mount |

#### Monitoring

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `monitoring.serviceMonitor.enabled` | `boolean` | `false` | Render a `monitoring.coreos.com/v1` ServiceMonitor targeting the metrics port. Requires prometheus-operator CRDs in the cluster |
| `monitoring.serviceMonitor.interval` | `string` | `30s` | Scrape interval on the ServiceMonitor endpoint |

See [Enable Keystone Operator Metrics](../../guides/keystone/enable-keystone-operator-metrics.md)
for the end-to-end scraping setup.

#### NetworkPolicy

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `networkPolicy.enabled` | `boolean` | `false` | Opt-in NetworkPolicy that default-denies operator pod traffic and opens explicit kube-apiserver, DNS, Keystone API health-check, webhook, and metrics rules |
| `networkPolicy.kubeApiServer.cidrs` / `.ports` | `list` | `[]` | API-server egress targets. Both **must** be non-empty when the policy is enabled (fail-closed template guard) |
| `networkPolicy.dns.enabled` | `boolean` | `true` | Enable DNS egress on UDP and TCP port 53 |
| `networkPolicy.dns.namespaceSelector` | `object` | `kubernetes.io/metadata.name: kube-system` | Select the namespace containing the DNS pods |
| `networkPolicy.dns.podSelector` | `object` | `k8s-app: kube-dns` | Select the DNS pods |
| `networkPolicy.allowMetricsFrom` | `list` | `[]` | Ingress peers allowed to scrape metrics |
| `networkPolicy.webhookClients.cidrs` | `list` | `[]` | Webhook ingress override; falls back to `kubeApiServer.cidrs` |

Full semantics live in the [Operator NetworkPolicy reference](../keystone/keystone-operator-networkpolicy.md).
The Keystone API health-check rule allows TCP ports 5000 and 5050 to Keystone API
pods with the chart's labels in any namespace; it has no chart value.

#### Federation

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `federation.metadataAllowCidrs` | `list` of CIDR strings | `[]` | CIDRs the SSRF-guarded federation-metadata client may additionally dial to fetch identity-provider metadata (OIDC discovery documents and SAML IdP metadata) — e.g. the cluster's Service CIDR `10.96.0.0/12` for a trusted in-cluster IdP. Rendered as `--federation-metadata-allow-cidrs`. Empty (the default) keeps the operator refusing every non-public address; loopback, link-local (cloud IMDS), multicast, and unspecified addresses stay blocked even when an entry covers them. Operator-level only — never derived from a CR |

See [Attach an OIDC Federation Backend to Keystone](../../guides/keystone/oidc-federation.md#discovery-against-an-in-cluster-identity-provider)
for the discovery workflow this value enables.

#### Naming

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `nameOverride` | `string` | `""` | Override the chart name used in resource names |
| `fullnameOverride` | `string` | `""` | Override the full resource-name prefix, skipping the release-name prefix |

#### Service Account

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `serviceAccount.create` | `boolean` | `true` | Create a ServiceAccount for the operator. Set to `false` to use an existing ServiceAccount |
| `serviceAccount.name` | `string` | `""` (fullname) | Name of the ServiceAccount. When empty, defaults to the Helm release fullname |

### Rendered Resources

The chart renders the following Kubernetes resources with default values:

| Resource | Kind | Name Pattern | Conditional |
| --- | --- | --- | --- |
| ServiceAccount | `v1/ServiceAccount` | `{fullname}` | `serviceAccount.create` |
| ClusterRole | `rbac.authorization.k8s.io/v1/ClusterRole` | `{fullname}` | `rbac.namespaceScoped=false` (default); carries the standalone webhook rule set instead of the manager rules under `webhook.standalone=true` |
| ClusterRoleBinding | `rbac.authorization.k8s.io/v1/ClusterRoleBinding` | `{fullname}` | `rbac.namespaceScoped=false` (default) |
| Role | `rbac.authorization.k8s.io/v1/Role` | `{fullname}` | `rbac.namespaceScoped=true` |
| RoleBinding | `rbac.authorization.k8s.io/v1/RoleBinding` | `{fullname}` | `rbac.namespaceScoped=true` |
| Deployment | `apps/v1/Deployment` | `{fullname}` | Always |
| Service | `v1/Service` | `{fullname}` | Always |
| PodDisruptionBudget | `policy/v1/PodDisruptionBudget` | `{fullname}` | `replicas > 1` and `podDisruptionBudget.enabled` |
| Issuer | `cert-manager.io/v1/Issuer` | `{fullname}-selfsigned` | `webhook.enabled` |
| Certificate | `cert-manager.io/v1/Certificate` | `{fullname}-webhook` | `webhook.enabled` |
| NetworkPolicy | `networking.k8s.io/v1/NetworkPolicy` | `{fullname}` | `networkPolicy.enabled` |
| ServiceMonitor | `monitoring.coreos.com/v1/ServiceMonitor` | `{fullname}` | `monitoring.serviceMonitor.enabled` |
| MutatingWebhookConfiguration | `admissionregistration.k8s.io/v1` | `{fullname}-mutating` | `webhook.enabled` |
| ValidatingWebhookConfiguration | `admissionregistration.k8s.io/v1` | `{fullname}-validating` | `webhook.enabled` |

The `{fullname}` pattern resolves to `{release-name}-keystone-operator` unless
`fullnameOverride` is set.

### Standard Labels

All resources rendered from chart templates include standard Helm labels via the
`operator-library.labels` helper:

| Label | Value |
| --- | --- |
| `helm.sh/chart` | `keystone-operator-0.15.0` |
| `app.kubernetes.io/name` | `keystone-operator` |
| `app.kubernetes.io/instance` | `{release-name}` |
| `app.kubernetes.io/version` | `0.1.0` |
| `app.kubernetes.io/managed-by` | `Helm` |

Selector labels (used by the Deployment, Service, PDB, NetworkPolicy, and
ServiceMonitor) are a subset:
`app.kubernetes.io/name` and `app.kubernetes.io/instance`.

### Deployment Configuration

The chart passes these arguments to the manager; conditional arguments are
omitted when their values are unset or disabled:

::: v-pre
**Container arguments:**

| Argument | Value | Configurable |
| --- | --- | --- |
| `--leader-elect` | Present when `leaderElection.enabled=true` and `webhook.standalone=false` | Yes |
| `--namespace` | Release namespace when `rbac.namespaceScoped=true` | Yes |
| `--clusters-namespace=` | Set to an empty value when `rbac.namespaceScoped=true` | Yes |
| `--enable-controllers=false` | Present when `webhook.standalone=true` | Yes |
| `--enable-webhooks=false` | Present when `webhook.enabled=false` | Yes |
| `--max-concurrent-reconciles` | `controller.maxConcurrentReconciles` (default `2`) | Yes |
| `--default-image-pull-policy` | `controller.defaultImagePullPolicy` | Yes; omitted when unset |
| `--metrics-bind-address` | `:{{ .Values.metrics.port }}` (default `:8080`) | Yes (port) |
| `--health-probe-bind-address` | `:8081` | No (hardcoded in bootstrap.Run) |
| `--federation-metadata-allow-cidrs` | Comma-separated `.Values.federation.metadataAllowCidrs`; omitted when the list is empty | Yes |
| `--zap-devel` | Present when `logging.development=true` | Yes |
| `--zap-log-level` | `logging.level` | Yes; omitted when empty |
| `--zap-encoder` | `logging.encoder` | Yes; omitted when empty |
| Extra arguments | `extraArgs`, appended verbatim after chart-rendered flags | Yes |
:::

**Health probes:**

| Probe | Path | Port | Protocol |
| --- | --- | --- | --- |
| Liveness | `/healthz` | `8081` | HTTP |
| Readiness | `/readyz` | `8081` | HTTP |

The health probe port (8081) is hardcoded in the `bootstrap.Run()` defaults and is not
configurable via Helm values.

::: v-pre
**Container ports:**

| Name | Port | Conditional |
| --- | --- | --- |
| `metrics` | `{{ .Values.metrics.port }}` (default 8080) | Always |
| `health` | `8081` | Always |
| `webhook` | `9443` | `webhook.enabled` |
:::

**Pod security context:**

| Field | Value |
| --- | --- |
| `runAsNonRoot` | `true` |
| `runAsUser` | `65532` |
| `runAsGroup` | `65532` |
| `fsGroup` | `65532` |
| `seccompProfile.type` | `RuntimeDefault` |

**Container security context:**

| Field | Value |
| --- | --- |
| `allowPrivilegeEscalation` | `false` |
| `capabilities.drop` | `[ALL]` |
| `readOnlyRootFilesystem` | `true` |
| `seccompProfile.type` | `RuntimeDefault` |

**Scheduling and disruption:**

The pod template carries a best-effort topology spread constraint (`maxSkew: 1`,
`topologyKey: kubernetes.io/hostname`, `whenUnsatisfiable: ScheduleAnyway`) so
replicas are scheduled on different nodes where possible while single-node clusters
(kind) stay schedulable. With the default values, the `PodDisruptionBudget`
(`minAvailable: 1`, rendered when `replicas > 1` and `podDisruptionBudget.enabled`)
means a voluntary disruption such as a node drain can never evict every replica,
and with it the in-process admission webhook, at once. Other budget values are
listed under [Pod Disruption Budget](#pod-disruption-budget). `nodeSelector`,
`tolerations` and `priorityClassName` render on the pod spec only when set (see
[Scheduling](#scheduling)).

### Service Configuration

The Service is type `ClusterIP` with two ports:

::: v-pre
| Name | Port | Target Port | Purpose | Conditional |
| --- | --- | --- | --- | --- |
| `webhook` | `443` | `9443` | Admission webhook callbacks from the API server | `webhook.enabled` |
| `metrics` | `{{ .Values.metrics.port }}` | `{{ .Values.metrics.port }}` | Prometheus metrics scraping | Always |
:::

### RBAC Configuration

For a normal (non-standalone) release, the ClusterRole includes permissions
generated from the kubebuilder RBAC markers in the Keystone operator's Go sources
(see the RBAC paragraph under [Helm Chart](#helm-chart)). controller-gen merges
resources that share a verb set into one rule and sorts the verbs, so the
rendered rules group differently from the table below; the grants are the same.
The chart also adds `get`, `create`, and `update` on `coordination.k8s.io/leases`
for leader election. These are the minimum permissions required for the operator
to manage Keystone resources and their dependencies:

| API Group | Resources | Verbs |
| --- | --- | --- |
| `""` (core) | `configmaps` | create, delete, get, list, watch |
| `""` (core) | `events` | create, patch |
| `""` (core) | `pods` | get, list |
| `""` (core) | `secrets` | create, delete, get, list, patch, update, watch |
| `""` (core) | `serviceaccounts` | create, delete, get, list, update, watch |
| `""` (core) | `services` | create, delete, get, list, patch, watch |
| `apps` | `deployments` | create, delete, get, list, patch, watch |
| `autoscaling` | `horizontalpodautoscalers` | create, delete, get, list, patch, watch |
| `autoscaling.k8s.io` | `verticalpodautoscalers` | create, delete, get, list, patch, watch |
| `batch` | `jobs`, `cronjobs` | get, list, watch, create, update, patch, delete |
| `batch` | `jobs` | get, list, watch, create, delete |
| `cert-manager.io` | `certificates` | get, list, watch, create, update, patch, delete |
| `k8s.mariadb.com` | `databases`, `users`, `grants` | get, list, watch, create, update, patch, delete |
| `k8s.mariadb.com` | `mariadbs`, `maxscales` | get, list, watch |
| `memcached.c5c3.io` | `memcacheds` | get, list, watch |
| `external-secrets.io` | `externalsecrets` | get, list, watch, create, update, patch |
| `external-secrets.io` | `pushsecrets` | get, list, watch, create, update, patch, delete |
| `external-secrets.io` | `clustersecretstores` | get, list, watch |
| `gateway.networking.k8s.io` | `httproutes` | get, list, watch, create, update, patch, delete |
| `gateway.networking.k8s.io` | `httproutes/status` | get |
| `keystone.openstack.c5c3.io` | `keystoneidentitybackends` | get, list, update, watch |
| `keystone.openstack.c5c3.io` | `keystoneidentitybackends/status` | get, patch, update |
| `keystone.openstack.c5c3.io` | `keystones` | get, list, patch, update, watch |
| `keystone.openstack.c5c3.io` | `keystones/finalizers` | update |
| `keystone.openstack.c5c3.io` | `keystones/status` | get, update |
| `memcached.c5c3.io` | `memcacheds` | get, list, watch |
| `networking.k8s.io` | `networkpolicies` | create, delete, get, list, patch, watch |
| `policy` | `poddisruptionbudgets` | create, delete, get, list, patch, watch |
| `rbac.authorization.k8s.io` | `roles`, `rolebindings` | create, delete, get, list, update, watch |
| `scheduling.k8s.io` | `priorityclasses` | get |
| `coordination.k8s.io` | `leases` | create, get, update (chart-added leader-election rule) |

**Notable verb restrictions:**

- **`events`** has only `create` and `patch`: the operator emits events but never reads
  or deletes them.
- **`externalsecrets`, `secretstores`, and `clustersecretstores`** are read-only
  to the manager. **`pushsecrets`** includes `delete`: the OpenBao finalizer
  deletes key-backup PushSecrets when a Keystone CR is deleted so ESO stops
  pushing to OpenBao.
- **`mariadbs`, `memcacheds`, `priorityclasses`, `pods`, and
  `httproutes/status`** are read-only: the operator observes them but never
  mutates them.

The ClusterRoleBinding binds the ClusterRole to the operator's ServiceAccount in the
release namespace only. With `rbac.namespaceScoped=true` the identical rule set is
rendered as a namespaced Role/RoleBinding instead (both library templates
include the generated `keystone-operator.rbacRules` partial). The cluster-scoped
entries, `clustersecretstores` and `priorityclasses`, grant nothing in a Role.
The operator started with `--namespace` neither watches nor reads them: it
registers no `ClusterSecretStore` watch and refuses a Keystone whose effective
store is cluster-scoped, and only the webhook, which this mode disables, reads
PriorityClasses.

#### Standalone webhook rule set

Under `webhook.standalone=true` the ClusterRole carries only what the admission
webhooks read through their uncached API reader:

| API Group | Resources | Verbs | Read by |
| --- | --- | --- | --- |
| `scheduling.k8s.io` | `priorityclasses` | get | The validating `Keystone` webhook, for `spec.deployment.priorityClassName` and the Job priority classes |
| `keystone.openstack.c5c3.io` | `keystoneidentitybackends` | list | The validating `KeystoneIdentityBackend` webhook, for the backends in the namespace |

It grants nothing on `secrets`, `rolebindings` or `leases`. Each read is
declared with a `+kubebuilder:rbac` marker ending in `roleName=keystone-webhook`
beside the `+kubebuilder:webhook` markers of the file that performs it
(`operators/keystone/api/v1alpha1/keystone_webhook.go`,
`keystoneidentitybackend_webhook.go`). controller-gen writes those markers into
a second ClusterRole of `config/rbac/role.yaml`, `keystone-webhook`, and
`make sync-helm-rbac` copies its rules into `templates/_webhook-rbac-rules.tpl`
as the `keystone-operator.webhookRbacRules` template, without a leases rule.

The generator also guards coverage: every `(apiGroup, resource, verb)` the
`keystone-webhook` ClusterRole grants must be granted by the manager rules too,
and `make sync-helm-rbac` and `make verify-helm-rbac` exit 1 otherwise. The
in-process webhook of a cluster-wide release runs under the manager identity, so
a read declared for the webhook role alone would fail there. The manager rules
also grant `get` on `priorityclasses`; controller-gen merges the verbs of
markers per resource.

### Webhook Configuration

Two webhook configurations are rendered when `webhook.enabled=true`:

**MutatingWebhookConfiguration (`{fullname}-mutating`):**

| Field | Value |
| --- | --- |
| Webhook names | `mkeystone.kb.io`, `mkeystoneidentitybackend.kb.io` |
| Paths | `/mutate-keystone-openstack-c5c3-io-v1alpha1-keystone`, `/mutate-keystone-openstack-c5c3-io-v1alpha1-keystoneidentitybackend` |
| Operations | `CREATE`, `UPDATE` |
| API group | `keystone.openstack.c5c3.io` |
| API version | `v1alpha1` |
| Resources | `keystones`, `keystoneidentitybackends` |
| Failure policy | `Fail` |
| Side effects | `None` |
| Admission review versions | `v1` |

**ValidatingWebhookConfiguration (`{fullname}-validating`):**

| Field | Value |
| --- | --- |
| Webhook names | `vkeystone.kb.io`, `vkeystoneidentitybackend.kb.io` |
| Paths | `/validate-keystone-openstack-c5c3-io-v1alpha1-keystone`, `/validate-keystone-openstack-c5c3-io-v1alpha1-keystoneidentitybackend` |
| Operations | `CREATE`, `UPDATE` |
| API group | `keystone.openstack.c5c3.io` |
| API version | `v1alpha1` |
| Resources | `keystones`, `keystoneidentitybackends` |
| Failure policy | `Fail` |
| Side effects | `None` |
| Admission review versions | `v1` |

Neither configuration intercepts `DELETE`: the webhook is served in-process by
the operator, or by the standalone webhook release, so with `failurePolicy: Fail`
a `DELETE` rule would let a down webhook pod block CR (and thereby namespace)
deletion.

Both configurations include the annotation:

::: v-pre
```yaml
cert-manager.io/inject-ca-from: {{ .Release.Namespace }}/{{ include "operator-library.fullname" . }}-webhook
```
:::

This instructs cert-manager to inject the CA bundle from the named Certificate resource
into the webhook `caBundle` field automatically. The chart renders that Certificate
itself: `templates/certificate.yaml` creates a self-signed Issuer
(`{fullname}-selfsigned`) and the `{fullname}-webhook` Certificate in the release
namespace whenever `webhook.enabled=true`.

## FluxCD HelmRelease

**File:** `deploy/bases/operators/releases/keystone-operator.yaml`

The HelmRelease deploys the Keystone operator chart via FluxCD's helm-controller,
following the established pattern used by other operators in the project (memcached-operator,
mariadb-operator).

| Property | Value |
| --- | --- |
| API version | `helm.toolkit.fluxcd.io/v2` |
| Name | `keystone-operator` |
| Target namespace | `keystone-system` |
| Reconciliation interval | `30m` |
| Chart | `keystone-operator` |
| Version constraint | `>=0.8.0 <1.0.0` |
| Source | `c5c3-charts` HelmRepository in `flux-system` namespace |

The lower bound is `0.8.0`, the first chart version whose schema accepts
`image.digest`.

**Helm values applied by the HelmRelease:**

| Key | Value | Purpose |
| --- | --- | --- |
| `replicas` | `2` | High availability with leader election |
| `leaderElection.enabled` | `true` | Single active controller with 2 replicas |
| `image.tag` | `latest` | Uses the latest image on the default deployment path |
| `image.digest` | From `keystone-operator-image-digest` ConfigMap | Optional digest pin on the `WITH_CONTROLPLANE` Flux path |

The ConfigMap is referenced through optional `valuesFrom`; when it is absent,
the chart runs with `image.tag: latest` and no digest. All other values use chart
defaults.

**Install settings:**

| Setting | Value |
| --- | --- |
| `install.crds` | `CreateReplace` |
| `install.createNamespace` | `true` |
| `install.remediation.retries` | `3` |

**Upgrade settings:**

| Setting | Value |
| --- | --- |
| `upgrade.crds` | `CreateReplace` |
| `upgrade.remediation.retries` | `3` |

**Kustomization inclusion:** The HelmRelease is listed in
`deploy/bases/operators/kustomization.yaml` under the `resources` list as
`releases/keystone-operator.yaml`.

## Dependency Chain

The Keystone operator depends on four infrastructure releases that must be ready
before it starts. FluxCD enforces this ordering via `spec.dependsOn`:

![The install order of the Flux resources of deploy/flux-system, in four layers. Layer 1 declares no dependency: cert-manager, mariadb-operator-crds and prometheus-operator-crds. Layer 2 waits for cert-manager: mariadb-operator, which also waits for mariadb-operator-crds, memcached-operator, which also waits for prometheus-operator-crds, external-secrets, garage-operator, openbao, openbao-operator and ovn-operator. Layer 3 waits for mariadb-operator, memcached-operator and external-secrets: keystone-operator, which also waits for cert-manager, and c5c3-operator. Layer 4 waits for keystone-operator: horizon-operator, glance-operator, placement-operator, cinder-operator, nova-operator, barbican-operator, which also waits for openbao-operator, and neutron-operator, which also waits for ovn-operator. A solid arrow is a dependsOn entry of a HelmRelease, and the one solid arrow between Flux Kustomizations says that the Kustomization messaging-topology-operator waits for the Kustomization rabbitmq-cluster-operator. Two dotted arrows are dependencies that no manifest declares, because each crosses between a HelmRelease and a Flux Kustomization: the Kustomization rabbitmq-cluster-operator needs the CRDs of cert-manager and retries until they exist, and c5c3-operator starts only once the Kustomization k-orc has installed the K-ORC CRDs.](../../diagrams/deploy-flux-dependencies.svg)

The keystone-operator HelmRelease sits in layer 3 of the figure: it waits for
`cert-manager`, `mariadb-operator`, `memcached-operator` and `external-secrets`.

### Why Each Dependency Is Required

| Dependency | Namespace | Reason |
| --- | --- | --- |
| `cert-manager` | `cert-manager` | Provides TLS certificate injection for admission webhooks via `cert-manager.io/inject-ca-from` annotation. Without cert-manager, webhook TLS is not provisioned and the API server cannot call admission webhooks |
| `mariadb-operator` | `mariadb-system` | Waits for the separate `mariadb-operator-crds` release, which installs the `k8s.mariadb.com` CRDs (`MariaDB`, `Database`, `User`, `Grant`); Keystone reads `MariaDB` and creates the other resources for each Keystone CR |
| `memcached-operator` | `memcached-system` | Installs the `memcached.c5c3.io` CRDs (`Memcached`) that the Keystone operator references for cache discovery |
| `external-secrets` | `external-secrets` | Installs the `external-secrets.io` CRDs (`SecretStore`, `ClusterSecretStore`, `ExternalSecret`, `PushSecret`) used by Keystone to read store and secret status and create PushSecrets |

### Deployment Sequence

FluxCD resolves the dependency graph and deploys in this order:

1. **cert-manager**, **mariadb-operator-crds**, and **prometheus-operator-crds** — base
   layer, no dependencies
2. **mariadb-operator**, **memcached-operator**, and **external-secrets** — wait for
   cert-manager; MariaDB and Memcached also wait for their CRD prerequisites, and can
   install in parallel once those releases are ready
3. **keystone-operator** — depends on all four, installs last

If any dependency is not ready (HelmRelease not in `Ready` condition), the
keystone-operator HelmRelease remains in a pending state until all dependencies are
satisfied.

## CRD Installation Behavior

**CRD files:**

- `operators/keystone/helm/keystone-operator/crds/keystone.openstack.c5c3.io_keystones.yaml`
- `operators/keystone/helm/keystone-operator/crds/keystone.openstack.c5c3.io_keystoneidentitybackends.yaml`

### Helm CRD Lifecycle

Both CRDs are placed in the chart's `crds/` directory (not `templates/`). Helm
handles CRDs in `crds/` with special behavior:

1. **On install:** Helm installs both CRDs from `crds/` **before** rendering and
   applying templates. This ensures the CRDs exist before resources that reference
   them are created.

2. **On upgrade (with FluxCD `crds: CreateReplace`):** FluxCD's helm-controller replaces
   the existing CRD with the version from the chart. This enables CRD schema updates
   when the chart version is upgraded.

3. **On uninstall:** Helm does **not** delete CRDs from the `crds/` directory on
   `helm uninstall`. CRDs are cluster-scoped resources, and deleting them would
   destroy all custom resources of those types across all namespaces.

### CRD Source

Each chart CRD file matches its generated source under
`operators/keystone/config/crd/bases/`: the `keystones` CRD defines the `Keystone`
kind, and the `keystoneidentitybackends` CRD defines `KeystoneIdentityBackend`.
Both use the `keystone.openstack.c5c3.io` API group. The chart files add a
provenance header; `make verify-crd-sync` checks the CRD content against the
generated sources.

When a source CRD changes, run `make sync-crds` to update the chart copies, then
`make verify-crd-sync` to check them.

### FluxCD CRD Policy

The HelmRelease configures both install and upgrade to use `crds: CreateReplace`:

```yaml
install:
  crds: CreateReplace
upgrade:
  crds: CreateReplace
```

| Policy | Behavior |
| --- | --- |
| `CreateReplace` | Create CRDs if they do not exist; replace (overwrite) if they do. This ensures CRD schema updates are applied on chart upgrades |
| Alternatives | `Skip` (never touch CRDs), `Create` (create only, never update). `CreateReplace` is recommended for operator charts where CRD evolution is expected |

## Data Flow

End-to-end deployment flow from FluxCD reconciliation to operator startup:

![How the Flux helm-controller installs the keystone-operator HelmRelease, in seven steps. It reconciles the HelmRelease, checking that cert-manager, mariadb-operator, memcached-operator and external-secrets are ready; fetches the chart from the c5c3-charts OCI HelmRepository; installs the Keystone and KeystoneIdentityBackend CRDs from crds/; renders the templates with chart defaults and HelmRelease values, yielding the ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment, Service, PodDisruptionBudget, Issuer, Certificate, the two webhook configurations, and the NetworkPolicy and ServiceMonitor when opted in; applies namespaced resources in keystone-system and cluster-scoped resources at cluster scope; cert-manager fills the webhook configurations' caBundle from the Certificate through the inject-ca-from annotation; and the operator pods start, where leader election picks the active replica that begins reconciling Keystone resources.](../../diagrams/deploy-keystone-operator-helmrelease.svg)
