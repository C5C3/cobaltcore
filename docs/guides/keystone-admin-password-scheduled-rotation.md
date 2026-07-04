---
title: Schedule Keystone Admin Password Rotation
quadrant: operator
---

# Schedule Keystone Admin Password Rotation

This guide walks an operator through enabling *scheduled* rotation of a Keystone
instance's admin password ("Model B"), understanding the resource chain
it stands up, and proving end-to-end that a rotation reached the running Keystone.

This is a companion to
[Rotate the Keystone Admin Password](keystone-admin-password-rotation.md), which
covers the **manual** flow. The difference in one line:

- **Manual.** *You* write the new password into OpenBao by hand; ESO
  projects it into the admin Secret and the operator re-bootstraps.
- **Scheduled / Model B.** A CronJob *mints* a fresh password on a
  schedule, the operator validates it and pushes it to OpenBao, and then the same
  re-bootstrap path applies it. You configure the schedule once and the operator
  does the minting.

Both flows converge on the same final hop — `reconcileBootstrap` re-runs
`keystone-manage bootstrap` against the new credential. This guide therefore
*cross-links* the manual guide's verification steps rather than repeating them.

> **Terminology.** In this document `<ks>` is the Keystone CR's `.metadata.name`
> (e.g. `keystone-default`) and `<ns>` is its namespace (typically `openstack`).
> The admin password lives in the Secret referenced by
> `spec.bootstrap.adminPasswordSecretRef` under the key `password`; this guide
> calls it the *admin Secret* and refers to it as `<admin-secret>`. The
> Model B resources are named after `<ks>` (e.g. the CronJob is
> `<ks>-admin-password-rotate`).

---

## Prerequisites

- A bootstrapped Keystone CR (`BootstrapReady=True`) with the **manual** admin-password
  flow already working — scheduled rotation reuses the same ESO/OpenBao path and final
  re-bootstrap hop. See [Rotate the Keystone Admin Password](keystone-admin-password-rotation.md).
