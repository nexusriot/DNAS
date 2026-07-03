#!/usr/bin/env bash
# Build Debian .deb packages for DNAS (amd64 and/or arm64) using dpkg-deb.
#
#   scripts/build-deb.sh                    # amd64 + arm64
#   ARCHES="amd64" scripts/build-deb.sh     # just one architecture
#   VERSION=1.2.3 scripts/build-deb.sh      # stamp an explicit version
#
# The package installs:
#   /usr/bin/dnas          the node / wallet / spv CLI
#   /usr/bin/dnas-tui      the terminal client
#   /usr/bin/dnas-gui      launcher for the PyQt desktop client
#   /usr/share/dnas/dnas_gui.py
set -euo pipefail
cd "$(dirname "$0")/.."

command -v dpkg-deb >/dev/null 2>&1 || { echo "dpkg-deb not found (install the dpkg package)"; exit 1; }

RAW_VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)}"
# Debian versions must start with a digit and use a restricted charset.
DEB_VERSION="$(printf '%s' "$RAW_VERSION" | sed -e 's/^v//' -e 's/[^A-Za-z0-9.+~]/~/g')"
case "$DEB_VERSION" in [0-9]*) ;; *) DEB_VERSION="0~${DEB_VERSION}" ;; esac

ARCHES="${ARCHES:-amd64 arm64}"
OUT="${OUT:-dist}"
GO="${GO:-go}"
MAINTAINER="${MAINTAINER:-DNAS <vananyev@hystax.com>}"
LDFLAGS="-s -w -X main.version=${RAW_VERSION}"

mkdir -p "$OUT"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

for arch in $ARCHES; do
	case "$arch" in
		amd64|arm64) ;;
		*) echo "unsupported arch: $arch (use amd64 or arm64)"; exit 1 ;;
	esac
	echo "==> packaging dnas ${DEB_VERSION} (${arch})"
	root="${WORK}/${arch}"
	install -d "$root/DEBIAN" "$root/usr/bin" "$root/usr/share/dnas" "$root/usr/share/doc/dnas"

	# Static binaries for the target architecture (pure-Go cross build).
	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
		"$GO" build -trimpath -ldflags "$LDFLAGS" -o "$root/usr/bin/dnas" ./cmd/dnas
	( cd tui && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" \
		"$GO" build -trimpath -ldflags "$LDFLAGS" -o "$root/usr/bin/dnas-tui" . )
	chmod 0755 "$root/usr/bin/dnas" "$root/usr/bin/dnas-tui" # normalize umask perms

	# Python GUI + a small launcher on PATH.
	install -m 0644 gui/dnas_gui.py "$root/usr/share/dnas/dnas_gui.py"
	cat > "$root/usr/bin/dnas-gui" <<'EOF'
#!/bin/sh
exec python3 /usr/share/dnas/dnas_gui.py "$@"
EOF
	chmod 0755 "$root/usr/bin/dnas-gui"

	# Docs (README, quickstart, copyright, Debian changelog).
	install -m 0644 README.md QUICKSTART.md "$root/usr/share/doc/dnas/"
	cat > "$root/usr/share/doc/dnas/copyright" <<EOF
DNAS ("Definitely Not A Scam")
Upstream source: https://github.com/nexusriot/DNAS

Copyright (C) 2026 the DNAS authors.

License: MIT-style — permission is granted, free of charge, to use, copy,
modify, and distribute this software. It is provided "as is", without warranty
of any kind. This is a learning project, not money; do not point it at the
internet.
EOF
	chmod 0644 "$root/usr/share/doc/dnas/copyright"
	# Version carries no Debian revision, so this is a native package: its
	# changelog is named changelog.gz (not changelog.Debian.gz).
	printf 'dnas (%s) unstable; urgency=low\n\n  * Automated build of dnas %s.\n\n -- %s  %s\n' \
		"$DEB_VERSION" "$RAW_VERSION" "$MAINTAINER" \
		"$(date -R 2>/dev/null || echo 'Thu, 01 Jan 1970 00:00:00 +0000')" \
		| gzip -9 -n > "$root/usr/share/doc/dnas/changelog.gz"
	chmod 0644 "$root/usr/share/doc/dnas/changelog.gz"

	# Lintian overrides: the Go binaries are intentionally static (CGO disabled)
	# for portability, and this toy ships no man pages.
	install -d "$root/usr/share/lintian/overrides"
	cat > "$root/usr/share/lintian/overrides/dnas" <<'EOF'
dnas: statically-linked-binary [usr/bin/dnas]
dnas: statically-linked-binary [usr/bin/dnas-tui]
dnas: no-manual-page [usr/bin/dnas]
dnas: no-manual-page [usr/bin/dnas-tui]
dnas: no-manual-page [usr/bin/dnas-gui]
EOF
	chmod 0644 "$root/usr/share/lintian/overrides/dnas"

	# Installed-Size (KiB) is measured before the control file is written.
	size_kb="$(du -ks "$root" | cut -f1)"
	cat > "$root/DEBIAN/control" <<EOF
Package: dnas
Version: ${DEB_VERSION}
Architecture: ${arch}
Maintainer: ${MAINTAINER}
Installed-Size: ${size_kb}
Section: net
Priority: optional
Recommends: python3, python3-pyqt6
Homepage: https://github.com/nexusriot/DNAS
Description: Definitely Not A Scam - a toy proof-of-work cryptocurrency
 A small but working proof-of-work cryptocurrency: Ed25519-signed
 transactions, mining rewards with coinbase maturity, an account+nonce
 ledger, most-work consensus with reorgs, an authenticated and encrypted
 peer-to-peer network, an HTTP API with a web explorer, plus terminal
 (dnas-tui) and desktop (dnas-gui) clients.
 .
 This is a learning project, not money. Do not point it at the internet.
EOF

	deb="${OUT}/dnas_${DEB_VERSION}_${arch}.deb"
	dpkg-deb --root-owner-group --build "$root" "$deb" >/dev/null
	echo "    -> ${deb}"
done

echo "done. packages in ${OUT}/:"
ls -1 "${OUT}"/*.deb 2>/dev/null || true
