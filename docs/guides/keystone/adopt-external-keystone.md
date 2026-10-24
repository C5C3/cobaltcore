---
title: Adopt an External Keystone
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# Adopt an External Keystone

This guide shows how to connect a ControlPlane to a Keystone installation that
already exists.

External mode does not deploy Keystone, MariaDB, or Memcached. The c5c3 operator
configures K-ORC to create and rotate the admin application credential and to
manage service accounts declared as `KeystoneService` resources. It also asks
K-ORC to import the existing admin identity, domain, identity service, and
endpoints as unmanaged resources; those imports are not changed or deleted by
the ControlPlane. When the ControlPlane is deleted, it removes resources it
managed, including the generated credential and managed service accounts, while
the existing Keystone installation and imported objects remain.

Adoption has four phases. This guide covers the implemented first phase; the
remaining phases are sketches. You can stop after any phase. See
[Brownfield Keystone Adoption](../../future/brownfield-keystone-adoption.md)
for details.

## Prerequisites

::: info Devstack
This guide is written against the
[Deploy a ControlPlane](../../quick-start-controlplane.md).

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow it through its final Verify step, so the operators and the shared
infrastructure (OpenBao, External Secrets, K-ORC) are running. This guide adds a
second, External-mode ControlPlane in its own namespace. The managed
`controlplane` the tutorial creates in `openstack` stays as it is.
:::

Beyond the devstack, adopting a real installation needs:

- A Keystone API reachable from the cluster. K-ORC connects from inside the
  cluster. If egress policy restricts traffic, allow the K-ORC namespace to
  reach the endpoint and port.
