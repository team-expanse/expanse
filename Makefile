# Expanse Makefile

.PHONY: help build test lint fmt proto vm-test perf clean

help:            ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build:           ## Build the binary
	go build -o bin/expanse ./cmd/expanse/

test:            ## Run unit tests with coverage
	go test -race -coverprofile=coverage.out ./...

lint:            ## Run golangci-lint
	golangci-lint run ./...

fmt:             ## Format Go and Nix
	gofumpt -w . && nixpkgs-fmt .

proto:           ## Regenerate protobuf Go code (requires protoc + plugins on PATH)
	protoc --go_out=. --go_opt=module=github.com/expanse/expanse \
	       --go-grpc_out=. --go-grpc_opt=module=github.com/expanse/expanse \
	       --proto_path=. proto/*.proto

vm-test:         ## Run NixOS VM tests
	nix build .#checks.$(nix eval --raw --impure --expr builtins.currentSystem).smoke -L

perf: build      ## Check performance budgets
	RUN_PERF=1 go test ./test/perf/... -v

clean:           ## Clean build artifacts
	rm -rf bin result coverage.out