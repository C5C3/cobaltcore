#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Validate every CobaltCore CR fixture under tests/ against its current CRD schema.

This is the X1/X2 half of audit-fixture-drift.sh, factored out because the
checks need a real YAML parser: fixtures are multi-document files (a Keystone CR
behind an ExternalSecret, say), and the CRD schema they have to match is nested
eight levels deep in controller-gen output. The grep-and-awk version this
replaced read only each file's first document and silently matched no CRD
properties at all, so X2 never ran.

X1 — every document whose apiVersion is in an API group the repo's own CRDs
declare (spec.group under operators/*/config/crd/bases/) names a kind some CRD
declares, on a version that CRD still serves. Documents in any other *.c5c3.io
group come from an external operator (memcached.c5c3.io: the memcached-operator
chart deploy/flux-system/releases/memcached-operator.yaml installs) and are
reported as one [INFO] line per group. A group that looks like the repo's own
but is not declared — it ends in .openstack.c5c3.io, or its first label names a
repo group (keystone.opnestack.c5c3.io) — is a typo and fails.

X2 — every field in an in-scope document's spec exists in that CRD's schema,
recursively. Descent stops at x-kubernetes-preserve-unknown-fields and at
free-form maps (additionalProperties without properties), which accept any key
by design. Chainsaw assertion keys — "(length(items))", "~.(spec)", "($name)" —
are not CR fields and are skipped along with their subtree.

Prints [PASS]/[FAIL]/[INFO] lines in the audit script's protocol and exits 1 on
any [FAIL]. Run it through the audit script rather than directly:

  bash .claude/skills/check-fixture-drift/scripts/audit-fixture-drift.sh
"""

import re
import sys
from pathlib import Path

import yaml

REPO_ROOT = Path(__file__).resolve().parents[4]

# The whole test tree is walked rather than a list of suite roots: a hardcoded
# list silently loses coverage every time a suite root is added — tests/tempest,
# tests/e2e-ovn-overlay and tests/e2e-controlplane-sso all landed after the
# original three and went unchecked until this was generalized.
TEST_ROOT = "tests"

# A CR field name. Anything else in a fixture mapping is Chainsaw assertion
# syntax — a JMESPath expression, a binding, or a modifier — never a CRD field.
FIELD_NAME = re.compile(r"^[A-Za-z][A-Za-z0-9_.-]*$")

fail_count = 0


def emit(level: str, message: str) -> None:
    global fail_count
    if level == "FAIL":
        fail_count += 1
    print(f"[{level}] {message}")


def header(title: str) -> None:
    print()
    print(f"=== {title} ===")


def load_crds():
    """Map (apiVersion, kind) -> (crd path, spec schema); served versions per kind; API groups."""
    crds = {}
    served = {}
    groups = set()
    for path in sorted(REPO_ROOT.glob("operators/*/config/crd/bases/*.yaml")):
        doc = yaml.safe_load(path.read_text())
        group = doc["spec"]["group"]
        groups.add(group)
        kind = doc["spec"]["names"]["kind"]
        for version in doc["spec"]["versions"]:
            api_version = f"{group}/{version['name']}"
            schema = version.get("schema", {}).get("openAPIV3Schema", {})
            spec_schema = (schema.get("properties") or {}).get("spec", {})
            crds[(api_version, kind)] = (path.relative_to(REPO_ROOT), spec_schema)
            if version.get("served", True):
                served.setdefault(kind, set()).add(api_version)
    return crds, served, groups


def api_group(api_version) -> str:
    """The group of an apiVersion ("" for the core group or a non-string)."""
    if not isinstance(api_version, str) or "/" not in api_version:
        return ""
    return api_version.rsplit("/", 1)[0]


def looks_like_repo_group(group: str, repo_groups: set) -> bool:
    """True for an undeclared group that is probably a typo of a repo group."""
    if group.endswith(".openstack.c5c3.io"):
        return True
    first_labels = {g.split(".", 1)[0] for g in repo_groups if g.count(".") > 1}
    return group.split(".", 1)[0] in first_labels


def walk(schema: dict, obj, path: str, findings: list) -> None:
    """Collect every field in obj that schema does not declare."""
    if not isinstance(obj, dict) or not isinstance(schema, dict):
        return
    # Both shapes accept arbitrary keys by design.
    if schema.get("x-kubernetes-preserve-unknown-fields"):
        return
    props = schema.get("properties")
    if props is None:
        return
    for key, value in obj.items():
        if not isinstance(key, str) or not FIELD_NAME.match(key):
            continue  # Chainsaw assertion key, not a CR field.
        if key not in props:
            findings.append(f"{path}.{key}")
            continue
        sub = props[key]
        if isinstance(value, dict):
            walk(sub, value, f"{path}.{key}", findings)
        elif isinstance(value, list):
            items = sub.get("items", {})
            for index, element in enumerate(value):
                if isinstance(element, dict):
                    walk(items, element, f"{path}.{key}[{index}]", findings)


def fixture_documents():
    """Yield (relative path, doc index, document) for every parsable fixture doc."""
    unparsable = []
    for path in sorted((REPO_ROOT / TEST_ROOT).rglob("*.yaml")):
        rel = path.relative_to(REPO_ROOT)
        try:
            documents = list(yaml.safe_load_all(path.read_text()))
        except yaml.YAMLError:
            unparsable.append(rel)
            continue
        for index, document in enumerate(documents):
            if isinstance(document, dict):
                yield rel, index, document
    for rel in unparsable:
        emit("INFO", f"{rel}: not parsable as YAML — skipped (Chainsaw template?)")


def main() -> int:
    crds, served, repo_groups = load_crds()
    if not crds:
        emit("FAIL", "no CRDs under operators/*/config/crd/bases/ — run: make manifests")
        return 1

    documents = list(fixture_documents())
    in_scope = []
    # Documents in a *.c5c3.io group no repo CRD declares: per group, the
    # documents (for the typo FAIL) — external groups only get counted.
    undeclared = {}
    for rel, index, doc in documents:
        group = api_group(doc.get("apiVersion"))
        if group in repo_groups:
            in_scope.append((rel, index, doc))
        elif group == "c5c3.io" or group.endswith(".c5c3.io"):
            undeclared.setdefault(group, []).append((rel, index, doc))

    suites = sorted({rel.parts[1] for rel, _, _ in in_scope if len(rel.parts) > 1})
    emit(
        "INFO",
        f"{len(crds)} CRD version(s) across {len(served)} kind(s) in "
        f"{len(repo_groups)} API group(s); "
        f"{len(in_scope)} CobaltCore CR document(s) under {TEST_ROOT}/",
    )
    emit("INFO", f"suite roots carrying CobaltCore CRs: {', '.join(suites)}")

    # ---------------------------------------------------------------------
    header("X1: every CobaltCore fixture names a known kind on a served apiVersion")
    unknown = 0
    for group in sorted(undeclared):
        entries = undeclared[group]
        if looks_like_repo_group(group, repo_groups):
            for rel, index, doc in entries:
                where = f"{rel}" if index == 0 else f"{rel} (document {index + 1})"
                emit(
                    "FAIL",
                    f"{where}: API group '{group}' is declared by no CRD under "
                    "operators/*/config/crd/bases/ — a typo of a repo group, "
                    "or a CRD make manifests has not generated",
                )
                unknown += 1
        else:
            kinds = ", ".join(sorted({str(doc.get("kind")) for _, _, doc in entries}))
            files = len({rel for rel, _, _ in entries})
            emit(
                "INFO",
                f"{group}: {len(entries)} document(s) in {files} file(s) ({kinds}) — "
                "external operator's CRD, not a repo CRD; X1/X2 skip it",
            )
    per_kind = {}
    for rel, index, doc in in_scope:
        kind = doc.get("kind")
        api_version = doc["apiVersion"]
        where = f"{rel}" if index == 0 else f"{rel} (document {index + 1})"
        if kind not in served:
            emit("FAIL", f"{where}: kind '{kind}' matches no CRD under operators/*/config/crd/bases/")
            unknown += 1
        elif api_version not in served[kind]:
            expected = ", ".join(sorted(served[kind]))
            emit("FAIL", f"{where}: apiVersion '{api_version}' is not served for kind {kind} (served: {expected})")
            unknown += 1
        else:
            per_kind.setdefault(kind, []).append(rel)
    for kind in sorted(per_kind):
        files = {str(f) for f in per_kind[kind]}
        emit("INFO", f"{kind}: {len(per_kind[kind])} document(s) in {len(files)} file(s)")
    if unknown == 0:
        emit("PASS", f"all {len(in_scope)} CobaltCore CR document(s) use a known kind on a served apiVersion")

    # ---------------------------------------------------------------------
    header("X2: every spec field in a CobaltCore fixture exists in the CRD schema")
    checked = 0
    offenders = 0
    for rel, index, doc in in_scope:
        key = (doc.get("apiVersion"), doc.get("kind"))
        if key not in crds:
            continue  # already reported by X1
        crd_path, spec_schema = crds[key]
        spec = doc.get("spec")
        if not isinstance(spec, dict):
            continue
        checked += 1
        findings = []
        walk(spec_schema, spec, "spec", findings)
        where = f"{rel}" if index == 0 else f"{rel} (document {index + 1})"
        for field in findings:
            emit("FAIL", f"{where}: {doc['kind']} field '{field}' is not in {crd_path}")
        if findings:
            offenders += 1
    if offenders == 0:
        emit("PASS", f"all {checked} document(s) with a spec block validate against their CRD schema")

    return 1 if fail_count else 0


if __name__ == "__main__":
    sys.exit(main())
