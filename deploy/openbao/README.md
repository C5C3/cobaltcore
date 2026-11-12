# OpenBao bootstrap and policies

This directory configures OpenBao after a server exists. The reusable
[shared OpenBao base](../bases/shared-openbao/) installs the shared server;
[`bootstrap/`](bootstrap/) supplies executable scripts and
[`policies/`](policies/) supplies HCL access policies.

## The different OpenBao roles

| Role | Definition and responsibility |
| --- | --- |
| Shared/bootstrap server | A [HelmRelease](../flux-system/releases/openbao/helmrelease.yaml) in `shared-services`; stores bootstrap material and serves auth, policies, tenant secrets and dynamic database credentials |
| OpenBao operator | A separate [operator release](../flux-system/releases/openbao-operator.yaml); reconciles `OpenBaoCluster` and `OpenBaoTenant` resources |
| Development proving instance | [`kind/infrastructure/openbao-instance.yaml`](../kind/infrastructure/openbao-instance.yaml), also inherited by the lab; a separate static-seal server for Barbican integration |
| Dedicated service instances | Created by the [c5c3 operator](../../operators/c5c3/internal/controller/reconcile_barbican_openbao.go) in service namespaces at runtime |

Existing configuration calls the shared server “management OpenBao”. That names
its role, not always its physical cluster: the split devstack places OpenBao and
ESO on the target cluster. The shared Helm installation is not managed by the
OpenBao instance operator.

The proving instance's key is custodied in shared OpenBao, synchronized by ESO,
and adopted before the instance is unpaused. Merging the two server definitions
would change that lifecycle. The shared server's HA and development standalone
configurations also differ deliberately in replica count, Raft joining and
service registration.

## Execution and packaging

`make deploy-infra` orchestrates its initialization/unseal and then runs:

1. `setup-secret-engines.sh`
2. `setup-auth.sh`
3. `setup-policies.sh`
4. `write-bootstrap-secrets.sh`

Database and ESO tenant onboarding have separate scripts and readiness
requirements. Follow the
[bootstrap reference](../../docs/reference/infrastructure/openbao-bootstrap.md)
for direct script execution, environment variables, key/token custody and
recovery. In particular, auth setup precedes policies that substitute its runtime
mount accessor.

The two Kustomize roots package these same inputs for a deployment repository:

```bash
kubectl kustomize deploy/openbao/bootstrap
kubectl kustomize deploy/openbao/policies
```

They generate `cobaltcore-openbao-scripts` and `cobaltcore-openbao-policies`
ConfigMaps with content-derived name suffixes. They do not create or run a Job.
The consuming environment supplies execution resources, RBAC, credentials,
mounts, retry behavior and readiness checks. Include the Jobs and generators in
the same Kustomize composition so references to generated names can be updated.
Scripts load adjacent `common.sh`; `setup-policies.sh` uses adjacent policies or
an explicit `POLICIES_DIR`.

For a cold start, bootstrap must be allowed to run while the server is still
uninitialized or sealed; do not make that work depend on the unsealed readiness
it is meant to establish. Gate secret consumers on the resulting usable API and
configured auth, not just on rendered ConfigMaps or a delivered HelmRelease.

ESO's shared store is described in [`../eso/`](../eso/README.md). Client mTLS is a
transport requirement; Kubernetes/AppRole authentication and policies still
control authorization.
