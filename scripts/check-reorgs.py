#!/usr/bin/env python3
"""Check if any reorged (orphaned) blocks were falsely confirmed by FCR."""

import csv
import sys

CSV_SCHEMA_HEADER = "# fcr-simulator-csv-schema-version:2"

def open_versioned_csv(path):
    f = open(path)
    schema_header = f.readline().rstrip("\n")
    if schema_header != CSV_SCHEMA_HEADER:
        print(
            f"Unsupported CSV schema in {path}: {schema_header!r} "
            f"(expected {CSV_SCHEMA_HEADER!r})",
            file=sys.stderr,
        )
        sys.exit(1)
    return f, csv.DictReader(f)

def main():
    if len(sys.argv) < 3:
        print(f"Usage: {sys.argv[0]} <merged.csv> <reorged_slots.csv>")
        sys.exit(1)

    merged_path = sys.argv[1]
    reorgs_path = sys.argv[2]

    # Load reorged slots: slot -> old_head_block (the orphaned block root)
    reorged = {}
    with open(reorgs_path) as f:
        reader = csv.DictReader(f)
        for row in reader:
            reorged[int(row["slot"])] = row["old_head_block"]

    # Scan simulation results
    total = 0
    in_range = 0
    confirmed_reorgs = []

    f, reader = open_versioned_csv(merged_path)
    with f:
        for row in reader:
            total += 1
            slot = int(row["slot"])
            if slot in reorged:
                in_range += 1
                confirmed = row["fast_confirmed"] == "true"
                head_root = row["head_root"]
                confirmed_root = row["confirmed_root"]
                orphaned_root = reorged[slot]

                # Check: did FCR confirm the orphaned block?
                if confirmed and confirmed_root == orphaned_root:
                    confirmed_reorgs.append({
                        "slot": slot,
                        "orphaned_root": orphaned_root,
                        "confirmed_root": confirmed_root,
                        "head_root": head_root,
                    })

    print(f"Total simulation slots: {total}")
    print(f"Reorged slots in simulation range: {in_range}")
    print(f"Reorged slots where FCR confirmed the orphaned block: {len(confirmed_reorgs)}")
    print()

    if confirmed_reorgs:
        print("SAFETY VIOLATION - FCR confirmed blocks that were later reorged:")
        for r in confirmed_reorgs:
            print(f"  slot={r['slot']} orphaned={r['orphaned_root'][:18]}... confirmed={r['confirmed_root'][:18]}...")
    else:
        print("SAFE - FCR never confirmed any block that was later reorged.")

if __name__ == "__main__":
    main()
