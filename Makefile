.DEFAULT_GOAL := help

GO ?= go
BIN := northwatch
PKG := ./...
IMAGE ?= northwatch:dev

.PHONY: help build run test vet lint css image clean helm-lint helm-smoke helm-smoke-v3 e2e

help: ## Show this help.
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*##/ { printf "  %-14s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

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

image: ## Build the container image (override with IMAGE=name:tag).
	docker build -f deploy/docker/Dockerfile -t $(IMAGE) .

clean: ## Remove build artifacts.
	rm -f $(BIN)

helm-lint: ## Lint the Helm chart.
	helm lint deploy/helm/northwatch

helm-smoke: ## Run the chart smoke test using the system helm (mise default = 4.2.0).
	HELM=helm KIND_CLUSTER=northwatch-smoke bash deploy/helm/scripts/smoke.sh

helm-smoke-v3: ## Run the chart smoke test against Helm 3.21.0 (downloaded into a temp dir).
	@set -eu; \
	tmp=$$(mktemp -d); \
	trap "rm -rf $$tmp" EXIT; \
	os=$$(uname -s | tr '[:upper:]' '[:lower:]'); \
	arch=$$(uname -m); \
	case "$$arch" in x86_64) arch=amd64;; aarch64|arm64) arch=arm64;; esac; \
	url="https://get.helm.sh/helm-v3.21.0-$${os}-$${arch}.tar.gz"; \
	echo "Downloading $$url"; \
	curl -fsSL "$$url" -o "$$tmp/helm.tgz"; \
	expected=$$(curl -fsSL "$$url.sha256sum" | awk '{print $$1}'); \
	actual=$$(shasum -a 256 "$$tmp/helm.tgz" | awk '{print $$1}'); \
	if [ "$$expected" != "$$actual" ]; then \
	  echo "checksum mismatch: expected $$expected, got $$actual" >&2; \
	  exit 1; \
	fi; \
	tar -xzf "$$tmp/helm.tgz" -C "$$tmp"; \
	helm3="$$tmp/$${os}-$${arch}/helm"; \
	"$$helm3" version --short; \
	HELM="$$helm3" KIND_CLUSTER=northwatch-smoke-v3 bash deploy/helm/scripts/smoke.sh

e2e: ## Run the killer-demo e2e: kind up, build/load image, run go test, kind down.
	@set -eu; \
	cluster="northwatch-e2e"; \
	cleanup() { \
	  [ -n "$${E2E_KEEP_CLUSTER:-}" ] && return 0; \
	  kind delete cluster --name "$$cluster" >/dev/null 2>&1 || true; \
	}; \
	trap cleanup EXIT; \
	trap 'trap - EXIT INT TERM; cleanup; exit 130' INT; \
	trap 'trap - EXIT INT TERM; cleanup; exit 143' TERM; \
	app_version=$$(helm show chart deploy/helm/northwatch \
	  | awk '/^appVersion:/ {gsub(/"/, "", $$2); print $$2; exit}'); \
	image="ghcr.io/northwatchlabs/northwatch:$$app_version"; \
	echo "==> appVersion=$$app_version image=$$image cluster=$$cluster"; \
	if ! kind get clusters | grep -qx "$$cluster"; then \
	  kind create cluster --name "$$cluster" --wait 60s; \
	fi; \
	docker build -f deploy/docker/Dockerfile -t "$$image" .; \
	kind load docker-image "$$image" --name "$$cluster"; \
	E2E_CLUSTER="$$cluster" $(GO) test -tags=e2e -count=1 -timeout 10m -v ./test/e2e/...
