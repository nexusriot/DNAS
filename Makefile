# DNAS build & packaging.
#
# Common targets:  make build | test | dist | deb | install | clean
# Override VERSION (defaults to `git describe`), PREFIX, PLATFORMS, ARCHES, GO.

GO        ?= go
PREFIX    ?= /usr/local
BINDIR    := $(DESTDIR)$(PREFIX)/bin
# The release number lives in the VERSION file; scripts/version.sh adds the git
# detail (which commit, whether the tree is dirty) when git is available. That
# keeps the version readable from the source alone — inside the e2e container,
# which has no .git, and in a source tarball.
VERSION   ?= $(shell ./scripts/version.sh)
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOMODS    := ./core/... ./node/... ./api/... ./cmd/... ./wallet/...
# Plain directories, for the tools that take paths rather than package patterns.
GODIRS    := core node api cmd wallet e2e
PLATFORMS ?= linux/amd64 linux/arm64
ARCHES    ?= amd64 arm64

# Containerized e2e. The run gets its own loopback and nothing else: no network,
# no writable filesystem outside a tmpfs for the nodes' data directories, no
# privileges. Whatever passes in there passed without touching the host, so a
# green run means the product works, not that the machine helped.
E2E_IMAGE ?= dnas-e2e
E2E_ARGS  ?=
E2E_DOCKER_RUN := --rm --init --network none --read-only \
	--tmpfs /tmp:rw,noexec,nosuid,nodev,size=512m \
	--cap-drop ALL --security-opt no-new-privileges --pids-limit 512

# Pass computed settings down to the build/packaging scripts.
export VERSION PLATFORMS ARCHES GO

.PHONY: all build dnas tui wasm test test-race test-upgrades bench bench-update bench-check verify-reproducible e2e e2e-docker e2e-docker-shell vet fmt dist deb rpm demo install uninstall clean version help

all: build

## build: compile the wasm bundle + dnas + dnas-tui for the host into bin/
build: wasm dnas tui

## dnas: build just the dnas CLI/daemon into bin/
dnas:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/dnas ./cmd/dnas

## tui: build just the terminal client into bin/ (its own module)
tui:
	cd tui && $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/dnas-tui .

## wasm: build the browser verification bundle into api/wasmdist/
# The explorer verifies proofs by calling into core compiled to WebAssembly,
# rather than a JavaScript reimplementation of the consensus rules. Every copy
# of those rules this project kept outside core has silently rotted — the
# explorer's own was hashing a preimage without the header's Version field — so
# the bundle is the only verifier the page has, and `build` depends on it.
#
# wasm_exec.js is Go's runtime shim and must come from the toolchain that built
# the .wasm, so it is copied here rather than vendored.
wasm:
	$(GO) env GOROOT >/dev/null
	GOOS=js GOARCH=wasm $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o api/wasmdist/dnas.wasm ./cmd/wasmverify
	cp "$$($(GO) env GOROOT)/lib/wasm/wasm_exec.js" api/wasmdist/wasm_exec.js

## test: run all Go tests (root modules + tui) and the GUI tests (skipped if PyQt6 is absent)
test:
	$(GO) test $(GOMODS)
	cd tui && $(GO) test ./...
	@if python3 -c 'import PyQt6' >/dev/null 2>&1; then \
		echo "cd gui && QT_QPA_PLATFORM=offscreen python3 -m unittest test_dnas_gui"; \
		cd gui && QT_QPA_PLATFORM=offscreen python3 -m unittest test_dnas_gui; \
	else \
		echo "skipping GUI tests (python3 / PyQt6 not available)"; \
	fi

## test-upgrades: run the Go tests with every consensus upgrade active
# A second pass over the same suite, not a different suite. The default run
# leaves upgrades unscheduled and each test switches on the one it is about, so
# the rules only ever got exercised ONE AT A TIME — which is how a rule that is
# correct alone and wrong in combination survives. Everything the commitments
# touch (pruning, snapshots, compact relay) and every rule that reads a
# transaction's shape (checked addresses vs. asset and name operations) is only
# covered here.
#
# The header commitments activate at height 1 rather than 0: genesis is a
# constant whose hash is the network's identity, and a commitment active at
# height 0 is inside its preimage. See core/main_test.go.
test-upgrades:
	DNAS_TEST_UPGRADES=all $(GO) test -count=1 $(GOMODS)

