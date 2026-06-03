---
title: OpenBao Bootstrap Procedure
quadrant: infrastructure
---

# OpenBao Bootstrap Procedure

Reference documentation for the OpenBao deployment and bootstrap procedure.
OpenBao is deployed as a 3-replica HA Raft cluster via FluxCD HelmRelease, then
initialized and configured through a sequence of idempotent bootstrap scripts. The
scripts provision secret engines, authentication backends, least-privilege policies, and
initial credentials required by downstream services.

## Architecture Overview

```text
┌─────────────────────────────────────────────────────────────────────┐
│                        Management Cluster                           │
│                                                                     │
│  ┌──────────────┐   ┌──────────────┐   ┌──────────────┐             │
│  │  openbao-0   │   │  openbao-1   │   │  openbao-2   │             │
│  │  (leader)    │◄──►  (follower)  │◄──►  (follower)  │             │
│  │  Raft peer   │   │  Raft peer   │   │  Raft peer   │             │
│  └──────┬───────┘   └──────────────┘   └──────────────┘             │
│         │                                                           │
│         │  TLS (openbao-tls Secret from cert-manager)               │
│         │                                                           │
│  ┌──────▼────────────────────────────────────────────────────────┐  │
│  │              ClusterSecretStore: openbao-cluster-store        │  │
│  │              (kubernetes/management auth, role eso-management)│  │
│  └──────┬────────────────────────────────────────────────────────┘  │
│         │                                                           │
│  ┌──────▼──────┐  ┌──────────────┐  ┌──────────────────────────┐    │
│  │ ExternalSec │  │ ExternalSec  │  │ ExternalSecret           │    │
│  │ keystone-   │  │ keystone-db  │  │ mariadb-root-password    │    │
│  │ admin       │  │              │  │                          │    │
│  └─────────────┘  └──────────────┘  └──────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────┘
```

## Prerequisites

The following must be in place before running the bootstrap scripts:

| Prerequisite | Details |
| --- | --- |
| Kubernetes cluster | Management cluster with `kubectl` access configured |
| FluxCD | source-controller and helm-controller installed |
| cert-manager | Deployed and healthy (CRDs installed, webhook ready) |
| Base manifests | `kubectl apply -k deploy/flux-system/` completed successfully |
| Infrastructure manifests | `kubectl apply -k deploy/flux-system/infrastructure/` completed (includes openbao-tls Certificate) |
| OpenBao pods | All 3 replicas (`openbao-0`, `openbao-1`, `openbao-2`) running in `openbao-system` namespace |
| CLI tools | `kubectl`, `jq` available on the operator workstation; `openssl` available inside the OpenBao pod (used for in-pod password generation) |

**Verification commands:**

```bash
# Confirm all 3 OpenBao pods are Running
kubectl get pods -n openbao-system -l app.kubernetes.io/name=openbao

# Confirm TLS certificate is ready
kubectl get certificate openbao-tls -n openbao-system

# Confirm cert-manager ClusterIssuer exists
kubectl get clusterissuer selfsigned-cluster-issuer
```

## Directory Layout

```text
deploy/
├── openbao/
│   ├── bootstrap/
│   │   ├── common.sh                   Shared functions (log, bao_exec) sourced by all scripts
│   │   ├── init-unseal.sh              Initialize and unseal the cluster
│   │   ├── setup-secret-engines.sh     Enable KV v2 and PKI secret engines
│   │   ├── setup-auth.sh              Configure Kubernetes and AppRole auth
│   │   ├── setup-policies.sh           Apply all HCL access control policies
│   │   └── write-bootstrap-secrets.sh  Generate and seed initial credentials
│   └── policies/
│       ├── eso-management.hcl          ESO policy for management cluster
│       ├── eso-control-plane.hcl       ESO policy for control-plane cluster
│       ├── eso-hypervisor.hcl          ESO policy for hypervisor cluster
│       ├── eso-storage.hcl             ESO policy for storage cluster
│       ├── push-app-credentials.hcl    PushSecret policy for app credentials
│       ├── push-ceph-keys.hcl          PushSecret policy for Ceph keys
│       ├── ci-cd-provisioner.hcl       CI/CD pipeline provisioning policy
│       └── pki-issuer.hcl             cert-manager PKI issuing policy
├── eso/
│   ├── kustomization.yaml              Kustomize entrypoint for ESO resources
│   ├── clustersecretstore.yaml         ClusterSecretStore for OpenBao
│   └── externalsecrets/
│       ├── keystone-admin.yaml         Keystone admin credentials
│       ├── keystone-db.yaml            Keystone database credentials
│       └── mariadb-root-password.yaml  MariaDB root password
└── flux-system/
    ├── releases/
    │   └── openbao.yaml                HelmRelease for OpenBao HA cluster
    └── infrastructure/
        └── openbao-tls-cert.yaml       cert-manager Certificate for TLS
```

