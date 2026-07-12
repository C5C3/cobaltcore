#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0
#
# scaffold-guide.sh — print a convention-conform how-to guide skeleton for a
# chosen devstack anchor to stdout. The script never writes to the tree; the
# caller redirects stdout into docs/guides/<slug>.md.
#
# The skeleton carries the standardized prerequisites block (the
# "::: info Devstack" container with the devstack's verbatim bring-up command
# and completion pointer), the section structure, and the terminal
# "## Tested by" section. Until the tested-by suite path resolves to a real
# chainsaw-test.yaml the skeleton fails
# tests/unit/docs/guide_devstack_and_tested_by_test.sh — that is intended.
#
# NOTE: bring-up commands, completion pointers, and name sets MUST stay in
# sync with the devstack table in docs/contributing/guide-conventions.md —
# that page is the prose source of truth; this script is the machine copy.
#
# Usage: scaffold-guide.sh <slug> --devstack <devstack> [options]
#   <slug>                guide file slug ([a-z0-9-]+, becomes docs/guides/<slug>.md)
#   --devstack <name>     quick-start | quick-start-extended | quick-start-controlplane
#   --title <title>       guide title (default: title-cased slug)
#   --opt-in WITH_X=true  compose a WITH_* opt-in flag onto the bring-up command
#                         (repeatable; WITH_CONTROLPLANE is a devstack, not an opt-in)
#   --suite <path>        tests/e2e/... suite for '## Tested by' (default: TODO placeholder)

set -euo pipefail

usage() {
  echo "usage: $0 <slug> --devstack <quick-start|quick-start-extended|quick-start-controlplane> [--title <title>] [--opt-in WITH_X=true]... [--suite tests/e2e/...]" >&2
  exit 2
}

SLUG=""
DEVSTACK=""
TITLE=""
SUITE=""
OPT_INS=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --devstack)
      [[ $# -ge 2 ]] || usage
      DEVSTACK="$2"
      shift 2
      ;;
    --title)
      [[ $# -ge 2 ]] || usage
      TITLE="$2"
      shift 2
      ;;
    --opt-in)
      [[ $# -ge 2 ]] || usage
      if [[ ! "$2" =~ ^WITH_[A-Z0-9_]+= ]]; then
        echo "error: --opt-in must be a WITH_*=... flag, got '$2'" >&2
        exit 2
      fi
      if [[ "$2" == WITH_CONTROLPLANE=* ]]; then
        echo "error: WITH_CONTROLPLANE is not an opt-in — use --devstack quick-start-controlplane" >&2
        exit 2
      fi
      OPT_INS="${OPT_INS}${OPT_INS:+ }$2"
      shift 2
      ;;
    --suite)
      [[ $# -ge 2 ]] || usage
      if [[ ! "$2" =~ ^tests/ ]]; then
        echo "error: --suite must be a tests/... path, got '$2'" >&2
        exit 2
      fi
      SUITE="$2"
      shift 2
      ;;
    -*)
      usage
      ;;
    *)
      [[ -z "${SLUG}" ]] || usage
      SLUG="$1"
      shift
      ;;
  esac
done

[[ -n "${SLUG}" ]] || usage
if [[ ! "${SLUG}" =~ ^[a-z0-9][a-z0-9-]*$ ]]; then
  echo "error: slug must match [a-z0-9][a-z0-9-]*, got '${SLUG}'" >&2
  exit 2
fi
case "${DEVSTACK}" in
  quick-start | quick-start-extended | quick-start-controlplane) ;;
  *) usage ;;
esac

if [[ -z "${TITLE}" ]]; then
  TITLE="$(printf '%s' "${SLUG}" | awk -F- '{
    for (i = 1; i <= NF; i++) $i = toupper(substr($i, 1, 1)) substr($i, 2)
    print
  }' OFS=' ')"
fi

# Warn (stderr only — stdout is the skeleton) when the suite does not resolve
# yet; the docs gate stays red until it does.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd)"
if [[ -n "${SUITE}" && ! -f "${REPO_ROOT}/${SUITE}/chainsaw-test.yaml" ]]; then
  echo "note: ${SUITE}/chainsaw-test.yaml does not exist yet — the docs gate fails until it does" >&2
