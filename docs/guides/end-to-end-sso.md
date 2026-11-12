---
title: End-to-End SSO
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# End-to-End SSO

This guide projects an SSO choice onto a working ControlPlane's Horizon login
page, walks through the browser flow with a host-reachable identity provider,
and adds a domain field for users who live in an LDAP-backed domain.

It assumes you already know how to attach identity backends. If you do not,
read [Attach an OIDC Federation Backend](./keystone/oidc-federation.md) and
[Attach an LDAP Domain Backend](./keystone/ldap-domain-backend.md) first. This guide
continues that workflow and shows what the ControlPlane does with those backends.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so the ControlPlane's
projected `controlplane-keystone` and `controlplane-horizon` children are
running. Every resource name in the examples below is one that devstack produces.
:::

On the kind devstack, stand up the fixture IdP and LDAP directory this guide
federates against. These are the fixtures of the mirroring e2e suite. Its
Keycloak registers the gateway host of Keystone as a redirect URI, which is where
the identity provider sends the browser back in hop 4 of
[the login](#login-hops):

```bash
kubectl apply -f tests/e2e-controlplane-sso/00-keycloak.yaml \
              -f tests/e2e-controlplane-sso/01-openldap.yaml
kubectl -n openstack rollout status deploy/keycloak
kubectl -n openstack rollout status deploy/openldap
```

If you previously applied the `oidc-federation` guide's copy of the Keycloak
fixture, re-apply the manifest above and restart the pod. Keycloak imports its
realms at pod start, and this fixture includes the gateway redirect URIs:

```bash
kubectl -n openstack rollout restart deploy/keycloak
```

## What the ControlPlane does for you

Attaching a `KeystoneIdentityBackend` to a ControlPlane's Keystone child is the
only action you take. The ControlPlane operator watches those backends and, for
every one that reaches `Ready`, projects:

- a **WebSSO choice** onto the Horizon child (`spec.websso`), so the login page
  gains an entry in its "Authenticate using" dropdown for each OIDC backend;
- a **domain field** onto the Horizon child (`spec.multiDomain`), once any
  LDAP-backed domain is in play, unless `services.horizon.multiDomain` is
  enabled (Step 4);
- the **trusted dashboard origin** onto the Keystone child
  (`spec.federation.trustedDashboards`), so Keystone accepts the token hand-off
  back to your dashboard. This one waits for no backend: the operator derives it
  from `services.horizon` on every pass.

An OIDC backend must reach `Ready` before it adds a new SSO choice, and an LDAP
backend must reach `Ready` before it adds the domain field. If an already
projected backend becomes temporarily unhealthy while it remains attached, the
ControlPlane retains the current Horizon block. Detaching the last backend of
that type clears its block; the login page returns to local credentials when
the last OIDC backend is detached. An explicitly enabled
`services.horizon.multiDomain` keeps its domain field after an LDAP backend is
detached.

The SSO choices require both services to be published (Step 1). Until then the
operator projects no `spec.websso`, even for a `Ready` backend, and logs why.
The local login form stays available alongside federated choices for the
bootstrap admin and LDAP-domain users.

## Step 1: Publish both services

The WebSSO hand-off is a browser flow, so both Keystone and the dashboard must
be reachable under the names the browser uses.

```yaml
apiVersion: c5c3.io/v1alpha1
kind: ControlPlane
metadata:
  name: controlplane
  namespace: openstack
spec:
  services:
    keystone:
      gateway:
        hostname: keystone.example.com
        parentRef:
          name: openstack-gw
      # Use the externally routable Keystone URL, not the cluster-local Service URL.
      publicEndpoint: https://keystone.example.com/v3
    horizon:
      gateway:
        hostname: horizon.example.com
        parentRef:
          name: openstack-gw
      # The browser-observed dashboard base URL. The operator derives the
      # trusted WebSSO origin from it: publicEndpoint + "/auth/websso/".
      publicEndpoint: https://horizon.example.com
```

::: warning Keystone matches the origin verbatim
Keystone compares the origin the dashboard sends against its
`[federation] trusted_dashboard` list character for character. Three rules
follow:

- **`publicEndpoint` must include a non-default port.** If you publish the
  dashboard on `https://horizon.example.com:8443`, say so. When
  `publicEndpoint` is empty the operator derives
  `https://{gateway.hostname}` on port 443. The hand-off is rejected on any
  other port.
- **`publicEndpoint` and `gateway.hostname` must name the same host.** Django
  derives the origin it sends from the request's `Host` header, which carries
  the gateway hostname. If `publicEndpoint` names a different host, the
  validating webhook rejects the ControlPlane. The port may still
  differ, since Gateway API hostnames carry none.
- **`publicEndpoint` must use `https` behind a gateway.** The Gateway listener
  terminates TLS, and after every federated login the browser posts the
  unscoped WebSSO token to this origin, in a form Keystone hands it. Over `http`
  that bearer token carries the user's full API privileges. Over `http` it
  travels in cleartext, so the validating webhook rejects it. Without a gateway the value
  is only warned about.
:::

## Step 2: Attach a federation backend

Apply an OIDC `KeystoneIdentityBackend` pointing at your ControlPlane's Keystone
child. The child is named `{controlplane-name}-keystone`:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: keycloak
  namespace: openstack
spec:
  keystoneRef:
    name: controlplane-keystone
  domain:
    name: federated
  type: OIDC
  oidc:
    # Fixture-true values from the two-fixture apply in Prerequisites. Explicit
    # endpoints are required because the operator's metadata-fetch SSRF guard
    # blocks .well-known discovery against the in-cluster Keycloak. See
    # [Attach an OIDC Federation Backend](./keystone/oidc-federation.md) Step 4 for the
    # full worked example (mappings, groups, and why introspection is https).
    issuer: http://keycloak.openstack:8080/realms/cobaltcore
    clientID: keystone
    clientSecretRef:
      name: keycloak-cobaltcore-client
    endpoints:
      # The browser opens this endpoint, so it uses the issuer's host: Keycloak
      # posts the login form to that host, and a session cookie set on the FQDN
      # would not be sent along (400, cookie_not_found).
      authorizationEndpoint: http://keycloak.openstack:8080/realms/cobaltcore/protocol/openid-connect/auth
      tokenEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/cobaltcore/protocol/openid-connect/token
      jwksURI: http://keycloak.openstack.svc.cluster.local:8080/realms/cobaltcore/protocol/openid-connect/certs
      userinfoEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/cobaltcore/protocol/openid-connect/userinfo
      introspectionEndpoint: https://keycloak.openstack.svc.cluster.local:8443/realms/cobaltcore/protocol/openid-connect/token/introspect
    oauth2Introspection:
      enabled: true
      tlsVerify: false
```

**Use a separate domain for federation.** The `Default` domain hosts the
SQL-backed service users and bootstrap admin, so a federation backend may not
back it.

Once the backend reports `Ready`, watch the projection appear on the Horizon
child without touching it:

```bash
kubectl get horizon controlplane-horizon -n openstack -o jsonpath='{.spec.websso}' | jq
```

```json
{
  "enabled": true,
  "initialChoice": "credentials",
  "keystoneURL": "https://keystone.example.com/v3",
  "choices": [
    { "id": "credentials", "label": "Keystone Credentials" },
    { "id": "keycloak_openid", "label": "keycloak" }
  ],
  "idpMapping": {
    "keycloak_openid": { "identityProvider": "keycloak", "protocol": "openid" }
  }
}
```

The `credentials` entry leads the list and is preselected. The operator always
offers it alongside federated choices, preserving local access for the bootstrap
admin and LDAP-domain users.

## Step 3: Complete a login

The bundled Keycloak fixture has a cluster-internal issuer, so a browser on
your workstation cannot reach its login page. To complete this flow in a
browser, use a host-reachable IdP and substitute its issuer, endpoints, client
secret, and matching redirect URIs as described in the linked OIDC guide. The
fixture verifies the projection here and exercises the full hand-off headlessly
in the e2e suite.

With a host-reachable IdP configured, open
`https://horizon.example.com/auth/login/`. The "Authenticate using" dropdown
offers it; select it, authenticate at the provider, and return to the dashboard.

### The hops of a login {#login-hops}

The figure follows one login with an OIDC backend. Its numbers are the hops
below. An arrow with an open head is a request the browser sends, one with a
filled head a call between servers.

![A federated login through Horizon in eight numbered hops. Hop 1: the browser opens the login page of Horizon through the Gateway and picks an identity provider, and Horizon answers with a redirect. Hop 2: the browser follows it to the websso path of that provider on the public Keystone URL and passes the dashboard origin. The Service of Keystone sends every request to the federation-proxy container of the Keystone pod on port 5050, which redirects a browser without a session to the identity provider. Hop 3: the browser opens the authorization endpoint of the identity provider, and the user logs in there. Hop 4: the identity provider sends the browser back to /v3/OS-FEDERATION/redirect_uri on the public Keystone URL, with a code. Hop 5: the proxy exchanges the code at the token endpoint of the identity provider, a call that leaves the pod. Hop 6: the proxy passes the request to Keystone on 127.0.0.1:5000 with the claims as request headers, and Keystone answers with a form that carries a token, if the origin is a trusted dashboard. Hop 7: the browser posts that form to the origin, the path /auth/websso/ of Horizon. Hop 8: Horizon validates the token against the cluster-local Keystone URL and starts the session. Hops 1 to 4 and 7 are requests of the browser and use public names; hops 5 and 8 are calls between servers and use names a pod resolves. A SAML backend differs in hops 4 and 5: the browser posts the assertion to the proxy, and no call to the identity provider follows.](../diagrams/service-federated-login.svg)

1. The browser opens `https://horizon.example.com/auth/login/` and submits the
  chosen entry of "Authenticate using".
2. The browser follows it to
   `{keystoneURL}/auth/OS-FEDERATION/identity_providers/{idp}/protocols/{protocol}/websso`
   and passes the dashboard origin, `https://horizon.example.com/auth/websso/`,
   as `origin`. `{keystoneURL}` is `spec.websso.keystoneURL` of the Horizon
   child, the public Keystone URL. The Service of Keystone sends every request
   to the `federation-proxy` container on port 5050, which protects this path
   and redirects a browser without a session to the identity provider.
3. The browser opens the authorization endpoint of the issuer, and the user
   logs in there.
4. The identity provider sends the browser back to
   `/v3/OS-FEDERATION/redirect_uri` on the public Keystone host, with an
   authorization code.
5. The proxy exchanges the code at the token endpoint of the provider. This
   call leaves the Keystone pod, so the pod has to resolve and reach the
   issuer.
6. The proxy passes the request to Keystone on `127.0.0.1:5000`, with the
   claims as `OIDC-` request headers. Keystone maps them to a user, compares
   `origin` with its `[federation] trusted_dashboard` list, and answers with a
   form that carries an unscoped token.
7. The browser submits that form to the origin,
   `https://horizon.example.com/auth/websso/`.
8. Horizon validates the token against `OPENSTACK_KEYSTONE_URL`, the
   cluster-local Service URL of Keystone, and starts the session.

Hops 1 to 4 and 7 use names the browser resolves, which is why Step 1
publishes both services. Hops 5 and 8 use names a pod resolves. A SAML backend
differs in hops 4 and 5: the identity provider has the browser post the
assertion to the proxy's endpoint
`/v3/OS-FEDERATION/identity_providers/{idp}/protocols/{protocol}/auth/mellon/postResponse`,
and no call from the proxy to the provider follows.

For a copy-pasteable devstack login that does not need a browser, use the CLI
bearer flow in [Attach an OIDC Federation Backend](./keystone/oidc-federation.md#step-6-log-in-as-a-federated-user).

## Step 4: Multi-domain login for LDAP users

Attach an LDAP `KeystoneIdentityBackend` the same way. Once it is `Ready`, the
Horizon child gains:

```bash
kubectl get horizon controlplane-horizon -n openstack -o jsonpath='{.spec.multiDomain}' | jq
```

```json
{
  "enabled": true,
  "defaultDomain": "Default"
}
```

The login page now shows a domain field. Users who leave it blank authenticate
in `Default`, keeping the bootstrap admin reachable.

The field accepts free text. Horizon bounds a
domain dropdown by `OPENSTACK_KEYSTONE_DOMAIN_CHOICES` and rejects every domain
outside it, but the operator only sees the domains your LDAP backends declare.
A dropdown built from LDAP backend domains could exclude users in domains the
operator cannot enumerate, such as a SQL-backed domain populated out-of-band or
the domain an OIDC backend targets.

Once you have enumerated every domain your users live in, declare the dropdown
on the ControlPlane yourself. The same block also turns on the domain field
with no LDAP backend attached, for users of SQL-backed domains outside
`Default`:

```yaml
spec:
  services:
    horizon:
      multiDomain:
        enabled: true
        domainDropdown: true
        domainChoices:
          - name: Default
            label: Default
          - name: corp
            label: Corp directory
```

An enabled block replaces the one the LDAP backend projects, and an empty
`defaultDomain` resolves to `Default`. A block with `enabled: false` cannot
switch the domain field off. The dropdown admits only the listed domains, so
list every domain your users log in from, `Default` and every LDAP-backed
domain included. The field's rules are in the
[`ServiceHorizonSpec` reference](../reference/c5c3/controlplane-crd.md#servicehorizonspec).

For standalone Horizon, set `spec.multiDomain.domainDropdown` and `domainChoices`
directly after enumerating every domain your users live in.

LDAP authentication alone is not enough to get a scoped token: a fresh domain
carries no role assignments, and Keystone answers `401` for a scope the user
holds no role on. Grant the role before asking users to log in:

```bash
openstack role add --domain corp --user alice --user-domain corp member
```

A `403` on `/project/` right after login is expected on this devstack. The
catalog has no compute or network services for the project dashboard to render.

## Standalone Keystone, without a ControlPlane

Without the ControlPlane there is nothing to project the trusted origin, so set
it directly on the Keystone CR. It is an oslo `MultiStrOpt`, so the field is a
list and renders as one `trusted_dashboard` line per entry:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
spec:
  federation:
    trustedDashboards:
      - https://horizon.example.com/auth/websso/
      - https://horizon.example.com:8443/auth/websso/
```

The section renders as soon as an origin is declared, so you can prepare the
trust relationship before the first backend attaches. Configure
`spec.websso` on the Horizon CR using the shape the ControlPlane projects.

The typed list is written after the `spec.extraConfig` merge, so declaring
`[federation] trusted_dashboard` in both places would silently drop the
`extraConfig` value; the validating webhook rejects the combination. Do not set
the same value in both fields.

## Publishing the dashboard on a non-default port

Local development clusters, and any deployment that cannot bind `:443`, publish
the dashboard somewhere else. Two things must agree with what the browser sees:

```yaml
    horizon:
      gateway:
        hostname: horizon.127-0-0-1.nip.io
        parentRef:
          name: openstack-gw
      publicEndpoint: https://horizon.127-0-0-1.nip.io:8443
```

The operator then projects
`https://horizon.127-0-0-1.nip.io:8443/auth/websso/` as the trusted origin. The
port cannot be derived from the hostname, which is why the override
exists.

## Pinning the federation proxy image

Attaching an OIDC backend makes the Keystone operator inject an Apache /
`mod_auth_openidc` sidecar. The ControlPlane defaults to the mutable tag
`ghcr.io/c5c3/keystone-federation-proxy:latest`; each node pulls it when the pod
starts. Pin it:

```yaml
    keystone:
      federationProxyImage:
        repository: ghcr.io/c5c3/keystone-federation-proxy
        digest: sha256:<digest>
```

The override also accepts a locally built tag. The `federated-controlplane` e2e
suite uses it to test the candidate sidecar before publication.

## Troubleshooting

**The login page shows no SSO choice.** The backend has not reached `Ready`.
Only `Ready` backends contribute a choice:

```bash
kubectl get keystoneidentitybackend -n openstack
```

**Keystone answers "Origin ... is not a trusted dashboard host".** The origin
the dashboard sent does not match `trusted_dashboard` character for character.
This is hop 6 of [the login](#login-hops). Compare them:

```bash
kubectl get keystone controlplane-keystone -n openstack \
  -o jsonpath='{.spec.federation.trustedDashboards}'
```

Then check two of the rules from Step 1: the port must be present when it is not
443, and `publicEndpoint` must name the same host as `gateway.hostname`.

**The SSO button redirects to an unreachable URL.** This is hop 2 of
[the login](#login-hops). `spec.websso.keystoneURL` is projected from
`services.keystone.publicEndpoint`. If that is unset, the operator derives
`https://{gateway.hostname}/v3`, the default-443 form, which a gateway published
on another port does not answer. Set `publicEndpoint` with the port. Only a
Horizon CR you write yourself falls back to `spec.keystoneEndpoint`, the
cluster-local Service URL, when its `spec.websso.keystoneURL` is empty.

## Tested by

The suite asserts the full ControlPlane-driven SSO chain: both services
published through the shared gateway, OIDC and LDAP backends attached, and a
headless WebSSO hand-off. It runs on its own CI e2e kind cluster:

```bash
chainsaw test --test-dir tests/e2e-controlplane-sso
```

::: details The ControlPlane CR the suite applies
The suite runs a second full ControlPlane in its own CI job (the webhook permits
only one ControlPlane per namespace), so its CR name (`controlplane-sso`)
differs from the `controlplane` devstack name used in the
walkthrough above.

<<< ../../tests/e2e-controlplane-sso/02-controlplane-cr.yaml#controlplane-cr
:::

::: details The identity backends the suite applies
Both fixture backends reference `controlplane-sso-keystone`. The walkthrough
uses the `controlplane-keystone` child.

<<< ../../tests/e2e-controlplane-sso/04-backends.yaml#backends
:::
