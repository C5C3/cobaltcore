#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Helpers of fernet-rotation-operator-outage, sourced by its scripts from the
# suite directory with NAMESPACE set by Chainsaw.
#
# The token checks are those of ../../e2e/keystone/fernet-rotation/
# (key-helpers.sh and step 4), taken to both API pods of
# keystone-chaos-fernet: a token goes into and out of an API pod over stdin,
# never into an argument. The Secret helpers read the production Secret
# keystone-chaos-fernet-fernet-keys and the staging Secret
# keystone-chaos-fernet-fernet-keys-rotation the rotation CronJob writes and
# the operator promotes (internal/common/rotation/rotation.go). State that
# outlives a step goes into ConfigMap fernet-outage-state, the token T1 into
# Secret fernet-outage-t1.

KS=keystone-chaos-fernet
PROD=keystone-chaos-fernet-fernet-keys
STAGING=keystone-chaos-fernet-fernet-keys-rotation
CRONJOB=keystone-chaos-fernet-fernet-rotate
STATE=fernet-outage-state
T1_SECRET=fernet-outage-t1
COMPLETED_AT=cobaltcore.c5c3.io/rotation-completed-at
# The pod-template-hash label is set on Deployment-owned pods alone, so the
# selector leaves out the rotation Job pods that share the other two labels.
API_SELECTOR=app.kubernetes.io/name=keystone,app.kubernetes.io/instance=keystone-chaos-fernet,pod-template-hash

# api_pods: the names of the API pods, one per line.
api_pods() {
  kubectl get pods -n "$NAMESPACE" -l "$API_SELECTOR" \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

# api_uids: the UIDs of the API pods, sorted, space-separated.
api_uids() {
  kubectl get pods -n "$NAMESPACE" -l "$API_SELECTOR" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort | xargs
}

# admin_password: the admin password the Keystone bootstrapped with.
admin_password() {
  kubectl get secret keystone-admin -n "$NAMESPACE" -o jsonpath='{.data.password}' | base64 -d
}

# issue_token <pod>: an admin token issued by <pod>.
issue_token() {
  admin_password | kubectl exec -i -n "$NAMESPACE" "$1" -- python3 -c '
import json, sys, urllib.request
pw = sys.stdin.read()
body = json.dumps({"auth": {"identity": {"methods": ["password"], "password": {"user": {"name": "admin", "domain": {"id": "default"}, "password": pw}}}}}).encode()
req = urllib.request.Request("http://localhost:5000/v3/auth/tokens", data=body, headers={"Content-Type": "application/json"})
with urllib.request.urlopen(req) as r:
    sys.stdout.write(r.headers.get("X-Subject-Token", ""))
'
}

# validate <pod> <token>: the HTTP code <pod> answers GET /v3/auth/tokens with,
# the token as both X-Auth-Token and X-Subject-Token.
validate() {
  printf '%s' "$2" | kubectl exec -i -n "$NAMESPACE" "$1" -- python3 -c '
import sys, urllib.request, urllib.error
tok = sys.stdin.read()
req = urllib.request.Request("http://localhost:5000/v3/auth/tokens", headers={"X-Auth-Token": tok, "X-Subject-Token": tok})
try:
    with urllib.request.urlopen(req) as r:
        sys.stdout.write(str(r.status))
except urllib.error.HTTPError as e:
    sys.stdout.write(str(e.code))
'
}

# validate_everywhere <token> <label>: the token answers 200 on every API pod.
validate_everywhere() {
  local pod code
  for pod in $(api_pods); do
    code="$(validate "$pod" "$1")"
    if [ "$code" != "200" ]; then
      echo "FAIL: ${2} answers HTTP ${code} on ${pod}, want 200"
      return 1
    fi
    echo "OK: ${2} answers 200 on ${pod}"
  done
}

# prod_keys: the key indices of the production Secret, ascending.
prod_keys() {
  kubectl get secret "$PROD" -n "$NAMESPACE" -o json |
    jq -r '.data // {} | keys | sort_by(tonumber) | join(" ")'
}

# pod_keys <pod>: the digit-named entries of the pod's key directory,
# ascending; the kubelet projection adds ..data entries, which the filter
# drops.
pod_keys() {
  kubectl exec -n "$NAMESPACE" "$1" -- python3 -c 'import os; print(" ".join(sorted((n for n in os.listdir("/etc/keystone/fernet-keys") if n.isdigit()), key=int)))'
}

# wait_projection <seconds>: poll until every API pod projects the key set of
# the production Secret.
wait_projection() {
  local limit="$1" want pod have deadline
  deadline=$(($(date +%s) + limit))
  while true; do
    want="$(prod_keys)"
    have=""
    for pod in $(api_pods); do
      if [ "$(pod_keys "$pod" 2>/dev/null || true)" != "$want" ]; then
        have="${have} ${pod}"
      fi
    done
    if [ -z "$have" ]; then
      echo "OK: every API pod projects the keys [${want}]"
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "FAIL: after ${limit}s these API pods do not project the keys [${want}]:${have}"
      return 1
    fi
    sleep 2
  done
}

# data_hash <secret>: the SHA-256 of the Secret's sorted .data. A Secret
# without data fails the call: sha256sum of nothing would be a hash too.
data_hash() {
  local data
  data="$(kubectl get secret "$1" -n "$NAMESPACE" -o json | jq -cS '.data // empty')"
  if [ -z "$data" ]; then
    echo "FAIL: Secret $1 holds no data" >&2
    return 1
  fi
  printf '%s' "$data" | sha256sum | cut -d' ' -f1
}

# staging_state: absent, empty (no data and no completion annotation),
# staged:<data hash> (data and the annotation), or partial.
staging_state() {
  local json data annotated
  if ! json="$(kubectl get secret "$STAGING" -n "$NAMESPACE" -o json 2>&1)"; then
    case "$json" in
      *NotFound*)
        echo absent
        return 0
        ;;
    esac
    echo "FAIL: reading Secret ${STAGING} failed: ${json}" >&2
    return 1
  fi
  data="$(printf '%s' "$json" | jq -cS '.data // {} | select(length > 0)')"
  annotated="$(printf '%s' "$json" | jq -r --arg a "$COMPLETED_AT" '.metadata.annotations // {} | has($a)')"
  if [ -z "$data" ] && [ "$annotated" = "false" ]; then
    echo empty
  elif [ -n "$data" ] && [ "$annotated" = "true" ]; then
    echo "staged:$(printf '%s' "$data" | sha256sum | cut -d' ' -f1)"
  else
    echo partial
  fi
}