fi
if [[ -z "${SUITE}" ]]; then
  SUITE="tests/e2e/keystone/<TODO-suite>"
  echo "note: no --suite given — '## Tested by' carries a TODO placeholder the docs gate rejects" >&2
fi

# Bring-up command per devstack, with opt-ins composed onto the
# 'make deploy-infra' line (guide-conventions.md: "Opt-in flags compose").
OPT_SUFFIX=""
[[ -n "${OPT_INS}" ]] && OPT_SUFFIX="${OPT_INS} "
case "${DEVSTACK}" in
  quick-start)
    DEVSTACK_LABEL="Quick Start"
    BRING_UP="KIND_HOST_PORT=8443 ${OPT_SUFFIX}make deploy-infra"
    ;;
  quick-start-extended)
    DEVSTACK_LABEL="Quick Start (Extended)"
    BRING_UP="kind create cluster --name forge --config hack/kind-config.yaml
${OPT_SUFFIX}make deploy-infra"
    ;;
  quick-start-controlplane)
    DEVSTACK_LABEL="Quick Start (ControlPlane)"
    BRING_UP="KIND_HOST_PORT=8443 WITH_CONTROLPLANE=true ${OPT_SUFFIX}make deploy-infra"
    ;;
esac

# --- Skeleton ---------------------------------------------------------------

cat <<EOF
---
title: ${TITLE}
quadrant: operator
---

<!--
SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
SPDX-License-Identifier: Apache-2.0
-->

# How-to: ${TITLE}

TODO: one paragraph — what this guide accomplishes on a running cluster and
why an operator reaches for it.

## Prerequisites

::: info Devstack
This guide is written against the **[${DEVSTACK_LABEL}](../${DEVSTACK}.md)** devstack. Stand it up first:

\`\`\`bash
${BRING_UP}
\`\`\`

EOF

case "${DEVSTACK}" in
  quick-start-controlplane)
    cat <<'EOF'
Follow that tutorial through to its final **Verify** step, so a `ControlPlane`
CR named `controlplane` is `Ready` in the `openstack` namespace and its projected
`controlplane-keystone` Keystone child is running. Every resource name in the
examples below is one that devstack produces.
:::

::: warning The Keystone child is operator-owned
On a ControlPlane deployment the `controlplane-keystone` Keystone CR is
**projected** by the c5c3-operator, so a knob you set directly on the child is
reverted on the next reconcile. Set operational knobs on the `ControlPlane` CR
and let the operator project them down. Where the `ControlPlane` CRD does not
expose a knob, this guide points to the
[Standalone Keystone](#standalone-keystone-without-a-controlplane) section,
which drives a Keystone CR you own.
:::
EOF
    ;;
  quick-start-extended)
    cat <<'EOF'
Follow that tutorial through to its final **Verify the deployment** step, so a
Keystone CR named `keystone` is `Ready` in the `openstack` namespace. Every
resource name in the examples below is one that devstack produces.
:::
EOF
    ;;
  quick-start)
    cat <<'EOF'
Follow that tutorial through to its final **Verify** step, so a Keystone CR named
`keystone` is `Ready` in the `openstack` namespace. Every resource name in the
examples below is one that devstack produces.
:::
EOF
    ;;
esac

cat <<EOF

1. **TODO guide-specific prerequisite.** Anything the devstack does not
   provide (an external LDAP server, a Keycloak realm, a CNI that enforces
   NetworkPolicy, ...). Delete this list if there is none.

---

## Steps

### 1. TODO first step

TODO: every command uses only the resource names the declared devstack
produces (see the devstack table in docs/contributing/guide-conventions.md).

## Verification

TODO: how the reader confirms the change took effect.
EOF

if [[ "${DEVSTACK}" == "quick-start-controlplane" ]]; then
  cat <<'EOF'

## Standalone Keystone, without a ControlPlane

TODO: where a standalone (non-ControlPlane) installation differs, describe it
here against a Keystone CR the reader owns — do not interleave the two naming
worlds in the primary walkthrough above.
EOF
fi

cat <<EOF

## See also

- TODO: related guides and reference pages.

## Tested by

The flow above mirrors the following end-to-end suite:

\`\`\`bash
chainsaw test --test-dir ${SUITE}
\`\`\`
EOF
