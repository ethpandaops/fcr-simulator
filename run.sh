#!/usr/bin/env bash
# fcr-simulator chunked runner (v3, orchestrator + engine binary).
#
# Splits [start-epoch, end-epoch) into chunks of CHUNK_SIZE epochs, runs the
# orchestrator on each chunk so a mid-run failure doesn't lose all progress.
# Completed chunk outputs persist on disk; the script merges them into
# results/merged.csv.

set -euo pipefail

usage() {
    cat >&2 <<USAGE
Usage: $0 \
    --start-epoch START \
    --end-epoch END \
    --beacon-node-url URL \
    [--engine NAME]               (default: lighthouse)
    [--engine-binary PATH]        (default: \$FCR_ENGINE_BINARY or ./results/fcr-lighthouse)
    [--orchestrator PATH]         (default: ./results/fcr-orchestrator)
    [--warmup-epochs N]           (default: 10)
    [--parallel WORKERS]          (default: 2)
    [--chunk-size EPOCHS]         (default: 1000)
    [--cache-dir DIR]             (default: \$HOME/.cache/fcr-simulator)
    [--output-dir DIR]            (default: ./results)
    [--attestation-source-mode M] (default: next-non-missed)
    [--lookahead-cap N]           (default: 4)
USAGE
    exit 1
}

CACHE_DIR="$HOME/.cache/fcr-simulator"
OUTPUT_DIR="./results"
CHUNK_SIZE=1000
PARALLEL=2
WARMUP_EPOCHS=10
ENGINE="lighthouse"
ENGINE_BINARY="${FCR_ENGINE_BINARY:-./results/fcr-lighthouse}"
ORCHESTRATOR="./results/fcr-orchestrator"
ATT_MODE="next-non-missed"
LOOKAHEAD_CAP=4
CSV_SCHEMA_HEADER="# fcr-simulator-csv-schema-version:3"

while [[ $# -gt 0 ]]; do
    case $1 in
        --start-epoch) START_EPOCH="$2"; shift 2 ;;
        --end-epoch) END_EPOCH="$2"; shift 2 ;;
        --parallel) PARALLEL="$2"; shift 2 ;;
        --chunk-size) CHUNK_SIZE="$2"; shift 2 ;;
        --beacon-node-url) BEACON_NODE_URL="$2"; shift 2 ;;
        --cache-dir) CACHE_DIR="$2"; shift 2 ;;
        --output-dir) OUTPUT_DIR="$2"; shift 2 ;;
        --engine) ENGINE="$2"; shift 2 ;;
        --engine-binary) ENGINE_BINARY="$2"; shift 2 ;;
        --orchestrator) ORCHESTRATOR="$2"; shift 2 ;;
        --warmup-epochs) WARMUP_EPOCHS="$2"; shift 2 ;;
        --attestation-source-mode) ATT_MODE="$2"; shift 2 ;;
        --lookahead-cap) LOOKAHEAD_CAP="$2"; shift 2 ;;
        *) echo "Unknown option: $1" >&2; usage ;;
    esac
done

if [[ -z "${START_EPOCH:-}" || -z "${END_EPOCH:-}" || -z "${BEACON_NODE_URL:-}" ]]; then
    usage
fi

mkdir -p "$OUTPUT_DIR"
MERGED="$OUTPUT_DIR/merged.csv"

csv_schema_valid() {
    local f="$1"
    [[ -f "$f" ]] || return 1
    local first
    IFS= read -r first < "$f" || return 1
    [[ "$first" == "$CSV_SCHEMA_HEADER" ]]
}

chunk_complete() {
    local chunk_file="$1"
    local start_epoch="$2"
    local end_epoch="$3"
    local manifest="${chunk_file%.csv}.manifest.json"
    [[ -f "$chunk_file" && -f "$manifest" ]] || return 1
    csv_schema_valid "$chunk_file" || return 1
    # Sanity: count of data rows should equal (end - start) * 32 slots.
    local expected=$(( (end_epoch - start_epoch) * 32 ))
    local actual=$(( $(wc -l < "$chunk_file") - 2 ))  # subtract schema marker + header row
    [[ "$actual" -eq "$expected" ]]
}

