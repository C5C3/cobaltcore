#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of cert-manager-outage, sourced by its scripts from the suite
# directory with NAMESPACE set by Chainsaw.
#
# The suite compares three things across a forced renewal: the revision of
# Certificate keystone-nip-io-tls, the leaf certificate in its Secret, and the
# leaf certificate the https listener of Gateway openstack-gw serves. A leaf is
# named by the SHA-256 of its DER encoding.

CERT=keystone-nip-io-tls
HOST=keystone.127-0-0-1.nip.io
STATE=cert-manager-outage-state
WORKLOAD_POD=chaos-wl-cert-manager-outage

# shellcheck source=tests/e2e-chaos/lib/gateway.sh
. "$(dirname "${BASH_SOURCE[0]}")/../lib/gateway.sh"

# cert_field <jsonpath>: one field of the Certificate.
cert_field() {
  kubectl get certificate "$CERT" -n "$NAMESPACE" -o "jsonpath=$1"
}

# secret_leaf: the hash of the leaf in Secret keystone-nip-io-tls.
secret_leaf() {
  kubectl get secret "$CERT" -n "$NAMESPACE" -o 'jsonpath={.data.tls\.crt}' |
    base64 -d | openssl x509 -outform DER | sha256sum | cut -d' ' -f1
}

# served_leaf: the hash of the leaf the listener serves for the Keystone
# hostname. The handshake runs in the workload pod, which reaches the
# ClusterIP, and checks nothing: the leaf is self-signed. A caller that probes
# in a loop sets GW_IP once, so each probe skips the Service lookup.
served_leaf() {
  local out
  out="$(kubectl exec "$WORKLOAD_POD" -n "$NAMESPACE" -- python3 -c '
import hashlib, socket, ssl, sys
ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ctx.check_hostname = False
ctx.verify_mode = ssl.CERT_NONE
with socket.create_connection((sys.argv[1], 443), timeout=10) as sock:
    with ctx.wrap_socket(sock, server_hostname=sys.argv[2]) as tls:
        print("SERVED " + hashlib.sha256(tls.getpeercert(binary_form=True)).hexdigest())
' "${GW_IP:-$(gateway_ip)}" "$HOST")"
  sed -n 's/^SERVED //p' <<<"$out"
}

# baseline <key>: a value step 2 stored in ConfigMap cert-manager-outage-state.
baseline() {
  kubectl get configmap "$STATE" -n "$NAMESPACE" -o "jsonpath={.data.$1}"
}
