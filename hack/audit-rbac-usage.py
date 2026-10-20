#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Diff the RBAC an operator's ServiceAccount used against what its markers grant.

The kube-apiserver of the audited kind cluster (hack/kind-config-audit.yaml)
logs every ServiceAccount request at Metadata level (hack/audit-policy.yaml).
This script reads that log and, per operator, compares the
(apiGroup, resource[/subresource], verb) triples the operator's ServiceAccount
issued with the triples of the <op>-operator ClusterRole that `make manifests`
renders from the kubebuilder RBAC markers into
operators/<op>/config/rbac/role.yaml, plus the leases rule
hack/gen-helm-rbac-rules.py appends for leader election.

The ServiceAccount is <op>-operator in <op>-system, which is what
hack/ci-deploy-operator.sh installs. An event counts when its user.username is
system:serviceaccount:<namespace>:<op>-operator and it carries an objectRef;
discovery requests (/api, /apis) carry none. A line that is not JSON, or an
event without a verb, is counted as skipped.

Three mapping rules cover verbs a request exercises without logging them as
its verb:

  - A patch or update answered with 201 created the object, and the API server
    authorizes that creation as create, so the event uses create as well. A
    Server-Side Apply of an absent object is the common case.
  - A watch with sendInitialEvents=true is an informer's initial list, which
    client-go streams over a watch where the API server supports it and issues
    as a plain list where it does not, so the event uses list as well.
  - An unexercised get is not reported while list on the same kind is granted:
    list already returns every object get could read. A cached client reads
    through an informer, which issues list and watch, never get. A get on the
    kind's status subresource reads the same object, so list on the kind
    covers it too.

The report has three sections per operator:

  USED, NOT GRANTED  used triples no rule covers; expected empty. A row here is
                     a grant another binding supplies (the authorization reason
                     is printed) or a mapping this script lacks.
  UNUSED             per rule and per (apiGroup, resource) of it, the granted
                     verbs no event used. A rule whose verbs are "*" reports
                     nothing.
  GET IMPLIED BY LIST  the unexercised get verbs the second mapping rule keeps
                     out of UNUSED.

--json writes the same per operator:
  {"<op>": {"used": [...], "unused": [...], "used_not_granted": [...],
            "get_implied_by_list": [...], "events": n, "skipped_lines": n}}

Exit codes: 0 after a report, also when it has findings; 1 under
--fail-on-unused when any UNUSED row exists; 2 when an audit log cannot be
read, no event carries an operator's username, or role.yaml lacks the
<op>-operator ClusterRole.

