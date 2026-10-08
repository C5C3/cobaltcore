#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Generate each operator chart's RBAC rules templates from controller-gen output.

The kubebuilder RBAC markers in an operator's Go sources are the single source
of truth for what its ServiceAccount may do. `make manifests` has controller-gen
render them to operators/<op>/config/rbac/role.yaml, one ClusterRole per
roleName the markers carry. This script writes two named templates per chart
from that file:

  templates/_rbac-rules.tpl          "<op>-operator.rbacRules", the rules of the
                                     <op>-operator ClusterRole (the manager)
  templates/_webhook-rbac-rules.tpl  "<op>-operator.webhookRbacRules", the rules
                                     of the <op>-webhook ClusterRole (what the
                                     admission webhooks read)

The operator-library ClusterRole and Role templates include the first; the
ClusterRole includes the second instead under webhook.standalone. One rule is
appended to the first that no marker carries: leases in coordination.k8s.io,
which the manager's leader election needs. The second gets no leases rule,
because a standalone webhook elects no leader.

A webhook read is declared with a marker ending in roleName=<op>-webhook beside
the webhook that performs it. Before any file is written, and under --check, a
coverage guard checks that every (apiGroup, resource, verb) the <op>-webhook
ClusterRole grants is granted by the <op>-operator ClusterRole as well (a
manager verb "*" covers every verb): the in-process webhook of a cluster-wide
operator runs under the manager identity, so a read declared for the webhook
role alone would fail there.

The rationale for individual grants lives as comments next to the markers; the
generated templates carry none, so edit the markers, not this output.

Charts are discovered from the repository layout (operators/<op>/helm/<op>-operator)
under --repo-root, the repository this script lives in by default. Run via the
Makefile:

  make sync-helm-rbac    # regenerate every chart's RBAC rules templates
  make verify-helm-rbac  # exit non-zero if any committed file is stale
"""

import argparse
import sys
from pathlib import Path

import yaml

REPO_ROOT = Path(__file__).resolve().parent.parent

LEADER_ELECTION_RULE = {
    "apiGroups": ["coordination.k8s.io"],
    "resources": ["leases"],
    "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"],
}


def discover_charts(repo_root: Path) -> list[tuple[str, Path]]:
    """Return (operator, chart directory) pairs for every operator chart."""
    charts = []
    for chart_dir in sorted(repo_root.glob("operators/*/helm/*-operator")):
        op = chart_dir.parent.parent.name
        if chart_dir.name != f"{op}-operator":
            sys.exit(f"error: {chart_dir} does not follow the operators/<op>/helm/<op>-operator layout")
        charts.append((op, chart_dir))
    return charts


def role_source(op: str) -> Path:
    """Return the repository-relative path of the operator's role.yaml."""
    return Path("operators") / op / "config" / "rbac" / "role.yaml"


def load_roles(op: str, repo_root: Path) -> tuple[list[dict], list[dict]]:
    """Return the rules of the <op>-operator and <op>-webhook ClusterRoles.

    controller-gen writes no document for a role without rules, so a missing
    document exits with a message naming the role and how to produce it.
    """
    source = role_source(op)
    path = repo_root / source
    if not path.exists():
        sys.exit(f"error: {source} missing; run 'make manifests' first")
    roles = {
        doc["metadata"]["name"]: doc
        for doc in yaml.safe_load_all(path.read_text(encoding="utf-8"))
        if doc and doc.get("kind") == "ClusterRole"
    }
    manager = roles.get(f"{op}-operator")
    if manager is None:
        sys.exit(f"error: {source} has no ClusterRole named {op}-operator; run 'make manifests' first")
    webhook = roles.get(f"{op}-webhook")
    if webhook is None:
        sys.exit(
            f"error: {source} has no ClusterRole named {op}-webhook; add a roleName={op}-webhook "
            "marker beside the webhook that performs the read"
        )
    return list(manager.get("rules") or []), list(webhook.get("rules") or [])


