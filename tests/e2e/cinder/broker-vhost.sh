#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/cinder/broker-vhost.sh: give one Cinder e2e suite its own vhost on
# the kind-only shared-rabbitmq broker, and write the transport URL Secret the
# suite's CR references.
#
# One vhost per suite, because the suites run in parallel against one broker and
# Cinder's RPC topics are named after the deployment rather than after the CR:
# every scheduler consumes "cinder-scheduler", and every volume service consumes
# "cinder-volume.<host>@<backend>". Two suites on the same vhost would share
# those queues, so one suite's scheduler could answer the other's API and the
# volume service of a torn-down suite would still be a consumer. A vhost is the
# boundary RabbitMQ draws around a queue namespace, so a vhost per suite keeps
# each set of topics to itself.
#
# The managed mode of spec.messaging (messaging.clusterRef) is not an option
# here: it always lands on the default vhost, because BuildTransportURL renders
# the path as "/" (internal/common/messaging/messaging.go:96-105). The suites
# therefore use the brownfield mode (messaging.secretRef) against the Secret
# this helper writes.
#
# The credentials are the broker's own default user, copied into one Secret per
# suite. That user holds full permissions on every vhost this helper creates, so
# the isolation is by vhost and not by credential: a suite that addressed
# another suite's vhost would reach it. Nothing does, since each CR names its
# own Secret, and the kind broker exists for these suites alone.
#
# Usage:
#   broker-vhost.sh create <vhost> <secret-name> <namespace>
#   broker-vhost.sh delete <vhost>
#
# A suite calls the create form from its first step and the delete form from
# that step's cleanup, with the CR name as the vhost. Chainsaw runs script steps
# in the suite directory, so the calls read "../broker-vhost.sh create ...".
# add_vhost on an existing vhost is a no-op, so a retried step converges.
#
# EVERY delete call site guards with `|| true`, and that is the contract rather
# than a per-suite choice. A cleanup block runs after its suite has reached a
# verdict, and delete reaches the broker with `kubectl exec` into the one
# 512Mi pod every suite shares, two suites at a time. A broker that is
# restarting under that load would otherwise redden a run whose every
# assertion passed. The leak is not silent — delete prints one WARNING: line
# for any failure other than an already-absent vhost — and the vhost dies with
# the kind cluster either way.

set -euo pipefail

# The broker side is fixed by deploy/kind/messaging/shared-rabbitmq.yaml: the
# RabbitmqCluster lives in openstack, and the cluster operator names its single
# server pod <cluster>-server-0 with the broker in the "rabbitmq" container.
BROKER_NAMESPACE="openstack"
BROKER="shared-rabbitmq"
BROKER_POD="${BROKER}-server-0"
BROKER_CONTAINER="rabbitmq"
BROKER_HOST="${BROKER}.${BROKER_NAMESPACE}.svc.cluster.local"
BROKER_PORT="5672"

usage() {
  echo "usage: broker-vhost.sh create <vhost> <secret-name> <namespace>" >&2
  echo "       broker-vhost.sh delete <vhost>" >&2
  exit 2
}

# broker_ctl runs rabbitmqctl inside the broker pod. A failure (an absent pod,
# a rejected command) travels out with kubectl's own message and exit code.
broker_ctl() {
  kubectl exec -n "${BROKER_NAMESPACE}" "${BROKER_POD}" -c "${BROKER_CONTAINER}" -- \
    rabbitmqctl "$@"
}

# secret_field prints one base64-decoded key of a Secret in the broker namespace.
secret_field() {
  kubectl get secret "$1" -n "${BROKER_NAMESPACE}" -o "jsonpath={.data.$2}" | base64 -d
}

create_vhost() {
  local vhost="$1" secret="$2" namespace="$3"

  # The cluster operator generates the default-user Secret and publishes its
  # name in the status, so the name is read rather than assumed.
  local default_user
  default_user="$(kubectl get rabbitmqcluster "${BROKER}" -n "${BROKER_NAMESPACE}" \
    -o 'jsonpath={.status.defaultUser.secretReference.name}')"
  if [ -z "${default_user}" ]; then
    echo "ERROR: shared-rabbitmq has no status.defaultUser.secretReference.name" >&2
    exit 1
  fi

  local username password
  username="$(secret_field "${default_user}" username)"
  password="$(secret_field "${default_user}" password)"
  if [ -z "${username}" ] || [ -z "${password}" ]; then
    echo "ERROR: default-user Secret ${default_user} lacks username or password" >&2
    exit 1
  fi

  broker_ctl add_vhost "${vhost}"
  broker_ctl set_permissions -p "${vhost}" "${username}" '.*' '.*' '.*'

  # Create-or-apply so a retried step refreshes the Secret instead of failing on
  # the existing object.
  kubectl create secret generic "${secret}" --namespace "${namespace}" \
    --from-literal=transport_url="rabbit://${username}:${password}@${BROKER_HOST}:${BROKER_PORT}/${vhost}" \
    --dry-run=client -o yaml | kubectl apply -f -

  echo "OK: vhost ${vhost} carries ${namespace}/${secret}"
}

delete_vhost() {
  local vhost="$1" output

  if output="$(broker_ctl delete_vhost "${vhost}" 2>&1)"; then
    echo "${output}"
    echo "OK: vhost ${vhost} deleted"
    return 0
  fi

  echo "${output}" >&2
  # A vhost the create step never got to is not a cleanup failure.
  if printf '%s' "${output}" | grep -q 'no_such_vhost'; then
    echo "OK: vhost ${vhost} was already absent"
    return 0
  fi
  # Everything else leaked the vhost. The call sites run this in a cleanup
  # block behind `|| true` (see the header), so the exit status below reaches
  # no verdict and this one greppable line is the record of the leak.
  echo "WARNING: vhost ${vhost} was not deleted on ${BROKER}: see the rabbitmqctl output above" >&2
  exit 1
}

case "${1:-}" in
create)
  [ "$#" -eq 4 ] || usage
  create_vhost "$2" "$3" "$4"
  ;;
delete)
  [ "$#" -eq 2 ] || usage
  delete_vhost "$2"
  ;;
*)
  usage
  ;;
esac
