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
FLUX_VERSION="2.9.4"
KIND_VERSION="v0.32.0"
KUBECTL_VERSION="v1.36.3"

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
FLUX_SHA256_linux_amd64="4092b367fd060097976fb7261601f79420ecf7266328417dfd8dfee27de0e6a3"
# shellcheck disable=SC2034
FLUX_SHA256_linux_arm64="665bfe5aeb843faf907e7f77250c31840499ff2dc65a64c2479409e3728748d1"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_amd64="e4583db32e4f4f4ec195084f0381c704e5104a55a7ec4c1a444396137ecdb161"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_arm64="1dd6791738b1c250748c24c24b48e92908881596f999ad8bf10f84763d66fdb3"

# shellcheck disable=SC2034
KIND_SHA256_linux_amd64="50030de23cf40a18505f20426f6a8506bedf13c6e509244bd1fa9463721b0f54"
# shellcheck disable=SC2034
KIND_SHA256_linux_arm64="b92cd615e97585de8ddade28ed5cd7feb4248d717c233eea5b03c37298900f5d"
# shellcheck disable=SC2034
KIND_SHA256_darwin_amd64="295ac6d0d634c9819c9907df45e3017d1f13166bd13c3404c45e79f7faa47498"
# shellcheck disable=SC2034
KIND_SHA256_darwin_arm64="dca67911095a110c2b5c36e26df6cac860c602033e456c0db47be498cdef1ebb"

# shellcheck disable=SC2034
KUBECTL_SHA256_linux_amd64="1e9045ec32bea85da43de85f0065358529ea7c7a152eca78154fba5b58c27d82"
# shellcheck disable=SC2034
KUBECTL_SHA256_linux_arm64="c957eb8c4bea27a3bb35b269edd9082e27f027f7b76b20b5bf4afebc726c6d3e"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_amd64="ce6c5e55cd17559e87e4fb5e73ebbbc2511bcf2b695d7a40c1b1461a9817d4b3"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_arm64="4408c85c83fd3a31adaa555bdf3c7a6c81f74b19449a9060ba31ab91926f023d"

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
