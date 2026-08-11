---
title: BarbicanSecretStore CRD
quadrant: operator
---

# BarbicanSecretStore CRD

Reference documentation for the BarbicanSecretStore Custom Resource Definition.
One CR attaches to a [Barbican](./barbican-crd.md) CR via `spec.barbicanRef` and
describes the backend the secret material itself is written to. Phase 1 ships
`type: OpenBao`, driven by barbican's vault secret-store plugin.

The attachment is inverted: the store points at the Barbican, so a store is
added or replaced without editing the Barbican CR. A dedicated controller owns
the store lifecycle, the AppRole credentials and the per-store conditions, while
the barbican-side sub-reconciler aggregates the attached, credential-ready
stores into the rendered `barbican.conf` and promotes the one marked `isDefault`
to `global_default`. For that controller topology see
[Barbican Reconciler Architecture](./barbican-reconciler.md).

The CRD is generated from
`operators/barbican/api/v1alpha1/barbicansecretstore_types.go`; the webhook
lives in `barbicansecretstore_webhook.go` and the controllers in
`operators/barbican/internal/controller/barbicansecretstore_controller.go` and
`reconcile_secretstores.go`.

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `barbican.openstack.c5c3.io` |
| Version | `v1alpha1` |
| Kind | `BarbicanSecretStore` |
| List Kind | `BarbicanSecretStoreList` |
| Scope | Namespaced |

**Printer columns:** `kubectl get barbicansecretstores` shows Ready
(`.status.conditions[?(@.type=='Ready')].status`), Type (`.spec.type`), Default
(`.spec.isDefault`), Barbican (`.spec.barbicanRef.name`), and Age.

## Example

```yaml
apiVersion: barbican.openstack.c5c3.io/v1alpha1
kind: BarbicanSecretStore
metadata:
  name: openbao-primary
  namespace: openstack
spec:
  barbicanRef:
    name: barbican
  type: OpenBao
  isDefault: true
  openBao:
    instanceRef:
      name: openbao-instance
status:
  conditions:
  - type: CredentialsReady
    status: "True"
    reason: CredentialsAvailable
  - type: ProvisioningReady
    status: "True"
    reason: Provisioned
  - type: ConfigProjected
    status: "True"
    reason: ConfigProjected
  - type: Ready
    status: "True"
    reason: AllReady
```

The `metadata.name` (`openbao-primary`) becomes the store identifier: it is the
suffix of the `[secretstore:openbao-primary]` section barbican reads and one
entry of the `[secretstore] stores_lookup_suffix` list.

## Spec

### BarbicanSecretStoreSpec