## Script Execution Order

The bootstrap scripts **must** be executed in the following order. Each script depends
on the successful completion of the previous step:

```text
1. init-unseal.sh           Initialize Shamir keys, unseal all replicas
       │
       ▼
2. setup-secret-engines.sh  Enable KV v2 and PKI engines
       │
       ▼
3. setup-auth.sh            Configure Kubernetes auth + AppRole
       │
       ▼
4. setup-policies.sh        Apply 8 HCL least-privilege policies
       │
       ▼
5. write-bootstrap-secrets.sh  Generate and seed initial passwords
```

**Dependency rationale:**

- `init-unseal.sh` must run first because OpenBao is sealed after initial deployment
  and all subsequent operations require an unsealed vault with a root token.
- `setup-secret-engines.sh` enables the KV v2 engine that `write-bootstrap-secrets.sh`
  writes to, so engines must exist before secrets can be written.
- `setup-auth.sh` creates the auth mounts and roles that `setup-policies.sh` links
  policies to, though technically policies can be written before auth configuration.
- `setup-policies.sh` must run before `write-bootstrap-secrets.sh` to ensure access
  control is in place before credentials are seeded.

## Environment Setup

All bootstrap scripts execute `bao` CLI commands inside the `openbao-0` pod via
`kubectl exec`. No direct network connection to OpenBao is required from the operator
workstation.

### Required Environment Variables

| Variable | Required By | Description |
| --- | --- | --- |
| `BAO_TOKEN` | All scripts except `init-unseal.sh` | Root token obtained from `init-unseal.sh` output |

The `init-unseal.sh` script does not require `BAO_TOKEN` — it produces the root token
as output. All subsequent scripts require the root token to be set as `BAO_TOKEN` in the
shell environment.

### Internal Script Variables

Each script sets the following variables internally via `kubectl exec` environment
injection:

| Variable | Value | Purpose |
| --- | --- | --- |
| `BAO_ADDR` | `https://127.0.0.1:8200` | OpenBao API address (pod-local loopback) |
| `VAULT_CACERT` | `/openbao/tls/ca.crt` | CA certificate path for TLS verification |
| `VAULT_CLIENT_CERT` | `/openbao/client-tls/tls.crt` | Client certificate the in-pod `bao` CLI presents on every API call. Required because `tls_require_and_verify_client_cert = true` is enabled on the listener; without it the TLS handshake fails before any application-layer auth runs. |
| `VAULT_CLIENT_KEY` | `/openbao/client-tls/tls.key` | Matching private key for `VAULT_CLIENT_CERT`. Both files are mounted from the `openbao-client-tls` Secret at `/openbao/client-tls`. |

Defaults are set in `deploy/openbao/bootstrap/common.sh` and forwarded by every
`bao`-invoking wrapper (`bao_exec`, `bao_exec_stdin` in `common.sh`, the private
`kube_exec` in `init-unseal.sh`, and `openbao_kube_exec` in
`hack/deploy-infra.sh`). Operators wishing to run `bao` from outside the pod
must export both vars to a copy of the client keypair extracted from the
`openbao-client-tls` Secret.

