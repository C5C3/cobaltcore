---
title: Rotation Scripts
quadrant: backend
---

# Rotation Scripts

<!-- DECISION: Task 4.2 specified this as a standalone reference document. The rotation
script documentation was integrated into keystone-reconciler.md instead, because the
scripts are sub-reconciler implementation details — their ConfigMap mounting, content-hash
naming, go:embed pattern, and error handling are documented in the context of the
sub-reconcilers that own them. A standalone doc would duplicate or fragment that context.
This file serves as a pointer to the canonical location. Reviewer: please verify. -->

The Fernet and credential rotation scripts (`fernet_rotate.sh`, `credential_rotate.sh`)
are embedded in the operator binary via `go:embed` and mounted into CronJob pods as
immutable, content-hash-named ConfigMaps.

For full reference documentation — including the go:embed pattern, ConfigMap mounting,
content-hash naming, CronJob volume layout, error handling, and idempotency guarantees —
see the sub-reconciler sections in
[Keystone Reconciler Architecture](../keystone/keystone-reconciler.md):

- **`reconcileFernetKeys`** — Fernet key rotation script lifecycle
- **`reconcileCredentialKeys`** — Credential key rotation script lifecycle

## Script Locations

```text
operators/keystone/internal/controller/
├── scripts/
│   ├── fernet_rotate.sh          # Fernet key rotation
│   ├── credential_rotate.sh      # Credential key rotation
│   ├── admin_password_rotate.sh  # Scheduled admin-password rotation (Model B)
│   ├── bootstrap_db_seed.py      # Database seeding helper
│   └── db_sync_reset.py          # db-sync pre-step
├── reconcile_fernet.go           # go:embed + ConfigMap creation
└── reconcile_credential.go       # go:embed + ConfigMap creation
```

The embedded `scripts/` directory also carries `admin_password_rotate.sh`,
`bootstrap_db_seed.py` and `db_sync_reset.py`, which sit outside the
Fernet/credential scope of this page. `admin_password_rotate.sh` belongs to the
scheduled admin-password rotation and follows the same go:embed/ConfigMap
delivery pattern. See the `reconcilePasswordRotation` section of the reconciler
reference and the
[scheduled rotation guide](../../guides/keystone/keystone-admin-password-scheduled-rotation.md).
The two Python scripts are the pre-steps of the bootstrap and db-sync Jobs:
the operator inlines each into its Job's command through a `python3 - <<'PY'`
heredoc, ahead of `keystone-manage bootstrap` and `keystone-manage db_sync`.

## Script Contract

Both scripts follow the same contract:

1. Exit 1 before touching any key when the `rotation-state` emptyDir at
   `/rotation-state` does not exist.
2. A restarted container that finds the key set it saved in
   `/rotation-state/rotated` compares the production keys of the read-only
   source mount `/{type}-keys-src` with that set and with the SHA-256 digest
   in `/rotation-state/source`. Production keys equal to the saved set mean
   the operator committed a PATCH whose response the container never
   received: the script logs `Retry: the production Secret already holds ...`
   and exits 0. Production keys that no longer match the digest mean another
   Job, for example one created with `kubectl create job --from`, committed a
   rotation in the meantime: the script logs
   `Retry: the production ... keys changed since this pod rotated; starting over`
   and discards the saved set.
3. Reset the working copy in `/etc/keystone/{type}-keys` from the source mount
   and write its digest to `/rotation-state/source`. A restarted container
   that still finds its saved set restores that set instead, logs a
   `Retry: restoring ...` line and skips steps 4 and 5.
4. Run `keystone-manage fernet_rotate`, or for credentials
   `keystone-manage credential_migrate` and then `credential_rotate`. The first
   migration puts credentials left under another key under the production
   primary, because `credential_rotate` refuses while any credential is not
   encrypted with the primary key.
5. Save the rotated set to `/rotation-state/rotated.tmp` at mode `0400` and
   rename it to `/rotation-state/rotated`, so a restart finds either the whole
   set or none.
6. For credentials, run `keystone-manage credential_migrate` again to
   re-encrypt the database under the new primary key. A restarted container
   runs it as well: it re-encrypts only credentials under another key, and it
   fails before the PATCH when a credential is under a key the restored set
   lacks.
7. Read rotated keys from the local filesystem
8. Base64-encode key data and PATCH the Kubernetes Secret via the in-cluster API
9. Exit non-zero on HTTP error (status >= 300). The kubelet then restarts the
   container in the same pod (`restartPolicy: OnFailure`), and the retry
   starts again at step 1.

Environment variables required by both scripts:

| Variable | Description |
|---|---|
| `SECRET_NAMESPACE` | Namespace of the target keys Secret |
| `SECRET_NAME` | Name of the target keys Secret |

Optional variables, set only by the script tests
(`operators/keystone/internal/controller/rotation_scripts_test.go`):

| Variable | Default | Description |
|---|---|---|
| `KEY_DIR` | `/etc/keystone/{type}-keys` | Working copy that `keystone-manage` rotates |
| `SRC_DIR` | `/{type}-keys-src` | Read-only production keys the working copy is reset from |
| `STATE_DIR` | `/rotation-state` | Directory holding the saved key set and the digest of the production keys it was rotated from |
