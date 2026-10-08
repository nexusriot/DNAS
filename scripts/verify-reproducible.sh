#!/usr/bin/env bash
#
# Build the release twice, into two different directories, and check that every
# artifact comes out byte-identical.
#
#   scripts/verify-reproducible.sh
#   PLATFORMS="linux/amd64" scripts/verify-reproducible.sh
#
# Why bother. A published checksum is a claim that a binary came from a given
# commit, and it is only a claim anyone can CHECK if two people building that
# commit get the same bytes. Otherwise the hash attests to whatever the release
# machine happened to produce, which is precisely the thing a reader cannot
# verify and therefore has to trust.
#
# Building into two paths is the important part of the test: the build path is
# the usual thing that leaks into a binary, and a check that builds twice in the
# same directory would not catch it.
set -euo pipefail
cd "$(dirname "$0")/.."

a="$(mktemp -d)"
b="$(mktemp -d)"
trap 'rm -rf "$a" "$b"' EXIT

# The same source, built from two different working directories, so an absolute
# path baked into an artifact shows up as a difference.
echo "==== copying the tree to two build directories ===="
for dir in "$a" "$b"; do
	# git archive would miss uncommitted work, which is usually what is being
	# checked. Copy the tree, minus the things a build creates.
	tar --exclude=./dist --exclude=./bin --exclude=./.git -cf - . | tar -C "$dir" -xf -
done

# SOURCE_DATE_EPOCH is normally derived from the commit, which the copies have
# no git metadata for. Fix it here so both runs agree, exactly as two machines
# building the same commit would.
export SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --pretty=%ct 2>/dev/null || echo 1700000000)}"
export VERSION="${VERSION:-$(./scripts/version.sh)}"
export PLATFORMS="${PLATFORMS:-linux/amd64}"

for dir in "$a" "$b"; do
	echo "==== building in ${dir} ===="
	( cd "$dir" && ./scripts/build.sh >/dev/null )
done

echo "==== comparing ===="
rc=0
for f in "$a"/dist/*.tar.gz; do
	name="$(basename "$f")"
	ha="$(sha256sum "$f" | cut -d' ' -f1)"
	if [ ! -f "$b/dist/$name" ]; then
		echo "MISSING  $name (built once, not twice)"
		rc=1
		continue
	fi
	hb="$(sha256sum "$b/dist/$name" | cut -d' ' -f1)"
	if [ "$ha" = "$hb" ]; then
		echo "ok       $name  $ha"
	else
		echo "DIFFERS  $name"
		echo "         $ha"
		echo "         $hb"
		rc=1
	fi
done

if [ $rc -ne 0 ]; then
	echo
	echo "The build is NOT reproducible. Usual causes, in order of likelihood:" >&2
	echo "  - a timestamp or build path baked into an artifact (check -trimpath, -buildid=)" >&2
	echo "  - a file in the tarball whose mtime was not normalised" >&2
	echo "  - a map iterated in a generator without sorting its keys" >&2
	exit 1
fi
echo
echo "reproducible: both builds produced identical artifacts."