### Running the Full Bootstrap

```bash
# Step 1: Initialize and unseal (produces root token)
cd deploy/openbao/bootstrap
./init-unseal.sh

# Retrieve the root token from the Kubernetes Secret
export BAO_TOKEN=$(kubectl get secret openbao-init-keys -n openbao-system \
  -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')

# Step 2-5: Run remaining scripts in order
./setup-secret-engines.sh
./setup-auth.sh
./setup-policies.sh
./write-bootstrap-secrets.sh
```

> **Note:** For a kind-only walkthrough that uses the same `BAO_TOKEN` extraction to open
> the OpenBao web UI, see
> [Quick Start (Extended) — Step 4b: Open the OpenBao UI](../../quick-start-extended.md#step-4b-openbao-ui)
>. The UI is disabled in the production flux-system overlay.

## Script Reference

### init-unseal.sh

**Purpose:** Initialize OpenBao with Shamir secret sharing and unseal all 3 replicas.

**File:** `deploy/openbao/bootstrap/init-unseal.sh`

| Parameter | Value |
| --- | --- |
| Key shares | 5 |
| Key threshold | 3 |
| Output format | JSON |
| Target pods | `openbao-0`, `openbao-1`, `openbao-2` |
| Namespace | `openbao-system` |

**Behavior:**

1. Checks if OpenBao is already initialized by running `bao status -format=json` on `openbao-0`
   and parsing the `initialized` field from the JSON output.
2. If not initialized: runs `bao operator init -key-shares=5 -key-threshold=3
   -format=json` and stores the full JSON output (containing unseal keys and root
   token) as a Kubernetes Secret `openbao-init-keys` in the `openbao-system` namespace.
3. If already initialized: retrieves existing unseal keys from the `openbao-init-keys`
   Secret.
4. Iterates over all 3 pods (`openbao-0`, `openbao-1`, `openbao-2`) and unseals each
   by providing 3 unseal keys (meeting the threshold).
5. Skips unsealing for pods that are already unsealed.

**Idempotency:** Runs `bao status -format=json` on `openbao-0` (ignoring the exit
code via `|| true`) and uses `jq -e '.initialized == true'` to reliably distinguish
an uninitialized cluster from an initialized-but-sealed one (both return exit code
`2`). If already initialized and unsealed, the script logs a message and exits cleanly.

**Output:** The `openbao-init-keys` Kubernetes Secret contains:

| Key | Description |
| --- | --- |
| `init-output` | Full JSON output from `bao operator init` including `unseal_keys_b64` array and `root_token` |

**Production security:** After bootstrap is complete and the cluster is verified
operational, the `openbao-init-keys` Secret should be exported to secure offline
storage (e.g., hardware security module, air-gapped backup) and deleted from the
cluster. The unseal keys and root token stored in this Secret grant full control
over the vault — leaving them in-cluster increases the blast radius of a
Kubernetes namespace compromise. Re-sealing and unsealing after pod restarts
requires the exported keys, so ensure they are recoverable before deletion.

### setup-secret-engines.sh

**Purpose:** Enable KV version 2 and PKI secret engines.

**File:** `deploy/openbao/bootstrap/setup-secret-engines.sh`

**Requires:** `BAO_TOKEN` environment variable.

| Engine | Mount Path | Configuration |
| --- | --- | --- |
| KV v2 | `kv-v2/` | `version=2` |
| PKI | `pki/` | `max-lease-ttl=87600h` (10 years) |

**Idempotency:** Before enabling each engine, the script checks `bao secrets list
-format=json` for the mount path. If the path already exists, the engine enable is
skipped with a log message.

### setup-auth.sh

**Purpose:** Configure Kubernetes authentication for 4 cluster contexts and AppRole
authentication for CI/CD pipelines.

**File:** `deploy/openbao/bootstrap/setup-auth.sh`

**Requires:** `BAO_TOKEN` environment variable.

#### Kubernetes Auth Mounts

| Mount Path | Cluster | ESO Role | Bound SA | Bound NS | Policy | TTL | Max TTL |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `kubernetes/management` | Management | `eso-management` | `external-secrets` | `external-secrets` | `eso-management` | 1h | 4h |
| `kubernetes/control-plane` | Control Plane | `eso-control-plane` | `external-secrets` | `external-secrets` | `eso-control-plane` | 1h | 4h |
| `kubernetes/hypervisor` | Hypervisor | `eso-hypervisor` | `external-secrets` | `external-secrets` | `eso-hypervisor` | 1h | 4h |
| `kubernetes/storage` | Storage | `eso-storage` | `external-secrets` | `external-secrets` | `eso-storage` | 1h | 4h |

Each Kubernetes auth mount creates a role named `eso-<cluster>` that binds to the
`external-secrets` service account in the `external-secrets` namespace. The role is
linked to the corresponding `eso-<cluster>` policy.

**Note:** The management cluster mount is fully configured — the script explicitly writes
`auth/kubernetes/management/config` with the in-cluster Kubernetes API endpoint and CA
certificate. This requires the OpenBao service account to have the `system:auth-delegator`
ClusterRole (created by the Helm chart when `server.authDelegator.enabled=true`, the
default). The control-plane, hypervisor, and storage cluster mounts have roles created but
their Kubernetes host and CA configuration is deferred until those clusters are provisioned.

#### AppRole Auth

| Mount Path | Role | Policy | Token TTL | Max TTL | Secret ID TTL |
| --- | --- | --- | --- | --- | --- |
| `approle/` | `provisioner` | `ci-cd-provisioner` | 1h | 4h | `8760h` (1 year) |

**Idempotency:** Before enabling each auth method, the script checks `bao auth list
-format=json` for the mount path. If the path already exists, the auth enable is
skipped. Role creation uses `bao write` which is an upsert operation (creates or
updates).

### setup-policies.sh

**Purpose:** Apply all HCL access control policies from the `policies/` directory.

**File:** `deploy/openbao/bootstrap/setup-policies.sh`

**Requires:** `BAO_TOKEN` environment variable.

The script iterates over all `.hcl` files in `deploy/openbao/policies/` and applies
each one via `bao policy write <name> -` (reading from stdin). The policy name is
derived from the filename without the `.hcl` extension.

**Idempotency:** `bao policy write` is an upsert operation — it creates a new policy
or overwrites an existing one with the same name. Re-running with the same policy
content is inherently idempotent.

### write-bootstrap-secrets.sh

**Purpose:** Generate cryptographically secure passwords and seed initial credentials
into the KV v2 secret engine.

**File:** `deploy/openbao/bootstrap/write-bootstrap-secrets.sh`

**Requires:** `BAO_TOKEN` environment variable.

| KV v2 Path | Secret Keys | Description |
| --- | --- | --- |
| `kv-v2/bootstrap/keystone-admin` | `password` | Keystone admin user password |
| `kv-v2/infrastructure/mariadb` | `root-password` | MariaDB root password |
| `kv-v2/openstack/keystone/db` | `username`, `password` | Keystone database credentials (username is `keystone`) |

**Password generation:** Each password is generated **inside the OpenBao pod** using
`openssl rand -base64 32` via `sh -c` within `kubectl exec`, producing a 32-byte
(256-bit) cryptographically secure random value encoded as base64 (44 characters).
Generating passwords inside the pod prevents cleartext passwords from appearing in
host `/proc/<pid>/cmdline` process argument lists.

**Security:** Generated passwords are never echoed to stdout or stderr and never
appear as command-line arguments visible to host process listings. The script
outputs only status messages (e.g., `Writing kv-v2/bootstrap/keystone-admin...` or
`Skipping kv-v2/bootstrap/keystone-admin (already exists)`).

**Idempotency:** Before writing each secret, the script checks `bao kv get` for the
path. If the secret already exists, the write is skipped to prevent overwriting
existing credentials. This is critical — overwriting would create a mismatch between
the credentials stored in OpenBao and those already provisioned to consuming services.

## HCL Access Control Policies

Eight HCL policies enforce least-privilege access for each consumer type. All policy
paths under the KV v2 engine include the `data/` prefix, which is required by the
OpenBao/Vault KV v2 API for read and write operations.

### ESO Policies (Read-Only)

These policies grant the External Secrets Operator read-only access to pull secrets
from OpenBao into Kubernetes Secrets.

| Policy | Paths | Capabilities |
| --- | --- | --- |
| `eso-management` | `kv-v2/data/bootstrap/*`, `kv-v2/data/infrastructure/*`, `kv-v2/data/openstack/keystone/*` | `read` |
| `eso-control-plane` | `kv-v2/data/bootstrap/*`, `kv-v2/data/openstack/*`, `kv-v2/data/infrastructure/*`, `kv-v2/data/ceph/*` | `read` |
| `eso-hypervisor` | `kv-v2/data/ceph/client-nova`, `kv-v2/data/openstack/nova/compute-*` | `read` |
| `eso-storage` | `kv-v2/data/ceph/*` | `read`, `create`, `update` |

**Note:** `eso-storage` is the only ESO policy with write capabilities. This allows
the Ceph cluster to write its own keys back to OpenBao via PushSecret.

**Note:** `eso-hypervisor` has the narrowest scope — it can only access the specific
Ceph client key for Nova and Nova compute configuration, not broader secret paths.

### Operational Policies

| Policy | Paths | Capabilities | Purpose |
| --- | --- | --- | --- |
| `push-app-credentials` | `kv-v2/data/openstack/*/app-credential` | `create`, `update`, `read` | PushSecret for OpenStack application credentials |
| `push-ceph-keys` | `kv-v2/data/ceph/*` | `create`, `update`, `read` | PushSecret for Ceph client keys |
| `ci-cd-provisioner` | `kv-v2/data/*` (create/update/read), `kv-v2/metadata/*` (read/list) | `create`, `update`, `read`, `list` | CI/CD pipeline secret provisioning |
| `pki-issuer` | `pki/issue/*`, `pki/sign/*` | `create`, `update` | cert-manager PKI certificate issuing |

**Note:** `ci-cd-provisioner` intentionally lacks `delete` capability. The CI/CD
pipeline can create, update, and read secrets but cannot delete them, preventing
accidental secret removal during automated deployments.

**Note:** `push-app-credentials` and `push-ceph-keys` include `read` capability
so that ESO's PushSecret controller can check the current remote value during
reconciliation and only write when the secret has actually changed.

## Secret Paths

All secrets are stored under the `kv-v2/` mount point (KV version 2 engine).

### Bootstrap Secrets

| Path | Keys | Provisioned By | Consumed By |
| --- | --- | --- | --- |
| `kv-v2/bootstrap/keystone-admin` | `password` | `write-bootstrap-secrets.sh` | ExternalSecret `keystone-admin` |
| `kv-v2/infrastructure/mariadb` | `root-password` | `write-bootstrap-secrets.sh` | ExternalSecret `mariadb-root-password` |
| `kv-v2/openstack/keystone/db` | `username`, `password` | `write-bootstrap-secrets.sh` | ExternalSecret `keystone-db` |

### ESO Integration

The ClusterSecretStore `openbao-cluster-store` connects the External Secrets Operator
to OpenBao. ExternalSecret resources reference this store to pull secrets into
Kubernetes.

| ExternalSecret | Namespace | Remote Path | Remote Property | K8s Secret Name | K8s Secret Key |
| --- | --- | --- | --- | --- | --- |
| `keystone-admin` | `openstack` | `bootstrap/keystone-admin` | `password` | `keystone-admin-credentials` | `password` |
| `keystone-db` | `openstack` | `openstack/keystone/db` | `username`, `password` | `keystone-db-credentials` | `username`, `password` |
| `mariadb-root-password` | `openstack` | `infrastructure/mariadb` | `root-password` | `mariadb-root-password` | `password` |

**Note:** The ExternalSecret `remoteRef.key` is the path **under** the store's mount
path. The ClusterSecretStore already sets `path: kv-v2`, so ExternalSecrets use
`infrastructure/mariadb` (not `kv-v2/infrastructure/mariadb`).

**Note:** The `mariadb-root-password` ExternalSecret maps the OpenBao key
`root-password` to the Kubernetes Secret key `password`. The MariaDB CR
references this Secret with `rootPasswordSecretKeyRef.key: password`, which
reads the exact key specified — the MariaDB CRD uses a standard
`SecretKeySelector` with no key remapping.

## OpenBao HelmRelease

**File:** `deploy/flux-system/releases/openbao.yaml`

| Property | Value |
| --- | --- |
| Target namespace | `openbao-system` |
| Chart | `openbao` |
| Version constraint | `>=0.5.0 <1.0.0` |
| Source | `openbao` HelmRepository |
| Dependencies | `cert-manager` in `cert-manager` namespace |

### HA Raft Configuration

| Setting | Value |
| --- | --- |
| Replicas | 3 |
| Storage backend | Raft |
| Raft data path | `/openbao/data` |
| PVC size | `10Gi` |
| PVC storage class | `local-path` |
| Leader election | Automatic via Raft consensus |

All 3 replicas are configured with `retry_join` stanzas pointing to each other's
headless service DNS names (`openbao-0.openbao-internal`, `openbao-1.openbao-internal`,
`openbao-2.openbao-internal`), enabling automatic cluster formation at startup.

### TLS Configuration

| Setting | Value |
| --- | --- |
| TLS certificate | `/openbao/tls/tls.crt` |
| TLS key | `/openbao/tls/tls.key` |
| Listener address | `[::]:8200` (dual-stack) |
| Cluster address | `[::]:8201` |
| TLS disabled | `false` |
| Certificate source | `openbao-tls` Secret (cert-manager) |
| Certificate duration | `8760h` (1 year) |
| Renewal window | `720h` (30 days before expiry) |
| `tls_client_ca_file` | `/openbao/tls/ca.crt` — CA the listener uses to verify presented client certs. The file resolves to the `openbao-ca` CA bundle because the server cert (`openbao-tls`) and every client cert are signed by the same `openbao-ca-issuer` (see below) |
| `tls_require_and_verify_client_cert` | `true` — every TLS handshake on `:8200` must present a valid client cert; the listener rejects any connection that does not, before any application-layer auth (Kubernetes JWT, AppRole, root token) runs |

The TLS certificate is issued by the `openbao-ca-issuer` (a CA-type ClusterIssuer)
via a cert-manager Certificate resource at
`deploy/flux-system/infrastructure/openbao-tls-cert.yaml`. The CA keypair itself
is bootstrapped by `selfsigned-cluster-issuer` in
`deploy/flux-system/infrastructure/openbao-ca-issuer.yaml` — a SelfSigned issuer
cannot sign leaves for a separate trust chain, so the openbao trust domain owns
its own CA (mirrors the `openstack-db-ca` precedent).

**Client certificates.** Two additional `cert-manager.io/v1` Certificates
issue *client*-auth keypairs from the same `openbao-ca-issuer`, both
declared in `deploy/flux-system/infrastructure/openbao-client-tls-cert.yaml`:

| Certificate | Secret | Consumer | Mount / Reference |
| --- | --- | --- | --- |
| `openbao-client-tls` | `openbao-client-tls` (namespace `openbao-system`) | OpenBao pods themselves — Raft `retry_join` peer auth + in-pod `bao` exec via `bootstrap/*.sh` | StatefulSet volume `client-tls` mounted read-only at `/openbao/client-tls`, distinct from the server-cert mount at `/openbao/tls` |
| `eso-openbao-client-tls` | `eso-openbao-client-tls` (namespace `openbao-system`) | External Secrets Operator `ClusterSecretStore/openbao-cluster-store` | `spec.provider.vault.tls.certSecretRef` / `keySecretRef` (`deploy/eso/clustersecretstore.yaml`); Kubernetes-token `auth.kubernetes` block is unchanged — mTLS is purely a transport-layer admission gate |

Both client Certificates carry `usages: ["client auth"]` and share the
`openbao-tls` duration / `renewBefore` so server and client rotation cadences
align. The `commonName` / `dnsNames` on the client certs are identifiers only —
the OpenBao listener does not verify SANs on client auth, only the issuing CA.

**Certificate SANs:**

| SAN | Type | Cert | Usages | Purpose |
| --- | --- | --- | --- | --- |
| `openbao-0.openbao-internal` | DNS | `openbao-tls` (server) | `server auth` | StatefulSet pod 0 |
| `openbao-1.openbao-internal` | DNS | `openbao-tls` (server) | `server auth` | StatefulSet pod 1 |
| `openbao-2.openbao-internal` | DNS | `openbao-tls` (server) | `server auth` | StatefulSet pod 2 |
| `openbao.openbao-system.svc` | DNS | `openbao-tls` (server) | `server auth` | Kubernetes Service endpoint |
| `127.0.0.1` | IP | `openbao-tls` (server) | `server auth` | Pod-local loopback (bootstrap scripts, `bao_exec`) |
| `::1` | IP | `openbao-tls` (server) | `server auth` | IPv6 loopback |
| `openbao-client.openbao-system.svc` | DNS | `openbao-client-tls` | `client auth` | Identifier only; presented by OpenBao pods on Raft `retry_join` and in-pod `bao` exec. SANs are not verified by the listener for client auth — chain-to-CA is. |
| `eso-openbao-client.openbao-system.svc` | DNS | `eso-openbao-client-tls` | `client auth` | Identifier only; presented by ESO `ClusterSecretStore/openbao-cluster-store` on every Vault call. SANs are not verified. |

### Resource Limits

| Resource | Request | Limit |
| --- | --- | --- |
| Memory | 256Mi | 512Mi |
| CPU | 250m | — |

### Disabled Features

| Feature | Value | Reason |
| --- | --- | --- |
| `injector.enabled` | `false` | Secrets are managed via ESO, not sidecar injection |
| `ui` | `false` | No web UI required for headless secret management |

## Idempotency Guarantees

All bootstrap scripts are designed to be safely re-run without side effects. This
table summarizes the idempotency mechanism for each script:

| Script | Guard Mechanism | Behavior on Re-run |
| --- | --- | --- |
| `init-unseal.sh` | `bao status -format=json` JSON parsing | Skips initialization if already initialized; skips unseal for already-unsealed pods |
| `setup-secret-engines.sh` | `bao secrets list` path check | Skips engine enable if mount path already exists |
| `setup-auth.sh` | `bao auth list` path check | Skips auth enable if mount path already exists; role write is upsert |
| `setup-policies.sh` | `bao policy write` upsert semantics | Overwrites existing policy with same content (no-op if unchanged) |
| `write-bootstrap-secrets.sh` | `bao kv get` existence check | Skips secret write if path already contains data |

**Critical invariant:** `write-bootstrap-secrets.sh` never overwrites existing secrets.
This prevents credential rotation from being accidentally triggered by a re-run. To
rotate credentials, existing secrets must be explicitly deleted first.

## Error Handling

All scripts use `set -euo pipefail` for strict error handling:

| Flag | Behavior |
| --- | --- |
| `-e` | Exit immediately on any command failure |
| `-u` | Treat unset variables as errors |
| `-o pipefail` | Propagate failures through pipes (not just the last command) |

Scripts log timestamped status messages to stdout using ISO 8601 format. Error messages
from `bao` CLI commands are propagated to stderr by `kubectl exec`.

## Troubleshooting

### OpenBao pods not starting

Verify the TLS certificate Secret exists and is populated:

```bash
kubectl get secret openbao-tls -n openbao-system -o jsonpath='{.data.tls\.crt}' | wc -c
```

If the Secret is empty or missing, check cert-manager logs:

```bash
kubectl logs -n cert-manager -l app.kubernetes.io/name=cert-manager
```

### Unseal keys lost

If the `openbao-init-keys` Secret is deleted, the unseal keys cannot be recovered.
OpenBao must be completely redeployed (delete PVCs, delete pods, re-run init):

```bash
kubectl delete pvc -n openbao-system -l app.kubernetes.io/name=openbao
kubectl delete pods -n openbao-system -l app.kubernetes.io/name=openbao
# Wait for pods to restart, then re-run init-unseal.sh
```

### Script fails with "permission denied"

Ensure scripts have execute permissions:

```bash
chmod +x deploy/openbao/bootstrap/*.sh
```

### ExternalSecrets stuck in "SecretSyncedError"

Verify the ClusterSecretStore is healthy:

```bash
kubectl get clustersecretstore openbao-cluster-store -o jsonpath='{.status.conditions}'
```

Common causes:

- OpenBao is sealed (re-run `init-unseal.sh`)
- ESO service account missing (verify `external-secrets` SA exists in `external-secrets` namespace)
- TLS trust failure (verify `openbao-tls` Secret contains `ca.crt` key)
- Missing client certificate: verify the `eso-openbao-client-tls`
  Secret exists in `openbao-system` and the `ClusterSecretStore`
  `spec.provider.vault.tls.certSecretRef` / `keySecretRef` point at it. With
  `tls_require_and_verify_client_cert = true` on the listener, an absent or
  mis-referenced client cert appears as a generic TLS handshake error in the
  ESO controller log (`remote error: tls: bad certificate` or similar), not as
  an HTTP 401/403.

### Verify mTLS enforcement

Confirm the listener rejects a client that does not present a valid certificate.
The probe runs entirely inside the pod (so the CA bundle is on the
filesystem and reaches the loopback listener) and exits **non-zero** on success,
because the handshake must fail:

```bash
# Expected output ends with:
#   curl: (35) ... alert certificate required        OR
#   curl: (56) ... tls: certificate required         OR
#   exit code 35/56 from curl with no body returned.
# Exit code MUST be non-zero. A 200 OK from this command would indicate that
# tls_require_and_verify_client_cert is NOT being enforced and is a P0 incident.
kubectl exec -n openbao-system openbao-0 -- \
  sh -c 'curl --cacert /openbao/tls/ca.crt -sS -o /dev/null \
             -w "http_code=%{http_code}\n" \
             https://127.0.0.1:8200/v1/sys/health; echo "exit=$?"'
```

Then confirm the same call succeeds **with** the client cert (this is what
`bao_exec` does on every reconcile):

```bash
# Expected: http_code=200 (or 429 if standby), exit=0.
kubectl exec -n openbao-system openbao-0 -- \
  sh -c 'curl --cacert /openbao/tls/ca.crt \
             --cert   /openbao/client-tls/tls.crt \
             --key    /openbao/client-tls/tls.key \
             -sS -o /dev/null -w "http_code=%{http_code}\n" \
             https://127.0.0.1:8200/v1/sys/health; echo "exit=$?"'
```

If the first command unexpectedly returns `http_code=200`, the listener is not
enforcing client-cert auth — re-check
`deploy/flux-system/releases/openbao.yaml` for `tls_client_ca_file` and
`tls_require_and_verify_client_cert = true`, and that the HelmRelease has
reconciled (`kubectl get helmrelease openbao -n openbao-system`).

## Related Resources

- [Infrastructure Manifests](./infrastructure-manifests.md) — FluxCD base deployment
- `deploy/flux-system/releases/openbao.yaml` — OpenBao HelmRelease
- `deploy/flux-system/infrastructure/openbao-tls-cert.yaml` — server TLS Certificate
- `deploy/flux-system/infrastructure/openbao-client-tls-cert.yaml` — client TLS Certificates
- `deploy/eso/clustersecretstore.yaml` — ClusterSecretStore configuration (now uses client-cert mTLS)
- `deploy/eso/externalsecrets/` — ExternalSecret resources
