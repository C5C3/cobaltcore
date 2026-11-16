---
title: Rotate the Keystone Admin Password
quadrant: operator
---

# Rotate the Keystone Admin Password

This guide walks an operator through manually rotating a Keystone instance's
admin password, verifying that the operator re-bootstraps the instance to
apply the new credential, and recovering when the admin Secret is missing or
empty.

This manual flow does not use a rotation CronJob. The password lives in
OpenBao and is projected into the cluster by the External Secrets Operator
(ESO). When ESO updates the admin Secret, the Keystone operator re-runs
`keystone-manage bootstrap` to update the admin credential in place. For
automated rotation, see
[Schedule Keystone Admin Password Rotation](./keystone-admin-password-scheduled-rotation.md).

For the reconciler-side contract (when the bootstrap Job is recreated, the
event reasons, the failure path), see
[reconcileBootstrap](../../reference/keystone/keystone-reconciler.md#reconcilebootstrap)
in [Keystone Reconciler Architecture](../../reference/keystone/keystone-reconciler.md).

> **Names.** The examples target the ControlPlane devstack's projected Keystone:
> the CR is `controlplane-keystone` in the `openstack` namespace, and its admin
> password lives in the Secret referenced by
> `spec.bootstrap.adminPasswordSecretRef`: `controlplane-keystone-admin-credentials`,
> under the key `password`; this guide calls it the *admin Secret*. If your
> Keystone CR has a different name, substitute it (and the Secret and Job names
> derived from it) throughout. For a standalone Keystone with no ControlPlane,
> read the final section.

---

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the ControlPlane's
projected `controlplane-keystone` Keystone is `Ready`. Every resource name in the
examples below is one that devstack produces.
:::

- A bootstrapped Keystone CR (`BootstrapReady=True`): see [Observability & Diagnostics](../observability.md).
- The admin password projected via ESO: the `controlplane-keystone-admin-credentials`
  ExternalSecret is present and `Ready`. This flow requires ESO; a plain
  Secret without a ready ExternalSecret does not satisfy the operator's
  secret-readiness gate. Rotate by updating the OpenBao source.
- `bao` CLI access to the OpenBao KV mount and `openssl` for password
  fingerprints. In kind, OpenBao enforces mTLS, so the CLI needs a client
  certificate signed by the OpenBao CA. Without one, a connection reset is
  expected and does not indicate a pod defect.
- `kubectl` access to the CR's namespace (`openstack`).
- For step 7, the `openstack` CLI configured with the usual Keystone
  authentication environment variables.

---

## Background: How a Password Rotation Reaches Keystone

The admin password is not stored on the Keystone CR. It flows through three hops:

| Actor | Writes to | Reads from |
| --- | --- | --- |
| Password writer (operator or person) | OpenBao path `kv-v2/bootstrap/openstack/controlplane-keystone/admin` (key `password`) | — |
| External Secrets Operator (ESO) | The admin Secret `controlplane-keystone-admin-credentials` (key `password`, `creationPolicy: Owner`) | OpenBao path `bootstrap/openstack/controlplane-keystone/admin`, property `password` |
| Keystone operator | The `controlplane-keystone-bootstrap` Job's pod template | The admin Secret `controlplane-keystone-admin-credentials` (key `password`) |

The figure shows these hops as steps 3 to 6, which the
[scheduled-rotation guide](./keystone-admin-password-scheduled-rotation.md#_3-topology-what-the-operator-stands-up)
lists. A manual rotation enters at the "by hand" chip. On this devstack the
admin Secret is `controlplane-keystone-admin-credentials`.

![Rotation of the Keystone admin password in six numbered steps. On a standalone Keystone a CronJob generates a password and patches it onto a staging Secret, the keystone-operator validates it and commits it to a push-source Secret, and a PushSecret writes it to OpenBao. A person can instead write the password to the same OpenBao path by hand. From OpenBao an ExternalSecret updates the admin Secret, the keystone-operator sees the changed password hash and recreates the bootstrap Job, and keystone-manage bootstrap sets the new password in the Keystone database without restarting the API pods. A rejected password raises the event AdminPasswordRotationRejected and is not pushed.](../../diagrams/secrets-rotation-admin-password.svg)

On every reconcile the operator reads the `password` key of the admin Secret,
computes `hex(SHA-256(password))`, and stamps it onto the bootstrap Job's pod
template as the `cobaltcore.c5c3.io/admin-password-hash` annotation. It uses
the same digest as the bootstrap Job's **re-run key**. After initial bootstrap,
the re-run gate triggers when the admin password digest changes; an image-tag
change alone does not rerun bootstrap. Re-running
`keystone-manage bootstrap` after a cross-version DB migration fails on the
already-migrated admin user. When the password digest changes, the operator
deletes the stale `controlplane-keystone-bootstrap` Job and recreates it, re-running
`keystone-manage bootstrap`, which updates the admin credential to the new password.

The operator also watches the admin Secret directly, so an ESO write triggers
a reconcile with **no CR edit**. During the re-run
`BootstrapReady` transitions `False`/`BootstrapInProgress` → `True`/`BootstrapComplete`.

> **No rollout.** Re-bootstrap reruns a Job and leaves the Keystone API
> Deployment untouched. The running API pods keep their UIDs. The bootstrap
> Job updates the same database the API pods serve; allow a short propagation
> delay before checking authentication.

---

## 1. Write the new password to OpenBao

The admin password is sourced from OpenBao at
`kv-v2/bootstrap/openstack/controlplane-keystone/admin` (key `password`).
Before rotating, record the current bootstrap Job UID and password digest so
you can confirm both changed:

```bash
kubectl -n openstack get job controlplane-keystone-bootstrap \
  -o jsonpath='{.metadata.uid}{"\n"}'
kubectl -n openstack get secret controlplane-keystone-admin-credentials \
  -o jsonpath='{.data.password}' \
  | openssl base64 -d -A | openssl dgst -sha256
```

Write a different password to OpenBao. The prompt keeps it out of shell
history and command arguments:

```bash
printf 'new admin password: '
IFS= read -r -s NEW_PASSWORD
printf '\n'
if [ -z "$NEW_PASSWORD" ]; then
  echo "admin password must not be empty" >&2
  unset NEW_PASSWORD
  exit 1
fi
printf '%s' "$NEW_PASSWORD" \
  | bao kv put kv-v2/bootstrap/openstack/controlplane-keystone/admin password=-
unset NEW_PASSWORD
```

> **Path convention (per-CR).** The admin-password path is scoped per Keystone
> CR as `bootstrap/{namespace}/{name}/admin`. For the `controlplane-keystone`
> CR in `openstack`, that is `bootstrap/openstack/controlplane-keystone/admin`.
> Per-CR scoping prevents different Keystone CRs from colliding on a shared
> OpenBao object. This is the path the ESO
> `controlplane-keystone-admin-credentials` ExternalSecret reads
> (`remoteRef.key: bootstrap/openstack/controlplane-keystone/admin`,
> `property: password`) and the path
> `deploy/bases/shared-openbao/bootstrap/write-bootstrap-secrets.sh` seeds. If your deployment
> uses a different KV mount or path, update the `bao` command and ensure the
> ExternalSecret's `remoteRef.key` points to it. If that ExternalSecret has a
> different name, substitute its name in step 2.

The cluster does not change immediately. OpenBao holds the new value, and the
admin Secret keeps the old value until ESO syncs.

---

## 2. (Optional) Force ESO to sync the new value

ESO refreshes on its `spec.refreshInterval` (the shipped ExternalSecret uses
`1h`). To apply the rotation immediately, annotate the ExternalSecret to force a
sync:

```bash
kubectl -n openstack annotate externalsecret controlplane-keystone-admin-credentials \
  force-sync=$(date +%s) --overwrite
```

ESO re-reads OpenBao and PATCHes the admin Secret's `password` key. Confirm the
Secret now carries the new value:

```bash
kubectl -n openstack get secret controlplane-keystone-admin-credentials \
  -o jsonpath='{.data.password}' \
  | openssl base64 -d -A | openssl dgst -sha256
```

Compare the digest after `=` with the value recorded before rotation. It must
differ if the password changed. Keep the new digest for step 3. If it still
matches the old value, wait for ESO's refresh or force a sync again.

---

## 3. Observe the recreated bootstrap Job

The operator detects that the live `controlplane-keystone-bootstrap` Job's
`cobaltcore.c5c3.io/admin-password-hash` no longer matches the Secret, deletes the
stale Job, and recreates one carrying the new hash:

```bash
kubectl -n openstack get jobs
```

Inspect the recreated Job's admin-password-hash annotation and confirm it equals
the digest from step 2:

```bash
kubectl -n openstack get job controlplane-keystone-bootstrap \
  -o jsonpath="{.spec.template.metadata.annotations['cobaltcore\.c5c3\.io/admin-password-hash']}{\"\n\"}"
```

Reconciliation is asynchronous. If the Job is briefly absent or the hash still
shows the old value, repeat the queries after a few seconds. The new annotation
must equal the step 2 digest. It is a 64-character lowercase hexadecimal
SHA-256 value. If it has not changed after three minutes, inspect the
ExternalSecret status and bootstrap Job events before continuing.

The replacement Job must also have a different UID from the one recorded
before the rotation, which confirms the operator deleted and recreated it:

```bash
kubectl -n openstack get job controlplane-keystone-bootstrap -o jsonpath='{.metadata.uid}{"\n"}'
```

> **Job retention.** The bootstrap Job has no `TTLSecondsAfterFinished` setting.
> The completed Job is the operator's record of the
> applied password digest (its re-run key), so it is retained, not
> garbage-collected, and is removed only when the Keystone CR is deleted via
> owner-reference GC. The steady state is therefore a single completed
> `controlplane-keystone-bootstrap` Job present at all times. A rotation deletes
> the completed Job and creates its replacement.

---

## 4. Watch the `BootstrapReady` transitions

While the recreated Job runs, `BootstrapReady` drops to `False` with reason
`BootstrapInProgress`, then returns to `True` with reason `BootstrapComplete`:

```bash
kubectl -n openstack describe keystone controlplane-keystone | grep -A4 'Conditions:'
```

Or watch just the condition's status and reason:

```bash
kubectl -n openstack get keystone controlplane-keystone \
  -o jsonpath="{range .status.conditions[?(@.type=='BootstrapReady')]}{.status}/{.reason}{\"\n\"}{end}" -w
```

Expected progression:

```
False/BootstrapInProgress
True/BootstrapComplete
```

While `BootstrapReady` is false, the aggregate `Ready` condition also becomes
`False` with reason `NotAllReady`; it returns to `True`/`AllReady` when
bootstrap completes. The operator does not restart the Keystone API
Deployment.

---

## 5. Observe the event stream

On a successful re-bootstrap the operator emits a Normal event on the Keystone CR:

```bash
kubectl -n openstack get events \
  --field-selector reason=BootstrapComplete,involvedObject.name=controlplane-keystone \
  --sort-by='.lastTimestamp'
```

Expected output:

```
LAST SEEN   TYPE     REASON             OBJECT            MESSAGE
5s          Normal   BootstrapComplete  keystone/controlplane-keystone     Keystone bootstrap completed successfully
```

If you see a **Warning** with reason `AdminSecretInvalid`, the admin
Secret is missing, unreadable, or its `password` key is empty. See
[Recover from `AdminSecretInvalid`](#_6-recover-from-adminsecretinvalid).

```bash
kubectl -n openstack describe keystone controlplane-keystone | grep -A1 -E 'AdminSecretInvalid|BootstrapComplete'
```

---

## 6. Recover from `AdminSecretInvalid`

An admin password is a hard precondition for bootstrap: the operator will not
build a Job with empty credentials. If the admin Secret is missing/unreadable
or its `password` key is empty, the operator sets `BootstrapReady=False` with
reason `AdminSecretInvalid`, emits a Warning event, and requeues with backoff.

### Symptoms

```bash
kubectl -n openstack get events \
  --field-selector reason=AdminSecretInvalid,involvedObject.name=controlplane-keystone \
  --sort-by='.lastTimestamp'
```

Example output:

```
LAST SEEN   TYPE      REASON              OBJECT            MESSAGE
12s         Warning   AdminSecretInvalid  keystone/controlplane-keystone     Admin password Secret openstack/controlplane-keystone-admin-credentials is missing, unreadable, or has an empty "password" value
```

### Inspect

Confirm the admin Secret exists and that its `password` key decodes to a
non-empty value:

```bash
kubectl -n openstack get secret controlplane-keystone-admin-credentials \
  -o jsonpath='{.data.password}' \
  | openssl base64 -d -A | wc -c
```

A result of `0` (or a `NotFound` error on the `get`) is the cause. The usual
culprit is ESO: check that the ExternalSecret synced cleanly.

```bash
kubectl -n openstack get externalsecret controlplane-keystone-admin-credentials \
  -o jsonpath="{range .status.conditions[*]}{.type}={.status}/{.reason}{\"\n\"}{end}"
```

### Remediate

1. Fix the source in OpenBao so this path has a non-empty `password` property.
   Re-sync ESO as in step 2.
2. Once ESO repopulates the admin Secret, the operator's pending requeue or
   Secret watch event re-runs bootstrap automatically. `BootstrapReady`
   returns to `True`/`BootstrapComplete`, and no CR edit is required.

> **Safety note.** While the reason is `AdminSecretInvalid`, the operator does
> not run the bootstrap Job or clear the credential. The last valid admin
> password remains active until a valid Secret lets bootstrap run again.

---

## 7. Post-rotation smoke check

Confirm the new password authenticates and the old one no longer does. With the
new password exported as `OS_PASSWORD` (plus the usual `OS_AUTH_URL`,
`OS_USERNAME=admin`, `OS_USER_DOMAIN_NAME=Default`, `OS_PROJECT_NAME`):

```bash
openstack token issue -f value -c expires
```

An expiry timestamp confirms the new credential authenticated without printing
the bearer token.

The **old** password must now be rejected. Enter it at the prompt and expect
a `401`:

```bash
printf 'old admin password: '
IFS= read -r -s OLD_OS_PASSWORD
printf '\n'
OS_PASSWORD="$OLD_OS_PASSWORD" openstack token issue -f value -c expires
unset OLD_OS_PASSWORD
# The request was not authorized to perform this action. (HTTP 401)
```

This check covers new authentication requests. It does not test the lifetime
or revocation behavior of tokens issued before the password change.

---

## Standalone Keystone, without a ControlPlane

The [Quick Start](../../quick-start.md) and
[Quick Start (Extended)](../../quick-start-extended.md) devstacks run a standalone
Keystone CR that is not projected by a ControlPlane. There the names are:

| ControlPlane devstack | Standalone devstack |
| --- | --- |
| CR `controlplane-keystone` | CR `keystone` |
| admin Secret `controlplane-keystone-admin-credentials` | admin Secret `keystone-admin` |
| ExternalSecret `controlplane-keystone-admin-credentials` | ExternalSecret `keystone-admin` |
| bootstrap Job `controlplane-keystone-bootstrap` | bootstrap Job `keystone-bootstrap` |

Substitute those names in every command above. The re-bootstrap contract,
condition transitions, and smoke check are identical.

::: warning The kind shim reads the default-identity path
On the kind Quick Start, the `keystone-admin` ExternalSecret is a standalone
shim (`deploy/bases/devstack/infrastructure/keystone-admin-externalsecret.yaml`). It reads
the default ControlPlane identity's per-CR path,
`bootstrap/openstack/controlplane-keystone/admin`. Its path does not change with
the standalone CR's name. The step 1 command already uses this path; keep it
unchanged when substituting the standalone resource names.

On a non-kind standalone deployment you own the ExternalSecret's `remoteRef.key`,
so write to whatever path it reads.
:::

---

## Related reference

- [reconcileBootstrap](../../reference/keystone/keystone-reconciler.md#reconcilebootstrap): the authoritative contract for the bootstrap sub-reconciler and the admin-password-hash re-run gate.
- [Labels and Annotations](../../reference/keystone/keystone-reconciler.md#labels-and-annotations): stable metadata keys, including `cobaltcore.c5c3.io/admin-password-hash` and `cobaltcore.c5c3.io/pod-spec-hash`.
- See also [Schedule Keystone Admin Password Rotation](keystone-admin-password-scheduled-rotation.md): the Model B scheduled flow, where a CronJob creates the password and writes it to OpenBao.
- See also [Rotate Keystone Fernet and Credential Keys](keystone-key-rotation.md): the key-rotation counterpart to this admin-password rotation.

## Tested by

This guide's happy path is asserted end-to-end on the CI e2e kind cluster. The
suite verifies re-bootstrap on Secret change, old-password `401` and
new-password `201` cutover, and unchanged API pod UIDs:

```bash
chainsaw test --test-dir tests/e2e/keystone/admin-password-rotation
```
