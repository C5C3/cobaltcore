---
title: Expose the Console Proxy
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: Expose the Console Proxy

This guide publishes the noVNC console proxy of the projected Nova through the
shared Gateway, fetches a console URL for a server, and follows the URL through
the proxy as far as a devstack without a hypervisor allows. It closes with what
a real deployment needs beyond that, and how to switch the proxy off.

The console proxy takes a hostname of its own. The URL the compute API hands a
browser is `https://<host>/vnc_lite.html?path=%3Ftoken%3D<token>`, and the
WebSocket the page then opens starts on `/` as well, so the two share one path
and only a separate hostname can route them to the proxy.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through Step 6's **Boot a first server**, including its
optional block **Optional: boot a server on a fake compute**, and skip that
block's cleanup, so `demo-server` is `ACTIVE` on the fake compute
`controlplane-fake-compute` and the `OS_*` variables of the token-issue step are
exported. Every resource name in the examples below is one that devstack
produces.
:::

- The kind overlay already carries the listener this guide attaches to:
  `https-nova-console` on the Gateway `openstack-gw`, for the hostname
  `nova-console.127-0-0-1.nip.io`, with its own certificate.
- If your host cannot resolve `*.nip.io`, add
  `127.0.0.1 nova-console.127-0-0-1.nip.io` to `/etc/hosts`, the way the quick
  start's DNS tip does for its other hostnames.

## Steps

### 1. Attach the console proxy to the Gateway

Set the console gateway on the ControlPlane, which projects it into the child's
`spec.consoleProxy.gateway`:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"consoleProxy":{"gateway":{"parentRef":{"name":"openstack-gw"},"hostname":"nova-console.127-0-0-1.nip.io"}}}}}}'
```

The nova operator creates the HTTPRoute `controlplane-nova-console` and reports
the Gateway's verdict on it as `ConsoleHTTPRouteReady`:

```bash
kubectl wait nova/controlplane-nova -n openstack \
  --for=condition=ConsoleHTTPRouteReady --timeout=5m
kubectl get httproute controlplane-nova-console -n openstack
```

The same change rewrites `[vnc] novncproxy_base_url` from the cluster-local
Service to `https://nova-console.127-0-0-1.nip.io/vnc_lite.html`, in the
control plane's `nova.conf` and in the compute contract alike. A `nova-compute`
builds a console URL from its own copy of that option, read at start, so
restart the fake compute once the contract carries the new value:

```bash
kubectl get secret controlplane-nova-compute-config -n openstack \
  -o jsonpath='{.data.nova-compute\.conf}' | base64 -d | grep novncproxy_base_url
kubectl rollout restart deploy/controlplane-fake-compute -n openstack
kubectl rollout status deploy/controlplane-fake-compute -n openstack --timeout=5m
```

A compute that kept its old configuration keeps answering with the cluster-local
URL, which no browser outside the cluster reaches.

### 2. Fetch a console URL

```bash
URL=$(openstack --insecure console url show --novnc demo-server -f value -c url)
echo "$URL"
```

The URL reads
`https://nova-console.127-0-0-1.nip.io/vnc_lite.html?path=%3Ftoken%3D<token>`.
It carries no port, because the operator renders the console base URL from the
gateway hostname alone and assumes the listener answers on 443. With
`KIND_HOST_PORT=8443` the Gateway answers on host port 8443, so insert `:8443`
after the hostname before you open the URL or reuse it:

```bash
echo "${URL/nip.io\//nip.io:8443/}"
TOKEN=$(sed -n 's/.*token%3D\([0-9a-f-]*\).*/\1/p' <<<"$URL")
```

On the default `KIND_HOST_PORT=443` the URL works as printed. A token lives for
600 seconds (`[consoleauth] token_ttl`), so fetch a fresh URL when the checks
below run later than that.

### 3. Verify the path through the proxy

The client page the proxy serves itself answers `200`. Envoy can take a few
seconds to pick up a new route, so retry briefly if the first request answers
`404`:

```bash
curl -sk -o /dev/null -w '%{http_code}\n' \
  https://nova-console.127-0-0-1.nip.io:8443/vnc_lite.html
```

The WebSocket upgrade the page makes next, spelled out with curl, answers
`HTTP/1.1 101`. `--http1.1` pins the protocol version an upgrade needs, and the
socket opens on `/websockify`, the path websockify serves its endpoint under:

