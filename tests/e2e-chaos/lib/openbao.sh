#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# The shared OpenBao of the kind stack, openbao-0 in shared-services, as the
# suites drive it, sourced by their scripts or libraries. The root token comes
# from Secret openbao-init-keys and reaches the pod on stdin, so it lands in
# neither a file nor an argv.

# bao_root_token: the root token in Secret shared-services/openbao-init-keys.
# Fails on an empty or null token.
bao_root_token() {
  local token
  token="$(kubectl get secret openbao-init-keys -n shared-services \
    -o jsonpath='{.data.init-output}' | base64 -d | jq -r '.root_token')" || token=""
  if [ -z "$token" ] || [ "$token" = "null" ]; then
    echo "ERROR: no OpenBao root token in Secret shared-services/openbao-init-keys" >&2
    return 1
  fi
  echo "$token"
}

# bao_exec <command...>: run a command in openbao-0 with the address and the CA
# the bao CLI needs. The command reads the caller's stdin.
bao_exec() {
  kubectl exec -i -n shared-services openbao-0 -- \
    env BAO_ADDR=https://127.0.0.1:8200 VAULT_CACERT=/openbao/tls/ca.crt "$@"
}

# bao_root <bao arguments>: run bao in openbao-0 with the root token. The token
# is the first line of the exec's stdin and whatever the caller pipes in
# follows it.
bao_root() {
  local token
  token="$(bao_root_token)" || return 1
  { echo "$token"; cat; } |
    bao_exec sh -c 'IFS= read -r BAO_TOKEN; export BAO_TOKEN; exec bao "$@"' _ "$@"
}
