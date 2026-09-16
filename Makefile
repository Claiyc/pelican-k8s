# pelican-k8s build targets. Run `make help` for a summary.
SHELL := /bin/bash
GO ?= go
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
REGISTRY ?= ghcr.io/claiyc/pelican-k8s
IMAGES := shim agent gateway operator
CRD_DIR := charts/pelican-k8s/crds
LDFLAGS := -s -w -X github.com/Claiyc/pelican-k8s/internal/version.Version=$(VERSION)

.PHONY: help generate build test lint vet fmt tidy images push crds

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  %-14s %s\n", $$1, $$2}'

generate: ## Regenerate deepcopy code and CRD manifests
	$(GO) tool controller-gen object crd paths=./api/... output:crd:dir=$(CRD_DIR)

build: ## Build all binaries into ./bin
	@mkdir -p bin
	@for c in $(IMAGES); do CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o bin/$$c ./cmd/$$c || exit 1; done

test: ## Run unit tests
	$(GO) test -race -count=1 ./...

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## gofmt all sources
	gofmt -s -w $$(git ls-files '*.go')

tidy: ## go mod tidy
	$(GO) mod tidy

images: ## Build container images (docker)
	@for c in $(IMAGES); do docker build -f build/$$c.Dockerfile -t $(REGISTRY)/$$c:$(VERSION) --build-arg VERSION=$(VERSION) . || exit 1; done

push: images ## Push container images
	@for c in $(IMAGES); do docker push $(REGISTRY)/$$c:$(VERSION) || exit 1; done
