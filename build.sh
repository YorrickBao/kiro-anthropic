#!/usr/bin/env bash
#
# build.sh — cross-compile kiro-anthropic for common platforms and package the
# results (archives + checksums) into an artifacts directory.
#
# Run "./build.sh --help" for usage, options, environment variables and examples.
#
set -euo pipefail

cd "$(dirname "$0")"

APP="kiro-anthropic"
DIST="${DIST:-dist}"

# Default platform matrix (also shown by --help). Override by passing
# "os/arch" arguments on the command line.
DEFAULT_PLATFORMS=(
  darwin/amd64
  darwin/arm64
  linux/amd64
  linux/arm64
  linux/386
  windows/amd64
  windows/arm64
)

usage() {
  local plats
  plats="$(printf '%s, ' "${DEFAULT_PLATFORMS[@]}")"
  plats="${plats%, }"
  cat <<EOF
${APP} build script — cross-compile and package release artifacts.

USAGE:
  ./build.sh [options] [platform ...]

ARGUMENTS:
  platform        One or more "os/arch" targets to build (e.g. linux/amd64).
                  If omitted, the default platform matrix is built.

OPTIONS:
  -h, --help      Show this help and exit.

ENVIRONMENT:
  VERSION         Version string stamped into the binary via -ldflags
                  (default: \`git describe --tags --always --dirty\`, else "dev").
  DIST            Output directory for artifacts (default: dist).
  SKIP_TESTS      Set to 1 to skip the "go test ./..." gate before building.

DEFAULT PLATFORMS:
  ${plats}

OUTPUT (written to \$DIST):
  ${APP}_<version>_<os>_<arch>.tar.gz   Unix archives (contain the binary)
  ${APP}_<version>_<os>_<arch>.zip      Windows archives (contain ${APP}.exe)
  checksums.txt                          SHA-256 of every archive

EXAMPLES:
  ./build.sh                             Build the default matrix into ./dist
  VERSION=1.0.0 ./build.sh               Stamp version 1.0.0
  DIST=artifacts ./build.sh              Write artifacts to ./artifacts
  ./build.sh linux/amd64 darwin/arm64    Build only these two targets
EOF
}

# Handle help before doing any real work (fast, needs no git/toolchain).
for arg in "$@"; do
  case "$arg" in
    -h | --help | help)
      usage
      exit 0
      ;;
  esac
done

# --- unit tests (gate the build; skip with SKIP_TESTS=1) -------------------
if [ "${SKIP_TESTS:-0}" != "1" ]; then
  echo "==> go test ./..."
  if ! go test ./...; then
    echo "!! tests failed; aborting build (set SKIP_TESTS=1 to bypass)" >&2
    exit 1
  fi
fi

# --- version metadata ------------------------------------------------------
VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"

# --- platform matrix -------------------------------------------------------
# Use the platforms passed as arguments, otherwise the default matrix.
if [ "$#" -gt 0 ]; then
  PLATFORMS=("$@")
else
  PLATFORMS=("${DEFAULT_PLATFORMS[@]}")
fi

have_zip=0
command -v zip >/dev/null 2>&1 && have_zip=1

echo "==> ${APP} ${VERSION} (commit ${COMMIT}, ${DATE})"
echo "==> output: ${DIST}/"
rm -rf "${DIST}"
mkdir -p "${DIST}"

built=()
failed=()

for platform in "${PLATFORMS[@]}"; do
  GOOS="${platform%%/*}"
  GOARCH="${platform##*/}"
  if [ -z "$GOOS" ] || [ -z "$GOARCH" ] || [ "$GOOS" = "$platform" ]; then
    echo "  !! invalid platform '${platform}', expected os/arch" >&2
    failed+=("$platform")
    continue
  fi

  bin="${APP}"
  [ "$GOOS" = "windows" ] && bin="${APP}.exe"

  stage="$(mktemp -d)"
  printf '  %-16s ' "${GOOS}/${GOARCH}"

  if ! CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
      go build -trimpath -ldflags "$LDFLAGS" -o "${stage}/${bin}" . 2>"${stage}/err"; then
    echo "FAILED"
    sed 's/^/      /' "${stage}/err" >&2 || true
    failed+=("$platform")
    rm -rf "$stage"
    continue
  fi

  name="${APP}_${VERSION}_${GOOS}_${GOARCH}"
  if [ "$GOOS" = "windows" ] && [ "$have_zip" = 1 ]; then
    ( cd "$stage" && zip -q "${name}.zip" "$bin" )
    mv "${stage}/${name}.zip" "${DIST}/"
    artifact="${name}.zip"
  else
    tar -C "$stage" -czf "${DIST}/${name}.tar.gz" "$bin"
    artifact="${name}.tar.gz"
  fi
  built+=("$artifact")
  rm -rf "$stage"
  echo "ok  (${artifact})"
done

# --- checksums -------------------------------------------------------------
if [ "${#built[@]}" -gt 0 ]; then
  ( cd "$DIST"
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum "${built[@]}" > checksums.txt
    else
      shasum -a 256 "${built[@]}" > checksums.txt
    fi
  )
fi

echo
echo "==> artifacts in ${DIST}/:"
ls -lh "${DIST}"

if [ "${#failed[@]}" -gt 0 ]; then
  echo
  echo "!! failed platforms: ${failed[*]}" >&2
  exit 1
fi
