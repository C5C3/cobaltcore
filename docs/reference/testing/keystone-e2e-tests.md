---
title: Keystone E2E Test Suites
quadrant: operator
---

# Keystone E2E Test Suites

Reference documentation for the Keystone Chainsaw E2E test suites. These
tests validate the KeystoneReconciler's end-to-end behavior in a real Kubernetes cluster
with all infrastructure dependencies deployed (MariaDB, Memcached, ESO, OpenBao).

For CRD validation E2E tests (`invalid-cr`), see
[Keystone CRD API Reference](../keystone/keystone-crd.md#chainsaw-e2e-tests). For the reconciler
architecture and sub-reconciler contracts, see
[Keystone Reconciler Architecture](../keystone/keystone-reconciler.md). For infrastructure
deployment automation, see
[Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md).

## Overview

The test suites cover the full reconciler lifecycle — from initial deployment through
scaling, key rotation, image upgrades, cross-release upgrades, and deletion cleanup. Each
suite is independent and creates its own Keystone CR with a unique name in the `openstack`
namespace, enabling parallel execution (Chainsaw runs up to 4 suites concurrently).

`tests/e2e/keystone/` currently holds **58 suites** and is the canonical
inventory — the [Test Suite Inventory](#test-suite-inventory) below lists all of
them, and the [Test Suite Details](#test-suite-details) sections walk through a
representative subset step by step.

Most suites run in the `openstack` namespace; `pod-security-restricted` creates
its own `keystone-pss-restricted-test` namespace, and `multi-namespace` creates
`keystone-mns-a`, `keystone-mns-b` and `keystone-mns-c`. Two operator-level suites,
`metrics` and `prometheus-stack`, exercise the keystone-operator Helm deployment
in `keystone-system`, and `keystone-operator/env-key-material` reads the spec
and the process environment of that deployment's pod.
The operator-level suite `namespace-scoped-rbac` installs a namespace-scoped
release and a standalone webhook release of the chart into `openstack`. The
operator-level suite `keystone-operator/leader-election` deletes the operator's
Lease in `keystone-system` and runs alone (`concurrent: false`), because the
former leader can restart during it. All suites assume the pre-deployed infrastructure stack (MariaDB, Memcached, ESO,
OpenBao).

## Prerequisites

All test suites require the infrastructure stack to be deployed and healthy. The
`infra-stack-health` test (`tests/e2e/infrastructure/`) verifies this precondition.

| Prerequisite | Details |
| --- | --- |
| Infrastructure stack | Deployed via `make deploy-infra` (see [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md)) |
| Keystone operator | Deployed to the cluster with CRDs installed |
| ESO ExternalSecrets | `keystone-admin`, `keystone-db` synced in `openstack` namespace |
| MariaDB instance | `openstack-db` MariaDB CR Ready in `openstack` namespace |
| Memcached instance | `openstack-memcached` Memcached CR Ready in `openstack` namespace |

## Running the Tests

```bash
# Run all E2E tests (infrastructure + Keystone)
make e2e

# Run only Keystone E2E tests
chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/

# Run a specific test suite
chainsaw test --config tests/e2e/chainsaw-config.yaml tests/e2e/keystone/basic-deployment/
```

## Chainsaw Configuration

All tests use the shared configuration at `tests/e2e/chainsaw-config.yaml`:

| Setting | Value | Purpose |
| --- | --- | --- |
| `timeouts.apply` | 30s | Resource application timeout |
| `timeouts.assert` | 120s | Default assertion timeout (overridden per-step in most suites) |
| `timeouts.cleanup` | 60s | Post-test resource cleanup |
| `timeouts.delete` | 30s | Resource deletion timeout |
| `timeouts.error` | 30s | Error assertion timeout |
| `timeouts.exec` | 30s | Script execution timeout |
| `execution.parallel` | 4 | Maximum concurrent test suites |
| `execution.failFast` | true | Stop on first failure |
| `report.format` | JUNIT-TEST | JUnit XML output for CI |
| `report.path` | `_output/reports` | Report directory |

Individual test suites override the assert timeout to 5 minutes (`5m`) to accommodate
the full reconciliation cycle (Secret sync, database provisioning, db_sync Job,
Deployment rollout, bootstrap Job).

## Test Suite Inventory

| Suite | CR Name | Reconciler Behavior Validated |
| --- | --- | --- |
| [basic-deployment](#basic-deployment) | `keystone-basic` | Full happy-path reconciliation, all conditions, owned resources, API accessibility |
| [missing-secret](#missing-secret) | `keystone-missing-secret` | SecretsReady requeue on missing ESO Secrets, recovery on creation |
| [invalid-identitybackend-cr](../keystone/identity-backend-crd.md#chainsaw-e2e-tests) | (rejected at admission) | KeystoneIdentityBackend CEL/webhook rejection corpus: union rule, Default-domain protection, extraOptions denylist, immutability transitions, domain uniqueness |
| [ldap-domain-backend](../keystone/identity-backend-crd.md#chainsaw-e2e-tests) | `keystone-ldap` | Full LDAP attach cycle against an in-suite OpenLDAP fixture: domain provisioning, config projection + rollout, LDAP user listing and token issuance, detach with deletionPolicy Delete |
| [oidc-federation](../keystone/identity-backend-crd.md#chainsaw-e2e-tests) | `keystone-oidc` | Full OIDC federation cycle against an in-suite two-realm Keycloak fixture: federation-object provisioning, mod_auth_openidc sidecar rollout + Service targetPort switch, bearer/CLI flow via introspection, browser websso round trip, mapping-driven group membership, multi-realm issuer pinning, clean detach. The multi-realm coverage exercises both discovery shapes: the second backend reaches Ready via discovery-mode `.well-known` metadata fetch against the in-cluster IdP (the e2e-operator job allowlists kind's Service CIDR through `federation.metadataAllowCidrs`), while the first keeps explicit `spec.oidc.endpoints` |
| [saml-federation](../keystone/identity-backend-crd.md#chainsaw-e2e-tests) | `keystone-saml` | Full SAML federation cycle against an in-suite Keycloak SAML realm: IdP-metadata Secret from the live descriptor, federation-object provisioning, mod_auth_mellon sidecar rollout + Service targetPort switch, SP-metadata export, OIDC/SAML coexistence on one sidecar, clean detach |
| [fernet-rotation](#fernet-rotation) | `keystone-fernet` | CronJob schedule and `concurrencyPolicy: Forbid`, manual rotation trigger, Secret data change, pod UID stability (no rollout), token validation, staging-Secret recreation after an out-of-band delete, pruning past `maxActiveKeys` (a pruned token gets a 401), a refused staging PATCH retried without a second rotation, a held scheduled rotation skipped under Forbid (`JobAlreadyActive`) |
| [scale](#scale) | `keystone-scale` | Replica scaling up (3→5) and down (5→2) |
| [deletion-cleanup](#deletion-cleanup) | `keystone-cleanup` | Owner reference cascading deletion of all owned resources |
| [policy-overrides](#policy-overrides) | `keystone-policy` | oslo.policy integration via ConfigMap reference |
| [middleware-config](#middleware-config) | `keystone-middleware` | WSGI middleware pipeline customization in api-paste.ini |
| [brownfield-database](#brownfield-database) | `keystone-brownfield` | Explicit database host (no MariaDB CRs created) |
| [db-sync-partial-schema](#db-sync-partial-schema) | `keystone-partial-schema` | The db-sync pre-step drops the rowless schema an interrupted first `db_sync` leaves, and the first Job attempt rebuilds it |
| [image-upgrade](#image-upgrade) | `keystone-upgrade` | Rolling image update without losing Ready status |
| [image-pull-policy](#image-pull-policy) | `keystone-ipp` | Without `spec.image.pullPolicy` every container of the Deployment and the db-sync Job carries the operator default `IfNotPresent`; `pullPolicy: Never` reaches every container of the Deployment and the CronJobs, re-runs db-sync and keeps the bootstrap Job |
| [release-upgrade](#release-upgrade) | `keystone-release-upgrade` | Cross-release upgrade from 2026.1 to 2026.2 via expand-migrate-contract, API accessibility and admin authentication before/after, token continuity, fernet keys unchanged |
| [concurrent-cr-conflicts](#concurrent-cr-conflicts) | `keystone-concurrent-a`, `keystone-concurrent-b` | Concurrent CR reconciliation with shared secrets, sub-resource isolation, deletion without cross-CR impact |
| [config-pruning](#config-pruning) | `keystone-pruning` | Immutable ConfigMap pruning — stale ConfigMaps removed after multiple config changes, retain+1 cap, Ready=True preserved |
| [events](#events) | `keystone-events` | Kubernetes event emission for BootstrapComplete, DatabaseSynced, FernetKeysGenerated, CredentialKeysGenerated |
| [graceful-shutdown](#graceful-shutdown) | `keystone-graceful-shutdown` | Deployment configured with `terminationGracePeriodSeconds=30`, preStop sleep hook, startup probe |
| [healthcheck](#healthcheck) | `keystone-healthcheck` | Post-Deployment HTTP health check gates `KeystoneAPIReady=True` with reason `APIHealthy` before aggregate `Ready` flips |
| [policy-validation](#policy-validation) | `keystone-policy-validation` | `PolicyValidReady` gates the Deployment; validation Job lifecycle on `policyOverrides` add/remove |
| [priority-class](#priority-class) | `keystone-pc` | `spec.deployment.priorityClassName` propagation: unset → empty, set → applied to the Deployment, the db-sync Job and the fernet-rotate CronJob, `spec.jobs.priorityClassName: ""` → removed from the Jobs only, patched empty → removed |
| [node-placement](#node-placement) | `keystone-np` | `spec.deployment` `nodeSelector`, `tolerations` and `affinity` on the Deployment; the db-sync Job and the fernet-rotate CronJob inherit the selector, the toleration and the node affinity, not the pod anti-affinity |
| [schema-drift-detection](#schema-drift-detection) | `keystone-schema-drift` | `DatabaseReady=True` with message "revision verified"; schema-check Job runs and completes |
| [semantic-invariants](#semantic-invariants) | `keystone-invariants` | `status.endpoint` URL format, `ownerReferences` fan-out, `observedGeneration` tracking, `lastTransitionTime` monotonicity, ConfigMap immutability |
| [topology-spread](#topology-spread) | `keystone-tsc` | `spec.deployment.topologySpreadConstraints`: `nil` injects 2 defaults; non-empty slice passes through verbatim; `[]` disables all constraints |
| [pod-security-restricted](#pod-security-restricted) | `keystone-pss-restricted` | Reconciliation reaches Ready=True/AllReady inside a `pod-security.kubernetes.io/enforce=restricted` namespace; every Pod the reconciler creates (API Deployment, bootstrap Job, db-sync Job, policy-validation Job, manually-triggered fernet-rotation Job) admits under PSS Restricted; zero `FailedCreate` events carry the literal violation `violates PodSecurity "restricted:latest"` |
| admin-password-rotation | `keystone-adminpw` | Re-bootstrap on admin-password Secret change: stale bootstrap Job replaced, new password authenticates against `/v3` |
| admin-password-scheduled-rotation | `keystone-adminpw-sched` | Model B scheduled rotation: rotation CronJob rendered from `spec.passwordRotation`, full OpenBao/ESO evidence chain |
| autoscaling | `keystone-autoscaling` | HPA create/update/delete driven by `spec.autoscaling` (CPU and memory targets); the API PodDisruptionBudget follows the HPA minimum: `minAvailable: 1` with `minReplicas` unset, `maxUnavailable: 1` at `minReplicas: 1`, `minAvailable: 1` again after autoscaling is removed; a CPU target of 150 and a `scaleDown` behavior reach the HPA as `averageUtilization: 150` and `spec.behavior.scaleDown` |
| basic-deployment-2026-2 | `keystone-basic-2026-2` | Happy-path deployment pinned to the 2026.2 release image |
| configmap-no-secrets | `keystone-cc0080` | No secrets leak into the ConfigMap: placeholder URL in `keystone.conf`, real DSN only in the derived `<name>-db-connection` Secret |
| credential-rotation | `keystone-credential` | Credential-key CronJob schedule, manual rotation changes Secret data, `credential_migrate` step, a refused staging PATCH retried without a second rotation while a credential stored before it keeps its blob; after the rotation the keystone-operator release exports the `keystone_operator_key_rotation_age_seconds{key_type="credential"}` sample |
| database-tls | `keystone-dbtls` | `spec.database.tls` up to `verify-full`: `DatabaseTLSReady=True`, cert-manager client cert, encrypted MariaDB connection |
| [db-expand-failure](#db-expand-failure) | `keystone-db-expand-failure` | A 2026.1 → 2026.2 upgrade whose expand Job is OOM-killed until `BackoffLimitExceeded`: `DatabaseReady=False/ExpandFailed` naming the failed Job while `upgradePhase` stays `Expanding`, a Warning `ExpandFailed` event, the `keystone_operator_db_sync_total{result="failed"}` sample, and the Deployment still on 2026.1 |
| env-key-material | — (operator-level) | The keystone-operator pod in `keystone-system` reads no Secret into env, and its manager's process environment, read from an ephemeral container on the `keystone:2026.1` image by `tests/e2e/lib/assert-operator-env-clean.sh`, holds no variable named like key material and no value such as a PEM block; a finding names the variable, never its value |
| gateway-quick-start | `keystone-gateway-qs` | Envoy Gateway quick-start wiring: GatewayClass/Gateway Programmed, HTTPRoute attached, endpoint reachable |
| gateway-quick-start-smoke | `keystone-smoke` | `curl https://keystone.127-0-0-1.nip.io/v3` returns HTTP 200 with a Keystone v3 JSON body |
| httproute | `keystone-httproute` | `spec.gateway` lifecycle: HTTPRoute created, updated, and removed with the spec block |
| invalid-cr | multiple (rejected) | Every CEL XValidation and webhook rejection path pinned to a deterministic admission failure |
| key-repository-mode | `keystone-keymode` | Fernet/credential key volumes are not world-readable (no `key_repository is world readable` startup warning) |
| [leader-election](#leader-election) | `keystone-lease` (operator-level) | Deleting the operator's Lease `keystone.openstack.c5c3.io` in `keystone-system`: a new Lease with a holder within 90s, acquired after the old one's last renewal unless the leader recreated it within its own term, renewed by its holder and unchanged for 30s; after a failover the former leader restarts; the CR stays Ready throughout and the re-elected leader reconciles a replica patch |
| logging | `keystone-logging` | `spec.logging` propagation to oslo.log: defaults, level/format overrides, per-logger levels |
| maintenance-endpoint-isolation | `keystone-endpoint-isolation` | Maintenance pods never become API Service backends: the non-terminating EndpointSlice address count for the API Service stays at exactly the live replica count — never above, never empty — across a sampling window that observes a live trust-flush pod holding a pod IP |
| metrics | — (operator-level) | keystone-operator chart renders and removes the ServiceMonitor; metrics endpoint scrapeable |
| [multi-namespace](#multi-namespace) | `keystone-mns` in `keystone-mns-a`, `keystone-mns-b` and `keystone-mns-c` | One Keystone name in three namespaces: every child owned by its own namespace's Keystone alone, one schema each, three distinct fernet backup keys and key sets, a token valid at home (200) and refused in the other two (401); deleting one instance leaves the other two unchanged |
| namespace-scoped-rbac | `keystone-ns-scoped` | Operator deployed with `rbac.namespaceScoped=true` + `webhook.enabled=false` beside a standalone webhook release (`webhook.standalone=true`): the standalone release's ClusterRole carries only the two webhook reads, the CR is defaulted at admission and reconciles to Ready, an invalid cron expression is rejected at admission, and the standalone pod's `controller_runtime_webhook_requests_total` counters show that it admitted both. With the CR Ready, the operator installed with `rbac.namespaceScoped=true` and `webhook.enabled=false` has started in namespace-scoped mode, logged no forbidden watch, never restarted, and recorded a successful reconcile on its own metrics endpoint |
| network-policy | `keystone-netpol` | Per-CR NetworkPolicy create/update/delete driven by `spec.networkPolicy` ingress sources; probe pods then find the target dropped on 9999, resolving a cluster name and reaching 3306, 6443 and 11211, a `keystone-system` client dropped on 9999 and reaching 5000, and a `keystone-netpol-outside` client dropped on 5000 until the update admits its namespace |
| [plugins](#plugins) | `keystone-plugins` | `spec.plugins` selects keystone's `strict_two_level` unified-limit model: the rendered `[unified_limit]` section and `GET /v3/limits/model` report it; pods of a model keystone cannot load never turn Ready while the old pod keeps serving; without the plugin the API reports `flat` |
| prometheus-stack | — (operator-level) | `WITH_PROMETHEUS=true` opt-in addon: kube-prometheus-stack scrapes the operator end to end |
| [rapid-spec-changes](#rapid-spec-changes) | `keystone-rapid` | Twelve back-to-back `extraConfig` patches converge on the last one: each bumps the generation and a misspelt one is refused, the live ConfigMap and every pod carry `expiration = 4320`, a token lives 4318 to 4320 s, a repeated patch renders no ConfigMap, at most four ConfigMaps remain, and nothing moves for 60s |
| resources | `keystone-resources` | Render-time per-resource defaults on the Deployment and a running Pod (no CPU limit, Burstable), the Job defaults on the db-sync Job template (70m CPU and 368Mi memory requests, a 368Mi memory limit, no CPU limit), propagation of a patched `spec.deployment.resources`, and default memory that follows `spec.uwsgi.processes` once the block is removed |
| rolling-update-zero-downtime | `keystone-rolling-update` | Full graceful-termination chain keeps the API serving during an image-tag rolling update |
| trust-flush | `keystone-trust-flush` | Trust-flush CronJob creation, schedule and suspend tracking `spec.trustFlush` |
| trust-flush-default | `keystone-trust-flush-default` | Default-on posture: omitted `spec.trustFlush` materializes the hourly CronJob |
| upgrade-abort | `keystone-upgrade-abort` | In-flight upgrade abort by reverting the image tag; the upgrade wedges in `Expanding` because its target is pulled from an unresolvable `registry.invalid` repository, whichever releases exist, and recovers cleanly |
| upgrade-flow | `keystone-upgrade-flow` | Expand-migrate-contract phase progression with `installedRelease`/`targetRelease` bookkeeping, skip-level rejection (`UpgradePathInvalid`) and downgrade rejection after the completed upgrade (`DowngradeNotSupported`, image kept) |
| uwsgi | `keystone-uwsgi` | `spec.uwsgi` defaulting and propagation into the uWSGI command line |
| [vertical-autoscaling](#vertical-autoscaling) | `keystone-vertical` | `spec.deployment.verticalAutoscaling` on a cluster without the VerticalPodAutoscaler: `VPAReady=False/VPANotInstalled` naming `autoscaling.k8s.io` while the Deployment and the API converge and `Ready` stays `False`; removing the opt-in returns `VPAReady=True/VPANotRequired` and `Ready=True` |

---

## Test Suite Details

### basic-deployment

**File:** `tests/e2e/keystone/basic-deployment/chainsaw-test.yaml`

**Purpose:** Validates the full happy-path reconciliation cycle in managed mode. Deploys
a Keystone CR with `clusterRef` and verifies that the five sub-conditions it asserts
(`SecretsReady`, `DatabaseReady`, `FernetKeysReady`, `DeploymentReady`,
`BootstrapReady`) progress to True,
the aggregate Ready condition reaches True with reason `AllReady`, all owned resources
exist, a ConfigMap with the expected prefix exists, and the Keystone API at `/v3` is
accessible. It then reads the keystone-operator's own metric families and holds the Ready
CR to a budget of zero events over 120 seconds.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-basic` in managed mode |
| 2 | Assert all sub-conditions and Ready | `assert` (5m) | Verifies SecretsReady=True (SecretsAvailable), DatabaseReady=True (DatabaseSynced), FernetKeysReady=True (FernetKeysAvailable), DeploymentReady=True, BootstrapReady=True (BootstrapComplete), Ready=True (AllReady). Condition order follows the `subConditionTypes` display order. |
| 3 | Assert Deployment and Service | `assert` (5m) | Deployment `keystone-basic-api` has `availableReplicas > 0`; Service `keystone-basic-api` has port 5000 |
| 4 | Assert Fernet resources | `assert` (5m) | CronJob `keystone-basic-fernet-rotate`, Secret `keystone-basic-fernet-keys`, ServiceAccount/Role/RoleBinding `keystone-basic-fernet-rotate`, PushSecret `keystone-basic-fernet-keys-backup` all exist |
| 5 | Assert OpenBao PushSecret sync | `assert` (5m) | PushSecrets `keystone-basic-fernet-keys-backup` and `keystone-basic-credential-keys-backup` report `Ready=True`, so ESO wrote both key sets to OpenBao |
| 6 | Assert ConfigMap exists | `script` | `kubectl get cm -n openstack -o name \| grep keystone-basic-config-` — verifies a ConfigMap with content-hash suffix exists |
| 7 | Assert API accessibility | `script` | `curl -sf http://keystone-basic-api.openstack.svc:5000/v3` — verifies the Keystone API responds |
| 8 | Assert the operator's metric families | `script` (120s) | `tests/e2e/lib/assert-operator-metric-families.sh` against the `keystone-operator` release: a reconcile is counted, no error carries `condition_type="UNKNOWN"`, and the `keystone_operator_db_sync_total{result="succeeded"}` and `keystone_operator_db_sync_duration_seconds_count` samples of `keystone-basic` are exported |
| 9 | Assert the steady state emits no event | `script` (4m) | `tests/e2e/lib/assert-steady-state-events.sh`: no event on `keystone-basic` appears or raises its count in 120 seconds, and the CR stays Ready at the same generation |

**Fixtures:** `00-keystone-cr.yaml`

---

### missing-secret

**File:** `tests/e2e/keystone/missing-secret/chainsaw-test.yaml`

**Purpose:** Validates the reconciler's secret dependency recovery behavior. Applies a
Keystone CR referencing non-existent ExternalSecret names, verifies the operator sets
SecretsReady=False and waits, then creates the missing ExternalSecrets and verifies
recovery to Ready=True.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with non-existent secret references | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-missing-secret` with unique secretRef names |
| 2 | Assert SecretsReady=False | `assert` (5m) | SecretsReady condition has status False with reason WaitingForDBCredentials |
| 3 | Create the missing ExternalSecrets | `apply` | Applies `01-late-secrets.yaml` — creates ExternalSecrets pointing to existing OpenBao paths |
| 4 | Assert recovery to Ready=True | `assert` (5m) | Ready condition transitions to True with reason AllReady |

**Fixtures:** `00-keystone-cr.yaml`, `01-late-secrets.yaml`

**Design note:** Recovery requires creating ExternalSecrets (not raw Secrets) because
`reconcileSecrets` calls `WaitForExternalSecret`, which checks for the ExternalSecret
resource. The ESO controller then syncs the actual Secret.

---

### fernet-rotation

**File:** `tests/e2e/keystone/fernet-rotation/chainsaw-test.yaml`

**Purpose:** Validates the CronJob-based Fernet key rotation mechanism end-to-end.
Deploys a Keystone CR, verifies the CronJob schedule matches `spec.fernet.rotationSchedule`
and the CronJob carries `concurrencyPolicy: Forbid`, triggers a manual rotation via
`kubectl create job --from=cronjob`, and verifies that Secret data changes, pod UIDs remain
stable (no Deployment rollout), and a fernet token obtained before rotation still validates
after rotation. The suite then deletes the staging Secret between rotations and checks that
the operator recreates it empty and commits nothing from it. A second rotation past
`maxActiveKeys: 3` purges the key that signed the pre-rotation token: that token gets a 401,
and tokens signed by surviving keys validate. A third rotation runs while an admission
policy refuses its first staging PATCH: the kubelet restarts the rotate container in the
same pod, and the retry commits the keys of one rotation. Last, an admission policy holds
a scheduled rotation Job, and the CronJob controller skips the next tick with
`JobAlreadyActive`.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-fernet` |
| 2 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Assert CronJob schedule, suspend and concurrency policy | `assert` (5m) | CronJob `keystone-fernet-fernet-rotate` has schedule `"0 0 * * 0"`, `suspend: false` and `concurrencyPolicy: Forbid` |
| 4 | Trigger rotation, verify no rollout, validate token | `script` (180s) | Records pod UIDs and Secret hash before rotation, obtains a fernet token and stores it in the Secret `keystone-fernet-pre-rotation-token` for step 7, creates manual Job from CronJob, verifies Secret data hash changed, asserts pod UIDs are unchanged (no rollout), validates the pre-rotation token still works. The token reaches `kubectl` over stdin, and server-side apply keeps it out of the `kubectl.kubernetes.io/last-applied-configuration` annotation. A step `cleanup` deletes the token Secret at the end of the test; the `catch` lists it without its `token` key |
| 4.5 | Assert rotation Role RBAC split | `script` | Role `keystone-fernet-fernet-rotate` grants `patch` on the staging Secret `keystone-fernet-fernet-keys-rotation` and never on the production Secret `keystone-fernet-fernet-keys` |
| 5 | Assert Ready=True maintained | `assert` (5m) | Ready=True with reason AllReady after rotation |
| 6 | Delete the staging Secret between rotations | `script` (180s) + `assert` (5m) | Waits up to 60 s for `keystone-fernet-fernet-keys-rotation` and records its UID, the production Secret's data hash and `cobaltcore.c5c3.io/rotation-completed-at` annotation, and the `FernetKeysRotated` event count. Deletes the staging Secret. Within 60 s a Secret of that name with a new UID must appear, with the label `cobaltcore.c5c3.io/rotation-target=fernet-keys`, a controller owner reference to Keystone `keystone-fernet`, no data and no completion annotation. After 20 s the hash, the annotation and the event count are unchanged and no `RotationRejected` event exists. The `assert` requires `FernetKeysReady=True` with reason `FernetKeysAvailable` and `Ready=True` with reason `AllReady` |
| 7 | Prune a key past maxActiveKeys | `script` (600s) | Reads the stored token `T0`, waits up to 150 s until the API pod projects the production key set `B`, issues `T1`, and computes `E = {0, max(B), max(B)+1}` and the keys to purge, `P = B \ E`. Runs the manual Job `keystone-fernet-manual-rotate-2`, requires the production keys to equal `E` with 3 keys (`{0,3,4}` on an unrotated week), and waits for the projection of `E`. Then `T0` must get HTTP 401 within 60 s, and `T1` and a freshly issued `T2` must get HTTP 200 |
| 8 | Retry a refused staging PATCH in place | `script` (90s) + `script` (660s) | The first script waits up to 60 s for the staging Secret `keystone-fernet-fernet-keys-rotation`, applies `05-refuse-staging-patch.yaml` with `kubectl`, and waits up to 30 s until a server-side dry-run PATCH of that Secret as the rotation ServiceAccount is refused with the policy message. The second waits for the projection of the production key set `B`, issues `T3`, computes `E = {0, max(B), max(B)+1}` (`{0,4,5}` on an unrotated week) and runs the manual Job `keystone-fernet-manual-rotate-3`. Within 90 s the container `fernet-rotate` must report `restartCount >= 1`, and its `--previous` log or, once the restarted attempt has failed as well, its current log must hold the policy message. The script then deletes the policy, waits up to 180 s for the Job to complete, and requires one Job pod and the production keys `E`. The new primary `max(B)+1` must hold the old key 0, key `max(B)` must keep its value, and the current log must show the `Retry: restoring ...` line. After the projection of `E`, `T3` must get HTTP 200. A step `finally` deletes the policy, pass or fail |
| 9 | Hold a scheduled rotation under Forbid | `apply` + `script` (60s) + `patch` + `assert` + `script` (240s) + `patch` + `assert` + `script` (60s) | Applies `02-hold-scheduled-rotation.yaml` and waits up to 30 s until a server-side dry run of a probe pod with the rotation labels is refused with the policy message. Patches the schedule to `* * * * *` (`03-patch-schedule-every-minute.yaml`) and asserts it with `concurrencyPolicy: Forbid`. Expects one unfinished Job named `keystone-fernet-fernet-rotate-<minutes>`, listed in the CronJob's `status.active` and without a pod, then a `JobAlreadyActive` event on the CronJob, then still that one Job and an unchanged production key hash. Restores `"0 0 * * 0"` (`04-patch-schedule-weekly.yaml`), deletes the held Job and waits for an empty `status.active`. A step `finally` restores `"0 0 * * 0"` and then deletes Jobs with the scheduled name |
| 10 | Pause rotation via spec.fernet.suspend | `patch` | Applies `01-patch-suspend-fernet.yaml` — `spec.fernet.suspend: true` |
| 11 | Assert CronJob preserved, suspended, schedule unchanged | `assert` (5m) | CronJob `keystone-fernet-fernet-rotate` keeps schedule `"0 0 * * 0"` with `suspend: true` |
| 12 | Assert the operator's metric families | `script` (120s) | `tests/e2e/lib/assert-operator-metric-families.sh` against the `keystone-operator` release, with the `keystone_operator_key_rotation_age_seconds{key_type="fernet"}` sample of `keystone-fernet` that the rotations of steps 4, 7 and 8 set |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-suspend-fernet.yaml`, `02-hold-scheduled-rotation.yaml`,
`03-patch-schedule-every-minute.yaml`, `04-patch-schedule-weekly.yaml`, `05-refuse-staging-patch.yaml`

**Rotation verification approach:** The script step verifies in-place key delivery
by asserting that pod UIDs remain unchanged after rotation. The Secret
data hash comparison confirms the rotation actually occurred, while the token
validation confirms that Keystone can still decrypt tokens issued with the
previous key set.

**Design notes:**

- Step 8 refuses the staging PATCH with the ValidatingAdmissionPolicy
  `keystone-fernet-refuse-staging-patch`. It matches only an UPDATE (the API server
  admits a PATCH as UPDATE) of `keystone-fernet-fernet-keys-rotation` in `openstack` by
  the ServiceAccount `keystone-fernet-fernet-rotate`, so the operator's own writes pass.
  The step applies and deletes the policy itself with `kubectl`, because it lifts the
  policy partway through the step, and its `finally` deletes the policy when the step
  fails. The step runs before the hold step, because the hold policy stays in force to
  the end of the test and would refuse the rotation pod.
- Step 9 holds a scheduled Job with the ValidatingAdmissionPolicy
  `keystone-fernet-hold-rotation`. It refuses every pod in `openstack` that carries
  `app.kubernetes.io/instance=keystone-fernet` and
  `app.kubernetes.io/component=fernet-rotation`. A refused pod creation does not count as
  a pod failure, so the Job does not fail before its one-hour `activeDeadlineSeconds` and
  stays in the CronJob's `status.active`, where
  the next tick meets Forbid. The step waits for the dry-run probe to be refused before it
  moves the schedule to every minute, so the first tick cannot run a real rotation.
  Chainsaw deletes the policy and its binding at the end of the test, before it runs any
  step `cleanup`. The step `finally` therefore runs while the policy is still in force,
  pass or fail: it restores the weekly schedule, so no tick starts a new Job, and then
  deletes Jobs with the scheduled name. A failed run leaves no held Job that starts once
  the policy is gone.
- `kubectl create job --from=cronjob/...` names the CronJob as the Job's controller, but
  the CronJob controller never adds such a Job to `status.active` and records
  `UnexpectedJob` for it while it runs. Forbid therefore never counts the manual Jobs
  `keystone-fernet-manual-rotate`, `keystone-fernet-manual-rotate-2` and
  `keystone-fernet-manual-rotate-3`, and step 9 counts only Jobs named
  `keystone-fernet-fernet-rotate-<minutes>`, the name the CronJob controller gives a
  scheduled Job. Step 9 also skips finished Jobs, because a completed weekly run carries
  the same name.
- The fixture's `extraConfig.cache.enabled: "false"` must stay. Keystone reads the key
  repository on every token operation, and with caching on, a token cached on issue can
  keep validating after its key is purged, so step 7 would see HTTP 200 for `T0`.
- The weekly schedule (Sunday 00:00 UTC) can add a rotation between steps 4 and 8.
  Steps 7 and 8 derive `E` from the key set they read first, so an extra rotation before
  them shifts `E` and the checks still hold. A rotation inside the 20 s window of step 6
  changes the production hash and fails that step.

---

### scale

**File:** `tests/e2e/keystone/scale/chainsaw-test.yaml`

**Purpose:** Validates that patching `spec.deployment.replicas` on the Keystone CR propagates to the
underlying Deployment. Tests both scale-up (3→5) and scale-down (5→2).

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with replicas: 3 | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-scale` with replicas: 3 |
| 2 | Assert Ready and initial replica count | `assert` (5m) | Ready=True, Deployment `keystone-scale-api` has replicas: 3 and availableReplicas >= 3 |
| 3 | Scale up to 5 replicas | `patch` | Applies `01-patch-scale-up.yaml` — patches replicas to 5 |
| 4 | Assert scale-up | `assert` (5m) | Deployment has replicas: 5 and availableReplicas >= 5 |
| 5 | Scale down to 2 replicas | `patch` | Applies `02-patch-scale-down.yaml` — patches replicas to 2 |
| 6 | Assert scale-down | `assert` (5m) | Deployment has replicas: 2 and availableReplicas == 2 (exact equality to verify scale-down completed) |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-scale-up.yaml`, `02-patch-scale-down.yaml`

---

### deletion-cleanup

**File:** `tests/e2e/keystone/deletion-cleanup/chainsaw-test.yaml`

**Purpose:** Validates that deleting a Keystone CR triggers Kubernetes garbage collection
of all owned resources via owner references. Deploys a CR, waits for Ready, deletes the
CR, then error-asserts that all owned resources return NotFound.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-cleanup` |
| 2 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Delete the Keystone CR | `delete` | Deletes Keystone CR `keystone-cleanup` from namespace `openstack` |
| 4 | Assert all owned resources deleted | `error` | 12 error assertions verifying NotFound for: Deployment `keystone-cleanup`, Service `keystone-cleanup`, CronJob `keystone-cleanup-fernet-rotate`, Secret `keystone-cleanup-fernet-keys`, ServiceAccount `keystone-cleanup-fernet-rotate`, Role `keystone-cleanup-fernet-rotate`, RoleBinding `keystone-cleanup-fernet-rotate`, PushSecret `keystone-cleanup-fernet-keys-backup`, Job `keystone-cleanup-db-sync`, Database `keystone-cleanup`, User `keystone-cleanup`, Grant `keystone-cleanup` |
| 5 | Assert dynamically-named ConfigMap deleted | `script` | Inverted grep verifies no ConfigMap matching `keystone-cleanup-config-*` remains after garbage collection |

**Fixtures:** `00-keystone-cr.yaml`

**Design note:** The bootstrap Job (`keystone-cleanup-bootstrap`) has
`TTLSecondsAfterFinished: 300` and may be TTL-cleaned before CR deletion. It is excluded
from the error assertions because its absence is expected in both cases.

---

### policy-overrides

**File:** `tests/e2e/keystone/policy-overrides/chainsaw-test.yaml`

**Purpose:** Validates oslo.policy integration. Applies a policy source ConfigMap and a
Keystone CR with `policyOverrides.configMapRef`, then verifies the generated ConfigMap
contains a `policy.yaml` data key with the expected rules and that `keystone.conf`
contains the `[oslo_policy]` section with `policy_file`.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply policy source ConfigMap | `apply` | Applies `00-policy-cm.yaml` — ConfigMap with policy rules (e.g., `identity:list_users`) |
| 2 | Apply Keystone CR with policyOverrides | `apply` | Applies `01-keystone-cr.yaml` — Keystone CR `keystone-policy` with `policyOverrides.configMapRef` |
| 3 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 4 | Verify ConfigMap contents | `script` | Gets ConfigMap matching `keystone-policy-config-*`, verifies `policy.yaml` data key contains `identity:list_users` and `keystone.conf` contains `policy_file` |

**Fixtures:** `00-policy-cm.yaml`, `01-keystone-cr.yaml`

---

### middleware-config

**File:** `tests/e2e/keystone/middleware-config/chainsaw-test.yaml`

**Purpose:** Validates WSGI middleware pipeline customization. Applies a Keystone CR with
custom `spec.middleware` entries and verifies the generated ConfigMap's `api-paste.ini`
contains the custom filter name in the pipeline definition and the filter factory entry.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with custom middleware | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-middleware` with middleware entries (e.g., `audit` filter with `keystonemiddleware.audit` factory) |
| 2 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 3 | Verify api-paste.ini contents | `script` | Gets ConfigMap matching `keystone-middleware-config-*`, verifies `api-paste.ini` contains `audit` filter reference and `keystonemiddleware.audit` factory |

**Fixtures:** `00-keystone-cr.yaml`

---

### brownfield-database

**File:** `tests/e2e/keystone/brownfield-database/chainsaw-test.yaml`

**Purpose:** Validates brownfield database support — using an explicit `database.host`
without `clusterRef`. Verifies no MariaDB CRs (Database, User, Grant) are created, the
generated `keystone.conf` connection string contains the explicit host, and the
reconciliation completes to Ready=True.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with brownfield database | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-brownfield` with `database.host` and `database.port` (no `clusterRef`) |
| 2 | Assert no MariaDB CRs created | `error` | 3 error assertions verifying NotFound for: Database `keystone-brownfield`, User `keystone-brownfield`, Grant `keystone-brownfield` |
| 3 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 4 | Verify keystone.conf connection | `script` | Gets ConfigMap matching `keystone-brownfield-config-*`, verifies `keystone.conf` `[database]` section contains `openstack-db.openstack.svc.cluster.local` |

**Fixtures:** `00-keystone-cr.yaml`

---

### db-sync-partial-schema

**File:** `tests/e2e/keystone/db-sync-partial-schema/chainsaw-test.yaml`

**Purpose:** Validates that the db-sync Job recovers a schema an interrupted first
`db_sync` left behind. The suite seeds the database `keystone_partial_schema` by hand
with three tables of the initial revision `27e647c0fad4` and no rows in them, an empty
`alembic_version`, and the `<<keystone.domain.root>>` project row. A plain
`keystone-manage db_sync` fails on that schema with MySQL error 1050. The pre-step
`scripts/db_sync_reset.py` drops the four tables, and the `db_sync` of the same attempt
rebuilds the schema. The seed replaces a real interruption because
`controlplane-provisioning-mariadb-kill` catches the defect only when its kill lands
inside the migration.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | seed-partial-schema | `script` | Reads the root Secret from `.spec.rootPasswordSecretKeyRef` of `mariadb/openstack-db` and pipes the password on stdin into `kubectl exec openstack-db-0 -c mariadb`, which drops and re-creates `keystone_partial_schema` and seeds it (up to three attempts under `timeout 20`) |
| 2 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml`: Keystone CR `keystone-partial-schema` in managed mode with `database.database: keystone_partial_schema` |
| 3 | Wait for Ready=True | `assert` (5m) | Ready=True with reason AllReady |
| 4 | verify-reset | `script` | Job `keystone-partial-schema-db-sync` reads `succeeded` 1 and no failed pod, and its log carries `db-sync-reset: dropped 4 tables an interrupted first db_sync left (recorded revisions: none): access_rule, alembic_version, application_credential, project` |

**Fixtures:** `00-keystone-cr.yaml`

---

### image-upgrade

**File:** `tests/e2e/keystone/image-upgrade/chainsaw-test.yaml`

**Purpose:** Validates non-disruptive rolling image upgrades. Deploys a Keystone CR,
waits for Ready, patches `spec.image.tag` to a new value, then verifies the Deployment
container image updates and Ready=True is maintained after the rollout completes.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-upgrade` |
| 2 | Assert Ready and initial image tag | `assert` + `script` (5m) | Ready=True; script verifies Deployment `keystone-upgrade-api` container image contains `2026.1` |
| 3 | Patch image tag | `patch` | Applies `01-patch-image.yaml` — patches `spec.image.tag` to `2026.1-upgraded` |
| 4 | Assert image updated and Ready maintained | `script` (120s) + `assert` (5m) | Script polls up to 120s to verify Deployment image contains `2026.1-upgraded`; assert verifies Ready=True, availableReplicas > 0, and updatedReplicas == replicas (rollout complete) |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-image.yaml`

---

### image-pull-policy

**File:** `tests/e2e/keystone/image-pull-policy/chainsaw-test.yaml`

**Purpose:** Validates the resolution of the image pull policy at render time.
A CR whose `spec.image` names a tag and no `pullPolicy` takes the operator
default, which `hack/ci-deploy-operator.sh` sets to `IfNotPresent`; an operator
that ignored `--default-image-pull-policy` would render `Always` for the tag, so
the suite fails by design against an operator installed without the flag. An
explicit `pullPolicy` then wins on every rendered container, re-runs the Job
gated by its pod template hash and leaves the key-gated bootstrap Job alone.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR without pullPolicy | `apply` | `00-keystone-cr.yaml`: Keystone CR `keystone-ipp`, tag `2026.1`, one replica |
| 2 | Assert Ready and the operator default | `assert` + `script` | Ready=True (AllReady); every container of Deployment `keystone-ipp` and of Job `keystone-ipp-db-sync` carries `IfNotPresent`; the UIDs of the db-sync and bootstrap Jobs go into ConfigMap `keystone-ipp-job-uids` |
| 3 | Patch: pullPolicy Never | `patch` | `01-patch-pull-policy-never.yaml`: `spec.image.pullPolicy: Never` |
| 4 | Assert Never, the re-run and Ready | `assert` + `script` | Every container and init container of the Deployment, the db-sync Job and the `fernet-rotate` and `credential-rotate` CronJobs carries `Never`; Ready=True at `observedGeneration: 2`; a script checks every CronJob of the instance (trust-flush included), a new db-sync UID and an unchanged bootstrap UID |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-pull-policy-never.yaml`

---

### release-upgrade

**File:** `tests/e2e/keystone/release-upgrade/chainsaw-test.yaml`

**Purpose:** Validates a cross-release upgrade from OpenStack 2026.1 to 2026.2 via the
expand-migrate-contract database migration path (keystone 29.0.0 → 30.0.0). Deploys a
Keystone CR with tag 2026.1, verifies the Keystone API at `/v3` is accessible, patches
`spec.image.tag` to 2026.2, then verifies the expand/migrate/contract Jobs are created,
the Deployment image updates to 2026.2, the rollout completes, `installedRelease` reaches
2026.2, and the Keystone API remains accessible post-upgrade. After the upgrade, the suite
also issues an admin token scoped to the project `admin` (the authenticated request
[#420](https://github.com/c5c3/cobaltcore/issues/420) asked for), validates a token issued
before the upgrade, and checks that the fernet key Secret is unchanged unless a rotation
applied in between.

This differs from `image-upgrade`, which tests same-release tag swaps
(2026.1→2026.1-upgraded) without database migration, and from `upgrade-flow`,
which focuses on internal state machine mechanics (skip-level rejection).

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with tag 2026.1 | `apply` | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-release-upgrade` in managed mode with tag 2026.1 |
| 2 | Assert Ready and initial image | `assert` (5m) + `script` | Ready=True (AllReady), `installedRelease`=2026.1; script verifies Deployment `keystone-release-upgrade-api` container image ends with `2026.1` |
| 3 | Verify API and admin token before upgrade | `script` (90s) + `script` (120s) | `kubectl run curl-test-release-pre` with python3 `urllib.request` verifies GET `/v3` through the Service. The second script runs `kubectl exec` into the API pod and sends `POST /v3/auth/tokens` to `localhost:5000` as user `admin`, scoped to the project `admin` in the domain `default`: HTTP 201, a non-empty `X-Subject-Token` and user, project and role `admin` in the body. It hashes `.data` of `keystone-release-upgrade-fernet-keys` and stores token, hash and the `cobaltcore.c5c3.io/rotation-completed-at` annotation of that Secret (`none` when absent) in the Secret `keystone-release-upgrade-pre-hop`, which a `cleanup` on this step deletes at the end of the test |
| 4 | Patch image tag to 2026.2 | `patch` | Applies `01-patch-upgrade.yaml` — patches `spec.image.tag` to `2026.2` |
| 5 | Assert upgrade completes | `assert` (5m) + `script` | `installedRelease`=2026.2, Ready=True (AllReady); scripts verify db-expand, db-migrate, db-contract Jobs exist and Deployment image ends with `2026.2`; assert verifies `updatedReplicas == replicas` and `availableReplicas > 0` |
| 6 | Verify API, admin token and token continuity after upgrade | `script` (90s) + `script` (120s) | `kubectl run curl-test-release-post` with python3 `urllib.request` verifies GET `/v3` post-upgrade. The second script issues a fresh admin token from a 2026.2 API pod with the checks of step 3, then validates the stashed token with `GET /v3/auth/tokens` (fresh token as `X-Auth-Token`, stashed token as `X-Subject-Token`): HTTP 200 naming user `admin`. Last, the hash of `.data` of `keystone-release-upgrade-fernet-keys` must equal the stashed one. A changed hash passes with a NOTE only when the `rotation-completed-at` annotation carries a timestamp other than the stashed one |

**Fixtures:** `00-keystone-cr.yaml` (keystone-side caching off through
`extraConfig.cache.enabled: "false"`), `01-patch-upgrade.yaml`

**Diagnostics:** Steps 2 and 5 include `catch` blocks that capture pod logs (including
`--previous`), Job logs, pod descriptions, Job status, and namespace events for
debugging failures. The `catch` blocks of steps 3 and 6 capture the Keystone Service,
Endpoints and pods, the events, the API pod log, and the Secret
`keystone-release-upgrade-pre-hop` without its `token` key and without the
`kubectl.kubernetes.io/last-applied-configuration` annotation, which holds the token too.

**Design notes:**

- API accessibility is tested via `kubectl run` with python3 `urllib.request` rather than
  direct curl, because the test pods use the Keystone service image which provides python3.
  The curl test pods use unique names (`curl-test-release-pre`, `curl-test-release-post`) to
  avoid conflicts during parallel execution.
- The admin token probes run with `kubectl exec` in the API pod against `localhost:5000`:
  the keystone image ships python3 and no curl, and the loopback request takes no
  kube-proxy hop that a retry would have to cover. The `GET /v3` probes keep their
  `kubectl run` form because they prove the Service path. The pre-upgrade token is a
  credential, so step 3 stores it in a Secret, written with
  `kubectl create --dry-run=client -o yaml | kubectl apply -f -` so a local re-run on the
  same cluster passes. A `cleanup` on step 3 deletes that Secret. Chainsaw runs it at the
  end of the test whether the test passed or any step failed; a `finally` on step 6
  would not run once step 4 or 5 fails. The fixture turns keystone-side caching off like
  every keystone suite that authenticates as `admin` by name: the keystone suites run
  four at a time against one `openstack-memcached`, keystone's cache keys carry no
  per-instance discriminator, and a neighbouring suite's cached `admin` user or project
  would answer the token probes with 401 or 404
  ([#227](https://github.com/c5c3/cobaltcore/issues/227) tracks per-instance keys).
- The fixture's weekly fernet rotation runs at Sunday 00:00 UTC and can apply between
  steps 3 and 6. The operator writes the rotated keys and a new
  `cobaltcore.c5c3.io/rotation-completed-at` timestamp in one update, so step 6 reads
  changed keys under a new timestamp as that rotation and changed keys under the
  stashed timestamp as an upgrade defect. Both steps read keys and annotation from one
  `kubectl get`. The pre-hop token still has to validate: with three active keys, one
  rotation keeps the key that signed it. The `fernet-rotation` suite owns rotation.
- The 5-minute assert timeout accommodates the full expand→migrate→rolling-update→contract
  cycle.
- This test complements `upgrade-flow`: `upgrade-flow` validates internal state
  machine behavior (skip-level rejection, phase transitions), while `release-upgrade`
  validates the user-facing lifecycle (API accessibility before/after, Deployment rollout).

---

### concurrent-cr-conflicts

**File:** `tests/e2e/keystone/concurrent-cr-conflicts/chainsaw-test.yaml`

**Purpose:** Validates that two Keystone CRs sharing the same `secretRef` and
`adminPasswordSecretRef` can coexist in the same namespace without interference.
Both CRs reach Ready=True with unique owned resources (Deployments, Services,
CronJobs, ConfigMaps), and deleting one CR does not affect the other's health
or resources.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply both CR fixtures | `apply` | Applies `00-keystone-cr-a.yaml` and `01-keystone-cr-b.yaml` — Keystone CRs `keystone-concurrent-a` and `keystone-concurrent-b` sharing `keystone-db` secretRef and `keystone-admin` adminPasswordSecretRef |
| 2 | Assert both CRs Ready=True | `assert` (5m) | Both CRs have Ready=True with reason AllReady |
| 3 | Assert unique Deployments and Services | `assert` (5m) | Deployment `keystone-concurrent-a-api` and `keystone-concurrent-b-api` both have `availableReplicas > 0`; Services `keystone-concurrent-a-api` and `keystone-concurrent-b-api` both have port 5000 |
| 4 | Assert unique Fernet CronJobs and ConfigMaps | `assert` + `script` | CronJobs `keystone-concurrent-a-fernet-rotate` and `keystone-concurrent-b-fernet-rotate` exist; script verifies ConfigMaps `keystone-concurrent-a-config-*` and `keystone-concurrent-b-config-*` exist |
| 5 | Delete CR-A and assert cleanup | `delete` + `error` + `script` | Deletes Keystone CR `keystone-concurrent-a`; error assertions verify Deployment, Service, and CronJob for CR-A are deleted; script verifies exactly 1 Deployment with `app.kubernetes.io/name=keystone` remains |
| 6 | Assert CR-B still Ready | `assert` (5m) | CR-B has Ready=True with reason AllReady and Deployment `keystone-concurrent-b-api` has `availableReplicas > 0` |

**Fixtures:** `00-keystone-cr-a.yaml`, `01-keystone-cr-b.yaml`

**Catch blocks:** Steps 2–6 include catch blocks dumping CR status, Deployment status,
Service status, CronJob status, ConfigMap list, pod logs, and namespace events.

**Design notes:**

- Both CRs share `secretRef: keystone-db` and `adminPasswordSecretRef: keystone-admin`
  to exercise the `secretToKeystoneMapper` under resource contention — the mapper must
  enqueue both CRs when the shared Secret changes without causing cross-CR interference.
- Each CR uses a unique database name (`keystone_concurrent_a`, `keystone_concurrent_b`)
  to avoid MariaDB conflicts while sharing the same `clusterRef`.
- Step 5 verifies isolation by checking that exactly 1 Deployment remains after CR-A
  deletion, confirming owner references correctly scope garbage collection.

---

### config-pruning

**File:** `tests/e2e/keystone/config-pruning/chainsaw-test.yaml`

**Purpose:** Validates that the immutable ConfigMap pruning logic caps
the number of historical config ConfigMaps at `retain + 1` (current + 3
historical = 4 max) across multiple config changes, while keeping
`Ready=True` throughout the churn.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-pruning` |
| 2 | Assert Ready=True | `assert` (5m) | Ready condition reaches True/AllReady |
| 3 | Trigger 4 config changes | `script` | Repeated `spec.extraConfig` patches to force new ConfigMap revisions |
| 4 | Assert ConfigMap count ≤ 4 | `script` | Counts ConfigMaps matching the base prefix and asserts ≤ `retain + 1` |

**Fixtures:** `00-keystone-cr.yaml`

---

### events

**File:** `tests/e2e/keystone/events/chainsaw-test.yaml`

**Purpose:** Verifies that the reconciler emits Kubernetes Events for key
lifecycle transitions that unit tests with `FakeRecorder` cannot
observe: `BootstrapComplete`, `DatabaseSynced`, `FernetKeysGenerated`,
`CredentialKeysGenerated`.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-events` |
| 2 | Assert DatabaseReady, BootstrapReady, Ready | `assert` (5m) | `DatabaseReady=True/DatabaseSynced`, `BootstrapReady=True/BootstrapComplete`, `Ready=True/AllReady` |
| 3 | Assert events exist | `script` | `kubectl get events` filtered by reason — all four expected reasons present |

**Fixtures:** `00-keystone-cr.yaml`

---

### graceful-shutdown

**File:** `tests/e2e/keystone/graceful-shutdown/chainsaw-test.yaml`

**Purpose:** Ensures the reconciler configures the Keystone API Deployment with
the graceful-shutdown shape:
`terminationGracePeriodSeconds=30`, a `preStop` exec hook (`sleep 5`), and a
startup probe (`HTTP GET /v3:5000`, `failureThreshold=30`).

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-graceful-shutdown` |
| 2 | Assert Ready=True | `assert` (5m) | Ready condition reaches True/AllReady |
| 3 | Assert Deployment pod spec shape | `assert` | `terminationGracePeriodSeconds: 30`, preStop exec hook present, startupProbe shape |

**Fixtures:** `00-keystone-cr.yaml`

---

### healthcheck

**File:** `tests/e2e/keystone/healthcheck/chainsaw-test.yaml`

**Purpose:** Validates the post-Deployment HTTP health check sub-reconciler. The aggregate `Ready` condition must not flip to `True` until the
separate `KeystoneAPIReady` condition is `True` with reason `APIHealthy`,
meaning the API genuinely responds after `DeploymentReady`.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-healthcheck` |
| 2 | Assert DeploymentReady=True | `assert` (5m) | Reason `DeploymentReady` |
| 3 | Assert KeystoneAPIReady=True | `assert` (5m) | Reason `APIHealthy` |
| 4 | Assert Ready=True | `assert` (5m) | Aggregate Ready after API healthcheck succeeds |

**Fixtures:** `00-keystone-cr.yaml`

---

### policy-validation

**File:** `tests/e2e/keystone/policy-validation/chainsaw-test.yaml`

**Purpose:** Exercises the policy-validation gating sub-reconciler.
When `policyOverrides` is set, a validation Job runs before the Deployment is
reconciled; `PolicyValidReady` transitions `False/PolicyValidationInProgress →
True/PolicyValidationPassed`. Removing `policyOverrides` flips the condition
to `True/PolicyValidationNotRequired` and cleans up the Job.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply policy ConfigMap | `apply` | `00-policy-cm.yaml` |
| 2 | Apply Keystone CR with policyOverrides | `apply` | `01-keystone-cr.yaml` — Keystone CR `keystone-policy-validation` |
| 3 | Assert PolicyValidReady=True | `assert` (5m) | Reason `PolicyValidationPassed` |
| 4 | Assert Ready=True | `assert` (5m) | Aggregate Ready with policyOverrides active |
| 5 | Patch: disable policyOverrides | `patch` | `02-patch-disable-policy.yaml` |
| 6 | Assert PolicyValidReady=True/NotRequired | `assert` (5m) | Validation Job garbage-collected |

**Fixtures:** `00-policy-cm.yaml`, `01-keystone-cr.yaml`, `02-patch-disable-policy.yaml`

---

### priority-class

**File:** `tests/e2e/keystone/priority-class/chainsaw-test.yaml`

**Purpose:** Validates `spec.deployment.priorityClassName` propagation:
a CR without the field yields an empty `priorityClassName` on the Deployment;
patching with a valid class sets it on the Deployment and, because `spec.jobs`
is unset, on the Jobs and CronJobs too; `spec.jobs.priorityClassName: ""` opts
the Jobs out while the Deployment keeps its class; patching `spec.deployment`
with empty string removes it.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Create PriorityClass | `apply` | `00-priority-class.yaml` (cluster-scoped) |
| 2 | Apply Keystone CR without priorityClassName | `apply` | `01-keystone-cr.yaml` — Keystone CR `keystone-pc` |
| 3 | Assert Ready and empty priorityClassName | `assert` + `script` | Deployment `keystone-pc` has empty `.spec.template.spec.priorityClassName` |
| 4 | Patch: set priorityClassName | `patch` | `02-patch-priority-class.yaml` — sets a valid class |
| 5 | Assert priorityClassName applied | `assert` | Deployment `keystone-pc`, Job `keystone-pc-db-sync` and CronJob `keystone-pc-fernet-rotate` carry `openstack-control-plane` |
| 6 | Patch: opt the Jobs out | `patch` | `04-patch-jobs-empty-priority-class.yaml` — `spec.jobs.priorityClassName: ""` |
| 7 | Assert the Jobs opted out | `assert` | Job `keystone-pc-db-sync` has no priority class; the Deployment keeps `openstack-control-plane` |
| 8 | Patch: clear priorityClassName | `patch` | `03-patch-empty-priority-class.yaml` |
| 9 | Assert priorityClassName cleared | `assert` | Deployment back to empty |

**Fixtures:** `00-priority-class.yaml`, `01-keystone-cr.yaml`,
`02-patch-priority-class.yaml`, `03-patch-empty-priority-class.yaml`,
`04-patch-jobs-empty-priority-class.yaml`

---

### node-placement

**File:** `tests/e2e/keystone/node-placement/chainsaw-test.yaml`

**Purpose:** Validates that `spec.deployment.nodeSelector`, `tolerations` and
`affinity` reach the Deployment pod template, and that the Jobs and CronJobs
fall back to them while `spec.jobs` is unset: the node selector, the
tolerations and the node affinity carry over, the pod anti-affinity does not.
The CR selects `kubernetes.io/os: linux` and requires it through node affinity,
so every kind node qualifies and the pods schedule.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with node placement | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-np` with a node selector, a `c5c3.io/e2e-dedicated` toleration, a required node affinity and a preferred pod anti-affinity |
| 2 | Assert Ready and the rendered placement | `assert` (5m) | Ready `AllReady`; Deployment `keystone-np` carries all three fields; Job `keystone-np-db-sync` carries the selector and the toleration, `(affinity.nodeAffinity != null): true` and `(affinity.podAntiAffinity == null): true`; CronJob `keystone-np-fernet-rotate` carries the selector |

**Fixtures:** `00-keystone-cr.yaml`

---

### schema-drift-detection

**File:** `tests/e2e/keystone/schema-drift-detection/chainsaw-test.yaml`

**Purpose:** Validates schema-drift detection after successful deployment. The reconciler runs a schema-check Job whose completion produces
`DatabaseReady=True` with the message
`"Database schema is up to date (revision verified)"`.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-schema-drift` |
| 2 | Assert DatabaseReady and revision message | `assert` (5m) | Condition message contains "revision verified" |
| 3 | Assert schema-check Job | `assert` | Job exists and has `succeeded: 1` |

**Fixtures:** `00-keystone-cr.yaml`

---

### semantic-invariants

**File:** `tests/e2e/keystone/semantic-invariants/chainsaw-test.yaml`

**Purpose:** Asserts five Keystone CR `status` semantic invariants that no other E2E suite gates today: `status.endpoint` URL format, `ownerReferences` fan-out across every operator-managed kind, `status.observedGeneration` catch-up after a `spec` change, `condition.lastTransitionTime` monotonicity across a `True → False → True` flip, and immutability of the generated config ConfigMap. The fixture intentionally omits `spec.gateway`, `spec.networkPolicy`, and `spec.autoscaling` so the cluster-local `status.endpoint` surfaces and the `observedGeneration` assertion stays exact regardless of optional sub-condition reasons.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR and gate on Ready=True | `apply` + `assert` (5m) | Applies `00-keystone-cr.yaml` — Keystone CR `keystone-invariants` — and gates the rest of the suite on `Ready=True/AllReady` |
| 2 | Assert `status.endpoint` literal | `assert` (5m) | `status.endpoint == http://keystone-invariants.openstack.svc.cluster.local:5000/v3` exactly — fails on any port/scheme/path drift, not only on an empty value |
| 3 | Assert `ownerReferences` fan-out | `script` (2m) | Reads CR UID, then for every managed kind (Deployment, Service, PodDisruptionBudget; ConfigMap families resolved via `cobaltcore.c5c3.io/config-base` label, never by hash suffix; per-CR Secrets `*-db-connection`, `*-fernet-keys`, `*-credential-keys`; ServiceAccount/Role/RoleBinding/CronJob `*-{fernet,credential}-rotate`; PushSecrets `*-{fernet,credential}-keys-backup`) asserts `ownerReferences[0]` is `{kind: Keystone, controller: true, blockOwnerDeletion: true, uid: <CR UID>}` via `jq -e`; reports the offending GVK+name on mismatch |
| 4 | Assert `observedGeneration` tracking | `script` (6m) | Patches `spec.deployment.replicas` `1 → 2` to bump `metadata.generation`, then polls up to 5 minutes until every `status.conditions[*].observedGeneration` matches `metadata.generation`; lists lagging condition types on timeout |
| 5 | Assert `lastTransitionTime` monotonicity | `script` (12m) | Captures `T0` from `DeploymentReady.lastTransitionTime`, scales the Deployment to 0 replicas (immediate flip; side-steps `progressDeadlineSeconds`), polls for `DeploymentReady=False`, then waits for the operator to reconcile the Deployment back to `spec.replicas=2` (set by Step 4) and captures `T1` once `DeploymentReady=True` again; asserts `T1 > T0` via lexicographic compare on the RFC3339 UTC timestamps |
| 6 | Assert ConfigMap immutability | `script` | Resolves the active config ConfigMap by `cobaltcore.c5c3.io/config-base=keystone-invariants-config` (never by hash suffix), runs `kubectl patch` against `data.keystone.conf`, and asserts non-zero exit with `field is immutable` in stderr |

**Fixtures:** `00-keystone-cr.yaml`

**Source files contributing managed kinds.** Step 3 is the tracked counterpart of these files — a reviewer adding a new managed kind in any of them MUST extend the iteration list in Step 3, otherwise the ownership invariant can regress unnoticed:

- `operators/keystone/internal/controller/reconcile_deployment.go` — Deployment, Service, PodDisruptionBudget
- `operators/keystone/internal/controller/reconcile_fernet.go` — `*-fernet-keys` Secret, `*-fernet-rotate-script` ConfigMap family, `*-fernet-rotate` ServiceAccount/Role/RoleBinding/CronJob, `*-fernet-keys-backup` PushSecret
- `operators/keystone/internal/controller/reconcile_credential.go` — `*-credential-keys` Secret, `*-credential-rotate-script` ConfigMap family, `*-credential-rotate` ServiceAccount/Role/RoleBinding/CronJob, `*-credential-keys-backup` PushSecret
- `operators/keystone/internal/controller/reconcile_trustflush.go` — `*-trust-flush` CronJob (always materialised — the defaulting webhook at `operators/keystone/api/v1alpha1/keystone_webhook.go` populates `spec.trustFlush` whenever unset)
- `operators/keystone/internal/controller/reconcile_database.go` — MariaDB `Database`, `User`, and `Grant` CRs in managed mode (created via `database.EnsureDatabase` / `database.EnsureDatabaseUser` whenever `spec.database.clusterRef` is set, as the fixture does)
- `operators/keystone/internal/controller/reconcile_dbconnection_secret.go` — `*-db-connection` Secret
- `operators/keystone/internal/controller/reconcile_config.go` + `internal/common/config/config.go` (`CreateImmutableConfigMap`) — `*-config` ConfigMap family carrying the `cobaltcore.c5c3.io/config-base` label and `Immutable: true`
- `operators/keystone/internal/controller/reconcile_httproute.go` — HTTPRoute (only when `spec.gateway` is set — out of scope for this suite)
- `operators/keystone/internal/controller/reconcile_networkpolicy.go` — NetworkPolicy (only when `spec.networkPolicy` is set — out of scope)
- `operators/keystone/internal/controller/reconcile_hpa.go` — HorizontalPodAutoscaler (only when `spec.autoscaling` is set — out of scope)

---

### topology-spread

**File:** `tests/e2e/keystone/topology-spread/chainsaw-test.yaml`

**Purpose:** Validates `spec.deployment.topologySpreadConstraints` behavior:
`nil` (unset) injects the two default constraints (zone + hostname,
`MaxSkew=1`, `ScheduleAnyway`); a non-empty slice passes through verbatim;
an empty slice explicitly disables all constraints.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR without TSC | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-tsc` |
| 2 | Assert Ready + 2 default constraints | `assert` (5m) | Deployment `keystone-tsc-api` carries zone-spread and hostname-spread |
| 3 | Patch: custom TSC | `patch` | `01-patch-custom-tsc.yaml` |
| 4 | Assert custom TSC applied verbatim | `assert` | Deployment has the patched constraints exactly |
| 5 | Patch: empty TSC | `patch` | `02-patch-empty-tsc.yaml` |
| 6 | Assert TSC disabled | `assert` | Deployment has no constraints |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-custom-tsc.yaml`, `02-patch-empty-tsc.yaml`

---

### pod-security-restricted

**File:** `tests/e2e/keystone/pod-security-restricted/chainsaw-test.yaml`

**Purpose:** Validates that the Keystone reconciler admits its full workload — API
Deployment, bootstrap Job, db-sync Job, policy-validation Job, and the
fernet-rotation Pod — into a namespace labelled with
`pod-security.kubernetes.io/enforce=restricted`. The test asserts Ready=True/AllReady
within the 5-minute budget AND zero `FailedCreate` events carry the literal violation
`violates PodSecurity "restricted:latest"`. This is the executable regression net for
any future reconciler that forgets `restrictedSecurityContext()` — a unit-level test
exists in `security_context_test.go`, but only this E2E proves the helper is wired
into every Pod-creating reconciler call site.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply labelled namespace + ESO ExternalSecrets + policy CM, provision the per-tenant store | `apply` × 2 + `script` + 4× `assert` | Applies `00-namespace.yaml` — Namespace `keystone-pss-restricted-test` with **both** `pod-security.kubernetes.io/enforce=restricted` AND `pod-security.kubernetes.io/enforce-version=latest` labels, plus ExternalSecrets `keystone-admin` and `keystone-db` that authenticate through the namespace's own per-tenant `openbao-tenant-store` and read namespace-scoped paths (`bootstrap/keystone-pss-restricted-test/pss/admin`, `openstack/keystone/keystone-pss-restricted-test/pss/db`). Then applies `00-policy-cm.yaml` — ConfigMap `keystone-policy-source`, referenced by the CR's `policyOverrides.configMapRef` so `reconcilePolicyValidation` builds the policy-validation Job (without it the reconciler short-circuits and that Pod is never admitted). A `script` then provisions the per-tenant store via `setup-eso-tenant.sh` and seeds those two paths by copying the shared admin/db values (#606, so the `keystone` user password still matches `brownfield-database`). Asserts the PSS labels are present so a typo would surface here, before any Pod is created, asserts each ExternalSecret reaches `Ready=True` so an ESO sync regression fails here, not deeper in step 3, and asserts the policy ConfigMap exists. ESO is mandatory because `reconcileSecrets` gates SecretsReady on `WaitForExternalSecret` |
| 2 | Pre-create brownfield MariaDB Database/User/Grant | `apply` + 3× `assert` (5m) | Applies `00-brownfield-db-setup.yaml` — Database `keystone-pss-restricted-db` (database `keystone_pss_restricted`), User `keystone-pss-restricted-user` (manages canonical MariaDB user `keystone` against the existing `openstack/keystone-db` Secret), Grant `keystone-pss-restricted-grant` (ALL PRIVILEGES on `keystone_pss_restricted` to user `keystone`), all in the `openstack` namespace and all marked `cleanupPolicy: Skip` so deletion of the test's CRs does not strand a sibling brownfield test still managing the same MariaDB user. Asserts each MariaDB CR reaches `Ready=True`. The CRs live in `openstack` because `mariaDbRef` is a `LocalObjectReference` (same-namespace only) |
| 3 | Apply Keystone CR; assert Ready=True/AllReady; no in-namespace MariaDB CRs | `apply` + `assert` (5m) + 3× `error` | Applies `01-keystone-cr.yaml` — Keystone CR `keystone-pss-restricted` in `keystone-pss-restricted-test` with brownfield database (`host=openstack-db.openstack.svc.cluster.local`, `port=3306`, `database=keystone_pss_restricted`, `secretRef=keystone-db`), brownfield cache (`servers=[openstack-memcached.openstack.svc:11211]`, `backend=dogpile.cache.pymemcache`), and `policyOverrides.configMapRef=keystone-policy-source` so the policy-validation Job is exercised under PSS=restricted. Asserts `Ready=True` with `reason=AllReady` within 5m; error-asserts no `Database`, `User`, or `Grant` CR named `keystone-pss-restricted` exists in the test namespace (brownfield-mode invariant — same as `brownfield-database`) |
| 4 | Trigger manual fernet rotation Job | `script` (180s) | `kubectl create job keystone-pss-restricted-manual-rotate --from=cronjob/keystone-pss-restricted-fernet-rotate -n keystone-pss-restricted-test`, then `kubectl wait --for=condition=complete job/... --timeout=2m`, then asserts `status.succeeded > 0`. Reaching Ready=True alone does not exercise the rotation Pod (the CronJob's first scheduled run is at midnight Sunday); this step proves the rotation Pod also admits under restricted PSS |
| 5 | Assert zero PSS-violation `FailedCreate` events | `script` | Captures `kubectl get events -n keystone-pss-restricted-test --field-selector reason=FailedCreate -o jsonpath='{range .items[*]}{.message}{"\n"}{end}'` and counts lines matching the literal `violates PodSecurity "restricted:latest"` via `grep -cF`. Match count must equal 0; on non-zero, the script prints the offending events and exits non-zero |

**Fixtures:** `00-namespace.yaml`, `00-policy-cm.yaml`, `00-brownfield-db-setup.yaml`, `01-keystone-cr.yaml`

**Catch blocks:** Steps 3–5 each carry a `catch` block that dumps (every command
guarded with `|| true` so a missing resource never short-circuits the dump): Pod
securityContext extracts (`jsonpath`), the last 30 sorted events in the test
namespace, bootstrap / db-sync / manual-rotate Job pod logs (current AND
`--previous`), Job describe output, Job status, and — in Step 5 — the full
`FailedCreate` event list with `involvedObject.kind/.name` so the offending parent
Deployment/Job/CronJob is identifiable. Mirrors the catch-block shape from
[`fernet-rotation`](#fernet-rotation).

**Design notes:**

- **Opt-out from per-test namespace (`spec.namespace: ""`).** Chainsaw's
  per-test namespace plumbing creates a `chainsaw-*` namespace **without** PSS
  labels. To exercise restricted-profile admission this test must own the
  namespace, so `spec.namespace: ""` **opts out** of Chainsaw's plumbing
  entirely — Chainsaw creates no namespace, and the test applies the labelled
  namespace from `00-namespace.yaml`. This is a **different mechanism** from
  [`namespace-scoped-rbac`](#test-suite-inventory), which sets
  `spec.namespace: openstack` to **pin** to a pre-existing namespace (Chainsaw
  uses, but does not create, that namespace). Both tests avoid a `chainsaw-*`
  per-test namespace, but the underlying mechanism differs (opt-out vs.
  pin-to-existing).
- **Brownfield mode (no `clusterRef`).** `Database.ClusterRef` and
  `Cache.ClusterRef` are `LocalObjectReference`s; the reconciler resolves them in
  the Keystone CR's own namespace and creates Database/User/Grant CRs there too,
  so a CR in `keystone-pss-restricted-test` cannot reach
  `openstack/openstack-db` through `clusterRef`. Brownfield wiring
  (`database.host` + `cache.servers`) is therefore the **only** path that
  satisfies the parent issue's "no new infra in `deploy/kind/`" constraint.
  Mirrors [`brownfield-database`](#brownfield-database) for the no-MariaDB-CRs
  invariant.
- **PSS label shape: `enforce-version=latest`.** Without the version label the
  apiserver pins to the cluster's current Kubernetes version, which can cause
  spurious failures when the cluster image is upgraded. Pinning to `latest`
  tracks the newest baseline — the regression net we actually want.
- **Manual fernet-rotation Job.** Ready=True alone does not cover the rotation
  Pod (the CronJob has not fired yet). Step 4 triggers a one-shot Job from the
  CronJob template so the rotation Pod's spec is actually admitted under PSS.
  The credential-rotation and trust-flush CronJobs are **not** triggered here:
  their Pod specs use the same `restrictedSecurityContext()` helper, so a single
  rotation trigger is sufficient to prove the helper-vs-PSS contract for all
  CronJob-driven Pods. A future reconciler that introduces a *new* unique Pod
  spec is caught on first reconcile by Step 3's Ready=True assertion.
- **Test secrets are ESO-managed ExternalSecrets, not plain Secrets.**
  `reconcileSecrets` gates SecretsReady on its `WaitForExternalSecret` calls
  in `operators/keystone/internal/controller/reconcile_secrets.go`, so a plain
  Secret with all required keys still leaves the CR stuck at
  SecretsReady=False/WaitingForDBCredentials. The
  ExternalSecrets in `00-namespace.yaml` authenticate through the test
  namespace's OWN per-tenant `openbao-tenant-store` (provisioned by the Step-1
  script) and read namespace-scoped paths that the `eso-tenant` policy templates
  to that namespace. Since #606 the shared cluster store grants no read on the
  `openstack/keystone/*` subtree and is namespace-restricted to `openstack` and
  `shared-services`, so a foreign namespace cannot reach the shared
  standalone paths through it; the
  Step-1 script instead COPIES the shared `bootstrap/openstack/controlplane-keystone/admin`
  and `openstack/keystone/openstack/standalone/db` values into the namespace's own
  segment. Copying the shared DB value (whose `username` is `keystone`) is what
  keeps the `keystone` MariaDB user's password matching what 00-brownfield-db-setup.yaml
  and the `brownfield-database` test reconcile onto the same shared user — no SET
  PASSWORD race. `openstack/keystone/openstack/standalone/db` is the only static
  DB credential `write-bootstrap-secrets.sh` still seeds: the stage-(a)
  per-ControlPlane path `openstack/keystone/{ns}/{name}/db` is retired since #439
  (managed-mode ControlPlanes draw engine-issued credentials from
  `database/mariadb/creds/keystone-{ns}`); the standalone e2e fixtures keep
  their local ExternalSecret/Secret name `keystone-db` and only the `remoteRef.key`
  path moved.
- **Unique resource-name and database-name prefix.** Brownfield Database/User/Grant
  CRs land in the shared `openstack` namespace alongside `brownfield-database`'s
  `keystone-brownfield-*` CRs. The `keystone-pss-restricted-` prefix and
  `keystone_pss_restricted` database name guarantee no collisions under
  `parallel: 4`.
- **PushSecret backup paths.** Per-CR fernet/credential PushSecrets push to
  `kv-v2/openstack/keystone/<CR-name>/{fernet,credential}-keys`; the unique CR
  name `keystone-pss-restricted` keeps these paths distinct from sibling tests.
  The per-tenant `eso-tenant` OpenBao policy covers these fernet/credential
  backup paths, scoped to the caller's own namespace (the earlier shared wildcard
  policy was retired in #606), so no policy change is needed.

**Cross-references:**

- Feature: **Pod Security Standards (restricted) admission gate** (this test).
- Parent issue: **#317** — PSS-restricted admission gate for Keystone.
- Phase 1 baseline: **#277** — security/compliance baseline. The Keystone
  unit-test evidence for `restrictedSecurityContext()` lives in
  `operators/keystone/internal/controller/security_context_test.go`; this E2E
  closes the loop with an executable check against a real apiserver.
- Helper definition: `operators/keystone/internal/controller/security_context.go`.
- Reconciler architecture and helper call sites:
  [Keystone Reconciler Architecture](../keystone/keystone-reconciler.md).
- Sibling E2E patterns reused:
  [`brownfield-database`](#brownfield-database) (brownfield-mode invariant),
  [`fernet-rotation`](#fernet-rotation) (manual rotation + catch shape).
  Related but **not** reused (different mechanism, see design note above):
  [`namespace-scoped-rbac`](#test-suite-inventory) pins to a pre-existing
  namespace via `spec.namespace: openstack`; this test opts out via
  `spec.namespace: ""`.

---

### db-expand-failure

**File:** `tests/e2e/keystone/db-expand-failure/chainsaw-test.yaml`

**Purpose:** Drives a release upgrade into the failure branch of the expand phase. The
patch that moves `spec.image.tag` from 2026.1 to 2026.2 also sets a 16Mi memory limit in
`spec.jobs.resources`. The expand Job runs the real 2026.2 image, the kernel OOM-kills its
container on all five attempts, and the Job ends `Failed/BackoffLimitExceeded`.
`upgrade-flow` covers a successful upgrade only, and `upgrade-abort` wedges its expand Job
in `ImagePullBackOff`, which never exhausts the backoff limit.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply Keystone CR with initial release 2026.1 | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-db-expand-failure` at tag `"2026.1"` |
| 2 | Assert Ready and installedRelease set | `assert` (8m) | `installedRelease: "2026.1"`, `Ready=True/AllReady` |
| 3 | Patch the tag and a 16Mi Job memory limit | `patch` | `01-patch-expand-failure.yaml` sets tag `"2026.2"` and `spec.jobs.resources.limits.memory: 16Mi` |
| 4 | Assert the expand phase failed for good | `assert` (8m) + `script` | `installedRelease: "2026.1"`, `targetRelease: "2026.2"`, `upgradePhase: Expanding`, `DatabaseReady=False/ExpandFailed`; Job `keystone-db-expand-failure-db-expand` has `Failed=True/BackoffLimitExceeded`; the `DatabaseReady` message contains `Expand job keystone-db-expand-failure-db-expand failed: job has permanently failed`. The script also prints how each Job pod terminated |
| 5 | Assert the Warning ExpandFailed event | `script` (90s) | Polls `kubectl get events` on the CR's UID with `reason=ExpandFailed,type=Warning` |
| 6 | Assert the failed phase Job is counted | `script` (120s) | `tests/e2e/lib/assert-operator-metric-families.sh` against the `keystone-operator` release, with the `keystone_operator_db_sync_total{result="failed"}` sample of `keystone-db-expand-failure` |
| 7 | Assert the Deployment keeps the 2026.1 image | `assert` (8m) | Container `keystone` of Deployment `keystone-db-expand-failure` runs `ghcr.io/c5c3/keystone:2026.1`, because the Database step fails before the Deployment step runs |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-expand-failure.yaml`

Chainsaw's cleanup deletes the CR while the upgrade is still in `Expanding`. The
finalizer does not depend on the upgrade state.

---

### leader-election

**File:** `tests/e2e/keystone-operator/leader-election/chainsaw-test.yaml`

**Purpose:** Proves that the keystone-operator elects a leader again after its
Lease is deleted, and that the elected leader reconciles. The chart runs two
replicas with leader election on, and `bootstrap.Run`
(`internal/common/bootstrap/manager.go`) keeps controller-runtime's default
durations, so the Lease `keystone.openstack.c5c3.io` lives in `keystone-system`
and names its holder as `<pod name>_<uuid>`. Every operator starts its manager
through `bootstrap.Run`, so this suite covers the election code they share.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply the Keystone CR | `apply` + `assert` (5m) | `00-keystone-cr.yaml`, Keystone CR `keystone-lease` with one replica, reaches `Ready=True/AllReady` |
| 2 | Delete the Lease and watch the re-election | `script` (6m) | Reads the Lease's uid, holder and `renewTime`, fails unless the holder is a Ready `keystone-operator` pod, and deletes the Lease. Within 90s a Lease with a new uid and a holder must exist (`ERROR: no new leader within 90s` otherwise). Its `acquireTime` must not predate the old `renewTime`, unless the same holder recreated it within its own term. Two reads 6s apart must show the holder renewing. Within 120s the operator Deployment is Ready again; when the standby won, the former leader's `manager` container has restarted and is Ready again. For 30s the Lease keeps its uid and holder. On every poll the CR's `Ready` must be `True`. Prints `OK: <pod> re-acquired the Lease` or `OK: failover to <pod>; <former pod> restarted <n> time(s)` |
| 3 | The re-elected leader reconciles | `patch` + `script` (6m) | `01-patch-replicas.yaml` sets two replicas; within 300s `status.observedGeneration` equals `metadata.generation`, `Ready=True`, and Deployment `keystone-lease` has two ready replicas. Prints `OK: the re-elected operator reconciled generation <n>` |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-replicas.yaml`

**Catch blocks:** Steps 2 and 3 dump the Lease, the `keystone-system` pods, the
last 100 log lines of each operator pod with and without `--previous`, and the
`keystone-system` events.

**Design notes:**

- The suite runs alone (`concurrent: false`). When the standby wins, the former
  leader loses its Lease and its manager exits, so its container restarts. Beside
  another suite that restart would break the metric-family scrape and the
  steady-state event window `basic-deployment` runs against the operator.
- Which replica wins is a race between the leader's next renewal and the
  standby's next poll, and both outcomes pass. When the leader wins, client-go's
  fast path (`tryAcquireOrRenew` in `tools/leaderelection`) updates the Lease
  with the `acquireTime` and `leaseTransitions` it observed before, and the API
  server turns that update of the deleted Lease into a create. The new Lease
  then carries the old `acquireTime`, which step 2 accepts for the same holder only.
- Deleting the `kubectl delete lease` line makes step 2 fail with
  `ERROR: no new leader within 90s`, so the step cannot pass without a new Lease.

---

### multi-namespace

**File:** `tests/e2e/keystone/multi-namespace/chainsaw-test.yaml`

**Purpose:** Runs one Keystone name, `keystone-mns`, in three namespaces at once
and proves that the three instances share nothing: no child object, no key
material and no token. Anything the operator keys by name alone would collide.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Namespaces, tenant stores and ExternalSecrets | `apply` + `script` (5m) + `assert` (10m) | `00-namespaces.yaml` creates the three namespaces with their `keystone-admin` and `keystone-db` ExternalSecrets. The script writes the namespace's MariaDB user name and copies the shared database and admin passwords to `openstack/keystone/<ns>/mns/db` and `bootstrap/<ns>/mns/admin` inside `openbao-0`, and runs `setup-eso-tenant.sh` for each namespace; the six ExternalSecrets reach `Ready=True` |
| 2 | Brownfield schemas, users and grants | `apply` + `assert` (10m) | `00-brownfield-db-setup.yaml` in `openstack`: Databases `keystone_mns_a` to `keystone_mns_c`, each with its own MariaDB user (`keystone-mns-a` to `keystone-mns-c`) and Grant, all `Ready=True` |
| 3 | Three Keystones of one name | `apply` + `assert` (10m) | `01-keystone-crs.yaml` applies all three at once; each reaches `Ready=True/AllReady` |
| 4 | Objects, keys and tokens stay in their namespace | `script` (10m) | Per namespace, Deployment and Service `keystone-mns`, the config ConfigMap, Secrets `keystone-mns-fernet-keys` and `keystone-mns-db-connection`, and PushSecret `keystone-mns-fernet-keys-backup` exist with one ownerReference, to that namespace's Keystone. The connection URL names `keystone_mns_<x>`. The PushSecrets write to three distinct keys `openstack/keystone/<ns>/keystone-mns/fernet-keys` and are `Ready=True`. The three fernet key sets hash differently. A token issued in each namespace answers 200 there and 401 in the other two |
| 5 | Deleting one instance | `script` (6m) | Records the Deployment uid, API pod uids, ConfigMap and fernet hash of `keystone-mns-b` and `keystone-mns-c` and a `keystone-mns-b` token, deletes the Keystone of `keystone-mns-a` and waits up to 180s for it to be gone. Both others stay `Ready=True` with the same values, and the token still answers 200 |

**Fixtures:** `00-namespaces.yaml`, `00-brownfield-db-setup.yaml`, `01-keystone-crs.yaml`

**Catch blocks:** Every step dumps, per namespace, the Keystone status, the pods,
the events, and the ExternalSecret and SecretStore status. Step 1's `finally`
removes the staged OpenBao root token.

**Design notes:**

- The suite sets `spec.namespace: ""` and applies its own namespaces, as
  `pod-security-restricted` does.
- `database.clusterRef` and `cache.clusterRef` are `LocalObjectReference`s, so the
  three Keystones reach the shared `openstack-db` and `openstack-memcached`
  through `database.host` and `cache.servers`, and the schemas are created in
  `openstack`. `cleanupPolicy: Skip` keeps the three users and schemas at
  cleanup, as in `pod-security-restricted`.
- Each namespace connects as its own MariaDB user. A mariadb-operator `User`
  allows 10 connections by default, and the canonical `keystone` user that
  `brownfield-database` and `pod-security-restricted` share cannot also carry
  three more Keystones: once it reaches the cap, a keystone request that needs
  a new connection answers 500. The three users share the password of
  `openstack/keystone-db`.
- The OpenBao root token is staged in `openbao-0` over stdin at
  `/tmp/.bao-token-keystone-multi-namespace`, a name no other suite uses, so a
  parallel suite's cleanup cannot remove it. Each field is copied by a pipe inside
  the pod, so no value leaves `openbao-0` or appears in a command argument, which
  the API server's audit log records. Tokens and passwords reach the API pods over
  stdin.
- The fixtures turn caching off. The three Keystones share `openstack-memcached`,
  and keystone's cache keys carry no per-instance discriminator (#227), so a token
  validation cached by one instance would answer the same token in the other two.
- The connection URL always ends in `?charset=utf8`, so step 4 compares the URL's
  path and never prints the URL, which carries the password.

---

### plugins

**File:** `tests/e2e/keystone/plugins/chainsaw-test.yaml`

**Purpose:** Proves that a driver selected through `spec.plugins` is the one
keystone loads. `plugins.RenderPluginConfig` (`internal/common/plugins/plugins.go`)
turns each entry into an INI section named by its `configSection`; the fixture's
entry sets `[unified_limit] enforcement_model = strict_two_level`. Keystone resolves
that option through stevedore (namespace `keystone.unified_limit.model`), and
`GET /v3/limits/model` returns the name of the model it loaded.

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply the Keystone CR | `apply` + `assert` (5m) | `00-keystone-cr.yaml`, Keystone CR `keystone-plugins` with the plugin, reaches `Ready=True/AllReady` |
| 2 | The API reports the plugin's model | `script` (3m) | The ConfigMap the pod template names sets `enforcement_model = strict_two_level` under `[unified_limit]`, and `GET /v3/limits/model` in a Ready API pod reports `strict_two_level`, retried every 5s for up to 60s |
| 3 | A model keystone cannot load never turns Ready | `script` (6m) | Records the Ready pod P0 and its ConfigMap, then patches `enforcement_model` to `no_such_model`. Within 120s the template names a new ConfigMap with that value and a pod mounts it. For the next 120s no such pod turns Ready, P0 stays Ready, and P0 still reports `strict_two_level`. The last 50 log lines of each new pod are printed either way |
| 4 | Without spec.plugins keystone loads its default | `script` (8m) | Sets `spec.plugins` to `null`; within 300s the CR is `Ready=True` at its generation, the rollout finishes, the ConfigMap has no `[unified_limit]` section, and a pod of the new template reports `flat` |

**Fixtures:** `00-keystone-cr.yaml`

**Catch blocks:** Every step dumps the CR status, the ReplicaSets and pods, and the
last 100 log lines of each API pod with and without `--previous`.

**Design notes:**

- The keystone image installs keystone with the `ldap` extra and no other package,
  so no out-of-tree plugin exists to load. `strict_two_level` is an in-tree entry
  point resolved through the same stevedore loader an out-of-tree driver uses.
- The webhook never checks plugin values, so `no_such_model` is admitted, and
  keystone fails to load its application. Step 3 does not assert the CR's `Ready`:
  `IsDeploymentReady` (`internal/common/deployment/deployment.go`) counts the
  Deployment ready while the old pod still serves, and the rollout runs with
  `maxSurge: 1` and `maxUnavailable: 0`.
- The fixture turns caching off for the token requests, for the shared-memcached
  reason `fernet-rotation` gives (#227).

---

### rapid-spec-changes

**File:** `tests/e2e/keystone/rapid-spec-changes/chainsaw-test.yaml`

**Purpose:** Proves that a burst of spec changes, sent faster than the operator
reconciles, converges on the last one in the running API. Each render writes an
immutable ConfigMap named after the hash of its data, and a render cache keyed on
the CR's uid and generation skips unchanged renders
(`operators/keystone/internal/controller/reconcile_config.go`).

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | Apply the Keystone CR | `apply` + `assert` (5m) | `00-keystone-cr.yaml`, Keystone CR `keystone-rapid` with `[token] expiration = 3600`, reaches `Ready=True/AllReady` |
| 2 | Twelve patches in a burst | `script` (20m) | Twelve merge patches set `spec.extraConfig.token.expiration` from 3660 to 4320 in steps of 60 with no pause. Between patches 6 and 7 a patch of the misspelt `expiraton` is refused with `no such option in the keystone 2026.1 option catalog` and leaves the generation unchanged. The generation then stands at G0 + 12. Within 600s the CR is `Ready=True/AllReady` at that generation and `kubectl rollout status` finishes. The ConfigMap the pod template names sets `expiration = 4320` under `[token]`, and every live API pod mounts it. A project-scoped admin token issued in an API pod lives 4318 to 4320 seconds. Repeating the last patch leaves the generation unchanged and renders no ConfigMap within 30s. Within 120s at most four `keystone-rapid-config-` ConfigMaps remain. For 60s the template, the Deployment's generation and the CR's `observedGeneration` do not move |

**Fixtures:** `00-keystone-cr.yaml`

**Catch blocks:** Both steps dump the CR status, the Deployment and its
ReplicaSets, the `keystone-rapid-config-` ConfigMaps, and the last 200 log lines
of the keystone-operator pods.

**Design notes:**

- `extraConfig` is a map of strings, so every value is sent quoted. The values
  stay 60 seconds apart, so the token lifetime tells the last value from any
  earlier one.
- The cap of four ConfigMaps is the current one plus the three the prune keeps,
  the bound `config-pruning` asserts.
- The fixture turns caching off for the token request, for the shared-memcached
  reason `fernet-rotation` gives (#227).

---

### vertical-autoscaling

**File:** `tests/e2e/keystone/vertical-autoscaling/chainsaw-test.yaml`

**Purpose:** Proves the absent case of `spec.deployment.verticalAutoscaling`:
on a cluster that does not serve `autoscaling.k8s.io/v1`
VerticalPodAutoscaler, the opt-in is reported through
[`VPAReady`](../keystone/keystone-crd.md#vpaready-condition) and holds nothing
else back. The `e2e-operator` keystone leg deploys no VPA, so the suite runs
there; its first step fails rather than skips on a cluster that serves the
kind, because it would then prove nothing. The present case, the operators
creating VPAs, is proven by
[e2e-autoscaling](controlplane-e2e-tests.md#e2e-autoscaling).

**Steps:**

| # | Step Name | Type | Details |
| --- | --- | --- | --- |
| 1 | The cluster serves no VPA | `script` | `kubectl api-resources --api-group=autoscaling.k8s.io` lists no `verticalpodautoscalers`; otherwise the step fails and names `WITH_VPA=true` |
| 2 | Apply the opted-in Keystone CR | `apply` | `00-keystone-cr.yaml` — Keystone CR `keystone-vertical` with `verticalAutoscaling.updateMode: "Off"` |
| 3 | Assert the opt-in is reported | `assert` (5m) + `script` | `DeploymentReady=True`, `KeystoneAPIReady=True`, `VPAReady=False/VPANotInstalled` with a message containing `autoscaling.k8s.io`, `Ready=False`; `kubectl get vpa` fails with `doesn't have a resource type` |
| 4 | Patch: remove the opt-in | `patch` | `01-patch-disable-vertical-autoscaling.yaml` sets `verticalAutoscaling` to `null` |
| 5 | Assert Ready returns | `assert` (5m) | `VPAReady=True/VPANotRequired` and `Ready=True/AllReady` |

**Fixtures:** `00-keystone-cr.yaml`, `01-patch-disable-vertical-autoscaling.yaml`

---

## Assertion Patterns

The test suites use three Chainsaw assertion patterns:

### Resource Assertion (`assert`)

Declarative YAML matching against a Kubernetes resource using JMESPath filter syntax for
condition assertions. Used for condition checks, replica counts, and resource existence.
The assert timeout is set to 5 minutes at the spec level (`timeouts.assert: 5m`).

```yaml
- try:
    - assert:
        resource:
          apiVersion: keystone.openstack.c5c3.io/v1alpha1
          kind: Keystone
          metadata:
            name: keystone-basic
            namespace: openstack
          status:
            (conditions[?type == 'Ready']):
            - status: "True"
              reason: AllReady
```

### Error Assertion (`error`)

Verifies that a resource does **not** exist. Used in `deletion-cleanup` and
`brownfield-database` to assert garbage collection and absence of MariaDB CRs.

```yaml
- name: Assert Deployment not found
  try:
    - error:
        resource:
          apiVersion: apps/v1
          kind: Deployment
          metadata:
            name: keystone-cleanup
            namespace: openstack
```

### Script Assertion (`script`)

Shell commands for assertions that cannot be expressed declaratively — ConfigMap name
patterns (content-hash suffix), API endpoint connectivity, and rotation verification
(Secret data change, pod UID stability, token validation).

```yaml
- name: Assert ConfigMap exists
  try:
    - script:
        content: |
          kubectl get cm -n openstack -o name | grep keystone-basic-config-
```

---

## Sub-Condition Progression

The Keystone reconciler sets conditions in this order during a successful reconciliation.

> **Note:** This diagram shows the _execution order_ within `Reconcile()`, which differs
> from the order of `subConditionTypes`, the list of every sub-condition type that
> decides how conditions appear in status output.

![The order the Keystone reconciler sets its conditions in, as the basic-deployment suite asserts them: SecretsReady=True with reason SecretsAvailable, then reconcileConfig, which sets no condition and returns configMapName, FernetKeysReady=True with reason FernetKeysAvailable, DatabaseReady=True with reason DatabaseSynced, DeploymentReady=True with reason DeploymentReady, BootstrapReady=True with reason BootstrapComplete, and Ready=True with reason AllReady, the aggregate of every sub-condition. The suite asserts the five sub-conditions and Ready in one step.](../../diagrams/test-keystone-condition-progression.svg)

The `basic-deployment` test asserts all 6 conditions (5 sub-conditions + Ready) in a
single assert step, validating the full progression.

---

## File Layout

```text
tests/e2e/keystone/
├── admin-password-rotation/
│   ├── chainsaw-test.yaml              Re-bootstrap on admin-password change
│   └── 00-keystone-cr.yaml             Keystone CR for rotation test
├── admin-password-scheduled-rotation/
│   ├── chainsaw-test.yaml              Model B scheduled rotation chain
│   └── 00-keystone-cr.yaml             Keystone CR with passwordRotation
├── autoscaling/
│   ├── chainsaw-test.yaml              HPA reconciliation
│   ├── 00-keystone-cr.yaml             Keystone CR with CPU autoscaling
│   ├── 01-patch-add-memory-metric.yaml Patch to add memory metric
│   ├── 02-patch-min-replicas-one.yaml  Patch to lower minReplicas to 1
│   ├── 03-patch-behavior-target-above-100.yaml Patch to a 150% CPU target and a scale-down behavior
│   └── 04-patch-disable-autoscaling.yaml Patch to disable autoscaling
├── basic-deployment/
│   ├── chainsaw-test.yaml              Happy-path reconciliation
│   └── 00-keystone-cr.yaml             Keystone CR in managed mode
├── basic-deployment-2026-2/
│   ├── chainsaw-test.yaml              Happy-path reconciliation 2026.2
│   └── 00-keystone-cr.yaml             Keystone CR with 2026.2 image
├── brownfield-database/
│   ├── chainsaw-test.yaml              External database mode
│   ├── 00-brownfield-db-setup.yaml     External database setup
│   └── 00-keystone-cr.yaml             Keystone CR with database.host
├── concurrent-cr-conflicts/
│   ├── chainsaw-test.yaml              Concurrent CR conflict handling
│   ├── 00-keystone-cr-a.yaml           Keystone CR fixture A (keystone-concurrent-a)
│   └── 01-keystone-cr-b.yaml           Keystone CR fixture B (keystone-concurrent-b)
├── config-pruning/
│   ├── chainsaw-test.yaml              Immutable ConfigMap pruning
│   └── 00-keystone-cr.yaml             Keystone CR for pruning test
├── configmap-no-secrets/
│   ├── chainsaw-test.yaml              No secrets leak into the ConfigMap
│   └── 00-keystone-cr.yaml             Keystone CR for env-override split
├── credential-rotation/
│   ├── chainsaw-test.yaml              Credential key rotation
│   ├── 00-keystone-cr.yaml             Keystone CR with rotation schedule
│   ├── 01-patch-suspend-credential.yaml Patch to suspend rotation
│   └── 02-refuse-staging-patch.yaml    Admission policy that refuses the staging PATCH
├── database-tls/
│   ├── chainsaw-test.yaml              Keystone ↔ MariaDB TLS (verify-full)
│   └── 00-keystone-cr.yaml             Keystone CR with database.tls
├── db-expand-failure/
│   ├── chainsaw-test.yaml              Expand phase fails with ExpandFailed
│   ├── 00-keystone-cr.yaml             Keystone CR at release 2026.1
│   └── 01-patch-expand-failure.yaml    Tag 2026.2 with a 16Mi Job memory limit
├── db-sync-partial-schema/
│   ├── chainsaw-test.yaml              db-sync pre-step drops a half-built schema
│   └── 00-keystone-cr.yaml             Keystone CR on the seeded database
├── deletion-cleanup/
│   ├── chainsaw-test.yaml              Garbage collection
│   └── 00-keystone-cr.yaml             Keystone CR for cleanup test
├── events/
│   ├── chainsaw-test.yaml              Kubernetes event emission
│   └── 00-keystone-cr.yaml             Keystone CR for event test
├── fernet-rotation/
│   ├── chainsaw-test.yaml              Fernet key rotation
│   ├── 00-keystone-cr.yaml             Keystone CR with rotation schedule
│   ├── 01-patch-suspend-fernet.yaml    Patch to suspend rotation
│   ├── 02-hold-scheduled-rotation.yaml Admission policy that refuses rotation pods
│   ├── 03-patch-schedule-every-minute.yaml Patch to rotate every minute
│   ├── 04-patch-schedule-weekly.yaml   Patch to restore the weekly schedule
│   ├── 05-refuse-staging-patch.yaml    Admission policy that refuses the staging PATCH
│   └── key-helpers.sh                  Key and token helpers that steps 7 and 8 source
├── gateway-quick-start/
│   ├── chainsaw-test.yaml              Envoy Gateway quick-start wiring
│   ├── 00-namespace.yaml               Test namespace
│   └── 01-keystone-cr.yaml             Keystone CR with spec.gateway
├── gateway-quick-start-smoke/
│   ├── chainsaw-test.yaml              nip.io HTTPS smoke test
│   ├── 00-namespace.yaml               Test namespace
│   └── 01-smoke-keystone-cr.yaml       Keystone CR for smoke test
├── graceful-shutdown/
│   ├── chainsaw-test.yaml              Graceful shutdown
│   └── 00-keystone-cr.yaml             Keystone CR for graceful shutdown
├── healthcheck/
│   ├── chainsaw-test.yaml              Post-Deployment API health check
│   └── 00-keystone-cr.yaml             Keystone CR for healthcheck test
├── httproute/
│   ├── chainsaw-test.yaml              spec.gateway HTTPRoute lifecycle
│   ├── 00-httproute-crd.yaml           HTTPRoute CRD install
│   ├── 01-keystone-cr.yaml             Keystone CR with spec.gateway
│   └── 02-patch-remove-gateway.yaml    Patch removing spec.gateway
├── image-upgrade/
│   ├── chainsaw-test.yaml              Rolling image upgrade
│   ├── 00-keystone-cr.yaml             Keystone CR with initial image tag
│   └── 01-patch-image.yaml             Patch spec.image.tag
├── image-pull-policy/
│   ├── chainsaw-test.yaml              spec.image.pullPolicy and the operator default
│   ├── 00-keystone-cr.yaml             Keystone CR without pullPolicy
│   └── 01-patch-pull-policy-never.yaml Patch to set spec.image.pullPolicy Never
├── invalid-cr/
│   ├── chainsaw-test.yaml                                  CRD webhook + CEL validation
│   ├── _generate.py                                        Canonical scaffold + generator for invalid-cr fixtures
│   ├── test_generate.py                                    Fast unit tests for the generator: drift check, FIXTURES count, chainsaw-test.yaml cross-reference (run by `make verify-invalid-cr-fixtures`)
│   ├── 00-invalid-cron.yaml                                Invalid cron expression CR
│   ├── 01-duplicate-plugins.yaml                           Duplicate plugin configSection CR
│   ├── 02-database-both-modes.yaml                         Database both modes set (generated)
│   ├── 03-cache-both-modes.yaml                            Cache both modes set (generated)
│   ├── 04-autoscaling-no-target.yaml                       Autoscaling without target (generated)
│   ├── 05-policy-overrides-no-source.yaml                  PolicyOverrides without source (generated)
│   ├── 06-policy-overrides-empty-rule-key.yaml             PolicyOverrides empty rule key (generated)
│   ├── 07-networkpolicy-empty-ingress.yaml                 NetworkPolicy empty ingress (generated)
│   ├── 09-replicas-negative.yaml                           replicas: -1 (generated; subsumes dropped 08-replicas-zero case)
│   ├── 10-hpa-min-greater-than-max.yaml                    HPA minReplicas > maxReplicas (generated)
│   ├── 11-fernet-maxactivekeys-below-minimum.yaml          Fernet maxActiveKeys < 3 (generated)
│   └── 12-credentialkeys-maxactivekeys-below-minimum.yaml  CredentialKeys maxActiveKeys < 3 (generated)
├── key-repository-mode/
│   ├── chainsaw-test.yaml              Key volumes not world-readable
│   └── 00-keystone-cr.yaml             Keystone CR for key-mode test
├── logging/
│   ├── chainsaw-test.yaml              spec.logging → oslo.log propagation
│   ├── 00-keystone-cr.yaml             Keystone CR without spec.logging
│   ├── 01-patch-debug-true.yaml        Patch enabling debug
│   ├── 02-patch-format-json.yaml       Patch switching to JSON format
│   └── 03-patch-invalid-perloggerlevel.yaml Invalid per-logger level (rejected)
├── maintenance-endpoint-isolation/
│   ├── chainsaw-test.yaml              API Service endpoints during a trust-flush run
│   └── 00-keystone-cr.yaml             Keystone CR with an every-minute trust flush
├── metrics/
│   └── chainsaw-test.yaml              Operator ServiceMonitor render/remove
├── middleware-config/
│   ├── chainsaw-test.yaml              Middleware pipeline
│   └── 00-keystone-cr.yaml             Keystone CR with custom middleware
├── missing-secret/
│   ├── chainsaw-test.yaml              Secret dependency recovery
│   ├── 00-keystone-cr.yaml             Keystone CR with non-existent secretRefs
│   └── 01-late-secrets.yaml            ExternalSecrets created after CR
├── multi-namespace/
│   ├── chainsaw-test.yaml              One Keystone name in three namespaces
│   ├── 00-namespaces.yaml              Three namespaces + their ExternalSecrets
│   ├── 00-brownfield-db-setup.yaml     Three Databases, Users and Grants
│   └── 01-keystone-crs.yaml            Keystone CR keystone-mns per namespace
├── namespace-scoped-rbac/
│   ├── chainsaw-test.yaml              Namespace-scoped RBAC + standalone webhook
│   ├── 00-keystone-cr.yaml             Keystone CR for RBAC test
│   ├── 01-priority-class.yaml          PriorityClass the CR names
│   └── 02-invalid-cron.yaml            Keystone CR the standalone webhook rejects
├── network-policy/
│   ├── chainsaw-test.yaml              NetworkPolicy reconciliation
│   ├── 00-keystone-cr.yaml             Keystone CR with ingress policy
│   ├── 01-patch-update-ingress.yaml    Patch ingress rule
│   ├── 02-patch-disable-networkpolicy.yaml Patch to disable NetworkPolicy
│   └── 03-probe-pods.yaml              Probe pods: target, sink, inside and outside clients
├── node-placement/
│   ├── chainsaw-test.yaml              spec.deployment node placement, inherited by the Jobs
│   └── 00-keystone-cr.yaml             Keystone CR with nodeSelector, tolerations and affinity
├── plugins/
│   ├── chainsaw-test.yaml              spec.plugins model reaches the API
│   └── 00-keystone-cr.yaml             Keystone CR with the strict_two_level plugin
├── pod-security-restricted/
│   ├── chainsaw-test.yaml              PSS-restricted admission gate
│   ├── 00-namespace.yaml               PSS-labelled test namespace + plain Secrets
│   ├── 00-brownfield-db-setup.yaml     Brownfield Database/User/Grant + db-pw Secret
│   └── 01-keystone-cr.yaml             Keystone CR with brownfield db + cache
├── policy-overrides/
│   ├── chainsaw-test.yaml              oslo.policy integration
│   ├── 00-policy-cm.yaml               Policy source ConfigMap
│   └── 01-keystone-cr.yaml             Keystone CR with policyOverrides
├── policy-validation/
│   ├── chainsaw-test.yaml              Policy validation gating
│   ├── 00-policy-cm.yaml               Policy source ConfigMap
│   ├── 01-keystone-cr.yaml             Keystone CR with policyOverrides
│   └── 02-patch-disable-policy.yaml    Patch to remove policyOverrides
├── priority-class/
│   ├── chainsaw-test.yaml              spec.priorityClassName propagation
│   ├── 00-priority-class.yaml          Cluster-scoped PriorityClass fixture
│   ├── 01-keystone-cr.yaml             Keystone CR without priorityClassName
│   ├── 02-patch-priority-class.yaml    Patch to set priorityClassName
│   ├── 03-patch-empty-priority-class.yaml Patch to clear priorityClassName
│   └── 04-patch-jobs-empty-priority-class.yaml Patch opting the Jobs out
├── prometheus-stack/
│   └── chainsaw-test.yaml              WITH_PROMETHEUS opt-in addon path
├── rapid-spec-changes/
│   ├── chainsaw-test.yaml              Twelve-patch burst converges on the last
│   └── 00-keystone-cr.yaml             Keystone CR with [token] expiration 3600
├── release-upgrade/
│   ├── chainsaw-test.yaml              Cross-release upgrade 2026.1→2026.2
│   ├── 00-keystone-cr.yaml             Keystone CR with initial tag 2026.1
│   └── 01-patch-upgrade.yaml           Patch spec.image.tag to 2026.2
├── resources/
│   ├── chainsaw-test.yaml              Resource defaults and propagation
│   ├── 00-keystone-cr.yaml             Keystone CR without explicit resources
│   ├── 01-patch-custom-resources.yaml  Patch with custom resource limits
│   └── 02-patch-uwsgi-processes.yaml   Patch removing resources, uwsgi.processes 4
├── rolling-update-zero-downtime/
│   ├── chainsaw-test.yaml              Zero-downtime rolling update
│   ├── 00-keystone-cr.yaml             Keystone CR with initial image tag
│   └── 01-patch-image.yaml             Patch spec.image.tag
├── scale/
│   ├── chainsaw-test.yaml              Replica scaling and PDB
│   ├── 00-keystone-cr.yaml             Keystone CR with replicas: 3
│   ├── 01-patch-scale-up.yaml          Patch replicas to 5
│   ├── 02-patch-scale-down.yaml        Patch replicas to 2
│   └── 03-patch-scale-to-one.yaml      Patch replicas to 1
├── schema-drift-detection/
│   ├── chainsaw-test.yaml              Schema drift detection
│   └── 00-keystone-cr.yaml             Keystone CR for schema drift test
├── semantic-invariants/
│   ├── chainsaw-test.yaml              CR status semantic invariants
│   └── 00-keystone-cr.yaml             Keystone CR for invariants test
├── topology-spread/
│   ├── chainsaw-test.yaml              spec.topologySpreadConstraints
│   ├── 00-keystone-cr.yaml             Keystone CR without explicit TSC
│   ├── 01-patch-custom-tsc.yaml        Patch with custom TSC
│   └── 02-patch-empty-tsc.yaml         Patch with empty TSC (disable)
├── trust-flush/
│   ├── chainsaw-test.yaml              Trust flush CronJob
│   ├── 00-keystone-cr.yaml             Keystone CR with explicit trustFlush config
│   └── 01-patch-suspend-trust-flush.yaml Patch suspending trust flush via spec.trustFlush.suspend=true
├── trust-flush-default/
│   ├── chainsaw-test.yaml              Default-on trust flush via webhook materialization
│   └── 00-keystone-cr.yaml             Keystone CR omitting trustFlush — webhook injects hourly schedule
├── upgrade-abort/
│   ├── chainsaw-test.yaml              Abort an in-flight upgrade by reverting the image tag
│   ├── 00-keystone-cr.yaml             Keystone CR at release 2026.1
│   ├── 01-patch-stuck-upgrade.yaml     Patch to 2026.2 on registry.invalid (pull fails — wedges in Expanding)
│   └── 02-patch-abort.yaml             Patch back to ghcr.io/c5c3/keystone:2026.1 to abort
├── upgrade-flow/
│   ├── chainsaw-test.yaml              Expand-migrate-contract upgrade
│   ├── 00-keystone-cr.yaml             Keystone CR with initial release
│   ├── 01-patch-upgrade.yaml           Patch for sequential upgrade
│   ├── 02-patch-skip-level.yaml        Patch for skip-level upgrade
│   └── 03-patch-downgrade.yaml         Patch back to the start release (refused by the controller)
├── uwsgi/
│   ├── chainsaw-test.yaml              uWSGI command propagation
│   ├── 00-keystone-cr.yaml             Keystone CR without explicit uWSGI
│   └── 01-patch-custom-uwsgi.yaml      Patch with custom uWSGI settings
└── vertical-autoscaling/
    ├── chainsaw-test.yaml              VPAReady on a cluster without the VPA
    ├── 00-keystone-cr.yaml             Keystone CR opting into a VPA
    └── 01-patch-disable-vertical-autoscaling.yaml Patch removing the opt-in
```

## Related Resources

- [Keystone CRD API Reference](../keystone/keystone-crd.md) — CRD types, webhooks, and `invalid-cr` E2E tests
- [Keystone Reconciler Architecture](../keystone/keystone-reconciler.md) — Sub-reconciler contracts and unit tests
- [Infrastructure E2E Deployment](../infrastructure/e2e-deployment.md) — Infrastructure stack deployment and `infra-stack-health` test
- `tests/e2e/chainsaw-config.yaml` — Shared Chainsaw configuration
- `.github/workflows/ci.yaml` — CI workflow with E2E job
