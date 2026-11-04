#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""The seeded draw of the random chaos runs (#1395).

hack/chaos-random.sh pipes tests/chaos-random/catalogue.yaml as JSON
(``yq -o=json``) to this script, which draws a sequence of faults for one
environment and renders it as one Chaos Mesh ``Workflow``. It uses the
standard library only, keeps to Python 3.10 syntax and runs as ``python3 -I``.
``random.Random(seed)`` gives the same sequence on every platform, which
awk's ``rand()`` does not between gawk and mawk, so a seed replays a night.

The draw starts at ``t = 0`` with no previous step:

1. Draw ``a`` uniformly from the entries of the environment that were not in
   the previous step.
2. With probability ``--overlap``, draw ``b`` from those entries without
   ``a`` and without any whose first label selector equals ``a``'s; a step of
   two runs both at once.
3. The step holds for the largest ``duration`` of its entries. When it would
   end past ``--window``, the draw stops.
4. The pause after it is the largest ``recovery`` of the step plus a uniform
   whole number of seconds in ``[--pause-min, --pause-max]``.
5. The step starts at ``t``; ``t`` moves past the step and its pause, and the
   step's entries become the previous step.

The Workflow ``--name`` in ``--namespace`` runs a ``Serial`` template whose
children alternate the steps and ``Suspend`` templates of the pauses, with no
pause after the last step. A step is the chaos template ``s<NN>-<entry>``, or
the ``Parallel`` template ``s<NN>`` of two. Every chaos template carries
``deadline: <duration>`` and the entry's spec with ``@CP@`` and ``@NS@``
replaced by ``--cp`` and ``--namespace``. ``--plan`` writes (``-`` or no
option: prints) ``{seed, environment, window, steps: [{index, offset,
entries, length, pause}]}``; ``--workflow`` writes the Workflow as JSON.

Exit codes: 0 drawn; 1 no entry of the environment, or a window too short for
the first step; 2 an input error, naming the entry and the field.

docs/reference/testing/chaos-e2e-tests.md (Random runs) describes the draw.
"""

from __future__ import annotations

import argparse
import json
import random
import re
import sys
from typing import Any

EXIT_OK = 0
EXIT_NO_STEP = 1
EXIT_INPUT = 2

KINDS = {"PodChaos": "podChaos", "NetworkChaos": "networkChaos"}
ENVIRONMENTS = ("ci", "lab")
FIELDS = ("name", "kind", "environments", "duration", "recovery", "spec")
SECONDS_RE = re.compile(r"^[1-9][0-9]*s$")


class InputError(Exception):
    """An input the draw refuses, exit 2."""


def seconds(entry: str, field: str, value: Any) -> int:
    """Return the whole seconds of an ``<n>s`` value, n >= 1."""
    if not isinstance(value, str) or not SECONDS_RE.match(value):
        raise InputError(f"entry {entry}: {field} {json.dumps(value)} is not <n>s with n >= 1")
    return int(value[:-1])


def check_entries(catalogue: Any) -> list[dict]:
    """Return the catalogue's entries, each checked, or raise InputError."""
    if not isinstance(catalogue, list):
        raise InputError("the catalogue is not a list of entries")
    entries = []
    for index, raw in enumerate(catalogue):
        if not isinstance(raw, dict):
            raise InputError(f"entry {index + 1}: not a mapping")
        name = raw.get("name") if isinstance(raw.get("name"), str) else f"{index + 1}"
        for field in FIELDS:
            if field not in raw:
                raise InputError(f"entry {name}: {field} is missing")
        if raw["kind"] not in KINDS:
            raise InputError(f"entry {name}: kind {json.dumps(raw['kind'])} is neither PodChaos nor NetworkChaos")
        if not isinstance(raw["environments"], list) or not raw["environments"]:
            raise InputError(f"entry {name}: environments is empty")
        for env in raw["environments"]:
            if env not in ENVIRONMENTS:
                raise InputError(f"entry {name}: environment {json.dumps(env)} is neither ci nor lab")
        if not isinstance(raw["spec"], dict):
            raise InputError(f"entry {name}: spec is not a mapping")
        entries.append({
            "name": raw["name"],
            "kind": raw["kind"],
            "environments": raw["environments"],
            "duration": seconds(name, "duration", raw["duration"]),
            "recovery": seconds(name, "recovery", raw["recovery"]),
            "spec": raw["spec"],
        })
    return entries


def first_label(entry: dict) -> tuple[str, str] | None:
    """The first key and value of the entry's selector labelSelectors."""
    labels = (entry["spec"].get("selector") or {}).get("labelSelectors") or {}
    for key, value in labels.items():
        return key, value
    return None


def draw(entries: list[dict], args: argparse.Namespace) -> list[dict]:
    """Draw the steps of the run; see the module docstring."""
    rng = random.Random(args.seed)
    eligible = [e for e in entries if args.environment in e["environments"]]
    if not eligible:
        print(f"draw.py: no catalogue entry for environment {args.environment}", file=sys.stderr)
        sys.exit(EXIT_NO_STEP)
    steps: list[dict] = []
    t = 0
    previous: set[str] = set()
    while True:
        candidates = [e for e in eligible if e["name"] not in previous]
        if not candidates:
            break
        a = rng.choice(candidates)
        step = [a]
        if rng.random() < args.overlap:
            partners = [e for e in candidates if e["name"] != a["name"] and first_label(e) != first_label(a)]
            if partners:
                step.append(rng.choice(partners))
        length = max(e["duration"] for e in step)
        if t + length > args.window:
            break
        pause = max(e["recovery"] for e in step) + rng.randint(args.pause_min, args.pause_max)
        steps.append({
            "index": len(steps) + 1,
            "offset": t,
            "entries": [e["name"] for e in step],
            "length": length,
            "pause": pause,
        })
        t += length + pause
        previous = {e["name"] for e in step}
    if not steps:
        print(f"draw.py: the window of {args.window} s holds no step", file=sys.stderr)
        sys.exit(EXIT_NO_STEP)
    return steps


