---
title: Adopt an External Keystone
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Adopt an External Keystone

This guide walks a platform owner through putting an **existing** Keystone
installation under operator management without deploying anything into it.

A ControlPlane in **External mode** is service-less: it projects no MariaDB, no
Memcached, and no Keystone workload. It manages the *identity plane* against the
Keystone API you already run — minting and rotating the admin **application
credential**, provisioning declarative **service accounts**, and importing your
existing **service catalog** rather than writing to it. Your installation keeps
serving tokens throughout, and deleting the ControlPlane leaves it untouched.

This is the first step of a staged adoption. Taking over the database and the
Keystone deployment itself are later, separate phases — see
[Brownfield Keystone Adoption](../future/brownfield-keystone-adoption.md).

## Prerequisites

::: info Devstack
This guide is written against the
[Quick Start (ControlPlane)](../quick-start-controlplane.md).

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow it through its final **Verify** step, so the operators and the shared
infrastructure (OpenBao, External Secrets, K-ORC) are running. This guide adds a
**second, External-mode** ControlPlane in its own namespace; the managed
`controlplane` the tutorial creates in `openstack` stays as it is.
:::

Beyond the devstack, adopting a real installation needs:

- **A reachable Keystone API.** K-ORC dials it from inside the cluster. Nothing
  restricts the operator's egress by default, but on a cluster that enforces a
  restrictive egress policy you must explicitly allow traffic from the K-ORC
  namespace to the endpoint and port.
