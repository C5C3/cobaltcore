#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-read-chart-version.sh — Print the chart version a YAML file names:
# the value of its first top-level `version:` line, with double and single
# quotes stripped.
#
# Usage: hack/ci-read-chart-version.sh <file>
#
# The file is an operator's Chart.yaml or the baseline.yaml pin of an operator
# upgrade suite. A missing file, or one without a `version:` line, prints
# nothing and exits 0: what an empty or malformed version means is the
# caller's decision.
#
# set -euo pipefail, SPDX Apache-2.0 header, shellcheck-clean.

set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "::error::usage: hack/ci-read-chart-version.sh <file>" >&2
  exit 1
fi

grep -E '^version:' "$1" 2>/dev/null | head -1 | awk '{print $2}' | tr -d "\"'" || true
