BINARY := pfkeeper
CLI := pfk
GOFLAGS := CGO_ENABLED=0
GOFREEBSD := GOOS=freebsd GOARCH=amd64 CGO_ENABLED=0
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

.PHONY: gen build build-freebsd test lint clean certs

gen: ## regenerate gRPC code from api/proto into api/pfkeeperv1 (per go_package)
	cd api && PATH="$$PATH:$(shell go env GOPATH)/bin" protoc \
	  --go_out=.. --go_opt=module=github.com/wsgoggway/lb-dsr-lab/pfkeeper \
	  --go-grpc_out=.. --go-grpc_opt=module=github.com/wsgoggway/lb-dsr-lab/pfkeeper \
	  proto/v1/pfkeeper.proto

build: ## build host binaries into bin/
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/$(BINARY) ./cmd/$(BINARY)
	CGO_ENABLED=0 go build -ldflags '$(LDFLAGS)' -o bin/$(CLI) ./cmd/$(CLI)

build-freebsd: ## cross-compile for FreeBSD 14/amd64 into bin/freebsd/
	mkdir -p bin/freebsd
	$(GOFREEBSD) go build -ldflags '$(LDFLAGS)' -o bin/freebsd/$(BINARY) ./cmd/$(BINARY)
	$(GOFREEBSD) go build -ldflags '$(LDFLAGS)' -o bin/freebsd/$(CLI) ./cmd/$(CLI)

test: ## run all tests
	go test ./... -count=1

lint: ## go vet everything
	go vet ./...

clean: ## remove build artifacts
	rm -rf bin

certs: ## generate lab CA + client/node certificates into certs/
	bash scripts/pfkeeper-certs.sh

help: ## show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-14s %s\n", $$1, $$2}'
