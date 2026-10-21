MISE_VERSION = 2026.10.17

GO ?= go
MODULE = github.com/csansoon/hostel
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
GOFLAGS_BUILD = -trimpath -ldflags "$(LDFLAGS)"
TARGETS = darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

export CGO_ENABLED = 0

.PHONY: build dist lint test-unit

build:
	$(GO) build $(GOFLAGS_BUILD) -o dist/hostel ./cmd/hostel

dist:
	@set -e; for t in $(TARGETS); do \
		goos=$${t%/*}; goarch=$${t#*/}; ext=; \
		if [ $$goos = windows ]; then ext=.exe; fi; \
		echo "build dist/hostel-$$goos-$$goarch$$ext"; \
		GOOS=$$goos GOARCH=$$goarch $(GO) build $(GOFLAGS_BUILD) -o dist/hostel-$$goos-$$goarch$$ext ./cmd/hostel; \
	done
	./build/fetch-mise.sh $(MISE_VERSION) build/mise-checksums.txt dist

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet -tags e2e ./...

test-unit:
	$(GO) test ./...
