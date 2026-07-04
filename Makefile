# DNAS build & packaging.
#
# Common targets:  make build | test | dist | deb | install | clean
# Override VERSION (defaults to `git describe`), PREFIX, PLATFORMS, ARCHES, GO.

GO        ?= go
PREFIX    ?= /usr/local
BINDIR    := $(DESTDIR)$(PREFIX)/bin
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)
LDFLAGS   := -s -w -X main.version=$(VERSION)
GOMODS    := ./core/... ./node/... ./api/... ./cmd/... ./wallet/...
PLATFORMS ?= linux/amd64 linux/arm64
ARCHES    ?= amd64 arm64

# Pass computed settings down to the build/packaging scripts.
export VERSION PLATFORMS ARCHES GO

.PHONY: all build dnas tui test test-race vet fmt dist deb demo install uninstall clean version help

all: build

## build: compile dnas + dnas-tui for the host into bin/
build: dnas tui

## dnas: build just the dnas CLI/daemon into bin/
dnas:
	$(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/dnas ./cmd/dnas

## tui: build just the terminal client into bin/ (its own module)
tui:
	cd tui && $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o ../bin/dnas-tui .

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

## test-race: run the Go tests under the race detector
test-race:
	$(GO) test -race $(GOMODS)
	cd tui && $(GO) test -race ./...

## vet: go vet across all modules
vet:
	$(GO) vet $(GOMODS)
	cd tui && $(GO) vet ./...

## fmt: gofmt all modules
fmt:
	$(GO) fmt $(GOMODS)
	cd tui && $(GO) fmt ./...

## dist: cross-compile release tarballs (PLATFORMS) into dist/
dist:
	./scripts/build.sh

## deb: build .deb packages (ARCHES) into dist/
deb:
	./scripts/build-deb.sh

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

## help: list targets
help:
	@echo "DNAS make targets (VERSION=$(VERSION)):"
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## /  /'
