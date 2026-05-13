#!/bin/bash
set -euo pipefail

# FCR Simulator chunked runner
# Runs the simulator in chunks so crashes don't lose all progress.
# Completed chunk CSVs persist on disk regardless of later failures.

usage() {
    echo "Usage: $0 --start-epoch START --end-epoch END --parallel WORKERS --chunk-size EPOCHS --beacon-node-url URL [--cache-dir DIR] [--output-dir DIR]"
    exit 1
}

# Defaults
CACHE_DIR="$HOME/.cache/fcr-simulator"
OUTPUT_DIR="./results"
CHUNK_SIZE=1000
PARALLEL=2
BINARY="./lighthouse/target/release/fcr-simulator"
CSV_SCHEMA_HEADER="# fcr-simulator-csv-schema-version:2"

while [[ $# -gt 0 ]]; do
    case $1 in
        --start-epoch) START_EPOCH="$2"; shift 2 ;;
        --end-epoch) END_EPOCH="$2"; shift 2 ;;
        --parallel) PARALLEL="$2"; shift 2 ;;
        --chunk-size) CHUNK_SIZE="$2"; shift 2 ;;
        --beacon-node-url) BEACON_NODE_URL="$2"; shift 2 ;;
        --cache-dir) CACHE_DIR="$2"; shift 2 ;;
        --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
        --binary) BINARY="$2"; shift 2 ;;
        *) echo "Unknown option: $1"; usage ;;
    esac
done

if [[ -z "${START_EPOCH:-}" || -z "${END_EPOCH:-}" || -z "${BEACON_NODE_URL:-}" ]]; then
    usage
fi

mkdir -p "$OUTPUT_DIR"

MERGED="$OUTPUT_DIR/merged.csv"

chunk_manifest_complete() {
    local chunk_file="$1"
    local start_epoch="$2"
    local end_epoch="$3"
    local manifest="${chunk_file}.manifest.json"
    local start_slot=$((start_epoch * 32))
    local end_slot=$((end_epoch * 32))

    [[ -f "$chunk_file" && -f "$manifest" ]] || return 1

    python3 - "$manifest" "$start_slot" "$end_slot" <<'PY'
import json
import sys

manifest_path, expected_start, expected_end = sys.argv[1], int(sys.argv[2]), int(sys.argv[3])
with open(manifest_path) as f:
    manifest = json.load(f)

if manifest.get("schema_version") != 1:
    sys.exit(1)
if manifest.get("partial"):
    sys.exit(1)

ranges = sorted(
    (int(r["start_slot"]), int(r["end_slot"]))
    for r in manifest.get("completed_ranges", [])
)
cursor = expected_start
for start, end in ranges:
    if start != cursor or end < start:
        sys.exit(1)
    cursor = end

if cursor != expected_end:
    sys.exit(1)
PY
}

csv_schema_valid() {
    local csv_file="$1"
    local first_line

    [[ -f "$csv_file" ]] || return 1
    IFS= read -r first_line < "$csv_file" || return 1
    [[ "$first_line" == "$CSV_SCHEMA_HEADER" ]]
}

chunk_epochs_from_path() {
    local path="$1"
    local base
    base=$(basename "$path")
    [[ "$base" =~ ^chunk-([0-9]+)-([0-9]+)\.csv$ ]] || return 1
    echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]}"
}

