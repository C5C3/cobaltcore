---
title: Keystone Operator Packaging
quadrant: backend
feature: CC-0017
---

# Keystone Operator Packaging

Reference documentation for the Keystone operator packaging artifacts (CC-0017). This covers
the multi-stage Dockerfile, Helm chart configuration, FluxCD HelmRelease integration,
dependency chain, and CRD installation behavior. These artifacts package the Keystone
operator for deployment into Kubernetes clusters via the GitOps pipeline.

## Directory Layout

```text
operators/keystone/
├── Dockerfile                          Multi-stage operator image build
├── helm/
│   └── keystone-operator/
│       ├── Chart.yaml                  Helm chart metadata (v0.1.0)
│       ├── values.yaml                 Default configuration values
│       ├── crds/
│       │   └── keystone.openstack.c5c3.io_keystones.yaml   CRD (auto-installed by Helm)
│       └── templates/
│           ├── _helpers.tpl            Template helper functions
│           ├── serviceaccount.yaml     ServiceAccount (conditional)
│           ├── clusterrole.yaml        ClusterRole with RBAC rules
│           ├── clusterrolebinding.yaml ClusterRoleBinding
│           ├── deployment.yaml         Operator Deployment
│           ├── service.yaml            ClusterIP Service (webhook + metrics)
│           └── webhook-configuration.yaml  Mutating + Validating webhooks (conditional)
deploy/flux-system/
├── kustomization.yaml                  Base kustomization (includes keystone-operator release)
└── releases/
    └── keystone-operator.yaml          FluxCD HelmRelease
```

## Dockerfile

**Location:** `operators/keystone/Dockerfile`

The Dockerfile uses a multi-stage build to produce a minimal, statically-linked operator
binary in a distroless runtime image. The build context must be the workspace root
(`/workspace`) because the Go workspace (`go.work`) uses `replace` directives that
reference sibling modules.

### Build Stages

| Stage | Base Image | Purpose |
| --- | --- | --- |
| `builder` | `golang:1.25` | Compiles the operator binary with CGO disabled |
| runtime | `gcr.io/distroless/static:nonroot` | Minimal runtime with no shell or package manager |

### Image Layers

The builder stage is structured for optimal Docker layer caching:

1. **Layer 1 — Dependency manifests:** Copies `go.work`, `go.work.sum`, and all
   `go.mod`/`go.sum` files for workspace modules. This layer is cached as long as
   dependency versions do not change.

   ```dockerfile
   COPY go.work go.work.sum ./
   COPY internal/common/go.mod internal/common/go.sum ./internal/common/
   COPY operators/keystone/go.mod operators/keystone/go.sum ./operators/keystone/
   COPY operators/c5c3/go.mod operators/c5c3/go.sum ./operators/c5c3/
   ```

2. **Layer 2 — Module download:** Runs `go mod download` to fetch all dependencies.
   Cached when dependency manifests are unchanged.

3. **Layer 3 — Source code:** Copies the full source trees for `internal/common/`,
   `operators/keystone/`, and `operators/c5c3/`. Invalidated on any source change.

4. **Layer 4 — Compilation:** Builds the static binary from `operators/keystone/main.go`.

   ```dockerfile
   CGO_ENABLED=0 GOOS=linux go build -o manager main.go
   ```

The runtime stage copies only the compiled `/manager` binary from the builder stage.

### Build Context

The build context **must** be the workspace root, not the operator directory. The Go
workspace file (`go.work`) contains `replace` directives pointing to relative paths
(`internal/common`, `operators/c5c3`) that must be resolvable at build time.

```bash
# Correct: build from workspace root
docker build -f operators/keystone/Dockerfile .

# Incorrect: will fail because go.work references are unresolvable
docker build operators/keystone/
```

### Build Arguments

The Dockerfile does not declare any `ARG` instructions. All build configuration is
determined by the Go workspace and module files.

### Runtime Image Properties

| Property | Value |
| --- | --- |
| Base image | `gcr.io/distroless/static:nonroot` |
| Binary | `/manager` |
| User | `65532:65532` (nonroot) |
| Entrypoint | `["/manager"]` |
| Shell | None (distroless) |
| Package manager | None (distroless) |

### OCI Annotations

Static OCI Image Spec annotations are embedded in the runtime stage via `LABEL` instructions:

| Annotation | Value |
| --- | --- |
| `org.opencontainers.image.title` | `keystone-operator` |
| `org.opencontainers.image.description` | `CobaltCore Keystone Operator for managing OpenStack Identity Service` |
| `org.opencontainers.image.licenses` | `Apache-2.0` |
| `org.opencontainers.image.vendor` | `SAP SE` |