- The per-CR OpenBao path `bootstrap/<ns>/<ks>/admin` already seeded **and** stamped with
  `custom_metadata managed-by=external-secrets`; without that marker ESO refuses the very
  first PushSecret. `deploy/openbao/bootstrap/write-bootstrap-secrets.sh` stamps it for the
  default CR (see [Topology](#2-topology-what-the-operator-stands-up) below).
- `kubectl` access to the CR's namespace (`<ns>`).

---

## 1. Enable scheduled rotation

Scheduled rotation is opt-in. Add a `spec.passwordRotation` block to the
Keystone CR:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: <ks>
  namespace: <ns>
spec:
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: <admin-secret>
  passwordRotation:
    enabled: true
    schedule: "0 0 1 * *"   # monthly, midnight on the 1st (default)
    suspend: false          # default
    passwordLength: 32       # default; minimum 24
```

The four fields of `passwordRotation`:

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `enabled` | bool | `false` | Turns the feature on. `false` (or a nil block) tears down every Model B resource. |
| `schedule` | cron string | `"0 0 1 * *"` | When the CronJob mints a new password. |
| `suspend` | bool | `false` | Pauses the CronJob without deleting any resource. |
| `passwordLength` | int | `32` (minimum `24`) | Length of the generated password. |

> **Note.** The defaulting webhook only fills `schedule` and `passwordLength`
> *once `enabled: true`*. A **nil** `passwordRotation` block is
> never materialized — upgrading a CR that never set it must not silently enable
> rotation. The leaf fields above are shown explicitly for clarity; you can omit
> `schedule`, `suspend`, and `passwordLength` and let the webhook default them.

---

## 2. Topology: what the operator stands up

When `enabled: true`, the `reconcilePasswordRotation` sub-reconciler ensures a
chain of resources. A rotation flows left to right:

```
CronJob <ks>-admin-password-rotate
  │  (mounts only /scripts/admin_password_rotate.sh; never runs keystone-manage)
  │  PATCH password + forge.c5c3.io/rotation-completed-at
  ▼
staging Secret <ks>-admin-password-rotation
  │  operator validates (non-empty, >= min length) and COMMITS
  ▼
push-source Secret <ks>-admin-password-next   (operator-owned)
  │  PushSecret <ks>-admin-password-backup mirrors it
  ▼
OpenBao  bootstrap/<ns>/<ks>/admin   (per-CR path)
  │  ESO keystone-admin ExternalSecret syncs it
  ▼
admin Secret <admin-secret>
  │  secretToKeystoneMapper triggers a reconcile
  ▼
reconcileBootstrap re-runs `keystone-manage bootstrap`  →  credential cut over
```

The resources, by name:

| Resource | Name | Role |
| --- | --- | --- |
| CronJob | `<ks>-admin-password-rotate` | Mints a fresh password on `schedule` and PATCHes it onto the staging Secret. Mounts only the rotation script; never runs `keystone-manage`. |
| Staging Secret | `<ks>-admin-password-rotation` | Drop box the CronJob writes; the only Secret the CronJob SA may patch. |
| Push-source Secret | `<ks>-admin-password-next` | Operator-owned. The operator commits the validated password here; the PushSecret selects it. |
| PushSecret | `<ks>-admin-password-backup` | Mirrors the push-source Secret to OpenBao `bootstrap/<ns>/<ks>/admin` (per-CR path). |
| RBAC trio | `<ks>-admin-password-rotate` (ServiceAccount, Role, RoleBinding) | The CronJob's split-RBAC identity. |
| Script ConfigMap | `<ks>-admin-password-rotate-script` (content-hash suffixed) | Immutable mount of `admin_password_rotate.sh`. |

Two safety properties are worth calling out:

> **Split RBAC.** The CronJob's Role grants `get` + `patch` on **only** the
> staging Secret `<ks>-admin-password-rotation`, and `get` (read-only) on the
> push-source Secret. The CronJob can never write the push-source Secret — *the
> operator* validates the staged value and writes the push-source. Write access to
> the Secret a PushSecret backs is a token-forgery primitive, and it is kept off
> the CronJob's attack surface.

> **Clobber-safe push.** The PushSecret is only created once the push-source
> Secret actually holds a valid password (non-empty, at least the minimum length).
> Before the first rotation completes the push-source Secret is empty, so the
> operator does not push — it would otherwise overwrite the seeded
> `bootstrap/<ns>/<ks>/admin` value with nothing.

> **ESO-managed source path.** The PushSecret can only write
> `bootstrap/<ns>/<ks>/admin` if that OpenBao path carries the custom-metadata
> marker `managed-by=external-secrets`. The ESO Vault/OpenBao provider refuses to
> overwrite a path it does not own and fails the PushSecret with `secret not
> managed by external-secrets`. The standard bootstrap
> (`deploy/openbao/bootstrap/write-bootstrap-secrets.sh`) stamps the marker when it
> seeds the path, and re-running it adopts a path written by an older bootstrap
> that predates this marker. If you seed `bootstrap/<ns>/<ks>/admin` by hand, set
> the marker too:
> `bao kv metadata put -custom-metadata=managed-by=external-secrets kv-v2/bootstrap/<ns>/<ks>/admin`.

For the full sub-reconciler contract (validation rules, event reasons, the
clobber-safe gate, RBAC shape) see
[`reconcilePasswordRotation`](../reference/keystone/keystone-reconciler.md#reconcilepasswordrotation).

---

## 3. Prove a scheduled rotation end-to-end

The monthly default schedule rarely fires when you want to observe it, so trigger
a run on demand and follow the evidence chain. Each step below is something you
can run and confirm.

### 3.1 Confirm the CronJob exists with your schedule

```bash
kubectl -n <ns> get cronjob <ks>-admin-password-rotate
```

The `SCHEDULE` column should show your `spec.passwordRotation.schedule`
(e.g. `0 0 1 * *`).

### 3.2 Trigger a run on demand

Instantiate a one-shot Job from the CronJob template and wait for it to finish:

```bash
JOB=<ks>-admin-password-rotate-manual-$(date +%s)
kubectl -n <ns> create job --from=cronjob/<ks>-admin-password-rotate "$JOB"

kubectl -n <ns> wait --for=condition=complete job/"$JOB" --timeout=120s
```

The timestamp suffix keeps the Job name unique, so re-running this step never
collides with a prior manual Job (`AlreadyExists`) before it is cleaned up.

The Job's pod mints a fresh password and PATCHes it onto the staging Secret
`<ks>-admin-password-rotation`.

### 3.3 Observe the operator commit

On the next reconcile the operator validates the staged password, copies it onto
the push-source Secret, deletes staging, and emits a Normal event. Confirm all
three:

```bash
# Push-source Secret now carries a non-empty password and the completion stamp.
kubectl -n <ns> get secret <ks>-admin-password-next \
  -o jsonpath="{.metadata.annotations['forge\.c5c3\.io/rotation-completed-at']}{\"\n\"}"

# Staging Secret has been deleted (NotFound is the success signal here).
kubectl -n <ns> get secret <ks>-admin-password-rotation

# The success event.
kubectl -n <ns> get events \
  --field-selector reason=AdminPasswordRotated,involvedObject.name=<ks> \
  --sort-by='.lastTimestamp'
```

Expected event:

```
LAST SEEN   TYPE     REASON               OBJECT          MESSAGE
5s          Normal   AdminPasswordRotated keystone/<ks>   admin password rotation applied from staging secret <ks>-admin-password-rotation
```

> **Note.** The password value itself is never logged or echoed in an event. If
> the staged value is rejected you will instead see a Warning with reason
> `AdminPasswordRotationRejected` (empty or too short) or
> `AdminPasswordRotationAnnotationInvalid` (malformed completion timestamp), and
> the staging Secret is retained for inspection.

### 3.4 Confirm the OpenBao value changed and ESO synced it

The PushSecret mirrors the push-source Secret into OpenBao at
`bootstrap/<ns>/<ks>/admin`, and the `keystone-admin` ExternalSecret projects it
back into the admin Secret `<admin-secret>`. Use the manual guide's
force-sync + fingerprint technique to confirm the admin Secret carries the new
value: see
[step 2 of the manual guide](keystone-admin-password-rotation.md#2-optional-force-eso-to-sync-the-new-value).

The short version:

```bash
kubectl -n <ns> get secret <admin-secret> \
  -o jsonpath='{.data.password}' | base64 -d | sha256sum
```

This digest is the same one the operator stamps onto the recreated bootstrap Job
in the next step.

### 3.5 Watch the re-bootstrap (cross-link to the manual guide)

From here the flow is *identical* to a manual rotation — the operator's
`secretToKeystoneMapper` observes the admin Secret change and re-runs the
idempotent bootstrap Job. Follow the manual guide's steps, which are the
authoritative walkthrough:

- [Step 3 — Observe the recreated bootstrap Job](keystone-admin-password-rotation.md#3-observe-the-recreated-bootstrap-job)
  (the `<ks>-bootstrap` Job is delete+recreated with a fresh UID and the new
  `forge.c5c3.io/admin-password-hash`).
- [Step 4 — Watch the `BootstrapReady` transitions](keystone-admin-password-rotation.md#4-watch-the-bootstrapready-transitions)
  (`False`/`BootstrapInProgress` → `True`/`BootstrapComplete`).
- [Step 5 — Observe the event stream](keystone-admin-password-rotation.md#5-observe-the-event-stream).
- [Step 6 — Recover from `AdminSecretInvalid`](keystone-admin-password-rotation.md#6-recover-from-adminsecretinvalid)
  if the synced value is empty.
- [Step 7 — Post-rotation smoke check](keystone-admin-password-rotation.md#7-post-rotation-smoke-check)
  (the new password authenticates `201`; the old one is rejected `401`).

### 3.6 Note the separation from the live credential

The push-source Secret `<ks>-admin-password-next` is a **distinct object** from
the live admin Secret `<admin-secret>`. The operator commits the new password onto
the push-source Secret; the running Keystone keeps using the value in
`<admin-secret>` until ESO has synced the new value back. A scheduled rotation
never clobbers the credential the running Keystone is using mid-flight.

---

## 4. Per-CR path isolation

Model B scopes the OpenBao RemoteKey **per Keystone CR** to
`bootstrap/<ns>/<ks>/admin`, where `<ns>`/`<ks>` are the
Keystone CR's namespace and name. Each Model-B-enabled Keystone CR therefore
writes its admin password to its **own** OpenBao object, and the matching
`keystone-admin` ExternalSecret reads that same per-CR path. Two Model-B-enabled
Keystone CRs in the same cluster no longer share one OpenBao object — they cannot
clobber each other's rotations, so scheduled rotation can be enabled on multiple
Keystone CRs concurrently.

> **Path in lockstep.** The `keystone-admin` ExternalSecret's `remoteRef.key` must
> match the per-CR path of the Keystone CR whose rotation feeds it. The bootstrap
> seed (`deploy/openbao/bootstrap/write-bootstrap-secrets.sh`) seeds
> `bootstrap/<ns>/<ks>/admin` for each ControlPlane identity in
> `KORC_CONTROLPLANES` (default `openstack/controlplane`, whose projected Keystone
> CR is `controlplane-keystone`, i.e. `bootstrap/openstack/controlplane-keystone/admin`).

---

## 5. Suspend or disable rotation

There are two ways to stop rotating, with different blast radius.

**Suspend** — keep every resource but stop new runs:

```bash
kubectl -n <ns> patch keystone <ks> --type merge \
  -p '{"spec":{"passwordRotation":{"suspend":true}}}'
```

This sets the CronJob's `spec.suspend: true`; the CronJob and all sibling
resources remain. No new password is minted until you set `suspend: false` again.

**Disable** — tear everything down:

```bash
kubectl -n <ns> patch keystone <ks> --type merge \
  -p '{"spec":{"passwordRotation":{"enabled":false}}}'
```

Setting `enabled: false` (or removing the `passwordRotation` block entirely)
deletes **all** Model B resources — the CronJob, the staging and push-source
Secrets, the RBAC trio, the PushSecret, and the script ConfigMap — and reports
`PasswordRotationReady=True` with reason `RotationDisabled`.

> **Safety note.** Disabling does **not** remove the last-pushed password from
> OpenBao. The PushSecret uses `DeletionPolicy: None`, so the value already in
> `bootstrap/<ns>/<ks>/admin` stays put when the PushSecret is deleted. Disabling
> rotation can never lock the admin out of Keystone.

---

## Related reference

- [`reconcilePasswordRotation`](../reference/keystone/keystone-reconciler.md#reconcilepasswordrotation) — the authoritative contract for the Model B sub-reconciler: validation rules, event reasons, the clobber-safe PushSecret gate, and the split RBAC.
- [`reconcileBootstrap`](../reference/keystone/keystone-reconciler.md#reconcilebootstrap) — the bootstrap sub-reconciler and the `admin-password-hash` re-run gate that applies the rotated credential.
- [Rotate the Keystone Admin Password](keystone-admin-password-rotation.md) — the manual rotation guide whose verification steps (3–7) this guide cross-links.
- [Rotate Keystone Fernet and Credential Keys](keystone-key-rotation.md) — the key-rotation counterpart, which uses an analogous staging→production split.
- Chainsaw test: `tests/e2e/keystone/admin-password-scheduled-rotation/chainsaw-test.yaml` asserts this guide's evidence chain end-to-end — CronJob run → push-source commit → OpenBao change → ESO sync → re-bootstrap `BootstrapReady=True` → new-password `201` / old-password `401`, and the disable→teardown `RotationDisabled` posture.