## test-race: run the Go tests under the race detector
# The timeout is explicit because go's default is 10 minutes and the core suite
# under -race is a real proof-of-work workload — it sat at roughly two minutes
# here, which is too little headroom for a slower CI runner to be trusted with.
# It had grown past 20 minutes before the mining loop stopped rebuilding each
# header preimage with fmt.Sprintf (see core.Mine); if it creeps back up, look
# for a hash on a hot path before reaching for a bigger number here.
test-race:
	$(GO) test -race -timeout 20m $(GOMODS)
	cd tui && $(GO) test -race -timeout 20m ./...

## bench: run the performance benchmarks (core hot paths)
bench:
	./scripts/bench.sh

## bench-update: re-record the benchmark baseline (do this deliberately)
bench-update:
	./scripts/bench.sh --update

## bench-check: fail if a benchmark regressed past the baseline threshold
bench-check:
	./scripts/bench.sh --check

## e2e: run the black-box end-to-end suite against a binary built from this tree
e2e:
	$(GO) test -tags e2e -count=1 -timeout 20m ./e2e/...

## e2e-docker: run the same suite hermetically in a container (needs only Docker)
e2e-docker:
	docker build -f e2e/Dockerfile -t $(E2E_IMAGE) .
	docker run $(E2E_DOCKER_RUN) $(E2E_IMAGE) -test.v -test.timeout=20m $(E2E_ARGS)

## e2e-docker-shell: drop into the e2e image to poke at it by hand
e2e-docker-shell:
	docker build -f e2e/Dockerfile -t $(E2E_IMAGE) .
	docker run $(E2E_DOCKER_RUN) -it --entrypoint /bin/sh $(E2E_IMAGE)

## vet: go vet across all modules
vet:
	$(GO) vet $(GOMODS)
	cd tui && $(GO) vet ./...

## fmt: gofmt all modules
# `go fmt` resolves its package patterns against a single main module, which the
# repo root is not — it fails with "directory prefix core does not contain main
# module" for every entry in GOMODS. gofmt takes directories and does not care,
# and it is what CI checks with, so both agree.
fmt:
	gofmt -l -w $(GODIRS)
	cd tui && gofmt -l -w .

## dist: cross-compile release tarballs (PLATFORMS) into dist/
dist:
	./scripts/build.sh

## verify-reproducible: build the release twice and check the artifacts match
verify-reproducible:
	./scripts/verify-reproducible.sh

## deb: build .deb packages (ARCHES) into dist/
deb:
	./scripts/build-deb.sh

## rpm: build an RPM for THIS machine's architecture into dist/
# Unlike deb it does not cross-compile: rpmbuild runs the build itself, so an
# RPM for another architecture has to be built on (or in a container for) it.
rpm:
	./scripts/build-rpm.sh

## demo: run the 3-node end-to-end demo
demo:
	./scripts/demo.sh

## install: install dnas + dnas-tui to $(PREFIX)/bin
install: build
	install -d $(BINDIR)
	install -m 0755 bin/dnas $(BINDIR)/dnas
	install -m 0755 bin/dnas-tui $(BINDIR)/dnas-tui

## uninstall: remove the installed binaries
uninstall:
	rm -f $(BINDIR)/dnas $(BINDIR)/dnas-tui

## version: print the version the build would stamp
version:
	@echo $(VERSION)

## clean: remove build artifacts
clean:
	rm -rf bin dist dnas dnas-tui dnas-race tui/tui
	rm -f api/wasmdist/dnas.wasm api/wasmdist/wasm_exec.js

## help: list targets
help:
	@echo "DNAS make targets (VERSION=$(VERSION)):"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
