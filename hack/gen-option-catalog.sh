#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/gen-option-catalog.sh — Extract a per-release option catalog from a
# shipped service image.
#
# Runs the service image's own oslo-config-generator (--format json) against
# the upstream generator config for the pinned release, then post-processes the
# machine-readable output into the compact catalog format consumed by
# internal/common/config.ParseOptionCatalog. The machine-readable output spells
# option names with dashes; the catalog holds the underscore form oslo.config
# reads from a file, so names are normalized on the way out.
#
# Usage:
#   hack/gen-option-catalog.sh [--check] <service> <release> [image-ref]
#
# Without --check the catalog is written to
# operators/<service>/api/v1alpha1/catalogs/<release>.json. With --check the
# freshly generated catalog is diffed against the committed file and any
# difference (or a missing committed file) is a non-zero exit — the committed
# files are byte-stable caches verified this way.
#
# Host dependencies: docker, curl, yq. All Python runs inside the service image
# so no host Python is required (macOS-safe).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

usage() {
  cat >&2 <<'EOF'
Usage: hack/gen-option-catalog.sh [--check] <service> <release> [image-ref]

  --check       diff the generated catalog against the committed file instead
                of writing it; non-zero exit on any difference.
  service       keystone, glance, placement, barbican, or neutron.
  release       release directory name (e.g. 2025.2).
  image-ref     service image to extract from
                (default: ghcr.io/c5c3/<service>:<release>).
EOF
}

# ---------------------------------------------------------------------------
# 1. Parse arguments
# ---------------------------------------------------------------------------
CHECK=false
if [[ "${1:-}" == "--check" ]]; then
  CHECK=true
  shift
fi
SERVICE="${1:-}"
RELEASE="${2:-}"
IMAGE_REF="${3:-}"
if [[ -z "${SERVICE}" || -z "${RELEASE}" ]]; then
  usage
  exit 1
fi

# ---------------------------------------------------------------------------
# 2. Resolve the upstream source ref (before the service->path mapping so an
#    unlisted service reports the missing ref, not an unsupported service).
# ---------------------------------------------------------------------------
REFS_FILE="${REPO_ROOT}/releases/${RELEASE}/source-refs.yaml"
SERVICE_REF=""
if [[ -f "${REFS_FILE}" ]]; then
  SERVICE_REF="$(yq ".\"${SERVICE}\"" "${REFS_FILE}")"
fi
if [[ -z "${SERVICE_REF}" || "${SERVICE_REF}" == "null" ]]; then
  echo "no source ref for ${SERVICE} in releases/${RELEASE}/source-refs.yaml" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 3. Map the service to one or more oslo-config-generator config paths, to the
#    number of sections a complete extraction must yield, and to the namespaces
#    to drop from the generator config. That floor catches a truncated generator
#    run, and it is per service because a full run yields very different counts:
#    keystone registers 44 sections, glance 30 and barbican 22, so 10 is a loose
#    guard for all three, while placement registers 8 (2025.2) and 9 (2026.1).
#    Every service but neutron maps to a single path; neutron unions three
#    per-process generator files, see its case below.
# ---------------------------------------------------------------------------
DROP_NAMESPACES=""
case "${SERVICE}" in
  keystone)
    GEN_CONFIG_PATHS="config-generator/keystone.conf"
    MIN_SECTIONS=10
    ;;
  glance)
    GEN_CONFIG_PATHS="etc/oslo-config-generator/glance-api.conf"
    MIN_SECTIONS=10
    ;;
  placement)
    GEN_CONFIG_PATHS="etc/placement/config-generator.conf"
    MIN_SECTIONS=8
    ;;
  barbican)
    GEN_CONFIG_PATHS="etc/oslo-config-generator/barbican.conf"
    MIN_SECTIONS=10
    # barbican registers its kmip secret store unconditionally while the image
    # ships no pykmip, and a namespace the image cannot import aborts the whole
    # generator run. The catalog describes what the shipped image accepts, and a
    # plugin the image cannot load accepts nothing.
    DROP_NAMESPACES="barbican.plugin.secret_store.kmip"
    ;;
  neutron)
    # neutron splits its options across per-process generator configs. The
    # operator renders neutron.conf and ml2_conf.ini, which the server loads
    # into one oslo CONF, and neutron_ovn_metadata_agent.ini for the metadata
    # agent, so the catalog is the union of those three files' namespaces.
    # oslo.log appears in all three and is kept once. 10 is the same loose
    # floor keystone, glance and barbican use; a full neutron extraction
    # registers well over 10 sections.
    # The catalog format carries no per-section provenance, so the union is
    # flat: a metadata-agent-only section ([metadata_rate_limiting], [agent],
    # [ovs]) validates the same as a server-only one ([ml2], [ovn_nb_global],
    # [service_providers]), and an option placed in the wrong process' config
    # passes validation while oslo.config silently ignores it at runtime.
    # #904 has to decide before it embeds these files whether to emit one
    # catalog per rendered file or to add a provenance field, because the
    # catalog shape becomes an API at that point.
    GEN_CONFIG_PATHS="etc/oslo-config-generator/neutron.conf etc/oslo-config-generator/ml2_conf.ini etc/oslo-config-generator/neutron_ovn_metadata_agent.ini"
    MIN_SECTIONS=10
    ;;
  *)
    echo "unsupported service ${SERVICE}: no oslo-config-generator config mapping" >&2
    exit 1
    ;;