def grants(rules: list[dict]) -> list[tuple[str, str, str]]:
    """Return every (apiGroup, resource, verb) triple the rules grant, in order."""
    return [
        (group, resource, verb)
        for rule in rules
        for group in rule.get("apiGroups") or []
        for resource in rule.get("resources") or []
        for verb in rule.get("verbs") or []
    ]


def check_coverage(op: str, manager_rules: list[dict], webhook_rules: list[dict]) -> None:
    """Exit 1 if <op>-webhook grants a triple that <op>-operator does not."""
    granted = set(grants(manager_rules))
    for group, resource, verb in grants(webhook_rules):
        if (group, resource, verb) not in granted and (group, resource, "*") not in granted:
            sys.exit(
                f'error: {op}-webhook grants {verb} on {resource} in apiGroup "{group}" that '
                f"{op}-operator does not; the in-process webhook of the operator runs under the "
                "manager identity, so declare the read for the manager too"
            )


def dump_rules(rules: list[dict]) -> str:
    """Return the rules as the YAML list a named template body holds."""
    return yaml.safe_dump(rules, sort_keys=False, default_flow_style=False, width=1000)


def render(op: str, rules: list[dict]) -> str:
    """Return the exact text of the chart's templates/_rbac-rules.tpl."""
    source = role_source(op)
    return (
        "{{/*\n"
        f"SOURCE: {source}\n"
        "GENERATED by hack/gen-helm-rbac-rules.py from the kubebuilder RBAC markers in\n"
        f"operators/{op}; do not edit. The leases rule is appended for leader election.\n"
        "Run 'make verify-helm-rbac' to check for drift, or 'make sync-helm-rbac' to update.\n"
        "*/}}\n"
        f'{{{{- define "{op}-operator.rbacRules" -}}}}\n'
        f"{dump_rules(rules + [LEADER_ELECTION_RULE])}"
        "{{- end }}\n"
    )


def render_webhook(op: str, rules: list[dict]) -> str:
    """Return the exact text of the chart's templates/_webhook-rbac-rules.tpl."""
    source = role_source(op)
    return (
        "{{/*\n"
        f"SOURCE: {source} (ClusterRole {op}-webhook)\n"
        "GENERATED by hack/gen-helm-rbac-rules.py from the kubebuilder RBAC markers in\n"
        f"operators/{op} that carry roleName={op}-webhook; do not edit.\n"
        "No leases rule: a standalone webhook elects no leader.\n"
        "Run 'make verify-helm-rbac' to check for drift, or 'make sync-helm-rbac' to update.\n"
        "*/}}\n"
        f'{{{{- define "{op}-operator.webhookRbacRules" -}}}}\n'
        f"{dump_rules(rules)}"
        "{{- end }}\n"
    )


def main() -> int:
    """Write or check the two RBAC rules templates of every operator chart."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="exit non-zero (without writing) if any committed file is stale",
    )
    parser.add_argument(
        "--repo-root",
        type=Path,
        default=REPO_ROOT,
        help="repository root the charts and role.yaml files are read from (default: %(default)s)",
    )
    args = parser.parse_args()
    repo_root = args.repo_root.resolve()

    # Every role is loaded and guarded before the first file is written, so a
    # failing chart leaves no other chart half-updated.
    outputs = []
    for op, chart_dir in discover_charts(repo_root):
        manager_rules, webhook_rules = load_roles(op, repo_root)
        check_coverage(op, manager_rules, webhook_rules)
        templates = chart_dir / "templates"
        outputs.append((op, templates / "_rbac-rules.tpl", render(op, manager_rules)))
        outputs.append((op, templates / "_webhook-rbac-rules.tpl", render_webhook(op, webhook_rules)))

    drift = False
    for op, path, want in outputs:
        rel = path.relative_to(repo_root)
        if args.check:
            have = path.read_text(encoding="utf-8") if path.exists() else ""
            if have != want:
                drift = True
                print(f"DRIFT: {rel} is out of sync with {role_source(op)}")
        else:
            path.write_text(want, encoding="utf-8")
            print(f"wrote {rel}")

    if args.check and drift:
        print("Helm RBAC rules drift detected. Run 'make sync-helm-rbac'.")
        return 1
    if args.check:
        print("Helm RBAC rules check passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
