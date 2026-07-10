#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/nix-devshell-hook.sh — Provision the exactly-pinned CI toolchain inside
# the Nix devshell (flake.nix sources this on `nix develop`).
#
# The flake pins the base runtimes (Go, Node, Python, Helm, shellcheck, yq, jq,
# GNU userland) from nixpkgs. This hook installs the tools the pipeline pins
# *exactly* — controller-gen, gofumpt, golangci-lint, setup-envtest, kustomize,
# chainsaw, kind, kubectl, flux, the helm-unittest plugin, and the envtest
# assets — by reading the pins *where they already live*, so no version literal
# is ever duplicated into the flake:
#
#   - .github/workflows/ci.yaml  env block  → CONTROLLER_GEN_VERSION,
#                                              GOFUMPT_VERSION,
#                                              GOLANGCI_LINT_VERSION
#   - .github/workflows/ci.yaml  inline     → setup-envtest@<ref>,
#                                              KUSTOMIZE_VERSION,
#                                              helm-unittest --version
#   - Makefile                              → ENVTEST_K8S_VERSION
#   - hack/install-test-deps.sh (reused)    → chainsaw, kind, kubectl, flux
#
# Everything installs into the repo's gitignored bin/ (the Makefile's LOCALBIN)
# and bin/ is prepended to PATH. A Renovate bump to any canonical file above
# self-heals the shell on the next entry — no flake edit needed.
#
# COUPLING: the resolver functions grep the canonical files by their current
# shape (the ci.yaml env block, the `setup-envtest@` / `KUSTOMIZE_VERSION=` /
# `helm-unittest --version` lines, and Makefile ENVTEST_K8S_VERSION). A refactor
# that moves or reshapes those lines needs a matching change here; the unit test
# tests/unit/hack/nix_devshell_hook_test.sh fails loudly if the coupling drifts.
#
# Usage:
#   source hack/nix-devshell-hook.sh     # provision the devshell (flake.nix)
#   bash hack/nix-devshell-hook.sh --print-pins   # print the resolved pins

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null || pwd)"
CI_FILE="${REPO_ROOT}/.github/workflows/ci.yaml"
MAKEFILE="${REPO_ROOT}/Makefile"

# ---------------------------------------------------------------------------
# Pin resolvers — each reads one value from its canonical file. Single-process
# awk (no `| head`) keeps them safe under `set -o pipefail`.
# ---------------------------------------------------------------------------
resolve_controller_gen_version() {
  awk '$1 == "CONTROLLER_GEN_VERSION:" { print $2; exit }' "${CI_FILE}"
}

resolve_gofumpt_version() {
  awk '$1 == "GOFUMPT_VERSION:" { print $2; exit }' "${CI_FILE}"
}

resolve_golangci_lint_version() {
  awk '$1 == "GOLANGCI_LINT_VERSION:" { print $2; exit }' "${CI_FILE}"
}

resolve_setup_envtest_ref() {
  awk 'match($0, /setup-envtest@[^[:space:]]+/) {
         s = substr($0, RSTART, RLENGTH); sub(/.*@/, "", s); print s; exit
       }' "${CI_FILE}"
}

resolve_envtest_k8s_version() {
  # Mirror the exact awk the CI job "Get envtest k8s version" uses so the shell
  # and CI resolve the same value from the Makefile.
  awk '/ENVTEST_K8S_VERSION/ { print $NF; exit }' "${MAKEFILE}"
}

resolve_kustomize_version() {
  awk 'match($0, /KUSTOMIZE_VERSION=v[0-9][0-9.]*/) {
         s = substr($0, RSTART, RLENGTH); sub(/.*=/, "", s); print s; exit
       }' "${CI_FILE}"
}

resolve_helm_unittest_version() {
  awk '/helm-unittest/ && match($0, /--version[[:space:]]+v[0-9][0-9.]*/) {
         s = substr($0, RSTART, RLENGTH); sub(/.*[[:space:]]/, "", s); print s; exit
       }' "${CI_FILE}"
}

