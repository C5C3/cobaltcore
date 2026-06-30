#!/usr/bin/env bash
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

# hack/ci-govulncheck.sh — Run govulncheck across workspace modules with an
# explicit, justified allowlist for advisories that are not actionable here.
#
# govulncheck has no native suppression flag, so this wrapper runs it in JSON
# mode per module, keeps only the *reachable* symbol-level findings (the ones
# that fail the default text report), and drops any whose advisory ID appears in
# the ALLOWLIST below. The build fails if, and only if, a reachable finding
# survives the allowlist — matching govulncheck's normal exit-3 semantics while
# letting us ride out advisories that have no fix and no real exposure.
#
# Every ALLOWLIST entry MUST carry a one-line justification and is expected to be
# present: if an allowlisted advisory is no longer reported, the wrapper prints a
# notice so the stale entry can be removed.
#
# Usage:
#   hack/ci-govulncheck.sh <module-dir> [<module-dir> ...]
#
# Requires: govulncheck, jq (both available on CI runners and via `make` deps).

set -euo pipefail

# ALLOWLIST maps an OSV/Go advisory ID to the reason it is not actionable here.
# Remove an entry once a fixed dependency version is available and pinned.
declare -A ALLOWLIST=(
	# GO-2026-5377 (CVE-2026-42876, GHSA-fq7h-9x26-6j22): privilege escalation via
	# secret overwriting in the External Secrets *controller*. forge only imports
	# the external-secrets/apis types for scheme registration in envtest/simulator
	# test harnesses and never runs the ESO controller, so the vulnerable code path
	# is not shipped. The advisory's apis-submodule range is "introduced: 0" with no
	# fixed event (the fix, v2.4.1, lands in the main module), so no apis bump clears
	# this. Drop once the Go vuln DB records a fixed apis version we can pin to.
	[GO-2026-5377]="external-secrets/apis types only; ESO controller is never run, no fixed apis version exists"
)

if [[ $# -eq 0 ]]; then
	echo "usage: $0 <module-dir> [<module-dir> ...]" >&2
	exit 2
fi

workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

# seen tracks which allowlisted IDs were actually observed, so we can flag stale
# allowlist entries that no longer correspond to a live finding.
declare -A seen=()
blocked=0

for dir in "$@"; do
	echo "Scanning ${dir} module..."
	out="${workdir}/$(echo "$dir" | tr '/' '_').json"
	err="${out}.err"

	# JSON mode exits 0 even when vulnerabilities are found; a non-zero exit means
	# govulncheck itself failed (build error, bad module), which must fail the run.
	if ! (cd "$dir" && govulncheck -format json ./...) >"$out" 2>"$err"; then
		echo "  ERROR: govulncheck failed to run in ${dir}:" >&2
		cat "$err" >&2
		exit 1
	fi

	# Reachable findings have a top trace frame with a resolved function — exactly
	# the ones govulncheck's text "Symbol Results" report fails on.
	mapfile -t reachable < <(
		jq -rs '[.[] | select(.finding) | .finding
			| select(.trace[0].function != null) | .osv] | unique[]' "$out"
	)

	for osv in "${reachable[@]}"; do
		if [[ -v "ALLOWLIST[$osv]" ]]; then
			seen[$osv]=1
			summary="$(jq -rs --arg id "$osv" \
				'[.[] | select(.osv.id == $id) | .osv.summary][0] // ""' "$out")"
			echo "  ALLOWLISTED ${osv}: ${ALLOWLIST[$osv]}"
			[[ -n "$summary" ]] && echo "    (${summary})"
		else
			summary="$(jq -rs --arg id "$osv" \
				'[.[] | select(.osv.id == $id) | .osv.summary][0] // ""' "$out")"
			echo "  VULNERABLE ${osv}: ${summary}" >&2
			echo "    More info: https://pkg.go.dev/vuln/${osv}" >&2
			blocked=1
		fi
	done
done

# Warn about allowlist entries that no longer match any reachable finding so the
# suppression list does not silently rot.
for osv in "${!ALLOWLIST[@]}"; do
	if [[ ! -v "seen[$osv]" ]]; then
		echo "NOTE: allowlisted ${osv} was not reported by any module — remove the stale ALLOWLIST entry." >&2
	fi
done

if [[ "$blocked" -ne 0 ]]; then
	echo "govulncheck: reachable, non-allowlisted vulnerabilities found." >&2
	exit 1
fi

echo "govulncheck: no actionable vulnerabilities (allowlisted advisories suppressed)."
