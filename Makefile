.DEFAULT_GOAL := help

GO ?= go
BIN := northwatch
PKG := ./...

.PHONY: help build run test vet lint css clean

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z_-]+:.*##/ { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the northwatch binary.
	$(GO) build -o $(BIN) ./cmd/northwatch

run: build ## Build and run the binary.
	./$(BIN)

test: ## Run unit tests.
	$(GO) test $(PKG)

vet: ## Run go vet.
	$(GO) vet $(PKG)

lint: ## Run golangci-lint (must be installed).
	golangci-lint run $(PKG)

css: ## Compile Tailwind CSS from web/input.css to internal/ui/static/style.css.
	@command -v tailwindcss >/dev/null 2>&1 || { echo "tailwindcss not found in PATH; install standalone binary or via npm"; exit 1; }
	tailwindcss -i web/input.css -o internal/ui/static/style.css --minify

clean: ## Remove build artifacts.
	rm -f $(BIN)
