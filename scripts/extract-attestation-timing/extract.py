#!/usr/bin/env python3
"""
Extract attestation first-seen timing data from xatu parquet files on R2.

Produces a dense committee-order format: one row per slot with parallel arrays
of slot_offsets and vote_ids indexed by committee position.

Processes one hour at a time to keep memory usage bounded (~4-6 GB).

Output schema per slot:
  - slot (u64)
  - slot_offsets (list[u8]): one per committee position. 255 = not seen by xatu.
    Value N means attestation was first seen N slots after duty slot.
  - vote_ids (list[u8]): one per committee position. 255 = not seen.
    Index into the votes lookup table for this slot.
  - votes (list[struct]): distinct (head_root, source_epoch, source_root, target_epoch, target_root)
    tuples observed in this slot. Typically 4-14 entries.
  - committee_size (u32): total validators in committee
  - seen_count (u32): validators seen by xatu

Usage:
    python extract.py --start-date 2024-07-01 --end-date 2024-07-02 --output-dir ./output
    python extract.py --start-date 2024-07-01 --end-date 2024-07-01 --output-dir ./output --hours 0,1,2
"""

import argparse
import hashlib
import json
import os
import time
from datetime import datetime, timedelta
from pathlib import Path

import duckdb
import httpx

R2_BASE = "https://data.ethpandaops.io/xatu/mainnet/databases/default"
BEACON_API_TABLE = "beacon_api_eth_v1_events_attestation"
COMMITTEE_TABLE = "canonical_beacon_committee"


def check_parquet_exists(table: str, year: int, month: int, day: int, part: int | str) -> tuple[bool, int]:
    if isinstance(part, int):
        url = f"{R2_BASE}/{table}/{year}/{month}/{day}/{part}.parquet"
    else:
        url = f"{R2_BASE}/{table}/{year}/{month}/{day}.parquet"
    try:
        resp = httpx.head(url, follow_redirects=True, timeout=10)
        if resp.status_code == 200:
            return True, int(resp.headers.get("content-length", 0))
        return False, 0
    except Exception:
        return False, 0


def download_file(url: str, dest: Path) -> bool:
    """Download a file from URL to local path. Returns success."""
    try:
        with httpx.stream("GET", url, follow_redirects=True, timeout=httpx.Timeout(connect=10, read=300, write=10, pool=10)) as resp:
            if resp.status_code != 200:
                return False
            with open(dest, "wb") as f:
                for chunk in resp.iter_bytes(chunk_size=65536):
                    f.write(chunk)
        return True
    except Exception as e:
        print(f"    download error {url}: {e}")
        return False


def process_hour(db: duckdb.DuckDBPyConnection, committee_file: Path, attestation_urls: list[str],
                 slot_min: int, slot_max: int) -> bool:
    """Process one hour. Returns True on success, False if skipped due to bad data."""

    try:
        return _process_hour_inner(db, committee_file, attestation_urls, slot_min, slot_max)
    except Exception as e:
        print(f"    WARNING: skipping hour (slots {slot_min}-{slot_max}): {e}")
        # Clean up any partial tables
        for t in ["hour_committee", "hour_timing", "hour_joined", "hour_with_votes"]:
            db.execute(f"DROP TABLE IF EXISTS {t}")
        return False


