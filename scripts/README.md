# scripts

Helper scripts for DNAS. The [Makefile](../Makefile) wraps these (`make demo`,
`make dist`, `make deb`) and passes `VERSION`/`PLATFORMS`/`ARCHES` through, but
each script also runs standalone from the repo root.

## demo.sh

`./scripts/demo.sh` builds the binary and runs a three-node network end to end:
wrong-`netkey` rejection, peer discovery, a signed transfer converging on every
node, transaction expiry, the dynamic fee floor, a derived multisig address, a
generated HD wallet, and a Python light-client SPV verification. Edit the `190xx`
ports at the top if they are taken.

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
