<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->
---
title: Enable Keystone Database TLS/mTLS
quadrant: operator
---

# How-to: Enable Keystone Database TLS/mTLS

This guide walks an operator through opting a `Keystone` CR into encrypted,
mutually-authenticated connections to MariaDB/MaxScale. When enabled,
the keystone-operator provisions a cert-manager `Certificate` from the shared
OpenStack DB CA, mounts the resulting keypair into every Keystone workload that
opens a database connection, and appends the `ssl_*` parameters to the database
DSN so the live transport is TLS-protected.

For the authoritative field reference, see
[DatabaseTLSSpec](../reference/keystone/keystone-crd.md#databasetlsspec) in the
Keystone CRD reference. For the underlying MariaDB and CA issuer manifests, see
[OpenStack DB CA Issuer](../reference/infrastructure/infrastructure-manifests.md#openstack-db-ca-issuer)
and
[MariaDB Galera Cluster](../reference/infrastructure/infrastructure-manifests.md#mariadb-galera-cluster).

---

## Prerequisites

1. **cert-manager installed.** The chart from `deploy/flux-system/releases/cert-manager.yaml`
   provides the `cert-manager.io/v1` CRDs (`Certificate`, `Issuer`, `ClusterIssuer`).
   Confirm the controller is healthy:

   ```bash
   kubectl -n cert-manager get pods
   ```

2. **`openstack-db-ca-issuer` ClusterIssuer Ready.** The dedicated DB CA is
   declared in `deploy/flux-system/infrastructure/db-ca-issuer.yaml`.
   `hack/deploy-infra.sh` applies it in Phase 2 so MariaDB can resolve
   the issuer when it renders its server certificate. Verify:

   ```bash
   kubectl get clusterissuer openstack-db-ca-issuer \
     -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
   ```

   Expected: `True`.

3. **MariaDB CR with `spec.tls.enabled=true, required=true`.** The MariaDB
   manifest under `deploy/flux-system/infrastructure/mariadb.yaml` enables TLS
   on the Galera nodes and the MaxScale listener via the same
   `openstack-db-ca-issuer`. Confirm the cluster is healthy:

   ```bash
   kubectl -n openstack get mariadb openstack-db
   ```

   Expected: `STATUS=Ready`.

4. **keystone-operator running.** Either via the Helm release in
   `deploy/flux-system/releases/keystone-operator.yaml` or a local
   `make deploy-operator`. The operator's RBAC must include the
   `cert-manager.io/certificates` rule; the chart's
   ClusterRole carries it by default.

5. **A `Keystone` CR you control.** New CRs and existing plaintext CRs both
   work — the `tls` block is an optional pointer (a `nil` value preserves the
   previous plaintext behavior), so flipping it on is a no-mutation patch.

---

## Steps

### 1. Patch the Keystone CR with a `spec.database.tls` block

The minimal TLS-enabled block uses `mode: verify-full` (the strongest mode —
verifies the server certificate chain AND that the server hostname matches the
certificate identity). In managed mode, the operator-provisioned Secret
`<name>-db-client` carries `ca.crt`, `tls.crt`, and `tls.key`, so a single
reference satisfies both `caBundleSecretRef` and `clientCertSecretRef`:

```yaml
spec:
  database:
    clusterRef:
      name: openstack-db
    database: keystone
    secretRef:
      name: keystone-db
    tls:
      enabled: true
      mode: verify-full
      caBundleSecretRef:
        name: keystone-db-client
      clientCertSecretRef:
        name: keystone-db-client
```

Apply via patch or full re-apply:

```bash
kubectl -n openstack patch keystone keystone --type merge --patch '
spec:
  database:
    tls:
      enabled: true
      mode: verify-full
      caBundleSecretRef:
        name: keystone-db-client
      clientCertSecretRef:
        name: keystone-db-client
'
```

The mutating webhook does **not** materialize the `tls` block when it is
omitted, and never sets `enabled` — TLS is strictly opt-in.
When `tls` is present with an empty `mode`, the webhook materializes
`mode: "require"` as the documented baseline.

### 2. Wait for the operator to issue the client `Certificate`

`reconcileDatabaseTLS` creates a cert-manager `Certificate` named
`<keystone-name>-db-client` with `issuerRef = openstack-db-ca-issuer`
(ClusterIssuer). cert-manager writes the resulting keypair into a Secret of the
same name. The reconciler reports progress via the `DatabaseTLSReady` status
condition:

| Condition reason | Meaning |
| --- | --- |
| `NotRequired` | `tls` is `nil` or `enabled=false` — plaintext connection. |
| `CertificatePending` | Managed mode; cert-manager has not yet issued the leaf. |
| `CertificateIssued` | Managed mode; client keypair ready and mounted. |
| `ExternallyManaged` | Brownfield mode (`spec.database.host` set) — the client keypair must be supplied out-of-band. |

---

## Verification

### 1. `DatabaseTLSReady=True` with `reason=CertificateIssued`

```bash
kubectl -n openstack get keystone keystone \
  -o jsonpath='{range .status.conditions[?(@.type=="DatabaseTLSReady")]}{.status} {.reason} {.message}{"\n"}{end}'
```

Expected: `True CertificateIssued Database client Certificate "<name>-db-client" issued into Secret "<name>-db-client"`.

The Secret should carry the three keys cert-manager writes:

```bash
kubectl -n openstack get secret keystone-db-client \
  -o go-template='{{range $k, $_ := .data}}{{$k}}{{"\n"}}{{end}}'
```

Expected: `ca.crt`, `tls.crt`, `tls.key`.

### 2. The live connection is encrypted (`Ssl_cipher` non-empty)

Exec into a running Keystone Pod and ask MariaDB to report the cipher in use
for this very session. The Keystone container has `pymysql` installed (it is
the MySQL driver pinned by `openstack/requirements`) and the `db-tls` volume
mounted at `/etc/keystone/db-tls/`, so we can parse the same
`OS_DATABASE__CONNECTION` DSN the API uses and dial directly:

```bash
POD=$(kubectl -n openstack get pods \
  -l app.kubernetes.io/instance=keystone,app.kubernetes.io/name=keystone \
  -o jsonpath='{.items[0].metadata.name}')

kubectl -n openstack exec "$POD" -c keystone -- python3 -c '
import os, ssl, pymysql
from urllib.parse import urlparse, parse_qs
url = urlparse(os.environ["OS_DATABASE__CONNECTION"])
qs = parse_qs(url.query)
ssl_kwargs = {}
if "ssl_ca" in qs:    ssl_kwargs["ca"] = qs["ssl_ca"][0]
if "ssl_cert" in qs:  ssl_kwargs["cert"] = qs["ssl_cert"][0]
if "ssl_key" in qs:   ssl_kwargs["key"] = qs["ssl_key"][0]
if ssl_kwargs:
    ssl_kwargs["verify_mode"] = ssl.CERT_REQUIRED if qs.get("ssl_verify_cert", ["false"])[0].lower() == "true" else ssl.CERT_NONE
    ssl_kwargs["check_hostname"] = qs.get("ssl_verify_identity", ["false"])[0].lower() == "true"
kw = dict(host=url.hostname, port=url.port or 3306,
          user=url.username, password=url.password,
          database=url.path.lstrip("/"))
if ssl_kwargs: kw["ssl"] = ssl_kwargs
conn = pymysql.connect(**kw)
with conn.cursor() as cur:
    cur.execute("SHOW STATUS LIKE %s", ("Ssl_cipher",))
    row = cur.fetchone()
    print(row[1] if row else "")
conn.close()
'
```

Expected: a non-empty TLS cipher name (e.g.,
`TLS_AES_256_GCM_SHA384`). An empty value means the live connection is **not**
encrypted — re-check `DatabaseTLSReady` and confirm the MariaDB CR has
`spec.tls.required=true`.

### 3. Plaintext connections are rejected by MariaDB

Because the MariaDB CR sets `spec.tls.required=true`, any
connection that does not negotiate TLS is rejected at the transport layer
before authentication. Probe from inside the Keystone Pod by deliberately
omitting the `ssl=` kwarg:

```bash
kubectl -n openstack exec "$POD" -c keystone -- python3 -c '
import pymysql
try:
    pymysql.connect(host="openstack-db.openstack.svc", port=3306,
                    user="probe", password="probe",
                    database="keystone", connect_timeout=10)
    print("PLAINTEXT_ACCEPTED")
except Exception as exc:
    print("PLAINTEXT_REJECTED:", type(exc).__name__, exc)
'
```

Expected: `PLAINTEXT_REJECTED: …` from the server's TLS-required enforcement.

### 4. Run the end-to-end chainsaw test (canonical check)

The repository ships a chainsaw E2E suite that pins all three of the above
verifications:

```bash
chainsaw test --test-dir tests/e2e/keystone/database-tls/
```

The suite is also listed in the
[E2E inventory](../reference/keystone/keystone-crd.md#chainsaw-e2e-tests) of
the Keystone CRD reference.

---

## Disabling

> **WARNING — data-plane outage if you skip the MariaDB step.** The MariaDB CR
> in `deploy/flux-system/infrastructure/mariadb.yaml` ships with
> `spec.tls.required=true`. Disabling TLS on the Keystone side **only** will
> brick the deployment: MariaDB rejects every plaintext handshake at the
> transport layer, so Keystone cannot connect to the database. Before applying
> the Keystone patch below, set `spec.tls.required=false` (or fully disable
> TLS) on the MariaDB CR — updating MariaDB is out of scope of this guide but
> is a prerequisite for a working plaintext deployment.

To revert a CR to plaintext without uninstalling the operator, drop the `tls`
block. The mutating webhook never re-materializes it, so the
absence is persistent:

```bash
kubectl -n openstack patch keystone keystone --type json --patch '[
  {"op": "remove", "path": "/spec/database/tls"}
]'
```

The `DatabaseTLSReady` condition transitions to `True` with `reason=NotRequired`,
the `<name>-db-client` Secret is garbage-collected via the cert-manager
`Certificate` owner reference cascade, and subsequent connections fall back to
plaintext TCP — but only succeed if MariaDB's `spec.tls.required` is also
turned off (see warning above).

---

## See also

- [Keystone CRD — DatabaseTLSSpec](../reference/keystone/keystone-crd.md#databasetlsspec) — authoritative field reference.
- [Keystone CRD — Mode → connect-args mapping](../reference/keystone/keystone-crd.md#mode--connect-args-mapping) — DSN parameters per mode.
- [Infrastructure Manifests — OpenStack DB CA Issuer](../reference/infrastructure/infrastructure-manifests.md#openstack-db-ca-issuer) — CA keypair and ClusterIssuer.
- [Infrastructure Manifests — MariaDB Galera Cluster](../reference/infrastructure/infrastructure-manifests.md#mariadb-galera-cluster) — server-side TLS configuration.
- [`tests/e2e/keystone/database-tls/`](https://github.com/c5c3/forge/tree/main/tests/e2e/keystone/database-tls) — chainsaw E2E suite.
