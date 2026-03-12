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
CHAINSAW_VERSION="v0.3.1"
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
# install_tool — generic installer
#
# Usage: install_tool <name> <version> <version_cmd> <url> <mode>
#   name        — binary name (e.g. chainsaw)
#   version     — expected version string to grep for
#   version_cmd — command that prints version info
#   url         — download URL
#   mode        — "tarball" or "binary"
# ---------------------------------------------------------------------------
install_tool() {
  local name="$1"
  local version="$2"
  local version_cmd="$3"
  local url="$4"
  local mode="$5"

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
    tar -xzf "${tmp_dir}/${name}.tar.gz" -C "${tmp_dir}"
    mv "${tmp_dir}/${name}" "${INSTALL_DIR}/${name}"
    chmod +x "${INSTALL_DIR}/${name}"
  elif [[ "${mode}" == "binary" ]]; then
    curl -fsSL "${url}" -o "${INSTALL_DIR}/${name}"
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
  "tarball"

# ---------------------------------------------------------------------------
# Install flux
# ---------------------------------------------------------------------------
install_tool "flux" "${FLUX_VERSION}" \
  "flux version --client" \
  "https://github.com/fluxcd/flux2/releases/download/v${FLUX_VERSION}/flux_${FLUX_VERSION}_${OS}_${ARCH}.tar.gz" \
  "tarball"

# ---------------------------------------------------------------------------
# Install kind
# ---------------------------------------------------------------------------
install_tool "kind" "${KIND_VERSION}" \
  "kind version" \
  "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${OS}-${ARCH}" \
  "binary"

# ---------------------------------------------------------------------------
# Install kubectl
# ---------------------------------------------------------------------------
install_tool "kubectl" "${KUBECTL_VERSION}" \
  "kubectl version --client" \
  "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${OS}/${ARCH}/kubectl" \
  "binary"

log "All E2E test dependencies are installed."
