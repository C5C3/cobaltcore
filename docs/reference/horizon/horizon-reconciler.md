---
title: Horizon Reconciler Architecture
quadrant: operator
---

# Horizon Reconciler Architecture

The Horizon controller runs the shared table-driven pipeline
(`internal/common/reconcile`) with seven sub-reconcilers. Every step is
instrumented under the `horizon_operator` metrics prefix
(`horizon_operator_reconcile_duration_seconds`,
`horizon_operator_reconcile_errors_total`), and the first step to return a
non-zero result or an error short-circuits the chain — conditions and the
requeue are persisted on every exit path.

## Pipeline

```text
Secrets ──► Config ──► Deployment ──► (prune) ──► ┬─ HTTPRoute
                                                  ├─ HealthCheck
                                                  ├─ HPA
                                                  └─ NetworkPolicy  (parallel)
```

| Step | What it does | Condition |
| --- | --- | --- |
| Secrets | Gates on the OpenBao ClusterSecretStore and the ESO-synced `SECRET_KEY` Secret; digests the key material for the rollout annotation | `SecretsReady` |
| Config | Renders `local_settings.py` (signed-cookie sessions, `CACHES`, `OPENSTACK_KEYSTONE_URL`, `OPENSTACK_ENDPOINT_TYPE = "internalURL"`, `LOGGING`, offline-compression settings, merged `extraConfig`) into an immutable content-addressed ConfigMap | `ConfigReady` |
| Deployment | Ensures the uWSGI Deployment (login-page readiness/startup probes, `HORIZON_SECRET_KEY` env var, secret-key-hash pod annotation), the Service (port 8080), and the PDB; sets `status.endpoint` | `DeploymentReady` |
| (prune) | Uninstrumented retention sweep of historical config ConfigMaps (retain 3 + current); failures flip `ConfigReady` |  |
| HTTPRoute | Full `spec.gateway` lifecycle; reflects the Gateway's Accepted condition | `HTTPRouteReady` |
| HealthCheck | HTTP GET of the cluster-local login page through the shared TTL probe cache — rendering it exercises Django routing, templates, and the static-asset manifest without a live Keystone | `HorizonAPIReady` |
| HPA | Creates/deletes the HorizontalPodAutoscaler | `HPAReady` |
| NetworkPolicy | Creates/deletes the NetworkPolicy; refuses an empty ingress list (fail-closed) | `NetworkPolicyReady` |

## Conditions

The aggregate `Ready` condition is `True` (reason `AllReady`) exactly when
all seven sub-conditions are `True`:

| Type | True reasons | False reasons |
| --- | --- | --- |
| `SecretsReady` | `SecretsAvailable` | `SecretStoreNotReady`, `WaitingForSecretKey` |
| `ConfigReady` | `ConfigRendered` | `ConfigError` |
| `DeploymentReady` | `DeploymentReady` | `WaitingForDeployment` |
| `HTTPRouteReady` | `HTTPRouteAccepted`, `HTTPRouteNotRequired` | `HTTPRouteNotAccepted`, `GatewayAPINotInstalled` |
| `HorizonAPIReady` | `APIHealthy` | `APIUnhealthy`, `EndpointNotReady`, `HealthCheckTimeout`, `ConnectionFailed`, `HealthCheckFailed` |
| `HPAReady` | `HPAReady`, `HPANotRequired` | — (errors propagate) |
| `NetworkPolicyReady` | `NetworkPolicyReady`, `NetworkPolicyNotRequired` | — (errors propagate) |

## Requeue semantics

| Interval | Used by |
| --- | --- |
| 10s | Deployment readiness polling, HTTPRoute acceptance, health-check retry |
| 15s | ESO secret gate polling |
| 30s TTL | Health-probe cache (a passing login-page probe is reused within the TTL) |

## Rotation and deletion

- **`SECRET_KEY` rotation** happens at the OpenBao source: when ESO re-syncs
  the Secret, the Secrets step produces a new digest, the pod-template
  annotation changes, and the Deployment rolls. The key is consumed via an
  environment variable, so a restart is required for it to take effect.
- **Deletion needs no finalizer**: every owned resource is namespace-scoped
  and carries a controller owner reference, so Kubernetes garbage collection
  reclaims the whole set when the CR is deleted.

## Watches

Beyond the owned resources, the controller watches Secrets (indexed reverse
lookup on `spec.secretKeyRef.name`, plus group-scoped owner references) and
the OpenBao `ClusterSecretStore` (fan-out to every Horizon CR), so upstream
credential and backend changes retrigger reconciliation without waiting for
a periodic requeue. The HTTPRoute watch is registered only when the Gateway
API CRD is installed; without it, `spec.gateway` surfaces
`HTTPRouteReady=False` with reason `GatewayAPINotInstalled` instead of
crashing the controller.
