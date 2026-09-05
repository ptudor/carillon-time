# carillon — NTP-compatible time daemon with serial-port PPS
#
# Production builds are pure Go (CGO_ENABLED=0); cross-builds land in
# ./dist/<os>-<arch>/. Only `make test` (race detector) and `make abicheck`
# (system headers) need cgo, and both are native-only.

GO          ?= go
BIN         := bin
DIST        := dist
PKG         := carillon
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILDTIME   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.BuildTime=$(BUILDTIME)
GOFLAGS     := -trimpath
CMDS        := carillon carillonctl

# Production builds are pure Go: no C toolchain, so cross-compiling is one
# command (see CLAUDE.md, "No cgo"). Only the race detector and the ABI header
# checks need cgo, and they are native-only; they set CGO_ENABLED themselves.
BUILD_ENV   := CGO_ENABLED=0

# The Go race detector is implemented in C runtime code, so on Linux and
# FreeBSD `go test -race` fails outright under CGO_ENABLED=0 ("-race requires
# cgo"). Native race testing therefore needs cgo and a working C compiler.
# Cross-compiled race testing is not supported: run make test on each host.
TEST_ENV    := CGO_ENABLED=1

.PHONY: all build test vet fmt tidy clean freebsd linux dist abicheck

all: build

build: $(BIN)
	$(BUILD_ENV) $(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

$(BIN):
	mkdir -p $(BIN)

test: vet
	$(TEST_ENV) $(GO) test -race ./...

vet:
	$(BUILD_ENV) $(GO) vet ./...

# Header-only ABI comparison of the hand-declared kernel structs against the
# system headers. Native only, requires a C compiler, and touches no device
# and no clock -- that is why it uses the abicheck tag rather than hwtest.
abicheck:
	CGO_ENABLED=1 $(GO) test -tags abicheck -run 'Layout' ./internal/pps/ ./internal/clock/

fmt:
	gofmt -l -w .

tidy:
	$(GO) mod tidy

# Cross builds. Each target produces stripped static binaries.
define cross
	mkdir -p $(DIST)/$(1)-$(2)
	GOOS=$(1) GOARCH=$(2) $(BUILD_ENV) $(3) $(GO) build $(GOFLAGS) -ldflags "-s -w $(LDFLAGS)" -o $(DIST)/$(1)-$(2)/ ./cmd/...
endef

freebsd:
	$(call cross,freebsd,amd64,)
	$(call cross,freebsd,arm64,)

linux:
	$(call cross,linux,amd64,)
	$(call cross,linux,arm64,)

dist: freebsd linux

clean:
	rm -rf $(BIN) $(DIST)
