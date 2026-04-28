#!/usr/bin/env python3
"""
Aggregate bench CSVs across run_id by taking the median per scenario.

Two CSV shapes are supported and auto-detected from the header:
  - scale:  rows from TestScaleMatrix             (key = storage_name + cluster_count)
  - perf:   rows from TestStoragePerformance{,HA} (key = storage_name + profile + concurrency)

Usage:
  python aggregate.py [--out-prefix=aggregate] [--md] CSV [CSV ...]

For each input CSV, writes a *-medians.csv next to the prefix, and (when --md)
prints a per-question Markdown table to stdout suitable for the article.

Stdlib only — no pandas / numpy dependency.
"""

from __future__ import annotations

import argparse
import csv
import glob
import os
import statistics
import sys
from collections import OrderedDict, defaultdict


SCALE_KEY = ("storage_name", "cluster_count", "parallelism")
PERF_KEY = (
    "storage_name", "profile", "concurrency", "ops",
    "hcp_cpu_limit", "hcp_memory_limit",
)

# Columns that should be treated as text — kept verbatim from the first row in
# the group rather than averaged.
TEXT_COLUMNS = {
    "timestamp",
    "storage_name",
    "profile",
    "hcp_cpu_limit",
    "hcp_memory_limit",
    "create_first_err",
    "update1_first_err",
    "update2_first_err",
    "delete_first_err",
    "watch_first_err",
    "storage_internals_json",
}


def detect_kind(header: list[str]) -> str:
    if "cluster_count" in header and "provision_p50_s" in header:
        return "scale"
    if "concurrency" in header and "write_p50_s" in header:
        return "perf"
    raise ValueError(f"unrecognised CSV shape; header={header!r}")


def median_or_first(values: list[str]) -> str:
    """Median of numeric strings; pass-through string for text columns."""
    nums = []
    for v in values:
        if v == "":
            continue
        try:
            nums.append(float(v))
        except ValueError:
            return values[0]
    if not nums:
        return ""
    m = statistics.median(nums)
    if m == int(m):
        return str(int(m))
    return f"{m:.3f}"


def aggregate(rows: list[dict], key_cols: tuple[str, ...]) -> list[dict]:
    groups: dict[tuple, list[dict]] = OrderedDict()
    for r in rows:
        k = tuple(r.get(c, "") for c in key_cols)
        groups.setdefault(k, []).append(r)

    out: list[dict] = []
    for k, members in groups.items():
        agg: dict[str, str] = {}
        for col in members[0].keys():
            vals = [m.get(col, "") for m in members]
            if col in TEXT_COLUMNS:
                agg[col] = vals[0]
            elif col == "run_id":
                agg[col] = f"median-of-{len(members)}"
            else:
                agg[col] = median_or_first(vals)
        out.append(agg)
    return out


def fmt_md(rows: list[dict], cols: list[str]) -> str:
    if not rows:
        return ""
    header = "| " + " | ".join(cols) + " |"
    sep = "| " + " | ".join("---" for _ in cols) + " |"
    body = ["| " + " | ".join(str(r.get(c, "")) for c in cols) + " |" for r in rows]
    return "\n".join([header, sep, *body])


# Selected columns for the article's headline tables. Everything else stays in
# the medians CSV for deeper digging.
SCALE_HEADLINE = [
    "storage_name", "cluster_count",
    "provision_p50_s", "provision_p99_s",
    "hcp_total_mem_mi", "hcp_total_cpu_m",
    "storage_total_mem_mi", "storage_total_cpu_m",
    "operator_mem_mi", "operator_cpu_m",
    "worker_cpu_max_pct",
    "churn_recovery_p50_s",
]

PERF_HEADLINE = [
    "storage_name", "profile", "concurrency",
    "write_p50_s", "write_p99_s", "write_throughput_ops",
    "write_error_rate",
    "watch_lag_p99_s",
    "hcp_max_mem_mi", "hcp_max_cpu_m",
]


def process_csv(path: str, out_prefix: str, want_md: bool) -> None:
    with open(path, newline="") as f:
        reader = csv.DictReader(f)
        rows = list(reader)
        if not rows:
            print(f"{path}: empty", file=sys.stderr)
            return
        kind = detect_kind(reader.fieldnames or [])

    if kind == "scale":
        medians = aggregate(rows, SCALE_KEY)
        headline_cols = SCALE_HEADLINE
    else:
        medians = aggregate(rows, PERF_KEY)
        headline_cols = PERF_HEADLINE

    out_csv = f"{out_prefix}-{kind}-{os.path.splitext(os.path.basename(path))[0]}.csv"
    with open(out_csv, "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=list(medians[0].keys()))
        w.writeheader()
        w.writerows(medians)
    print(f"wrote {out_csv}  ({len(medians)} median rows from {len(rows)} raw)", file=sys.stderr)

    if want_md:
        present_cols = [c for c in headline_cols if c in medians[0]]
        print(f"\n## {os.path.basename(path)}  ({kind}, median across run_id)\n")
        print(fmt_md(medians, present_cols))


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser()
    p.add_argument("paths", nargs="+", help="CSV files (globs OK)")
    p.add_argument("--out-prefix", default="aggregate",
                   help="prefix for the medians CSV output (default: %(default)s)")
    p.add_argument("--md", action="store_true",
                   help="print headline Markdown table to stdout per input")
    args = p.parse_args(argv)

    expanded: list[str] = []
    for pat in args.paths:
        matches = glob.glob(pat)
        if not matches:
            print(f"warning: no match for {pat!r}", file=sys.stderr)
            continue
        expanded.extend(sorted(matches))

    if not expanded:
        print("no input files", file=sys.stderr)
        return 1

    for path in expanded:
        process_csv(path, args.out_prefix, args.md)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