esac

WORK_DIR="$(mktemp -d "/tmp/gen-option-catalog.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT
# mktemp -d creates the directory 0700 for the invoking user, but the generator
# reads the bind-mounted /work as the image's non-root user, so the directory
# and the config it holds must be world-readable.
chmod 755 "${WORK_DIR}"

# ---------------------------------------------------------------------------
# 4. Obtain the upstream generator configs. SOURCE_DIR points at a local source
#    checkout; otherwise fetch each file from the pinned ref on GitHub. Each
#    file is appended to upstream.conf in GEN_CONFIG_PATHS order.
# ---------------------------------------------------------------------------
for GEN_CONFIG_PATH in ${GEN_CONFIG_PATHS}; do
  if [[ -n "${SOURCE_DIR:-}" ]]; then
    SRC_CONFIG="${SOURCE_DIR}/${GEN_CONFIG_PATH}"
    if [[ ! -f "${SRC_CONFIG}" ]]; then
      echo "generator config not found: ${SRC_CONFIG}" >&2
      exit 1
    fi
    cat "${SRC_CONFIG}" >> "${WORK_DIR}/upstream.conf"
  else
    if ! curl -fsSL \
      "https://raw.githubusercontent.com/openstack/${SERVICE}/${SERVICE_REF}/${GEN_CONFIG_PATH}" \
      >> "${WORK_DIR}/upstream.conf"; then
      echo "fetching generator config for ${SERVICE} at ${SERVICE_REF} (${GEN_CONFIG_PATH}) failed" >&2
      exit 1
    fi
  fi
done

# ---------------------------------------------------------------------------
# 5. Build the generator config: a [DEFAULT] header plus only the namespace
#    lines from the upstream configs (dropping output_file/wrap_width so the
#    generator writes JSON to stdout). The first occurrence of each identical
#    namespace line wins, in GEN_CONFIG_PATHS order, so a namespace several
#    files list is registered once. Commented namespaces are excluded, and so
#    are the DROP_NAMESPACES entries: a namespace the image registers but cannot
#    import aborts the whole generator run. A namespace the image does not
#    register at all is left in and reaches the stevedore warning path in step 7.
# ---------------------------------------------------------------------------
NAMESPACE_LINES="$(grep -E '^[[:space:]]*namespace[[:space:]]*=' "${WORK_DIR}/upstream.conf" | awk '!seen[$0]++' || true)"
for NAMESPACE in ${DROP_NAMESPACES}; do
  NAMESPACE_LINES="$(printf '%s\n' "${NAMESPACE_LINES}" |
    grep -vE "^[[:space:]]*namespace[[:space:]]*=[[:space:]]*${NAMESPACE}[[:space:]]*$" || true)"
done
{
  echo "[DEFAULT]"
  printf '%s\n' "${NAMESPACE_LINES}"
} > "${WORK_DIR}/gen.conf"
chmod 644 "${WORK_DIR}/gen.conf"

# ---------------------------------------------------------------------------
# 6. Resolve the image ref and ensure it is present locally.
# ---------------------------------------------------------------------------
IMAGE_REF="${IMAGE_REF:-ghcr.io/c5c3/${SERVICE}:${RELEASE}}"
if ! docker image inspect "${IMAGE_REF}" >/dev/null 2>&1; then
  docker pull "${IMAGE_REF}"
fi