In CI, `docker/metadata-action` supplements these with dynamic labels (`created`,
`revision`, `source`, `url`, `version`) at push time.

### Local Build

```bash
# From workspace root
docker build -f operators/keystone/Dockerfile -t keystone-operator:dev .

# Verify
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
| `version` | `0.1.0` |
| `appVersion` | `0.1.0` |

### Configuration Reference

**File:** `values.yaml`

All configurable parameters with their types, defaults, and descriptions:

#### Image

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `image.repository` | `string` | `ghcr.io/c5c3/keystone-operator` | Container image registry and repository |
| `image.tag` | `string` | `""` (appVersion) | Image tag. When empty, defaults to `appVersion` from `Chart.yaml` |
| `image.pullPolicy` | `string` | `IfNotPresent` | Kubernetes image pull policy (`Always`, `IfNotPresent`, `Never`) |

#### Replicas

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `replicas` | `integer` | `2` | Number of operator pod replicas. Use 2+ for high availability with leader election |

#### Resources

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `resources.limits.cpu` | `string` | `500m` | CPU limit per operator pod |
| `resources.limits.memory` | `string` | `128Mi` | Memory limit per operator pod |
| `resources.requests.cpu` | `string` | `10m` | CPU request per operator pod |
| `resources.requests.memory` | `string` | `64Mi` | Memory request per operator pod |

#### Leader Election

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `leaderElection.enabled` | `boolean` | `true` | Enable leader election for controller manager. Required when running multiple replicas to ensure only one active controller |

When enabled, the `--leader-elect` flag is passed to the manager binary. When disabled,
the flag is omitted (not set to `false`), and all replicas process reconciliation events
concurrently. Disable only for single-replica development deployments.

#### Webhook

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `webhook.enabled` | `boolean` | `true` | Enable admission webhooks (MutatingWebhookConfiguration and ValidatingWebhookConfiguration). Requires cert-manager for TLS certificate injection |

When disabled, the webhook container port (9443) is omitted from the Deployment and no
webhook configuration resources are created. The operator continues to function without
admission validation — CRs are not validated or defaulted at admission time.

#### Metrics

| Parameter | Type | Default | Description |
| --- | --- | --- | --- |
| `metrics.port` | `integer` | `8080` | Port for the Prometheus metrics endpoint. Exposed via both the container port and the Service |

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
| ClusterRole | `rbac.authorization.k8s.io/v1/ClusterRole` | `{fullname}` | Always |
| ClusterRoleBinding | `rbac.authorization.k8s.io/v1/ClusterRoleBinding` | `{fullname}` | Always |
| Deployment | `apps/v1/Deployment` | `{fullname}` | Always |
| Service | `v1/Service` | `{fullname}` | Always |
| MutatingWebhookConfiguration | `admissionregistration.k8s.io/v1` | `{fullname}-mutating` | `webhook.enabled` |
| ValidatingWebhookConfiguration | `admissionregistration.k8s.io/v1` | `{fullname}-validating` | `webhook.enabled` |

The `{fullname}` pattern resolves to `{release-name}-keystone-operator` unless
`fullnameOverride` is set.

### Standard Labels

All resources include standard Helm labels via the `keystone-operator.labels` helper:

| Label | Value |
| --- | --- |
| `helm.sh/chart` | `keystone-operator-0.1.0` |
| `app.kubernetes.io/name` | `keystone-operator` |
| `app.kubernetes.io/instance` | `{release-name}` |
| `app.kubernetes.io/version` | `0.1.0` |
| `app.kubernetes.io/managed-by` | `Helm` |

Selector labels (used by Deployment and Service) are a subset:
`app.kubernetes.io/name` and `app.kubernetes.io/instance`.

### Deployment Configuration

The operator Deployment is configured with the following fixed settings:

**Container arguments:**

| Argument | Value | Configurable |
| --- | --- | --- |
| `--leader-elect` | Present when `leaderElection.enabled=true` | Yes |
| `--metrics-bind-address` | `:{{ .Values.metrics.port }}` (default `:8080`) | Yes (port) |
| `--health-probe-bind-address` | `:8081` | No (hardcoded in bootstrap.Run) |

**Health probes:**

| Probe | Path | Port | Protocol |
| --- | --- | --- | --- |
| Liveness | `/healthz` | `8081` | HTTP |
| Readiness | `/readyz` | `8081` | HTTP |

The health probe port (8081) is hardcoded in the `bootstrap.Run()` defaults and is not
configurable via Helm values.

**Container ports:**

| Name | Port | Conditional |
| --- | --- | --- |
| `metrics` | `{{ .Values.metrics.port }}` (default 8080) | Always |
| `health` | `8081` | Always |
| `webhook` | `9443` | `webhook.enabled` |

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

### Service Configuration

The Service is type `ClusterIP` with two ports:

| Name | Port | Target Port | Purpose | Conditional |
| --- | --- | --- | --- | --- |
| `webhook` | `443` | `9443` | Admission webhook callbacks from the API server | `webhook.enabled` |
| `metrics` | `{{ .Values.metrics.port }}` | `{{ .Values.metrics.port }}` | Prometheus metrics scraping | Always |

### RBAC Configuration

The ClusterRole includes permissions derived from kubebuilder RBAC markers in
`operators/keystone/internal/controller/keystone_controller.go`. These are the minimum
permissions required for the operator to manage Keystone resources and their dependencies:

| API Group | Resources | Verbs |
| --- | --- | --- |
| `keystone.openstack.c5c3.io` | `keystones` | get, list, watch, create, update, patch, delete |
| `keystone.openstack.c5c3.io` | `keystones/status` | get, update, patch |
| `keystone.openstack.c5c3.io` | `keystones/finalizers` | update |
| `apps` | `deployments` | get, list, watch, create, update, patch, delete |
| `""` (core) | `services`, `configmaps`, `secrets`, `serviceaccounts` | get, list, watch, create, update, patch, delete |
| `""` (core) | `events` | create, patch |
| `batch` | `jobs`, `cronjobs` | get, list, watch, create, update, patch, delete |
| `k8s.mariadb.com` | `databases`, `users`, `grants` | get, list, watch, create, update, patch, delete |
| `external-secrets.io` | `externalsecrets`, `pushsecrets` | get, list, watch, create, update, patch |
| `rbac.authorization.k8s.io` | `roles`, `rolebindings` | get, list, watch, create, update, patch, delete |

**Notable verb restrictions:**

- **`events`** has only `create` and `patch` — the operator emits events but never reads
  or deletes them.
- **`external-secrets.io` resources** have no `delete` verb — the operator creates and
  updates ExternalSecret/PushSecret CRs but does not delete them (secret lifecycle is
  managed by the External Secrets Operator).

The ClusterRoleBinding binds the ClusterRole to the operator's ServiceAccount in the
release namespace only.

### Webhook Configuration

Two webhook configurations are rendered when `webhook.enabled=true`:

**MutatingWebhookConfiguration (`{fullname}-mutating`):**

| Field | Value |
| --- | --- |
| Webhook name | `mkeystone.kb.io` |
| Path | `/mutate-keystone-openstack-c5c3-io-v1alpha1-keystone` |
| Operations | `CREATE`, `UPDATE` |
| API group | `keystone.openstack.c5c3.io` |
| API version | `v1alpha1` |
| Resource | `keystones` |
| Failure policy | `Fail` |
| Side effects | `None` |
| Admission review versions | `v1` |

**ValidatingWebhookConfiguration (`{fullname}-validating`):**

| Field | Value |
| --- | --- |
| Webhook name | `vkeystone.kb.io` |
| Path | `/validate-keystone-openstack-c5c3-io-v1alpha1-keystone` |
| Operations | `CREATE`, `UPDATE`, `DELETE` |
| API group | `keystone.openstack.c5c3.io` |
| API version | `v1alpha1` |
| Resource | `keystones` |
| Failure policy | `Fail` |
| Side effects | `None` |
| Admission review versions | `v1` |

Both configurations include the annotation:

```yaml
cert-manager.io/inject-ca-from: {{ .Release.Namespace }}/{{ include "keystone-operator.fullname" . }}-webhook
```

This instructs cert-manager to inject the CA bundle from the named Certificate resource
into the webhook `caBundle` field automatically. The Certificate resource must exist in
the release namespace with the name `{fullname}-webhook`.

## FluxCD HelmRelease

**File:** `deploy/flux-system/releases/keystone-operator.yaml`

The HelmRelease deploys the Keystone operator chart via FluxCD's helm-controller,
following the established pattern used by other operators in the project (memcached-operator,
mariadb-operator).

| Property | Value |
| --- | --- |
| API version | `helm.toolkit.fluxcd.io/v2` |
| Name | `keystone-operator` |
| Target namespace | `openstack` |
| Reconciliation interval | `30m` |
| Chart | `keystone-operator` |
| Version constraint | `>=0.1.0 <1.0.0` |
| Source | `c5c3-charts` HelmRepository in `flux-system` namespace |

**Helm values applied by the HelmRelease:**

| Key | Value | Purpose |
| --- | --- | --- |
| `replicas` | `2` | High availability with leader election |
| `leaderElection.enabled` | `true` | Single active controller with 2 replicas |

All other values use chart defaults.

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
`deploy/flux-system/kustomization.yaml` under the `resources` list as
`releases/keystone-operator.yaml`.

## Dependency Chain

The Keystone operator depends on four infrastructure operators that must be running
before it starts. FluxCD enforces this ordering via `spec.dependsOn`:

```text
cert-manager (cert-manager namespace)
├── mariadb-operator (mariadb-system namespace)
├── memcached-operator (memcached-system namespace)
├── external-secrets (external-secrets namespace)
└── keystone-operator (openstack namespace)
    ├── dependsOn: cert-manager/cert-manager
    ├── dependsOn: mariadb-operator/mariadb-system
    ├── dependsOn: memcached-operator/memcached-system
    └── dependsOn: external-secrets/external-secrets
