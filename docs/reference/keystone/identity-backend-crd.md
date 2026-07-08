---
title: KeystoneIdentityBackend CRD API Reference
quadrant: operator
---

# KeystoneIdentityBackend CRD API Reference

Reference documentation for the KeystoneIdentityBackend Custom Resource
Definition. One CR attaches to a [Keystone](./keystone-crd.md) CR via
`spec.keystoneRef` and describes one external identity backend:

- **`type: LDAP`** — an LDAP/AD-backed domain: connection and bind
  credentials, the user/group tree layout and attribute mapping, read-only
  mode, TLS, and an `extraOptions` escape hatch.
- **`type: OIDC`** — an OpenID Connect federation backend: the issuer and
  discovery shape, the relying-party client, typed keystone mapping rules,
  and declarative target groups with role assignments. The controller
  provisions the keystone federation API objects (identity provider,
  mapping, protocol) and the keystone-side projection renders the
  `mod_auth_openidc` reverse-proxy sidecar configuration.

For the task-oriented walkthroughs, see the
[LDAP Domain Backend guide](../../guides/ldap-domain-backend.md) and the
[OIDC Federation guide](../../guides/oidc-federation.md). For the
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

An OIDC federation backend:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: keycloak-forge
  namespace: openstack
spec:
  keystoneRef:
    name: keystone
  domain:
    name: forge
    mode: Manage
    deletionPolicy: Delete
  type: OIDC
  oidc:
    issuer: https://keycloak.example.com/realms/forge
    clientID: keystone
    clientSecretRef:
      name: keycloak-forge-client
    oauth2Introspection:
      enabled: true
  mappings:
  - remote:
    - type: HTTP_OIDC_ISS
      anyOneOf:
      - https://keycloak.example.com/realms/forge
    - type: HTTP_OIDC_PREFERRED_USERNAME
    local:
    - user:
        name: "{1}"
      group:
        name: federated-users
        domain:
          name: forge
  groups:
  - name: federated-users
    roleAssignments:
    - role: member
      domain: true
