---
title: Rotation Scripts
quadrant: backend
feature: CC-0073
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
│   ├── fernet_rotate.sh          # Fernet key rotation (CC-0073)
│   └── credential_rotate.sh      # Credential key rotation (CC-0073)
├── reconcile_fernet.go           # go:embed + ConfigMap creation
└── reconcile_credential.go       # go:embed + ConfigMap creation
```

## Script Contract

Both scripts follow the same contract:

1. Run `keystone-manage {type}_rotate` (and `credential_migrate` for credentials)
2. Read rotated keys from the local filesystem
3. Base64-encode key data and PATCH the Kubernetes Secret via the in-cluster API
4. Exit non-zero on HTTP error (status >= 300)

Environment variables required by both scripts:

| Variable | Description |
|---|---|
| `SECRET_NAMESPACE` | Namespace of the target keys Secret |
| `SECRET_NAME` | Name of the target keys Secret |