The CI jobs run it through hack/ci-rbac-usage-report.sh; see
docs/reference/testing/rbac-usage-audit.md.
"""

import argparse
import importlib.util
import json
import sys
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import yaml

REPO_ROOT = Path(__file__).resolve().parent.parent

EXIT_UNUSED = 1
EXIT_INPUT = 2


def load_generator():
    """Import hack/gen-helm-rbac-rules.py, the source of the chart layout and the leases rule."""
    path = Path(__file__).resolve().parent / "gen-helm-rbac-rules.py"
    spec = importlib.util.spec_from_file_location("gen_helm_rbac_rules", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


GENERATOR = load_generator()


def fail(message: str) -> None:
    """Exit with the input-error code."""
    print(f"error: {message}", file=sys.stderr)
    sys.exit(EXIT_INPUT)


def load_manager_rules(op: str, repo_root: Path) -> list[dict]:
    """Return the <op>-operator ClusterRole rules plus the appended leases rule."""
    source = GENERATOR.role_source(op)
    path = repo_root / source
    try:
        docs = list(yaml.safe_load_all(path.read_text(encoding="utf-8")))
    except OSError as err:
        fail(f"cannot read {source}: {err.strerror}")
    for doc in docs:
        if doc and doc.get("kind") == "ClusterRole" and doc.get("metadata", {}).get("name") == f"{op}-operator":
            return list(doc.get("rules") or []) + [GENERATOR.LEADER_ELECTION_RULE]
    fail(f"{source} has no ClusterRole named {op}-operator; run 'make manifests' first")
    return []


def resource_matches(pattern: str, resource: str) -> bool:
    """Match a rule resource against resource[/subresource] the way RBAC does."""
    if pattern in ("*", resource):
        return True
    _, _, subresource = resource.partition("/")
    return bool(subresource) and pattern == f"*/{subresource}"


def rule_covers(rule: dict, group: str, resource: str, verb: str, name: str = "") -> bool:
    """Return whether rule grants verb on group/resource (and name, under resourceNames)."""
    if not any(g in ("*", group) for g in rule.get("apiGroups") or []):
        return False
    if not any(resource_matches(r, resource) for r in rule.get("resources") or []):
        return False
    if not any(v in ("*", verb) for v in rule.get("verbs") or []):
        return False
    names = rule.get("resourceNames") or []
    return not names or name in names


def read_events(paths: list[Path], usernames: dict[str, str]) -> tuple[dict, dict, int]:
    """Read the audit logs and return the used triples and the event count per operator.

    usernames maps each ServiceAccount username to its operator. The used triples
    of an operator map (apiGroup, resource, verb) to the object name and the
    authorization reason of the first event that used it.
    """
    used = {op: {} for op in usernames.values()}
    events = {op: 0 for op in usernames.values()}
    skipped = 0
    for path in paths:
        try:
            handle = path.open(encoding="utf-8", errors="replace")
        except OSError as err:
            fail(f"cannot read audit log {path}: {err.strerror}")
        with handle:
            for line in handle:
                if not line.strip():
                    continue
                try:
                    event = json.loads(line)
                except json.JSONDecodeError:
                    skipped += 1
                    continue
                if not isinstance(event, dict) or not isinstance(event.get("verb"), str):
                    skipped += 1
                    continue
                op = usernames.get((event.get("user") or {}).get("username"))
                if op is None:
                    continue
                events[op] += 1
                ref = event.get("objectRef")
                if not isinstance(ref, dict) or not ref.get("resource"):
                    continue
                group = ref.get("apiGroup") or ""
                resource = ref["resource"] + (f"/{ref['subresource']}" if ref.get("subresource") else "")
                verbs = [event["verb"]]
                code = (event.get("responseStatus") or {}).get("code")
                if event["verb"] in ("patch", "update") and code == 201:
                    verbs.append("create")
                query = parse_qs(urlsplit(event.get("requestURI") or "").query)
                if event["verb"] == "watch" and query.get("sendInitialEvents") == ["true"]:
                    verbs.append("list")
                reason = (event.get("annotations") or {}).get("authorization.k8s.io/reason", "")
                for verb in verbs:
                    used[op].setdefault((group, resource, verb), (ref.get("name") or "", reason))
    return used, events, skipped


def diff(rules: list[dict], used: dict) -> dict:
    """Return the used, unused, used-not-granted and implied-get findings of one operator."""
    not_granted = [
        {"apiGroup": g, "resource": r, "verb": v, "reason": reason}
        for (g, r, v), (name, reason) in sorted(used.items())
        if not any(rule_covers(rule, g, r, v, name) for rule in rules)
    ]
    unused, implied = [], []
    for rule in rules:
        verbs = rule.get("verbs") or []
        if "*" in verbs:
            continue
        for group in rule.get("apiGroups") or []:
            for resource in rule.get("resources") or []:
                probe = {"apiGroups": [group], "resources": [resource], "verbs": []}
                idle = []
                for verb in verbs:
                    probe["verbs"] = [verb]
                    if not any(rule_covers(probe, g, r, v) for (g, r, v) in used if v == verb):
                        idle.append(verb)
                kind = resource.removesuffix("/status")
                if "get" in idle and any(rule_covers(other, group, kind, "list") for other in rules):
                    idle.remove("get")
                    implied.append({"apiGroup": group, "resource": resource})
                if idle:
                    unused.append({"apiGroup": group, "resource": resource, "verbs": idle})
    return {
        "used": [{"apiGroup": g, "resource": r, "verb": v} for (g, r, v) in sorted(used)],
        "unused": unused,
        "used_not_granted": not_granted,
        "get_implied_by_list": implied,
    }


def label(group: str, resource: str) -> str:
    """Return the core-group-aware display name of a kind."""
    return f"{resource}.{group}" if group else resource


def print_report(op: str, username: str, result: dict) -> None:
    """Print the three sections and the counts of one operator."""
    print(f"== {op}-operator ({username})")
    print(f"USED, NOT GRANTED ({len(result['used_not_granted'])})")
    for row in result["used_not_granted"]:
        reason = f"  [{row['reason']}]" if row["reason"] else ""
        print(f"  {label(row['apiGroup'], row['resource'])}: {row['verb']}{reason}")
    print(f"UNUSED ({len(result['unused'])})")
    for row in result["unused"]:
        print(f"  {label(row['apiGroup'], row['resource'])}: {', '.join(row['verbs'])}")
    print(f"GET IMPLIED BY LIST ({len(result['get_implied_by_list'])})")
    for row in result["get_implied_by_list"]:
        print(f"  {label(row['apiGroup'], row['resource'])}")
    unused_verbs = sum(len(row["verbs"]) for row in result["unused"])
    print(
        f"events: {result['events']}, skipped lines: {result['skipped_lines']}, "
        f"used: {len(result['used'])}, unused: {unused_verbs} verbs in {len(result['unused'])} rows, "
        f"used-not-granted: {len(result['used_not_granted'])}"
    )
    print()


def main() -> int:
    """Diff every requested operator and report; see the module docstring for exit codes."""
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--audit-log", action="append", type=Path, required=True, help="audit log file (repeatable)")
    parser.add_argument(
        "--operator",
        action="append",
        help="operator to audit (repeatable; default every chart hack/gen-helm-rbac-rules.py discovers)",
    )
    parser.add_argument("--namespace", help="namespace of the ServiceAccount (default <op>-system)")
    parser.add_argument(
        "--repo-root",
        type=Path,
        default=REPO_ROOT,
        help="repository root role.yaml files are read from (default: %(default)s)",
    )
    parser.add_argument("--json", type=Path, help="also write the findings as JSON to this file")
    parser.add_argument("--fail-on-unused", action="store_true", help="exit 1 when any UNUSED row exists")
    args = parser.parse_args()
    repo_root = args.repo_root.resolve()

    operators = args.operator or [op for op, _ in GENERATOR.discover_charts(repo_root)]
    rules = {op: load_manager_rules(op, repo_root) for op in operators}
    usernames = {
        f"system:serviceaccount:{args.namespace or f'{op}-system'}:{op}-operator": op for op in operators
    }
    used, events, skipped = read_events(args.audit_log, usernames)

    results = {}
    for username, op in usernames.items():
        results[op] = diff(rules[op], used[op]) | {"events": events[op], "skipped_lines": skipped}
        # Without a single event every grant would read as unused; the error
        # below says why instead.
        if events[op]:
            print_report(op, username, results[op])

    if args.json:
        args.json.write_text(json.dumps(results, indent=2) + "\n", encoding="utf-8")

    silent = [op for op in operators if events[op] == 0]
    if silent:
        sys.stdout.flush()
        for op in silent:
            print(f"error: no audit event carries the username of {op}-operator", file=sys.stderr)
        return EXIT_INPUT
    if args.fail_on_unused and any(result["unused"] for result in results.values()):
        return EXIT_UNUSED
    return 0


if __name__ == "__main__":
    sys.exit(main())
