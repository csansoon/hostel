MISE_VERSION = 2026.10.17

GO ?= go
MODULE = github.com/csansoon/hostel
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X $(MODULE)/internal/version.Version=$(VERSION)
GOFLAGS_BUILD = -trimpath -ldflags "$(LDFLAGS)"
TARGETS = darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64

export CGO_ENABLED = 0

.PHONY: build dist release mise mise-linux lint test-unit test-e2e test-e2e-10 test-smoke-namespace

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

# The files of a GitHub release (install.sh downloads them).
release: dist
	rm -rf dist/release && mkdir dist/release
	cp dist/hostel-* dist/mise-* dist/release/
	COPYFILE_DISABLE=1 tar -czf dist/release/hostel-platforms.tar.gz platforms
	cd dist/release && for f in *; do printf '%s  %s\n' "$$(shasum -a 256 $$f | cut -d' ' -f1)" "$$f"; done > ../SHA256SUMS && mv ../SHA256SUMS .

mise:
	./build/fetch-mise.sh $(MISE_VERSION) build/mise-checksums.txt dist

# The E2E rooms are Linux containers: they need only the Linux binaries.
mise-linux:
	./build/fetch-mise.sh $(MISE_VERSION) build/mise-checksums.txt dist linux/amd64 linux/arm64

lint:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	$(GO) vet -tags e2e ./...
	shellcheck install.sh platforms/*/*.sh test/platforms/*/*.sh

test-unit:
	$(GO) test ./...

test-e2e: mise-linux
	$(GO) test -tags e2e -count=1 -timeout 60m ./test/e2e/...

test-e2e-10: mise-linux
	@set -e; for i in 1 2 3 4 5 6 7 8 9 10; do \
		echo "== E2E run $$i of 10"; \
		$(GO) test -tags e2e -count=1 -timeout 60m ./test/e2e/...; \
	done

# Against the real Namespace account: it costs money (docs/m3-implementation.md).
test-smoke-namespace: dist
	HOSTEL_SMOKE_NAMESPACE=1 $(GO) test -tags e2e -count=1 -timeout 30m -run '^TestSmokeNamespace(Instance)?$$' -v ./test/e2e/...