# rotated_count: the summed count of FernetKeysRotated events on the Keystone.
# The recorder raises an event's count on a repeat; an event without one
# counts as 1.
rotated_count() {
  local uid
  uid="$(kubectl get keystone "$KS" -n "$NAMESPACE" -o jsonpath='{.metadata.uid}')"
  kubectl get events -n "$NAMESPACE" \
    --field-selector "involvedObject.uid=${uid},reason=FernetKeysRotated" -o json |
    jq '[.items[] | .count // 1] | add // 0'
}

# stash_t1 <token> and read_t1: Secret fernet-outage-t1. The token reaches
# kubectl over stdin, and server-side apply keeps it out of the
# last-applied-configuration annotation.
stash_t1() {
  printf '%s' "$1" | kubectl create secret generic "$T1_SECRET" -n "$NAMESPACE" \
    --from-file=token=/dev/stdin --dry-run=client -o yaml | kubectl apply --server-side -f - >/dev/null
}
read_t1() {
  kubectl get secret "$T1_SECRET" -n "$NAMESPACE" -o jsonpath='{.data.token}' | base64 -d
}

# state_set <key> <value> and state_get <key>: ConfigMap fernet-outage-state.
state_set() {
  kubectl patch configmap "$STATE" -n "$NAMESPACE" --type=merge -p "{\"data\":{\"$1\":\"$2\"}}" >/dev/null
}
state_get() {
  kubectl get configmap "$STATE" -n "$NAMESPACE" -o "jsonpath={.data.$1}"
}

# run_rotation <job>: one rotation Job from the CronJob, waited to complete.
run_rotation() {
  kubectl delete job "$1" -n "$NAMESPACE" --ignore-not-found
  kubectl create job "$1" -n "$NAMESPACE" --from="cronjob/${CRONJOB}"
  if ! kubectl wait --for=condition=complete "job/$1" -n "$NAMESPACE" --timeout=120s; then
    echo "FAIL: rotation Job $1 did not complete within 120s"
    kubectl logs -n "$NAMESPACE" "job/$1" --all-containers --tail=60 2>&1 || true
    return 1
  fi
  echo "OK: rotation Job $1 completed"
}