merge_chunks() {
    local header_written=false
    : > "$MERGED.tmp"
    for f in "$OUTPUT_DIR"/chunk-*.csv; do
        [[ -f "$f" ]] || continue

        if ! csv_schema_valid "$f"; then
            echo "  Skipping $f: schema marker missing or wrong version" >&2
            continue
        fi

        if [[ "$header_written" == false ]]; then
            sed -n '1,2p' "$f" >> "$MERGED.tmp"
            header_written=true
        fi
        tail -n +3 "$f" >> "$MERGED.tmp"
    done

    if [[ -s "$MERGED.tmp" ]]; then
        mv "$MERGED.tmp" "$MERGED"
        local total
        total=$(($(wc -l < "$MERGED") - 2))
        echo "  Merged: $total slots into $MERGED"
    else
        rm -f "$MERGED.tmp"
    fi
}

TOTAL_EPOCHS=$((END_EPOCH - START_EPOCH))
TOTAL_CHUNKS=$(( (TOTAL_EPOCHS + CHUNK_SIZE - 1) / CHUNK_SIZE ))

echo "=== FCR Simulator (v3 / orchestrator) ==="
echo "Engine:           $ENGINE ($ENGINE_BINARY)"
echo "Orchestrator:     $ORCHESTRATOR"
echo "Range:            epochs $START_EPOCH..$END_EPOCH ($TOTAL_EPOCHS epochs)"
echo "Chunk size:       $CHUNK_SIZE epochs ($TOTAL_CHUNKS chunks)"
echo "Workers/chunk:    $PARALLEL"
echo "Warmup:           $WARMUP_EPOCHS epochs"
echo "Source mode:      $ATT_MODE (cap=$LOOKAHEAD_CAP)"
echo "Cache:            $CACHE_DIR"
echo "Output:           $OUTPUT_DIR"
echo ""

CURSOR=$START_EPOCH
COMPLETED=0
FAILED=0

while [[ $CURSOR -lt $END_EPOCH ]]; do
    CHUNK_END=$((CURSOR + CHUNK_SIZE))
    if [[ $CHUNK_END -gt $END_EPOCH ]]; then
        CHUNK_END=$END_EPOCH
    fi

    CHUNK_NUM=$((COMPLETED + FAILED + 1))
    CHUNK_FILE="$OUTPUT_DIR/chunk-${CURSOR}-${CHUNK_END}.csv"

    if chunk_complete "$CHUNK_FILE" "$CURSOR" "$CHUNK_END"; then
        echo "[$CHUNK_NUM/$TOTAL_CHUNKS] chunk $CURSOR..$CHUNK_END already complete, skipping"
        COMPLETED=$((COMPLETED + 1))
        CURSOR=$CHUNK_END
        continue
    fi

    echo "[$CHUNK_NUM/$TOTAL_CHUNKS] running chunk $CURSOR..$CHUNK_END"

    if "$ORCHESTRATOR" \
        --engine "$ENGINE" \
        --engine-binary "$ENGINE_BINARY" \
        --network mainnet \
        --start-epoch "$CURSOR" \
        --end-epoch "$CHUNK_END" \
        --warmup-epochs "$WARMUP_EPOCHS" \
        --parallel "$PARALLEL" \
        --beacon-node-url "$BEACON_NODE_URL" \
        --output "$CHUNK_FILE" \
        --output-format both \
        --attestation-source-mode "$ATT_MODE" \
        --lookahead-cap "$LOOKAHEAD_CAP" \
        --cache-dir "$CACHE_DIR"; then
        COMPLETED=$((COMPLETED + 1))
        echo "[$CHUNK_NUM/$TOTAL_CHUNKS] chunk complete"
    else
        FAILED=$((FAILED + 1))
        echo "[$CHUNK_NUM/$TOTAL_CHUNKS] chunk FAILED (exit $?)"
    fi

    merge_chunks
    CURSOR=$CHUNK_END
done

merge_chunks

echo ""
echo "=== Summary ==="
echo "Completed: $COMPLETED / $TOTAL_CHUNKS chunks"
echo "Failed:    $FAILED"