# ---------------------------------------------------------------------------
# 7. Run the generator inside the image. A namespace listed but not installed
#    (e.g. glance's os_brick) degrades to a stevedore warning on stderr, which
#    we let pass through; only a non-zero generator exit is fatal.
# ---------------------------------------------------------------------------
docker run --rm -v "${WORK_DIR}:/work" "${IMAGE_REF}" \
  /var/lib/openstack/bin/oslo-config-generator \
  --config-file /work/gen.conf --format json > "${WORK_DIR}/raw.json"

# ---------------------------------------------------------------------------
# 8./9. Transform the raw JSON into the compact catalog using the image's own
#    Python (stdlib json only). The plausibility floor is enforced here so a
#    truncated run exits non-zero before anything is written.
# ---------------------------------------------------------------------------
TRANSFORM=$(cat <<'PYEOF'
import json
import sys


def normalize(name):
    # oslo emits raw option names with dashes; a config file spells them with
    # underscores, which is the form the catalog must hold.
    return name.replace("-", "_")


service = sys.argv[1]
release = sys.argv[2]
min_sections = int(sys.argv[3])
options = json.load(sys.stdin)["options"]

opts_by_section = {}   # section -> set of current option names
removal = {}           # section -> set of names deprecated for removal (no rename)
renamed = {}           # section -> {old_name: "[current_section] current_name"}

for section, group in options.items():
    for opt in group["opts"]:
        current = normalize(opt["name"])
        opts_by_section.setdefault(section, set()).add(current)
        deprecated_opts = opt.get("deprecated_opts") or []
        for old in deprecated_opts:
            old_section = old.get("group") or section or "DEFAULT"
            old_name = normalize(old["name"])
            renamed.setdefault(old_section, {})[old_name] = "[%s] %s" % (section, current)
        # A live option that is being removed with no replacement records a
        # bare deprecation marker under its own name, but only survives below
        # if that name is not itself a live opt in the section (opts wins).
        if opt.get("deprecated_for_removal") and not deprecated_opts:
            removal.setdefault(section, set()).add(current)

sections = {}
for section in set(opts_by_section) | set(removal) | set(renamed):
    live = opts_by_section.get(section, set())
    deprecated = {}
    for name in removal.get(section, set()):
        deprecated[name] = ""
    # A rename carries a replacement and outranks a bare removal marker.
    for name, replacement in renamed.get(section, {}).items():
        deprecated[name] = replacement
    # A name that is a live opt never doubles as a deprecated key.
    deprecated = {name: value for name, value in deprecated.items() if name not in live}
    sections[section] = {"opts": sorted(live), "deprecated": deprecated}

total = sum(len(section["opts"]) for section in sections.values())
if len(sections) < min_sections or total < 100 or "DEFAULT" not in sections:
    sys.stderr.write(
        "plausibility floor not met for %s %s: sections=%d (min %d) options=%d default=%s\n"
        % (service, release, len(sections), min_sections, total, "DEFAULT" in sections)
    )
    sys.exit(1)

catalog = {"service": service, "release": release, "sections": sections}
sys.stdout.write(json.dumps(catalog, indent=2, sort_keys=True) + "\n")
PYEOF
)

docker run -i --rm "${IMAGE_REF}" /var/lib/openstack/bin/python3 -c "${TRANSFORM}" \
  "${SERVICE}" "${RELEASE}" "${MIN_SECTIONS}" < "${WORK_DIR}/raw.json" > "${WORK_DIR}/catalog.json"

# ---------------------------------------------------------------------------
# 10. Write the catalog, or (in --check mode) diff it against the committed one.
# ---------------------------------------------------------------------------
CATALOG_DIR="${REPO_ROOT}/operators/${SERVICE}/api/v1alpha1/catalogs"
CATALOG_FILE="${CATALOG_DIR}/${RELEASE}.json"

if [[ "${CHECK}" == true ]]; then
  if [[ ! -f "${CATALOG_FILE}" ]]; then
    echo "committed catalog missing: ${CATALOG_FILE}" >&2
    diff -u /dev/null "${WORK_DIR}/catalog.json" || true
    exit 1
  fi
  if ! diff -u "${CATALOG_FILE}" "${WORK_DIR}/catalog.json"; then
    exit 1
  fi
  echo "catalog up to date: ${CATALOG_FILE}"
else
  mkdir -p "${CATALOG_DIR}"
  mv "${WORK_DIR}/catalog.json" "${CATALOG_FILE}"
  echo "wrote ${CATALOG_FILE}"
fi
