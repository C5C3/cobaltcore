#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Derive the sizing figures from the recommendations of a VPA run.

A `ci:measure-sizing` run uploads one sizing-* artifact per job, each holding
the recommendations.tsv that hack/ci-vpa-recommendations.sh writes. This
script reads every recommendations.tsv under a directory, applies the fixed
rules of docs/reference/testing/sizing-calibration.md, and prints:

  1. one key=value line per figure, keyed by the constant it replaces
     (memoryBase, defaultMemoryPerProcess, glanceMemoryPerProcess,
     defaultCPURequest, minimalServiceCPURequest, minimalDatabase,
     minimalCache, minimalMessaging, minimalSecretStore, sidecarCPURequest,
     sidecarMemory; the four minimal* keys as <cpu>,<memory>);
  2. the projected node budget of the Minimal ControlPlane;
  3. a Markdown section with every row the figures use, for the reference
     page.

Usage:

  python3 hack/derive-sizing-figures.py --budget-total <cpu>m,<memory>Mi <dir>

--budget-total is the TOTAL row of the node budget table (Link 6z) that the
e2e-controlplane job of the same run printed.

Exit codes: 0 derived; 1 incomplete input or a budget miss; 2 usage or parse
error. Only the standard library is used.
"""

import argparse
import re
import sys
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path

PROG = "derive-sizing-figures"

COLUMNS = [
    "namespace", "kind", "workload", "replicas", "owner_kind", "owner_name",
    "app", "component", "container", "processes", "threads", "cpu_target_m",
    "memory_target_mi", "cpu_upper_m", "memory_upper_mi", "cpu_request_m",
    "memory_request_mi", "memory_limit_mi", "snapshot",
]
# Columns that hold a whole number or "-"; the two targets must be numbers.
OPTIONAL_NUMBERS = {
    "replicas", "processes", "threads", "cpu_upper_m", "memory_upper_mi",
    "cpu_request_m", "memory_request_mi", "memory_limit_mi",
}
REQUIRED_NUMBERS = {"cpu_target_m", "memory_target_mi"}

CONTROLPLANE_JOBS = ("e2e-controlplane", "e2e-controlplane-sso")
TEMPEST_JOB = "tempest"
JOBS = CONTROLPLANE_JOBS + (TEMPEST_JOB,)

FORMULA_OWNERS = {
    "Keystone", "Horizon", "Placement", "Barbican", "Neutron", "Cinder",
    "Nova", "NeutronMetadataAgent",
}
FORMULA_OVN_COMPONENTS = {"northd", "sb-relay"}
SIDECARS = {"federation-proxy", "cache-maintenance"}
# Containers whose memory the operator fixes in place of the formula, as
# (owner_kind, component). cinder-backup's backupMemory follows the backup
# chunk size, not the process count (operators/cinder/internal/controller/
# reconcile_backupservice.go). The metadata agent's metadataAgentMemory
# follows the networks on its node (operators/neutron/internal/controller/
# reconcile_daemonset.go). Their CPU counts like any service's; their
# memory stays out of the fit and keeps its figure in the projection.
FIXED_MEMORY = {("Cinder", "backup"), ("NeutronMetadataAgent", "metadata-agent")}
# The workloads the ControlPlane does not size, so the Minimal profile does
# not reach them.
NOT_PROFILED_OWNERS = {"NeutronMetadataAgent", "OVNCentral"}

# memoryPerExtraThread stays 32Mi (internal/common/types/resources.go).
MEMORY_PER_EXTRA_THREAD = 32
# Nova scheduler/conductor and eventlet Glance take their process count from
# a config option, not from the command line: Minimal sets one worker, the
# operators default to two.
WORKERS_BY_JOB = {"e2e-controlplane": 1, "e2e-controlplane-sso": 1, TEMPEST_JOB: 2}

# kind -> (key, memory floor in MiB). The floors are figures the backing
# service's own configuration depends on.
BACKING = {
    "MariaDB": ("minimalDatabase", 1024),
    "Memcached": ("minimalCache", 96),
    "RabbitmqCluster": ("minimalMessaging", 512),
    "OpenBaoCluster": ("minimalSecretStore", 64),
}
# The shared kind broker keeps its overlay figure on the measured cluster, so
# the projection leaves it alone.
PROJECTED_BACKING = ("MariaDB", "Memcached", "OpenBaoCluster")

BUDGET_CPU_M = 4000
BUDGET_MEMORY_MI = 16384


class Incomplete(Exception):
    """The input lacks rows a figure needs (exit 1)."""


class ParseFailure(Exception):
    """The command line or an input file cannot be parsed (exit 2)."""


@dataclass
class Row:
    """One container row of a recommendations.tsv, with its classification."""

    file: str
    line: int
    job: str
    leg: str
    values: dict
    cls: str = "ignored"
    p: int = 1
    t: int = 1

    def __getattr__(self, name):
        try:
            return self.__dict__["values"][name]
        except KeyError:
            raise AttributeError(name) from None

    @property
    def key(self) -> str:
        return f"{self.owner_kind}/{self.component}/{self.container}"

    @property
    def fixed_memory(self) -> bool:
        """The operator gives this container a fixed memory figure."""
        return (self.owner_kind, self.component) in FIXED_MEMORY

    @property
    def normalized_memory(self) -> int:
        """T: the memory target without the extra threads of each process."""
        return self.memory_target_mi - self.p * (self.t - 1) * MEMORY_PER_EXTRA_THREAD

    @property
    def pods(self) -> int:
        # A DaemonSet runs one pod on the single kind node.
        return 1 if self.replicas is None else self.replicas


def round_up(x, step: int) -> int:
    """Round x up to a multiple of step."""
    return int(-(-Fraction(x) // step) * step)


def upper_median(values: list) -> int:
    """The middle value; with an even count, the upper of the two middle ones."""
    ordered = sorted(values)
    return ordered[len(ordered) // 2]


def fmt_memory(mi: int) -> str:
    return f"{mi // 1024}Gi" if mi % 1024 == 0 else f"{mi}Mi"


def parse_budget(text: str | None) -> tuple:
    if text is None:
        raise ParseFailure("cannot parse --budget-total: the option is required "
                           "(--budget-total <cpu>m,<memory>Mi)")
    m = re.fullmatch(r"(\d+)m,(\d+)Mi", text)
    if not m:
        raise ParseFailure(f"cannot parse --budget-total: {text!r} is not <cpu>m,<memory>Mi")
    return int(m.group(1)), int(m.group(2))


def parse_file(path: Path) -> list:
    """Read one recommendations.tsv into rows."""
    lines = path.read_text().splitlines()
    if not lines or not lines[0].startswith("# "):
        raise ParseFailure(f"cannot parse {path}:1: the comment line (# run=... job=...) is missing")
    meta = dict(tok.split("=", 1) for tok in lines[0][2:].split() if "=" in tok)
    job = meta.get("job", "")
    if job not in JOBS:
        raise ParseFailure(f"cannot parse {path}:1: job={job or '<missing>'} is none of {', '.join(JOBS)}")
    if len(lines) < 2 or lines[1].split("\t") != COLUMNS:
        raise ParseFailure(f"cannot parse {path}:2: the header row does not list the {len(COLUMNS)} columns")

    rows = []
    for number, text in enumerate(lines[2:], start=3):
        if not text.strip():
            continue
        fields = text.split("\t")
        if len(fields) != len(COLUMNS):
            raise ParseFailure(f"cannot parse {path}:{number}: {len(fields)} columns, want {len(COLUMNS)}")
        values = dict(zip(COLUMNS, fields))
        for col in REQUIRED_NUMBERS | OPTIONAL_NUMBERS:
            raw = values[col]
            if raw == "-" and col in OPTIONAL_NUMBERS:
                values[col] = None
            elif re.fullmatch(r"\d+", raw):
                values[col] = int(raw)
            else:
                raise ParseFailure(f"cannot parse {path}:{number}: {col} {raw!r} is not a whole number")
        rows.append(Row(file=str(path), line=number, job=job, leg=meta.get("leg", "-"), values=values))
    return rows


def classify(row: Row) -> None:
    """Rules 1 and 2: the row's class and its process and thread counts."""
    if row.container in SIDECARS:
        row.cls = "sidecar"
    elif row.owner_kind == "Glance":
        row.cls = "glance"
    elif row.owner_kind in FORMULA_OWNERS or (
            row.owner_kind == "OVNCentral" and row.component in FORMULA_OVN_COMPONENTS):
        row.cls = "formula"
    elif row.owner_kind in BACKING and row.job == "e2e-controlplane":
        row.cls = "backing"
    else:
        row.cls = "ignored"

    if row.cls not in ("formula", "glance"):
        return
    row.t = row.threads or 1
    if row.processes is not None:
        row.p = row.processes
    elif row.cls == "glance" or (row.owner_kind == "Nova" and row.component in ("scheduler", "conductor")):
        row.p = WORKERS_BY_JOB[row.job]
    else:
        row.p = 1


