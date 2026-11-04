# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The token checks of steps 7 and 8 of chainsaw-test.yaml source this file from
# bash under set -euo pipefail. It sets ADMIN_PASS, the admin password, and
# POD, the API pod of keystone-fernet, and defines prod_keys, pod_keys,
# wait_projection, issue_token and validate.
# shellcheck shell=bash

ADMIN_PASS=$(kubectl get secret keystone-admin -n openstack \
  -o jsonpath='{.data.password}' | base64 -d)
if [ -z "$ADMIN_PASS" ]; then
  echo "FAIL: keystone-admin Secret has empty or missing password field"
  exit 1
fi
POD=$(kubectl get pods -n openstack \
  -l app.kubernetes.io/name=keystone,app.kubernetes.io/instance=keystone-fernet,pod-template-hash \
  -o jsonpath='{.items[0].metadata.name}')

# prod_keys: the key indices of the production Secret, ascending.
prod_keys() {
  kubectl get secret keystone-fernet-fernet-keys -n openstack -o json \
    | jq -r '.data // {} | keys | sort_by(tonumber) | join(" ")'
}
# pod_keys: the digit-named entries of the API pod's key directory,
# ascending. The kubelet projection adds ..data entries, which the
# filter drops; python3 lists the directory because the image does
# not promise ls.
pod_keys() {
  kubectl exec -n openstack "$POD" -- python3 -c 'import os; print(" ".join(sorted((n for n in os.listdir("/etc/keystone/fernet-keys") if n.isdigit()), key=int)))'
}
# wait_projection <keys> <seconds>: poll until the pod projects <keys>.
# A token issued before the projection completes would be signed
# with the old primary key.
wait_projection() {
  local want="$1" limit="$2" have=""
  local until_s=$((SECONDS + limit))
  while :; do
    have=$(pod_keys || true)
    [ "$have" = "$want" ] && return 0
    if [ "$SECONDS" -ge "$until_s" ]; then
      echo "FAIL: pod $POD projects keys [$have], Secret holds [$want] after ${limit}s"
      return 1
    fi
    sleep 2
  done
}
# issue_token: an admin token, the request of step 4. The password
# goes over stdin, never into an argument.
issue_token() {
  printf '%s' "$ADMIN_PASS" | kubectl exec -i -n openstack "$POD" -- python3 -c '
import json, sys, urllib.request
pw = sys.stdin.read()
body = json.dumps({"auth": {"identity": {"methods": ["password"], "password": {"user": {"name": "admin", "domain": {"id": "default"}, "password": pw}}}}}).encode()
req = urllib.request.Request("http://localhost:5000/v3/auth/tokens", data=body, headers={"Content-Type": "application/json"})
with urllib.request.urlopen(req) as r:
    sys.stdout.write(r.headers.get("X-Subject-Token", ""))
'
}
# validate <token>: the HTTP code of GET /v3/auth/tokens with the
# token as both X-Auth-Token and X-Subject-Token, the request of
# step 4. The token goes over stdin, never into an argument.
validate() {
  printf '%s' "$1" | kubectl exec -i -n openstack "$POD" -- python3 -c '
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
