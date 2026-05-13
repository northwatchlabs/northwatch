# NorthWatch

Open-source, GitOps-native, Kubernetes-aware status page with first-class
incident management. Single Go binary, single config file, Apache 2.0.

NorthWatch derives component health directly from Kubernetes resources
(`status.conditions`, replica readiness) on `Deployment`, `HelmRelease`
(Flux), and `Application` (ArgoCD) — no manual ping configuration. Run
it as a container or install via Helm.

> Status: pre-v0.1.0. The MVP is under active development. APIs,
> configuration, and Helm chart values may change without notice until
> v0.1.0 ships.

## Get started

```sh
# Build and run locally
make build
./northwatch
```

Once the v0.1.0 MVP lands, the quickstart will be:

```sh
helm install northwatch oci://ghcr.io/northwatchlabs/charts/northwatch \
  --version <x.y.z> --values your-config.yaml
```

See [`docs/conventions.md`](docs/conventions.md) for the foundational
naming decisions (Go module path, container registry, Helm OCI path)
that downstream tooling depends on.

## Develop

Tool versions are pinned in [`.tool-versions`](./.tool-versions) and
managed with [mise](https://mise.jdx.dev/) (asdf-compatible). Install
once, then trust the project on first `cd`:

```sh
brew install mise                            # or see https://mise.jdx.dev/installing-mise.html
echo 'eval "$(mise activate zsh)"' >> ~/.zshrc   # bash/fish: see mise docs
mise trust && mise install                   # installs go, golangci-lint
```

Then:

```sh
make build    # compile cmd/northwatch
make test     # run unit tests
make vet      # go vet
make lint     # golangci-lint
make run      # build and run
```

The existing `.tool-versions` file is also read by asdf if you prefer
to stick with that.

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