status:
  conditions:
  - type: DomainReady
    status: "True"
    reason: DomainProvisioned
  - type: FederationObjectsReady
    status: "True"
    reason: FederationObjectsProvisioned
  - type: MappingsReady
    status: "True"
    reason: MappingsApplied
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
| `type` | `IdentityBackendType` | Yes | — | Backend driver: `LDAP` or `OIDC`. **Immutable** (CEL transition rule). |
| `ldap` | [`LDAPBackendSpec`](#ldapbackendspec) | When `type: LDAP` | — | LDAP/AD connection, tree layout, and attribute mapping. The union rule (`(self.type == 'LDAP') == has(self.ldap)`) enforces exactly one backend block matching `spec.type` at the schema layer. |
| `oidc` | [`OIDCBackendSpec`](#oidcbackendspec) | When `type: OIDC` | — | OpenID Connect federation configuration. The mirror union rule (`(self.type == 'OIDC') == has(self.oidc)`) applies. |
| `mappings` | [`[]MappingRuleSpec`](#mappingrulespec) | No (OIDC only) | — | Keystone federation mapping rules applied to the backend's protocol; type-gated to `OIDC` at the schema layer. Federation cannot provision its protocol without at least one rule (keystone rejects rule-less mappings) — an empty list parks the backend at `MappingsReady=False/NoMappingRules`. Max 32 rules. |
| `groups` | [`[]FederationGroupSpec`](#federationgroupspec) | No (OIDC only) | — | Declarative local groups (in this backend's domain) the mapping rules target, plus their role assignments; type-gated to `OIDC`. Max 32. |
| `extraOptions` | `map[string]string` | No (LDAP only) | — | Free-form `[ldap]` section options not covered by typed fields, keyed by bare option name (e.g. `page_size`); type-gated to `LDAP`. See the [denylist](#extraoptions-denylist). |

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

### OIDCBackendSpec

Configures an OpenID Connect federation backend for one domain. Discovery is
either metadata-driven (`providerMetadataURL`, defaulted to
`<issuer>/.well-known/openid-configuration` by the webhook) or fully
explicit (`endpoints`); the two shapes are mutually exclusive (CEL rule plus
webhook defense-in-depth). The operator fetches the discovery document at
reconcile time and pre-provisions it into the sidecar's read-only
`OIDCMetadataDir` — the module cannot self-cache there.

| Field | Type | Required | Default | Description |
| --- | --- | --- | --- | --- |
| `issuer` | `string` | Yes | — | The OIDC issuer URL exactly as the IdP asserts it (the `iss` claim); pattern `^https?://`. Registered as the keystone identity provider's remote ID and encoded (scheme-stripped, URL-escaped) into the metadata file basenames. |
| `providerMetadataURL` | `string` | No | derived from `issuer` | The OIDC discovery document URL; pattern `^https?://`. Mutually exclusive with `endpoints`. A fetch failure or a document whose `issuer` mismatches the spec parks the backend pending with a Warning event. |
| `endpoints` | [`OIDCEndpointsSpec`](#oidcendpointsspec) | No | — | Explicit provider endpoints for air-gapped operators or IdPs unreachable from the operator pod. |
| `clientID` | `string` | Yes | — | The relying-party client ID registered at the IdP. |
| `clientSecretRef` | `SecretRefSpec` | Yes | — | Secret holding the relying-party client secret under the **fixed data key** `clientSecret`. The `key` field must stay empty (webhook-enforced), mirroring the LDAP bind Secret contract. |
| `protocolID` | `string` | No | `openid` | Keystone federation protocol ID embedded in the websso/auth URLs (`…/protocols/<protocolID>/websso`); pattern `^[A-Za-z0-9_-]+$`. |
| `identityProviderName` | `string` | No | the CR name | Keystone identity provider ID this backend provisions; pattern `^[A-Za-z0-9_-]+$`. Unique per referenced Keystone (webhook-enforced). |
| `remoteIDAttribute` | `string` | No | `HTTP_OIDC_ISS` | WSGI environ key keystone reads the asserted issuer from (`[openid] remote_id_attribute`). Must be uniform across every OIDC backend of one Keystone (webhook-enforced) because it renders into the single `[openid]` section. |
| `scopes` | `[]string` | No | `["openid", "email", "profile"]` | OIDC scopes requested from the IdP. Max 16. |
| `responseType` | `string` | No | `code` | OIDC response type (the authorization-code flow). |
| `oauth2Introspection` | [`OIDCIntrospectionSpec`](#oidcintrospectionspec) | No | — | Turns the proxy into an OAuth2 resource server so CLI clients present IdP-issued bearer tokens directly. **At most one OIDC backend per Keystone may enable this** (webhook-enforced): the module's `OIDCOAuth*` directives are server-scoped. |
| `sessionType` | `client-cookie` \| `client-cookie-persistent` | No | `client-cookie` | mod_auth_openidc session storage. The default keeps the whole session in the browser cookie — HA-safe across replicas, no shared server-side cache. |
| `stateInputHeaders` | `none` \| `user-agent` \| `x-forwarded-for` \| `both` | No | `none` | Request headers folded into the state cookie hash. `none` is the HA-safe default: replicas behind one Service must not bind state to per-connection headers. |

### OIDCEndpointsSpec

All URL fields share the `^https?://` scheme guard.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `authorizationEndpoint` | `string` | Yes | OAuth2 authorization endpoint. |
| `tokenEndpoint` | `string` | Yes | OAuth2 token endpoint. |
| `jwksURI` | `string` | Yes | JSON Web Key Set document. |
| `userinfoEndpoint` | `string` | No | OIDC userinfo endpoint. |
| `endSessionEndpoint` | `string` | No | RP-initiated logout endpoint. |
| `introspectionEndpoint` | `string` | No | OAuth2 token introspection endpoint. **Required when `oauth2Introspection` is enabled with explicit endpoints** (webhook-enforced). |

### OIDCIntrospectionSpec

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `enabled` | `bool` | Yes | Turns bearer-token introspection on for this backend. |

### MappingRuleSpec

One keystone federation mapping rule: remote assertion matchers plus the
local objects they map to. The typed shape mirrors keystone's mapping-rule
JSON one-to-one — the rule grammar is closed, so no free-form escape hatch
exists. The camelCase field names map to the snake_case JSON keys:

| CRD field | Keystone mapping JSON key |
| --- | --- |
| `mappings[].local` / `mappings[].remote` | `rules[].local` / `rules[].remote` |
| `remote[].type` | `type` |
| `remote[].regex` | `regex` |
| `remote[].anyOneOf` | `any_one_of` |
| `remote[].notAnyOf` | `not_any_of` |
| `remote[].blacklist` | `blacklist` |
| `remote[].whitelist` | `whitelist` |
| `local[].user.{id,name,email,type}` | `user.{id,name,email,type}` |
| `local[].user.domain.{id,name}` | `user.domain.{id,name}` |
| `local[].group.{id,name}` | `group.{id,name}` |
| `local[].group.domain.{id,name}` | `group.domain.{id,name}` |
| `local[].groupIds` | `group_ids` |
| `local[].groups` | `groups` |
| `local[].projects[].name` | `projects[].name` |
| `local[].projects[].roles[].name` | `projects[].roles[].name` |
| `local[].domain.{id,name}` | `domain.{id,name}` |

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `local` | `[]MappingLocalRuleSpec` | Yes (1–16) | Local Identity API objects the matched remote user maps to. Every rule needs at least one local entry (webhook-enforced). |
| `remote` | `[]MappingRemoteRuleSpec` | Yes (1–16) | Assertion matchers. `type` is the **full WSGI environ key** (e.g. `HTTP_OIDC_ISS`, `HTTP_OIDC_PREFERRED_USERNAME`) — keystone's `assertion_prefix` stays empty so the claim headers arrive unmodified. Every rule needs at least one remote entry with a non-empty type (webhook-enforced). |

### FederationGroupSpec

Declares one local keystone group (created if missing in the backend's
domain, never mutated afterwards — keystone cascades group deletion with the
domain per the deletion policy) plus the role assignments granting its
members access.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | `string` | Yes | Group name inside the backend's domain (1–128 chars). |
| `description` | `string` | No | Projected onto the group at creation. |
| `roleAssignments` | `[]FederationRoleAssignmentSpec` | No (max 32) | Role grants for the group. |

### FederationRoleAssignmentSpec

Exactly one of `project` or `domain` must be set (CEL rule plus webhook
defense-in-depth). The role and project are resolved by name at reconcile
time; a missing role or project parks the backend at
`MappingsReady=False/RoleOrProjectNotFound` on a bounded poll.

| Field | Type | Required | Description |
| --- | --- | --- | --- |
| `role` | `string` | Yes | Role name to grant (e.g. `member`). |
| `project` | `FederationProjectScopeSpec` | No | Scopes the assignment to a project: `name` (required) and `domainName` (defaults to the backend's own domain). |
| `domain` | `bool` | No | Scopes the assignment to the backend's domain. |

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
| `conditions` | `[]metav1.Condition` | `DomainReady`, `ConfigProjected`, for OIDC backends additionally `FederationObjectsReady` and `MappingsReady`, and the aggregate `Ready` (see below). The aggregate derives from the backend type's own sub-condition set, so LDAP backends are unaffected by the federation-only types. |
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
| `FederationObjectsReady` | True | `FederationObjectsProvisioned` | OIDC: the identity provider and protocol are upserted and drift-free. |
| `FederationObjectsReady` | False | `NoMappingRules` | OIDC: `spec.mappings` is empty — keystone cannot represent a rule-less mapping and the protocol needs one. |
| `FederationObjectsReady` | False | `IdentityAPIError` | OIDC: an identity-provider/protocol call failed. |
| `MappingsReady` | True | `MappingsApplied` | OIDC: the mapping rules, declarative groups, and role assignments are applied. |
| `MappingsReady` | False | `NoMappingRules` | OIDC: `spec.mappings` is empty. |
| `MappingsReady` | False | `RoleOrProjectNotFound` | OIDC: a role assignment references a role, project, or project domain that does not exist (yet); retried on a bounded poll. |
| `MappingsReady` | False | `IdentityAPIError` | OIDC: a mapping/group/assignment call failed. |
| `ConfigProjected` | True | `ConfigProjected` | LDAP: the Keystone Deployment's `domains` volume Secret carries this backend's `keystone.<domain>.conf`. OIDC: the federation Secret mounted by the sidecar carries this backend's client document. |
| `ConfigProjected` | False | `WaitingForProjection` | The projection has not landed in the Deployment yet. |
| `Ready` | True | `AllReady` | Both sub-conditions are True. |
| `Ready` | False | `NotAllReady` | At least one sub-condition is not True. |

## Deletion Semantics

Deleting a backend CR runs the `keystone.openstack.c5c3.io/identitybackend`
finalizer:

1. **De-projection first.** The keystone-side sub-reconciler drops the
   backend's config from the projection (the domains Secret for LDAP, the
   federation Secret — and with the last OIDC backend, the whole sidecar —
   for OIDC) and rolls the Deployment; the finalizer waits for this so
   keystone never runs with config pointing at a dead domain.
2. **Federation-object teardown (OIDC).** The protocol, mapping, and
   identity provider are removed in reverse dependency order —
   unconditionally, regardless of the domain deletion policy — tolerating
   objects already gone. Declarative groups follow the domain (keystone
   cascades domain contents).
3. **Deletion policy.** Only for `mode: Manage` + `deletionPolicy: Delete`
   with a recorded `status.domainID`: the domain is disabled and then deleted
   (keystone forbids deleting an enabled domain). `Retain` (the default) and
   adopted domains always leave the domain in place.
4. **Fail open.** When the referenced Keystone CR is gone (stack teardown),
   or the admin credential is no longer available, the finalizer releases
   with a Warning event instead of holding the backend hostage; the domain is
   retained.

## Immutability and Validation Summary

Schema-layer rules (CEL / kubebuilder markers, enforced by the API server
even when the webhook is down): keystoneRef / type / domain.name /
domain.mode transition rules, the type-vs-ldap and type-vs-oidc unions, the
type-gating of `mappings`/`groups` (OIDC) and `extraOptions` (LDAP), the
Default-domain rejection, the URL scheme patterns, the
providerMetadataURL-vs-endpoints exclusivity, the
project-vs-domain exclusivity on role assignments, and the domain-name /
identity-provider-name grammars.

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

OIDC-specific webhook rules: the fixed `clientSecret` data-key contract on
`clientSecretRef`, per-rule mapping completeness (at least one local and one
remote matcher with a non-empty type), `identityProviderName` uniqueness per
referenced Keystone, `remoteIDAttribute` uniformity across the OIDC siblings
of one Keystone, at most one introspection-enabled sibling, and a
config-injection guard over every value the proxy configuration or metadata
documents embed. The federation render re-validates those values (plus the
Secret-sourced client secret path) as the last line of defense, mirroring
the LDAP contract.

## Chainsaw E2E Tests

The rejection corpus lives in `tests/e2e/keystone/invalid-identitybackend-cr/`
(generated from `_generate.py`, guarded by `make verify-invalid-cr-fixtures`);
the end-to-end LDAP flow — in-suite OpenLDAP fixture, domain provisioning,
projection, token issuance for an LDAP user, and clean detach — lives in
`tests/e2e/keystone/ldap-domain-backend/`. The OIDC federation flow —
in-suite two-realm Keycloak fixture, federation-object provisioning, the
sidecar rollout, the bearer/CLI and browser websso flows, the multi-realm
proof, and clean detach — lives in `tests/e2e/keystone/oidc-federation/`;
chaos coverage (sidecar container-kill, IdP outage) lives in
`tests/e2e-chaos/keystone-federation/`. See
[Keystone E2E Test Suites](../testing/keystone-e2e-tests.md).

## Retained Artefacts

Like the config ConfigMaps, up to 3 historical content-hashed
`<keystone>-domains-<hash>` and `<keystone>-federation-<hash>` Secrets are
retained while backends of the matching type exist (fast rollback); when the
last backend of a type detaches every historical Secret of that type is
removed (no bind password, client secret, or crypto-passphrase copy may
linger), and all of them are garbage-collected with the owning Keystone CR.
The stable-named `<keystone>-oidc-crypto-passphrase` Secret holds the
operator-generated `OIDCCryptoPassphrase`; it is regenerable by design (a
rotation invalidates in-flight login sessions only) and therefore has no
OpenBao backup.
