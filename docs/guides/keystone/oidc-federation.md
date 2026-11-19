---
title: Attach an OIDC Federation Backend to Keystone
quadrant: operator
---

# Attach an OIDC Federation Backend to Keystone

This guide uses the `KeystoneIdentityBackend` custom resource definition (CRD)
to federate a running Keystone with an OpenID Connect (OIDC) identity provider
(IdP). Create a client-secret Secret, apply one CR, watch the conditions
converge, and verify that a federated user can log in. The worked example uses
the same Keycloak fixture deployed by the end-to-end (e2e) suite and is
reproducible on the kind devstack.

When at least one OIDC backend attaches, the operator injects an Apache HTTP
Server sidecar with the `mod_auth_openidc` module, configures the uWSGI
application server to listen on localhost behind it, and switches the Service
target port to the proxy.
Detaching the last federation backend restores the plain uWSGI-only pod,
whether it used OIDC or Security Assertion Markup Language (SAML). The sidecar
runs only while federation is in use.

For the full field reference, see the
[KeystoneIdentityBackend CRD API Reference](../../reference/keystone/identity-backend-crd.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

- An OIDC identity provider reachable from the cluster (this guide uses a
  Keycloak realm) with a **confidential client** registered for keystone.
  For browser WebSSO, register the exact redirect URI
  `<keystone endpoint base>/v3/OS-FEDERATION/redirect_uri`. The kind fixture
  instead registers `http://keystone-oidc.openstack.svc.cluster.local:5000/*`
  for its e2e Keystone; this guide's CLI bearer flow does not use a redirect URI.
- **A managed Keystone workload and federation proxy image.** On a managed
  ControlPlane path the c5c3-operator
  projects `ghcr.io/c5c3/keystone-federation-proxy:latest` onto the child
  automatically, overridable via `spec.services.keystone.federationProxyImage`
  (Step 3); standalone Keystone installations set `spec.federation.proxyImage`
  themselves (see the [Standalone Keystone](#standalone-keystone-without-a-controlplane)
  section). A standalone Keystone with no proxy image leaves OIDC backends
  pending with a `FederationProxyImageMissing` Warning. An External-mode
  ControlPlane deploys no Keystone workload, so this sidecar-based guide does
  not apply to that mode.
- **Service users stay SQL-backed.** Federated users are ephemeral shadow
  users; OpenStack service accounts and the bootstrap admin remain in the
  SQL-backed `Default` domain, which the CRD hard-rejects federating.

## Step 1: Deploy the fixture IdP (kind devstack)

On the kind devstack, stand up the same Keycloak the e2e suite uses. It is a
plain namespace-pinned manifest, so `kubectl apply` runs it verbatim:

```bash
kubectl apply -f tests/e2e/keystone/oidc-federation/00-keycloak.yaml
kubectl -n openstack rollout status deploy/keycloak
```

The fixture imports two realms in the `openstack` namespace. This walkthrough
uses `cobaltcore`; `cobaltcore2` and its client Secret support the e2e
multi-IdP case. The walkthrough's values are:

| What | Value |
| --- | --- |
| Realm | `cobaltcore` |
| Issuer | `http://keycloak.openstack:8080/realms/cobaltcore` |
| Confidential client | `keystone` (direct access grants + standard flow enabled) |
| Client secret | `keystone-cobaltcore-secret` (also shipped as Secret `keycloak-cobaltcore-client`) |
| Test user | `fry` / `fry-password` (group `/engineers`) |
| Keycloak admin | `admin` / `admin-password` |

The fixture pins `KC_HOSTNAME` to the cluster-internal issuer above, so tokens
carry a stable `iss` regardless of how you reach the pod. It also serves an
https listener on `8443` with a throwaway self-signed cert for the
introspection endpoint (mod_auth_openidc requires https there).

::: details Registering a client at your own IdP (non-kind)
On a non-kind cluster, skip the fixture and use your own Keycloak (or any OIDC
IdP): create (or pick) a realm, add a **confidential** client `keystone` with
*Standard flow* enabled and a client secret, and note the realm's issuer URL.
It must match the `iss` claim the IdP asserts, byte for byte. Substitute your
issuer, endpoints, and client secret for the fixture values below.
:::

## Step 2: Create the client-secret Secret

The projection reads one fixed data key, `clientSecret`. Read the client secret
silently and pipe it to `kubectl` so it does not appear in shell history or the
command's arguments:

```bash
printf 'Keycloak client secret: '
IFS= read -r -s CLIENT_SECRET
printf '\n'
printf '%s' "$CLIENT_SECRET" |
  kubectl create secret generic keycloak-cobaltcore-client -n openstack \
    --from-file=clientSecret=/dev/stdin --dry-run=client -o yaml |
  kubectl apply -f -
unset CLIENT_SECRET
```

The operator watches this Secret and automatically re-renders the federation
configuration when it changes.

**On the kind devstack:** Step 1's fixture already ships the
`keycloak-cobaltcore-client` Secret with the fixed
`clientSecret: keystone-cobaltcore-secret` key. Skip this command; the backend
CR in Step 4 references that Secret.

## Step 3: Pin the federation proxy image (optional)

On the managed ControlPlane path the c5c3-operator already projects
`ghcr.io/c5c3/keystone-federation-proxy:latest` onto the `controlplane-keystone`
child, so OIDC federation works out of the box. Use this step to pin an immutable
digest for production or to test a locally built sidecar. Override
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

The sidecar's CPU and memory come from
`spec.sizing.keystone.federationProxy.resources` on the same CR, which the
operator projects onto the child's `spec.federation.proxyResources`.

**Do not edit the projected Keystone child.** Editing `spec.federation.proxyImage`
(or any `spec.federation` field, including `proxyResources`) on the
`controlplane-keystone` child is reverted on the next reconcile. Set these
values on the ControlPlane CR instead. The projected image is inert until a
federation backend, OIDC or SAML, attaches.

## Step 4: Apply the backend CR

On a managed ControlPlane deployment, the c5c3-operator owns the projected
Keystone child's `spec.federation` block and derives trusted dashboard origins
from `spec.services.horizon`; see [End-to-End SSO](../end-to-end-sso.md). The
`KeystoneIdentityBackend` CR you apply is yours to manage.

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: keycloak-cobaltcore
  namespace: openstack
spec:
  keystoneRef:
    name: controlplane-keystone
  domain:
    name: cobaltcore
    mode: Manage           # the operator creates the domain
    deletionPolicy: Retain # keep the domain when this CR is deleted
  type: OIDC
  oidc:
    issuer: http://keycloak.openstack:8080/realms/cobaltcore
    clientID: keystone
    clientSecretRef:
      name: keycloak-cobaltcore-client
    # Explicit endpoints are required against the fixture: the operator's
    # metadata-fetch SSRF guard blocks discovery against the in-cluster
    # Keycloak's private ClusterIP, so the .well-known auto-discovery
    # (bare `issuer:` alone) does not run here. Everything speaks the fixture's
    # plain-http :8080 listener except for introspection, which mod_auth_openidc
    # requires HTTPS. The fixture serves it on the self-signed :8443
    # listener, and tlsVerify opts out of verifying that throwaway cert.
    # A publicly resolvable IdP can drop this block and rely on discovery.
    endpoints:
      # The browser opens this endpoint, so it uses the issuer's host: Keycloak
      # posts the login form to that host, and a session cookie scoped to the
      # fully qualified domain name
      # would not be sent along (400, cookie_not_found).
      authorizationEndpoint: http://keycloak.openstack:8080/realms/cobaltcore/protocol/openid-connect/auth
      tokenEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/cobaltcore/protocol/openid-connect/token
      jwksURI: http://keycloak.openstack.svc.cluster.local:8080/realms/cobaltcore/protocol/openid-connect/certs
      userinfoEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/cobaltcore/protocol/openid-connect/userinfo
      introspectionEndpoint: https://keycloak.openstack.svc.cluster.local:8443/realms/cobaltcore/protocol/openid-connect/token/introspect
    oauth2Introspection:
      enabled: true        # CLI clients may present IdP-issued bearer tokens
      tlsVerify: false     # the fixture's :8443 cert is a throwaway self-signed cert
  mappings:
  - remote:
    - type: HTTP_OIDC_ISS
      anyOneOf:
      - http://keycloak.openstack:8080/realms/cobaltcore
    - type: HTTP_OIDC_PREFERRED_USERNAME
    local:
    - user:
        name: "{0}"        # {N} indexes only the value-producing remote rules
      group:
        name: federated-users
        domain:
          name: cobaltcore
  groups:
  - name: federated-users
    description: Federated Keycloak users
    roleAssignments:
    - role: member
      domain: true         # or scope to a project: {project: {name: demo}}
```

What the operator does with this:

- The dedicated backend controller provisions the domain, then the three
  keystone federation API objects: the identity provider `keycloak-cobaltcore`
  (remote ID = the issuer), the mapping `keycloak-cobaltcore-mapping` (your typed
  rules, drift-corrected on every change), and the protocol `openid` binding
  the two. It also creates the `federated-users` group and grants it
  `member` on the domain.
- The Keystone controller fetches the IdP's discovery document
  (`<issuer>/.well-known/openid-configuration` unless you set
  `providerMetadataURL` or spell out `endpoints` for air-gapped setups),
  renders the `mod_auth_openidc` configuration plus per-provider metadata
  into a content-hashed Secret, and rolls the Deployment with the sidecar.
- `keystone.conf` gains the `openid` auth method, the `[openid]`
  `remote_id_attribute`, and the WebSSO callback template.

The mapping's `remote[].type` entries are **full Web Server Gateway Interface
(WSGI) environment keys**: the
proxy passes claims as `OIDC-<claim>` headers, which uWSGI surfaces as
`HTTP_OIDC_<CLAIM>`. The operator strips these headers from inbound
requests (in both dash and underscore spelling) so in-cluster clients cannot
spoof claims past the module.

The `{N}` placeholders in `local` index only the **value-producing** remote
rules, in order. A rule carrying `anyOneOf`/`notAnyOf` is a pure condition
and is skipped. In the example above `HTTP_OIDC_ISS` only gates the rule, so
`HTTP_OIDC_PREFERRED_USERNAME` is `{0}`. Referencing an index past the last
value (e.g. `{1}` here) makes keystone reject the assertion with a
`DirectMappingError`.

## Step 5: Watch the conditions converge

```bash
kubectl get keystoneidentitybackends -n openstack
NAME             READY   DOMAIN   KEYSTONE               AGE
keycloak-cobaltcore   True    cobaltcore    controlplane-keystone  1m
```

`kubectl describe` shows the progression: `DomainReady=True`, then
`FederationObjectsReady=True` (identity provider + protocol upserted) and
`MappingsReady=True` (mapping, groups, role assignments applied), then
`ConfigProjected=True` (the sidecar mounts this backend's client document),
then `Ready=True`. The Keystone CR aggregates all attached backends via its
`IdentityBackendsReady` condition.

The figure shows that order for every satellite kind. Its gate,
`CredentialsReady`, is `DomainReady` on an identity backend.
[The handshake](../../reference/backend/kubernetes-packages.md#satellite-handshake)
lists the five steps and what differs per kind.

![The handshake between a satellite resource and the service it attaches to, in five numbered steps across two controllers. 1: the satellite controller checks the credentials and sets CredentialsReady on the satellite. 2: the aggregation step of the service controller reads only that condition. 3: it renders one section per satellite that passed into a Secret whose name carries a hash of its content. 4: the pod template of the service's Deployment mounts that Secret, and a new name rolls the pods. 5: the satellite controller finds its section in the mounted Secret and sets ConfigProjected. Ready turns True once both conditions are. An arrow marked never runs from Ready to the aggregation step: reading Ready there would deadlock, because Ready needs ConfigProjected, which needs that step. On a KeystoneIdentityBackend the gate is DomainReady, and ConfigProjected also waits until the rollout has finished.](../../diagrams/service-satellite-handshake.svg)

The Deployment now runs two containers (`keystone`, `federation-proxy`). The
Service's targetPort points at the proxy; the API endpoint remains unchanged.

## Step 6: Log in as a federated user {#step-6-log-in-as-a-federated-user}

### CLI (bearer token): reproducible on the kind devstack

This flow needs `oauth2Introspection` enabled on exactly one backend (Step 4
sets it). The fixture's `keystone` client has direct-access-grants enabled, so
you can request a token with a username and password without a browser.
This command uses only the fixture's disposable credentials; do not adapt it
for production credentials. Port-forward Keycloak and obtain an access
token:

```bash
kubectl -n openstack port-forward svc/keycloak 8080:8080 &

printf 'Keycloak client secret: '
IFS= read -r -s CLIENT_SECRET
printf '\n'
TOKEN=$(printf '%s' "$CLIENT_SECRET" |
  curl -fsS http://localhost:8080/realms/cobaltcore/protocol/openid-connect/token \
  -d grant_type=password -d client_id=keystone \
  --data-urlencode client_secret@- \
  -d username=fry -d password=fry-password \
  -d scope=openid | jq -er .access_token)
unset CLIENT_SECRET
```

`KC_HOSTNAME` pins the realm issuer to
`http://keycloak.openstack:8080/realms/cobaltcore`, so a token
minted through the port-forward still carries the cluster-internal `iss` the
in-cluster proxy expects. Exchange the bearer for an unscoped Keystone token
against the devstack's published Keystone endpoint (`-k` for the devstack's
self-signed gateway cert, matching the Quick Start):

```bash
# The proxy introspects the bearer (in-cluster, over :8443) and keystone maps
# it to an unscoped token:
curl -fsSik -H "Authorization: Bearer $TOKEN" \
  "https://keystone.127-0-0-1.nip.io:8443/v3/OS-FEDERATION/identity_providers/keycloak-cobaltcore/protocols/openid/auth" \
  | grep -i x-subject-token
```

The unscoped token exchanges for a domain- or project-scoped token through
`POST /v3/auth/tokens`, where the mapped group's roles authorize the request.

### Browser (WebSSO)

Navigate to

```
<keystone endpoint base>/v3/auth/OS-FEDERATION/identity_providers/keycloak-cobaltcore/protocols/openid/websso?origin=<dashboard origin>
```

You are redirected to the Keycloak login form; after authenticating,
keystone answers with the auto-submitting token form that hands the token to
the dashboard. The `origin` must be listed in keystone's
`[federation] trusted_dashboard`. On a managed ControlPlane deployment the operator
projects this onto the child's typed `spec.federation.trustedDashboards` from
the dashboard's `spec.services.horizon` (`publicEndpoint` and `gateway`). You
do not set it by hand; see [End-to-End SSO](../end-to-end-sso.md). On a standalone
Keystone, set `spec.federation.trustedDashboards` directly (see the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section).

The figure shows this login with Horizon as the dashboard. The URL above is
hop 2. [The hops of a login](../end-to-end-sso.md#login-hops) lists all eight.

![A federated login through Horizon in eight numbered hops. Hop 1: the browser opens the login page of Horizon through the Gateway and picks an identity provider, and Horizon answers with a redirect. Hop 2: the browser follows it to the websso path of that provider on the public Keystone URL and passes the dashboard origin. The Service of Keystone sends every request to the federation-proxy container of the Keystone pod on port 5050, which redirects a browser without a session to the identity provider. Hop 3: the browser opens the authorization endpoint of the identity provider, and the user logs in there. Hop 4: the identity provider sends the browser back to /v3/OS-FEDERATION/redirect_uri on the public Keystone URL, with a code. Hop 5: the proxy exchanges the code at the token endpoint of the identity provider, a call that leaves the pod. Hop 6: the proxy passes the request to Keystone on 127.0.0.1:5000 with the claims as request headers, and Keystone answers with a form that carries a token, if the origin is a trusted dashboard. Hop 7: the browser posts that form to the origin, the path /auth/websso/ of Horizon. Hop 8: Horizon validates the token against the cluster-local Keystone URL and starts the session. Hops 1 to 4 and 7 are requests of the browser and use public names; hops 5 and 8 are calls between servers and use names a pod resolves. A SAML backend differs in hops 4 and 5: the browser posts the assertion to the proxy, and no call to the identity provider follows.](../../diagrams/service-federated-login.svg)

**Browser limitation for the fixture:** its Keycloak issuer is a cluster-internal
Service name, so a browser on your workstation cannot complete the login form.
The WebSSO flow needs an **externally reachable** IdP, such as a production
Keycloak or a fixture published through the gateway with matching redirect
URIs. The mirroring e2e suite exercises the headless authorization-code round
trip against the in-cluster fixture (see [Tested by](#tested-by)); the CLI bearer
flow above is the copy-pasteable devstack path.

## Multiple identity providers

Attach one `KeystoneIdentityBackend` per realm/IdP; the shared sidecar
serves all of them from one metadata directory, and each per-IdP websso path
pins its own issuer. Two constraints apply across the OIDC backends of one
Keystone (webhook-enforced): `remoteIDAttribute` must be uniform, and at
most one backend may enable `oauth2Introspection`; the module's `OIDCOAuth*`
resource-server directives are server-scoped. The global
`/v3/auth/OS-FEDERATION/websso/<protocol>` path exists only for a protocol ID
that one backend of this Keystone uses by itself. Two backends with the same
protocol ID get none for it; use the per-IdP paths.

## Deleting a backend

`kubectl delete keystoneidentitybackend keycloak-cobaltcore` de-projects the
configuration first. With the last federation backend, OIDC or SAML, the sidecar
disappears and the Service returns to uWSGI. The controller always removes the
protocol, mapping, and identity provider, then applies
`spec.domain.deletionPolicy` to the domain like the LDAP flow.
Declarative groups live inside the domain and follow it.

## Discovery against an in-cluster identity provider

By default the operator blocks loopback, link-local, private, carrier-grade NAT,
NAT64, unspecified, and multicast addresses when it fetches an OIDC discovery
document or SAML IdP metadata. This server-side request forgery (SSRF) guard
prevents auto-discovery of `<issuer>/.well-known/openid-configuration` from
reaching an IdP on a private in-cluster address, such as the fixture Keycloak's
ClusterIP in the kind Service CIDR. Step 4 spells out `spec.oidc.endpoints`;
explicit endpoints skip the metadata fetch entirely and are the recommended
path for air-gapped setups and wherever the allowlist is not wanted.

To let discovery run against a **trusted** in-cluster IdP, add its address range
to the operator chart's `federation.metadataAllowCidrs` value. On a Flux-managed
cluster, add this under `spec.values` in the `keystone-operator` HelmRelease.
In this repository, that file is
`deploy/flux-system/releases/keystone-operator.yaml`; let Flux apply the change.
Do not use `helm upgrade` against a release managed by Flux. For a directly
managed Helm release, set the same value in the values file used to manage it.

```yaml
federation:
  metadataAllowCidrs:
    - 10.96.0.0/12   # the cluster's Service CIDR
```

With the Service CIDR allowlisted, an OIDC backend can omit
`spec.oidc.endpoints` and rely on `issuer:`. The operator fetches the discovery
document from the in-cluster IdP.

::: warning Scope the allowlist narrowly
Allowlist the **smallest** range that reaches the IdP: the cluster's Service
CIDR (e.g. `10.96.0.0/12`). A default route (`0.0.0.0/0`, `::/0`) is rejected at
startup. The allowlist is operator-level configuration and cannot be set or
widened from a `KeystoneIdentityBackend` CR, so a tenant cannot expand the
operator's reachable surface. The allowlist lifts blocks only for private,
carrier-grade NAT, and NAT64 ranges. Loopback, link-local (including the cloud
instance metadata service (IMDS) at `169.254.169.254`), multicast, and
unspecified addresses stay blocked even when an entry covers them.

A tenant can still aim `spec.oidc.providerMetadataURL` at any address **inside**
the range you allowlist, so keep the range to what the IdP actually needs.
Fetch failures (refused connection, timeout, HTTP status, or unparseable body)
produce a generic tenant-visible message; the operator log records the
underlying cause.
:::

**NetworkPolicy limitation:** the keystone-operator chart's opt-in
NetworkPolicy (`networkPolicy.enabled=true`) default-denies egress except to the
configured kube-apiserver and DNS, plus TCP ports 5000 and 5050 on Keystone
pods for health checks. It blocks metadata fetches at the network layer,
including to public or allowlisted addresses. With this policy enabled, use
explicit `spec.oidc.endpoints`; discovery fails even for public IdPs.

## Security considerations

- **Register an exact redirect URI at the IdP.** mod_auth_openidc derives the
  absolute `redirect_uri` from the request URL. With an exact registered URI,
  host-header manipulation causes login to fail. Avoid wildcard redirect URIs
  in production.
- **Expose Keystone through `spec.gateway` when a proxy sits in front.** The
  sidecar honors inbound `X-Forwarded-Host` / `X-Forwarded-Proto` for URL
  computation only when `spec.gateway` is set; that Gateway is the trust
  boundary and **must overwrite** these headers so an in-cluster client cannot
  spoof them. With no declared gateway the headers are ignored and the sidecar
  falls back to the request host.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| `IdentityBackendsReady=False` with a `FederationProxyImageMissing` Warning | The child Keystone CR has no `spec.federation.proxyImage`. The ControlPlane always projects this field, so the issue points to a standalone Keystone. Set `spec.federation.proxyImage` on it (see the [Standalone Keystone](#standalone-keystone-without-a-controlplane) section). |
| `IdentityBackendSkipped` Warning naming `provider metadata unavailable` | The discovery document could not be fetched from the operator pod, or its `issuer` does not equal `spec.oidc.issuer`. The Event hides the underlying cause; the operator log may include `refusing to dial non-public address`. Check egress/DNS, spell out `spec.oidc.endpoints`, or allowlist a trusted in-cluster IdP's CIDR with the `federation.metadataAllowCidrs` chart value (see [Discovery against an in-cluster identity provider](#discovery-against-an-in-cluster-identity-provider)). |
| `FederationObjectsReady=False/NoMappingRules` | `spec.mappings` is empty. Keystone cannot represent a rule-less mapping; add at least one rule. |
| `MappingsReady=False/RoleOrProjectNotFound` | A role assignment references a role or project that does not exist (yet); the backend retries on a bounded poll. |
| Federated login returns 401 with valid IdP credentials | The mapping did not match: compare the asserted claims (the sidecar logs them at debug) against your `remote[].type` matchers, and remember the issuer gate must equal the `iss` claim byte for byte. |
| WebSSO ends in `401`/`403` at the final hop | The `origin` parameter is not listed in `[federation] trusted_dashboard`. |
| Bearer-token auth returns non-2xx while browser login works | `oauth2Introspection` is not enabled on this backend, or another backend already holds the single introspection slot. |
| Bearer-token auth returns 401 and introspection yields `active:false` for a fresh token | The IdP does not list the resource-server client in the token's `aud`. Recent Keycloak (>= 25) rejects introspection when the introspecting client is absent from the audience. Add an audience mapper or client scope so the access token's `aud` includes the `clientID`. |
| Bearer-token auth returns 401 with `Access token JWT check failed` in the IdP log | The token's `iss` differs from what the IdP computes at the introspection endpoint (e.g. tokens minted over one hostname/scheme, introspected over another). Pin a fixed frontend URL / hostname on the IdP so the issuer is stable across listeners. |
| The backend is skipped with `is not https` in the Warning | The IdP publishes an http introspection endpoint, which mod_auth_openidc refuses. Point `endpoints.introspectionEndpoint` at an https listener (and set `oauth2Introspection.tlsVerify: false` if its certificate is not in the system trust store). |
| Admission rejects the CR | The message names the exact rule: discovery-shape exclusivity, the fixed `clientSecret` data-key contract, mapping-rule completeness, identity-provider-name uniqueness, remote-id uniformity, or the single-introspection limit. |

## Standalone Keystone, without a ControlPlane

On the [Quick Start](../../quick-start.md) / [Quick Start (Extended)](../../quick-start-extended.md)
devstacks a standalone Keystone CR named `keystone` runs without a ControlPlane
projection. Set the federation block directly on the Keystone CR.

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
standalone CR by name: set `keystoneRef.name: keystone`.

**Trusted dashboards.** Without the ControlPlane there is nothing to project the
WebSSO origin, so set `spec.federation.trustedDashboards` directly on the Keystone
CR. It is a list, rendered as one `[federation] trusted_dashboard` line per entry.
See the standalone section of [End-to-End SSO](../end-to-end-sso.md#standalone-keystone-without-a-controlplane)
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
backend CR points `keystoneRef` at `keystone-oidc` and enables
`oauth2Introspection` for the CLI bearer flow. The walkthrough uses
`controlplane-keystone`.

<<< ../../../tests/e2e/keystone/oidc-federation/02-backend-cr.yaml#backend-cr
:::
