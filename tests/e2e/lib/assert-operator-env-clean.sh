#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# tests/e2e/lib/assert-operator-env-clean.sh: prove that an operator's process
# environment holds no key material.
#
# A secret in a process environment leaks through every crash dump, debug
# container and child process. The operator charts inject env through the
# operator-library.chart.env hook and extraEnv alone, but a mutating webhook or
# a deployment overlay can add an entry no template shows, so the script reads
# both the pod spec and the environment of the running manager:
#   1. the Deployment's selector finds a Running, non-terminating pod;
#   2. no container, init container or ephemeral container of that pod reads a
#      Secret into env (env[].valueFrom.secretKeyRef, envFrom[].secretRef) or
#      sets an env entry whose name matches ENV_SCAN_DENY_NAME_RE;
#   3. an ephemeral container added with kubectl debug reads
#      /proc/<pid>/environ of every process of the manager container, and no
#      entry's name matches ENV_SCAN_DENY_NAME_RE and no line matches
#      ENV_SCAN_DENY_VALUE_RE.
#
# Usage:
#   assert-operator-env-clean.sh <namespace> <deployment> <debug-image>
#
# <debug-image> carries the sh, readlink and tr the read runs; the operator
# image is a static binary without a shell. It must already be on the node:
# the ephemeral container runs with imagePullPolicy Never. The container joins
# the process namespace of the container named manager (--target) and takes
# the pod's securityContext, so it runs as the manager's uid, which is what
# lets it read the manager's environ. Kubernetes reduces $$ in container
# arguments to $, so the read skips its own processes by their mount namespace
# instead of by PID. An ephemeral container cannot be removed from a pod; each
# run adds one named env-scan-<epoch>-<pid>, which a later run tells apart.
#
# A finding prints the name of the variable, never its value, so a CI log does
# not repeat what the check found. Each finding prints one FAIL: line and the
# script exits 1 after both checks ran; a clean run prints OK: lines and exits
# 0. A missing or empty argument, a timeout that is not a whole number, or a
# regex jq cannot compile prints a message on stderr and exits 2.
#
# Environment overrides:
#   ENV_SCAN_DENY_NAME_RE   names that hold key material, as a jq regex
#                           (default (^|_)(PASSWORD|PASSWD|SECRET|TOKEN|KEY|CREDENTIALS?|FERNET)(_|$));
#                           CERT is left out because every operator image
#                           sets SSL_CERT_FILE to its CA bundle
#   ENV_SCAN_ALLOW_NAME_RE  names to accept although the deny regex matches
#                           them (default empty, which accepts none); in the
#                           pod spec only an entry with a literal value
#   ENV_SCAN_DENY_VALUE_RE  lines that hold key material (default
#                           "-----BEGIN |password=")
#   ENV_SCAN_TIMEOUT        seconds to wait for the ephemeral container to
#                           terminate (default 60)
#
# Chainsaw runs script steps in the suite directory, so a suite calls this as
# "../../lib/assert-operator-env-clean.sh". Apart from the ephemeral container
# the script only reads.

set -uo pipefail

usage() {
  echo "usage: assert-operator-env-clean.sh <namespace> <deployment> <debug-image>" >&2
  exit 2
}

if [ "$#" -ne 3 ] || [ -z "$1" ] || [ -z "$2" ] || [ -z "$3" ]; then
  usage
fi

# Not NAMESPACE: chainsaw injects that variable into every script step.
ns="$1"
deployment="$2"
image="$3"

# The container the operator-library chart names in its Deployment template.
target=manager

default_deny_name='(^|_)(PASSWORD|PASSWD|SECRET|TOKEN|KEY|CREDENTIALS?|FERNET)(_|$)'
default_deny_value='-----BEGIN |password='
deny_name="${ENV_SCAN_DENY_NAME_RE:-$default_deny_name}"
allow_name="${ENV_SCAN_ALLOW_NAME_RE:-}"
deny_value="${ENV_SCAN_DENY_VALUE_RE:-$default_deny_value}"
timeout="${ENV_SCAN_TIMEOUT:-60}"

# No leading zero: bash arithmetic reads 010 as octal and rejects 08.
if ! [[ "$timeout" =~ ^(0|[1-9][0-9]*)$ ]]; then
  echo "ENV_SCAN_TIMEOUT must be a whole number of seconds, got '$timeout'" >&2
  exit 2
