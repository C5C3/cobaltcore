---
title: Enable Keystone Database TLS/mTLS
quadrant: operator
---

# How-to: Enable Keystone Database TLS/mTLS

This guide walks an operator through opting a control plane's Keystone into
encrypted, mutually-authenticated connections to MariaDB/MaxScale. On a
ControlPlane deployment the knob lives on the `ControlPlane` CR
(`spec.infrastructure.database.tls`); the c5c3-operator projects the whole
database block — including `tls` — onto the projected `controlplane-keystone`
child. When enabled, the keystone-operator provisions a cert-manager
`Certificate` from the shared OpenStack DB CA, mounts the resulting keypair into
every Keystone workload that opens a database connection, and appends the
`ssl_*` parameters to the database DSN so the live transport is TLS-protected.

For the authoritative field reference, see
[DatabaseTLSSpec](../reference/keystone/keystone-crd.md#databasetlsspec) in the
Keystone CRD reference and
[InfrastructureSpec](../reference/c5c3/controlplane-crd.md#infrastructurespec) in
the ControlPlane CRD reference. For the underlying MariaDB and CA issuer
manifests, see
[OpenStack DB CA Issuer](../reference/infrastructure/infrastructure-manifests.md#openstack-db-ca-issuer)
and
[MariaDB Galera Cluster](../reference/infrastructure/infrastructure-manifests.md#mariadb-galera-cluster).

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

1. **cert-manager installed.** The chart from `deploy/flux-system/releases/cert-manager.yaml`
   provides the `cert-manager.io/v1` CRDs (`Certificate`, `Issuer`, `ClusterIssuer`).
   Confirm the controller is healthy:

   ```bash
   kubectl -n cert-manager get pods
   ```

2. **`openstack-db-ca-issuer` ClusterIssuer Ready.** The dedicated DB CA is
   declared in `deploy/flux-system/infrastructure/db-ca-issuer.yaml`.
   `hack/deploy-infra.sh` applies it in Phase 2. Verify:

   ```bash
   kubectl get clusterissuer openstack-db-ca-issuer \
     -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}'
   ```

   Expected: `True`.

3. **A MariaDB that offers server-side TLS.** Client-side DB TLS only works
   against a server that speaks it. On the **managed** ControlPlane devstack the
   c5c3-operator provisions the `openstack-db` MariaDB from a minimal spec and
   deliberately leaves server-side TLS/issuerRefs unset — DB-server hardening is a
   platform-team concern outside the aggregate's knowledge (see the DECISION on
   `reconcileInfrastructure` in `operators/c5c3/internal/controller/reconcile_infrastructure.go`).
   So before enabling client DB TLS on a ControlPlane, run a MariaDB with
   `spec.tls.enabled=true` (and, for the plaintext-rejection check below,
   `required=true`) — either harden the managed MariaDB out-of-band or point
   `spec.infrastructure.database` at a brownfield MariaDB that already enables TLS
   via the same `openstack-db-ca-issuer`. Confirm the cluster is healthy:

   ```bash
   kubectl -n openstack get mariadb openstack-db
   ```

   Expected: `STATUS=Ready`.

   ::: warning Do not enable `verify-full` against a plaintext MariaDB
   Enabling `verify-full` client TLS against a MariaDB that offers no server TLS
   breaks the connection — the child Keystone reports `DatabaseReady=False`.
   The [standalone Quick Start](#standalone-keystone-without-a-controlplane)
   devstack ships its `openstack-db` with `spec.tls.required=true`, so the
   standalone flow at the end of this guide is fully runnable end-to-end without
   extra MariaDB hardening.
   :::

4. **keystone-operator running.** Either via the Helm release in
   `deploy/flux-system/releases/keystone-operator.yaml` or a locally built
   image deployed with `hack/ci-deploy-operator.sh`. The operator's RBAC must
   include the `cert-manager.io/certificates` rule; the chart's
   ClusterRole carries it by default.

5. **The `controlplane` ControlPlane CR you own.** The `tls` block is an optional
   pointer on `spec.infrastructure.database` (a `nil` value preserves the previous
   plaintext behavior), and it is **not** frozen by the ControlPlane immutability
   webhook, so flipping it on is a no-mutation patch on a live control plane.

---

## Steps

### 1. Patch the `ControlPlane` CR with a `spec.infrastructure.database.tls` block

Set the `tls` block on the `ControlPlane` CR's shared database, **not** on the
projected Keystone child. The reconciler deep-copies the whole
`spec.infrastructure.database` block — `tls` included — onto
`controlplane-keystone` on every reconcile. The minimal TLS-enabled block uses
`mode: verify-full` (the strongest mode — verifies the server certificate chain
AND that the server hostname matches the certificate identity). In managed mode,
the keystone-operator provisions the Secret `controlplane-keystone-db-client`
carrying `ca.crt`, `tls.crt`, and `tls.key`, so a single reference satisfies both
`caBundleSecretRef` and `clientCertSecretRef`:

```yaml
spec:
  infrastructure:
    database:
      clusterRef:
        name: openstack-db
      database: keystone
      secretRef:
        name: keystone-db
      tls:
        mode: verify-full
        caBundleSecretRef:
          name: controlplane-keystone-db-client
        clientCertSecretRef:
          name: controlplane-keystone-db-client
```

Apply via patch:

```bash
kubectl -n openstack patch controlplane controlplane --type merge --patch '
spec:
  infrastructure:
    database:
      tls:
        mode: verify-full
        caBundleSecretRef:
          name: controlplane-keystone-db-client
        clientCertSecretRef:
          name: controlplane-keystone-db-client
'
```

::: warning Do not patch `spec.database.tls` on the projected child
Setting `spec.database.tls` on the `controlplane-keystone` Keystone CR directly
is reverted on the next reconcile: the c5c3-operator re-asserts the **entire**
`spec.database` block from `spec.infrastructure.database`, so any `tls` you write
on the child that is not present on the `ControlPlane` CR is deep-copied away.
Always set `tls` on the `ControlPlane` CR.
:::

The keystone-operator's mutating webhook does **not** materialize the `tls` block
on the child when it is absent — TLS is strictly opt-in. When `tls` is present
with an empty `mode`, the webhook materializes `mode: "require"` as the documented
baseline (a present block means "on"). Set `mode: disabled` to keep the block and
its certificate references while turning TLS off.

### 2. Wait for the operator to issue the client `Certificate`

`reconcileDatabaseTLS` on the child creates a cert-manager `Certificate` named
`controlplane-keystone-db-client` with `issuerRef = openstack-db-ca-issuer`
(ClusterIssuer). cert-manager writes the resulting keypair into a Secret of the
same name. The reconciler reports progress via the `DatabaseTLSReady` status
condition on the child:

| Condition reason | Meaning |
| --- | --- |
| `NotRequired` | `tls` is `nil`, its `mode` is empty, or its `mode` is `disabled` — plaintext connection. |
| `CertificatePending` | Managed mode; cert-manager has not yet issued the leaf. |
| `CertificateIssued` | Managed mode; client keypair ready and mounted. |
| `ExternallyManaged` | Brownfield mode (`spec.infrastructure.database.host` set) — the client keypair must be supplied out-of-band. |

---

## Verification

### 1. `DatabaseTLSReady=True` with `reason=CertificateIssued`

```bash
kubectl -n openstack get keystone controlplane-keystone \
  -o jsonpath='{range .status.conditions[?(@.type=="DatabaseTLSReady")]}{.status} {.reason} {.message}{"\n"}{end}'
```

Expected: `True CertificateIssued Database client Certificate "controlplane-keystone-db-client" issued into Secret "controlplane-keystone-db-client"`.

The Secret should carry the three keys cert-manager writes:

```bash
kubectl -n openstack get secret controlplane-keystone-db-client \
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
  -l app.kubernetes.io/instance=controlplane-keystone,app.kubernetes.io/name=keystone \
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
`spec.tls.required=true` (see prerequisite 3).

### 3. Plaintext connections are rejected by MariaDB

This check requires the MariaDB from prerequisite 3 to set
`spec.tls.required=true`, so any connection that does not negotiate TLS is
rejected at the transport layer before authentication. The `probe`/`probe`
credentials below are deliberately bogus — they are never checked, because the
server rejects the plaintext handshake before it reaches authentication. Probe
from inside the Keystone Pod by deliberately omitting the `ssl=` kwarg:

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
verifications against a standalone Keystone CR (whose `openstack-db` ships
`tls.required=true`):

```bash
chainsaw test --test-dir tests/e2e/keystone/database-tls/
```

The suite is also listed in the
[E2E inventory](../reference/keystone/keystone-crd.md#chainsaw-e2e-tests) of
the Keystone CRD reference.

---

## Disabling

> **WARNING — data-plane outage if you skip the MariaDB step.** If the MariaDB
> serving the control plane ships with `spec.tls.required=true`, disabling TLS on
> the Keystone side **only** will brick the deployment: MariaDB rejects every
> plaintext handshake at the transport layer, so Keystone cannot connect to the
> database. Before applying the ControlPlane patch below, set
> `spec.tls.required=false` (or fully disable TLS) on the MariaDB CR — updating
> MariaDB is out of scope of this guide but is a prerequisite for a working
> plaintext deployment.

To revert to plaintext without uninstalling the operator, drop the `tls` block
from the `ControlPlane` CR. The reconciler then deep-copies a `nil` `tls` onto the
child and the keystone-operator's mutating webhook never re-materializes it, so
the absence is persistent:

```bash
kubectl -n openstack patch controlplane controlplane --type json --patch '[
  {"op": "remove", "path": "/spec/infrastructure/database/tls"}
]'
```

The child's `DatabaseTLSReady` condition transitions to `True` with
`reason=NotRequired`, the operator deletes the managed
`controlplane-keystone-db-client` `Certificate` (so cert-manager stops renewing
it), cert-manager then garbage-collects the issued Secret via the `Certificate`
owner-reference cascade, and subsequent connections fall back to plaintext TCP —
but only succeed if MariaDB's `spec.tls.required` is also turned off (see warning
above).

---

## Standalone Keystone, without a ControlPlane

On the [Quick Start](../quick-start.md) / [Quick Start (Extended)](../quick-start-extended.md)
devstacks a standalone Keystone CR named `keystone` runs with no ControlPlane
projecting it, and the shared `openstack-db` MariaDB ships with
`spec.tls.enabled=true, required=true` — so this flow is fully runnable
end-to-end without extra MariaDB hardening. Set the `tls` block on the Keystone
CR's own `spec.database`:

```bash
kubectl -n openstack patch keystone keystone --type merge --patch '
spec:
  database:
    tls:
      mode: verify-full
      caBundleSecretRef:
        name: keystone-db-client
      clientCertSecretRef:
        name: keystone-db-client
'
```

The names map as follows; substitute them in every command above:

| ControlPlane devstack | Standalone devstack |
| --- | --- |
| CR `controlplane` (patch `spec.infrastructure.database.tls`) | CR `keystone` (patch `spec.database.tls`) |
| child `controlplane-keystone` | CR `keystone` |
| Certificate / Secret `controlplane-keystone-db-client` | `keystone-db-client` |
| pod selector `app.kubernetes.io/instance=controlplane-keystone` | `app.kubernetes.io/instance=keystone` |

To disable, drop the `tls` block from the Keystone CR:

```bash
kubectl -n openstack patch keystone keystone --type json --patch '[
  {"op": "remove", "path": "/spec/database/tls"}
]'
```

The `tests/e2e/keystone/database-tls/` chainsaw suite pins the keystone-operator
mechanics through this standalone flow.

---

## See also

- [Keystone CRD — DatabaseTLSSpec](../reference/keystone/keystone-crd.md#databasetlsspec) — authoritative field reference.
- [Keystone CRD — Mode → connect-args mapping](../reference/keystone/keystone-crd.md#mode--connect-args-mapping) — DSN parameters per mode.
- [ControlPlane CRD — InfrastructureSpec](../reference/c5c3/controlplane-crd.md#infrastructurespec) — the `spec.infrastructure.database` block the reconciler projects.
- [Infrastructure Manifests — OpenStack DB CA Issuer](../reference/infrastructure/infrastructure-manifests.md#openstack-db-ca-issuer) — CA keypair and ClusterIssuer.
- [Infrastructure Manifests — MariaDB Galera Cluster](../reference/infrastructure/infrastructure-manifests.md#mariadb-galera-cluster) — server-side TLS configuration.
- [`tests/e2e/keystone/database-tls/`](https://github.com/c5c3/forge/tree/main/tests/e2e/keystone/database-tls) — chainsaw E2E suite.
