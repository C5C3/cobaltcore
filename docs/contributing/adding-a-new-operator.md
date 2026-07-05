---
title: Adding a New Operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Adding a New Operator

This checklist captures everything a new service operator (e.g. Horizon)
touches beyond its own `operators/<op>/` module. The generic controller
scaffolding lives in `internal/common` — a new operator consumes it instead of
copying the keystone implementation — and the remainder is a finite list of
build/CI/config seams that are still enumerated per operator.

## Shared packages to consume

Build the operator on the shared scaffolding rather than hand-rolling copies.
The keystone operator is the reference consumer for every package listed.

| Package | Provides |
| --- | --- |
| `internal/common/types` | Shared CRD spec types (`DatabaseSpec`, `CacheSpec`, `GatewaySpec`, `ImageSpec`, `DeploymentSpec`, `AutoscalingSpec`, `NetworkPolicySpec`, `LoggingSpec`, ...) with their CEL rules and `Default()` methods |
| `internal/common/naming` | Label keys, `CommonLabels`/`SelectorLabels`, `SubResourceName` — the workload naming convention (and the cross-service endpoint contract, see below) |
| `internal/common/reconcile` | Table-driven pipeline (`Step`/`RunPipeline`), parallel groups (`ParallelStep`/`RunParallelGroup`), `ShortestRequeue`, `SetAggregateReady`, the no-op-skipping `UpdateStatus`, `EnsureFinalizer` |
| `internal/common/watch` | `CRUpdatePredicate` for the `For(...)` watch, `SecretToOwnersMapper` + `RegisterSecretNameIndex`, `ClusterSecretStoreFanOut` |
| `internal/common/bootstrap` | `Run`/`ManagerConfig` manager bootstrap, `ControllerOptions` (concurrency + tuned rate limiter), `DetectOperatorNamespace` |
| `internal/common/instrumentation` | Sub-reconciler duration/error metrics; declare a `NewMetrics("<op>_operator")` instance beside the instrumenter glue |
| `internal/common/deployment` | SSA ensure primitives, `RestrictedSecurityContext`, PDB/HPA builders, replica normalization, pod-knob default helpers |
| `internal/common/gateway` | `IsGVKAvailable` CRD probe, HTTPRoute builder/acceptance/ensure/delete over the shared `GatewaySpec` |
| `internal/common/secrets` | ESO primitives, `OpenBaoClusterStoreName`, the `GateSyncedSecret` ladder |
| `internal/common/validation` | Shared webhook validators (DB/cache XOR, dynamic-credentials rule, cron parse, TSC selector, PriorityClass lookup) |
| `internal/common/database`, `internal/common/cache` | MariaDB CR apply, host/port/username resolution, pymysql DSN + TLS params + rollout digest, memcache server resolution |
| `internal/common/release` | OpenStack release parsing and upgrade/downgrade classification |
| `internal/common/healthcheck` | `HTTPDoer` seam, probe-error classifier, TTL probe cache |
| `internal/common/job` | `RunJob`/`RunJobWithRerunKey`/`EnsureCronJob`/`DeleteCronJob` |
| `internal/common/config` | oslo INI rendering + immutable-ConfigMap lifecycle (see the design decisions below for non-INI services) |
| `internal/common/testutil/envtest` | envtest bootstrap, `BuildScheme`, `CommonFakeCRDDirs`, `StartManagedEnvTest` |

## Residual touch list

Everything below is still enumerated per operator. Work through it top to
bottom when scaffolding `operators/<op>/`:

- **`go.work`** — add `./operators/<op>`; keep the Go directive, toolchain, and
  the controller-runtime/k8s.io dependency versions in lockstep with the other
  modules (see [Dependency Management](./dependency-management.md)).
- **`operators/Dockerfile`** — the parameterized Dockerfile builds every
  operator via `--build-arg OPERATOR=<op>`; add the new module's
  `go.mod`/`go.sum` and source `COPY` lines (two lines total).
- **`Makefile`** — extend `OPERATORS ?= keystone c5c3`; every build/test/
  generate/lint target iterates it, provided the chart lives at
  `operators/<op>/helm/<op>-operator/`.
- **Helm chart** — scaffold `operators/<op>/helm/<op>-operator/` consuming the
  `operator-library` chart: every shared manifest (deployment, certificate,
  service, serviceaccount, rolebindings, PDB, ServiceMonitor,
  webhook-configuration) is a one-line `include`; per-operator content is
  `Chart.yaml`, `values.yaml`, the `<op>-operator.rbacRules` helper, and the
  helm-unittest suite.
- **`hack/gen-helm-values-schema.py`** — charts are discovered from the
  directory layout; add the new chart's `WEBHOOK_ENABLED_DESCRIPTIONS` entry
  (the generator fails loudly without it), then run `make gen-helm-schema`.
- **`.github/workflows/ci.yaml`** — the biggest surface: add the operator to
  the paths-filter groups, `ALL_OPERATORS` and the `FILTER_<op>` env var, the
  unit/integration test matrices, the helm-validate chart loops, and the
  `build-e2e-images` operator resolution.
- **`.github/workflows/build-images.yaml`** — nothing to do for the operator
  image (the shared `operators/Dockerfile` is already wired); only new service
  images under `images/` need matrix entries.
- **`.github/workflows/cleanup-images.yaml`** — add `<op>-operator` to the
  `cleanup-operator-images` and `cleanup-e2e-stale-tags` matrices.
- **`.codecov.yml`** — add the per-operator `unit-<op>`/`integration-<op>`
  flag blocks; the components section auto-scales via `operators/*` globs.
- **Tests** — `operators/<op>/internal/testutil/` wraps
  `internal/common/testutil/envtest` with the op-local CRD/webhook paths and
  scheme list; E2E suites live under `tests/e2e/<op>/` (one directory per
  feature, mirroring `tests/e2e/keystone/`).
- **Docs** — a CRD reference and reconciler reference under
  `docs/reference/<op>/`, wired into `docs/.vitepress/config.ts`.

## Design decisions the shared scaffolding encodes

Two cross-service decisions were settled when the scaffolding was extracted;
new operators build on them rather than reopening them:

- **Cross-service endpoint discovery is convention-based.** Consumers derive a
  service URL from the naming convention (`internal/common/naming`):
  `http://<name>.<namespace>.svc.cluster.local:<port>` over the Service named
  `SubResourceName(<cr name>)`. Keystone publishes `Status.Endpoint` for human
  consumers only; no machine consumer reads it, and no status-based resolve
  helper or cross-CR watch exists. If a new operator needs endpoint shapes the
  convention cannot express, build that helper then — not preemptively.
- **Non-INI configuration rendering gets its own package.**
  `internal/common/config` renders oslo INI only and stays that way. A service
  that renders Python settings (e.g. Horizon's Django `local_settings.py`)
  gets a separate shared renderer package (working name `pysettings`) rather
  than bolting Python emission onto the INI renderer. Implement it together
  with the first consumer.