- The existing admin password, stored in a Secret in the ControlPlane namespace.
  The operator does not create or rotate this password. Rotate it at the
  installation, then update the Secret as described in
  [step 6](#_6-rotating-the-admin-password-out-of-band).
- A CA bundle Secret when the endpoint uses a private CA. Its key defaults to
  `ca.crt`. An IP-based `authURL` needs an IP SAN in the certificate; a hostname
  must resolve through cluster DNS.
- A `spec.region` that exists in the external catalog. The selected interface
  must also exist there; otherwise the ControlPlane reports the catalog error.

### Standing in a brownfield Keystone on kind

On kind there is no pre-existing installation to adopt, so create one. These are
the fixtures the e2e suite uses: a plain, operator-free Keystone in
namespace `brownfield-keystone`, serving
`http://keystone.brownfield-keystone.svc:5000/v3`.

```bash
kubectl apply -f tests/e2e/c5c3/external-keystone/00-fixture-keystone.yaml
kubectl -n brownfield-keystone rollout status deploy/keystone --timeout=5m

kubectl apply -f tests/e2e/c5c3/external-keystone/01-fixture-catalog-setup-job.yaml
kubectl -n brownfield-keystone wait --for=condition=Complete \
  job/keystone-fixture-setup --timeout=5m
```

The setup Job adds objects that are already present in a brownfield
installation:

- a non-default admin identity: user `brownfield-admin`, project
  `platform-admin`, and domain `heimdall`;
- two catalog services with type `identity`: `keystone` and
  `keystone-legacy`. In [step 2](#_2-apply-the-external-mode-controlplane),
  select the one to import by its service name.

Against a real installation, skip this section entirely.

## Steps

### 1. Create the namespace and the admin-password Secret

The ControlPlane needs its own namespace (one ControlPlane per namespace). Store
the existing installation's admin password in a Secret there.

```bash
kubectl create namespace brownfield

# Prompt without echo, then pipe the value in on stdin so the admin password
# never appears on argv (visible in /proc/<pid>/cmdline for the life of the
# process) or in your shell history file.
printf 'Keystone admin password: '
IFS= read -r -s PW
printf '\n'
printf '%s' "$PW" | kubectl -n brownfield create secret generic brownfield-admin-password \
  --from-file=password=/dev/stdin
unset PW
```

Type your installation's real admin password at the prompt. On the kind devstack
it is the fixture password `brownfield_admin_fixture_pw_0`, which the setup Job
of the previous section gave `brownfield-admin`.

Later requests authenticate with the application credential created from this
password; no other step reads the password.

### 2. Apply the External-mode ControlPlane

This is the manifest the e2e suite applies, imported from the suite itself. The
walkthrough and suite use the same namespace (`brownfield`), ControlPlane name
(`controlplane-external`), and admin-password Secret. See
[Tested by](#tested-by).

<<< @/../tests/e2e/c5c3/external-keystone/02-controlplane-external.yaml#controlplane-external

The file contains only the ControlPlane. The suite keeps its fixture
admin-password Secret in a separate file, so this apply will not overwrite the
Secret from step 1. On the devstack, apply the manifest as provided:

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
| `external.endpointType` | Catalog interface K-ORC authenticates against. Omitted here, so it defaults to `public`, usually reachable from outside the installation. |
| `external.caBundleSecretRef` | Private-CA bundle for the endpoint. Omitted here because the kind fixture uses plain HTTP; see the warning below. |
| `external.catalog.identityServiceName` | Exact name of the `identity` service to import. Set this when the catalog has multiple services of type `identity`; the fixture selects `keystone` from `keystone` and `keystone-legacy`. |
| `korc.adminCredential.cloudCredentialsRef` | Secret and cloud entry where K-ORC writes the generated credential. Step 4 reads the `clouds.yaml` from this Secret. The defaults are `k-orc-clouds-yaml` and `admin`. |
| `korc.adminCredential.passwordSecretRef` | The Secret from step 1. |
| `userName` / `projectName` / `domainName` | The admin identity to authenticate as. They default to `admin` / `admin` / `Default`; the fixture uses a non-default identity, so all three are set. |
| `applicationCredential.restricted` | Prevents the generated credential from creating additional application credentials. `true` is the default. |
| `applicationCredential.rotation.mode` | `PasswordDriven` uses a hash of the admin password to trigger credential replacement. This is also the default; see [step 6](#_6-rotating-the-admin-password-out-of-band). |

`spec.openStackRelease` remains required but is advisory in this mode. No images
are deployed; the value must match your installation only during a future
managed takeover.

::: danger `authURL` must be `https://` against a real installation
The CRD admits `http://` so the kind fixture can run without certificates. This
is the only reason to use HTTP. Over plain HTTP, the admin password and the
generated application credential travel in clear text between K-ORC and the
endpoint. TLS verification cannot detect an interception because there is no
TLS handshake to fail.

For any real installation, use `https://` and supply the private CA through
`external.caBundleSecretRef`. Admission rejects a CA bundle reference paired
with an `http://` authURL because a plaintext endpoint does not use that bundle.
:::

Create a new External-mode ControlPlane for adoption. The webhook rejects
`spec.infrastructure`, `services.horizon`, and managed-only Keystone fields in
External mode, and it rejects mode changes in either direction. The later
Managed-mode takeover is a separate phase; it does not change an existing CR.

### 3. What `Ready` means in this mode

Nothing is deployed, so the sub-reconcilers that would deploy resources report
`Status=True` with reason `ExternallyManaged`: `InfrastructureReady`,
`DBCredentialsReady`, `AdminPasswordReady`, and `KeystoneReady`. These conditions
show that the sub-reconcilers skipped those resources; they do not show that
anything converged.

The conditions that report useful progress are `KORCReady`, `AdminCredentialReady`,
and `CatalogReady`. All three are proxied through K-ORC. The operator holds no
OpenStack client of its own, so "can we reach and authenticate against your
Keystone?" is answered by whether imports and application-credential creation
succeed. `ServiceAccountsReady` is not among them here. It folds the
registrations the ControlPlane projects for its own built-in services, and an
External-mode plane manages none, so it reads `True` with reason
`NoServiceRegistrationsProjected` whatever your registrations are doing. Their
readiness is reported on each `KeystoneService` CR, as step 5 shows.

```bash
kubectl -n brownfield get controlplane controlplane-external
kubectl -n brownfield get controlplane controlplane-external \
  -o jsonpath='{range .status.conditions[*]}{.type}{"\t"}{.status}{"\t"}{.reason}{"\n"}{end}'
```

When something is wrong, `KORCReady` names the failure class:

| Reason | What it means | What to do |
| --- | --- | --- |
| `AuthenticationFailed` | Keystone rejected the admin credential (HTTP 401). | The password in the Secret is not (or is no longer) the installation's admin password. |
| `EndpointUnreachable` | K-ORC could not connect to `authURL` because of DNS, connection refusal, or timeout. | Check the URL, cluster DNS, and egress policy. |
| `TLSVerificationFailed` | The endpoint certificate did not verify. | Supply the private CA through `external.caBundleSecretRef`. A rotated bundle takes effect after K-ORC's provider cache expires (roughly half the token lifetime). |
| `CatalogEndpointMismatch` | Authentication worked, but the catalog lacks the requested interface or region. | Correct `external.endpointType` or `spec.region`. |
| `ImportStalled` | An import is waiting for a resource marked "will be created externally." In External mode, each import target must already exist, so this import will not complete. | Check the message's `endpoint_type` and `region`; verify that the named resource exists in Keystone. |
| `CredentialDrift` | The installation changed after the CR was created. | Reconcile the CR with the installation. The operator reports drift and does not write to the installation. |

The operator resolves each import once. If an imported object is later replaced
in Keystone, the change appears as drift; the operator does not select a
replacement automatically.

The full reason vocabulary for every condition is in the
[ControlPlane CRD reference](../../reference/c5c3/controlplane-crd.md#status-conditions).

### 4. Verify

First, converge:

```bash
kubectl -n brownfield wait --for=condition=Ready \
  controlplane/controlplane-external --timeout=10m
```

Then verify that the ControlPlane deployed no workloads:

```bash
kubectl -n brownfield get mariadbs,memcacheds,keystones,deployments
# No resources found in brownfield namespace.
```

Finally, verify that the generated credential authenticates against your
installation. Read it from the Kubernetes Secret that External Secrets
populates:

```bash
kubectl -n brownfield get secret k-orc-clouds-yaml \
  -o jsonpath='{.data.clouds\.yaml}' | base64 -d
```

Use that `clouds.yaml` with an OpenStack client (`openstack token issue`,
`openstack catalog list`). The e2e suite does this in a Job; see
[Tested by](#tested-by).

On kind, OpenBao enforces mTLS. Browsing to it or using the `bao` CLI requires a
client certificate; use the Kubernetes Secret shown above to read the credential.

### 5. Register a service account

Service users of other OpenStack services (nova, glance, …) are declared with a
`KeystoneService` CR, one per service. The ControlPlane itself carries no
account fields: the registration owns its Keystone user and project, the
operator-generated OpenBao-backed password, and the teardown of all three.

This is the registration the e2e suite creates in the ControlPlane's own
namespace, declaring a catalog entry and an account:

<<< @/../tests/e2e/c5c3/external-keystone/07-keystoneservice-nova.yaml#keystoneservice-nova

The semantics that matter:

- The CR's metadata names its child
  resources and the consumer Secret, and `metadata.namespace` is the delivery
  namespace. `account.userName` defaults to `metadata.name`.
- `account.project.create: true` makes the registration create
  `service-nova` and own it, so the project is deleted again at teardown. Set
  `create: false` to reference a project that already exists: it is then
  imported unmanaged, never created and never deleted.
- Neither `adopt` flag is set. Both default to rejecting conflicts, so a
  pre-existing Keystone user of the same name surfaces
  `ServiceAccountCollision` and a pre-existing catalog row of the same type and
  name surfaces `ServiceCollision`, instead of either being taken over
  silently. The fixture's catalog holds identity rows only, so the compute entry
  collides with nothing.

  Setting `account.adopt: true` is explicit consent to take over such a user.
  The operator then owns and deletes that user at teardown. The ControlPlane's
  admin identity cannot be adopted; the reconciler rejects it.

  With the currently pinned K-ORC, a newly adopted user keeps its existing
  password on the first pass, even though the registration reports
  `AccountReady=True` and its consumer Secret contains the generated password.
  Rotate the account once with a `CredentialRotation` to write the new password.
  This is tracked in
  [#920](https://github.com/C5C3/cobaltcore/issues/920).
- `account.roles` are projected. Each becomes an unmanaged K-ORC `Role`
  import plus a managed `RoleAssignment` binding the role to the user on the
  project,   and the account is not Ready until Keystone confirms every assignment.

A registration in the ControlPlane's namespace or in one of its dedicated
service namespaces is admitted. To register one in another namespace, first add
that namespace to `spec.korc.serviceRegistrations.allowedNamespaces` on the
ControlPlane.
[Register a Service the ControlPlane Does Not Manage](../register-a-foreign-service.md)
walks that flow.

The operator stores the generated password in OpenBao at
`openstack/keystone/{namespace}/{name}/service-accounts/credentials`. The path is
keyed by the `KeystoneService` CR, and External Secrets writes the value to a
consumer Secret with the same name:

```bash
kubectl -n brownfield get secret nova-credentials \
  -o jsonpath='{.data.clouds\.yaml}' | base64 -d
```

Readiness is reported on the registration itself (`Ready`, with `AccountReady`
and `CatalogReady` beneath it), so one lagging registration is attributable
without decoding the ControlPlane's aggregate condition.

### 6. Rotating the admin password, out-of-band

The admin password belongs to your installation. Rotate it there; the operator
has no database access and does not run a password-rotation job in this mode.

After rotating the password at the installation, update the referenced Secret.
The following command reads the new password without echoing it or putting it
in shell history.

Updating that Secret triggers credential replacement because the operator
compares a hash of its password with the hash used for the current application
credential. K-ORC deletes and revokes the current credential before creating its
replacement. Verify the new password against Keystone before writing the Secret.
The command below requires both password readings to match and the token request
to succeed.

::: details Verify the new password and update the Secret

```bash
(
  printf 'new Keystone admin password: '
  IFS= read -r -s PW
  printf '\n'
  printf 'confirm: '
  IFS= read -r -s PW2
  printf '\n'
  set -o pipefail

  if [ "$PW" != "$PW2" ]; then
    echo 'The entries differ. The Secret was not changed.' >&2
    exit 1
  elif ! token_output=$(
    printf '%s\n' "$PW" | kubectl run --rm -i --restart=Never \
      --namespace=brownfield \
      --image=ghcr.io/c5c3/tempest:2026.1 \
      --env=OS_IDENTITY_API_VERSION=3 \
      --env=OS_AUTH_URL=http://keystone.brownfield-keystone.svc:5000/v3 \
      --env=OS_USERNAME=brownfield-admin \
      --env=OS_PROJECT_NAME=platform-admin \
      --env=OS_USER_DOMAIN_NAME=heimdall \
      --env=OS_PROJECT_DOMAIN_NAME=heimdall \
      --command -- /bin/sh -c \
      'IFS= read -r OS_PASSWORD; export OS_PASSWORD; openstack token issue >/dev/null' \
      2>&1
  ); then
    printf '%s\n' "$token_output" >&2
    case "$token_output" in
      *401*|*Unauthorized*|*'requires authentication'*)
        echo 'Keystone rejected the password. The Secret was not changed.' >&2
        ;;
      *NameResolutionError*|*'Failed to resolve'*|*'no such host'*|*'Connection refused'*|*ConnectTimeout*|*'Connection timed out'*|*'Max retries exceeded'*|*'Unable to establish connection'*)
        echo 'The token check could not reach Keystone. Check the endpoint and cluster networking; the Secret was not changed.' >&2
        ;;
      *)
        echo 'The token check failed for another reason. Review the error above; the Secret was not changed.' >&2
        ;;
    esac
    exit 1
  else
    printf '%s' "$PW" | kubectl -n brownfield create secret generic brownfield-admin-password \
    --from-file=password=/dev/stdin --dry-run=client -o yaml | kubectl -n brownfield replace -f -
  fi
  rotation_status=$?
  if [ "$rotation_status" -eq 0 ]; then
    echo 'Keystone accepted the password. The Secret was updated.'
  else
    echo 'The Secret update failed. Check the Secret and ControlPlane status before retrying.' >&2
    exit "$rotation_status"
  fi
  unset PW PW2
)
```
:::

The example runs `openstack token issue` in the cluster, where the fixture's
cluster-internal `authURL` resolves. It uses the same `ghcr.io/c5c3/tempest:2026.1`
image as the e2e verification Job. For a real installation, use the namespace,
endpoint, and admin identity from your ControlPlane in the command, and ensure
the pod can reach that endpoint.
The command pipes the password to the pod through standard input; it never
appears in command arguments.

Matching entries catch a typo entered differently twice. They do not detect the
same typo entered twice, an old password, or a password that was not yet changed
at the installation. Each value changes the hash and triggers replacement. If
Keystone rejects it, K-ORC has already revoked the old credential and cannot
create a working replacement. `KORCReady` reports `AuthenticationFailed`, and
imports and service-account projections that depend on the credential stop
working until the password is corrected. The in-pod token check prevents this
failure before the Secret changes.

If you rotate the password at the installation but do not update the Secret, the
old password stops authenticating. `KORCReady` reports `AuthenticationFailed`.
The operator does not discover the new password automatically.

To replace the credential without changing the admin password, create a
`CredentialRotation` with `reMint: true`. The field forces a fresh credential.
Without it, an unchanged password hash produces `Ready=True` with reason
`NoRotationNeeded`. The rotation request targets the ControlPlane in its own
namespace; the one-ControlPlane-per-namespace rule means it needs no reference
field:

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

The request acts once per spec generation. Leaving `reMint: true` in the spec
does not trigger another rotation during resync.

A service-account password rotates the same way, with
`target: serviceAccountPassword` and `keystoneService: <name>` naming the
`KeystoneService` registration in the `CredentialRotation` namespace. Set
`reMint: true` for this target. The reconciler has no password-hash detection for
service-account passwords.

The `CredentialRotation` field `spec.serviceAccount` was replaced by
`spec.keystoneService`, which names a `KeystoneService` CR. Because the resource
is `v1alpha1`, no conversion preserves the old value. Re-create a rotation
request that still has `serviceAccount`; otherwise it reports `Ready=False` with
reason `MissingKeystoneService`.

Never copy the generated application credential into another Secret, a config
file, or an environment variable. K-ORC deletes and revokes the previous
credential before creating its replacement, with no grace period. Consumers
must read the current value from the Kubernetes Secret or its OpenBao path when
they use it.

### 7. OpenBao paths in this mode

External mode does not seed a bootstrap path because the operator does not create
the admin password. The application-credential and service-account paths are
created on first push. The operator provisions the per-tenant OpenBao store, so
no separate preparation is needed for each ControlPlane.

The full per-mode path catalog is in
[OpenBao paths per ControlPlane mode](../../reference/infrastructure/openbao-bootstrap.md#openbao-paths-per-controlplane-mode).

### 8. Deletion

Deleting the ControlPlane removes resources it created or adopted:

```bash
kubectl -n brownfield delete controlplane controlplane-external
```

- K-ORC's finalizer revokes the admin application credential, and the
  OpenBao-backed Secrets are removed.
- Managed service-account users and projects are deleted from Keystone,
  including users marked `adopt: true`.
- Unmanaged imports remain: the admin user, the domain, the catalog
  services and endpoints, and any project referenced with `project.create: false`.
- Your Keystone keeps serving tokens throughout.

The `c5c3.io/orc-teardown` finalizer keeps the ControlPlane until K-ORC revokes
the credential against the reachable Keystone.

## See also

- [ControlPlane CRD reference](../../reference/c5c3/controlplane-crd.md#externalkeystonespec):
  `ExternalKeystoneSpec` and the full [status-condition catalog](../../reference/c5c3/controlplane-crd.md#status-conditions).
- [ControlPlane reconciler reference](../../reference/c5c3/controlplane-reconciler.md):
  what each sub-reconciler does in External mode.
- [OpenBao bootstrap reference](../../reference/infrastructure/openbao-bootstrap.md#openbao-paths-per-controlplane-mode):
  which paths exist per mode.
- [Brownfield Keystone Adoption](../../future/brownfield-keystone-adoption.md):
  the later phases (infrastructure attach, service takeover).

## Tested by

```bash
chainsaw test --test-dir tests/e2e/c5c3/external-keystone
```

or `make e2e-external-keystone`. The suite stands up the brownfield Keystone,
adopts it with an External-mode ControlPlane, authenticates an OpenStack client
with the generated credential, rotates it, and asserts the imports remain after
deletion.

The suite runs in namespace `brownfield` with the ControlPlane
`controlplane-external`, the names used in this walkthrough. This lets
[step 2](#_2-apply-the-external-mode-controlplane) imports the suite's own
manifest instead of keeping a second copy.