def substitute(value: Any, cp: str, namespace: str) -> Any:
    """``value`` with @CP@ and @NS@ replaced in every string."""
    if isinstance(value, str):
        return value.replace("@CP@", cp).replace("@NS@", namespace)
    if isinstance(value, list):
        return [substitute(v, cp, namespace) for v in value]
    if isinstance(value, dict):
        return {k: substitute(v, cp, namespace) for k, v in value.items()}
    return value


def workflow(steps: list[dict], entries: list[dict], args: argparse.Namespace) -> dict:
    """Render the steps as a Chaos Mesh Workflow."""
    by_name = {e["name"]: e for e in entries}
    width = max(2, len(str(len(steps))))
    serial: list[str] = []
    templates: list[dict] = []

    def chaos_template(name: str, entry: dict) -> dict:
        return {
            "name": name,
            "templateType": entry["kind"],
            "deadline": f"{entry['duration']}s",
            KINDS[entry["kind"]]: substitute(entry["spec"], args.cp, args.namespace),
        }

    for step in steps:
        prefix = f"s{step['index']:0{width}d}"
        chaos = [chaos_template(f"{prefix}-{name}", by_name[name]) for name in step["entries"]]
        if len(chaos) == 1:
            serial.append(chaos[0]["name"])
        else:
            serial.append(prefix)
            templates.append({"name": prefix, "templateType": "Parallel", "children": [c["name"] for c in chaos]})
        templates.extend(chaos)
        if step is not steps[-1]:
            pause = f"p{step['index']:0{width}d}"
            serial.append(pause)
            templates.append({"name": pause, "templateType": "Suspend", "deadline": f"{step['pause']}s"})
    return {
        "apiVersion": "chaos-mesh.org/v1alpha1",
        "kind": "Workflow",
        "metadata": {"name": args.name, "namespace": args.namespace},
        "spec": {
            "entry": "entry",
            "templates": [{"name": "entry", "templateType": "Serial", "children": serial}] + templates,
        },
    }


def write_json(path: str, value: Any) -> None:
    """Write value as indented JSON with a trailing newline; - is stdout."""
    text = json.dumps(value, indent=2) + "\n"
    if path == "-":
        sys.stdout.write(text)
        return
    with open(path, "w", encoding="utf-8") as out:
        out.write(text)


def non_negative(value: str) -> int:
    """argparse type: a whole number of seconds, 0 or more."""
    if not re.match(r"^[0-9]+$", value):
        raise argparse.ArgumentTypeError(f"not a whole number of seconds: {value}")
    return int(value)


def share(value: str) -> float:
    """argparse type: a probability from 0 to 1."""
    try:
        number = float(value)
    except ValueError:
        raise argparse.ArgumentTypeError(f"not a number: {value}") from None
    if not 0 <= number <= 1:
        raise argparse.ArgumentTypeError(f"not between 0 and 1: {value}")
    return number


def main(argv: list[str] | None = None) -> int:
    """Parse the arguments, draw, and write the plan and the Workflow."""
    parser = argparse.ArgumentParser(prog="draw.py", description="Draw a random chaos run from the catalogue on stdin.")
    parser.add_argument("--environment", required=True, choices=ENVIRONMENTS)
    parser.add_argument("--seed", required=True, type=int)
    parser.add_argument("--window", required=True, type=non_negative, help="seconds the steps may span")
    parser.add_argument("--pause-min", required=True, type=non_negative)
    parser.add_argument("--pause-max", required=True, type=non_negative)
    parser.add_argument("--overlap", required=True, type=share)
    parser.add_argument("--name", default="chaos-random", help="the Workflow's name")
    parser.add_argument("--cp", default="controlplane", help="the ControlPlane's name, for @CP@")
    parser.add_argument("--namespace", default="openstack", help="the ControlPlane's namespace, for @NS@")
    parser.add_argument("--plan", default="-", help="where to write the plan (- prints it)")
    parser.add_argument("--workflow", help="where to write the Workflow")
    args = parser.parse_args(argv)
    if args.pause_min > args.pause_max:
        parser.error(f"--pause-min {args.pause_min} is above --pause-max {args.pause_max}")

    try:
        catalogue = json.load(sys.stdin)
    except json.JSONDecodeError as exc:
        print(f"draw.py: the catalogue on stdin is not JSON: {exc}", file=sys.stderr)
        return EXIT_INPUT
    try:
        entries = check_entries(catalogue)
    except InputError as exc:
        print(f"draw.py: {exc}", file=sys.stderr)
        return EXIT_INPUT

    steps = draw(entries, args)
    write_json(args.plan, {"seed": args.seed, "environment": args.environment, "window": args.window, "steps": steps})
    if args.workflow:
        write_json(args.workflow, workflow(steps, entries, args))
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
