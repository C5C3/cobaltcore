#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright 2026 SAP SE or an SAP affiliate company
#
# SPDX-License-Identifier: Apache-2.0

"""Compute, compare and record the keystone-operator performance baselines.

``token`` reads the log of the token stream of the chaos workload harness
(tests/e2e-chaos/lib/workload.py), takes the nearest-rank p50 and p99 of the
``ms`` of the ok issue and validate records, and writes them with the sample
and failure counts to a JSON file. hack/perf-token-baseline.sh runs it.

``compare`` judges the measured JSON files of a night against the committed
baseline (tests/perf/baseline.json): every baseline key that ends in ``_ms`` or
``_s`` may be at most the tolerance above its baseline value. Keys ending in
``.samples`` or ``.failed`` are printed and not judged. The nightly Performance
workflow (.github/workflows/performance.yaml) runs it.

``write-baseline`` merges measured JSON files into a new baseline that records
the release, the commit and the workflow run it was measured on.

Exit codes: 0 pass; 1 a judged failure (too few samples, a failed record, a
regression); 2 an input error. Only the standard library is used.
docs/reference/testing/reconcile-performance-benchmark.md describes the three
subcommands and the baseline file.
"""

from __future__ import annotations

import argparse
import datetime
import json
import os
import sys
from typing import Any

EXIT_OK = 0
EXIT_JUDGED = 1
EXIT_INPUT = 2

TOKEN_OPS = ("issue", "validate")
PERCENTILES = (50, 99)
OFFENDING_LIMIT = 10


class InputError(Exception):
    """An input the subcommand cannot judge; main exits 2 with the reason."""


def is_number(value: Any) -> bool:
    """Return whether value is an int or a float, a bool excluded."""
    return isinstance(value, (int, float)) and not isinstance(value, bool)


def write_json(path: str, data: dict) -> None:
    """Write data to path with sorted keys, a two-space indent and a newline.

    Raises InputError when the file cannot be written.
    """
    try:
        parent = os.path.dirname(path)
        if parent:
            os.makedirs(parent, exist_ok=True)
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(data, handle, indent=2, sort_keys=True)
            handle.write("\n")
    except OSError as exc:
        raise InputError(f"cannot write {path}: {exc}") from exc


def load_json(path: str) -> dict:
    """Return the JSON object in path.

    Raises InputError when the file cannot be read, is not JSON or holds
    something other than an object.
    """
    try:
        with open(path, encoding="utf-8") as handle:
            data = json.load(handle)
    except (OSError, ValueError) as exc:
        raise InputError(f"cannot read {path}: {exc}") from exc
    if not isinstance(data, dict):
        raise InputError(f"{path} holds no JSON object")
    return data


def read_token_records(path: str) -> tuple[list[dict], int]:
    """Return the token records of a workload log and the count of skipped lines.

    A line that is not a JSON object with a string ``op``, a boolean ``ok`` and
    a numeric ``ms`` is skipped and counted; READY and DONE are such lines. A
    record of another stream is left out without being counted. Blank lines
    are ignored.

    Raises InputError when the log cannot be opened or holds no token record.
    """
    try:
        with open(path, encoding="utf-8", errors="replace") as handle:
            lines = handle.read().splitlines()
    except OSError as exc:
        raise InputError(f"cannot open {path}: {exc}") from exc
    records = []
    skipped = 0
    for line in lines:
        if not line.strip():
            continue
        try:
            record = json.loads(line)
        except ValueError:
            skipped += 1
            continue
        if not (isinstance(record, dict)
                and isinstance(record.get("op"), str)
                and isinstance(record.get("ok"), bool)
                and is_number(record.get("ms"))):
            skipped += 1
            continue
        if record.get("s") == "token":
            records.append(record)
    if not records:
        raise InputError("the log holds no token record")
    return records, skipped


