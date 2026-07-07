---
title: KeystoneIdentityBackend CRD API Reference
quadrant: operator
---

# KeystoneIdentityBackend CRD API Reference

Reference documentation for the KeystoneIdentityBackend Custom Resource
Definition. One CR attaches to a [Keystone](./keystone-crd.md) CR via
`spec.keystoneRef` and describes one external identity backend — an LDAP/AD-backed
domain: connection and bind credentials, the user/group tree layout and
attribute mapping, read-only mode, TLS, and an `extraOptions` escape hatch.

For the task-oriented walkthrough, see the
[LDAP Domain Backend guide](../../guides/ldap-domain-backend.md). For the
controller topology (dedicated backend controller + keystone-side projection),
see [Keystone Reconciler Architecture](./keystone-reconciler.md).

## API Group and Version

| Field | Value |
| --- | --- |
| Group | `keystone.openstack.c5c3.io` |
| Version | `v1alpha1` |
| Kind | `KeystoneIdentityBackend` |
| List Kind | `KeystoneIdentityBackendList` |
| Scope | Namespaced |

**Scheme registration:** the `init()` function in
`keystoneidentitybackend_types.go` registers both Kinds with the shared
`SchemeBuilder`, so `AddToScheme` covers `Keystone` and
`KeystoneIdentityBackend` alike.

## Example

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: corp-ldap
  namespace: openstack
spec:
  keystoneRef:
    name: keystone
  domain:
    name: corp
    mode: Manage
    deletionPolicy: Retain
    description: Corporate directory
  type: LDAP
  ldap:
    url: ldaps://ldap.corp.example.com:636
    bindCredentialsSecretRef:
      name: corp-ldap-bind
    suffix: dc=corp,dc=example,dc=com
    readOnly: true
    users:
      treeDN: ou=people,dc=corp,dc=example,dc=com
      objectClass: inetOrgPerson
      idAttribute: uid
      nameAttribute: uid
      mailAttribute: mail
    groups:
      treeDN: ou=groups,dc=corp,dc=example,dc=com
    tls:
      caBundleSecretRef:
        name: corp-ldap-ca
status:
  conditions:
  - type: DomainReady
    status: "True"
    reason: DomainProvisioned
  - type: ConfigProjected
    status: "True"
    reason: ConfigProjected
  - type: Ready
    status: "True"
    reason: AllReady
  domainID: 8c2f1d3e5a794b0f9a1c
