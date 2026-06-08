---
title: Expose Keystone Externally via the Gateway
quadrant: operator
---

<!-- operator namespace is `keystone-system`; workload (Keystone CR) stays in `openstack`. -->

# How-to: Expose Keystone Externally via the Gateway

This guide walks an operator through publishing a Keystone API externally
through a shared Gateway API Gateway: setting `spec.gateway` so the operator
creates the `HTTPRoute`, setting `spec.bootstrap.publicEndpoint` so the
service catalog advertises a routable URL, and verifying that an external
client can issue a token. For the authoritative field schema and validation
rules, see [Keystone CRD — GatewaySpec](../reference/keystone/keystone-crd.md#gatewayspec).

> **Scope.** The operator plays the **application-developer** role in the
> Gateway API model: it manages only the `HTTPRoute`. The referenced
> `Gateway` and its `GatewayClass` are infrastructure the platform team
> provisions — the operator never creates or reconciles them.

---

## How it works — two fields, two jobs

External exposure needs two independent things, and each is driven by its
own field:

| Field | Drives | Without it |
|-------|--------|------------|
| `spec.gateway` | The **ingress path**: the operator creates an `HTTPRoute` that attaches to the referenced Gateway and forwards the matched host/path to the `{name}` Service on port 5000. Reflected by the `HTTPRouteReady` condition. | No `HTTPRoute`; `HTTPRouteReady` is `True`/`HTTPRouteNotRequired`; traffic never reaches Keystone from outside the cluster. |
| `spec.bootstrap.publicEndpoint` | The **catalog advertisement**: passed to `keystone-manage bootstrap --bootstrap-public-url`, so the URL clients discover from the service catalog is the externally routable one. | Bootstrap falls back to the cluster-local Service DNS, so the catalog advertises an in-cluster URL that external clients cannot reach. |

Both are needed because creating a route (`spec.gateway`) and telling
Keystone what URL to advertise (`publicEndpoint`) are separate concerns. A
route with no catalog URL leaves clients unable to discover the endpoint; a
catalog URL with no route advertises an address that nothing serves.

### What `.status.endpoint` reports

The operator projects the externally reachable URL into `.status.endpoint`.
When `spec.gateway` is set, resolution is:

1. `spec.bootstrap.publicEndpoint`, if non-empty — used **verbatim**.
2. otherwise `https://{spec.gateway.hostname}/v3` (implicit port 443).

`publicEndpoint` wins because the externally reachable URL can carry a port
that no Kubernetes object captures. The Gateway listener is always the
in-cluster TLS port (443), but kind `extraPortMappings`, LoadBalancer
overrides, and edge proxies can republish that listener on a different
host-side port (for example `:8443`). Synthesising `https://{hostname}/v3`
in that case would diverge from what clients actually dial, so set
`publicEndpoint` explicitly whenever the external port is not 443.

When `spec.gateway` is unset, `.status.endpoint` is the cluster-local
Service URL (`http://{name}.{namespace}.svc.cluster.local:5000/v3`) so CRs
without external exposure still report a usable address.

> **Validation.** When both fields are set, the admission webhook requires
> `publicEndpoint` to be a valid `https://` URL whose host **equals**
> `spec.gateway.hostname`. A mismatched host, a non-`https` scheme, or an
> unparseable URL is rejected at `kubectl apply`. `spec.gateway.hostname`
> and `spec.gateway.parentRef.name` are both required when `spec.gateway`
> is present.

---

## Prerequisites

1. **Gateway API CRDs installed** (`gateway.networking.k8s.io/v1`). If the
   `HTTPRoute` CRD is absent, the operator disables the route watch and
   reports `HTTPRouteReady=False` with reason `GatewayAPINotInstalled` for
   any CR that sets `spec.gateway`. Install the standard Gateway API CRDs
   and restart the operator.

2. **A pre-provisioned `Gateway`** with an HTTPS listener whose hostname
   matches the hostname you will put in `spec.gateway.hostname`, and whose
   `allowedRoutes` admits the Keystone CR's namespace. The kind Quick Start
   ships exactly this as `Gateway/openstack-gw` in the `openstack`
   namespace.

3. **External routing to the Gateway.** Clients must resolve
   `spec.gateway.hostname` to the Gateway's address. On kind the
   `*.127-0-0-1.nip.io` wildcard resolves to `127.0.0.1` with no
   `/etc/hosts` edit — see [kind specifics](#kind-specifics) below.

This guide assumes a running Keystone CR. The
[Quick Start](../quick-start.md) reaches all of the above in one
`make deploy-infra`.

---

## 1. Configure `spec.gateway` and `publicEndpoint`

Add both blocks to the Keystone CR. The example below targets the kind
Quick Start Gateway (`openstack-gw`, listener hostname
`keystone.127-0-0-1.nip.io`) published on host port `8443`:

```yaml
apiVersion: keystone.openstack.c5c3.io/v1alpha1
kind: Keystone
metadata:
  name: keystone
  namespace: openstack
spec:
  # ... database, cache, fernet omitted ...
  bootstrap:
    adminUser: admin
    adminPasswordSecretRef:
      name: keystone-admin
    region: RegionOne
    # Catalog advertisement. Carries the host-side :8443 port that the
    # Gateway listener (in-cluster :443) is republished on.
    publicEndpoint: https://keystone.127-0-0-1.nip.io:8443/v3
  gateway:
    parentRef:
      name: openstack-gw          # the pre-existing Gateway
      # namespace: openstack      # defaults to the CR namespace
      # sectionName: https        # target one listener when the Gateway has several
    hostname: keystone.127-0-0-1.nip.io   # must match the Gateway listener hostname
    path: /                        # PathPrefix match; defaults to "/"
    # annotations:                 # passed through verbatim to the HTTPRoute
    #   example.com/timeout: "60s"
```

Field notes:

- **`parentRef.name`** — the Gateway to attach to (required). `namespace`
  defaults to the CR's namespace; a cross-namespace reference additionally
  requires a `ReferenceGrant` in the Gateway's namespace, which this
  operator does not manage. `sectionName` pins one named listener when the
  Gateway exposes several.
- **`hostname`** — the SNI / `Host` header the route matches; it must be
  one the Gateway listener admits, and it is the host the webhook requires
  `publicEndpoint` to use.
- **`path`** — `PathPrefix` match, defaults to `/`. A value missing a
  leading slash (e.g. `identity`) is normalised to `/identity`.
- **`annotations`** — copied to the generated `HTTPRoute` metadata verbatim
  for implementation-specific tuning (rate limits, timeouts, CORS) without
  extending the CRD.

---

## 2. Apply and wait

```bash
kubectl apply -f keystone.yaml
kubectl wait keystone/keystone -n openstack \
  --for=condition=Ready --timeout=5m
```

---

## 3. Verify

### 3.1 `HTTPRouteReady` and the projected endpoint

```bash
kubectl -n openstack get keystone keystone \
  -o jsonpath='{.status.conditions[?(@.type=="HTTPRouteReady")]}{"\n"}{.status.endpoint}{"\n"}'
```

Expected: `HTTPRouteReady` is `True` with reason `HTTPRouteAccepted`, and
`.status.endpoint` is the `publicEndpoint` you set
(`https://keystone.127-0-0-1.nip.io:8443/v3`). For the full condition tree
and how to read `HTTPRouteReady`, see
[Observability & Diagnostics](./observability.md#status-conditions).

### 3.2 The Gateway accepted the route

`HTTPRouteReady` mirrors the parent `Accepted` condition that the Gateway
controller — not the operator — writes onto the `HTTPRoute`. The route is
named after the CR (no suffix):

```bash
kubectl -n openstack get httproute keystone \
  -o jsonpath='{.status.parents[*].conditions[?(@.type=="Accepted")]}{"\n"}'
```

Expected: `Accepted` is `True`. A `False` here (or an empty `parents`
list) is what drives `HTTPRouteReady=False/HTTPRouteNotAccepted` — jump to
[Troubleshooting](#troubleshooting).

### 3.3 External reachability

Unauthenticated version probe (the kind Gateway terminates TLS with a
self-signed certificate, so `-k` / `--insecure` is expected):

```bash
curl -k https://keystone.127-0-0-1.nip.io:8443/v3
```

Authenticated token request through the public endpoint:

```bash
export OS_AUTH_URL=https://keystone.127-0-0-1.nip.io:8443/v3
export OS_USERNAME=admin
export OS_PASSWORD=$(kubectl get secret keystone-admin -n openstack -o jsonpath='{.data.password}' | base64 -d)
export OS_PROJECT_NAME=admin
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
openstack --insecure token issue
```

A token table confirms the full path — Gateway → `HTTPRoute` → Service →
Keystone — and that the catalog advertises a URL external clients can reach.

---

## kind specifics

The Quick Start wires the Gateway path end to end; the pieces that matter
when you adapt it:

- **`Gateway/openstack-gw`** lives in the `openstack` namespace alongside
  the Keystone CR. Same-namespace `parentRef` resolution needs no
  `ReferenceGrant`, and the listener `allowedRoutes` is `from: Same` — so
  the Keystone CR must be in `openstack` for the route to attach.
- **Single HTTPS listener** named `https` on **port 443**, hostname
  `keystone.127-0-0-1.nip.io`, TLS `mode: Terminate` with a self-signed
  certificate. TLS is terminated at the Gateway; the backend hop to the
  Keystone Service on port 5000 is plain HTTP inside the cluster.
- **NodePort `31443`.** The Envoy data-plane Service is pinned to
  `type: NodePort` on `31443`, and kind `extraPortMappings` bridges a host
  TCP port to it. `KIND_HOST_PORT` selects that host port: the default is
  `443`, and the Quick Start uses `8443` (a non-privileged port — no
  `vmnetd` helper on macOS). So the public URL is
  `https://keystone.127-0-0-1.nip.io:8443/v3`, and `publicEndpoint` must
  carry that `:8443`.
- **`*.127-0-0-1.nip.io` wildcard.** `nip.io` resolves any
  `<anything>.127-0-0-1.nip.io` name to `127.0.0.1`, so the hostname
  reaches the kind node's published port with no `/etc/hosts` edit and no
  `kubectl port-forward`.

---

## Troubleshooting

### `HTTPRouteReady=False`, reason `HTTPRouteNotAccepted`

**Symptom:** the condition message is `HTTPRoute not yet accepted by
Gateway` and stays that way across reconciles.

The operator created the `HTTPRoute` but the Gateway controller has not
reported `Accepted=True`. Inspect the route's parent status for the real
reason:

```bash
kubectl -n openstack get httproute keystone -o yaml | yq '.status.parents'
```

Common causes:

- **Hostname mismatch (`NoMatchingListenerHostname`).** `spec.gateway.hostname`
  is not admitted by any listener on the Gateway. The route hostname must
  match (or fall under) a listener hostname — on kind the listener is
  `keystone.127-0-0-1.nip.io`, so the CR hostname must equal it.
- **`parentRef` does not resolve.** `parentRef.name` (or `namespace`) points
  at a Gateway that does not exist, so no controller ever writes a parent
  status and `parents` stays empty. Confirm the Gateway exists:
  `kubectl -n openstack get gateway openstack-gw`.
- **Namespace not allowed.** The listener's `allowedRoutes.namespaces` does
  not admit the Keystone CR's namespace. The kind Gateway uses `from: Same`,
  so the CR must live in the Gateway's namespace; a cross-namespace setup
  needs both a permissive `allowedRoutes` and a `ReferenceGrant`.

### `HTTPRouteReady=False`, reason `GatewayAPINotInstalled`

The `gateway.networking.k8s.io/v1` `HTTPRoute` CRD is not installed. The
operator skips the route watch at startup, so installing the CRDs alone is
not enough — install the Gateway API standard CRDs **and restart the
operator** to pick up the watch.

### `kubectl apply` rejected by the webhook

**Symptom:** apply fails citing `spec.bootstrap.publicEndpoint`.

The webhook enforces, when both fields are set, that `publicEndpoint` is a
valid `https://` URL whose host equals `spec.gateway.hostname`. Fix the
scheme (must be `https`, because the Gateway listener terminates TLS) or
align the host with the hostname — including the absence of a port in the
host part (the port is allowed, the host is what must match).

### TLS errors from external clients

The kind Gateway presents a self-signed certificate. Either pass
`-k`/`--insecure` (as above), or trust the issuing CA
(`selfsigned-cluster-issuer`) on the client. Production deployments should
point the Gateway listener at a certificate from a CA the clients already
trust; that is a Gateway-side concern, independent of the Keystone CR.

---

## Disabling external exposure

Remove `spec.gateway` and re-apply. On the next reconcile the operator
deletes the `HTTPRoute`, sets `HTTPRouteReady=True` with reason
`HTTPRouteNotRequired`, and `.status.endpoint` reverts to the cluster-local
Service URL. Leaving `spec.bootstrap.publicEndpoint` set after removing the
route advertises a catalog URL that nothing routes to, so clear it too
unless another ingress serves that host.

---

## See also

- [Keystone CRD — GatewaySpec](../reference/keystone/keystone-crd.md#gatewayspec) —
  authoritative field schema, validation markers, and the Gateway API CRD
  prerequisite.
- [Observability & Diagnostics](./observability.md#status-conditions) —
  reading `HTTPRouteReady` within the full condition tree.
- [Quick Start](../quick-start.md) — the end-to-end kind path that
  provisions `openstack-gw` and reaches an authenticated token call.
- [Gateway API HTTPRoute concepts](https://gateway-api.sigs.k8s.io/api-types/httproute/).
