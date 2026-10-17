#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of openbao-io-fault, sourced by its scripts from the suite directory
# with NAMESPACE set by Chainsaw. Every bao call goes through ../lib/openbao.sh,
# so the root token reaches openbao-0 on stdin.

# shellcheck source=tests/e2e-chaos/lib/faults.sh
. ../lib/faults.sh
# shellcheck source=tests/e2e-chaos/lib/openbao.sh
. ../lib/openbao.sh

KV=kv-v2/chaos/openbao-io
ACKED=openbao-io-acked

# kv_put <key> <value>: write v=<value> at $KV/<key>, bounded to 30 s so a
# storage that hangs fails the write instead of the step. Returns bao's exit
# status, 124 on the bound.
kv_put() {
  timeout 30 bash -c '. ../lib/openbao.sh && bao_root kv put "$1" "v=$2" </dev/null >/dev/null' \
    kv_put "${KV}/$1" "$2"
}

# kv_get <key>: the value v at $KV/<key>.
kv_get() {
  bao_root kv get -field=v "${KV}/$1" </dev/null
}

# acked: the <key>=<value> pairs recorded in ConfigMap openbao-io-acked.
acked() {
  kubectl get configmap "$ACKED" -n "$NAMESPACE" -o jsonpath='{.data.keys}' 2>/dev/null || true
}

# ack <key> <value>: record an acknowledged write in ConfigMap
# openbao-io-acked.
ack() {
  local pairs
  pairs="$(acked)"
  kubectl create configmap "$ACKED" -n "$NAMESPACE" --dry-run=client -o yaml \
    --from-literal=keys="${pairs:+${pairs} }$1=$2" | kubectl apply -f - >/dev/null
}

# kv_purge: delete every key under $KV with its versions. Prints the keys
# still listed afterwards; nothing when the path is empty.
kv_purge() {
  local key
  for key in $(bao_root kv list -format=json "$KV" </dev/null 2>/dev/null | jq -r '.[]' 2>/dev/null); do
    bao_root kv metadata delete "${KV}/${key}" </dev/null >/dev/null || true
  done
  bao_root kv list -format=json "$KV" </dev/null 2>/dev/null | jq -r '.[]' 2>/dev/null || true
}
