---
title: Attach a SAML Federation Backend to Keystone
quadrant: operator
---

# Attach a SAML Federation Backend to Keystone

This guide walks through federating a running Keystone with a SAML 2.0
identity provider using the `KeystoneIdentityBackend` CRD: supply the IdP
metadata, apply one CR, watch the conditions converge, export the generated
service-provider (SP) metadata, register it at the IdP out of band, and
verify a federated user can log in. The worked example uses Keycloak — the
same fixture the e2e suite deploys — so every value is adaptable to a kind
cluster.

SAML reuses almost all of the OIDC federation machinery: the same
federation sidecar (which now also ships `mod_auth_mellon`), the same
content-hashed federation Secret, and the same dedicated backend controller
that provisions the Keystone identity provider, mapping, and protocol. When
a SAML backend attaches, the operator generates `mod_auth_mellon`
configuration into the sidecar alongside `mod_auth_openidc`; the two
coexist, so a Keystone can serve OIDC and SAML backends at once.

For the full field reference, see the
[KeystoneIdentityBackend CRD API Reference](../reference/keystone/identity-backend-crd.md).

## Prerequisites

- A Keystone CR that reaches `Ready=True` (see the
  [Quick Start](../quick-start.md)).
- A SAML identity provider whose EntityDescriptor metadata is available
  inline, as a Secret, or by URL.
- **A federation proxy image.** The managed ControlPlane path projects
  `ghcr.io/c5c3/keystone-federation-proxy` automatically; standalone
  Keystone installations set `spec.federation.proxyImage` themselves.
  Without it, federation backends stay pending with a
  `FederationProxyImageMissing` Warning — no hidden default is assumed.
- **Service users stay SQL-backed.** Federated users are ephemeral shadow
  users; OpenStack service accounts and the bootstrap admin remain in the
  SQL-backed `Default` domain, which the CRD hard-rejects federating.
- **At most one SAML backend per Keystone.** `mod_auth_mellon`'s SP
  configuration projects onto a shared `/v3` parent Location, so the
  validating webhook rejects a second SAML backend on the same Keystone.

## Step 1 — Supply the IdP metadata

Pick exactly one of the three sources for `spec.saml.idpMetadata`:

- **`secretRef`** — a Secret carrying the EntityDescriptor XML under the
  fixed data key `idp-metadata.xml`:

  ```bash
  curl -s https://idp.example.com/realms/forge/protocol/saml/descriptor \
    -o idp-metadata.xml
  kubectl create secret generic corp-saml-idp-metadata \
    --from-file=idp-metadata.xml=idp-metadata.xml
  ```

- **`inline`** — paste the raw EntityDescriptor XML into the CR (the
  operator validates its `entityID` against `spec.saml.idpEntityID` at
  admission).
- **`url`** — the operator fetches the document through the same hardened,
  SSRF-guarded client the OIDC path uses.

## Step 2 — Configure the SP certificate (optional)

By default the operator generates a self-signed SP keypair once (stored in
the stable-named `<keystone>-saml-sp` Secret) and never rotates it —
regeneration would invalidate the IdP-side registration. IdPs pin the SP
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

## Step 3 — Configure the proxy image (standalone installations)

Set `spec.federation.proxyImage` on the Keystone CR if it is not already
projected by a managed ControlPlane:

```yaml
spec:
  federation:
    proxyImage:
      repository: ghcr.io/c5c3/keystone-federation-proxy
      tag: v0.1.0
```

## Step 4 — Apply the backend CR

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: KeystoneIdentityBackend
metadata:
  name: corp-saml
spec:
  keystoneRef:
    name: keystone
  domain:
    name: forge-saml
    mode: Manage
    deletionPolicy: Delete
  type: SAML
  saml:
    idpEntityID: https://idp.example.com/realms/forge
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
      - https://idp.example.com/realms/forge
    - type: HTTP_MELLON_USERNAME
    local:
    - user:
        name: "{0}"
      group:
        name: federated-users
        domain:
          name: forge-saml
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
must reference the **uppercased** spelling — a mapping remote type of
`HTTP_MELLON_username` silently matches nothing. See the troubleshooting
table below.

## Step 5 — Watch the conditions converge

```bash
kubectl get keystoneidentitybackend corp-saml \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

`DomainReady`, `FederationObjectsReady`, `MappingsReady`, `ConfigProjected`,
and the aggregate `Ready` all reach `True`.

## Step 6 — Export the SP metadata and register it at the IdP

Once the SP material resolves, the operator writes the SP metadata to a
stable-named Secret (also surfaced on the backend status as
`samlSPMetadataSecretName`):

```bash
kubectl get keystoneidentitybackend corp-saml \
  -o jsonpath='{.status.samlSPMetadataSecretName}'
kubectl get secret keystone-saml-sp-metadata \
  -o jsonpath='{.data.sp-metadata\.xml}' | base64 -d
```

Register that SP metadata with your IdP out of band (in Keycloak: create a
SAML client, import the SP metadata, and confirm the client's `clientId`
equals the SP `entityID` from the Secret). The export is written even before
the IdP metadata resolves, so you can register the SP first and supply the
IdP metadata afterward.

## Step 7 — Log in via WebSSO

Point the dashboard's WebSSO choice at the per-IdP path

```
/v3/auth/OS-FEDERATION/identity_providers/<identityProviderName>/protocols/mapped/websso
```

The proxy redirects the browser to the IdP, the IdP posts the signed
assertion back to `<endpoint>/postResponse`, and `mod_auth_mellon`
establishes the session and forwards the mapped attributes to Keystone.

## Coexistence with OIDC

A Keystone can carry OIDC and SAML backends at once. Both modules render into
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

- **One SAML backend per Keystone** (webhook-enforced). Multi-IdP SAML would
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
| IdP rejects the AuthnRequest | The SP was not registered, or its `clientId`/`entityID` at the IdP does not match the exported SP `entityID`. |
