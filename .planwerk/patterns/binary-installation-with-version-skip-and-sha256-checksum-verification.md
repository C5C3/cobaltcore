# Pattern: Binary installation with version-skip and SHA256 checksum verification

**Component**: hack/
**Category**: configuration
**Applies-When**: Adding a script that downloads and installs CLI tool binaries from GitHub Releases or upstream URLs for development/CI use

## Description

Each install_* function follows a consistent pattern: (1) check if the tool is already installed at the correct version (skip if so), (2) download the binary to a temporary directory, (3) verify SHA256 checksum via a shared verify_sha256 helper, (4) install with 0755 permissions. The temporary directory is cleaned up via trap RETURN. Version detection uses tool-specific version commands with grep -oP for version extraction. Platform detection (OS/ARCH) is centralized in a detect_platform function.

SHA256 hashes are pinned as per-platform constants in associative arrays (e.g., FLUX_SHA256, KIND_SHA256) at the top of the script, rather than fetched from the same GitHub Release as the binary. This prevents a compromised release page from substituting both the binary and its checksum file simultaneously. When pinned hashes are not yet available for a tool (e.g., due to release naming changes), upstream-fetched checksums are used as a fallback with a NOTE comment explaining why. To update after a version bump: download artifacts, compute sha256sum, replace the array values.

## Examples

### `hack/install-test-deps.sh:33-50`

```
declare -A FLUX_SHA256=(
  ["linux_amd64"]="f64c85db4b94aefcdf6e0f2825c32573fc2bd234e5489ff332fee62776973ec3"
  ["linux_arm64"]="35b6160d6b3c9ec3bbfe3f526927e713d877c274e7debffd13e270e47221a79f"
  ["darwin_amd64"]="8618395bbdd35b681768e26612e1c2f9cb6d67060f7e2df0f8d36ca67783478e"
  ["darwin_arm64"]="68c025b8059934457978d8952c0c62fd06c585d46b334804da72d268eaf630d4"
)
```

### `hack/install-test-deps.sh:176-182`

```
  # Verify download integrity against pinned SHA256 hash (CC-0010).
  local expected_hash="${FLUX_SHA256[${OS}_${ARCH}]}"
  if [[ -z "${expected_hash}" ]]; then
    log "ERROR: No pinned SHA256 hash for flux ${want} on ${OS}/${ARCH}."
    exit 1
  fi
  verify_sha256 "${tmpdir}/flux.tar.gz" "${expected_hash}"
```
