#!/usr/bin/env bash
# Print the version this tree builds as.
#
# The release number lives in the VERSION file, not in a git tag. That matters
# because the version has to be readable from the SOURCE: `git describe` returns
# nothing useful inside the e2e container (.dockerignore drops .git) or in a
# source tarball, and the old fallback there was a hard-coded "0.1.0" — a build
# that quietly claimed to be a version it was not.
#
# Git still contributes what only git knows: whether this build is the released
# commit or something after it. So:
#
#   0.3.0                    a clean tree at the commit tagged for that release
#   0.3.0+g1a2b3c4           built from some other commit
#   0.3.0+g1a2b3c4.dirty     ...with uncommitted changes
#   0.3.0                    no git at all (tarball, container)
#
# Override the whole thing with VERSION=1.2.3 in the environment.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ -n "${VERSION:-}" ]; then
	printf '%s\n' "$VERSION"
	exit 0
fi

if [ ! -f VERSION ]; then
	echo "scripts/version.sh: no VERSION file at the repo root" >&2
	exit 1
fi
base="$(tr -d ' \t\n\r' < VERSION)"
if [ -z "$base" ]; then
	echo "scripts/version.sh: VERSION file is empty" >&2
	exit 1
fi

# No git metadata (container, tarball): the file is all there is, and it is
# enough. Do not invent a suffix for what cannot be checked.
if ! git rev-parse --git-dir >/dev/null 2>&1; then
	printf '%s\n' "$base"
	exit 0
fi

suffix=""
# Tagged exactly at this release, with nothing uncommitted, is the only case
# that gets to call itself the bare release number.
tagged=""
for candidate in "v$base" "$base"; do
	if git rev-parse -q --verify "refs/tags/$candidate" >/dev/null 2>&1 &&
		[ "$(git rev-list -n1 "$candidate" 2>/dev/null)" = "$(git rev-parse HEAD 2>/dev/null)" ]; then
		tagged="yes"
		break
	fi
done
if [ -z "$tagged" ]; then
	sha="$(git rev-parse --short HEAD 2>/dev/null || true)"
	[ -n "$sha" ] && suffix="+g${sha}"
fi
if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
	suffix="${suffix}.dirty"
fi

printf '%s%s\n' "$base" "$suffix"
