#!/usr/bin/env python3
"""Per-slot cross-engine diff over the cross-client smoke results.

Reads results/cross-client/<engine>/epoch-<N>.csv for each engine and emits:
  - results/cross-client/per-slot.csv  (one row per (engine, slot) with consensus columns)
  - results/cross-client/divergences.csv  (slots where >=2 engines disagree on any column)
  - a stdout summary of agreement / divergence counts per column

Usage:
  scripts/cross-client/diff.py [--root results/cross-client] [engines ...]
"""

from __future__ import annotations

import argparse
import csv
import os
import sys
from collections import defaultdict
from pathlib import Path

CONSENSUS_COLS = [
    "has_block",
    "block_root",
    "head_root",
    "confirmed_root",
    "confirmed_slot",
    "confirmation_delay_slots",
    "fast_confirmed",
    "strict_one_slot_confirmed",
    "finalized_epoch",
    "justified_epoch",
    "num_attestations_injected",
    "is_epoch_boundary",
    "is_missed_slot",
]


def read_engine_csv(path: Path) -> list[dict]:
    with path.open() as f:
        first = f.readline()
        if not first.startswith("#"):
            f.seek(0)
        return list(csv.DictReader(f))


def collect(root: Path, engines: list[str]) -> dict[int, dict[str, dict]]:
    out: dict[int, dict[str, dict]] = defaultdict(dict)
    for engine in engines:
        engine_dir = root / engine
        if not engine_dir.exists():
            continue
        for csv_path in sorted(engine_dir.glob("epoch-*.csv")):
            for row in read_engine_csv(csv_path):
                slot = int(row["slot"])
                out[slot][engine] = row
    return out


def detect_engines(root: Path) -> list[str]:
    return sorted([p.name for p in root.iterdir() if p.is_dir()])


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", default="results/cross-client", type=Path)
    parser.add_argument("engines", nargs="*", help="restrict to these engines; default = all subdirs of --root")
    args = parser.parse_args()

    if not args.root.exists():
        print(f"no such directory: {args.root}", file=sys.stderr)
        return 2

    engines = args.engines or detect_engines(args.root)
    if not engines:
        print(f"no engine subdirectories found under {args.root}", file=sys.stderr)
        return 2

    per_slot = collect(args.root, engines)
    if not per_slot:
        print(f"no rows found for engines {engines} under {args.root}", file=sys.stderr)
        return 2

    per_slot_path = args.root / "per-slot.csv"
    div_path = args.root / "divergences.csv"

    rows_written = 0
    divergent_rows = 0
    per_col_disagree: dict[str, int] = {c: 0 for c in CONSENSUS_COLS}

    with per_slot_path.open("w", newline="") as f_all, div_path.open("w", newline="") as f_div:
        writer = csv.writer(f_all)
        div_writer = csv.writer(f_div)
        writer.writerow(["slot", "engine", *CONSENSUS_COLS])
        div_writer.writerow(["slot", "column", *engines])

        for slot in sorted(per_slot):
            engines_present = per_slot[slot]
            for engine in engines:
                if engine not in engines_present:
                    continue
                row = engines_present[engine]
                writer.writerow([slot, engine, *(row.get(c, "") for c in CONSENSUS_COLS)])
                rows_written += 1

            seen_engines = [e for e in engines if e in engines_present]
            if len(seen_engines) < 2:
                continue
            row_had_divergence = False
            for col in CONSENSUS_COLS:
                values = [engines_present[e].get(col, "") for e in seen_engines]
                if len(set(values)) > 1:
                    per_col_disagree[col] += 1
                    row_had_divergence = True
                    div_writer.writerow([slot, col, *values])
            if row_had_divergence:
                divergent_rows += 1

    total_slots = len(per_slot)
    print(f"wrote {per_slot_path} ({rows_written} engine-slot rows over {total_slots} unique slots)")
    print(f"wrote {div_path} ({divergent_rows} divergent slots of {total_slots})")
    print("")
    print("per-column disagreement counts (slot-level):")
    for col in CONSENSUS_COLS:
        marker = "*" if per_col_disagree[col] else " "
        print(f"  {marker} {col:30s} {per_col_disagree[col]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