def nearest_rank(values: list[int | float], percentile: int) -> int | float | None:
    """Return the nearest-rank percentile of values, or None when it is empty.

    The nearest rank is the smallest value at or above which percentile per
    cent of the values lie: the ceil(percentile * n / 100)-th smallest.
    """
    if not values:
        return None
    ordered = sorted(values)
    rank = max(1, (percentile * len(ordered) + 99) // 100)
    return ordered[rank - 1]


def token_metrics(records: list[dict]) -> dict[str, int | float | None]:
    """Return the eight token.* metrics of the token records.

    For issue and validate: the count of ok records (samples), the count of
    failed records, and the p50 and p99 over the ``ms`` of the ok records,
    None when the op has no ok record.
    """
    metrics: dict[str, int | float | None] = {}
    for op in TOKEN_OPS:
        own = [r for r in records if r["op"] == op]
        ok_ms = [r["ms"] for r in own if r["ok"]]
        metrics[f"token.{op}.samples"] = len(ok_ms)
        metrics[f"token.{op}.failed"] = len(own) - len(ok_ms)
        for percentile in PERCENTILES:
            metrics[f"token.{op}.p{percentile}_ms"] = nearest_rank(ok_ms, percentile)
    return metrics


def cmd_token(args: argparse.Namespace) -> int:
    """Write the token metrics of a log to --out and judge the samples.

    Returns 1 when an op has fewer than --min-samples ok records or any
    failed record, 0 otherwise. The JSON is written in both cases.
    """
    records, skipped = read_token_records(args.log)
    metrics = token_metrics(records)
    print(f"skipped {skipped} line(s) that are not workload records")
    for op in TOKEN_OPS:
        print(f"token.{op}: {metrics[f'token.{op}.samples']} ok, "
              f"{metrics[f'token.{op}.failed']} failed, "
              f"p50={metrics[f'token.{op}.p50_ms']} ms, "
              f"p99={metrics[f'token.{op}.p99_ms']} ms")
    write_json(args.out, {
        "meta": {
            "release": args.release,
            "measured_at": datetime.datetime.now(datetime.timezone.utc).isoformat(
                timespec="seconds"),
            "token_records": len(records),
        },
        "metrics": metrics,
    })
    print(f"wrote {args.out}")

    failures = []
    for op in TOKEN_OPS:
        samples = metrics[f"token.{op}.samples"]
        failed = metrics[f"token.{op}.failed"]
        if samples < args.min_samples:
            failures.append(f"token.{op}: {samples} ok record(s), want at "
                            f"least {args.min_samples}")
        if failed:
            failures.append(f"token.{op}: {failed} failed record(s)")
    if not failures:
        return EXIT_OK
    for failure in failures:
        print(f"FAIL: {failure}")
    offending = [r for r in records if not r["ok"] and r["op"] in TOKEN_OPS]
    for record in offending[:OFFENDING_LIMIT]:
        print(f"offending: {json.dumps(record)}")
    return EXIT_JUDGED


def merge_metrics(paths: list[str]) -> tuple[dict[str, Any], dict[str, str]]:
    """Merge the metrics of measured files.

    Returns the merged metrics and the meta.release of every file, keyed by
    path. Raises InputError when a file cannot be parsed, lacks meta.release
    or a metrics object, carries a value that is neither a number nor null,
    or names a metric another file names too.
    """
    merged: dict[str, Any] = {}
    origin: dict[str, str] = {}
    releases: dict[str, str] = {}
    for path in paths:
        data = load_json(path)
        meta = data.get("meta")
        if not isinstance(meta, dict) or not isinstance(meta.get("release"), str):
            raise InputError(f"{path} has no meta.release")
        metrics = data.get("metrics")
        if not isinstance(metrics, dict):
            raise InputError(f"{path} has no metrics object")
        releases[path] = meta["release"]
        for key, value in metrics.items():
            if value is not None and not is_number(value):
                raise InputError(f"{key} in {path} is not a number")
            if key in merged:
                raise InputError(f"metric {key} appears in {origin[key]} and {path}")
            merged[key] = value
            origin[key] = path
    return merged, releases


def judged(key: str) -> bool:
    """Return whether compare judges the key: a latency in _ms or _s."""
    return key.endswith("_ms") or key.endswith("_s")


def cmd_compare(args: argparse.Namespace) -> int:
    """Judge the measured files against the baseline and print one row per key.

    Returns 1 when a judged key exceeds its baseline by more than the
    tolerance, 0 otherwise. Raises InputError when the releases differ, a
    baseline key is missing from the measured files or a value is null.
    """
    baseline = load_json(args.baseline)
    recorded = baseline.get("recorded")
    if not isinstance(recorded, dict) or not isinstance(recorded.get("release"), str):
        raise InputError(f"{args.baseline} has no recorded.release")
    base_metrics = baseline.get("metrics")
    if not isinstance(base_metrics, dict):
        raise InputError(f"{args.baseline} has no metrics object")
    for key, value in base_metrics.items():
        if not is_number(value):
            raise InputError(f"baseline key {key} is not a number")

    measured, releases = merge_metrics(args.measured)
    for path, release in releases.items():
        if release != recorded["release"]:
            raise InputError(f"{path} is of release {release}, the baseline "
                             f"{args.baseline} of release {recorded['release']}")
    for key in sorted(base_metrics):
        if key not in measured:
            raise InputError(f"baseline key {key} is missing from the measured files")
    for key in sorted(measured):
        if measured[key] is None:
            raise InputError(f"{key} is null in the measured files")

    rows = [("key", "baseline", "measured", "delta", "verdict")]
    regressions = 0
    for key in sorted(set(base_metrics) | set(measured)):
        value = measured[key]
        if key not in base_metrics:
            rows.append((key, "-", json.dumps(value), "-", "new"))
            continue
        base = base_metrics[key]
        delta = "n/a"
        if base != 0:
            delta = f"{(value - base) / base * 100:+.1f} %"
        verdict = "-"
        if judged(key):
            verdict = "ok"
            if value > base * (1 + args.tolerance_percent / 100):
                verdict = "REGRESSION"
                regressions += 1
        rows.append((key, json.dumps(base), json.dumps(value), delta, verdict))

    widths = [max(len(row[i]) for row in rows) for i in range(len(rows[0]))]
    for row in rows:
        cells = (cell.ljust(width) for cell, width in zip(row, widths))
        print("  ".join(cells).rstrip())
    tolerance = f"{args.tolerance_percent:g}"
    if regressions:
        print(f"compare: {regressions} regression(s) above {tolerance} %")
        return EXIT_JUDGED
    print(f"compare: every judged metric is within {tolerance} %")
    return EXIT_OK


def cmd_write_baseline(args: argparse.Namespace) -> int:
    """Merge the measured files into a baseline at --out.

    Raises InputError when a measured file is of another release than
    --release or a value is null, since compare could never judge it.
    """
    metrics, releases = merge_metrics(args.measured)
    for path, release in releases.items():
        if release != args.release:
            raise InputError(f"{path} is of release {release}, not {args.release}")
    for key in sorted(metrics):
        if metrics[key] is None:
            raise InputError(f"{key} is null in the measured files; measure again")
    write_json(args.out, {
        "recorded": {
            "release": args.release,
            "commit": args.commit,
            "run": args.run,
            "date": datetime.datetime.now(datetime.timezone.utc).date().isoformat(),
        },
        "metrics": metrics,
    })
    print(f"wrote {args.out} ({len(metrics)} metrics)")
    return EXIT_OK


def non_negative_int(text: str) -> int:
    """Parse an argparse value as an int of at least 0."""
    try:
        value = int(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"{text!r} is not an integer") from exc
    if value < 0:
        raise argparse.ArgumentTypeError(f"{text} is negative")
    return value


def non_negative_float(text: str) -> float:
    """Parse an argparse value as a finite float of at least 0."""
    try:
        value = float(text)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"{text!r} is not a number") from exc
    if not 0 <= value < float("inf"):
        raise argparse.ArgumentTypeError(f"{text} is not a finite number of at least 0")
    return value


