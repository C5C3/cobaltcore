#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of keystoneuser-rotation-keystone-outage, sourced by its scripts
# from the suite directory with NAMESPACE set by Chainsaw, after
# ../lib/controlplane.sh.
#
# The plane cp-ku-rot lives in NAMESPACE, the KeystoneUser order ku-rot and
# its delivered Secret ku-rot-credentials in NAMESPACE-tenant. The K-ORC User
# and the password Secrets <prefix>password-v<N> the operator writes live in
# NAMESPACE (tests/e2e/c5c3/keystone-user/ describes the layout). State that
# outlives a step goes into ConfigMap ku-rot-state in NAMESPACE.

ORDER=ku-rot
TENANT_NS="${NAMESPACE}-tenant"
CREDS=ku-rot-credentials
GEN1_COPY=ku-rot-gen1-credentials
STATE=ku-rot-state

# ku_field <jsonpath>: one field of the order. A failed read fails the call.
ku_field() {
  kubectl get keystoneuser "$ORDER" -n "$TENANT_NS" -o "jsonpath=$1"
}

# ku_condition <type>: "<status>/<reason>" of one condition of the order.
ku_condition() {
  ku_field "{.status.conditions[?(@.type=='$1')].status}/{.status.conditions[?(@.type=='$1')].reason}"
}

# prefix: the common prefix of the order's children, read off the K-ORC User
# the operator names <prefix>user.
prefix() {
  local user
  user="$(kubectl get users.openstack.k-orc.cloud -n "$NAMESPACE" \
    -l "c5c3.io/keystoneuser-name=${ORDER},c5c3.io/keystoneuser-namespace=${TENANT_NS}" \
    -o jsonpath='{.items[0].metadata.name}')"
  if [ -z "$user" ]; then
    echo "FAIL: no K-ORC User of the order in ${NAMESPACE}" >&2
    return 1
  fi
  printf '%s' "${user%user}"
}

# password_ref <prefix>: the password Secret the K-ORC User points at.
password_ref() {
  kubectl get users.openstack.k-orc.cloud "${1}user" -n "$NAMESPACE" -o jsonpath='{.spec.resource.passwordRef}'
}

# secret_hash <secret>: the SHA-256 of the sorted .data of a Secret in the
# tenant namespace. A Secret without data fails the call.
secret_hash() {
  local data
  data="$(kubectl get secret "$1" -n "$TENANT_NS" -o json | jq -cS '.data // empty')"
  if [ -z "$data" ]; then
    echo "FAIL: Secret $1 in ${TENANT_NS} holds no data" >&2
    return 1
  fi
  printf '%s' "$data" | sha256sum | cut -d' ' -f1
}

# copy_gen1: a copy of the delivered Secret as ku-rot-gen1-credentials, with
# its type and data alone, so it carries no label the operator selects on and
# no owner it would be collected with. Server-side apply keeps the data out of
# the last-applied-configuration annotation.
copy_gen1() {
  kubectl get secret "$CREDS" -n "$TENANT_NS" -o json |
    jq --arg name "$GEN1_COPY" --arg ns "$TENANT_NS" \
      '{apiVersion: "v1", kind: "Secret", metadata: {name: $name, namespace: $ns}, type: .type, data: .data}' |
    kubectl apply --server-side -f - >/dev/null
}

# run_auth_job <job> <secret> <complete|failed>: render 03-auth-probe-job.yaml
# for <secret>, run it and require the outcome. A Job expected to fail must
# say HTTP 401 in its log: any other failure is no proof of a revoked
# password.
run_auth_job() {
  local job="$1" secret="$2" want="$3" log state deadline
  kubectl delete job "$job" -n "$TENANT_NS" --ignore-not-found --wait=true >/dev/null
  sed -e "s|@NAME@|${job}|g" -e "s|@SECRET@|${secret}|g" -e "s|@NS@|${NAMESPACE}|g" 03-auth-probe-job.yaml |
    kubectl apply -f - >/dev/null
  deadline=$(($(date +%s) + 240))
  while true; do
    state="$(kubectl get job "$job" -n "$TENANT_NS" \
      -o 'jsonpath={.status.conditions[?(@.status=="True")].type}' 2>/dev/null || true)"
    case " ${state} " in
      *" Complete "* | *" Failed "*) break ;;
    esac
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: Job ${job} neither completed nor failed within 240s"
      kubectl get pods -n "$TENANT_NS" -l "job-name=${job}" -o wide 2>&1 || true
      return 1
    fi
    sleep 3
  done
  # The log of a successful run holds the token, so it is printed only when
  # the outcome is wrong.
  log="$(kubectl logs -n "$TENANT_NS" "job/${job}" 2>&1 || true)"
  case "${want}:${state}" in
    complete:*Complete*)
      if ! grep -q 'OK: the credentials authenticate' <<<"$log"; then
        echo "FAIL: Job ${job} completed without its OK line"
        return 1
      fi
      echo "OK: Job ${job} authenticated with ${secret}"
      ;;
    failed:*Failed*)
      if ! grep -q 'HTTP 401' <<<"$log"; then
        echo "FAIL: Job ${job} failed without HTTP 401 in its log"
        echo "$log"
        return 1
      fi
      echo "OK: Job ${job} was refused with HTTP 401"
      ;;
    *)
      echo "FAIL: Job ${job} ended ${state:-<none>}, want ${want}"
      grep -v '^| id ' <<<"$log" || true
      return 1
      ;;
  esac
}

# state_set <key> <value> and state_get <key>: ConfigMap ku-rot-state.
state_set() {
  kubectl patch configmap "$STATE" -n "$NAMESPACE" --type=merge -p "{\"data\":{\"$1\":\"$2\"}}" >/dev/null
}
state_get() {
  kubectl get configmap "$STATE" -n "$NAMESPACE" -o "jsonpath={.data.$1}"
}
