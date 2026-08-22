# Hermetic end-to-end run: the dnas binary and the compiled test suite are both
# produced here, from this tree, at image build time. The run itself needs no Go
# toolchain, no source, no network and no writable disk — only Docker.
#
#   make e2e-docker                             # build + run, fully isolated
#   make e2e-docker E2E_ARGS='-test.run TestSPV'
#
# What "hermetic" buys, concretely:
#
#   * Both base images are pinned by digest, so the toolchain that compiles the
#     product is byte-identical on every machine and every rebuild — a green run
#     here today and a red one tomorrow can only be the source changing.
#   * The build fetches nothing: GOPROXY=off and GOTOOLCHAIN=local turn a stray
#     dependency or a toolchain bump into a build failure instead of a silent
#     download. (DNAS has no third-party modules; this keeps it that way.)
#   * The suite is compiled in the build stage, so the runtime image carries no
#     toolchain, no sources and no build cache: `docker run` can only execute
#     what `docker build` produced, and a compiled test binary cannot be served
#     from Go's test cache.
#   * The run is launched with --network none and a read-only root (see the
#     Makefile), so every node talks over the container's own loopback and
#     nothing — test, node, or CLI — can reach the host, a peer or a proxy
#     outside it, or leave a byte behind outside the tmpfs.
#
# Bumping the toolchain or the runtime base is deliberate: change the tag, then
#   docker pull golang:1.26-alpine
#   docker image inspect golang:1.26-alpine --format '{{index .RepoDigests 0}}'
# and paste the digest here. Digests are multi-arch manifest lists, so amd64 and
# arm64 hosts both resolve from the same pin.
#
# NB: no `# syntax=` directive on purpose — that would pull a frontend image
# from the network, off a floating tag, before the build even starts.

# --- build stage: compile the product and the suite that drives it ------------
FROM golang:1.26-alpine@sha256:28d89ee9cc0ff9fec75c82ca201e6bf7fdf9a679d4b7b24dfa04f2bb766bb468 AS build

ENV CGO_ENABLED=0 \
    GOTOOLCHAIN=local \
    GOFLAGS=-mod=readonly \
    GOPROXY=off \
    GOSUMDB=off

WORKDIR /src
COPY . .

# A broken tree fails the build, not the run: the binary must compile and answer
# `version`, the suite must vet clean (nothing else vets it — the Makefile's
# GOMODS stops at the product modules) and must compile against the tag.
RUN go build -trimpath -ldflags '-s -w' -o /out/dnas ./cmd/dnas \
    && /out/dnas version \
    && go vet -tags e2e ./e2e/... \
    && go test -c -trimpath -tags e2e -o /out/dnas-e2e ./e2e

# --- runtime stage: the two binaries, nothing else ----------------------------
FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

COPY --from=build /out/dnas /usr/local/bin/dnas
COPY --from=build /out/dnas-e2e /usr/local/bin/dnas-e2e

# An unprivileged user with a home it cannot write to. The suite claims no test
# touches $HOME; pointing HOME at a directory on the read-only root turns that
# claim into something the container enforces, rather than something a reviewer
# has to check. Everything a node needs lives under TMPDIR, which the run mounts
# as a tmpfs, so a finished container leaves nothing anywhere.
RUN adduser -D -H -u 1000 dnas && mkdir -p /home/dnas

ENV DNAS_BIN=/usr/local/bin/dnas \
    HOME=/home/dnas \
    TMPDIR=/tmp \
    TZ=UTC

USER dnas:dnas
WORKDIR /tmp

# Args land on the test binary, so a subset is one flag away:
#   docker run --rm --network none dnas-e2e -test.v -test.run TestReorg
ENTRYPOINT ["/usr/local/bin/dnas-e2e"]
CMD ["-test.v", "-test.timeout=20m"]
