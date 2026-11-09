---
title: Attach a SAML Federation Backend to Keystone
quadrant: operator
---

# Attach a SAML Federation Backend to Keystone

This guide walks through federating a running Keystone with a SAML 2.0
identity provider using the `KeystoneIdentityBackend` CRD: supply the IdP
metadata, apply one CR, watch the conditions converge, export the generated
service-provider (SP) metadata, register it at the IdP out of band, and
verify a federated user can log in. The worked example uses Keycloak, the
same IdP the mirroring e2e suite ([Tested by](#tested-by)) deploys.
You can reuse its [test fixture](#test-with-the-e2e-idp-fixture) on a kind cluster.

SAML reuses almost all of the OIDC federation machinery: the same
federation sidecar (which now also ships `mod_auth_mellon`), the same
content-hashed federation Secret, and the same dedicated backend controller
that provisions the Keystone identity provider, mapping, and protocol. When
a SAML backend attaches, the operator generates `mod_auth_mellon`
configuration into the sidecar alongside `mod_auth_openidc`; the two
coexist, so a Keystone can serve OIDC and SAML backends side by side.

For the full field reference, see the
[KeystoneIdentityBackend CRD API Reference](../../reference/keystone/identity-backend-crd.md).

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start](../../quick-start.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 make deploy-infra
```

Follow that tutorial through to its final **Verify** step, so a Keystone CR named
`keystone` is `Ready` in the `openstack` namespace. The examples below attach
a new identity backend to that Keystone.
:::

- A SAML identity provider whose EntityDescriptor metadata is available
  inline, as a Secret, or by URL.
- **A federation proxy image.** The managed ControlPlane path projects
  `ghcr.io/c5c3/keystone-federation-proxy` automatically; standalone
  Keystone installations set `spec.federation.proxyImage` themselves.
  Without it, federation backends stay pending with a
  `FederationProxyImageMissing` Warning.
- **Service users remain SQL-backed.** Federated users are ephemeral shadow
  users; OpenStack service accounts and the bootstrap admin remain in the
  SQL-backed `Default` domain, which the CRD hard-rejects federating.
- **One SAML backend per Keystone is supported.** Keystone acts as the
  service provider (SP): it consumes the IdP's assertions to authenticate users.
  The SP configuration includes its entity ID, certificate, and endpoints
  for receiving SAML responses and handling logout.
  The operator configures `mod_auth_mellon` with these settings on a shared
  Apache `/v3` parent Location. The validating webhook therefore rejects a
  second SAML backend on the same Keystone.

## Step 1: Supply the IdP metadata

Pick exactly one of the three sources for `spec.saml.idpMetadata`:

- **`secretRef`**: a Secret carrying the EntityDescriptor XML under the
  fixed data key `idp-metadata.xml`:

  ```bash
  curl --fail --silent --show-error https://idp.example.com/realms/cobaltcore/protocol/saml/descriptor \
    -o idp-metadata.xml
  kubectl create secret generic corp-saml-idp-metadata -n openstack \
    --from-file=idp-metadata.xml=idp-metadata.xml
  ```

- **`inline`**: paste the raw EntityDescriptor XML into the CR (the
  operator validates its `entityID` against `spec.saml.idpEntityID` at
  admission).
- **`url`**: the operator fetches the document through the same hardened,
  SSRF-guarded client the OIDC path uses. By default that client refuses to dial
  any non-public address; to fetch metadata from a trusted in-cluster SAML IdP,
  allowlist its CIDR through the same operator-level `federation.metadataAllowCidrs`
  chart value (`--federation-metadata-allow-cidrs` flag) described under
  [OIDC discovery against an in-cluster IdP](./oidc-federation.md#discovery-against-an-in-cluster-identity-provider).

### Test with the e2e IdP fixture

For a disposable kind test, reuse the Keycloak fixture that the
[SAML e2e suite](#tested-by) deploys. Run these commands from the repository root
after the Quick Start's Keystone is ready:

```bash
kubectl apply -f tests/e2e/keystone/saml-federation/00-keycloak-saml.yaml
kubectl rollout status deployment/keycloak-saml -n openstack --timeout=6m
```

The fixture imports the `cobaltcore-saml` realm, a SAML client, and a test user.
It runs Keycloak in development mode with HTTP, fixed test credentials,
and a 1 GiB memory limit. Do not use it in production or run it alongside the
Chainsaw suite, which manages the same resources. If the rollout times out,
inspect `kubectl logs -n openstack deployment/keycloak-saml` before continuing.

Fetch the descriptor from the running Keystone container, as the e2e suite
does, to reach the cluster-local Service without changing the operator's
metadata-fetch allowlist:

```bash
POD=$(kubectl get pod -n openstack -l app.kubernetes.io/instance=keystone \
  -o jsonpath='{.items[0].metadata.name}')
kubectl exec -n openstack "$POD" -c keystone -- python3 -c '
import urllib.request
url = "http://keycloak-saml.openstack:8080/realms/cobaltcore-saml/protocol/saml/descriptor"
print(urllib.request.urlopen(url, timeout=30).read().decode())
' > idp-metadata.xml
kubectl create secret generic corp-saml-idp-metadata -n openstack \
  --from-file=idp-metadata.xml=idp-metadata.xml
```

In the Step 4 CR, replace both occurrences of
`https://idp.example.com/realms/cobaltcore` (the `idpEntityID` and mapping
`anyOneOf` value) with
`http://keycloak-saml.openstack:8080/realms/cobaltcore-saml`.
Keep the guide's `corp-saml` and `keystone` resource names.

The fixture's preconfigured SAML client targets the suite's `keystone-saml`
instance. In Step 6, register the SP metadata exported by this guide's
`keystone` instance. The fixture suffices for backend reconciliation and SP
metadata checks; it does not provide browser access from your workstation.
For Step 7, make both the IdP and Keystone reachable by the browser and
configure their advertised URLs accordingly. If you change Keycloak's
`KC_HOSTNAME`, fetch its descriptor again and update the CR's entity ID and
mapping to match.

## Step 2: Configure the SP certificate (optional)

Supply your own SP certificate if your IdP registration requires one.
Otherwise the operator generates a self-signed SP keypair once, stores it in
the stable-named `<keystone>-saml-sp` Secret, and never rotates it.
Regeneration would invalidate the IdP-side registration. IdPs pin the SP
certificate from the SP metadata, where self-signed is standard.

To supply your own certificate (for example, one issued by cert-manager),
reference a `kubernetes.io/tls`-shaped Secret (fixed keys `tls.crt` /
`tls.key`):

```yaml
spec:
  saml:
    sp:
      certificateSecretRef:
        name: corp-saml-sp-cert
```

## Step 3: Configure the proxy image (standalone installations)

Set `spec.federation.proxyImage` on the Keystone CR if it is not already
projected by a managed ControlPlane:

```yaml
spec:
  federation:
    proxyImage:
      repository: ghcr.io/c5c3/keystone-federation-proxy
      digest: sha256:<64-hex-digest>   # pin by immutable digest, never a mutable tag
```

The sidecar terminates SAML on the authentication path, so pin it by immutable
`digest`. `latest` re-pulls on every restart (its default `pullPolicy` is
`Always`). A silently re-published image would enter the auth path unreviewed,
with no version to roll back to. The proxy image has release-independent
`:latest` and per-commit `:<sha>` tags. It does not follow the
`spec.openStackRelease` cadence. See the
[proxy build and publication reference](../../reference/ci-cd/build-images-workflow.md#build-keystone-federation-proxy-merge-keystone-federation-proxy-image).
Resolve the digest of the build you intend to run and pin that:

```bash
docker buildx imagetools inspect ghcr.io/c5c3/keystone-federation-proxy:latest \
  --format '{{.Manifest.Digest}}'
```

## Step 4: Apply the backend CR

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: corp-saml
  namespace: openstack
spec:
  keystoneRef:
    name: keystone
  domain:
    name: cobaltcore-saml
    mode: Manage
    deletionPolicy: Delete
  type: SAML
  saml:
    idpEntityID: https://idp.example.com/realms/cobaltcore
    identityProviderName: corp-saml
    idpMetadata:
      secretRef:
        name: corp-saml-idp-metadata
    forwardAttributes:
    - username
  mappings:
  - remote:
    - type: HTTP_MELLON_IDP
      anyOneOf:
      - https://idp.example.com/realms/cobaltcore
    - type: HTTP_MELLON_USERNAME
    local:
    - user:
        name: "{0}"
      group:
        name: federated-users
        domain:
          name: cobaltcore-saml
  groups:
  - name: federated-users
    roleAssignments:
    - role: member
      domain: true
```

### Attribute forwarding and the `HTTP_MELLON_<ATTR>` convention

`spec.saml.forwardAttributes` lists SAML assertion attribute names the proxy
forwards to Keystone as `MELLON-<attr>` request headers. Two attributes are
always forwarded (`MELLON-IDP` and `MELLON-NAME-ID`); the rest come from your
list.

The attribute names are **case-sensitive** and forwarded verbatim, but the
HTTP header hop uppercases them: an assertion attribute `username` arrives at
Keystone as the WSGI environ key `HTTP_MELLON_USERNAME`. Your mapping rules
must reference the **uppercased** spelling. A mapping remote type of
`HTTP_MELLON_username` silently matches nothing. See the troubleshooting
table below.

## Step 5: Watch the conditions converge

```bash
kubectl get keystoneidentitybackend corp-saml -n openstack \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

`DomainReady`, `FederationObjectsReady`, `MappingsReady`, `ConfigProjected`,
and the aggregate `Ready` all reach `True`.

The figure shows that order for every satellite kind. Its gate,
`CredentialsReady`, is `DomainReady` on an identity backend.
[The handshake](../../reference/backend/kubernetes-packages.md#satellite-handshake)
lists the five steps and what differs per kind.

![The handshake between a satellite resource and the service it attaches to, in five numbered steps across two controllers. 1: the satellite controller checks the credentials and sets CredentialsReady on the satellite. 2: the aggregation step of the service controller reads only that condition. 3: it renders one section per satellite that passed into a Secret whose name carries a hash of its content. 4: the pod template of the service's Deployment mounts that Secret, and a new name rolls the pods. 5: the satellite controller finds its section in the mounted Secret and sets ConfigProjected. Ready turns True once both conditions are. An arrow marked never runs from Ready to the aggregation step: reading Ready there would deadlock, because Ready needs ConfigProjected, which needs that step. On a KeystoneIdentityBackend the gate is DomainReady, and ConfigProjected also waits until the rollout has finished.](../../diagrams/service-satellite-handshake.svg)

## Step 6: Export the SP metadata and register it at the IdP

Once the SP material resolves, the operator writes the SP metadata to a
stable-named Secret (also surfaced on the backend status as
`samlSPMetadataSecretName`):

```bash
kubectl get keystoneidentitybackend corp-saml -n openstack \
  -o jsonpath='{.status.samlSPMetadataSecretName}'
kubectl get secret keystone-saml-sp-metadata -n openstack \
  -o jsonpath='{.data.sp-metadata\.xml}' | base64 -d
```

Register that SP metadata with your IdP out of band (in Keycloak: create a
SAML client, import the SP metadata, and confirm the client's `clientId`
equals the SP `entityID` from the Secret). The export is written once the
backend renders, and a backend whose IdP metadata does not resolve is skipped.
Supply the IdP metadata first, then register the SP.

## Step 7: Log in via WebSSO

Point the dashboard's WebSSO choice at the per-IdP path

```
/v3/auth/OS-FEDERATION/identity_providers/<identityProviderName>/protocols/mapped/websso
```

The proxy redirects the browser to the IdP with a SAML authentication request.
After login, the IdP returns a form that the browser posts to
`<endpoint>/postResponse` with the SAML response containing a signed assertion.
Here, `<endpoint>` is the SP endpoint base:
`/v3/OS-FEDERATION/identity_providers/<identityProviderName>/protocols/mapped/auth/mellon`.
`mod_auth_mellon` validates the response, establishes its session, and forwards
the configured attributes to Keystone.

![A SAML login through Horizon in seven numbered steps. 1: the browser chooses the SAML provider at Horizon, which returns a redirect. 2: the browser opens the provider's WebSSO path on the public Keystone URL with the dashboard origin; the federation proxy returns an IdP redirect carrying a SAML authentication request. 3: the browser visits the IdP and the user logs in; the IdP returns a form carrying a SAML response containing a signed assertion. 4: the browser posts that response to the proxy's Mellon postResponse endpoint. 5: mod_auth_mellon validates it and forwards attributes as MELLON headers to Keystone on 127.0.0.1:5000; Keystone returns a token form through the proxy for a trusted dashboard origin. 6: the browser posts the token to Horizon's /auth/websso/ path. 7: Horizon validates the token against cluster-local Keystone and starts a session. Browser requests use public names; there is no proxy-to-IdP token exchange. Gateway and Service routing are omitted.](../../diagrams/service-saml-login.svg)

The [OIDC login flow](../end-to-end-sso.md#login-hops) uses an authorization
code and a proxy-to-IdP token exchange instead of the browser's SAML POST.

1. The browser selects the SAML provider at Horizon, which redirects it to Keystone.
2. The browser opens the per-IdP WebSSO path with the dashboard `origin`.
   The proxy redirects it to the IdP with a SAML authentication request.
3. The user logs in at the IdP, which returns a form carrying a SAML response
   containing a signed assertion.
4. The browser posts that form to the proxy's Mellon `postResponse` endpoint.
5. Mellon validates the response and forwards attributes as `MELLON-*` headers
   to Keystone on `127.0.0.1:5000`. For a trusted dashboard origin, Keystone
   returns a token form through the proxy.
6. The browser posts that token to Horizon's `/auth/websso/` origin.
7. Horizon validates the token against cluster-local Keystone and starts a session.

## Coexistence with OIDC

A Keystone can carry OIDC and SAML backends side by side. Both modules render into
one `proxy.conf` on one sidecar: `mod_auth_openidc` serves the OIDC
Locations and `mod_auth_mellon` serves the SAML Locations. The
identity-provider name must be unique across every federation backend of one
Keystone (webhook-enforced).

## Deleting a backend

Deleting the CR removes the Keystone federation objects (protocol → mapping →
identity provider) unconditionally, applies the domain deletion policy, and
deletes the SP-metadata export Secret once no SAML backend remains. Detaching
the last federation backend restores the plain uWSGI-only pod.

## Limitations

- **One SAML backend per Keystone is supported** (webhook-enforced). Multi-IdP SAML would
  need `mod_auth_mellon`'s IdP-discovery machinery and is out of scope.
- **No CLI / ECP flow.** SAML Web-SSO is browser-mediated; there is no
  bearer-token equivalent to the OIDC introspection path.

## Security considerations

- **Header stripping.** The proxy strips every spoofable `MELLON-*` header
  (both dash and underscore spellings) before authentication, so an
  in-cluster client cannot forge a federated identity past the module. The
  forwarded values are set from the genuine assertion in the late phase,
  overwriting any forged inbound header.
- **Address check disabled.** `MellonSubjectConfirmationDataAddressCheck` is
  `Off` because behind the sidecar/gateway the assertion's
  `SubjectConfirmationData` address never matches the client's source IP.
- **SP key.** The generated SP key lives in the content-hashed federation
  Secret at mode `0400` alongside the other federation material.

## Troubleshooting

| Symptom | Likely cause |
| --- | --- |
| Mapped user has empty attributes | The mapping remote type does not match the uppercased header spelling — use `HTTP_MELLON_<ATTR>` (uppercase), not the assertion's original case. |
| `MappingsReady=False`, reason `NoMappingRules` | `spec.mappings` is empty; Keystone requires at least one mapping rule before the protocol can be provisioned. |
| Backend stays pending, `FederationProxyImageMissing` | `spec.federation.proxyImage` is not set on the Keystone CR. |
| Second SAML backend rejected at admission | At most one SAML backend per Keystone is supported. |
| IdP rejects the SAML authentication request | The SP was not registered, or its `clientId`/`entityID` at the IdP does not match the exported SP `entityID`. |

## Tested by

Attaching the SAML backend, watching the conditions converge, and exporting the
SP metadata are asserted on the CI e2e kind cluster by this chainsaw suite. It
runs no WebSSO login: the SAML round trip through a browser is left to a check
by hand.

```bash
chainsaw test --test-dir tests/e2e/keystone/saml-federation
```

::: details The backend CR the suite applies
The suite isolates its Keystone instance from the parallel suite pool, so its
backend CR name (`keycloak-saml`) and `keystoneRef` (`keystone-saml`)
differ from the `corp-saml` / `keystone` names used in the
walkthrough above.

<<< ../../../tests/e2e/keystone/saml-federation/02-backend-cr.yaml#backend-cr
:::