fi
for var in ENV_SCAN_DENY_NAME_RE ENV_SCAN_ALLOW_NAME_RE ENV_SCAN_DENY_VALUE_RE; do
  case "$var" in
    ENV_SCAN_DENY_NAME_RE) re="$deny_name" ;;
    ENV_SCAN_ALLOW_NAME_RE) re="$allow_name" ;;
    *) re="$deny_value" ;;
  esac
  if [ -n "$re" ] && ! jq -en --arg re "$re" '"" | test($re) | . or true' >/dev/null 2>&1; then
    echo "$var is not a regex jq can compile: '$re'" >&2
    exit 2
  fi
done

failed=0

# kubectl's stderr goes here, so a warning never ends up in the JSON or the
# environment lines the checks parse.
err="$(mktemp)"
trap 'rm -f "$err"' EXIT

# fail <message>: print one finding and remember it.
fail() {
  echo "FAIL: $*"
  failed=1
}

# finish: exit by the findings.
finish() {
  if [ "$failed" -ne 0 ]; then
    exit 1
  fi
  exit 0
}

# ── Check 1: the Deployment's pod ─────────────────────────────────────────
if ! deploy_json="$(kubectl get deployment "$deployment" -n "$ns" -o json 2>"$err")"; then
  fail "cannot read deployment $ns/$deployment: $(cat "$err")"
  finish
fi
if ! selector="$(jq -r '.spec.selector.matchLabels // {} | to_entries
    | map("\(.key)=\(.value)") | join(",")' <<<"$deploy_json" 2>&1)" || [ -z "$selector" ]; then
  fail "deployment $ns/$deployment has no spec.selector.matchLabels"
  finish
fi
if ! pods_json="$(kubectl get pods -n "$ns" -l "$selector" -o json 2>"$err")"; then
  fail "cannot list the pods of deployment $ns/$deployment: $(cat "$err")"
  finish
fi
# The first Running pod by name, so two runs against an unchanged Deployment
# read the same pod.
if ! pod_json="$(jq -c '[.items[] | select(.status.phase == "Running"
    and .metadata.deletionTimestamp == null)] | sort_by(.metadata.name) | .[0] // empty' \
    <<<"$pods_json" 2>&1)" || [ -z "$pod_json" ]; then
  fail "no Running pod of deployment $ns/$deployment (selector $selector)"
  finish
fi
pod="$(jq -r '.metadata.name' <<<"$pod_json")"
echo "OK: deployment $ns/$deployment runs pod $pod"