```bash
curl -sk -i -N --http1.1 --max-time 10 \
  -H 'Connection: Upgrade' \
  -H 'Upgrade: websocket' \
  -H 'Sec-WebSocket-Version: 13' \
  -H "Sec-WebSocket-Key: $(head -c 16 /dev/urandom | base64)" \
  "https://nova-console.127-0-0-1.nip.io:8443/websockify?token=${TOKEN}" | head -1
```

curl exits non-zero once the proxy drops the switched socket; the first line is
what counts. A `101` only says the Gateway routed the upgrade. The proxy log
says whether the token resolved:

```bash
kubectl logs deploy/controlplane-nova-novncproxy -n openstack --tail=200 \
  | grep -E 'connect info:|connecting to:'
```

Expect a `connect info:` line for the token and
`connecting to: fakevncconsole.com:6969`. That is the end of the proxy's own
work: it looked the token up in the cell database and dialled the address the
compute registered for the instance. The connection then fails, because the fake
driver reports that address for every instance and runs no VNC server behind
it. A browser opening the URL shows the noVNC page and then a failed
connection. With a real hypervisor the same path ends in the instance's screen.

## What a real deployment needs

The devstack stops at the fake console. Behind a real hypervisor the proxy has
more to reach:

- **Egress from the proxy to the hypervisors' VNC ports**, TCP 5900-65535. With
  `spec.networkPolicy` set on a `Nova`, the operator opens it in the
  `{name}-novncproxy` NetworkPolicy; any other firewall between the proxy pods
  and the hypervisors needs the same range.
- **A secured hop from the proxy to the hypervisors.** The console token admits
  a browser to the proxy and no further. libvirt's VNC servers take no password
  or TLS by default, and `[vnc] auth_schemes` defaults to `none`, so any host
  that reaches a hypervisor's VNC ports gets an instance's screen and keyboard
  without a token. Let those ports accept connections from the proxy pods'
  addresses alone. VeNCrypt is the upstream way to authenticate and encrypt the
  hop: `vnc_tls` in the hypervisor's `qemu.conf`, and on the proxy
  `[vnc] auth_schemes = vencrypt` with a client certificate
  (`vencrypt_client_key`, `vencrypt_client_cert`, `vencrypt_ca_certs`). The nova
  operator does not mount a client certificate into the proxy pod yet.
- **A routable `[vnc] server_proxyclient_address` on the compute side.** It is
  the address the compute registers for an instance's console and the address
  the proxy dials, so the proxy pods have to reach it. The compute contract
  leaves it out; the compute deployment sets it.
- **One Gateway listener and one certificate per hostname.** The console route
  may not share a listener hostname with the API's or the metadata API's route:
  the routes would tie and the Gateway would hand every request to the older one.
- **A `path` that is empty or `/`.** The console page and the WebSocket both sit
  at the root of the console host, so a prefix route matches neither, and the
  ControlPlane webhook rejects one.
- **The cell database.** Console tokens live in the cell schema, which is why the
  proxy carries the cell connection. A proxy that cannot reach the database
  rejects every token.

## Disabling the console proxy

Switch the proxy off on the ControlPlane, and drop the gateway in the same patch:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"consoleProxy":{"enabled":false,"gateway":null}}}}}'
```

The nova operator deletes the `controlplane-nova-novncproxy` Deployment and
Service, the HTTPRoute `controlplane-nova-console` and, where one exists, the
proxy's NetworkPolicy. It renders `[vnc] enabled = false` into `nova.conf` and
into the compute contract, and `ConsoleProxyReady` reads `True` under
`ConsoleProxyDisabled`. The ControlPlane rejects a `replicas` or a `gateway`
beside `enabled: false` (`replicas and gateway must not be set when
consoleProxy.enabled is false`), which is why the patch removes the gateway.

Restart the fake compute again so it reads the disabled console, the same way
as in step 1. `openstack console url show` then fails for every server.

To turn the proxy back on, set `enabled` back to `true` (or remove the
`consoleProxy` block) and repeat step 1.

## Tested by

The console-proxy suite runs the path of steps 1 to 3 against a standalone
`Nova` CR on a fake compute: it fetches the console URL through the compute API,
checks that it names the gateway hostname, loads `/vnc_lite.html`, upgrades
`/websockify` with a valid and an invalid token, and reads the proxy log for
`connect info:` and `connecting to: fakevncconsole.com:6969`. The gateway smoke
suite exposes all three Nova routes on the kind listeners this guide uses.

```bash
chainsaw test --test-dir tests/e2e/nova/console-proxy
chainsaw test --test-dir tests/e2e/nova/gateway-quick-start-smoke
```
