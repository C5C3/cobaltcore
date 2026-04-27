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
FLUX_VERSION="2.8.6"
KIND_VERSION="v0.31.0"
KUBECTL_VERSION="v1.36.0"

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
FLUX_SHA256_linux_amd64="c53cc990ae266f7840f64c81515d701d8821d558a9062aa4211d71b38cf044be"
# shellcheck disable=SC2034
FLUX_SHA256_linux_arm64="bc460320c2d33ad833791277896dd1aaf1cff6b3e64ba397c44238f00d4ae5bc"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_amd64="83ce032f39248ed04324f3e50344794575fb5f7149f24c071972e320b64826a6"
# shellcheck disable=SC2034
FLUX_SHA256_darwin_arm64="20de67ebf2da689dd165b004dc073469f33aa2a3eac45a69f38a40435e14d20b"

# shellcheck disable=SC2034
KIND_SHA256_linux_amd64="eb244cbafcc157dff60cf68693c14c9a75c4e6e6fedaf9cd71c58117cb93e3fa"
# shellcheck disable=SC2034
KIND_SHA256_linux_arm64="8e1014e87c34901cc422a1445866835d1e666f2a61301c27e722bdeab5a1f7e4"
# shellcheck disable=SC2034
KIND_SHA256_darwin_amd64="a8b3cf77b2ad77aec5bf710d1a2589d9117576132af812885cad41e9dede4d4e"
# shellcheck disable=SC2034
KIND_SHA256_darwin_arm64="88bf554fe9da6311c9f8c2d082613c002911a476f6b5090e9420b35d84e70c5c"

# shellcheck disable=SC2034
KUBECTL_SHA256_linux_amd64="123d8c8844f46b1244c547fffb3c17180c0c26dac9890589fe7e67763298748e"
# shellcheck disable=SC2034
KUBECTL_SHA256_linux_arm64="9f9d9c44a7b5264515ac9da5991584e2395bd50662e651132337e7b4d0c56f8f"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_amd64="06d7e9a3a26a326d43102c70b19f9d233db219d09890e558dbdc3647db732f06"
# shellcheck disable=SC2034
KUBECTL_SHA256_darwin_arm64="4bcf268eacdc1d2df74e37d86f639f27ca7dea3ae185b7b452b73b9fb5ddc14e"

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