```

### Why Each Dependency Is Required

| Dependency | Namespace | Reason |
| --- | --- | --- |
| `cert-manager` | `cert-manager` | Provides TLS certificate injection for admission webhooks via `cert-manager.io/inject-ca-from` annotation. Without cert-manager, webhook TLS is not provisioned and the API server cannot call admission webhooks |
| `mariadb-operator` | `mariadb-system` | Installs the `k8s.mariadb.com` CRDs (`Database`, `User`, `Grant`) that the Keystone operator creates to provision database resources for each Keystone CR |
| `memcached-operator` | `memcached-system` | Installs the `memcached.c5c3.io` CRDs (`Memcached`) that the Keystone operator references for cache discovery |
| `external-secrets` | `external-secrets` | Installs the `external-secrets.io` CRDs (`ExternalSecret`, `PushSecret`) that the Keystone operator creates to manage secret synchronization from the secret store |

### Deployment Sequence

FluxCD resolves the dependency graph and deploys in this order:

1. **cert-manager** — base layer, no dependencies
2. **mariadb-operator**, **memcached-operator**, and **external-secrets** — depend only on
   cert-manager, can install in parallel
3. **keystone-operator** — depends on all four, installs last

If any dependency is not ready (HelmRelease not in `Ready` condition), the
keystone-operator HelmRelease remains in a pending state until all dependencies are
satisfied.

## CRD Installation Behavior

**CRD file:** `operators/keystone/helm/keystone-operator/crds/keystone.openstack.c5c3.io_keystones.yaml`

### Helm CRD Lifecycle

The CRD is placed in the chart's `crds/` directory (not `templates/`). Helm handles
CRDs in `crds/` with special behavior:

1. **On install:** Helm installs CRDs from `crds/` **before** rendering and applying
   templates. This ensures the CRD exists before any templates that reference it are
   created, avoiding chicken-and-egg ordering issues.

2. **On upgrade (with FluxCD `crds: CreateReplace`):** FluxCD's helm-controller replaces
   the existing CRD with the version from the chart. This enables CRD schema updates
   when the chart version is upgraded.

3. **On uninstall:** Helm does **not** delete CRDs from the `crds/` directory on
   `helm uninstall`. This is intentional — CRDs are cluster-scoped resources and
   deleting them would destroy all custom resources of that type across all namespaces.

### CRD Source

The CRD file in `crds/` is an exact copy of the generated CRD at
`operators/keystone/config/crd/bases/keystone.openstack.c5c3.io_keystones.yaml`. It
defines the `Keystone` kind in the `keystone.openstack.c5c3.io` API group.

**Important:** The CRD in `crds/` must remain an exact copy of the source CRD. Manual
modifications would cause divergence between the Helm-installed CRD and the
kubebuilder-generated source. When the source CRD changes (e.g., new spec fields are
added), the copy in `crds/` must be updated to match.

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

```text
FluxCD helm-controller
  │
  ├─ 1. Reconciles HelmRelease (keystone-operator)
  │     Checks dependsOn: cert-manager ✓, mariadb-operator ✓, memcached-operator ✓
  │
  ├─ 2. Fetches chart from c5c3-charts OCI HelmRepository
  │
  ├─ 3. Installs CRDs from crds/ directory
  │     → keystone.openstack.c5c3.io_keystones.yaml applied to cluster
  │
  ├─ 4. Renders templates with merged values (chart defaults + HelmRelease values)
  │     → ServiceAccount, ClusterRole, ClusterRoleBinding, Deployment, Service,
  │       MutatingWebhookConfiguration, ValidatingWebhookConfiguration
  │
  ├─ 5. Applies rendered resources to openstack namespace
  │
  ├─ 6. cert-manager detects inject-ca-from annotation on webhook configurations
  │     → Injects CA bundle from Certificate resource into caBundle field
  │
  └─ 7. Operator pods start, leader election determines active replica
        → Active replica begins reconciling Keystone CRs
```