# print_pins — emit the seven resolved KEY=VALUE pins (the test/doc surface).
print_pins() {
  printf 'CONTROLLER_GEN_VERSION=%s\n' "$(resolve_controller_gen_version)"
  printf 'GOFUMPT_VERSION=%s\n' "$(resolve_gofumpt_version)"
  printf 'GOLANGCI_LINT_VERSION=%s\n' "$(resolve_golangci_lint_version)"
  printf 'SETUP_ENVTEST_REF=%s\n' "$(resolve_setup_envtest_ref)"
  printf 'ENVTEST_K8S_VERSION=%s\n' "$(resolve_envtest_k8s_version)"
  printf 'KUSTOMIZE_VERSION=%s\n' "$(resolve_kustomize_version)"
  printf 'HELM_UNITTEST_VERSION=%s\n' "$(resolve_helm_unittest_version)"
}

# ---------------------------------------------------------------------------
# Install helpers (used only by the provisioning path).
# ---------------------------------------------------------------------------

# _tool_has <path> <version-arg> <pin> — true when the installed tool already
# reports the pinned version, so the install is skipped (idempotency, mirroring
# hack/install-test-deps.sh and `make install-gofumpt`).
_tool_has() {
  local path="$1" varg="$2" pin="$3" re
  [ -x "${path}" ] || return 1
  # Anchor the pin to a whole version token so a pin that is a prefix of the
  # installed version (e.g. "2.12.2" vs "2.12.20") does not spuriously match and
  # skip a needed reinstall. Escape the dots before using the pin as a regex.
  re="${pin//./\\.}"
  "${path}" "${varg}" 2>/dev/null | grep -qE "(^|[^0-9.])${re}([^0-9.]|$)"
}

# _helm_plugin_pinned <plugin-list-output> <name> <pin> — true when the helm
# plugin list already reports <name> at exactly <pin>, so the install is skipped.
# Anchors the pin to a whole version token (dots escaped, trailing non-version
# boundary) so a pin that is a prefix of the installed version (e.g. "1.0.3" vs
# "1.0.30") does not spuriously match and skip a needed reinstall — the same
# guard _tool_has applies, kept in sync by the unit test.
_helm_plugin_pinned() {
  local list="$1" name="$2" pin="$3" re
  re="${pin#v}"
  re="${re//./\\.}"
  printf '%s\n' "${list}" | grep -qiE "${name}[[:space:]]+${re}([^0-9.]|$)"
}

# _detect_platform — echo "<os> <arch>" (linux|darwin amd64|arm64) or fail.
_detect_platform() {
  local os arch
  case "$(uname -s)" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) return 1 ;;
  esac
  case "$(uname -m)" in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) return 1 ;;
  esac
  printf '%s %s\n' "${os}" "${arch}"
}

# _sha256 <file> — print the file's SHA256 hex digest (GNU or BSD userland).
_sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

# _install_kustomize <version> <dest-dir> — fetch the kustomize release tarball
# for the detected platform (same URL shape the CI test-shell job uses) and
# verify it against the release's checksums.txt before installing. Returns
# non-zero without installing on a download or checksum failure, so a corrupted
# or truncated tarball never lands in bin/.
#
# SECURITY SCOPE: the checksums.txt is live-fetched from the same release, so
# this is a transmission-integrity check only. It matches how
# hack/install-test-deps.sh verifies chainsaw — NOT the stronger pinned-constant
# hashes that script applies to kind, kubectl, and flux (whose repo-side hashes
# survive a compromised release page that swaps both the binary and its checksum
# file). Kustomize stays on the live-fetch path deliberately: its version is
# resolved dynamically from ci.yaml's KUSTOMIZE_VERSION, so pinning four platform
# hashes here would duplicate a version-coupled literal and break the Renovate
# self-heal the rest of this hook is built around.
_install_kustomize() {
  local version="$1" dest="$2" plat os arch tarball url sums tmp expected actual
  plat="$(_detect_platform)" || return 1
  os="${plat% *}"
  arch="${plat#* }"
  tarball="kustomize_${version}_${os}_${arch}.tar.gz"
  url="https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/${version}/${tarball}"
  sums="https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize/${version}/checksums.txt"
  tmp="$(mktemp -d)" || return 1
  # Bound both fetches: this hook is sourced synchronously on every `nix develop`,
  # so a blackholed or crawling mirror must surface as a collected warning rather
  # than wedge shell entry with a curl that hangs forever.
  if curl -fsSL --connect-timeout 10 --max-time 120 "${url}" -o "${tmp}/kustomize.tar.gz" &&
    curl -fsSL --connect-timeout 10 --max-time 120 "${sums}" -o "${tmp}/checksums.txt"; then
    expected="$(awk -v f="${tarball}" '$2 == f { print $1; exit }' "${tmp}/checksums.txt")"
    actual="$(_sha256 "${tmp}/kustomize.tar.gz")"
    if [ -n "${expected}" ] && [ "${expected}" = "${actual}" ] &&
      tar -xzf "${tmp}/kustomize.tar.gz" -C "${tmp}" kustomize; then
      install -m 0755 "${tmp}/kustomize" "${dest}/kustomize"
      rm -rf "${tmp}"
      return 0
    fi
  fi
  rm -rf "${tmp}"
  return 1
}