def _process_hour_inner(db: duckdb.DuckDBPyConnection, committee_file: Path, attestation_urls: list[str],
                        slot_min: int, slot_max: int) -> bool:
    """Inner implementation of process_hour."""

    # Load committee for this hour's slots only
    db.execute(f"""
        CREATE OR REPLACE TABLE hour_committee AS
        SELECT slot,
            row_number() OVER (PARTITION BY slot ORDER BY committee_index, pos) - 1 AS position,
            vi AS validator_index
        FROM (
            SELECT slot, committee_index, unnest(validators) AS vi, generate_subscripts(validators, 1) AS pos
            FROM read_parquet('{committee_file}')
            WHERE slot BETWEEN {slot_min} AND {slot_max}
        )
    """)

    # Load and aggregate attestation data for this hour
    unions = []
    for url in attestation_urls:
        unions.append(f"""
            SELECT slot, attesting_validator_index AS validator_index,
                propagation_slot_start_diff, beacon_block_root AS head_root,
                source_epoch, source_root, target_epoch, target_root
            FROM read_parquet('{url}')
            WHERE attesting_validator_index IS NOT NULL
                AND slot BETWEEN {slot_min} AND {slot_max}
        """)

    union_sql = "\n            UNION ALL\n".join(unions)

    db.execute(f"""
        CREATE OR REPLACE TABLE hour_timing AS
        SELECT
            slot, validator_index,
            CAST(LEAST(MIN(propagation_slot_start_diff) / 12000, 254) AS UTINYINT) AS slot_offset,
            arg_min(head_root, propagation_slot_start_diff) AS head_root,
            arg_min(source_epoch, propagation_slot_start_diff) AS source_epoch,
            arg_min(source_root, propagation_slot_start_diff) AS source_root,
            arg_min(target_epoch, propagation_slot_start_diff) AS target_epoch,
            arg_min(target_root, propagation_slot_start_diff) AS target_root
        FROM ({union_sql})
        GROUP BY slot, validator_index
    """)

    # Join
    db.execute("""
        CREATE OR REPLACE TABLE hour_joined AS
        SELECT
            c.slot, c.position,
            COALESCE(t.slot_offset, CAST(255 AS UTINYINT)) AS slot_offset,
            t.head_root, t.source_epoch, t.source_root, t.target_epoch, t.target_root
        FROM hour_committee c
        LEFT JOIN hour_timing t ON c.slot = t.slot AND c.validator_index = t.validator_index
    """)

    # Build vote IDs
    db.execute("""
        CREATE OR REPLACE TABLE hour_with_votes AS
        SELECT
            slot, position, slot_offset,
            CASE WHEN head_root IS NOT NULL
                THEN CAST(dense_rank() OVER (
                    PARTITION BY slot
                    ORDER BY COALESCE(head_root, ''), COALESCE(source_epoch, 0),
                             COALESCE(source_root, ''), COALESCE(target_epoch, 0),
                             COALESCE(target_root, '')
                ) - 1 AS UTINYINT)
                ELSE CAST(255 AS UTINYINT)
            END AS vote_id,
            head_root, source_epoch, source_root, target_epoch, target_root
        FROM hour_joined
    """)

    # Pack into per-slot rows and append to results
    db.execute("""
        INSERT INTO day_results
        SELECT
            CAST(slot AS UBIGINT) AS slot,
            list(slot_offset ORDER BY position) AS slot_offsets,
            list(vote_id ORDER BY position) AS vote_ids,
            list(DISTINCT (head_root, source_epoch, source_root, target_epoch, target_root)
                ORDER BY (head_root, source_epoch, source_root, target_epoch, target_root))
                FILTER (WHERE head_root IS NOT NULL) AS votes,
            CAST(count(*) AS UINTEGER) AS committee_size,
            CAST(count(*) FILTER (WHERE slot_offset != 255) AS UINTEGER) AS seen_count
        FROM hour_with_votes
        GROUP BY slot
    """)

    # Free memory
    db.execute("DROP TABLE IF EXISTS hour_committee")
    db.execute("DROP TABLE IF EXISTS hour_timing")
    db.execute("DROP TABLE IF EXISTS hour_joined")
    db.execute("DROP TABLE IF EXISTS hour_with_votes")
    return True


