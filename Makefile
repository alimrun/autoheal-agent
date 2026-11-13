# AutoHeal agent. `make check` is the single gate before a commit or a release.

PKG     := github.com/alimrun/autoheal-agent/internal/buildinfo
VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null | sed 's/^v//')
ifeq ($(strip $(VERSION)),)
VERSION := 0.0.0-dev
endif
COMMIT  ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)

LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

.PHONY: build test check release

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o autoheal-agent ./cmd/autoheal-agent

test:
	go test -race ./...

check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then echo "gofmt -l lists files that need formatting:"; echo "$$unformatted"; exit 1; fi
	go vet ./...
	go test -race ./...
	@vulncheck="$$(command -v govulncheck || echo "$$(go env GOPATH)/bin/govulncheck")"; \
	if [ -x "$$vulncheck" ]; then "$$vulncheck" ./...; \
	else echo "govulncheck is not installed; skipped (go install golang.org/x/vuln/cmd/govulncheck@latest)"; fi

# Usage: make release V=0.2.0   (needs a clean tree and the tag v0.2.0 on HEAD)
release:
	@if [ -z "$(V)" ]; then echo "usage: make release V=<version>"; exit 2; fi
	./scripts/release.sh "$(V)"
