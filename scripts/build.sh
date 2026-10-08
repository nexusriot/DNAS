#!/usr/bin/env bash
# Cross-compile the DNAS binaries (dnas + dnas-tui) for one or more platforms and
# package each as a .tar.gz under dist/.
#
#   scripts/build.sh                          # linux/amd64 + linux/arm64
#   PLATFORMS="linux/amd64" scripts/build.sh  # just one platform
#   VERSION=1.2.3 scripts/build.sh            # stamp an explicit version
#
# Binaries are built static (CGO disabled) so they run on any matching kernel.
#
# REPRODUCIBLE. Two builds of the same commit on two machines produce
# byte-identical tarballs, which is what makes a published checksum worth
# anything: without it, "here is the sha256 of the release" means only that
# somebody computed a hash of whatever they happened to build, and nobody else
# can check the binary came from the source. `make verify-reproducible` builds
# twice and compares.
#
# What that costs, specifically:
#   - -trimpath, so the build path is not baked into the binary;
#   - -buildid= cleared, because Go derives it from inputs that include the
#     toolchain's own paths;
#   - every file in the tarball given the same mtime (SOURCE_DATE_EPOCH, which
#     defaults to the commit date), the same uid/gid/owner names, and a sorted
#     order, because tar otherwise records the filesystem's directory order and
#     whoever's account ran the build;
#   - gzip -n, so the compressed stream carries no timestamp or filename.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="$(VERSION="${VERSION:-}" ./scripts/version.sh)"
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64}"
OUT="${OUT:-dist}"
GO="${GO:-go}"
# -buildid= is what makes the binary itself reproducible; the rest is version
# stamping and size.
LDFLAGS="-s -w -buildid= -X main.version=${VERSION}"

# SOURCE_DATE_EPOCH fixes every timestamp in the archive. The commit date is the
# right default: it is a property of the source, so it is the same for everyone
# building the same commit, which is exactly the property being chased.
if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH="$(git log -1 --pretty=%ct 2>/dev/null || echo 0)"
fi
export SOURCE_DATE_EPOCH

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

	# A deterministic archive: fixed mtimes, no owner names, sorted entries, and
	# a gzip stream with no timestamp of its own.
	find "$stage" -exec touch -h -d "@${SOURCE_DATE_EPOCH}" {} +
	tar --sort=name \
		--mtime="@${SOURCE_DATE_EPOCH}" \
		--owner=0 --group=0 --numeric-owner \
		--pax-option=exthdr.name=%d/PaxHeaders/%f,delete=atime,delete=ctime \
		-C "$OUT" -cf - "$name" | gzip -n -9 >"${OUT}/${name}.tar.gz"
	echo "    -> ${OUT}/${name}.tar.gz"
done

# A checksum file over everything, so one signature covers the whole release.
( cd "$OUT" && sha256sum *.tar.gz | LC_ALL=C sort -k2 >SHA256SUMS )
echo "wrote ${OUT}/SHA256SUMS"

# Sign it if a signing key is configured. Unsigned is the default and is said
# out loud rather than left to be noticed: a checksum file anyone can rewrite
# proves nothing about who built the release.
if [ -n "${DNAS_SIGN_KEY:-}" ]; then
	if command -v gpg >/dev/null 2>&1; then
		gpg --batch --yes --local-user "$DNAS_SIGN_KEY" \
			--detach-sign --armor --output "${OUT}/SHA256SUMS.asc" "${OUT}/SHA256SUMS"
		echo "signed ${OUT}/SHA256SUMS.asc (key ${DNAS_SIGN_KEY})"
	else
		echo "DNAS_SIGN_KEY is set but gpg is not installed; release is UNSIGNED" >&2
		exit 1
	fi
else
	echo "note: release is UNSIGNED. Set DNAS_SIGN_KEY=<gpg key id> to sign SHA256SUMS."
fi

echo "done. artifacts in ${OUT}/:"
ls -1 "${OUT}"/*.tar.gz "${OUT}"/SHA256SUMS* 2>/dev/null || true
