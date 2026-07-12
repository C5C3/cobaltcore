---
title: Attach an OIDC Federation Backend to Keystone
quadrant: operator
---

# Attach an OIDC Federation Backend to Keystone

This guide walks through federating a running Keystone with an OpenID
Connect identity provider using the `KeystoneIdentityBackend` CRD: create
the client-secret Secret, apply one CR, watch the conditions converge, and
verify a federated user can log in. The worked example uses the same Keycloak
fixture the e2e suite deploys, and every value below is one that fixture
produces — so the whole flow is reproducible on the kind devstack.

When at least one OIDC backend attaches, the operator injects an
Apache/`mod_auth_openidc` reverse-proxy sidecar into the Keystone pod, binds
uWSGI to localhost behind it, and switches the Service to the proxy port.
Detaching the last OIDC backend restores the plain uWSGI-only pod — the
sidecar costs nothing until federation is in use.

For the full field reference, see the
[KeystoneIdentityBackend CRD API Reference](../reference/keystone/identity-backend-crd.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

::: warning The Keystone child is operator-owned
On a ControlPlane deployment the `controlplane-keystone` Keystone CR is
**projected** by the c5c3-operator, which re-asserts the entire
`spec.federation` block (proxy image and trusted dashboards) on every reconcile.
Set the federation proxy image on the `ControlPlane` CR (Step 3), and the trusted
dashboards flow from `spec.services.horizon` — see
[End-to-End SSO](./end-to-end-sso.md). The `KeystoneIdentityBackend` CR you apply
in Step 4 is yours to own; only the projected Keystone child's `spec.federation`
is operator-managed.
:::

- An OIDC identity provider reachable from the cluster (this guide uses a
  Keycloak realm) with a **confidential client** registered for keystone.
  The client's redirect URIs must cover
  `<keystone endpoint base>/v3/OS-FEDERATION/redirect_uri`.
- **A federation proxy image.** On the ControlPlane path the c5c3-operator
  projects `ghcr.io/c5c3/keystone-federation-proxy:latest` onto the child
  automatically, overridable via `spec.services.keystone.federationProxyImage`
  (Step 3); standalone Keystone installations set `spec.federation.proxyImage`
  themselves (see the [Standalone Keystone](#standalone-keystone-without-a-controlplane)
  section). Without a proxy image, OIDC backends stay pending with a
  `FederationProxyImageMissing` Warning — no hidden default is assumed.
- **Service users stay SQL-backed.** Federated users are ephemeral shadow
  users; OpenStack service accounts and the bootstrap admin remain in the
  SQL-backed `Default` domain, which the CRD hard-rejects federating.

## Step 1 — Deploy the fixture IdP (kind devstack)

On the kind devstack, stand up the same Keycloak the e2e suite uses. It is a
plain namespace-pinned manifest, so `kubectl apply` runs it verbatim:

```bash
kubectl apply -f tests/e2e/keystone/oidc-federation/00-keycloak.yaml
kubectl -n openstack rollout status deploy/keycloak
```

This provides, all in the `openstack` namespace:

| What | Value |
| --- | --- |
| Realm | `forge` |
| Issuer | `http://keycloak.openstack.svc.cluster.local:8080/realms/forge` |
| Confidential client | `keystone` (direct access grants + standard flow enabled) |
| Client secret | `keystone-forge-secret` (also shipped as Secret `keycloak-forge-client`) |
| Test user | `fry` / `fry-password` (group `/engineers`) |
| Keycloak admin | `admin` / `admin-password` |

The fixture pins `KC_HOSTNAME` to the cluster-internal issuer above, so tokens
carry a stable `iss` regardless of how you reach the pod. It also serves an
https listener on `8443` with a throwaway self-signed cert for the
introspection endpoint (mod_auth_openidc requires https there).

::: details Registering a client at your own IdP (non-kind)
On a non-kind cluster, skip the fixture and use your own Keycloak (or any OIDC
IdP): create (or pick) a realm, add a **confidential** client `keystone` with
*Standard flow* enabled and a client secret, and note the realm's issuer URL —
it must match the `iss` claim the IdP asserts, byte for byte. Substitute your
issuer, endpoints, and client secret for the fixture values below.
:::

## Step 2 — Create the client-secret Secret

The projection reads one fixed data key, `clientSecret`:

```bash
kubectl create secret generic keycloak-forge-client -n openstack \
  --from-literal=clientSecret='<the client secret from Keycloak>'
```

Rotating this Secret later re-renders the federation configuration
automatically — the operator watches it.

::: tip On the kind devstack
Step 1's fixture already ships the `keycloak-forge-client` Secret with the
fixed `clientSecret: keystone-forge-secret` key, so you can skip this
`kubectl create` — the backend CR in Step 4 references it as-is.
:::

## Step 3 — Pin the federation proxy image (optional)

On the ControlPlane path the c5c3-operator already projects
`ghcr.io/c5c3/keystone-federation-proxy:latest` onto the `controlplane-keystone`
child, so OIDC federation works out of the box — this step is only needed to pin
an immutable digest for production or to test a locally built sidecar. Override
the default on the `ControlPlane` CR:

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  services:
    keystone:
      federationProxyImage:
        repository: ghcr.io/c5c3/keystone-federation-proxy
        digest: sha256:<digest>   # pin a digest for production
```

::: warning Do not set `spec.federation.proxyImage` on the projected child
Editing `spec.federation.proxyImage` (or any `spec.federation` field) on the
`controlplane-keystone` child directly is reverted on the next reconcile: the
c5c3-operator re-asserts the whole `spec.federation` block from the ControlPlane.
Set the override via `spec.services.keystone.federationProxyImage` on the
`ControlPlane` CR instead.
:::

The projected image is inert until an OIDC backend attaches.

## Step 4 — Apply the backend CR

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: keycloak-forge
  namespace: openstack
spec:
  keystoneRef:
    name: controlplane-keystone
  domain:
    name: forge
    mode: Manage           # the operator creates the domain
    deletionPolicy: Retain # keep the domain when this CR is deleted
  type: OIDC
  oidc:
    issuer: http://keycloak.openstack.svc.cluster.local:8080/realms/forge
    clientID: keystone
    clientSecretRef:
      name: keycloak-forge-client
    # Explicit endpoints are required against the fixture: the operator's
    # metadata-fetch SSRF guard blocks discovery against the in-cluster
    # Keycloak's private ClusterIP, so the .well-known auto-discovery
    # (bare `issuer:` alone) does not run here. Everything speaks the fixture's
    # plain-http :8080 listener EXCEPT introspection, which mod_auth_openidc
    # requires to be https — the fixture serves it on the self-signed :8443
    # listener, and tlsVerify opts out of verifying that throwaway cert.
    # A publicly resolvable IdP can drop this block and rely on discovery.
    endpoints:
      authorizationEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/auth
      tokenEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/token
      jwksURI: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/certs
      userinfoEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/userinfo
      introspectionEndpoint: https://keycloak.openstack.svc.cluster.local:8443/realms/forge/protocol/openid-connect/token/introspect
    oauth2Introspection:
      enabled: true        # CLI clients may present IdP-issued bearer tokens
      tlsVerify: false     # the fixture's :8443 cert is a throwaway self-signed cert
  mappings:
  - remote:
    - type: HTTP_OIDC_ISS
      anyOneOf:
      - http://keycloak.openstack.svc.cluster.local:8080/realms/forge
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
NAME             READY   DOMAIN   KEYSTONE               AGE
keycloak-forge   True    forge    controlplane-keystone  1m
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

### CLI (bearer token) — reproducible on the kind devstack

This flow needs `oauth2Introspection` enabled on exactly one backend (Step 4
sets it). The fixture's `keystone` client has direct-access-grants enabled, so
you can mint a token with a username/password and exchange it — no browser
required. Port-forward Keycloak and obtain an access token:

```bash
kubectl -n openstack port-forward svc/keycloak 8080:8080 &

TOKEN=$(curl -s http://localhost:8080/realms/forge/protocol/openid-connect/token \
  -d grant_type=password -d client_id=keystone \
  -d client_secret=keystone-forge-secret \
  -d username=fry -d password=fry-password \
  -d scope=openid | jq -r .access_token)
```

`KC_HOSTNAME` pins the realm issuer to
`http://keycloak.openstack.svc.cluster.local:8080/realms/forge`, so a token
minted through the port-forward still carries the cluster-internal `iss` the
in-cluster proxy expects. Exchange the bearer for an unscoped Keystone token
against the devstack's published Keystone endpoint (`-k` for the devstack's
self-signed gateway cert, matching the Quick Start):

```bash
# The proxy introspects the bearer (in-cluster, over :8443) and keystone maps
# it to an unscoped token:
curl -sik -H "Authorization: Bearer $TOKEN" \
  "https://keystone.127-0-0-1.nip.io:8443/v3/OS-FEDERATION/identity_providers/keycloak-forge/protocols/openid/auth" \
  | grep -i x-subject-token
```

The unscoped token exchanges for a domain- or project-scoped one through the
regular `POST /v3/auth/tokens` — authorized by the roles the mapped group
carries.

### Browser (WebSSO)

Navigate to

```
<keystone endpoint base>/v3/auth/OS-FEDERATION/identity_providers/keycloak-forge/protocols/openid/websso?origin=<dashboard origin>
```

You are redirected to the Keycloak login form; after authenticating,
keystone answers with the auto-submitting token form that hands the token to
the dashboard. The `origin` must be listed in keystone's
`[federation] trusted_dashboard`. On a ControlPlane deployment the operator
projects this onto the child's typed `spec.federation.trustedDashboards` from
the dashboard's `spec.services.horizon` (`publicEndpoint` / `gateway`) — you do
not set it by hand; see [End-to-End SSO](./end-to-end-sso.md). On a standalone
Keystone, set `spec.federation.trustedDashboards` directly (see the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section).

::: warning The fixture IdP is not reachable from a host browser
The fixture Keycloak issuer is a cluster-internal Service name, so a browser on
your workstation cannot complete the login form against it — the browser
WebSSO flow needs an **externally reachable** IdP (your production Keycloak, or
a fixture published through the gateway with matching redirect URIs). The
headless authorization-code round trip against the in-cluster fixture is
exercised end-to-end by the mirroring e2e suite (see [Tested by](#tested-by));
the CLI bearer flow above is the copy-pasteable devstack path.
:::

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
| `IdentityBackendsReady=False` with a `FederationProxyImageMissing` Warning | The child Keystone CR has no `spec.federation.proxyImage`. On a ControlPlane deployment the operator always projects it, so this points to a **standalone** Keystone with no proxy image — set `spec.federation.proxyImage` on it (see the [Standalone Keystone](#standalone-keystone-without-a-controlplane) section). |
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

## Standalone Keystone, without a ControlPlane

On the [Quick Start](../quick-start.md) / [Quick Start (Extended)](../quick-start-extended.md)
devstacks a standalone Keystone CR named `keystone` runs with no ControlPlane
projecting it, so there is no operator projecting the federation block onto it —
you set it directly on the Keystone CR.

**Proxy image.** A standalone Keystone assumes no hidden default, so set
`spec.federation.proxyImage` yourself (mirroring the required `spec.image`).
Without it, OIDC backends stay pending with a `FederationProxyImageMissing`
Warning:

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

**Backend `keystoneRef`.** Point the `KeystoneIdentityBackend` from Step 4 at the
standalone CR by name — `keystoneRef.name: keystone` instead of
`controlplane-keystone`.

**Trusted dashboards.** Without the ControlPlane there is nothing to project the
WebSSO origin, so set `spec.federation.trustedDashboards` directly on the Keystone
CR. It is a list, rendered as one `[federation] trusted_dashboard` line per entry.
See the standalone section of [End-to-End SSO](./end-to-end-sso.md#standalone-keystone-without-a-controlplane)
for the full shape and the `spec.extraConfig` conflict rule.

## Tested by

Attaching the OIDC backend, watching the conditions converge, and the headless
CLI bearer flow are asserted end-to-end on the CI e2e kind cluster by this
chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e/keystone/oidc-federation
```

::: details The backend CR the suite applies
The suite isolates its Keystone instance from the parallel suite pool, so its
backend CR points `keystoneRef` at `keystone-oidc` (and enables
`oauth2Introspection` for the CLI bearer flow) — deliberately differing from the
`controlplane-keystone` reference used in the walkthrough above.

<<< @/../tests/e2e/keystone/oidc-federation/02-backend-cr.yaml#backend-cr
:::