```

**Printer columns:** `kubectl get keystoneidentitybackends` shows Ready
(`.status.conditions[?(@.type=='Ready')].status`), Domain
(`.spec.domain.name`), Keystone (`.spec.keystoneRef.name`), and Age.

## Spec

### KeystoneIdentityBackendSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `keystoneRef` | `KeystoneRefSpec` | Yes | — | Names the Keystone CR in the same namespace this backend attaches to. **Immutable** (CEL transition rule): re-pointing a backend at a different Keystone would strand the provisioned domain; delete and recreate instead. The referenced CR does **not** have to exist at admission time (GitOps ordering) — a dangling reference surfaces as `DomainReady=False/KeystoneNotFound`. |
| `domain` | [`DomainSpec`](#domainspec) | Yes | — | The Keystone domain this backend provides. |
| `type` | `IdentityBackendType` | Yes | — | Backend driver. Phase 1 supports `LDAP` only; the federation phases extend the enum. **Immutable** (CEL transition rule). |
| `ldap` | [`LDAPBackendSpec`](#ldapbackendspec) | When `type: LDAP` | — | LDAP/AD connection, tree layout, and attribute mapping. The union rule (`(self.type == 'LDAP') == has(self.ldap)`) enforces exactly one backend block matching `spec.type` at the schema layer. |
| `extraOptions` | `map[string]string` | No | — | Free-form `[ldap]` section options not covered by typed fields, keyed by bare option name (e.g. `page_size`). See the [denylist](#extraoptions-denylist). |

### KeystoneRefSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | Name of the Keystone CR in the same namespace (`MinLength=1`). |

### DomainSpec

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `name` | `string` | Yes | — | Keystone domain name (1–64 chars, pattern `^[A-Za-z0-9][A-Za-z0-9_.-]*$` — the grammar shared by Secret data keys and keystone's `keystone.<name>.conf` domain-config file naming). **Immutable.** `default` (any case) is rejected: the Default domain hosts the SQL-backed service users and the bootstrap admin and must never be backed by an external directory. Domain names must additionally be unique (case-insensitively) per referenced Keystone — webhook-enforced. |
| `mode` | `Manage` \| `Adopt` | No | `Manage` | `Manage`: the controller creates the domain (and reconciles description/enabled drift on its own domain). `Adopt`: the controller resolves a pre-existing domain by name and **never mutates it**. **Immutable.** |
| `deletionPolicy` | `Retain` \| `Delete` | No | `Retain` | What happens to a **managed** domain when this CR is deleted: `Retain` leaves it in place, `Delete` disables it and then deletes it (keystone forbids deleting an enabled domain). Adopted domains are always retained regardless of this field. Mutable, so operators decide at teardown time. |
| `description` | `string` | No | — | Projected onto the domain in `Manage` mode. |

### LDAPBackendSpec

Only user-set optional fields are rendered into the per-domain config, so
upstream keystone defaults apply for everything left unset — with one
deliberate exception: unless `extraOptions` carries any `user_enabled_*` key,
the projection renders `user_enabled_invert = true` and
`user_enabled_default = false`, which makes every user read as enabled.
Directories without an "enabled" concept (plain `inetOrgPerson` /
`posixAccount` trees — no standard LDAP attribute exists for it) otherwise
yield user models without the `enabled` key, and keystone's response-schema
validation rejects its own reply with HTTP 400 (`'enabled' is a required
property`) on every user listing. Deployments whose directory does model
enabled semantics (e.g. Active Directory's `userAccountControl` mask) set the
matching `user_enabled_*` options via `extraOptions`, which suppresses both
defaults.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `url` | `string` | Yes | — | LDAP server URL; pattern `^ldaps?://`. |
| `bindCredentialsSecretRef` | `SecretRefSpec` | Yes | — | Secret holding the bind credentials under the **fixed data keys** `username` (the bind DN) and `password`. The `key` field must stay empty (webhook-enforced). |
| `suffix` | `string` | Yes | — | LDAP suffix (base DN), e.g. `dc=example,dc=com`. |
| `users` | [`LDAPUserSpec`](#ldapuserspec) | Yes | — | User tree layout and attribute mapping. |
| `groups` | [`LDAPGroupSpec`](#ldapgroupspec) | No | — | Group tree layout and attribute mapping; when unset no `group_*` options are rendered. |
| `readOnly` | `*bool` | No | `true` | Forces `user_allow_create/update/delete` and `group_allow_create/update/delete` to `false` so keystone can never write into the corporate directory. Setting `false` is an explicit opt-in to a writable backend. |
| `tls` | [`LDAPTLSSpec`](#ldaptlsspec) | No | — | Certificate verification for `ldaps://` / STARTTLS connections. |
| `pool` | [`LDAPPoolSpec`](#ldappoolspec) | No | — | Keystone-side LDAP connection pool. |

### LDAPUserSpec

| Field | Type | Required | Rendered `[ldap]` option | Keystone default when unset |
| --- | --- | --- | --- | --- |
| `treeDN` | `string` | Yes | `user_tree_dn` | — |
| `filter` | `string` | No | `user_filter` | none |
| `objectClass` | `string` | No | `user_objectclass` | `inetOrgPerson` |
| `idAttribute` | `string` | No | `user_id_attribute` | `cn` |
| `nameAttribute` | `string` | No | `user_name_attribute` | `sn` |
| `mailAttribute` | `string` | No | `user_mail_attribute` | `mail` |

### LDAPGroupSpec

| Field | Type | Required | Rendered `[ldap]` option | Keystone default when unset |
| --- | --- | --- | --- | --- |
| `treeDN` | `string` | Yes | `group_tree_dn` | — |
| `filter` | `string` | No | `group_filter` | none |
| `objectClass` | `string` | No | `group_objectclass` | `groupOfNames` |
| `idAttribute` | `string` | No | `group_id_attribute` | `cn` |
| `nameAttribute` | `string` | No | `group_name_attribute` | `ou` |
| `memberAttribute` | `string` | No | `group_member_attribute` | `member` |

### LDAPTLSSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `caBundleSecretRef` | `SecretRefSpec` | Yes | Secret holding the CA bundle under the **fixed data key** `ca.crt` (the canonical cert-manager file name, mirroring the database TLS contract). The projection writes the PEM to `/etc/keystone/domains/<domain>-ca.pem` and points `tls_cacertfile` at it. |

### LDAPPoolSpec

| Field | Type | Required | Rendered `[ldap]` option |
| --- | --- | --- | --- |
| `enabled` | `bool` | No | `use_pool` |
| `size` | `*int32` (Minimum=1) | No | `pool_size` (only when set) |

## extraOptions Denylist

The validating webhook rejects `extraOptions` keys the projection owns, so the
escape hatch cannot silently contradict the typed spec:

| Group | Rejected keys |
| --- | --- |
| Rendered from typed fields | `url`, `suffix`, `user`, `password`, `tls_cacertfile`, `use_pool`, `pool_size`, `user_tree_dn`, `user_filter`, `user_objectclass`, `user_id_attribute`, `user_name_attribute`, `user_mail_attribute`, `group_tree_dn`, `group_filter`, `group_objectclass`, `group_id_attribute`, `group_name_attribute`, `group_member_attribute` |
| Operator-owned wiring | `driver`, `domain_config_dir` |
| readOnly-forced (only when `readOnly` is true, the default) | `user_allow_create`, `user_allow_update`, `user_allow_delete`, `group_allow_create`, `group_allow_update`, `group_allow_delete` |

Keys are bare `[ldap]` option names; per-domain `[identity]`-section tuning is
not expressible in Phase 1.

## Status

### KeystoneIdentityBackendStatus

The dedicated `KeystoneIdentityBackendReconciler` is the **single writer** of
this status. The keystone-side `identitybackends` sub-reconciler only reads
`DomainReady` (it gates config projection) and writes the aggregated
`IdentityBackendsReady` condition onto the Keystone CR instead.

| Field | Type | Description |
| --- | --- | --- |
| `conditions` | `[]metav1.Condition` | `DomainReady`, `ConfigProjected`, and the aggregate `Ready` (see below). |
| `observedGeneration` | `int64` | The `.metadata.generation` the controller last reconciled. |
| `domainID` | `string` | The Keystone domain ID this backend provisioned (Manage) or resolved (Adopt). The deletion path uses it to disable+delete exactly the domain this CR created, never a same-named foreign one. |

### Conditions

| Type | Status | Reason | Meaning |
| --- | --- | --- | --- |
| `DomainReady` | True | `DomainProvisioned` | Manage mode created (or re-recognized) the domain. |
| `DomainReady` | True | `DomainAdopted` | Adopt mode resolved the pre-existing domain by name. |
| `DomainReady` | False | `KeystoneNotFound` | `spec.keystoneRef` does not resolve (yet); the Keystone watch re-wakes the backend when it appears. |
| `DomainReady` | False | `WaitingForKeystoneAPI` | The referenced Keystone's `KeystoneAPIReady` condition is not True yet. |
| `DomainReady` | False | `AdminSecretUnavailable` | The bootstrap admin password Secret is missing or has no `password` key. |
| `DomainReady` | False | `DomainNotFound` | Adopt mode: no domain with the requested name exists (Adopt never creates). |
| `DomainReady` | False | `DomainAlreadyExists` | Manage mode: a same-named domain exists that this CR did not create — never silently seized; switch to `mode: Adopt` to attach to it. |
| `DomainReady` | False | `IdentityAPIError` | A domain lookup/create/update call failed. |
| `ConfigProjected` | True | `ConfigProjected` | The Keystone Deployment's `domains` volume Secret carries this backend's `keystone.<domain>.conf`. |
| `ConfigProjected` | False | `WaitingForProjection` | The projection has not landed in the Deployment yet. |
| `Ready` | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | False | `NotAllReady` | At least one sub-condition is not True. |

## Deletion Semantics

Deleting a backend CR runs the `keystone.openstack.c5c3.io/identitybackend`
finalizer:

1. **De-projection first.** The keystone-side sub-reconciler drops the
   backend's config file from the domains Secret and rolls the Deployment;
   the finalizer waits for this so keystone never runs with config pointing
   at a dead domain.
2. **Deletion policy.** Only for `mode: Manage` + `deletionPolicy: Delete`
   with a recorded `status.domainID`: the domain is disabled and then deleted
   (keystone forbids deleting an enabled domain). `Retain` (the default) and
   adopted domains always leave the domain in place.
3. **Fail open.** When the referenced Keystone CR is gone (stack teardown),
   or the admin credential is no longer available, the finalizer releases
   with a Warning event instead of holding the backend hostage; the domain is
   retained.

## Immutability and Validation Summary

Schema-layer rules (CEL / kubebuilder markers, enforced by the API server
even when the webhook is down): keystoneRef / type / domain.name /
domain.mode transition rules, the type-vs-ldap union, the Default-domain
rejection, the URL scheme pattern, and the domain-name grammar.

Webhook rules (defense-in-depth plus the rules CEL cannot express):
domain-name uniqueness per referenced Keystone (case-insensitive, via a
direct-reader List that skips Terminating siblings), the fixed data-key
contract on the bind/CA Secret references, the
[`extraOptions` denylist](#extraoptions-denylist), and an INI-injection guard
that rejects a newline or carriage-return in any rendered value (every typed
`ldap` field and every `extraOptions` value) — such a character would inject
arbitrary `[ldap]` lines through the verbatim config render, defeating the
`readOnly` forcing and the denylist. The projection re-validates the fully
assembled `[ldap]` values as the last line of defense: it is the only gate
that sees the Secret-sourced bind username/password and the only gate that
still runs when a CR bypassed admission, and it skips (with a Warning event)
any backend whose value carries a control character rather than emitting a
corrupted config.

## Chainsaw E2E Tests

The rejection corpus lives in `tests/e2e/keystone/invalid-identitybackend-cr/`
(generated from `_generate.py`, guarded by `make verify-invalid-cr-fixtures`);
the end-to-end LDAP flow — in-suite OpenLDAP fixture, domain provisioning,
projection, token issuance for an LDAP user, and clean detach — lives in
`tests/e2e/keystone/ldap-domain-backend/`. See
[Keystone E2E Test Suites](../testing/keystone-e2e-tests.md).

## Retained Artefacts

Like the config ConfigMaps, up to 3 historical content-hashed
`<keystone>-domains-<hash>` Secrets are retained while backends exist (fast
rollback); when the last backend detaches every historical Secret is removed
(no bind password may linger), and all of them are garbage-collected with the
owning Keystone CR.
