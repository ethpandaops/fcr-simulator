#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
ENGINE_DIR="$ROOT/engines/teku"
TEKU_SUBMODULE="$ENGINE_DIR/teku"
BUILD_SRC="$ENGINE_DIR/.build/teku"
DIST_DIR="$ENGINE_DIR/.build/dist"
RESULTS_DIR="$ROOT/results"
TEKU_SHA="c5825d53325cd67ab91b35cc544a7b660be317ff"

if [[ -z "${JAVA_HOME:-}" ]]; then
  if [[ -d /opt/homebrew/opt/openjdk@21 ]]; then
    export JAVA_HOME=/opt/homebrew/opt/openjdk@21
  elif [[ -d /usr/lib/jvm/java-21-openjdk-amd64 ]]; then
    export JAVA_HOME=/usr/lib/jvm/java-21-openjdk-amd64
  else
    echo "JAVA_HOME is unset and no JDK 21 was found at /opt/homebrew/opt/openjdk@21 or /usr/lib/jvm/java-21-openjdk-amd64" >&2
    exit 1
  fi
fi

if [[ ! -d "$TEKU_SUBMODULE/.git" && ! -f "$TEKU_SUBMODULE/.git" ]]; then
  git -C "$ROOT" submodule update --init --recursive "$TEKU_SUBMODULE"
fi

if [[ "$(git -C "$TEKU_SUBMODULE" rev-parse HEAD)" != "$TEKU_SHA" ]]; then
  git -C "$TEKU_SUBMODULE" fetch origin confirmation-2
  git -C "$TEKU_SUBMODULE" checkout "$TEKU_SHA"
fi

rm -rf "$BUILD_SRC"
mkdir -p "$(dirname "$BUILD_SRC")" "$DIST_DIR" "$RESULTS_DIR"
git clone --no-hardlinks "$TEKU_SUBMODULE" "$BUILD_SRC"
git -C "$BUILD_SRC" checkout --detach "$TEKU_SHA"

shopt -s nullglob
for patch in "$ENGINE_DIR"/patches/*.patch; do
  git -C "$BUILD_SRC" apply --check "$patch"
  git -C "$BUILD_SRC" apply "$patch"
done
shopt -u nullglob

GRADLE_ARGS=(${GRADLE_ARGS:-})
export GRADLE_USER_HOME="${GRADLE_USER_HOME:-$ENGINE_DIR/.gradle-home}"
(cd "$BUILD_SRC" && ./gradlew "${GRADLE_ARGS[@]}" :fcr-simulator-engine:shadowJar)

cp "$BUILD_SRC/fcr-simulator-engine/build/libs/fcr-teku-all.jar" "$DIST_DIR/fcr-teku-all.jar"

cat > "$RESULTS_DIR/fcr-teku" <<'LAUNCHER'
#!/usr/bin/env sh
set -e
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
JAR="$SELF_DIR/../engines/teku/.build/dist/fcr-teku-all.jar"
if [ -n "${JAVA_HOME:-}" ] && [ -x "$JAVA_HOME/bin/java" ]; then
  JAVA="$JAVA_HOME/bin/java"
elif [ -x /opt/homebrew/opt/openjdk@21/bin/java ]; then
  JAVA=/opt/homebrew/opt/openjdk@21/bin/java
elif [ -x /usr/lib/jvm/java-21-openjdk-amd64/bin/java ]; then
  JAVA=/usr/lib/jvm/java-21-openjdk-amd64/bin/java
elif command -v java >/dev/null 2>&1; then
  JAVA=java
else
  echo "fcr-teku: no Java runtime found. Install JDK 21 or set JAVA_HOME." >&2
  exit 1
fi
exec "$JAVA" -jar "$JAR" "$@"
LAUNCHER
chmod +x "$RESULTS_DIR/fcr-teku"

echo "wrote $RESULTS_DIR/fcr-teku"
