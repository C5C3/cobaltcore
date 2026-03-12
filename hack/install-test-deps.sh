#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# install-test-deps.sh — Installs pinned versions of E2E test dependencies.
# Feature: CC-0010

set -euo pipefail

# ---------------------------------------------------------------------------
# Pinned versions
# ---------------------------------------------------------------------------
CHAINSAW_VERSION="v0.2.14"
FLUX_VERSION="2.5.1"
KIND_VERSION="v0.27.0"
KUBECTL_VERSION="v1.32.3"

# ---------------------------------------------------------------------------
# Install directory
# ---------------------------------------------------------------------------
INSTALL_DIR="${INSTALL_DIR:-${HOME}/.local/bin}"
mkdir -p "${INSTALL_DIR}"

if [[ ":${PATH}:" != *":${INSTALL_DIR}:"* ]]; then
  echo "WARNING: ${INSTALL_DIR} is not on your PATH. Add it with: export PATH=\"${INSTALL_DIR}:\${PATH}\""
fi

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
log() {
  echo "[$(date -u '+%Y-%m-%dT%H:%M:%SZ')] $*"
}

# ---------------------------------------------------------------------------
# Architecture detection
# ---------------------------------------------------------------------------
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "${ARCH}" in
  x86_64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
esac

# ---------------------------------------------------------------------------
# verify_checksum — SHA-256 checksum verification for downloaded files.
#
# Usage: verify_checksum <file_path> <checksum_url> <download_filename>
#   file_path         — local path to the downloaded file
#   checksum_url      — URL of the checksum file to download
#   download_filename — filename to grep for in multi-entry checksum files;
#                       empty string means the checksum file contains only the hash
# ---------------------------------------------------------------------------
verify_checksum() {
  local file_path="$1"
  local checksum_url="$2"
  local download_filename="$3"

  local checksum_file
  checksum_file="$(mktemp)"
  curl -fsSL "${checksum_url}" -o "${checksum_file}"

  local expected_hash
  if [[ -n "${download_filename}" ]]; then
    # Multi-entry format: "<hash>  <filename>" — extract hash for our file.
    expected_hash=$(grep "${download_filename}\$" "${checksum_file}" | awk '{print $1}')
  else
    # Single-hash format: file contains only the hash (e.g. kubectl .sha256).
    expected_hash=$(tr -d '[:space:]' < "${checksum_file}")
  fi
  rm -f "${checksum_file}"

  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: Could not extract expected checksum for ${file_path} from ${checksum_url}"
    return 1
  fi

  local actual_hash
  actual_hash=$(sha256sum "${file_path}" | awk '{print $1}')

  if [[ "${actual_hash}" != "${expected_hash}" ]]; then
    log "ERROR: SHA-256 checksum mismatch for ${file_path}"
    log "  Expected: ${expected_hash}"
    log "  Actual:   ${actual_hash}"
    return 1
  fi

  log "  SHA-256 checksum verified."
}

# ---------------------------------------------------------------------------
# install_tool — generic installer with checksum verification
#
# Usage: install_tool <name> <version> <version_cmd> <url> <mode> \
#          <checksum_url> <checksum_filename>
#   name              — binary name (e.g. chainsaw)
#   version           — expected version string to grep for
#   version_cmd       — command that prints version info
#   url               — download URL
#   mode              — "tarball" or "binary"
#   checksum_url      — URL of the SHA-256 checksum file
#   checksum_filename — filename to match in multi-entry checksum files
#                       (empty string for single-hash files like kubectl)
# ---------------------------------------------------------------------------
install_tool() {
  local name="$1"
  local version="$2"
  local version_cmd="$3"
  local url="$4"
  local mode="$5"
  local checksum_url="${6:-}"
  local checksum_filename="${7:-}"

  # Check if the tool already exists at the correct version.
  if command -v "${name}" &>/dev/null; then
    if eval "${version_cmd}" 2>/dev/null | grep -q "${version}"; then
      log "${name} ${version} already installed — skipping"
      return 0
    fi
  fi

  log "Installing ${name} ${version} ..."

  if [[ "${mode}" == "tarball" ]]; then
    local tmp_dir
    tmp_dir="$(mktemp -d)"
    trap "rm -rf '${tmp_dir}'" RETURN

    curl -fsSL "${url}" -o "${tmp_dir}/${name}.tar.gz"

    if [[ -n "${checksum_url}" ]]; then
      verify_checksum "${tmp_dir}/${name}.tar.gz" "${checksum_url}" "${checksum_filename}"
    fi

    tar -xzf "${tmp_dir}/${name}.tar.gz" -C "${tmp_dir}"
    mv "${tmp_dir}/${name}" "${INSTALL_DIR}/${name}"
    chmod +x "${INSTALL_DIR}/${name}"
  elif [[ "${mode}" == "binary" ]]; then
    curl -fsSL "${url}" -o "${INSTALL_DIR}/${name}"

    if [[ -n "${checksum_url}" ]]; then
      verify_checksum "${INSTALL_DIR}/${name}" "${checksum_url}" "${checksum_filename}"
    fi

    chmod +x "${INSTALL_DIR}/${name}"
  fi

  log "${name} ${version} installed to ${INSTALL_DIR}/${name}"
}

# ---------------------------------------------------------------------------
# Install chainsaw
# ---------------------------------------------------------------------------
install_tool "chainsaw" "${CHAINSAW_VERSION}" \
  "chainsaw version" \
  "https://github.com/kyverno/chainsaw/releases/download/${CHAINSAW_VERSION}/chainsaw_${OS}_${ARCH}.tar.gz" \
  "tarball" \
  "https://github.com/kyverno/chainsaw/releases/download/${CHAINSAW_VERSION}/checksums.txt" \
  "chainsaw_${OS}_${ARCH}.tar.gz"

# ---------------------------------------------------------------------------
# Install flux
# ---------------------------------------------------------------------------
install_tool "flux" "${FLUX_VERSION}" \
  "flux version --client" \
  "https://github.com/fluxcd/flux2/releases/download/v${FLUX_VERSION}/flux_${FLUX_VERSION}_${OS}_${ARCH}.tar.gz" \
  "tarball" \
  "https://github.com/fluxcd/flux2/releases/download/v${FLUX_VERSION}/flux_${FLUX_VERSION}_checksums.txt" \
  "flux_${FLUX_VERSION}_${OS}_${ARCH}.tar.gz"

# ---------------------------------------------------------------------------
# Install kind
# ---------------------------------------------------------------------------
install_tool "kind" "${KIND_VERSION}" \
  "kind version" \
  "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${OS}-${ARCH}" \
  "binary" \
  "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${OS}-${ARCH}.sha256sum" \
  "kind-${OS}-${ARCH}"

# ---------------------------------------------------------------------------
# Install kubectl
# ---------------------------------------------------------------------------
install_tool "kubectl" "${KUBECTL_VERSION}" \
  "kubectl version --client" \
  "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${OS}/${ARCH}/kubectl" \
  "binary" \
  "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${OS}/${ARCH}/kubectl.sha256" \
  ""

log "All E2E test dependencies are installed."