def main(argv: list[str] | None = None) -> int:
    """Dispatch token, compare and write-baseline."""
    parser = argparse.ArgumentParser(
        prog="perf-baseline.py",
        description="Compute, compare and record the performance baselines.")
    sub = parser.add_subparsers(dest="command", required=True)

    token = sub.add_parser("token", help="token metrics of a workload log")
    token.add_argument("--log", required=True)
    token.add_argument("--release", required=True)
    token.add_argument("--min-samples", type=non_negative_int, required=True)
    token.add_argument("--out", required=True)

    compare = sub.add_parser("compare", help="judge measured files against the baseline")
    compare.add_argument("--baseline", required=True)
    compare.add_argument("--tolerance-percent", type=non_negative_float, required=True)
    compare.add_argument("--measured", nargs="+", required=True)

    write = sub.add_parser("write-baseline", help="write a baseline from measured files")
    write.add_argument("--measured", nargs="+", required=True)
    write.add_argument("--release", required=True)
    write.add_argument("--commit", required=True)
    write.add_argument("--run", required=True)
    write.add_argument("--out", required=True)

    args = parser.parse_args(argv)
    commands = {"token": cmd_token, "compare": cmd_compare,
                "write-baseline": cmd_write_baseline}
    try:
        return commands[args.command](args)
    except InputError as exc:
        print(f"{args.command}: {exc}", file=sys.stderr)
        return EXIT_INPUT


if __name__ == "__main__":
    sys.exit(main())