def is_profiled_service(row: Row) -> bool:
    """A service row the Minimal profile sizes (rule 6)."""
    return (row.job in CONTROLPLANE_JOBS and row.cls in ("formula", "glance")
            and row.owner_kind not in NOT_PROFILED_OWNERS)


def largest_per_key(rows: list) -> dict:
    """The largest CPU target per owner_kind/component/container key."""
    best = {}
    for row in rows:
        best[row.key] = max(best.get(row.key, 0), row.cpu_target_m)
    return best


def md_table(header: list, body: list) -> list:
    out = ["| " + " | ".join(header) + " |", "|" + "|".join(" --- " for _ in header) + "|"]
    out += ["| " + " | ".join(str(c) for c in r) + " |" for r in body]
    return out


def derive(rows: list, budget: tuple) -> tuple:
    """Apply rules 3 to 9. Returns (figures, projection, markdown lines, error)."""
    for row in rows:
        classify(row)
    formula = [r for r in rows if r.cls == "formula" and not r.fixed_memory]
    fixed = sorted({r.key for r in rows if r.cls == "formula" and r.fixed_memory})
    glance = [r for r in rows if r.cls == "glance"]

    # Rule 3: the shared memory formula.
    m_of_p = {}
    for row in formula:
        m_of_p[row.p] = max(m_of_p.get(row.p, row.normalized_memory), row.normalized_memory)
    counts = sorted(m_of_p)
    if len(counts) < 2:
        raise Incomplete("the formula needs rows at two process counts, found "
                         + (", ".join(map(str, counts)) or "none"))
    slope = max(Fraction(m_of_p[pj] - m_of_p[pi], pj - pi)
                for i, pi in enumerate(counts) for pj in counts[i + 1:])
    per_process = round_up(max(32, slope), 16)
    base = round_up(max(0, max(m_of_p[p] - p * per_process for p in counts)), 16)

    # Rule 4: Glance keeps its own per-process figure.
    if not glance:
        raise Incomplete("no Glance row was measured")
    glance_per_process = round_up(
        max(32, max(Fraction(r.normalized_memory - base, r.p) for r in glance)), 16)

    # Rule 5: the render-time CPU request, from the API load of the tempest legs.
    cpu_rows = [r for r in rows if r.job == TEMPEST_JOB and (
        r.cls in ("formula", "glance")
        or (r.owner_kind == "OVNCentral" and r.component in ("nb", "sb")))]
    if not cpu_rows:
        raise Incomplete("no tempest row was measured for the render-time CPU request")
    cpu_keys = largest_per_key(cpu_rows)
    cpu_median = upper_median(list(cpu_keys.values()))
    default_cpu = max(10, round_up(cpu_median, 10))

    # Rule 6: the Minimal service CPU request.
    profiled = [r for r in rows if is_profiled_service(r)]
    if not profiled:
        raise Incomplete("no e2e-controlplane row was measured for the Minimal CPU request")
    minimal_keys = largest_per_key(profiled)
    minimal_median = upper_median(list(minimal_keys.values()))
    minimal_cpu = max(10, round_up(minimal_median, 5))

    # Rule 7: the Minimal backing services. Per workload the main container is
    # the one with the largest memory target; a sidecar beside it keeps its own.
    main_rows = {}
    for row in (r for r in rows if r.cls == "backing"):
        wl = (row.owner_kind, row.namespace, row.kind, row.workload)
        cur = main_rows.get(wl)
        if cur is None or (row.memory_target_mi, row.cpu_target_m) > (cur.memory_target_mi, cur.cpu_target_m):
            main_rows[wl] = row
    backing = {}
    for kind, (_, floor) in BACKING.items():
        mains = [r for (k, *_), r in sorted(main_rows.items()) if k == kind]
        if not mains:
            raise Incomplete(f"no {kind} row was measured in e2e-controlplane")
        backing[kind] = (
            max(10, round_up(max(r.cpu_target_m for r in mains), 5)),
            max(floor, round_up(max(r.memory_target_mi for r in mains), 16)),
            mains,
        )

    # Rule 8: the shared sidecar figure only rises.
    proxies = [r for r in rows if r.container == "federation-proxy"]
    if not proxies:
        raise Incomplete("no federation-proxy row was measured (did e2e-controlplane-sso run?)")
    sidecar_cpu = max(25, round_up(max(r.cpu_target_m for r in proxies), 5))
    sidecar_memory = max(256, round_up(max(r.memory_target_mi for r in proxies), 16))

    # Rule 9: project the Minimal figures onto the measured e2e-controlplane node.
    budget_cpu, budget_memory = budget
    projected_rows = [r for r in rows if r.job == "e2e-controlplane" and is_profiled_service(r)]
    projected_backing = [r for kind in PROJECTED_BACKING for r in backing[kind][2]]

    def service_memory(row: Row):
        if row.fixed_memory or row.memory_limit_mi is None or row.memory_limit_mi != row.memory_request_mi:
            return row.memory_request_mi  # a fixed or hand-set figure, not the formula
        per = glance_per_process if row.cls == "glance" else per_process
        return base + row.p * (per + (row.t - 1) * MEMORY_PER_EXTRA_THREAD)

    def project(service_cpu: int) -> tuple:
        cpu, memory = budget_cpu, budget_memory
        for row in projected_rows:
            cpu += (service_cpu - (row.cpu_request_m or 0)) * row.pods
            memory += ((service_memory(row) or 0) - (row.memory_request_mi or 0)) * row.pods
        for row in projected_backing:
            fig_cpu, fig_memory, _ = backing[row.owner_kind]
            cpu += (fig_cpu - (row.cpu_request_m or 0)) * row.pods
            memory += (fig_memory - (row.memory_request_mi or 0)) * row.pods
        return cpu, memory

    steps = [(minimal_cpu, *project(minimal_cpu))]
    error = None
    while steps[-1][1] > BUDGET_CPU_M:
        if minimal_cpu <= 10:
            error = "the Minimal figures cannot fit 4000m CPU even at 10m per service container"
            break
        minimal_cpu -= 5
        steps.append((minimal_cpu, *project(minimal_cpu)))
    projected_cpu, projected_memory = steps[-1][1], steps[-1][2]
    if error is None and projected_memory > BUDGET_MEMORY_MI:
        error = (f"memory over budget by {projected_memory - BUDGET_MEMORY_MI}Mi "
                 "with every figure at its measured target")

    figures = [
        ("memoryBase", fmt_memory(base)),
        ("defaultMemoryPerProcess", fmt_memory(per_process)),
        ("glanceMemoryPerProcess", fmt_memory(glance_per_process)),
        ("defaultCPURequest", f"{default_cpu}m"),
        ("minimalServiceCPURequest", f"{minimal_cpu}m"),
    ]
    figures += [(key, f"{backing[kind][0]}m,{fmt_memory(backing[kind][1])}")
                for kind, (key, _) in BACKING.items()]
    figures += [("sidecarCPURequest", f"{sidecar_cpu}m"), ("sidecarMemory", fmt_memory(sidecar_memory))]
    projection = f"projected: {projected_cpu}m {projected_memory}Mi of {BUDGET_CPU_M}m {BUDGET_MEMORY_MI}Mi"

    # The Markdown section.
    md = []
    jobs = sorted({(r.job, r.leg) for r in rows})
    md += ["### Derived figures", ""]
    md += md_table(["Constant", "Value"], [[f"`{k}`", v] for k, v in figures])
    md += ["", f"Input: {len(rows)} rows from {len(jobs)} job runs. "
               "T is the memory target less 32Mi per extra thread of each process.", ""]

    md += ["### Memory formula", ""]
    md += md_table(["p", "M(p), the largest T (MiB)", f"{fmt_memory(base)} + p × {fmt_memory(per_process)}"],
                   [[p, m_of_p[p], base + p * per_process] for p in counts])
    md += ["", f"The steepest slope between two process counts is {float(slope):g}Mi, "
               f"which rounds up to {per_process}Mi."]
    if fixed:
        md += ["", "These containers have a fixed memory figure and stay out of the fit: "
                   + ", ".join(f"`{k}`" for k in fixed) + "."]
    md += [""]

    md += ["### Glance", ""]
    md += md_table(["Job", "Leg", "Workload", "p", "t", "T (MiB)", "(T − memoryBase) / p"],
                   [[r.job, r.leg, r.workload, r.p, r.t, r.normalized_memory,
                     f"{float(Fraction(r.normalized_memory - base, r.p)):g}"]
                    for r in sorted(glance, key=lambda r: (r.job, r.leg, r.workload))])
    md += [""]

    md += ["### Render-time CPU request", ""]
    md += md_table(["Key (owner_kind/component/container)", "Largest CPU target (m)"],
                   [[f"`{k}`", v] for k, v in sorted(cpu_keys.items())])
    md += ["", f"Median of {len(cpu_keys)} keys: {cpu_median}m, so `defaultCPURequest` is {default_cpu}m.", ""]

    md += ["### Minimal service CPU request", ""]
    md += md_table(["Key (owner_kind/component/container)", "Largest CPU target (m)"],
                   [[f"`{k}`", v] for k, v in sorted(minimal_keys.items())])
    md += ["", f"Median of {len(minimal_keys)} keys: {minimal_median}m, "
               f"which rounds up to {max(10, round_up(minimal_median, 5))}m.", ""]

    md += ["### Minimal backing services", ""]
    md += md_table(["Kind", "Workload", "Container", "CPU target (m)", "Memory target (MiB)", "Figure"],
                   [[kind, r.workload, r.container, r.cpu_target_m, r.memory_target_mi,
                     f"{backing[kind][0]}m, {fmt_memory(backing[kind][1])}"]
                    for kind in BACKING for r in backing[kind][2]])
    md += [""]

    md += ["### Sidecar", ""]
    md += md_table(["Job", "Leg", "Workload", "CPU target (m)", "Memory target (MiB)"],
                   [[r.job, r.leg, r.workload, r.cpu_target_m, r.memory_target_mi]
                    for r in sorted(proxies, key=lambda r: (r.job, r.leg, r.workload))])
    md += [""]

    md += ["### Budget projection", "",
           f"The Link 6z total of the measured e2e-controlplane node was {budget_cpu}m CPU and "
           f"{budget_memory}Mi of memory. These e2e-controlplane containers take the Minimal figures:", ""]
    body = []
    for row in sorted(projected_rows + projected_backing, key=lambda r: (r.workload, r.container)):
        if row.cls == "backing":
            new_cpu, new_memory = backing[row.owner_kind][0], backing[row.owner_kind][1]
        else:
            new_cpu, new_memory = minimal_cpu, service_memory(row)
        body.append([row.workload, row.container, row.pods,
                     f"{row.cpu_request_m if row.cpu_request_m is not None else '-'} → {new_cpu}",
                     f"{row.memory_request_mi if row.memory_request_mi is not None else '-'} → "
                     f"{new_memory if new_memory is not None else '-'}"])
    md += md_table(["Workload", "Container", "Pods", "CPU request (m)", "Memory request (MiB)"], body)
    md += [""]
    md += md_table(["minimalServiceCPURequest", "Projected CPU", "Projected memory"],
                   [[f"{s}m", f"{c}m", f"{m}Mi"] for s, c, m in steps])
    md += ["", f"The node budget is {BUDGET_CPU_M}m CPU and {BUDGET_MEMORY_MI}Mi of memory.", ""]

    md += ["### Input rows", ""]
    body = []
    for r in sorted(rows, key=lambda r: (r.job, r.leg, r.namespace, r.owner_kind, r.component,
                                         r.workload, r.container)):
        sized = r.cls in ("formula", "glance")
        body.append([r.job, r.leg, r.namespace, r.owner_kind, r.component, r.workload, r.container,
                     f"{r.cls}, fixed memory" if r.fixed_memory and sized else r.cls,
                     r.p if sized else "-", r.t if sized else "-", r.cpu_target_m,
                     r.memory_target_mi, r.normalized_memory if sized else "-"])
    md += md_table(["job", "leg", "namespace", "owner_kind", "component", "workload", "container",
                    "class", "p", "t", "cpu_target_m", "memory_target_mi", "T"], body)
    return figures, projection, md, error


def main() -> int:
    parser = argparse.ArgumentParser(
        prog="hack/derive-sizing-figures.py",
        description="Derive the sizing figures from the recommendations of a VPA run.")
    parser.add_argument("--budget-total", metavar="<cpu>m,<memory>Mi",
                        help="the TOTAL row of the Link 6z node budget table")
    parser.add_argument("dir", help="the directory the sizing-* artifacts were downloaded to")
    args = parser.parse_args()

    try:
        budget = parse_budget(args.budget_total)
        root = Path(args.dir)
        if not root.is_dir():
            raise ParseFailure(f"cannot parse {root}: not a directory")
        rows = [row for path in sorted(root.rglob("recommendations.tsv")) for row in parse_file(path)]
        figures, projection, md, error = derive(rows, budget)
    except ParseFailure as exc:
        print(f"{PROG}: {exc}", file=sys.stderr)
        return 2
    except Incomplete as exc:
        print(f"{PROG}: {exc}", file=sys.stderr)
        return 1

    for key, value in figures:
        print(f"{key}={value}")
    print(projection)
    print()
    print("\n".join(md))
    if error:
        print(f"{PROG}: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
