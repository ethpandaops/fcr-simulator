#!/usr/bin/env bash
# Builds the fcr-lodestar engine.
#
# The engine itself is a pnpm workspace that vendors Lodestar at a pinned SHA
# as nested workspace packages. After installing and building those workspaces
# (params, utils, types, config, state-transition, fork-choice), we esbuild
# src/main.ts into a non-bundled ESM module. The published "binary" is a small
# shim script at <repo>/results/fcr-lodestar that execs node on the built JS.

set -euo pipefail

ENGINE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${ENGINE_DIR}/../.." && pwd)"
LODESTAR_DIR="${ENGINE_DIR}/lodestar"
RESULTS_DIR="${REPO_ROOT}/results"
SHIM_PATH="${RESULTS_DIR}/fcr-lodestar"

if [[ ! -d "${LODESTAR_DIR}/.git" && ! -f "${LODESTAR_DIR}/.git" ]]; then
  echo "lodestar submodule missing at ${LODESTAR_DIR}; run git submodule update --init --recursive" >&2
  exit 1
fi

if ! command -v node >/dev/null 2>&1; then
  echo "node not found on PATH" >&2
  exit 1
fi

NODE_MAJOR="$(node -p 'process.versions.node.split(".")[0]')"
if [[ "${NODE_MAJOR}" -lt 24 ]]; then
  echo "node >=24.13.0 required (have $(node --version))" >&2
  exit 1
fi

if ! command -v corepack >/dev/null 2>&1; then
  echo "corepack not found on PATH" >&2
  exit 1
fi
PNPM=(corepack pnpm)

if LODESTAR_COMMIT="$(git -C "${LODESTAR_DIR}" rev-parse HEAD 2>/dev/null)"; then
  LODESTAR_DESCRIBE="$(git -C "${LODESTAR_DIR}" describe --always --tags 2>/dev/null || echo "${LODESTAR_COMMIT:0:8}")"
elif [[ -n "${FCR_LODESTAR_COMMIT:-}" ]]; then
  LODESTAR_COMMIT="${FCR_LODESTAR_COMMIT}"
  LODESTAR_DESCRIBE="${FCR_LODESTAR_DESCRIBE:-${LODESTAR_COMMIT:0:8}}"
else
  echo "lodestar git metadata unavailable at ${LODESTAR_DIR}; set FCR_LODESTAR_COMMIT when building from a worktree Docker context" >&2
  exit 1
fi

echo "[fcr-lodestar] installing engine + lodestar workspaces (pinned ${LODESTAR_COMMIT})" >&2
(
  cd "${ENGINE_DIR}"
  "${PNPM[@]}" install --frozen-lockfile=false
)

echo "[fcr-lodestar] building lodestar packages we depend on" >&2
(
  cd "${ENGINE_DIR}/lodestar"
  PATH="${ENGINE_DIR}/lodestar/node_modules/.bin:${PATH}" "${PNPM[@]}" \
    --filter '@lodestar/params' \
    --filter '@lodestar/utils' \
    --filter '@lodestar/types' \
    --filter '@lodestar/api' \
    --filter '@lodestar/db' \
    --filter '@lodestar/config' \
    --filter '@lodestar/state-transition' \
    --filter '@lodestar/fork-choice' \
    build
)

echo "[fcr-lodestar] writing src/version.ts" >&2
cat > "${ENGINE_DIR}/src/version.ts" <<EOF
export const LODESTAR_VERSION = "${LODESTAR_DESCRIBE}";
export const LODESTAR_COMMIT = "${LODESTAR_COMMIT}";
EOF

echo "[fcr-lodestar] compiling main.ts" >&2
(
  cd "${ENGINE_DIR}"
  rm -rf dist
  mkdir -p dist
  node_modules/.bin/esbuild src/main.ts \
    --bundle \
    --platform=node \
    --format=esm \
    --target=node24 \
    --outfile=dist/main.mjs \
    --sourcemap \
    --packages=external \
    --log-level=warning
)

mkdir -p "${RESULTS_DIR}"
cat > "${SHIM_PATH}" <<EOF
#!/usr/bin/env bash
# fcr-lodestar shim — invokes the compiled Lodestar engine.
exec node --enable-source-maps --max-old-space-size=12288 "${ENGINE_DIR}/dist/main.mjs" "\$@"
EOF
chmod +x "${SHIM_PATH}"

echo "[fcr-lodestar] built ${SHIM_PATH}" >&2