Three schema-level CEL rules hold even when the webhook is down: `barbicanRef`
and `type` are immutable (UPDATE transition rules), and the `type`/`openBao`
union rule `(self.type == 'OpenBao') == has(self.openBao)` requires one store
block matching `spec.type` and no other.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `barbicanRef` | [`BarbicanRefSpec`](#barbicanrefspec) | Yes | — | Names the Barbican CR in the same namespace. **Immutable** (CEL transition rule): re-pointing a store would leave the old deployment with a store nothing manages anymore and race the projection on the new one. Delete and recreate instead. The Barbican need not exist at admission time (GitOps ordering); a dangling reference surfaces as `Ready=False`. |
| `type` | `BarbicanSecretStoreType` | Yes | — | Store plugin; the enum holds one value, `OpenBao`. A brownfield HashiCorp Vault server attaches under the same value: the plugin talks the KV v2 API, which OpenBao and Vault both serve, so the two are interchangeable from barbican's side. **Immutable**, and for a harder reason than `barbicanRef`: material already written through a store cannot be read back through a different plugin. |
| `openBao` | [`*OpenBaoStoreSpec`](#openbaostorespec) | When `type: OpenBao` | — | The OpenBao-backed store. Required when `type` is `OpenBao` and forbidden otherwise (union rule). |
| `isDefault` | `bool` | No | `false` | Marks this store the Barbican global default (`global_default` in its `[secretstore:<name>]` section). **Mutable.** One attached, credential-ready store must carry it and no more than one; the barbican-side sub-reconciler and a sibling-uniqueness webhook both enforce that. |
| `extraOptions` | `map[string]string` | No | — | Free-form `[vault_plugin]` options not covered by the typed fields, keyed by bare option name. The entries merge into the process-global section every OpenBao store on the same Barbican shares. `MaxProperties=32`, and a CEL rule bounds each key at 256 and each value at 1024 characters. See the [denylist](#extraoptions-denylist). |

### BarbicanRefSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The referenced Barbican CR's name (`MinLength=1`). |

### SecretNameRefSpec

Unlike `commonv1.SecretRefSpec`, this reference carries no `key` field: the data
keys are fixed by contract, so there is nothing to select.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The referenced Secret's name (`MinLength=1`). |

### InstanceRefSpec

Kept apart from `SecretNameRefSpec` so the generated schema describes what the
name addresses: the OpenBao instance the operator provisions against, not a
Secret.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | The referenced `OpenBaoCluster`'s name (`MinLength=1`). |

### OpenBaoStoreSpec

Two modes, selected by which of `instanceRef` and `server` is set. Managed mode
points at an `OpenBaoCluster` this cluster runs: the operator mints the AppRole
credentials at runtime and owns the KV mount. Brownfield mode points at an
OpenBao or HashiCorp Vault server elsewhere and is validate-only, the same
read-only posture K-ORC takes with unmanaged imports: the operator reads the
referenced Secrets and renders the configuration, and never creates a mount, a
policy, an AppRole, or secret material on that server.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `instanceRef` | [`*InstanceRefSpec`](#instancerefspec) | Managed mode | — | The `OpenBaoCluster` (`openbao.org/v1alpha1`) in this store's namespace to provision against. Everything else is derived from that name by convention: the server URL `https://<instance>.<namespace>.svc:8200`, the CA Secret `<instance>-tls-ca`, and the provisioner ServiceAccount `<instance>-provisioner` the operator mints a bound token for. |
| `server` | [`*OpenBaoServerSpec`](#openbaoserverspec) | Brownfield mode | — | An OpenBao or Vault server provisioned outside this operator. |
| `kvMountpoint` | `string` (MinLength=1) | No | `barbican` | The path the KV v2 engine holding barbican's material is mounted at. A managed store is pinned to the default; a brownfield store names whatever mount its server provides. The default only fills an absent field, so the `MinLength` guard is what rejects an explicitly empty path. |
| `namespace` | `string` | No | — | Scopes every request to an OpenBao/Vault namespace (the enterprise-style multi-tenancy header). Brownfield only. It also scopes the operator's own login and capability read, since the AppRole lives in that namespace. |

Three further CEL rules shape the block:

| Rule | Message |
| --- | --- |
| `has(self.instanceRef) != has(self.server)` | `exactly one of instanceRef or server must be set` |
| `!has(self.instanceRef) \|\| (self.kvMountpoint == 'barbican' && !has(self.namespace))` | `managed stores (instanceRef) must keep kvMountpoint barbican and leave namespace unset: the self-init contract provisions only the barbican/ mount at the root namespace` |
| `self.kvMountpoint == oldSelf.kvMountpoint` | `kvMountpoint is immutable: the secret material already written under the old mount is not reachable under a new one` |

A fourth, `has(self.instanceRef) == has(oldSelf.instanceRef)`, freezes the mode
itself: switching between a managed instance and a brownfield server re-points
the plugin at a different server entirely. Delete and recreate the store.

### OpenBaoServerSpec

Both Secret references resolve in the store's namespace.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `url` | `string` | Yes | The server's API base URL. `MinLength=1` and pattern `^https://`. TLS is mandatory: the operator's own AppRole login and every secret barbican stores travel this URL, and a plaintext scheme would also make a supplied `caBundleSecretRef` a no-op, leaving a CR that looks TLS-configured and is not. The webhook re-checks the prefix. |
| `caBundleSecretRef` | `*SecretNameRefSpec` | No | The Secret holding the PEM CA bundle that authenticates the server, under the fixed data key `ca.crt`. Omit it when the server presents a certificate the pods already trust through their system store; the rendered config then omits `ssl_ca_crt_file`, and no bundle is mounted. |
| `credentialsSecretRef` | `SecretNameRefSpec` | Yes | The Secret holding the AppRole credentials barbican authenticates with, under the fixed data keys `role-id` and `secret-id`. The AppRole is created on the server outside this operator, which only reads the Secret. |

**Credentials data-key contract.** Both modes read the same keys, pinned by
contract and not selectable per CR (the constants live at the top of
`barbicansecretstore_types.go`):

| Data key | Where it goes |
| --- | --- |
| `role-id` | Rendered as `[vault_plugin] approle_role_id` |
| `secret-id` | Injected as the `OS_VAULT_PLUGIN__APPROLE_SECRET_ID` env var, never written to the config file |
| `ca.crt` | Mounted into the API pods; the mount path is what `[vault_plugin] ssl_ca_crt_file` points at |

## extraOptions Denylist

The validating webhook rejects `extraOptions` keys the projection owns, so the
escape hatch cannot contradict the typed spec. Every key must first match
`^[A-Za-z0-9_]+$` (letters, digits, underscore); vault plugin option names are
snake_case, so that is the full legitimate charset, and the pattern runs before
the exact-match denylist so an embedded newline or a denylist-evading trailing
space cannot slip through. Values carrying a newline or carriage return are
rejected as an INI-injection guard.

| Group | Rejected keys |
| --- | --- |
| Derived from the typed store spec | `vault_url`, `use_ssl`, `ssl_ca_crt_file`, `approle_role_id`, `approle_secret_id`, `root_token_id`, `kv_mountpoint`, `namespace` |
| Structural secret-store wiring | `secret_store_plugin`, `crypto_plugin`, `global_default` |

The renderer consults the same predicate when it merges `extraOptions` into the
plugin section. An exists check alone would only cover the keys the operator
wrote this pass, which leaves the four it never writes (`root_token_id`,
`approle_secret_id`, `crypto_plugin`, and `ssl_ca_crt_file` on a store with no
CA bundle) free to pass through on a CR that reached etcd without the webhook.
`root_token_id` is the one that matters: the plugin prefers a root token over
AppRole authentication, so it would defeat the mount-scoped AppRole the managed
store is built on.

Separately, a store's `metadata.name` becomes its `[secretstore:<name>]` section
suffix, and barbican resolves that suffix against the flat section namespace of
one config file. The webhook therefore rejects a name colliding with a reserved
section: `default` (matched case-insensitively, which covers the upstream
`DEFAULT`), `database`, `keystone_authtoken`, `secretstore`, `vault_plugin`,
`queue`, `oslo_policy`, `oslo_middleware`, and `healthcheck`.

## Default-Store Semantics

One attached, credential-ready store must be marked `isDefault`, and no more
than one. Two gates enforce it:

- **Admission (sibling uniqueness).** When a store under validation sets
  `isDefault`, the webhook lists its namespace siblings (an uncached read),
  filters to the same `spec.barbicanRef.name`, skips self and `Terminating`
  siblings, and rejects if another default already exists. A second rule with
  the same shape rejects a second `OpenBao`-typed sibling whether or not it is
  the default, because the `[vault_plugin]` section is process-global.
- **Reconcile (the counting rule).** The barbican-side sub-reconciler counts the
  credential-ready defaults. Zero or more than one is an invalid projection: it
  sets `SecretStoresReady=False / NoDefaultSecretStore`, re-renders nothing, and
  the config step retains the last-good Secret the live Deployment mounts. More
  than one credential-ready OpenBao store lands on the same path under
  `MultipleOpenBaoStores`.

Flipping `isDefault` between siblings re-renders the config Secret: unlike the
Glance backends split, barbican keeps the default marker inside the per-store
section (`global_default`) of the one document, so the content hash changes and
the pods roll.

Deleting a store is not a no-op for the data behind it. Barbican resolves every
stored secret through the `secret_stores` row naming the store it was written
to, so once a `[secretstore:<name>]` section is gone, every secret written under
it stops resolving. Nothing else says so: the store CR carries no finalizer, its
deletion is not validated, and the parent re-renders as soon as the remaining
stores form a valid projection. The `SecretStoreDetached` Warning event fires on
the pass that de-projects the store and names that consequence.

### Conditions

The `BarbicanSecretStoreReconciler` is the single writer of this status. The
barbican-side sub-reconciler reads `CredentialsReady` alone (never the aggregate
`Ready`, which also requires `ConfigProjected` and would deadlock, since
`ConfigProjected` only turns True after the projection lands) and writes the
aggregated `SecretStoresReady` condition onto the Barbican CR instead.

| Type | Owner | Status | Reason | Meaning |
| --- | --- | --- | --- | --- |
| `CredentialsReady` | BarbicanSecretStore | True | `CredentialsAvailable` | The credentials in hand are accepted by the server: a managed store's minted pair passed a login probe, a brownfield store's referenced pair logged in and holds the `create`, `read`, `update`, `delete`, `list` capabilities on the mount's data path |
| `CredentialsReady` | BarbicanSecretStore | False | `WaitingForCredentials` | The referenced Secret does not carry a non-empty `role-id`, `secret-id`, or `ca.crt` yet |
| `CredentialsReady` | BarbicanSecretStore | False | `InvalidCredentials` | The server rejects the credentials. A managed store re-mints once, at most once per ten minutes, and reports the rejection in between |
| `CredentialsReady` | BarbicanSecretStore | False | `InsufficientCapabilities` | The AppRole policy does not grant all five capabilities on the probe path |
| `CredentialsReady` | BarbicanSecretStore | False | `OpenBaoUnreachable` | The server did not answer, or the client could not be built |
| `ProvisioningReady` | BarbicanSecretStore | True | `Provisioned` | The referenced `OpenBaoCluster` is Available and carries the `barbican` KV mount and the `barbican` AppRole |
| `ProvisioningReady` | BarbicanSecretStore | False | `InstanceNotFound` | No `OpenBaoCluster` of that name exists in the store's namespace |
| `ProvisioningReady` | BarbicanSecretStore | False | `WaitingForInstance` | The instance is not Available yet, or its provisioner ServiceAccount does not exist |
| `ProvisioningReady` | BarbicanSecretStore | False | `WaitingForInstanceTLS` | The `<instance>-tls-ca` Secret does not carry the `ca.crt` trust bundle yet |
| `ProvisioningReady` | BarbicanSecretStore | False | `InstanceNotProvisioned` | The instance is up but the self-init requests (`barbican_kv`, `approle_auth`, `barbican_approle_role`) did not run: the mount or the AppRole is missing |
| `ProvisioningReady` | BarbicanSecretStore | False | `ProvisioningDenied` | The operator may not mint a bound token for the provisioner ServiceAccount, or OpenBao answered 403. The TokenRequest grant is namespace- and account-scoped by design and is rendered per `rbac.secretStoreNamespaces` key, so an unlisted namespace lands here |
| `ConfigProjected` | BarbicanSecretStore | True | `ConfigProjected` | The parent Barbican Deployment mounts a `barbican.conf` carrying this store's `[secretstore:<name>]` section |
| `ConfigProjected` | BarbicanSecretStore | False | `WaitingForProjection` | The projection has not landed in the Deployment yet |
| `Ready` | BarbicanSecretStore | True | `AllReady` | Every sub-condition of the store's mode is True |
| `Ready` | BarbicanSecretStore | False | `NotAllReady` | At least one is not |
| `SecretStoresReady` | Barbican | True | `AllStoresProjected` | Every attached store is credential-ready and projected, with a valid default |
| `SecretStoresReady` | Barbican | False | `NoDefaultSecretStore` | Zero or more than one credential-ready store is marked `isDefault`; last-good config is retained |
| `SecretStoresReady` | Barbican | False | `MultipleOpenBaoStores` | More than one credential-ready store is of type `OpenBao`; the reconcile-time backstop for a store that bypassed admission |
| `SecretStoresReady` | Barbican | False | `WaitingForSecretStores` | The default store passed its own credential gate, but its credentials Secret could not be read this pass, or an assembled option carried a control character |

`ProvisioningReady` is managed-mode only. A brownfield store has no instance
this operator provisions, so the condition is removed from the status on every
pass and the aggregate is derived from the remaining two, which keeps a store
that used to name an `instanceRef` from waiting on a condition nothing sets.

**Per-store fault isolation.** When the default store's credentials Secret
cannot be read, or an assembled `[vault_plugin]` value carries a control
character, the barbican-side step keeps the last-good configuration, emits a
`BarbicanSecretStoreSkipped` Warning event on the Barbican CR, and waits for the
store watch. None of the waiting states returns an error or a requeue: a store's
status flip re-enqueues the parent.

## Immutability and Validation Summary

Schema-layer rules (CEL and kubebuilder markers, enforced even when the webhook
is down): the `barbicanRef` and `type` transition rules, the `type`/`openBao`
union, the `OpenBao` type enum, the `instanceRef`/`server` union, the managed
mount-and-namespace pin, the `kvMountpoint` and mode transition rules, the
`^https://` URL pattern, the `MinLength` guards on every reference name, and the
`extraOptions` `MaxProperties` and key/value length bounds.

Webhook rules (defense in depth plus what CEL cannot express): the union
re-checks, the `https://` prefix re-check, the reserved-section collision guard
on `metadata.name`, the `extraOptions` key pattern and denylist, the
INI-injection guard on `extraOptions` values, the single-default sibling check,
and the single-OpenBao-store sibling check. The `kvMountpoint` default
(`barbican`) is materialized by the defaulting webhook for callers that bypass
it.

`metadata.name` is bounded at 55 characters, checked on create only for the
reason the [Barbican CRD](./barbican-crd.md#defaulting-and-validation) gives for
its own bound: the operator derives the AppRole credentials Secret by appending
`-approle`, and a Secret name is capped at the 63 characters of a DNS label, so
`MaxBarbicanSecretStoreNameLength = 63 − len("-approle")`.

## Chainsaw E2E Tests

The managed flow (an `OpenBaoCluster` in the suite, the minted `-approle`
Secret, and the rendered store section) lives in
`tests/e2e/barbican/secretstore-managed`. The brownfield flow, where the AppRole
is seeded outside the operator and the store is validate-only, lives in
`tests/e2e/barbican/secretstore-brownfield`. Flipping `isDefault` between two
attached stores and re-rendering `global_default` lives in
`tests/e2e/barbican/default-secretstore-switch`. The detach path lives in
`tests/e2e/barbican/deletion-cleanup`. The rejection corpus lives in
`tests/e2e/barbican/invalid-barbicansecretstore-cr` and covers the union rules,
the managed-mode pins, the reserved names, the name bound, the `extraOptions`
guards, the transition rules, and both sibling rules.

## Retained Artefacts

A managed store's AppRole credentials live in a Secret named after the store,
`<store>-approle`, carrying `role-id` and `secret-id` under the contract keys.
The operator writes it with a controller owner reference to the store CR, so
Kubernetes garbage collection reclaims it when the store is deleted. There is no
finalizer, and that is the design: the AppRole itself is shared instance state
the self-init contract owns rather than state of this CR, so revoking it on
delete would break every other store on the same instance. Deleting a store
detaches it and nothing more.

Two annotations on that Secret carry the re-mint timer:

| Annotation | Value |
| --- | --- |
| `barbican.c5c3.io/secret-id-minted-at` | RFC 3339 timestamp of the mint |
| `barbican.c5c3.io/secret-id-ttl-seconds` | The TTL the server returned; `0` means the secret ID does not expire |

They are the only record there is. OpenBao hands a secret ID out once and offers
no read-back, so the operator re-mints at two thirds of the recorded TTL and
verifies the credentials in hand with a login probe otherwise. A brownfield
store's Secrets are referenced, never written: the operator reads `role-id`,
`secret-id`, and (when `caBundleSecretRef` is set) `ca.crt`, and leaves their
lifecycle to whoever created them.