def process_day(date: datetime, output_dir: Path, hours: list[int] | None = None, dry_run: bool = False) -> dict:
    year, month, day = date.year, date.month, date.day
    date_str = date.strftime("%Y-%m-%d")
    output_file = output_dir / f"{date_str}.parquet"

    if output_file.exists() and output_file.stat().st_size > 0:
        print(f"  SKIP {date_str}: output already exists")
        return {"date": date_str, "status": "skipped"}

    hour_range = hours if hours is not None else list(range(24))

    # Check committee data
    committee_url = f"{R2_BASE}/{COMMITTEE_TABLE}/{year}/{month}/{day}.parquet"
    committee_exists, committee_size = check_parquet_exists(COMMITTEE_TABLE, year, month, day, "daily")
    if not committee_exists:
        committee_exists, committee_size = check_parquet_exists(COMMITTEE_TABLE, year, month, day, None)
    if not committee_exists:
        print(f"  SKIP {date_str}: no committee data")
        return {"date": date_str, "status": "no_committee_data"}

    # Collect attestation URLs per hour (beacon_api only)
    hour_sources: dict[int, str] = {}
    total_source_bytes = committee_size
    beacon_count = 0

    for h in hour_range:
        exists, size = check_parquet_exists(BEACON_API_TABLE, year, month, day, h)
        if exists and size > 0:
            hour_sources[h] = f"{R2_BASE}/{BEACON_API_TABLE}/{year}/{month}/{day}/{h}.parquet"
            total_source_bytes += size
            beacon_count += 1

    if not hour_sources:
        print(f"  SKIP {date_str}: no attestation data")
        return {"date": date_str, "status": "no_data"}

    print(f"  {date_str}: {beacon_count} beacon_api hours ({total_source_bytes / 1024 / 1024 / 1024:.1f} GB)")

    if dry_run:
        return {"date": date_str, "status": "dry_run", "beacon_hours": beacon_count}

    t0 = time.time()
    tmp_dir = output_dir / ".tmp"
    tmp_dir.mkdir(exist_ok=True)

    # Download committee file locally (avoids re-downloading per hour)
    committee_local = tmp_dir / f"{date_str}_committee.parquet"
    if not committee_local.exists():
        print(f"    downloading committee data...")
        if not download_file(committee_url, committee_local):
            print(f"  ERROR {date_str}: failed to download committee data")
            return {"date": date_str, "status": "error", "error": "committee download failed"}

    dl_time = time.time() - t0
    print(f"    committee ready ({dl_time:.0f}s)")

    # Get slot range from committee file
    db = duckdb.connect()
    db.execute("INSTALL httpfs; LOAD httpfs;")
    db.execute("SET http_retries = 3;")
    db.execute("SET http_retry_wait_ms = 2000;")
    db.execute(f"SET threads = {min(os.cpu_count() or 4, 8)};")

    slot_range = db.execute(f"SELECT min(slot), max(slot) FROM read_parquet('{committee_local}')").fetchone()
    min_slot, max_slot = slot_range[0], slot_range[1]
    if min_slot is None or max_slot is None:
        print(f"  SKIP {date_str}: committee data is empty")
        db.close()
        committee_local.unlink(missing_ok=True)
        return {"date": date_str, "status": "no_committee_data"}
    slots_per_hour = (max_slot - min_slot + 1) // 24

    # Create results table
    db.execute("""
        CREATE TABLE day_results (
            slot UBIGINT,
            slot_offsets UTINYINT[],
            vote_ids UTINYINT[],
            votes STRUCT(head_root VARCHAR, source_epoch UINTEGER, source_root VARCHAR, target_epoch UINTEGER, target_root VARCHAR)[],
            committee_size UINTEGER,
            seen_count UINTEGER
        )
    """)

    # Process each hour independently
    for h in sorted(hour_sources.keys()):
        h_slot_min = min_slot + h * slots_per_hour
        h_slot_max = min_slot + (h + 1) * slots_per_hour - 1
        if h == 23:
            h_slot_max = max_slot  # last hour gets remainder

        process_hour(db, committee_local, [hour_sources[h]], h_slot_min, h_slot_max)
        elapsed = time.time() - t0
        print(f"    hour {h}: {elapsed:.0f}s elapsed")

    # Write output
    db.execute(f"""
        COPY (SELECT * FROM day_results ORDER BY slot)
        TO '{output_file}' (FORMAT PARQUET, COMPRESSION ZSTD, ROW_GROUP_SIZE 100)
    """)

    elapsed = time.time() - t0
    output_size = output_file.stat().st_size
    row_count = db.execute(f"SELECT count() FROM day_results").fetchone()[0]
    total_seen = db.execute(f"SELECT sum(seen_count) FROM day_results").fetchone()[0]
    db.close()

    # Cleanup
    committee_local.unlink(missing_ok=True)

    sha256 = hashlib.sha256()
    with open(output_file, "rb") as f:
        for chunk in iter(lambda: f.read(65536), b""):
            sha256.update(chunk)

    meta = {
        "date": date_str,
        "status": "ok",
        "slots": row_count,
        "total_seen": total_seen,
        "output_bytes": output_size,
        "output_kb": round(output_size / 1024, 1),
        "source_bytes": total_source_bytes,
        "beacon_hours": beacon_count,
        "elapsed_seconds": round(elapsed, 1),
        "sha256": sha256.hexdigest(),
    }
    print(f"  OK {date_str}: {row_count} slots, {total_seen:,} seen, {meta['output_kb']} KB, {elapsed:.0f}s")
    return meta


def main():
    parser = argparse.ArgumentParser(description="Extract attestation timing data from xatu parquet files")
    parser.add_argument("--start-date", required=True, help="Start date (YYYY-MM-DD)")
    parser.add_argument("--end-date", required=True, help="End date inclusive (YYYY-MM-DD)")
    parser.add_argument("--output-dir", required=True, help="Output directory for parquet files")
    parser.add_argument("--hours", help="Comma-separated hours to process (default: all 24)", default=None)
    parser.add_argument("--dry-run", action="store_true", help="Check data availability without processing")
    args = parser.parse_args()

    start = datetime.strptime(args.start_date, "%Y-%m-%d")
    end = datetime.strptime(args.end_date, "%Y-%m-%d")
    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    hours = [int(h) for h in args.hours.split(",")] if args.hours else None

    manifest_path = output_dir / "manifest.json"
    manifest = {}
    if manifest_path.exists():
        with open(manifest_path) as f:
            manifest = json.load(f)

    print(f"Extracting attestation timing: {args.start_date} to {args.end_date}")
    print(f"Output: {output_dir}")
    if hours:
        print(f"Hours: {hours}")
    if args.dry_run:
        print("DRY RUN - no data will be written")
    print()

    current = start
    while current <= end:
        meta = process_day(current, output_dir, hours=hours, dry_run=args.dry_run)
        if meta["status"] == "ok":
            manifest[meta["date"]] = meta
            with open(manifest_path, "w") as f:
                json.dump(manifest, f, indent=2, sort_keys=True)
        current += timedelta(days=1)

    print(f"\nDone. Manifest: {manifest_path}")


if __name__ == "__main__":
    main()
