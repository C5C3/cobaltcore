#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of openbao-tenant-policy-narrowed, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.
#
# The suite narrows OpenBao policy eso-tenant, the identity of every per-tenant
# SecretStore, and restores it from ConfigMap policy-narrowed-orig.
# setup-policies.sh substitutes the auth-mount accessor when it writes the
# policy, so the file in deploy/openbao/policies/ is not what OpenBao holds:
# the suite captures the live text and writes that back.

POLICY=eso-tenant
ORIG=policy-narrowed-orig

# bao_root, with the root token on stdin.
# shellcheck source=tests/e2e-chaos/lib/openbao.sh
. "$(dirname "${BASH_SOURCE[0]}")/../lib/openbao.sh"

# narrow: copy the policy on stdin to stdout without the one stanza that grants
# read on the caller's own Keystone subtree,
# kv-v2/data/openstack/keystone/{{<namespace template>}}/*. The fernet-key,
# credential-key and app-credential stanzas under the same prefix end
# otherwise and stay. Exits 1 unless exactly one stanza matched.
narrow() {
  awk '
    index($0, "path \"kv-v2/data/openstack/keystone/{{") == 1 &&
      substr($0, length($0) - 4) == "/*\" {" { n++; skip = 1; next }
    skip { if ($0 == "}") skip = 0; next }
    { print }
    END {
      if (n != 1) {
        print "FAIL: narrowing matched " n + 0 " stanzas" > "/dev/stderr"
        exit 1
      }
    }
  '
}

# restore_policy: write the captured policy back, when there is a capture.
restore_policy() {
  if ! kubectl get configmap "$ORIG" -n "$NAMESPACE" >/dev/null 2>&1; then
    echo "no capture of policy ${POLICY}; nothing to restore"
    return 0
  fi
  kubectl get configmap "$ORIG" -n "$NAMESPACE" -o 'jsonpath={.data.policy\.hcl}' |
    bao_root policy write "$POLICY" -
}
