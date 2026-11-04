# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The steps of chainsaw-test.yaml source this file from bash under
# set -euo pipefail. It defines config_cm, wait_config_flip, dashboard_pod,
# dashboard_get and horizon_logs for the Horizon CR horizon-logging.
# shellcheck shell=bash

# config_cm: the ConfigMap the Deployment's config volume names.
config_cm() {
  kubectl get deployment horizon-logging -n openstack \
    -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}'
}

# wait_config_flip BEFORE MARKER: waits up to 90 s for the config volume to
# name a ConfigMap other than BEFORE and sets AFTER_CM to it. It fails unless
# the new local_settings.py contains the fixed string MARKER, then waits for
# the rollout, so the old pod is gone from the selector afterwards.
wait_config_flip() {
  local before=$1 marker=$2 deadline settings
  deadline=$(( $(date +%s) + 90 ))
  AFTER_CM=$before
  while [ "$(date +%s)" -lt "$deadline" ]; do
    AFTER_CM=$(config_cm 2>/dev/null || true)
    if [ -n "$AFTER_CM" ] && [ "$AFTER_CM" != "$before" ]; then
      break
    fi
    sleep 2
  done
  if [ -z "$AFTER_CM" ] || [ "$AFTER_CM" = "$before" ]; then
    echo "FAIL: the config volume still names $before after 90 s"
    return 1
  fi
  settings=$(kubectl get configmap "$AFTER_CM" -n openstack \
    -o jsonpath='{.data.local_settings\.py}')
  if ! grep -qF -- "$marker" <<<"$settings"; then
    echo "FAIL: local_settings.py of $AFTER_CM does not contain $marker"
    echo "$settings"
    return 1
  fi
  echo "OK: config rolled from $before to $AFTER_CM, which contains $marker"
  kubectl rollout status deployment/horizon-logging -n openstack --timeout=120s
}

# dashboard_pod CM: the name of a Ready, non-terminating dashboard pod that
# mounts the ConfigMap CM, or nothing. A pod of the previous template can
# still be terminating after the rollout, and a request it answers would
# test the old settings.
dashboard_pod() {
  kubectl get pods -n openstack -l app.kubernetes.io/instance=horizon-logging -o json \
    | jq -r --arg cm "$1" '[.items[]
        | select(.metadata.deletionTimestamp == null)
        | select(any(.spec.volumes[]?; .name == "config" and .configMap.name == $cm))
        | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
        | .metadata.name] | first // empty'
}

# dashboard_get POD PATH: GETs http://localhost:8080PATH inside POD, where
# localhost is in ALLOWED_HOSTS, and prints the body of the 404 the path is
# expected to answer. Any other status fails, so a 500 cannot pass for a
# missing page.
dashboard_get() {
  kubectl exec -n openstack "$1" -c horizon -- python3 -c '
import sys, urllib.error, urllib.request
try:
    urllib.request.urlopen("http://localhost:8080" + sys.argv[1], timeout=10)
except urllib.error.HTTPError as err:
    if err.code != 404:
        raise
    sys.stdout.write(err.read().decode("utf-8", "replace"))
else:
    sys.exit("expected HTTP 404 for " + sys.argv[1])
' "$2"
}

# horizon_logs [ARGS...]: the last 600 lines of every dashboard container,
# with any extra kubectl logs flags.
horizon_logs() {
  kubectl logs -n openstack -l app.kubernetes.io/instance=horizon-logging \
    --all-containers --tail=600 "$@" 2>/dev/null || true
}