# ── Check 2: the pod spec ─────────────────────────────────────────────────
# One line per finding. A Secret read into env is reported once, as a Secret
# read, even when its name matches the deny regex as well.
if ! spec_findings="$(jq -r --arg deny "$deny_name" --arg allow "$allow_name" '
    [ (.spec.containers // [] | map({kind: "container", c: .}))[],
      (.spec.initContainers // [] | map({kind: "init container", c: .}))[],
      (.spec.ephemeralContainers // [] | map({kind: "ephemeral container", c: .}))[] ]
    | .[] | .kind as $kind | .c.name as $name
    | ( (.c.env // [])[] | select(.valueFrom.secretKeyRef != null)
        | "\($kind) \($name): env \(.name) reads Secret \(.valueFrom.secretKeyRef.name // "?")/\(.valueFrom.secretKeyRef.key // "?")" ),
      ( (.c.envFrom // [])[] | select(.secretRef != null)
        | "\($kind) \($name): envFrom reads Secret \(.secretRef.name // "?")" ),
      ( (.c.env // [])[] | select(.valueFrom.secretKeyRef == null)
        | select(.name | test($deny))
        | select(($allow != "" and (.name | test($allow)) and ((.value // "") != "")) | not)
        | "\($kind) \($name): env \(.name) is named like key material" )
  ' <<<"$pod_json" 2>&1)"; then
  fail "cannot read the spec of pod $ns/$pod: $spec_findings"
  spec_findings=""
fi
if [ -n "$spec_findings" ]; then
  while IFS= read -r finding; do
    fail "$finding"
  done <<<"$spec_findings"
else
  echo "OK: the spec of pod $ns/$pod puts no Secret and no key-material name into env"
fi

# ── Check 3: the process environment ──────────────────────────────────────
# The read skips every process that shares its own mount namespace: the shell,
# and the readlink and tr it starts. Everything else in the shared process
# namespace belongs to the manager container.
# shellcheck disable=SC2016 # expanded by the shell inside the ephemeral container
scan='me=$(readlink /proc/self/ns/mnt); for p in /proc/[0-9]*; do [ "$(readlink "$p/ns/mnt" 2>/dev/null)" = "$me" ] && continue; [ -r "$p/environ" ] && tr "\0" "\n" < "$p/environ"; done; exit 0'
container="env-scan-$(date +%s)-$$"
if ! debug_out="$(kubectl debug "$pod" -n "$ns" --container "$container" --image "$image" \
  --image-pull-policy=Never --target="$target" --profile=restricted -- sh -c "$scan" 2>&1)"; then
  fail "kubectl debug could not add $container to pod $ns/$pod: $debug_out"
  finish
fi

deadline=$((SECONDS + timeout))
exit_code=""
while :; do
  state="$(kubectl get pod "$pod" -n "$ns" -o json 2>/dev/null \
    | jq -c --arg c "$container" '[.status.ephemeralContainerStatuses // [] | .[]
      | select(.name == $c)] | .[0].state // {}' 2>/dev/null)" || state='{}'
  [ -n "$state" ] || state='{}'
  exit_code="$(jq -r '.terminated.exitCode // empty' <<<"$state")"
  if [ -n "$exit_code" ]; then
    break
  fi
  reason="$(jq -r '.waiting.reason // empty' <<<"$state")"
  case "$reason" in
    ErrImageNeverPull | ErrImagePull | ImagePullBackOff | InvalidImageName | \
      CreateContainerError | CreateContainerConfigError | RunContainerError)
      fail "$container of pod $ns/$pod cannot start: $reason ($(jq -r '.waiting.message // ""' <<<"$state"))"
      finish
      ;;
  esac
  if [ "$SECONDS" -ge "$deadline" ]; then
    fail "$container of pod $ns/$pod did not terminate within ${timeout}s; its state: $state"
    finish
  fi
  sleep 2
done
if [ "$exit_code" != "0" ]; then
  fail "$container of pod $ns/$pod exited $exit_code"
fi

if ! environment="$(kubectl logs "$pod" -n "$ns" -c "$container" 2>"$err")"; then
  fail "cannot read the log of $container of pod $ns/$pod: $(cat "$err")"
  finish
fi

# One line per entry (ENTRY) and per finding (FAIL ...). The name of an entry
# is everything before its first =, because Kubernetes and an image's ENV
# accept names with - and . too, and the pod-spec check tests those whole. A
# line without = is the continuation of a multi-line value, such as a PEM
# block.
if ! analysis="$(jq -R -r --arg deny "$deny_name" --arg allow "$allow_name" --arg value "$deny_value" '
    ((capture("^(?<name>[^=]+)=") | .name) // null) as $name
    | ( if $name != null then "ENTRY" else empty end ),
      ( if $name != null and ($name | test($deny))
          and (($allow == "") or ($name | test($allow) | not))
        then "FAIL the process environment holds \($name), which is named like key material"
        else empty end ),
      ( if test($value) then
          ( if $name != null then "FAIL the value of \($name) in the process environment looks like key material"
            else "FAIL a continuation line of a multi-line value in the process environment looks like key material" end )
        else empty end )
  ' <<<"$environment" 2>&1)"; then
  fail "cannot read the process environment of pod $ns/$pod: $analysis"
  finish
fi

entries="$(grep -c '^ENTRY$' <<<"$analysis" || true)"
if [ "$entries" -eq 0 ]; then
  fail "read no process environment of pod $ns/$pod: $container saw no process of container $target"
  finish
fi
runtime_failed=0
while IFS= read -r line; do
  case "$line" in
    "FAIL "*)
      fail "${line#FAIL }"
      runtime_failed=1
      ;;
  esac
done <<<"$analysis"
if [ "$runtime_failed" -eq 0 ]; then
  echo "OK: $entries process environment entries of $ns/$pod hold no key material"
fi

finish
