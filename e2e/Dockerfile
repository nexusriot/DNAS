# Hermetic end-to-end run: build the dnas binary and drive it with the e2e suite,
# entirely inside the container. Nothing is needed on the host but Docker — no Go
# toolchain, no free ports (every node binds inside the container's own network
# namespace), and no state left behind.
#
#   docker build -f e2e/Dockerfile -t dnas-e2e .
#   docker run --rm dnas-e2e
#
# or just `make e2e-docker`.

FROM golang:1.26-alpine

# The suite shells out to `go build` only if DNAS_BIN is unset; we prebuild here
# so the image does the compiling once, at build time.
WORKDIR /src
COPY . .

RUN go build -trimpath -o /usr/local/bin/dnas ./cmd/dnas \
    && /usr/local/bin/dnas version

# Compile the tests too, so a broken suite fails the build rather than the run.
RUN go vet -tags e2e ./e2e/...

ENV DNAS_BIN=/usr/local/bin/dnas \
    CGO_ENABLED=0

# -count=1 defeats the test cache; the suite is I/O bound on real processes, so
# the default 10m per-test timeout is raised for the multi-node cases.
CMD ["go", "test", "-tags", "e2e", "-count=1", "-v", "-timeout", "20m", "./e2e/..."]
