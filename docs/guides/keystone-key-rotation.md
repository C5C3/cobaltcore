---
title: Rotate Keystone Fernet and Credential Keys
quadrant: operator
feature: CC-0073, CC-0081
---

# Rotate Keystone Fernet and Credential Keys

This guide walks an operator through triggering a manual rotation of a
Keystone instance's Fernet or credential-encryption keys, verifying each
stage of the split-compute-write rotation pipeline introduced by CC-0081,
and recovering when the operator rejects a staged rotation.

For the reconciler-side contract (RBAC split, staging-Secret naming,
validation rules, event reasons), see
[Key Rotation RBAC Split](../reference/keystone/keystone-reconciler.md#key-rotation-rbac-split-cc-0081)
under the Fernet and credential sub-reconciler sections in
[Keystone Reconciler Architecture](../reference/keystone/keystone-reconciler.md).

> **Terminology.** In this document `<ks>` is the Keystone CR's `.metadata.name`
> (e.g. `keystone-default`) and `<ns>` is its namespace (typically `openstack`).
> Commands target Fernet rotation; swap `fernet` → `credential` everywhere for
> credential-key rotation.

---

## Background: Who Writes What

Before CC-0081 the rotation CronJob wrote directly to the production
`{ks}-fernet-keys` Secret with `patch` RBAC. CC-0081 split that path:

| Actor | Writes to | Reads from |
| --- | --- | --- |
| Rotation CronJob (ServiceAccount `{ks}-fernet-rotate`) | Staging Secret `{ks}-fernet-keys-rotation` (via `patch`) | Production Secret `{ks}-fernet-keys` (via `get`, mounted as volume) |
| Operator (controller-manager ServiceAccount) | Production Secret `{ks}-fernet-keys` (via `patch`) | Staging Secret `{ks}-fernet-keys-rotation` (validates, then deletes) |

The staging Secret carries one controller-observable marker — the
`forge.c5c3.io/rotation-completed-at` annotation — that tells the operator
"the CronJob finished; please apply". Until that annotation is present
and parseable as RFC3339 UTC, the operator will not touch the production
Secret.

---

## 1. Trigger a manual rotation

Rotations run on the `spec.fernet.rotationSchedule` / `spec.credentialKeys.rotationSchedule`
cron schedule by default. To trigger one on demand, create a one-shot Job
from the CronJob template:

```bash
kubectl -n <ns> create job --from=cronjob/<ks>-fernet-rotate \
  <ks>-fernet-rotate-manual-$(date +%s)
```

Watch the Job run to completion:

```bash
kubectl -n <ns> get jobs -l job-name -w
kubectl -n <ns> logs job/<ks>-fernet-rotate-manual-<ts>
```

Expected log tail:

```
Fernet keys staging Secret updated successfully
```

At this point the CronJob has PATCHed the staging Secret with both the new
`data` map and the completion annotation. The operator has not yet acted.

---

## 2. Verify the staging Secret's completion annotation

The `forge.c5c3.io/rotation-completed-at` annotation is transient — it
appears on the staging Secret only between the CronJob's PATCH and the
operator's next reconcile, which typically closes the window in seconds.
To catch it, watch the staging Secret during a rotation:

```bash
kubectl -n <ns> get secret <ks>-fernet-keys-rotation \
  -o jsonpath='{.metadata.annotations.forge\.c5c3\.io/rotation-completed-at}{"\n"}'
```

A successful rotation shows the annotation briefly before the Secret is
deleted:

```
2026-04-18T12:34:56Z
```

After the operator applies the rotation, the staging Secret is deleted
entirely:

```bash
$ kubectl -n <ns> get secret <ks>-fernet-keys-rotation
Error from server (NotFound): secrets "<ks>-fernet-keys-rotation" not found
```

The operator recreates the empty staging Secret on the next reconcile —
see `ensureFernetStagingSecret`. If you see the staging Secret exist with
empty `Data`, that is the steady state between rotations.

---

## 3. Verify the operator applied the rotation (event on CR)

On a successful apply the operator emits a Normal event on the Keystone CR:

```bash
kubectl -n <ns> describe keystone <ks> | grep -A1 -E 'FernetKeysRotated|CredentialKeysRotated'
```

Expected output:

```
Normal  FernetKeysRotated  5s  keystone-controller  rotation applied from staging secret <ks>-fernet-keys-rotation (3 active keys)
```

Alternatively, filter the namespace's event stream:

```bash
kubectl -n <ns> get events \
  --field-selector reason=FernetKeysRotated,involvedObject.name=<ks> \
  --sort-by='.lastTimestamp'
```

---

## 4. Confirm the production Secret data changed

Capture the production Secret's `resourceVersion` and key fingerprints
before and after to prove the apply went through:

```bash
# Before triggering rotation
kubectl -n <ns> get secret <ks>-fernet-keys \
  -o jsonpath='{.metadata.resourceVersion}{"\n"}'
kubectl -n <ns> get secret <ks>-fernet-keys \
  -o jsonpath='{range .data.*}{@}{"\n"}{end}' | sha256sum

# After: resourceVersion has advanced and the hash differs
kubectl -n <ns> get secret <ks>-fernet-keys \
  -o jsonpath='{.metadata.resourceVersion}{"\n"}'
kubectl -n <ns> get secret <ks>-fernet-keys \
  -o jsonpath='{range .data.*}{@}{"\n"}{end}' | sha256sum
```

Thanks to the kubelet's in-place Secret projection (CC-0074), running
Keystone pods pick up the new keys on their next projection refresh
(~60 seconds) without a Deployment rollout. A token minted before the
rotation remains valid until its native TTL expires, because the old key
is retained in the Secret's active window until it ages out over
subsequent rotations.

---

## 5. Recover from `RotationRejected`

The operator validates every staged rotation before copying it onto the
production Secret. On failure it emits a Warning event and **keeps the
staging Secret in place** so you can inspect what the CronJob produced.

### Symptoms

```bash
kubectl -n <ns> get events \
  --field-selector reason=RotationRejected,involvedObject.name=<ks> \
  --sort-by='.lastTimestamp'
```

Example output:

```
LAST SEEN   TYPE      REASON              OBJECT                       MESSAGE
12s         Warning   RotationRejected    keystone/<ks>                staging secret <ks>-fernet-keys-rotation rejected: invalid key format: key "0" length=32, want 44
```

The companion Warning reason `RotationAnnotationInvalid` indicates the
annotation was present but did not parse as RFC3339; the remediation path
is the same.

### Inspect the staging Secret

```bash
kubectl -n <ns> get secret <ks>-fernet-keys-rotation -o yaml
```

Match the event message against the operator's validation contract
(see [Operator validation rules](../reference/keystone/keystone-reconciler.md#key-rotation-rbac-split-cc-0081)):

| Event message contains | Likely cause |
| --- | --- |
| `invalid key format: key "…" length=<n>, want 44` | CronJob wrote keys that are not the 44-byte base64url shape `generateFernetKey` produces. Check `keystone-manage fernet_rotate` output and the rotation script's `base64.b64encode` step. |
| `invalid key format: key "…" base64 decode: …` | Key value is not valid URL-safe base64. Likely the script wrote raw bytes without encoding. |
| `invalid key format: key "…" decoded length=<n>, want 32` | Keys decoded but were not 32 bytes. The `keystone-manage` key size was misconfigured. |
| `duplicate keys detected: keys "i" and "j"` | Two indices in the staged payload have identical bytes. Usually a script bug that copied the same file twice. |
| `key count out of range: got <n>, want [3, <max+1>]` | The CronJob wrote fewer than 3 keys or more than `spec.fernet.maxActiveKeys + 1`. Check `spec.fernet.maxActiveKeys` on the Keystone CR. |

### Remediate

The recovery sequence is always:

1. Fix the cause (CronJob image, script, or CR spec) so the next rotation
   will produce valid output.
2. Force a fresh rotation:

   ```bash
   kubectl -n <ns> delete secret <ks>-fernet-keys-rotation
   kubectl -n <ns> create job --from=cronjob/<ks>-fernet-rotate \
     <ks>-fernet-rotate-recover-$(date +%s)
   ```

   The operator re-creates an empty staging Secret on the next reconcile;
   the new Job PATCHes it; the operator validates and applies.
3. Confirm apply by repeating steps 2-4 above.

> **Production safety note.** The production Secret is never modified
> during a rejected rotation — that is the whole point of the CC-0081
> split. You can inspect a `RotationRejected` state as long as you like
> without impacting running Keystone pods; they continue to serve tokens
> with the previous key set.

---

## Credential-key specifics

Everything above applies to credential rotation unchanged — substitute:

| Fernet | Credential |
| --- | --- |
| `<ks>-fernet-keys` | `<ks>-credential-keys` |
| `<ks>-fernet-keys-rotation` | `<ks>-credential-keys-rotation` |
| `<ks>-fernet-rotate` | `<ks>-credential-rotate` |
| `FernetKeysRotated` event | `CredentialKeysRotated` event |
| `spec.fernet.*` | `spec.credentialKeys.*` |

One additional step runs inside the credential CronJob: after
`keystone-manage credential_rotate`, the script runs `keystone-manage credential_migrate`
**before** the staging PATCH. This re-encrypts existing stored credentials
with the new primary key, so by the time the operator applies the Secret
swap there is no surviving plaintext encrypted under a key scheduled for
aging-out (CC-0036).

> **Key-rollover window (pre-existing, not introduced by CC-0081).** There
> is a ~60s window between `credential_migrate` completion and the kubelet
> refreshing the in-place Secret projection (CC-0074). During this window,
> running Keystone pods still have the old keyset mounted and cannot decrypt
> rows already re-encrypted under the new primary. This is a pre-existing
> property of the rotation flow, not a regression introduced by CC-0081.

---

## Related reference

- [Key Rotation RBAC Split](../reference/keystone/keystone-reconciler.md#key-rotation-rbac-split-cc-0081) — the authoritative contract for the Fernet sub-reconciler (CC-0081).
- [Labels and Annotations](../reference/keystone/keystone-reconciler.md#labels-and-annotations) — stable metadata keys observable by consumers (CC-0081).
- [Rotation Scripts](../reference/backend/rotation-scripts.md) — the embedded `fernet_rotate.sh` / `credential_rotate.sh` contract (CC-0073).
- Chainsaw tests: `tests/e2e/keystone/fernet-rotation/chainsaw-test.yaml` and `tests/e2e/keystone/credential-rotation/chainsaw-test.yaml` assert this guide's happy path and the RBAC verb split end-to-end.