# provision_devshell — install every pinned tool into bin/, then export PATH and
# KUBEBUILDER_ASSETS. Each step degrades loudly (collected warning summary)
# instead of aborting the interactive shell when offline.
provision_devshell() {
  local repo_bin="${REPO_ROOT}/bin"
  mkdir -p "${repo_bin}"

  local controller_gen_version gofumpt_version golangci_lint_version
  local setup_envtest_ref envtest_k8s_version kustomize_version helm_unittest_version
  controller_gen_version="$(resolve_controller_gen_version)"
  gofumpt_version="$(resolve_gofumpt_version)"
  golangci_lint_version="$(resolve_golangci_lint_version)"
  setup_envtest_ref="$(resolve_setup_envtest_ref)"
  envtest_k8s_version="$(resolve_envtest_k8s_version)"
  kustomize_version="$(resolve_kustomize_version)"
  helm_unittest_version="$(resolve_helm_unittest_version)"

  local -a warnings=()

  if ! command -v go >/dev/null 2>&1; then
    echo "forge devshell: go not found on PATH — skipping Go-tool installs." >&2
    warnings+=("go toolchain (not on PATH)")
  else
    # gofumpt — reuse the Makefile target (installs into bin/ at the pin).
    make -C "${REPO_ROOT}" -s install-gofumpt || warnings+=("gofumpt ${gofumpt_version}")

    if ! _tool_has "${repo_bin}/controller-gen" --version "${controller_gen_version}"; then
      GOBIN="${repo_bin}" go install \
        "sigs.k8s.io/controller-tools/cmd/controller-gen@${controller_gen_version}" ||
        warnings+=("controller-gen ${controller_gen_version}")
    fi

    # golangci-lint prints its version without a leading "v".
    if ! _tool_has "${repo_bin}/golangci-lint" version "${golangci_lint_version#v}"; then
      GOBIN="${repo_bin}" go install \
        "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${golangci_lint_version}" ||
        warnings+=("golangci-lint ${golangci_lint_version}")
    fi

    # setup-envtest is pinned to a branch ref (not version-checkable) — stamp it.
    local envtest_stamp="${repo_bin}/.setup-envtest-ref"
    if [ ! -x "${repo_bin}/setup-envtest" ] ||
      [ "$(cat "${envtest_stamp}" 2>/dev/null || true)" != "${setup_envtest_ref}" ]; then
      if GOBIN="${repo_bin}" go install \
        "sigs.k8s.io/controller-runtime/tools/setup-envtest@${setup_envtest_ref}"; then
        printf '%s\n' "${setup_envtest_ref}" >"${envtest_stamp}"
      else
        warnings+=("setup-envtest ${setup_envtest_ref}")
      fi
    fi
  fi

  # kustomize — release tarball for the detected platform.
  if ! _tool_has "${repo_bin}/kustomize" version "${kustomize_version}"; then
    _install_kustomize "${kustomize_version}" "${repo_bin}" ||
      warnings+=("kustomize ${kustomize_version}")
  fi

  # chainsaw, kind, kubectl, flux — reuse the pinned, checksum-verifying script.
  INSTALL_DIR="${repo_bin}" WITH_FLUX_CLI=true bash "${REPO_ROOT}/hack/install-test-deps.sh" ||
    warnings+=("chainsaw/kind/kubectl/flux (hack/install-test-deps.sh)")

  # helm-unittest — install into a project-scoped plugin dir so the user's global
  # helm plugin store is never mutated. --verify=false matches CI (the plugin is
  # unsigned).
  if command -v helm >/dev/null 2>&1; then
    local helm_plugins_project="${repo_bin}/helm-plugins"
    mkdir -p "${helm_plugins_project}"
    # `helm plugin install` treats HELM_PLUGINS as a single install target (a
    # ListSeparator-joined value would create a bogus colon-named directory), so
    # scope the override to the install invocation and the idempotency probe.
    if ! _helm_plugin_pinned \
      "$(HELM_PLUGINS="${helm_plugins_project}" helm plugin list 2>/dev/null)" \
      unittest "${helm_unittest_version}"; then
      HELM_PLUGINS="${helm_plugins_project}" helm plugin install \
        https://github.com/helm-unittest/helm-unittest \
        --version "${helm_unittest_version}" --verify=false ||
        warnings+=("helm-unittest ${helm_unittest_version}")
    fi
    # For the interactive session, prepend the project dir to helm's plugin
    # search path (a ListSeparator-joined list helm scans in full) instead of
    # replacing it, so globally-installed plugins (helm-diff, helm-secrets, …)
    # stay visible. Query helm for the effective path first so an unset
    # HELM_PLUGINS still keeps the default global plugins.
    local helm_plugins_existing
    helm_plugins_existing="$(helm env 2>/dev/null | sed -n 's/^HELM_PLUGINS="\(.*\)"$/\1/p')"
    export HELM_PLUGINS="${helm_plugins_project}${helm_plugins_existing:+:${helm_plugins_existing}}"
  else
    warnings+=("helm-unittest (helm not on PATH)")
  fi

  export PATH="${repo_bin}:${PATH}"

  # envtest assets at the pinned k8s minor — the Makefile's SETUP_ENVTEST ?= …
  # picks up this export so `make test-integration` uses bin/setup-envtest.
  export SETUP_ENVTEST="${repo_bin}/setup-envtest"
  if [ -x "${SETUP_ENVTEST}" ]; then
    local assets
    if assets="$("${SETUP_ENVTEST}" use "${envtest_k8s_version}" -p path 2>/dev/null)" &&
      [ -n "${assets}" ]; then
      export KUBEBUILDER_ASSETS="${assets}"
    else
      warnings+=("envtest assets (k8s ${envtest_k8s_version})")
    fi
  fi

  if [ "${#warnings[@]}" -gt 0 ]; then
    echo "forge devshell: could not provision the following (offline/network?):" >&2
    local w
    for w in "${warnings[@]}"; do
      echo "  - ${w}" >&2
    done
    echo "forge devshell: re-run 'source hack/nix-devshell-hook.sh' when back online." >&2
  else
    echo "forge devshell: CI toolchain ready — bin/ prepended to PATH."
  fi
}

# ---------------------------------------------------------------------------
# Entry point. --print-pins runs strict (executed by the unit test); the
# provisioning path stays lenient because it is sourced into an interactive
# shell where `set -e` would kill the session on the first offline install.
# ---------------------------------------------------------------------------
if [ "${1:-}" = "--print-pins" ]; then
  set -euo pipefail
  print_pins
  exit 0
fi

# Provision on entry (the flake sources this with no arguments). The unit test
# sources the hook only to exercise the pure helpers, so it sets
# NIX_DEVSHELL_HOOK_NO_PROVISION=1 to skip the side-effecting install path.
if [ "${NIX_DEVSHELL_HOOK_NO_PROVISION:-}" != "1" ]; then
  provision_devshell
fi
