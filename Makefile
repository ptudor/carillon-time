# carillon — NTP-compatible time daemon with serial-port PPS
#
# Pure Go, CGO_ENABLED=0 everywhere. Cross-builds land in ./dist/<os>-<arch>/.

GO          ?= go
BIN         := bin
DIST        := dist
PKG         := carillon
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILDTIME   ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS     := -X $(PKG)/internal/buildinfo.Version=$(VERSION) -X $(PKG)/internal/buildinfo.BuildTime=$(BUILDTIME)
GOFLAGS     := -trimpath
CMDS        := carillon carillonctl

export CGO_ENABLED = 0

.PHONY: all build test vet fmt tidy clean freebsd linux dist

all: build

build: $(BIN)
	$(GO) build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN)/ ./cmd/...

$(BIN):
	mkdir -p $(BIN)

test: vet
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	gofmt -l -w .

tidy:
	$(GO) mod tidy

# Cross builds. Each target produces stripped static binaries.
define cross
	mkdir -p $(DIST)/$(1)-$(2)
	GOOS=$(1) GOARCH=$(2) $(3) $(GO) build $(GOFLAGS) -ldflags "-s -w $(LDFLAGS)" -o $(DIST)/$(1)-$(2)/ ./cmd/...
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
