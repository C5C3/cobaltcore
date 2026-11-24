#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# Render the promoted operator from its packaged RC, including its vendored
# library. A stable filename alone cannot change the image selected by Helm.
set -euo pipefail
repo=$(cd "$(dirname "$0")/../../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
mkdir -p operators/{keystone,shared}/helm assets
cp -R "$repo/operators/keystone/helm/keystone-operator" operators/keystone/helm/
cp -R "$repo/operators/shared/helm/operator-library" operators/shared/helm/
make -f "$repo/Makefile" helm-package OPERATOR=keystone CHART_VERSION=1.5.0-rc.10 CHART_APP_VERSION=1.5.0-rc.10
rc=keystone-operator-1.5.0-rc.10.tgz
stable=assets/keystone-operator-1.5.0.tgz

PYTHONDONTWRITEBYTECODE=1 PYTHONPATH="$repo/scripts/release" python3 - "$rc" <<'PY'
import sys
from pathlib import Path
from publish import package_stable_chart

package_stable_chart(Path(sys.argv[1]), Path("unpacked"), "v1.5.0-rc.10", "1.5.0", 1700000000)
PY

for version in 1.5.0-rc.10 1.5.0; do
  chart=$rc
  if [[ "$version" == 1.5.0 ]]; then chart=$stable; fi
  [[ $(helm show chart "$chart" | yq '.version') == "$version" ]]
  helm template keystone "$chart" > rendered.yaml
  grep -Fq "image: \"ghcr.io/c5c3/keystone-operator:$version\"" rendered.yaml
done
[[ $(helm show chart "$stable" | yq '.annotations."artifacthub.io/prerelease"') == null ]]
echo "RC and stable chart archives render the matching operator image."
