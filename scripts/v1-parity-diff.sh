#!/usr/bin/env bash
# Compare OLD (v2 schema, in-process) vs NEW (v3 schema, orchestrator+engine) outputs
# on the consensus-meaningful columns only.
#
# Usage: v1-parity-diff.sh <old.csv> <new.csv>

set -euo pipefail

OLD="${1:?old csv}"
NEW="${2:?new csv}"

# Columns we compare. Auto-narrowed to the intersection of OLD and NEW headers.
# Below is the maximal common set; the script will drop any not present in OLD.
COMMON_COLS=(
  slot epoch has_block block_root head_root
  confirmed_root confirmed_slot confirmation_delay_slots
  fast_confirmed strict_one_slot_confirmed
  finalized_epoch justified_epoch
  num_attestations_injected is_epoch_boundary is_missed_slot
)

project_csv() {
  local file="$1"
  local intersect="$2"
  python3 - "$file" "$intersect" <<'PY'
import csv, sys
path, intersect = sys.argv[1], sys.argv[2].split(",")
with open(path) as f:
    first = f.readline()
    if not first.startswith("#"):
        f.seek(0)
    reader = csv.DictReader(f)
    print(",".join(intersect))
    rows = list(reader)
    rows.sort(key=lambda r: int(r["slot"]))
    for row in rows:
        out = []
        for c in intersect:
            v = row.get(c, "")
            if c == "block_root" and v in ("null", "None", "<nil>"):
                v = ""
            out.append(v)
        print(",".join(out))
PY
}

header_of() {
  local file="$1"
  python3 - "$file" <<'PY'
import sys
with open(sys.argv[1]) as f:
    line = f.readline()
    if line.startswith("#"):
        line = f.readline()
    print(line.strip())
PY
}

OLD_HDR=$(header_of "$OLD")
NEW_HDR=$(header_of "$NEW")
IFS=',' read -ra OLD_COLS <<< "$OLD_HDR"
IFS=',' read -ra NEW_COLS <<< "$NEW_HDR"

INTERSECT=""
for c in "${COMMON_COLS[@]}"; do
  in_old=0; in_new=0
  for o in "${OLD_COLS[@]}"; do [ "$o" = "$c" ] && in_old=1; done
  for n in "${NEW_COLS[@]}"; do [ "$n" = "$c" ] && in_new=1; done
  if [ "$in_old" = "1" ] && [ "$in_new" = "1" ]; then
    INTERSECT+="$c,"
  fi
done
INTERSECT="${INTERSECT%,}"

echo "Comparing on intersection of common columns: $INTERSECT"
echo ""

OLD_TMP=$(mktemp)
NEW_TMP=$(mktemp)
trap "rm -f $OLD_TMP $NEW_TMP" EXIT

project_csv "$OLD" "$INTERSECT" > "$OLD_TMP"
project_csv "$NEW" "$INTERSECT" > "$NEW_TMP"

echo "Diffing $OLD vs $NEW (projected to consensus columns)..."
echo ""

if diff -u "$OLD_TMP" "$NEW_TMP"; then
  echo ""
  echo "=== PARITY HOLDS ==="
  echo "All $(($(wc -l < "$NEW_TMP") - 1)) recorded slots agree on all consensus columns."
  exit 0
else
  echo ""
  echo "=== PARITY BROKEN ==="
  echo "First divergent slot above is where harness behavior differs."
  exit 1
fi
