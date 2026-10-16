#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of envoy-gateway-outage, sourced by its scripts from the suite
# directory with NAMESPACE set by Chainsaw.

ROUTE=chaos-eg-late
LATE_HOST=placement.127-0-0-1.nip.io
WORKLOAD_POD=chaos-wl-envoy-gateway-outage

# shellcheck source=tests/e2e-chaos/lib/gateway.sh
. "$(dirname "${BASH_SOURCE[0]}")/../lib/gateway.sh"

# route_accepted: the status of the Accepted condition openstack-gw wrote on
# the late route, empty while no controller has written one.
route_accepted() {
  kubectl get httproute "$ROUTE" -n "$NAMESPACE" \
    -o jsonpath='{.status.parents[*].conditions[?(@.type=="Accepted")].status}'
}

# late_status: the HTTP status of GET /v3 through the proxy with SNI and Host
# set to the late route's hostname, from the workload pod, which reaches the
# ClusterIP. Prints the status, or "none" and the error when the handshake or
# the connection failed. The listener certificate is self-signed, so nothing
# is verified. A caller that probes in a loop sets GW_IP once, so each probe
# skips the Service lookup.
late_status() {
  kubectl exec "$WORKLOAD_POD" -n "$NAMESPACE" -- python3 -c '
import socket, ssl, sys
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
host = sys.argv[2]
try:
    with socket.create_connection((sys.argv[1], 443), timeout=10) as sock:
        with ctx.wrap_socket(sock, server_hostname=host) as tls:
            tls.sendall(("GET /v3 HTTP/1.1\r\nHost: " + host
                         + "\r\nConnection: close\r\n\r\n").encode())
            words = tls.makefile("rb").readline().decode().split()
            print(words[1] if len(words) > 1 else "none")
except OSError as exc:
    print("none " + repr(exc))
' "${GW_IP:-$(gateway_ip)}" "$LATE_HOST"
}
