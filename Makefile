# Expanse Makefile

.PHONY: help build test lint fmt proto vm-test perf chaos chaos-storage clean

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

chaos:           ## Run the chaos suite at full length (5 min/scenario + 30 min blocks)
	RUN_CHAOS=1 go test -count=1 -timeout 90m ./test/chaos/... -v

chaos-storage:   ## Run the storage linearizability suite at full length (1 h nightly)
	RUN_CHAOS=1 go test -count=1 -timeout 90m ./test/chaos/storage/linearizability/ -v

clean:           ## Clean build artifacts
	rm -rf bin result coverage.out