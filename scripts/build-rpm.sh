#!/usr/bin/env bash
# Build an RPM for DNAS.
#
#   scripts/build-rpm.sh                 # build for this machine's architecture
#   VERSION=1.2.3 scripts/build-rpm.sh   # stamp an explicit version
#
# Unlike build-deb.sh this does NOT cross-compile: rpmbuild wants to run the
# build itself, and an RPM built for another architecture from here would claim
# a %{_arch} it was not built for. Build on the target, or in a container for it.
set -euo pipefail
cd "$(dirname "$0")/.."

command -v rpmbuild >/dev/null 2>&1 || {
	echo "rpmbuild not found (install rpm-build / rpm)"
	exit 1
}

RAW_VERSION="$(VERSION="${VERSION:-}" ./scripts/version.sh)"
# RPM versions may not contain '-'; the git detail becomes part of the version
# with separators it allows.
RPM_VERSION="$(printf '%s' "$RAW_VERSION" | sed -e 's/^v//' -e 's/-/./g' -e 's/[^A-Za-z0-9._+]/./g')"
case "$RPM_VERSION" in [0-9]*) ;; *) RPM_VERSION="0.${RPM_VERSION}" ;; esac

OUT="${OUT:-dist}"
TOP="$(mktemp -d)"
trap 'rm -rf "$TOP"' EXIT
mkdir -p "$TOP"/{BUILD,RPMS,SOURCES,SPECS,SRPMS} "$OUT"

echo "==> packaging dnas ${RPM_VERSION}"

# rpmbuild wants a tarball whose top directory is name-version. Everything the
# spec's %build and %install need has to be inside it, so the whole tree goes in
# minus the things that are output rather than input.
STAGE="${TOP}/dnas-${RPM_VERSION}"
mkdir -p "$STAGE"
tar --exclude='./.git' --exclude='./dist' --exclude='./bin' \
    --exclude='./.github' --exclude='*.tmp' \
    -cf - . | tar -xf - -C "$STAGE"
( cd "$TOP" && tar -czf "SOURCES/dnas-${RPM_VERSION}.tar.gz" "dnas-${RPM_VERSION}" )

cp scripts/packaging/dnas.spec "$TOP/SPECS/dnas.spec"
rpmbuild --define "_topdir ${TOP}" \
         --define "dnas_version ${RPM_VERSION}" \
         -bb "$TOP/SPECS/dnas.spec"

find "$TOP/RPMS" -name '*.rpm' -exec cp {} "$OUT/" \;
echo "done. packages in ${OUT}/:"
ls -1 "${OUT}"/*.rpm 2>/dev/null || true
