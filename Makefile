SHELL := /bin/sh
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || printf dev)
IMAGE ?= oonfeewrt:$(VERSION)
PLATFORMS ?= linux/amd64,linux/arm64
RELEASE_DIR ?= dist

.PHONY: help ui build test check image release release-check

help:
	@printf '%s\n' \
		'make ui                              build the embedded UI' \
		'make build                           build the host controller binary' \
		'make test                            run Go and UI tests' \
		'make check                           run the local release gates' \
		'make image                           build the amd64/arm64 OCI image' \
		'make release RELEASE_VERSION=        package four release binaries' \
		'make release-check RELEASE_VERSION=  require a clean, reproducible release tree'

ui:
	npm --prefix ui ci --no-audit
	npm --prefix ui run build

build: ui
	CGO_ENABLED=0 go build -trimpath -buildvcs=false \
		-ldflags "-s -w -buildid= -X main.version=$(VERSION)" \
		-o oonfeewrtd ./cmd/oonfeewrtd

test: ui
	go test -count=1 ./...
	npm --prefix ui test

check: ui
	go mod verify
	go mod tidy -diff
	go test -count=1 ./...
	go vet ./...
	npm --prefix ui test
	./tools/budget_check.sh
	./tools/secret-scan.sh --tree

image:
	docker buildx build --platform $(PLATFORMS) \
		--build-arg VERSION=$(VERSION) -f deploy/Dockerfile -t $(IMAGE) .

release:
	@test -n "$(RELEASE_VERSION)" || { \
		echo 'release: set RELEASE_VERSION (for example v0.1.0-rc.1)' >&2; exit 2; \
	}
	./tools/release-build.sh "$(RELEASE_VERSION)" "$(RELEASE_DIR)"

release-check:
	@test -n "$(RELEASE_VERSION)" || { \
		echo 'release-check: set RELEASE_VERSION (for example v0.1.0)' >&2; exit 2; \
	}
	@test -f "RELEASE-NOTES-$(RELEASE_VERSION).md" || { \
		echo 'release-check: missing RELEASE-NOTES-$(RELEASE_VERSION).md' >&2; exit 2; \
	}
	@cmp -s "RELEASE-NOTES-$(RELEASE_VERSION).md" RELEASE-NOTES.md || { \
		echo 'release-check: versioned notes differ from RELEASE-NOTES.md' >&2; exit 2; \
	}
	./tools/secret-scan.sh
	./tools/reproducible-build-check.sh "$(RELEASE_VERSION)"