merge_chunks() {
    HEADER_WRITTEN=false
    for f in "$OUTPUT_DIR"/chunk-*.csv; do
        [[ -f "$f" ]] || continue
        [[ "$f" == *worker* ]] && continue

        if ! read -r chunk_start chunk_end < <(chunk_epochs_from_path "$f"); then
            echo "  Skipping $f: cannot parse chunk range"
            continue
        fi
        if ! chunk_manifest_complete "$f" "$chunk_start" "$chunk_end"; then
            echo "  Skipping $f: manifest is missing or incomplete"
            continue
        fi
        if ! csv_schema_valid "$f"; then
            echo "  Skipping $f: CSV schema header is missing or unsupported"
            continue
        fi

        if [[ "$HEADER_WRITTEN" == false ]]; then
            sed -n '1,2p' "$f" > "$MERGED.tmp"
            HEADER_WRITTEN=true
        fi
        tail -n +3 "$f" >> "$MERGED.tmp"
    done

    if [[ -f "$MERGED.tmp" ]]; then
        mv "$MERGED.tmp" "$MERGED"
        TOTAL_SLOTS=$(($(wc -l < "$MERGED") - 2))
        CONFIRMED=$(awk -F, '
            NR == 2 {
                for (i = 1; i <= NF; i++) {
                    if ($i == "fast_confirmed") {
                        fast_confirmed_col = i
                    }
                }
                next
            }
            NR > 2 && fast_confirmed_col && $fast_confirmed_col == "true"
        ' "$MERGED" | wc -l)
        if [[ $TOTAL_SLOTS -gt 0 ]]; then
            RATE=$(echo "scale=2; $CONFIRMED * 100 / $TOTAL_SLOTS" | bc)
            echo "  Merged: $TOTAL_SLOTS slots, $CONFIRMED confirmed ($RATE%)"
        fi
    fi
}

TOTAL_EPOCHS=$((END_EPOCH - START_EPOCH))
TOTAL_CHUNKS=$(( (TOTAL_EPOCHS + CHUNK_SIZE - 1) / CHUNK_SIZE ))

echo "=== FCR Simulator Chunked Run ==="
echo "Range: epoch $START_EPOCH - $END_EPOCH ($TOTAL_EPOCHS epochs)"
echo "Chunk size: $CHUNK_SIZE epochs"
echo "Total chunks: $TOTAL_CHUNKS"
echo "Workers per chunk: $PARALLEL"
echo "Output dir: $OUTPUT_DIR"
echo ""

COMPLETED=0
FAILED=0
CURSOR=$START_EPOCH

while [[ $CURSOR -lt $END_EPOCH ]]; do
    CHUNK_END=$((CURSOR + CHUNK_SIZE))
    if [[ $CHUNK_END -gt $END_EPOCH ]]; then
        CHUNK_END=$END_EPOCH
    fi

    CHUNK_NUM=$((COMPLETED + FAILED + 1))
    CHUNK_FILE="$OUTPUT_DIR/chunk-${CURSOR}-${CHUNK_END}.csv"

    # Skip if this chunk already completed
    if chunk_manifest_complete "$CHUNK_FILE" "$CURSOR" "$CHUNK_END"; then
        LINES=$(wc -l < "$CHUNK_FILE")
        echo "[$CHUNK_NUM/$TOTAL_CHUNKS] Chunk $CURSOR-$CHUNK_END already complete ($LINES lines), skipping"
        COMPLETED=$((COMPLETED + 1))
        CURSOR=$CHUNK_END
        continue
    fi

    echo "[$CHUNK_NUM/$TOTAL_CHUNKS] Running chunk: epoch $CURSOR - $CHUNK_END"

    if "$BINARY" \
        --beacon-node-url "$BEACON_NODE_URL" \
        --start-epoch "$CURSOR" \
        --end-epoch "$CHUNK_END" \
        --output "$CHUNK_FILE" \
        --cache-dir "$CACHE_DIR" \
        --parallel "$PARALLEL"; then
        COMPLETED=$((COMPLETED + 1))
        echo "[$CHUNK_NUM/$TOTAL_CHUNKS] Chunk complete"
    else
        FAILED=$((FAILED + 1))
        echo "[$CHUNK_NUM/$TOTAL_CHUNKS] Chunk FAILED (exit code $?)"
        # Salvage partial data from worker files if chunk CSV wasn't produced
        if [[ ! -f "$CHUNK_FILE" ]]; then
            WORKER_FILES=("$OUTPUT_DIR"/chunk-${CURSOR}-${CHUNK_END}.worker-*.csv)
            if [[ -f "${WORKER_FILES[0]}" ]]; then
                echo "  Salvaging data from worker files..."
                sed -n '1,2p' "${WORKER_FILES[0]}" > "$CHUNK_FILE"
                for wf in "${WORKER_FILES[@]}"; do
                    tail -n +3 "$wf" >> "$CHUNK_FILE"
                done
                SALVAGED=$(($(wc -l < "$CHUNK_FILE") - 2))
                echo "  Salvaged $SALVAGED slots from worker files"
            fi
        fi
    fi

    # Merge after every chunk so we always have usable data
    merge_chunks

    CURSOR=$CHUNK_END
done

echo ""
echo "=== Summary ==="
echo "Completed: $COMPLETED / $TOTAL_CHUNKS chunks"
echo "Failed: $FAILED"

# Final merge
merge_chunks
