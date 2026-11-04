# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# The steps of chainsaw-test.yaml source this file from bash under
# set -euo pipefail. It defines override_events and wait_override_event for
# the Horizon CR horizon-events.
# shellcheck shell=bash

# override_events: one "<type>\t<count>\t<message>" line per
# ExtraConfigOwnedKeyOverride event on horizon-events. The client-side
# correlator raises an Event's count on a repeat instead of creating a new
# one, so a repeat shows in the count; an event without a count counts as 1,
# as in ../../lib/assert-steady-state-events.sh. It selects by the CR's UID,
# so a rerun within the event TTL does not see the events of an earlier CR of
# the same name. Not UID: bash makes that variable read-only.
override_events() {
  local cr_uid
  cr_uid=$(kubectl get horizon horizon-events -n openstack -o jsonpath='{.metadata.uid}')
  kubectl get events -n openstack \
    --field-selector "reason=ExtraConfigOwnedKeyOverride,involvedObject.uid=$cr_uid" -o json |
    jq -r '.items[] | [.type, ((.count // 1) | tostring), .message] | @tsv'
}

# wait_override_event MESSAGE: waits up to 60 s for a Warning event with
# MESSAGE on horizon-events.
wait_override_event() {
  local deadline events
  deadline=$(( $(date +%s) + 60 ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    events=$(override_events)
    if grep -qxF -- "Warning"$'\t'"$1" <<<"$(cut -f1,3 <<<"$events")"; then
      echo "OK: Warning ExtraConfigOwnedKeyOverride event: $1"
      return 0
    fi
    sleep 2
  done
  echo "FAIL: no Warning ExtraConfigOwnedKeyOverride event with message '$1' within 60 s"
  echo "$events"
  return 1
}
