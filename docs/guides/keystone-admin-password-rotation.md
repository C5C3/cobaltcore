---
title: Rotate the Keystone Admin Password
quadrant: operator
---

# Rotate the Keystone Admin Password

This guide walks an operator through rotating a Keystone instance's admin
password, verifying that the operator re-bootstraps the instance to apply the
new credential, and recovering when the admin Secret is missing or empty.

Unlike Fernet/credential-key rotation, the admin password has no rotation
CronJob: the value lives in OpenBao and is projected into the cluster by the
External Secrets Operator (ESO). The Keystone operator observes the resulting
Secret and re-runs `keystone-manage bootstrap` to update the admin credential
in place.

For the reconciler-side contract (when the bootstrap Job is recreated, the
event reasons, the failure path), see
[reconcileBootstrap](../reference/keystone/keystone-reconciler.md#reconcilebootstrap)
in [Keystone Reconciler Architecture](../reference/keystone/keystone-reconciler.md).

> **Terminology.** In this document `<ks>` is the Keystone CR's `.metadata.name`
> (e.g. `keystone-default`) and `<ns>` is its namespace (typically `openstack`).
> The admin password lives in the Secret referenced by
> `spec.bootstrap.adminPasswordSecretRef` under the key `password`; this guide
> calls it the *admin Secret* and refers to it as `<admin-secret>`.

---

## Background: How a Password Rotation Reaches Keystone

The admin password is not stored on the Keystone CR. It flows through three hops:

| Actor | Writes to | Reads from |
| --- | --- | --- |
| Operator/secrets-tooling | OpenBao path `kv-v2/bootstrap/<ns>/<ks>/admin` (key `password`) | — |
| External Secrets Operator (ESO) | The admin Secret `<admin-secret>` (key `password`, `creationPolicy: Owner`) | OpenBao path `bootstrap/<ns>/<ks>/admin`, property `password` |
| Keystone operator | The `{ks}-bootstrap` Job's pod template | The admin Secret `<admin-secret>` (key `password`) |

On every reconcile the operator reads the `password` key of the admin Secret,
computes `hex(SHA-256(password))`, and stamps it onto the bootstrap Job's pod
template as the `forge.c5c3.io/admin-password-hash` annotation. Because the
Job's `forge.c5c3.io/pod-spec-hash` gate hashes the *full* pod template, a
changed password changes that gate. `job.RunJob` then deletes the stale
`{ks}-bootstrap` Job and recreates it, re-running `keystone-manage bootstrap`,
which updates the admin credential to the new password.

The operator also watches the admin Secret directly (`secretToKeystoneMapper`),
so an ESO write triggers a reconcile with **no CR edit**. During the re-run
`BootstrapReady` transitions `False`/`BootstrapInProgress` → `True`/`BootstrapComplete`.

> **No rollout.** Re-bootstrap is a Job re-run, not a Deployment rollout. The
> running Keystone API pods are not restarted and their UIDs do not change; the
> bootstrap Job talks to the same database the API pods serve, so the new
> credential is live the moment the Job completes.

---

## 1. Write the new password to OpenBao

The admin password is sourced from OpenBao at `kv-v2/bootstrap/<ns>/<ks>/admin`
(key `password`). Write the new value there:

```bash
bao kv put kv-v2/bootstrap/<ns>/<ks>/admin password=<new-password>
```

> **Path convention (per-CR).** The admin-password path is
> scoped per Keystone CR as `bootstrap/<ns>/<ks>/admin`, so two
> Model-B-enabled Keystone CRs never collide on a shared OpenBao object. This is
> the path the ESO `keystone-admin` ExternalSecret reads
> (`remoteRef.key: bootstrap/<ns>/<ks>/admin`, `property: password`) and the path
> `deploy/openbao/bootstrap/write-bootstrap-secrets.sh` seeds — for the default
> Quick Start CR `controlplane-keystone` in `openstack`, that is
> `bootstrap/openstack/controlplane-keystone/admin`. If your deployment uses a
> different KV mount or path, substitute it here and in step 2's ExternalSecret
> name accordingly.

Nothing happens in the cluster yet — OpenBao now holds the new value, but the
admin Secret still carries the old one until ESO syncs.

---

## 2. (Optional) Force ESO to sync the new value

ESO refreshes on its `spec.refreshInterval` (the shipped ExternalSecret uses
`1h`). To apply the rotation immediately rather than waiting for the next
refresh, annotate the ExternalSecret to force a sync:

```bash
kubectl -n <ns> annotate externalsecret keystone-admin \
  force-sync=$(date +%s) --overwrite
```

ESO re-reads OpenBao and PATCHes the admin Secret's `password` key. Confirm the
Secret now carries the new value (compare the fingerprint before and after):

```bash
kubectl -n <ns> get secret <admin-secret> \
  -o jsonpath='{.data.password}' | base64 -d | sha256sum
```

This `sha256sum` is the same digest the operator stamps onto the bootstrap Job
in step 3 — keep it handy to confirm the match.

---

## 3. Observe the recreated bootstrap Job

The operator detects that the live `{ks}-bootstrap` Job's
`forge.c5c3.io/admin-password-hash` no longer matches the Secret, deletes the
stale Job, and recreates one carrying the new hash:

```bash
kubectl -n <ns> get jobs
```

Inspect the recreated Job's admin-password-hash annotation and confirm it equals
the digest from step 2:

```bash
kubectl -n <ns> get job <ks>-bootstrap \
  -o jsonpath="{.spec.template.metadata.annotations['forge\.c5c3\.io/admin-password-hash']}{\"\n\"}"
```

Expected output (the hex SHA-256 of the new password):

```
9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08
```

You can prove the Job was delete+recreated (not patched) by capturing its
`.metadata.uid` before and after the rotation — the recreated Job has a fresh
UID:

```bash
kubectl -n <ns> get job <ks>-bootstrap -o jsonpath='{.metadata.uid}{"\n"}'
```

> **TTL note.** The bootstrap Job carries `TTLSecondsAfterFinished: 300`. A
> completed Job is garbage-collected ~5 minutes after it finishes; the next
> reconcile recreates it with the same (current) hash. A momentary absence of
> the Job between GC and recreation is normal, not a failure.

---

## 4. Watch the `BootstrapReady` transitions

While the recreated Job runs, `BootstrapReady` drops to `False` with reason
`BootstrapInProgress`, then returns to `True` with reason `BootstrapComplete`:

```bash
kubectl -n <ns> describe keystone <ks> | grep -A4 'Conditions:'
```

Or watch just the condition's status and reason:

```bash
kubectl -n <ns> get keystone <ks> \
  -o jsonpath="{range .status.conditions[?(@.type=='BootstrapReady')]}{.status}/{.reason}{\"\n\"}{end}" -w
```

Expected progression:

```
False/BootstrapInProgress
True/BootstrapComplete
```

The CR's top-level `Ready` condition stays `True`/`AllReady` throughout — the
API never goes down during an admin-password rotation.

---

## 5. Observe the event stream

On a successful re-bootstrap the operator emits a Normal event on the Keystone CR:

```bash
kubectl -n <ns> get events \
  --field-selector reason=BootstrapComplete,involvedObject.name=<ks> \
  --sort-by='.lastTimestamp'
```

Expected output:

```
LAST SEEN   TYPE     REASON             OBJECT            MESSAGE
5s          Normal   BootstrapComplete  keystone/<ks>     Keystone bootstrap completed successfully
```

If instead you see a **Warning** with reason `AdminSecretInvalid`, the admin
Secret is missing, unreadable, or its `password` key is empty — see
[Recover from `AdminSecretInvalid`](#6-recover-from-adminsecretinvalid).

```bash
kubectl -n <ns> describe keystone <ks> | grep -A1 -E 'AdminSecretInvalid|BootstrapComplete'
```

---

## 6. Recover from `AdminSecretInvalid`

An admin password is a hard precondition for bootstrap: the operator will not
build a Job with empty credentials. If the admin Secret is missing/unreadable
or its `password` key is empty, the operator sets `BootstrapReady=False` with
reason `AdminSecretInvalid`, emits a Warning event, and requeues with backoff.

### Symptoms

```bash
kubectl -n <ns> get events \
  --field-selector reason=AdminSecretInvalid,involvedObject.name=<ks> \
  --sort-by='.lastTimestamp'
```

Example output:

```
LAST SEEN   TYPE      REASON              OBJECT            MESSAGE
12s         Warning   AdminSecretInvalid  keystone/<ks>     Admin password Secret <ns>/<admin-secret> is missing, unreadable, or has an empty "password" value
```

### Inspect

Confirm the admin Secret exists and that its `password` key decodes to a
non-empty value:

```bash
kubectl -n <ns> get secret <admin-secret> \
  -o jsonpath='{.data.password}' | base64 -d | wc -c
```

A result of `0` (or a `NotFound` error on the `get`) is the cause. The usual
culprit is ESO: check that the ExternalSecret synced cleanly.

```bash
kubectl -n <ns> get externalsecret keystone-admin \
  -o jsonpath="{range .status.conditions[*]}{.type}={.status}/{.reason}{\"\n\"}{end}"
```

### Remediate

1. Fix the source. Ensure the OpenBao path holds a non-empty `password`
   (`bao kv get kv-v2/bootstrap/<ns>/<ks>/admin`), then re-sync ESO as in step 2.
2. Once ESO repopulates the admin Secret, the operator's pending requeue (or a
   fresh `secretToKeystoneMapper` event from the Secret write) re-runs bootstrap
   automatically; `BootstrapReady` returns to `True`/`BootstrapComplete`. No CR
   edit is required.

> **Safety note.** While `BootstrapReady` is `False`, the previously bootstrapped
> admin credential remains valid in the database — the operator does not clear
> or invalidate it. The instance keeps serving with the last good password until
> a valid Secret lets the bootstrap Job run again.

---

## 7. Post-rotation smoke check

Confirm the new password authenticates and the old one no longer does. With the
new password exported as `OS_PASSWORD` (plus the usual `OS_AUTH_URL`,
`OS_USERNAME=admin`, `OS_USER_DOMAIN_NAME=Default`, `OS_PROJECT_NAME`):

```bash
openstack token issue
```

A `| id | ... |` table confirms the new credential is live.

The **old** password must now be rejected. Re-run with the previous value and
expect a `401`:

```bash
OS_PASSWORD=<old-password> openstack token issue
# The request was not authorized to perform this action. (HTTP 401)
```

A token minted before the rotation remains valid until its native TTL expires;
only new authentications with the old password are rejected.

---

## Related reference

- [reconcileBootstrap](../reference/keystone/keystone-reconciler.md#reconcilebootstrap) — the authoritative contract for the bootstrap sub-reconciler and the admin-password-hash re-run gate.
- [Labels and Annotations](../reference/keystone/keystone-reconciler.md#labels-and-annotations) — stable metadata keys, including `forge.c5c3.io/admin-password-hash` and `forge.c5c3.io/pod-spec-hash`.
- See also [Schedule Keystone Admin Password Rotation](keystone-admin-password-scheduled-rotation.md) — the Model B scheduled flow, where a CronJob mints the password instead of an operator writing OpenBao by hand.
- Chainsaw test: `tests/e2e/keystone/admin-password-rotation/chainsaw-test.yaml` asserts this guide's happy path end-to-end — re-bootstrap on Secret change, old-password `401` / new-password `201` cutover, and unchanged API pod UIDs.
