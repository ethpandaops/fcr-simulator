#!/usr/bin/env bash
# Fast contract validation: call <binary> --manifest-json on every engine binary
# under results/fcr-* and verify it satisfies the registry's RequiredBuildFlags.
# Useful as a precondition for a real smoke run.
#
# Exits 0 only if every engine binary passes.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ORCHESTRATOR="${ROOT}/results/fcr-orchestrator"
if [[ ! -x "$ORCHESTRATOR" ]]; then
    echo "missing $ORCHESTRATOR; run: go build -o $ORCHESTRATOR ./cmd/fcr-orchestrator" >&2
    exit 2
fi

shopt -s nullglob
binaries=( "${ROOT}"/results/fcr-* )
binaries=( $(printf "%s\n" "${binaries[@]}" | grep -v "fcr-orchestrator$" || true) )

if [[ ${#binaries[@]} -eq 0 ]]; then
    echo "no fcr-<engine> binaries found under ${ROOT}/results/" >&2
    exit 2
fi

pass=0
fail=0
for bin in "${binaries[@]}"; do
    engine="${bin##*/fcr-}"
    printf "%-12s " "$engine"

    json=$("$bin" --manifest-json 2>/dev/null || true)
    if [[ -z "$json" ]]; then
        echo "FAIL: --manifest-json produced no output"
        fail=$((fail+1))
        continue
    fi

    if ! python3 -c "import json,sys; m=json.loads(sys.argv[1]); assert m['engine_name']=='$engine', f'name {m[\"engine_name\"]} != $engine'; assert 'fake_crypto' in m['build_flags'], 'fake_crypto missing'; assert m['engine_version'], 'engine_version empty'" "$json" 2>/dev/null; then
        echo "FAIL: manifest invalid"
        echo "  $json" | head -c 200
        echo
        fail=$((fail+1))
        continue
    fi

    version=$(python3 -c "import json,sys; print(json.loads(sys.argv[1])['engine_version'])" "$json")
    echo "OK   v=$version"
    pass=$((pass+1))
done

echo
echo "manifest-check: $pass/$((pass+fail)) engines pass"
exit $(( fail > 0 ? 1 : 0 ))
