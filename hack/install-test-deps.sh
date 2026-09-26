#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/install-test-deps.sh — Install pinned E2E test dependencies.
#
# By default installs: chainsaw, kind, kubectl. The Flux CLI is optional after
# the FluxInstance bootstrap migration and is installed
# only when WITH_FLUX_CLI=true is exported.

set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"

# ---------------------------------------------------------------------------
# Pinned versions
# ---------------------------------------------------------------------------
CHAINSAW_VERSION="v0.2.15"
FLUX_VERSION="2.9.5"
KIND_VERSION="v0.33.0"
KUBECTL_VERSION="v1.37.1"

# ---------------------------------------------------------------------------
# Pinned SHA256 hashes.
#
# Supply-chain hardening: hashes are pinned as constants so that a compromised
# GitHub Release page cannot substitute both the binary and its checksum file
# simultaneously.  To update after a version bump, download the new release
# artifacts, compute sha256sum, and replace the values below.
#
# Chainsaw v0.2.14 hashes are fetched from upstream (not pinned) because pinned
# values were not available at authoring time. Pin them here once verified.
# ---------------------------------------------------------------------------
# Use plain variables instead of associative arrays for bash 3.2 (macOS) compatibility.
# These are referenced via indirect expansion (e.g. ${!_flux_var}).
# shellcheck disable=SC2034
FLUX_SHA256_linux_amd64="b853df82adfd7736f580692f9f734473d571606307139f8fd20c2a80dd1ff473"
# shellcheck disable=SC2034
FLUX_SHA256_linux_arm64="f3e159af616ec0b9bd0a405c2185cf09d06b74652c1de3c7f377e8166826651a"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_amd64="5748583cf5da035ca2d751190d2c15f5f656d305c166ec28a312a2f3b6799e31"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_arm64="2869ef7151a6f1b27e6b5d2a6804f3ef23c7bdaa06a74e00d3fe5bfc646547fd"

# shellcheck disable=SC2034
KIND_SHA256_linux_amd64="aee6151561422756b764a4ae28e7f44cda5af5a9eead3cc9985112b1de8d8e0d"
# shellcheck disable=SC2034
KIND_SHA256_linux_arm64="20022bee6cfcd5086cb7234d218e3454e6090022f2a8f55d1fa7fcf42c3867a2"
# shellcheck disable=SC2034
KIND_SHA256_darwin_amd64="5a99f26f57246dc9319dd294803313197a0f34d33c525b3ea8b655db5916ece0"
# shellcheck disable=SC2034
KIND_SHA256_darwin_arm64="0c8c7dbe5e23594a198b786c4bc13dacc101fa6196b0cb0b23a1ca44e61f4b4f"

# shellcheck disable=SC2034
KUBECTL_SHA256_linux_amd64="6129359f4e1f3848a5572ccb0b26cf28b8ca08cef38c95a765b2f64a2c961a2f"
# shellcheck disable=SC2034
KUBECTL_SHA256_linux_arm64="922df28df248cc00a9e025f947704f1d1482de64ece54cfe57e61f19eaf1eef3"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_amd64="d5276c0f4fde77fc446070290f345944a7f1fda153df6b960e5fde93b7a9bccd"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_arm64="583beedaebe422e71d3f1a96acef8b1fef86ea2f09a45ad01aa6c9ce287c1380"

# ---------------------------------------------------------------------------
# log — Print a timestamped log message (ISO 8601 UTC).
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# detect_platform — Set OS and ARCH from uname.
# ---------------------------------------------------------------------------
detect_platform() {
  local uname_os uname_arch
  uname_os="$(uname -s)"
  uname_arch="$(uname -m)"

  case "${uname_os}" in
    Linux)  OS="linux" ;;
    Darwin) OS="darwin" ;;
    *)
      log "ERROR: Unsupported OS: ${uname_os}"
      exit 1
      ;;
  esac

  case "${uname_arch}" in
    x86_64)  ARCH="amd64" ;;
    aarch64 | arm64) ARCH="arm64" ;;
    *)
      log "ERROR: Unsupported architecture: ${uname_arch}"
      exit 1
      ;;
  esac

  log "Detected platform: ${OS}/${ARCH}"
}

# ---------------------------------------------------------------------------
# verify_sha256 — Verify SHA256 checksum of a downloaded file.
#
# Arguments:
#   $1 — path to the file to verify
#   $2 — expected SHA256 hex digest
# ---------------------------------------------------------------------------
verify_sha256() {
  local file="$1"
  local expected="$2"
  if [[ -z "${expected}" ]]; then
    log "ERROR: Empty expected SHA256 hash for $(basename "${file}")."
    exit 1
  fi
  local actual
  if command -v sha256sum &>/dev/null; then
    actual=$(sha256sum "${file}" | awk '{print $1}')
  else
    actual=$(shasum -a 256 "${file}" | awk '{print $1}')
  fi
  if [[ "${actual}" != "${expected}" ]]; then
    log "ERROR: SHA256 checksum mismatch for $(basename "${file}")"
    log "  expected: ${expected}"
    log "  actual:   ${actual}"
    exit 1
  fi
  log "  SHA256 checksum verified."
}

