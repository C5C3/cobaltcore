---
title: End-to-End SSO
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# End-to-End SSO

This guide takes you from a working ControlPlane to a Horizon login page with
a working SSO button, and then to a domain dropdown for users who live in an
LDAP-backed domain.

It assumes you already know how to attach identity backends. If you do not,
read [Attach an OIDC Federation Backend](./oidc-federation.md) and
[Attach an LDAP Domain Backend](./ldap-domain-backend.md) first — this guide
picks up where they leave off and shows what the ControlPlane does with them.

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
federates against. These are the same fixtures the two backend guides use, with
the WebSSO gateway redirect URIs added for the token hand-off:

```bash
kubectl apply -f tests/e2e-controlplane-sso/00-keycloak.yaml \
              -f tests/e2e-controlplane-sso/01-openldap.yaml
kubectl -n openstack rollout status deploy/keycloak
kubectl -n openstack rollout status deploy/openldap
```

::: tip Already applied the oidc-federation fixture?
Keycloak imports its realms at pod start, so if you previously applied the
`oidc-federation` guide's copy of the Keycloak fixture, re-apply the manifest
above and restart the pod so the gateway redirect URIs are picked up:

```bash
kubectl -n openstack rollout restart deploy/keycloak
```
:::

- [Attach an OIDC Federation Backend](./oidc-federation.md) — how to stand up the
  federation backend this guide projects onto the login page.
- [Attach an LDAP Domain Backend](./ldap-domain-backend.md) — how to stand up the
  LDAP domain backend used in the multi-domain step.

## What the ControlPlane does for you

Attaching a `KeystoneIdentityBackend` to a ControlPlane's Keystone child is the
only action you take. The ControlPlane operator watches those backends and, for
every one that reaches `Ready`, projects:

- a **WebSSO choice** onto the Horizon child (`spec.websso`), so the login page
  gains an entry in its "Authenticate using" dropdown for each federation
  backend;
- a **domain field** onto the Horizon child (`spec.multiDomain`), once any
  LDAP-backed domain is in play;
- the **trusted dashboard origin** onto the Keystone child
  (`spec.federation.trustedDashboards`), so Keystone accepts the token hand-off
  back to your dashboard.

Only `Ready` backends contribute. A backend whose Keystone-side federation
objects are not provisioned yet never produces an SSO button that dead-ends.
Detaching the last backend clears the block again, and the login page reverts to
local credentials.

The SSO choices also need both services published (Step 1). Until then the
operator projects no `spec.websso` at all, even for a `Ready` backend, and logs
why — a button whose hand-off Keystone would reject only *after* the user has
typed their corporate password is worse than no button.

## Step 1 — Publish both services

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
      # The browser follows the SSO redirect to this URL, so it must be the
      # externally routable Keystone — never the cluster-local Service URL.
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
`[federation] trusted_dashboard` list character for character. Two rules follow:

- **`publicEndpoint` must include a non-default port.** If you publish the
  dashboard on `https://horizon.example.com:8443`, say so. When
  `publicEndpoint` is empty the operator derives
  `https://{gateway.hostname}` — the default-443 form — and the hand-off is
  rejected on any other port.
- **`publicEndpoint` and `gateway.hostname` must name the same host.** Django
  derives the origin it sends from the request's `Host` header, i.e. from the
  gateway hostname, not from `publicEndpoint`. If the two disagree, Keystone
  would reject an origin you never see in your configuration — so the
  validating webhook rejects the ControlPlane instead. The port may still
  differ, since Gateway API hostnames carry none.
- **`publicEndpoint` must use `https` behind a gateway.** The Gateway listener
  terminates TLS, and Keystone POSTs the unscoped WebSSO token to this origin
  after every federated login. Over `http` that bearer token — good for the
  user's full API privileges — travels in cleartext, so the validating webhook
  rejects it. Without a gateway the value is only warned about.
:::

## Step 2 — Attach a federation backend

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
    # blocks .well-known discovery against the in-cluster Keycloak — see
    # [Attach an OIDC Federation Backend](./oidc-federation.md) Step 4 for the
    # full worked example (mappings, groups, and why introspection is https).
    issuer: http://keycloak.openstack.svc.cluster.local:8080/realms/forge
    clientID: keystone
    clientSecretRef:
      name: keycloak-forge-client
    endpoints:
      authorizationEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/auth
      tokenEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/token
      jwksURI: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/certs
      userinfoEndpoint: http://keycloak.openstack.svc.cluster.local:8080/realms/forge/protocol/openid-connect/userinfo
      introspectionEndpoint: https://keycloak.openstack.svc.cluster.local:8443/realms/forge/protocol/openid-connect/token/introspect
    oauth2Introspection:
      enabled: true
      tlsVerify: false
```

::: tip The Default domain is off limits
A federation backend may not back the `Default` domain: it hosts the SQL-backed
service users and the bootstrap admin. Give each federated backend its own
domain.
:::

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

The `credentials` entry leads the list and is preselected. Enabling SSO must
never lock out local accounts — including the bootstrap admin and every
LDAP-domain user — so the operator always offers the local login form alongside
the federated choices.

## Step 3 — Complete a login

Open `https://horizon.example.com/auth/login/`. The "Authenticate using"
dropdown now offers your identity provider. Pick it, authenticate at the
provider, and you land back on the dashboard with a session.

