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

The console proxy needs its own hostname on this devstack. Its HTTPRoute matches
`PathPrefix: /`, which covers both the noVNC page at `/vnc_lite.html` and the
WebSocket endpoint at `/websockify`. The Nova API route also matches `/`, so a
distinct hostname keeps the two routes from competing for the same requests.

## Prerequisites

::: info Devstack
This guide is written against the **[Quick Start (ControlPlane)](../../quick-start-controlplane.md)** devstack. Stand it up first:

```bash
KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true make deploy-infra
```

Follow that tutorial through Step 7's **Boot a first server**, including its
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
- `curl` must be on `PATH` for Step 3. The quick start covers `kubectl` and the
  OpenStack CLI used in the other steps.

## Steps

### 1. Attach the console proxy to the Gateway

Set the console gateway on the ControlPlane, which projects it into the child's
`spec.consoleProxy.gateway`:

```bash
kubectl patch controlplane controlplane -n openstack --type merge \
  -p '{"spec":{"services":{"nova":{"consoleProxy":{"gateway":{"parentRef":{"name":"openstack-gw"},"hostname":"nova-console.127-0-0-1.nip.io"}}}}}}'
```

The nova operator creates the HTTPRoute `controlplane-nova-console` and reports
the Gateway's verdict on it as `ConsoleHTTPRouteReady`. That condition already
reads `True` before the patch, under the reason `HTTPRouteNotRequired`, so wait
for the reason an accepted route sets:

```bash
kubectl wait nova/controlplane-nova -n openstack --timeout=5m \
  --for=jsonpath='{.status.conditions[?(@.type=="ConsoleHTTPRouteReady")].reason}'=HTTPRouteAccepted
kubectl get httproute controlplane-nova-console -n openstack
```

The same change rewrites `[vnc] novncproxy_base_url` from the cluster-local
Service to `https://nova-console.127-0-0-1.nip.io/vnc_lite.html`, in the
control plane's `nova.conf` and in the compute contract alike. A `nova-compute`
builds a console URL from its own copy of that option, read at start. Wait for
the compute contract to contain the new URL before restarting the fake compute;
if the check finds no matching line, let Nova reconcile and rerun it:

```bash
kubectl get secret controlplane-nova-compute-config -n openstack \
  -o jsonpath='{.data.nova-compute\.conf}' | base64 -d \
  | grep -F 'novncproxy_base_url = https://nova-console.127-0-0-1.nip.io/vnc_lite.html'
kubectl rollout restart deploy/controlplane-fake-compute -n openstack
kubectl rollout status deploy/controlplane-fake-compute -n openstack --timeout=5m
```

A compute that kept its old configuration keeps answering with the cluster-local
URL, which no browser outside the cluster reaches.

### 2. Fetch a console URL

The URL contains a short-lived console token. Do not share it in tickets or
shared terminal logs.

```bash
URL=$(openstack --insecure console url show --novnc demo-server -f value -c url)
echo "$URL"
```

Confirm the command prints a URL before continuing. The URL reads
`https://nova-console.127-0-0-1.nip.io/vnc_lite.html?path=%3Ftoken%3D<token>`.
It carries no port, because the operator renders the console base URL from the
gateway hostname alone and assumes the listener answers on 443. With
`KIND_HOST_PORT=8443` the Gateway answers on host port 8443, so insert `:8443`
after the hostname before you open the URL or reuse it. Keep this shell open;
the next block derives `TOKEN` from `URL`:

```bash
URL_WITH_PORT="${URL/nip.io\//nip.io:8443/}"
echo "$URL_WITH_PORT"
TOKEN=$(sed -n 's/.*token%3D\([0-9a-f-]*\).*/\1/p' <<<"$URL")
test -n "$TOKEN" && echo "Token extracted"
```

If the check does not print `Token extracted`, fetch a new URL and check its
format before continuing. On the default `KIND_HOST_PORT=443` the URL works as
printed. By default, a console token lives for 600 seconds
(`[consoleauth] token_ttl`), so fetch a fresh URL if the checks below run later
than that.

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
socket opens on `/websockify`, the path websockify serves its endpoint under.
The proxy closes the upgraded socket after the timeout. The command prints the
response line even if curl exits nonzero:

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
  `{name}-novncproxy` NetworkPolicy. Confirm which source address the
  hypervisors see and allow only that address through any other firewall.
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
- **A TLS listener for a distinct console hostname.** The kind overlay uses a
  dedicated listener and certificate. Another Gateway can reuse a listener if
  it accepts this hostname and presents a certificate that covers it. Do not
  reuse the Nova API hostname when both routes match `/`: the Gateway can send
  console requests to the older route.
- **A root path match.** The Nova operator accepts only an empty `path` or `/`,
  which produces a `PathPrefix: /` match for both `/vnc_lite.html` and
  `/websockify`. A non-root prefix such as `/console` matches neither endpoint,
  so the ControlPlane webhook rejects it.
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
