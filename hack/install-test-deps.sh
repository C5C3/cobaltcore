#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/install-test-deps.sh — Install pinned E2E test dependencies.
#
# By default installs: chainsaw, kind, kubectl. The Flux CLI is optional after
# the FluxInstance bootstrap migration (CC-0085, REQ-004) and is installed
# only when WITH_FLUX_CLI=true is exported.
# Feature: CC-0010, CC-0085

set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"

# ---------------------------------------------------------------------------
# Pinned versions
# ---------------------------------------------------------------------------
CHAINSAW_VERSION="v0.2.14"
FLUX_VERSION="2.5.1"
KIND_VERSION="v0.27.0"
KUBECTL_VERSION="v1.33.1"

# ---------------------------------------------------------------------------
# Pinned SHA256 hashes (CC-0010).
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
FLUX_SHA256_linux_amd64="f64c85db4b94aefcdf6e0f2825c32573fc2bd234e5489ff332fee62776973ec3"
# shellcheck disable=SC2034
FLUX_SHA256_linux_arm64="35b6160d6b3c9ec3bbfe3f526927e713d877c274e7debffd13e270e47221a79f"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_amd64="8618395bbdd35b681768e26612e1c2f9cb6d67060f7e2df0f8d36ca67783478e"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_arm64="68c025b8059934457978d8952c0c62fd06c585d46b334804da72d268eaf630d4"

# shellcheck disable=SC2034
KIND_SHA256_linux_amd64="a6875aaea358acf0ac07786b1a6755d08fd640f4c79b7a2e46681cc13f49a04b"
# shellcheck disable=SC2034
KIND_SHA256_linux_arm64="5e4507a41c69679562610b1be82ba4f80693a7826f4e9c6e39236169a3e4f9d0"
# shellcheck disable=SC2034
KIND_SHA256_darwin_amd64="3435134325b6b9406ccfec417b13bb46a808fc74e9a2ebb0ca31b379c8293863"
# shellcheck disable=SC2034
KIND_SHA256_darwin_arm64="5240ca1acb587e1d0386532dd8c3373d81f5173b5af322919fc56f0cdd646596"

# shellcheck disable=SC2034
KUBECTL_SHA256_linux_amd64="5de4e9f2266738fd112b721265a0c1cd7f4e5208b670f811861f699474a100a3"
# shellcheck disable=SC2034
KUBECTL_SHA256_linux_arm64="d595d1a26b7444e0beb122e25750ee4524e74414bbde070b672b423139295ce6"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_amd64="8d36a5c66142547ad16e332942fd16a0ca2b3346d9ebaab6c348de2c70d9d875"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_arm64="8ae6823839993bb2e394c3cf1919748e530642c625dc9100159595301f53bdeb"

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

  # Verify download integrity against release checksums (CC-0010).
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

  # Verify download integrity against pinned SHA256 hash (CC-0010).
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

  # Verify download integrity against pinned SHA256 hash (CC-0010).
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

  # Verify download integrity against pinned SHA256 hash (CC-0010).
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
  # (CC-0085, REQ-004). Set WITH_FLUX_CLI=true to install it anyway.
  if [[ "${WITH_FLUX_CLI:-false}" == "true" ]]; then
    install_flux
  fi
  install_kind
  install_kubectl
  log "=== Done ==="
}

main "$@"
