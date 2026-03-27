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

merge_chunks() {
    HEADER_WRITTEN=false
    for f in "$OUTPUT_DIR"/chunk-*.csv; do
        [[ -f "$f" ]] || continue
        [[ "$f" == *worker* ]] && continue

        if [[ "$HEADER_WRITTEN" == false ]]; then
            head -1 "$f" > "$MERGED.tmp"
            HEADER_WRITTEN=true
        fi
        tail -n +2 "$f" >> "$MERGED.tmp"
    done

    if [[ -f "$MERGED.tmp" ]]; then
        mv "$MERGED.tmp" "$MERGED"
        TOTAL_SLOTS=$(($(wc -l < "$MERGED") - 1))
        CONFIRMED=$(awk -F, 'NR>1 && $5=="true"' "$MERGED" | wc -l)
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
    if [[ -f "$CHUNK_FILE" ]]; then
        LINES=$(wc -l < "$CHUNK_FILE")
        EXPECTED_SLOTS=$(( (CHUNK_END - CURSOR) * 32 ))
        # Header + at least 90% of expected slots = good enough
        if [[ $LINES -gt $((EXPECTED_SLOTS * 9 / 10)) ]]; then
            echo "[$CHUNK_NUM/$TOTAL_CHUNKS] Chunk $CURSOR-$CHUNK_END already complete ($LINES lines), skipping"
            COMPLETED=$((COMPLETED + 1))
            CURSOR=$CHUNK_END
            continue
        fi
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
        echo "  Worker CSVs may be at: $OUTPUT_DIR/chunk-${CURSOR}-${CHUNK_END}.worker-*.csv"
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