# ---------------------------------------------------------------------------
# install_chainsaw — Install Kyverno Chainsaw (tarball).
# ---------------------------------------------------------------------------
install_chainsaw() {
  local target="${INSTALL_DIR}/chainsaw"
  local want="${CHAINSAW_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version 2>/dev/null | grep -oE 'v[0-9.]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "chainsaw ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing chainsaw ${want}..."
  local url="https://github.com/kyverno/chainsaw/releases/download/${want}/chainsaw_${OS}_${ARCH}.tar.gz"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/chainsaw.tar.gz"

  # Verify download integrity against release checksums.
  # NOTE: Chainsaw checksums are fetched from upstream (not pinned) because the
  # release asset naming changed across versions and pinned hashes were not
  # available at authoring time. Pin them in the CHAINSAW_SHA256 array once verified.
  local checksums_url="https://github.com/kyverno/chainsaw/releases/download/${want}/checksums.txt"
  curl -fsSL "${checksums_url}" -o "${tmpdir}/checksums.txt"
  local expected_hash
  expected_hash=$(awk "/chainsaw_${OS}_${ARCH}\\.tar\\.gz\$/ {print \$1}" "${tmpdir}/checksums.txt")
  verify_sha256 "${tmpdir}/chainsaw.tar.gz" "${expected_hash}"

  tar -xzf "${tmpdir}/chainsaw.tar.gz" -C "${tmpdir}" chainsaw
  install -m 0755 "${tmpdir}/chainsaw" "${target}"
  log "chainsaw ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# install_flux — Install Flux CLI (tarball).
# ---------------------------------------------------------------------------
install_flux() {
  local target="${INSTALL_DIR}/flux"
  local want="${FLUX_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version --client 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "flux ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing flux ${want}..."
  local url="https://github.com/fluxcd/flux2/releases/download/v${want}/flux_${want}_${OS}_${ARCH}.tar.gz"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/flux.tar.gz"

  # Verify download integrity against pinned SHA256 hash.
  local _flux_var="FLUX_SHA256_${OS}_${ARCH}"
  local expected_hash="${!_flux_var:-}"
  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: No pinned SHA256 hash for flux ${want} on ${OS}/${ARCH}."
    exit 1
  fi
  verify_sha256 "${tmpdir}/flux.tar.gz" "${expected_hash}"

  tar -xzf "${tmpdir}/flux.tar.gz" -C "${tmpdir}" flux
  install -m 0755 "${tmpdir}/flux" "${target}"
  log "flux ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# install_kind — Install kind (standalone binary).
# ---------------------------------------------------------------------------
install_kind() {
  local target="${INSTALL_DIR}/kind"
  local want="${KIND_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version 2>/dev/null | grep -oE 'v[0-9.]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "kind ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing kind ${want}..."
  local url="https://github.com/kubernetes-sigs/kind/releases/download/${want}/kind-${OS}-${ARCH}"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/kind"

  # Verify download integrity against pinned SHA256 hash.
  local _kind_var="KIND_SHA256_${OS}_${ARCH}"
  local expected_hash="${!_kind_var:-}"
  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: No pinned SHA256 hash for kind ${want} on ${OS}/${ARCH}."
    exit 1
  fi
  verify_sha256 "${tmpdir}/kind" "${expected_hash}"

  install -m 0755 "${tmpdir}/kind" "${target}"
  log "kind ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# install_kubectl — Install kubectl (standalone binary).
# ---------------------------------------------------------------------------
install_kubectl() {
  local target="${INSTALL_DIR}/kubectl"
  local want="${KUBECTL_VERSION}"

  if [[ -x "${target}" ]]; then
    local got
    got="$("${target}" version --client 2>/dev/null | grep -oE 'v[0-9.]+' | head -1)" || true
    if [[ "${got}" == "${want}" ]]; then
      log "kubectl ${want} already installed — skipping."
      return
    fi
  fi

  log "Installing kubectl ${want}..."
  local url="https://dl.k8s.io/release/${want}/bin/${OS}/${ARCH}/kubectl"
  local tmpdir
  tmpdir="$(mktemp -d)"
  trap 'rm -rf "${tmpdir:-}"' RETURN

  curl -fsSL "${url}" -o "${tmpdir}/kubectl"

  # Verify download integrity against pinned SHA256 hash.
  local _kubectl_var="KUBECTL_SHA256_${OS}_${ARCH}"
  local expected_hash="${!_kubectl_var:-}"
  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: No pinned SHA256 hash for kubectl ${want} on ${OS}/${ARCH}."
    exit 1
  fi
  verify_sha256 "${tmpdir}/kubectl" "${expected_hash}"

  install -m 0755 "${tmpdir}/kubectl" "${target}"
  log "kubectl ${want} installed to ${target}."
}

# ---------------------------------------------------------------------------
# main
# ---------------------------------------------------------------------------
main() {
  log "=== Installing E2E Test Dependencies ==="
  detect_platform
  mkdir -p "${INSTALL_DIR}"
  install_chainsaw
  # Flux CLI is optional: the kind Quick Start bootstraps Flux via the
  # flux-operator FluxInstance and no longer shells out to `flux`
  # Set WITH_FLUX_CLI=true to install it anyway.
  if [[ "${WITH_FLUX_CLI:-false}" == "true" ]]; then
    install_flux
  fi
  install_kind
  install_kubectl
  log "=== Done ==="
}

main "$@"
