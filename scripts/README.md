# scripts

Helper scripts for DNAS. The [Makefile](../Makefile) wraps these (`make demo`,
`make dist`, `make deb`) and passes `VERSION`/`PLATFORMS`/`ARCHES` through, but
each script also runs standalone from the repo root.

## demo.sh

`./scripts/demo.sh` builds the binary and runs a three-node network end to end:
wrong-`netkey` rejection, peer discovery, a signed transfer converging on every
node, transaction expiry, the dynamic fee floor, a derived multisig address, a
generated HD wallet, and a light-client SPV verification with
`dnas spv -cache "" verify` — the binary's own verifier, not a script-side copy of
the header rules (a Python one used to live here and drifted when BIP9 changed the
header). Edit the `190xx` ports at the top if they are taken.

## htlc-demo.sh

`./scripts/htlc-demo.sh` is a focused companion to `demo.sh`: on a single mining
node it funds two hash-time-locked contracts and settles one via the **claim**
(preimage) branch and the other via the **refund** (timeout) branch. Edit the
`190xx` ports at the top if they are taken.

## swap-demo.sh

`./scripts/swap-demo.sh` settles a full **asset-for-coin atomic swap** on one
node: Alice issues a native asset and wants coin, Bob has coin and wants the
asset, and neither has to trust the other. It derives both HTLC legs with
`dnas htlc swap`, funds them (including the coin an asset contract needs to pay
its own claim fee), and settles — Alice's claim publishes the preimage on-chain,
which is what lets Bob take the asset. Edit the `191xx` ports at the top if they
are taken.

## version.sh

Prints the version a build stamps. The release number lives in the
[`VERSION`](../VERSION) file at the repo root; this script reads it and appends
what only git can tell you — which commit, and whether the tree is dirty.

```sh
./scripts/version.sh          # 0.4.0, 0.4.0+g1a2b3c4, or 0.4.0+g1a2b3c4.dirty
VERSION=1.2.3 ./scripts/version.sh   # an override wins outright
```

It replaced a bare `git describe`, which cannot answer where it matters most:
the e2e container excludes `.git` and so does a source tarball, and both used to
fall back to a hard-coded `0.1.0` or report `dev`. A build should be able to say
what it is from the source it was built from. The Makefile and both build
scripts below call it; `make version` prints the result.

## build.sh

Cross-compiles static (CGO-free) `dnas` + `dnas-tui` binaries for one or more
platforms and packages each as a `.tar.gz` (with the GUI script and docs) under
`dist/`.

```sh
./scripts/build.sh                          # linux/amd64 + linux/arm64
PLATFORMS="linux/amd64" ./scripts/build.sh  # a single platform
VERSION=1.2.3 ./scripts/build.sh            # stamp an explicit version
```

Env: `PLATFORMS` (space-separated `os/arch`), `VERSION`, `OUT` (default `dist`),
`GO` (default `go`). Version is stamped into the binary via
`-ldflags "-X main.version=…"` and shown by `dnas version`.

## build-deb.sh

Builds Debian `.deb` packages (amd64 and/or arm64) with `dpkg-deb` — no
`fakeroot` needed (`--root-owner-group`). A package installs `/usr/bin/dnas`,
`/usr/bin/dnas-tui`, a `/usr/bin/dnas-gui` launcher, the PyQt script, and docs;
it `Recommends` `python3` + `python3-pyqt6`. The packages are lintian-clean (a
shipped overrides file acknowledges the intentional static binaries and absent
man pages).

```sh
./scripts/build-deb.sh                    # amd64 + arm64
ARCHES="amd64" ./scripts/build-deb.sh     # a single architecture
VERSION=1.2.3 ./scripts/build-deb.sh      # stamp an explicit version
```

Env: `ARCHES` (`amd64`/`arm64`), `VERSION`, `OUT` (default `dist`), `GO`,
`MAINTAINER`. The Debian version is derived from `VERSION` (leading `v` stripped,
sanitized to a valid version); since it carries no Debian revision, packages are
built as *native* (changelog named `changelog.gz`).

## build-rpm.sh

Builds an RPM (`make rpm`) from [packaging/dnas.spec](packaging/dnas.spec).
Unlike `build-deb.sh` it does **not** cross-compile: rpmbuild runs the build
itself, and an RPM built for another architecture from here would claim an
`%{_arch}` it was not built for. Build on the target, or in a container for it.

```sh
./scripts/build-rpm.sh                  # this machine's architecture
VERSION=1.2.3 ./scripts/build-rpm.sh    # stamp an explicit version
```

## verify-reproducible.sh

Builds the release twice, from two different working directories, and checks
every artifact comes out byte-identical (`make verify-reproducible`). A published
checksum is only a claim anyone can *check* if two people building the same
commit get the same bytes; building in two paths is the important part, since the
build path is the usual thing that leaks into a binary.

```sh
./scripts/verify-reproducible.sh
PLATFORMS="linux/amd64" ./scripts/verify-reproducible.sh
```

## bench.sh

Runs the performance benchmarks and optionally holds them to a recorded baseline
(`make bench`, `make bench-update`, `make bench-check`). Correctness tests use tiny
chains, so a cost that grows with the ledger never shows in one; a recorded
number does not have that blind spot. The regression threshold is deliberately
loose — it runs on developer machines, so the signal worth catching is "several
times slower", not "10% slower".

```sh
./scripts/bench.sh            # run, print results
./scripts/bench.sh --update   # run and overwrite the baseline
./scripts/bench.sh --check    # run and fail if anything regressed
```
