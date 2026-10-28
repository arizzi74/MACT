GO ?= go
VERSION ?= dev
LDFLAGS = -s -w -X github.com/iaia/telegramgw/internal/buildinfo.Version=$(VERSION)

.PHONY: test lint build release
test:
	$(GO) test ./...

lint:
	@test -z "$$(gofmt -l cmd internal migrations)"
	$(GO) vet ./...

build:
	mkdir -p bin
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/codex-gateway ./cmd/codex-gateway
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/codex-worker ./cmd/codex-worker
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/claude-worker ./cmd/claude-worker
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/codex-local ./cmd/codex-local
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/codex-telegramgw ./cmd/codex-telegramgw
	@if [ ! -e bin/mact ] && [ ! -L bin/mact ]; then ln -s codex-telegramgw bin/mact; elif [ "$$(readlink bin/mact)" != codex-telegramgw ]; then echo "bin/mact is already occupied; keeping existing command"; fi

release:
	VERSION='$(VERSION)' ./scripts/release.sh