- **The admin password, as a Secret** in the ControlPlane's namespace. The
  operator never invents it and never rotates it — you supply it and rotate it at
  the installation (see [step 6](#_6-rotating-the-admin-password-out-of-band)).
- **A CA bundle Secret**, if the endpoint uses a private CA. The key defaults to
  `ca.crt`. An IP-based `authURL` needs an IP SAN in the certificate; a hostname
  resolves through cluster DNS.
- **A `spec.region` that matches your catalog.** The region and the selected
  interface must both exist in the external catalog, or the control plane fails
  loudly rather than importing nothing.

### Standing in a brownfield Keystone on kind

On kind there is no pre-existing installation to adopt, so create one. These are
the same fixtures the e2e suite uses: a plain, operator-free Keystone in
namespace `brownfield-keystone`, serving
`http://keystone.brownfield-keystone.svc:5000/v3`.

```bash
kubectl apply -f tests/e2e/c5c3/external-keystone/00-fixture-keystone.yaml
kubectl -n brownfield-keystone rollout status deploy/keystone --timeout=5m

kubectl apply -f tests/e2e/c5c3/external-keystone/01-fixture-catalog-setup-job.yaml
kubectl -n brownfield-keystone wait --for=condition=Complete \
  job/keystone-fixture-setup --timeout=5m
```

The setup Job deliberately makes this look like a *real* installation rather than
a fresh bootstrap:

- a **non-default admin identity** — user `brownfield-admin`, project
  `platform-admin`, domain `heimdall` — so nothing relies on the `admin`/`Default`
  names a bootstrap would leave behind;
- a **duplicate identity-type service** (`keystone-legacy` alongside `keystone`),
  which is what forces the catalog disambiguation in
  [step 2](#_2-apply-the-external-mode-controlplane).

Against a real installation, skip this section entirely.

## Steps

### 1. Create the namespace and the admin-password Secret

The ControlPlane needs its own namespace (one ControlPlane per namespace), and
the admin password of the existing installation as a Secret in it.

```bash
kubectl create namespace brownfield

# Prompt without echo, then pipe the value in on stdin so the admin password
# never appears on argv (visible in /proc/<pid>/cmdline for the life of the
# process) or in your shell history file.
read -rs -p 'Keystone admin password: ' PW; echo
printf '%s' "$PW" | kubectl -n brownfield create secret generic brownfield-admin-password \
  --from-file=password=/dev/stdin
unset PW
```

Type your installation's real admin password at the prompt. On the kind devstack
it is the fixture password `brownfield_admin_fixture_pw_0`, which the setup Job
of the previous section gave `brownfield-admin`.

Nothing else in this guide reads the password directly — everything downstream
authenticates with the application credential the operator mints from it.

### 2. Apply the External-mode ControlPlane

This is the manifest the e2e suite applies, imported from the suite itself. The
walkthrough and the suite share the namespace `brownfield`, the ControlPlane name
`controlplane-external`, and the admin-password Secret from step 1, so there is one
manifest here rather than a hand-kept copy of one — see [Tested by](#tested-by).

<<< @/../tests/e2e/c5c3/external-keystone/02-controlplane-external.yaml#controlplane-external

That ControlPlane is the only thing in the file — the suite keeps its fixture
admin-password Secret in a sibling file precisely so applying this one cannot
overwrite the Secret you filled in step 1 — so on the devstack apply it as it
stands:

```bash
kubectl apply -f tests/e2e/c5c3/external-keystone/02-controlplane-external.yaml
```

Against a real installation, copy it out and set `external.authURL`, `spec.region`,
and the admin identity to yours.

Field by field:

| Field | Why |
| --- | --- |
| `mode: External` | Selects the service-less path. No Keystone workload is deployed and no child CR is projected. |
| `external.authURL` | The identity endpoint the operator manages against. |
| `external.endpointType` | Which catalog interface to authenticate against. Omitted here, so it defaults to `public` — the interface that is normally reachable from outside the installation. |
| `external.caBundleSecretRef` | The private-CA bundle, when the endpoint needs one. Omitted here: the kind fixture is plain HTTP — devstack only, see the warning below. |
| `external.catalog.identityServiceName` | Disambiguates the identity-service import. Only needed when your catalog holds **more than one** `identity`-type service — as the fixture deliberately does. |
| `korc.adminCredential.cloudCredentialsRef` | Where the minted credential is materialized — the `clouds.yaml` Secret and cloud entry read back in [step 4](#_4-verify). Spelled out here, but `k-orc-clouds-yaml` / `admin` are the defaults. |
| `korc.adminCredential.passwordSecretRef` | The Secret from step 1. |
| `userName` / `projectName` / `domainName` | The admin identity to authenticate as. They default to `admin` / `admin` / `Default`; the fixture uses a non-default identity, so all three are set. |
| `applicationCredential.restricted` | Keeps the minted credential least-privilege — it cannot mint further application credentials. Spelled out here, but `true` is the default. |
| `applicationCredential.rotation.mode` | `PasswordDriven` keys the re-mint on a hash of the admin password — see [step 6](#_6-rotating-the-admin-password-out-of-band). Spelled out here, but it is the default. |
| `korc.serviceAccounts` | Declarative service accounts — see [step 5](#_5-declare-service-accounts). |

`spec.openStackRelease` stays required but is **advisory** in this mode: no images
are deployed, so it only has to match your installation at a future managed
takeover.

::: danger `authURL` must be `https://` against a real installation
The CRD admits `http://` so the kind fixture can run without certificates. It is
the *only* reason. Over plain HTTP the admin password travels in the clear on
every mint — and the minted application credential's id and secret come back in
the clear — to anything on the path between K-ORC and the endpoint. There is no
handshake to fail, so `TLSVerificationFailed` never fires: the failure mode is a
silent success.

Against anything but a throwaway devstack, use `https://` and supply the private
CA via `external.caBundleSecretRef`. Pairing that ref with an `http://` authURL is
rejected at admission — a CA bundle a plaintext endpoint never consults would only
manufacture false confidence.
:::

::: warning Adoption means a *new* CR, never a flipped one
The webhook **forbids** `spec.infrastructure`, `services.horizon`, and every
managed-only Keystone knob in External mode — it names the offending field. It
also rejects `mode` transitions **in both directions**: `Managed → External` is
refused, and `External → Managed` is reserved for the phase-3 takeover. So you
adopt an installation by creating a *new* External-mode ControlPlane, never by
flipping an existing managed one.
:::

### 3. What `Ready` means in this mode

Nothing is deployed, so the sub-reconcilers that would deploy something report
`Status=True` with reason `ExternallyManaged` — `InfrastructureReady`,
`DBCredentialsReady`, `AdminPasswordReady`, and `KeystoneReady`. They are *not*
evidence that anything converged.

The conditions that carry real signal are `KORCReady`, `AdminCredentialReady`,
`CatalogReady`, and `ServiceAccountsReady`. All of them are proxied through K-ORC:
the operator holds no OpenStack client of its own, so "can we reach and
authenticate against your Keystone?" is answered by whether the imports and the
application-credential mint succeed.

```bash
kubectl -n brownfield get controlplane controlplane-external
kubectl -n brownfield get controlplane controlplane-external \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

When something is wrong, `KORCReady` names the failure class:

| Reason | What it means | What to do |
| --- | --- | --- |
| `AuthenticationFailed` | Keystone rejected the admin credential (HTTP 401). | The password in the Secret is not (or is no longer) the installation's admin password. |
| `EndpointUnreachable` | `authURL` could not be dialled — DNS, connection refused, timeout. | Check the URL, cluster DNS, and your egress policy. |
| `TLSVerificationFailed` | The endpoint's certificate did not verify. | Supply the private CA via `external.caBundleSecretRef`. A **rotated** bundle only takes effect after K-ORC's provider cache expires (roughly half the token lifetime). |
| `CatalogEndpointMismatch` | Authentication worked, but the requested interface/region is absent from the catalog. | Correct `external.endpointType` or `spec.region`. |
| `ImportStalled` | An import is waiting for a resource that "will be created externally" — but in this mode every import target already exists, so it never resolves. | The operator is looking in the wrong place; the message names `endpoint_type` and `region`. |
| `CredentialDrift` | The installation changed underneath the CR. | Reconcile the CR with reality. Drift is **surfaced, never remediated** — the operator does not write to your installation. |

Imports resolve **once**. If an imported object is later replaced in Keystone, that
surfaces as drift rather than the operator silently re-pointing at the new one.

The full reason vocabulary for every condition is in the
[ControlPlane CRD reference](../reference/c5c3/controlplane-crd.md#status-conditions).

### 4. Verify

First, converge:

```bash
kubectl -n brownfield wait --for=condition=Ready \
  controlplane/controlplane-external --timeout=10m
```

Then prove the ControlPlane deployed **nothing** — this is the whole point of the
mode:

```bash
kubectl -n brownfield get mariadbs,memcacheds,keystones,deployments
# No resources found in brownfield namespace.
```

Finally, prove the minted credential actually authenticates against your
installation — not merely that the operator called itself Ready. Read it from the
materialized Secret:

```bash
kubectl -n brownfield get secret k-orc-clouds-yaml \
  -o jsonpath='{.data.clouds\.yaml}' | base64 -d
```

Use that `clouds.yaml` with an OpenStack client (`openstack token issue`,
`openstack catalog list`). The e2e suite does exactly this in a Job; see
[Tested by](#tested-by).

::: tip Read the credential from Kubernetes, not the OpenBao UI
On kind, OpenBao enforces mTLS, so browsing to it or using the `bao` CLI needs a
client certificate. The materialized Secret above is the supported handle.
:::

### 5. Declare service accounts

`korc.serviceAccounts` gives the service users of other OpenStack services
(nova, glance, …) a managed home: a Keystone user and project with an
operator-generated, OpenBao-backed, rotatable password.

The entry in [step 2](#_2-apply-the-external-mode-controlplane) declares user
`nova` with a **created** project `service-nova`. The semantics that matter:

- **`project.create: false`** references an existing project via an unmanaged
  import — it is never created, and never deleted.
- **`adopt: true`** is explicit consent to take over a **pre-existing** Keystone
  user of that name. Without it, a name collision fails loudly
  (`ServiceAccountCollision`) rather than silently hijacking the account. Note an
  adopted user becomes operator-owned, so it *is* deleted at teardown.
- **`roles`** are projected — each becomes an unmanaged K-ORC `Role` import plus a
  managed `RoleAssignment` binding the role to the user on the project, and the
  account is not Ready until every assignment lands in Keystone.

Each account's password is generated into OpenBao at
`openstack/keystone/{namespace}/{controlplane}/service-accounts/{name}` and
materialized as a stable consumer Secret:

```bash
kubectl -n brownfield get secret controlplane-external-service-account-nova-credentials \
  -o jsonpath='{.data.clouds\.yaml}' | base64 -d
```

Per-account readiness is reported individually in
`status.serviceAccounts[].ready`, so one lagging account is attributable without
decoding the aggregate condition.

### 6. Rotating the admin password, out-of-band

The admin password belongs to your installation, so it rotates **there** — the
operator has no database access and no rotation job in this mode.

After rotating it at the installation, **update the referenced Secret** — again
off the command line, since this one carries the *fresh* production password.

Writing that Secret is not bookkeeping: it *is* the trigger. The operator keys the
mint on a hash of the admin password, so the write re-mints the application
credential — and a re-mint is **destructive-first**: K-ORC deletes and revokes the
credential that is currently working *before* it tries the fresh one. So the value
has to be proven against Keystone **before** it is written. Type it twice, then
issue a token with it; the Secret is only written if both readings agree *and* your
installation accepts the password.

```bash
read -rs -p 'new Keystone admin password: ' PW; echo
read -rs -p 'confirm: ' PW2; echo

# The admin identity the ControlPlane authenticates as (spec.korc.adminCredential).
export OS_IDENTITY_API_VERSION=3
export OS_AUTH_URL=http://keystone.brownfield-keystone.svc:5000/v3
export OS_USERNAME=brownfield-admin
export OS_PROJECT_NAME=platform-admin
export OS_USER_DOMAIN_NAME=heimdall OS_PROJECT_DOMAIN_NAME=heimdall

if [ "$PW" != "$PW2" ]; then
  echo 'passwords do not match — Secret left untouched, nothing rotated' >&2
elif ! OS_PASSWORD="$PW" openstack token issue >/dev/null 2>&1; then
  echo 'password does not authenticate — Secret left untouched, credential intact' >&2
else
  printf '%s' "$PW" | kubectl -n brownfield create secret generic brownfield-admin-password \
    --from-file=password=/dev/stdin --dry-run=client -o yaml | kubectl -n brownfield replace -f -
fi
unset PW PW2
```

Run the token check from wherever `authURL` is reachable — that is the network
position K-ORC dials from, so a pass there answers the same question the operator
is about to ask. On the kind devstack the endpoint is a cluster-internal Service,
so run it in a pod (`kubectl run --rm -i --image=… -- openstack token issue`), as
the suite does.

**Neither gate is ceremony, and equality alone is not enough.** Comparing two
readings catches a *divergent* typo and nothing else. It passes just as happily
when you mistype the same thing twice, when you type the old password from muscle
memory, and when you update the Secret before you actually rotated at the
installation. Each of those changes the hash, so the re-mint fires all the same —
revoking the working credential and then failing `401` against a password Keystone
never accepted. You are left with no valid admin credential at all, `KORCReady` on
`AuthenticationFailed`, and every import and service-account projection driven by
it broken until a human notices and redoes the step. Asking Keystone first is the
difference between a rotation and an outage.

**Forgetting the Secret update is the classic drift.** The old password stops
authenticating, `KORCReady` goes to `AuthenticationFailed`, and the operator
reports it — it will not go hunting for the new password.

To force a re-mint without a password change, request one. `reMint: true` is what
makes it a rotation: without it the reconciler falls back to the password-hash
check, finds the hash unchanged, and reports `Ready=True` with reason
`NoRotationNeeded` having rotated nothing. The CR binds to the ControlPlane by
**namespace** — one ControlPlane per namespace — so there is no reference field:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: CredentialRotation
metadata:
  name: rotate-admin
  namespace: brownfield
spec:
  target: adminApplicationCredential
  reMint: true
```

The nudge is one-shot per spec generation, so a `reMint: true` left in the spec
fires once per edit rather than on every resync.

A service-account password rotates the same way, with
`target: serviceAccountPassword` and `serviceAccount: nova` — and there `reMint`
is not merely advisable but **required**: that path has no password-hash
auto-detect at all, so without it the request is a guaranteed no-op.

::: warning The minted credential must never be copied
A re-mint is **delete + recreate**, and the previous credential is revoked at the
Keystone level the moment it happens — any client still holding it starts getting
`404 Could not find Application Credential`, with no grace period.

So every consumer must read the credential from the materialized Secret (or its
OpenBao path) **at use time**. A copy pasted into another Secret, a config file, or
an environment variable dies without warning on the next rotation.
:::

### 7. OpenBao paths in this mode

External mode **never seeds** a bootstrap path: there is no operator-generated
admin password to seed. Only the application-credential and service-account paths
exist, and both are created on first push — there is no per-ControlPlane OpenBao
preparation to do, because the operator provisions the per-tenant store itself.

The full per-mode path catalog is in
[OpenBao paths per ControlPlane mode](../reference/infrastructure/openbao-bootstrap.md#openbao-paths-per-controlplane-mode).

### 8. Deletion — zero blast radius

Deleting the ControlPlane tears down **only what it created or adopted**:

```bash
kubectl -n brownfield delete controlplane controlplane-external
```

- The admin **application credential is revoked** (K-ORC's finalizer), and the
  OpenBao-backed Secrets are removed.
- **Managed** service-account users and projects are deleted from Keystone —
  including any you marked `adopt: true`.
- **Every unmanaged import is untouched**: the admin user, the domain, the catalog
  services and endpoints, and any project referenced with `project.create: false`.
- Your Keystone keeps serving tokens throughout.

Ordering is held by the `c5c3.io/orc-teardown` finalizer, so the credential is
revoked against a still-reachable Keystone before the CR leaves etcd.

## See also

- [ControlPlane CRD reference](../reference/c5c3/controlplane-crd.md#externalkeystonespec) —
  `ExternalKeystoneSpec` and the full [status-condition catalog](../reference/c5c3/controlplane-crd.md#status-conditions).
- [ControlPlane reconciler reference](../reference/c5c3/controlplane-reconciler.md) —
  what each sub-reconciler does in External mode.
- [OpenBao bootstrap reference](../reference/infrastructure/openbao-bootstrap.md#openbao-paths-per-controlplane-mode) —
  which paths exist per mode.
- [Brownfield Keystone Adoption](../future/brownfield-keystone-adoption.md) —
  the later phases (infrastructure attach, service takeover).

## Tested by

```bash
chainsaw test --test-dir tests/e2e/c5c3/external-keystone
```

or `make e2e-external-keystone`. The suite stands up the brownfield Keystone,
adopts it with an External-mode ControlPlane, authenticates an OpenStack client
with the minted credential, rotates it, and asserts the imports survive deletion.

The suite runs in namespace `brownfield` with the ControlPlane
`controlplane-external` — exactly the names this walkthrough uses — so
[step 2](#_2-apply-the-external-mode-controlplane) imports the suite's own
manifest rather than restating it.