Under the hood the dashboard redirects the browser to
`{keystoneURL}/auth/OS-FEDERATION/identity_providers/{idp}/protocols/{protocol}/websso`,
passing its own origin. Keystone authenticates you through the provider and
POSTs a token back to that origin — but only if the origin appears in its
trusted list, which is what Step 1 configured.

::: warning The fixture IdP is not reachable from a host browser
On the kind devstack the fixture Keycloak issuer is a cluster-internal Service
name, so a browser on your workstation cannot complete the login form: it is
redirected to `http://keycloak.openstack.svc.cluster.local:8080/...`, which the
host cannot resolve. Clicking the SSO button through to a session needs an
**externally reachable** IdP (your production Keycloak, or a fixture published
through the gateway with matching redirect URIs and a host-resolvable issuer).
The full WebSSO hand-off against the in-cluster fixture — including the
Envoy-routed gateway redirect URIs this guide's fixtures register — is
exercised headlessly by the mirroring e2e suite (see [Tested by](#tested-by)).

For a copy-pasteable devstack login that does not need a browser, use the CLI
bearer flow in [Attach an OIDC Federation Backend](./oidc-federation.md#step-6-log-in-as-a-federated-user).
:::

## Step 4 — Multi-domain login for LDAP users

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

The login page now shows a domain field, and users who leave it blank land in
`Default` — so the bootstrap admin stays reachable.

The field is deliberately free text rather than a dropdown. Horizon bounds a
domain dropdown by `OPENSTACK_KEYSTONE_DOMAIN_CHOICES` and rejects every domain
outside it, but the operator only sees the domains your LDAP backends declare.
A dropdown built from those would lock out everyone in a domain it cannot
enumerate — a SQL-backed domain you populated out-of-band, or the domain an OIDC
backend targets. A standalone Horizon CR (one no ControlPlane projects onto) can
still set `spec.multiDomain.domainDropdown` and `domainChoices` itself, once you
have enumerated every domain your users live in.

LDAP authentication alone is not enough to get a scoped token: a fresh domain
carries no role assignments, and Keystone answers `401` for a scope the user
holds no role on. Grant the role before asking users to log in:

```bash
openstack role add --domain corp --user alice member
```

A `403` on `/project/` right after login is expected in a Keystone-only stack —
there are no compute or network services in the catalog yet.

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
trust relationship before the first backend attaches. You then configure the
dashboard's `spec.websso` block on the Horizon CR yourself — the same shape the
ControlPlane would have projected.

::: warning Do not also set it in `extraConfig`
`spec.extraConfig` wins the render-time merge, so declaring
`[federation] trusted_dashboard` in both places would silently drop the typed
list. The validating webhook rejects the combination.
:::

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
port cannot be derived from the hostname, which is exactly why the override
exists.

## Pinning the federation proxy image

Attaching an OIDC backend makes the Keystone operator inject an Apache /
`mod_auth_openidc` sidecar. The ControlPlane projects
`ghcr.io/c5c3/keystone-federation-proxy:latest` by default — a mutable tag, so
every node re-pulls it on each pod start. Pin it:

```yaml
    keystone:
      federationProxyImage:
        repository: ghcr.io/c5c3/keystone-federation-proxy
        digest: sha256:<digest>
```

The same override takes a locally built tag, which is how the
`federated-controlplane` e2e suite exercises the sidecar under review rather
than the one already published.

## Troubleshooting

**The login page shows no SSO choice.** The backend has not reached `Ready`.
Only `Ready` backends contribute a choice:

```bash
kubectl get keystoneidentitybackend -n openstack
```

**Keystone answers "Origin ... is not a trusted dashboard host".** The origin
the dashboard sent does not match `trusted_dashboard` character for character.
Compare them:

```bash
kubectl get keystone controlplane-keystone -n openstack \
  -o jsonpath='{.spec.federation.trustedDashboards}'
```

Then check the two rules from Step 1: the port must be present when it is not
443, and `publicEndpoint` must name the same host as `gateway.hostname`.

**The SSO button redirects to an unreachable URL.** `spec.websso.keystoneURL`
is projected from `services.keystone.publicEndpoint`. If that is unset, Horizon
falls back to `spec.keystoneEndpoint` — the cluster-local Service URL, which the
browser cannot resolve. Set `publicEndpoint`.

## Tested by

The full ControlPlane-driven SSO chain — both services published through the
shared gateway, the OIDC and LDAP backends attached, and a headless WebSSO
hand-off — is asserted end-to-end on its own CI e2e kind cluster by this
chainsaw suite:

```bash
chainsaw test --test-dir tests/e2e-controlplane-sso
```

::: details The ControlPlane CR the suite applies
The suite runs a second full ControlPlane in its own CI job (the webhook permits
only one ControlPlane per namespace), so its CR name (`controlplane-sso`)
deliberately differs from the `controlplane` devstack name used in the
walkthrough above.

<<< @/../tests/e2e-controlplane-sso/02-controlplane-cr.yaml#controlplane-cr
:::

::: details The identity backends the suite applies
Both backends reference the suite's projected Keystone child
(`controlplane-sso-keystone`) rather than the walkthrough's `controlplane-keystone`.

<<< @/../tests/e2e-controlplane-sso/04-backends.yaml#backends
:::
