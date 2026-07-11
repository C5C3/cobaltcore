---
title: Attach an OIDC Federation Backend to Keystone
quadrant: operator
---

# Attach an OIDC Federation Backend to Keystone

This guide walks through federating a running Keystone with an OpenID
Connect identity provider using the `KeystoneIdentityBackend` CRD: create
the client-secret Secret, apply one CR, watch the conditions converge, and
verify a federated user can log in. The worked example uses Keycloak — the
same fixture the e2e suite deploys — so every value is adaptable to a kind
cluster.

When at least one OIDC backend attaches, the operator injects an
Apache/`mod_auth_openidc` reverse-proxy sidecar into the Keystone pod, binds
uWSGI to localhost behind it, and switches the Service to the proxy port.
Detaching the last OIDC backend restores the plain uWSGI-only pod — the
sidecar costs nothing until federation is in use.

For the full field reference, see the
[KeystoneIdentityBackend CRD API Reference](../reference/keystone/identity-backend-crd.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a Keystone CR named
`keystone` is `Ready` in the `openstack` namespace. Every resource name in the
examples below is one that devstack produces.
:::

- An OIDC identity provider reachable from the cluster (this guide uses a
  Keycloak realm) with a **confidential client** registered for keystone.
  The client's redirect URIs must cover
  `<keystone endpoint base>/v3/OS-FEDERATION/redirect_uri`.
- **A federation proxy image.** The managed ControlPlane path projects
  `ghcr.io/c5c3/keystone-federation-proxy` automatically; standalone
  Keystone installations set `spec.federation.proxyImage` themselves
  (mirroring the required `spec.image`). Without it, OIDC backends stay
  pending with a `FederationProxyImageMissing` Warning — no hidden default
  is assumed.
- **Service users stay SQL-backed.** Federated users are ephemeral shadow
  users; OpenStack service accounts and the bootstrap admin remain in the
  SQL-backed `Default` domain, which the CRD hard-rejects federating.

## Step 1 — Register the client at the identity provider

In Keycloak: create (or pick) a realm — say `forge` — and add a confidential
client `keystone` with *Standard flow* enabled and a client secret. Note the
realm's issuer URL (`https://keycloak.example.com/realms/forge`); it must
match the `iss` claim the IdP asserts, byte for byte.

## Step 2 — Create the client-secret Secret

The projection reads one fixed data key, `clientSecret`:

```bash
kubectl create secret generic keycloak-forge-client -n openstack \
  --from-literal=clientSecret='<the client secret from Keycloak>'
```

Rotating this Secret later re-renders the federation configuration
automatically — the operator watches it.

## Step 3 — Configure the proxy image (standalone installations)

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
spec:
  # ...
  federation:
    proxyImage:
      repository: ghcr.io/c5c3/keystone-federation-proxy
      tag: latest   # pin a digest for production
```

This block is inert until an OIDC backend attaches.

## Step 4 — Apply the backend CR

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
    mode: Manage           # the operator creates the domain
    deletionPolicy: Retain # keep the domain when this CR is deleted
  type: OIDC
  oidc:
    issuer: https://keycloak.example.com/realms/forge
    clientID: keystone
    clientSecretRef:
      name: keycloak-forge-client
    oauth2Introspection:
      enabled: true        # CLI clients may present IdP-issued bearer tokens
  mappings:
  - remote:
    - type: HTTP_OIDC_ISS
      anyOneOf:
      - https://keycloak.example.com/realms/forge
    - type: HTTP_OIDC_PREFERRED_USERNAME
    local:
    - user:
        name: "{0}"        # {N} indexes only the value-producing remote rules
      group:
        name: federated-users
        domain:
          name: forge
  groups:
  - name: federated-users
    description: Federated Keycloak users
    roleAssignments:
    - role: member
      domain: true         # or scope to a project: {project: {name: demo}}
```

What the operator does with this:

- The dedicated backend controller provisions the domain, then the three
  keystone federation API objects: the identity provider `keycloak-forge`
  (remote ID = the issuer), the mapping `keycloak-forge-mapping` (your typed
  rules, drift-corrected on every change), and the protocol `openid` binding
  the two. It also creates the `federated-users` group and grants it
  `member` on the domain.
- The keystone-side projection fetches the IdP's discovery document
  (`<issuer>/.well-known/openid-configuration` unless you set
  `providerMetadataURL` or spell out `endpoints` for air-gapped setups),
  renders the `mod_auth_openidc` configuration plus per-provider metadata
  into a content-hashed Secret, and rolls the Deployment with the sidecar.
- `keystone.conf` gains the `openid` auth method, the `[openid]`
  `remote_id_attribute`, and the WebSSO callback template.

The mapping's `remote[].type` entries are **full WSGI environ keys**: the
proxy passes claims as `OIDC-<claim>` headers, which uWSGI surfaces as
`HTTP_OIDC_<CLAIM>`. The operator strips exactly these headers from inbound
requests (in both dash and underscore spelling) so in-cluster clients cannot
spoof claims past the module.

The `{N}` placeholders in `local` index only the **value-producing** remote
rules, in order — a rule carrying `anyOneOf`/`notAnyOf` is a pure condition
and is skipped. In the example above `HTTP_OIDC_ISS` only gates the rule, so
`HTTP_OIDC_PREFERRED_USERNAME` is `{0}`. Referencing an index past the last
value (e.g. `{1}` here) makes keystone reject the assertion with a
`DirectMappingError`.

## Step 5 — Watch the conditions converge

```bash
kubectl get keystoneidentitybackends -n openstack
NAME             READY   DOMAIN   KEYSTONE   AGE
keycloak-forge   True    forge    keystone   1m
```

`kubectl describe` shows the progression: `DomainReady=True`, then
`FederationObjectsReady=True` (identity provider + protocol upserted) and
`MappingsReady=True` (mapping, groups, role assignments applied), then
`ConfigProjected=True` (the sidecar mounts this backend's client document),
then `Ready=True`. The Keystone CR aggregates all attached backends via its
`IdentityBackendsReady` condition.

The Deployment now runs two containers (`keystone`, `federation-proxy`) and
the Service's targetPort points at the proxy — the API endpoint itself is
unchanged.

## Step 6 — Log in as a federated user

Browser (WebSSO): navigate to

```
<keystone endpoint base>/v3/auth/OS-FEDERATION/identity_providers/keycloak-forge/protocols/openid/websso?origin=<dashboard origin>
```

You are redirected to the Keycloak login form; after authenticating,
keystone answers with the auto-submitting token form that hands the token to
the dashboard. The `origin` must be listed in keystone's
`[federation] trusted_dashboard` (set it via `spec.extraConfig` until the
Horizon phase ships the typed field).

CLI (bearer token, needs `oauth2Introspection` enabled on exactly one
backend): obtain an access token from the IdP, then exchange it:

```bash
TOKEN=$(curl -s https://keycloak.example.com/realms/forge/protocol/openid-connect/token \
  -d grant_type=password -d client_id=keystone \
  -d client_secret=<secret> -d username=<user> -d password=<pw> \
  -d scope=openid | jq -r .access_token)

# The proxy introspects the bearer and keystone maps it to an unscoped token:
curl -si -H "Authorization: Bearer $TOKEN" \
  "<keystone endpoint base>/v3/OS-FEDERATION/identity_providers/keycloak-forge/protocols/openid/auth" \
  | grep -i x-subject-token
```

The unscoped token exchanges for a domain- or project-scoped one through the
regular `POST /v3/auth/tokens` — authorized by the roles the mapped group
carries.

## Multiple identity providers

Attach one `KeystoneIdentityBackend` per realm/IdP; the shared sidecar
serves all of them from one metadata directory, and each per-IdP websso path
pins its own issuer. Two constraints apply across the OIDC backends of one
Keystone (webhook-enforced): `remoteIDAttribute` must be uniform, and at
most one backend may enable `oauth2Introspection` — the module's `OIDCOAuth*`
resource-server directives are server-scoped. With more than one backend the
global `/v3/auth/OS-FEDERATION/websso/<protocol>` path is not pinned to any
provider; use the per-IdP paths.

## Deleting a backend

`kubectl delete keystoneidentitybackend keycloak-forge` de-projects the
configuration first (with the last OIDC backend the sidecar disappears and
the Service returns to uWSGI), then removes the protocol, mapping, and
identity provider — always — and finally applies
`spec.domain.deletionPolicy` to the domain exactly like the LDAP flow.
Declarative groups live inside the domain and follow it.

## Security considerations

- **Register an exact redirect URI at the IdP.** mod_auth_openidc derives the
  absolute `redirect_uri` from the request URL, so a strictly-registered
  client turns any host-header manipulation into a login failure rather than a
  usable redirect. Avoid wildcard redirect URIs in production.
- **Expose Keystone through `spec.gateway` when a proxy sits in front.** The
  sidecar honors inbound `X-Forwarded-Host` / `X-Forwarded-Proto` for URL
  computation only when `spec.gateway` is set; that Gateway is the trust
  boundary and **must overwrite** these headers so an in-cluster client cannot
  spoof them. With no declared gateway the headers are ignored and the sidecar
  falls back to the request host.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `IdentityBackendsReady=False` with a `FederationProxyImageMissing` Warning | `spec.federation.proxyImage` is not set on the Keystone CR — configure the sidecar image (Step 3). |
| `IdentityBackendsSkipped` Warning naming `provider metadata unavailable` | The discovery document could not be fetched from the operator pod, or its `issuer` does not equal `spec.oidc.issuer`. Check egress/DNS, or spell out `spec.oidc.endpoints`. |
| `FederationObjectsReady=False/NoMappingRules` | `spec.mappings` is empty — keystone cannot represent a rule-less mapping; add at least one rule. |
| `MappingsReady=False/RoleOrProjectNotFound` | A role assignment references a role or project that does not exist (yet); the backend retries on a bounded poll. |
| Federated login returns 401 with valid IdP credentials | The mapping did not match: compare the asserted claims (the sidecar logs them at debug) against your `remote[].type` matchers, and remember the issuer gate must equal the `iss` claim byte for byte. |
| WebSSO ends in `401`/`403` at the final hop | The `origin` parameter is not listed in `[federation] trusted_dashboard`. |
| Bearer-token auth returns non-2xx while browser login works | `oauth2Introspection` is not enabled on this backend, or another backend already holds the single introspection slot. |
| Bearer-token auth returns 401 and introspection yields `active:false` for a fresh token | The IdP does not list the resource-server client in the token's `aud`. Recent Keycloak (>= 25) rejects introspection when the introspecting client is absent from the audience — add an audience mapper (or client scope) so the access token's `aud` includes the `clientID`. |
| Bearer-token auth returns 401 with `Access token JWT check failed` in the IdP log | The token's `iss` differs from what the IdP computes at the introspection endpoint (e.g. tokens minted over one hostname/scheme, introspected over another). Pin a fixed frontend URL / hostname on the IdP so the issuer is stable across listeners. |
| The backend is skipped with `is not https` in the Warning | The IdP publishes an http introspection endpoint, which mod_auth_openidc refuses. Point `endpoints.introspectionEndpoint` at an https listener (and set `oauth2Introspection.tlsVerify: false` if its certificate is not in the system trust store). |
| Admission rejects the CR | The message names the exact rule: discovery-shape exclusivity, the fixed `clientSecret` data-key contract, mapping-rule completeness, identity-provider-name uniqueness, remote-id uniformity, or the single-introspection limit. |
