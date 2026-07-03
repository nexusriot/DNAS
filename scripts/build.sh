#!/usr/bin/env bash
# Cross-compile the DNAS binaries (dnas + dnas-tui) for one or more platforms and
# package each as a .tar.gz under dist/.
#
#   scripts/build.sh                          # linux/amd64 + linux/arm64
#   PLATFORMS="linux/amd64" scripts/build.sh  # just one platform
#   VERSION=1.2.3 scripts/build.sh            # stamp an explicit version
#
# Binaries are built static (CGO disabled) so they run on any matching kernel.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)}"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64}"
OUT="${OUT:-dist}"
GO="${GO:-go}"
LDFLAGS="-s -w -X main.version=${VERSION}"

echo "building dnas ${VERSION} for: ${PLATFORMS}"
mkdir -p "$OUT"

for platform in $PLATFORMS; do
	os="${platform%%/*}"; arch="${platform##*/}"
	name="dnas_${VERSION}_${os}_${arch}"
	stage="${OUT}/${name}"
	ext=""; [ "$os" = "windows" ] && ext=".exe"
	echo "==> ${os}/${arch}"
	rm -rf "$stage"; mkdir -p "$stage"

	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		"$GO" build -trimpath -ldflags "$LDFLAGS" -o "${stage}/dnas${ext}" ./cmd/dnas

	# The TUI is its own module (external deps), so build it from its directory.
	( cd tui && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
		"$GO" build -trimpath -ldflags "$LDFLAGS" -o "../${stage}/dnas-tui${ext}" . )

	# Bundle the Python GUI and docs alongside the binaries.
	cp gui/dnas_gui.py "$stage/"
	cp README.md QUICKSTART.md "$stage/" 2>/dev/null || true

	tar -C "$OUT" -czf "${OUT}/${name}.tar.gz" "$name"
	echo "    -> ${OUT}/${name}.tar.gz"
done

echo "done. artifacts in ${OUT}/:"
ls -1 "${OUT}"/*.tar.gz 2>/dev/null || true
