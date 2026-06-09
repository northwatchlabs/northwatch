# Local Setup

NorthWatch contributors should use the tool versions pinned in
`mise.toml`. The pinned tools include Go, Python for MkDocs, Tailwind,
kind, kubectl, kubectx, Flux, Helm, goreleaser, and golangci-lint.

## Bootstrap tools

Install `mise`, activate it in your shell, then install project tools:

```sh
brew install mise
mise trust
mise install
```

The docs tasks create a local Python virtual environment at
`.venv-docs`.

## Common commands

```sh
make build       # build ./cmd/northwatch into ./northwatch
make test        # run unit tests
make vet         # run go vet
make lint        # run golangci-lint
make css         # compile Tailwind into embedded style.css
make image       # build a local container image
make helm-lint   # lint the in-tree chart
mise run docs-build
```

`mise run docs-build` runs `mkdocs build --strict` after installing
`docs/requirements.txt`.

## Run without a cluster

Use `--no-cluster` when working on HTTP, template, incident, config, or
store behavior that does not require informers:

```sh
mise trust
mise install
make build

cat > /tmp/northwatch.yaml <<'EOF'
components:
  - kind: Deployment
    namespace: default
    name: api-gateway
    displayName: "API Gateway"
EOF

./northwatch serve \
  --no-cluster \
  --config /tmp/northwatch.yaml \
  --db /tmp/northwatch.db
```

Open <http://localhost:8080>. Components will stay `unknown` because
no watcher is connected to a cluster.

## Run against kind

Use kind when touching watcher behavior, the Helm chart, or the
container image.

```sh
mise trust
mise install

kind create cluster --name nw-demo
kubectl --context kind-nw-demo apply -f examples/basic/sample-deployment.yaml

make build
./northwatch serve \
  --config examples/basic/northwatch.yaml \
  --db /tmp/nw-demo.db \
  --kubeconfig ~/.kube/config \
  --kube-context kind-nw-demo \
  --allow-deactivate
```

Open <http://localhost:8080>. `Deployment/default/api-gateway` should
move to `operational` once the deployment is available.

Clean up with:

```sh
kind delete cluster --name nw-demo
```

## Incident write endpoints

Read endpoints are public. POST endpoints require a bearer token with
at least 16 characters.

```sh
export NORTHWATCH_API_TOKEN=dev-token-123456
./northwatch serve --no-cluster --config /tmp/northwatch.yaml --db /tmp/northwatch.db
```

If the token is omitted, NorthWatch serves reads and returns 401 for
writes. If `--api-token` or `NORTHWATCH_API_TOKEN` is configured but
empty, boot fails. If the configured token is shorter than 16 characters,
boot also fails.

## Database files

The binary default SQLite path is
`$XDG_DATA_HOME/northwatch/northwatch.db`, falling back to
`~/.local/share/northwatch/northwatch.db`. Pass `--db` or set
`NORTHWATCH_DB` to keep local runs isolated.

The container image defaults to
`/var/lib/northwatch/northwatch.db`. Mount a volume there when testing
container persistence across restarts.

## Watcher development

Watcher changes need Kubernetes tests or a local cluster check. Unit
tests cover mapping and informer behavior with fakes; the e2e path
uses kind, Docker, and the Helm chart:

```sh
make e2e
```

The e2e loop creates a kind cluster, builds and loads the image, runs
the product e2e tests, and removes the cluster unless
`E2E_KEEP_CLUSTER` is set.
