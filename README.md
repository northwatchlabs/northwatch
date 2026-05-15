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

## Try it locally

Walks the current functionality end-to-end against a real Kubernetes
cluster. Prerequisites: [mise](https://mise.jdx.dev/) (drives every
other tool — see [Develop](#develop)) and a running Docker daemon
(for `kind`).

### 1. Build the binary

```sh
mise trust && mise install
make build
```

### 2. Watch a Deployment

```sh
kind create cluster --name nw-demo
kubectl --context kind-nw-demo apply -f examples/basic/sample-deployment.yaml
./northwatch serve \
    --config examples/basic/northwatch.yaml \
    --db /tmp/nw-demo.db \
    --kubeconfig ~/.kube/config \
    --kube-context kind-nw-demo \
    --allow-deactivate
```

Open <http://localhost:8080>. **API Gateway** shows as `operational`.
In another terminal, scale the workload and refresh:

```sh
kubectl --context kind-nw-demo -n default scale deploy/api-gateway --replicas=0
# status flips to "down" within ~1s

kubectl --context kind-nw-demo -n default scale deploy/api-gateway --replicas=3
# status returns to "operational" once the rollout completes
```

The component config (`examples/basic/northwatch.yaml`) also lists
`HelmRelease/flux-system/cert-manager` and an ArgoCD `Application`,
which will show as `unknown` until those controllers are installed.
The CRD probe logs `"Flux CRDs not present, skipping helmrelease
watcher"` and continues — that's expected.

### 3. (Optional) Watch a HelmRelease

To exercise the HelmRelease watcher, install Flux's source and helm
controllers and apply a real release:

```sh
flux --context kind-nw-demo install \
    --components=source-controller,helm-controller

kubectl --context kind-nw-demo apply -f - <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata: { name: podinfo, namespace: default }
spec:
  interval: 5m
  url: https://stefanprodan.github.io/podinfo
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: { name: podinfo, namespace: default }
spec:
  interval: 5m
  chart:
    spec:
      chart: podinfo
      version: '6.x'
      sourceRef: { kind: HelmRepository, name: podinfo }
EOF
```

Point a config entry at `HelmRelease/default/podinfo`, restart
`northwatch`, and the release renders as `operational` once Flux
installs it. To watch it flip to `down`, patch the chart to a
non-existent version:

```sh
kubectl --context kind-nw-demo -n default patch hr podinfo \
    --type merge -p '{"spec":{"chart":{"spec":{"version":"999.0.0"}}}}'
```

### Cleanup

```sh
kind delete cluster --name nw-demo
```

### What's next

The v0.1.0 MVP installs as:

```sh
helm install northwatch oci://ghcr.io/northwatchlabs/charts/northwatch \
  --version <x.y.z> --values your-config.yaml
```

Tracking issues for the full incident-management story (live banner
driven by `POST /incidents`) and the Helm chart are in the
[v0.1.0 milestone](https://github.com/northwatchlabs/northwatch/milestone/1).

See [`docs/conventions.md`](docs/conventions.md) for the foundational
naming decisions (Go module path, container registry, Helm OCI path)
that downstream tooling depends on.

## Develop

Tool versions are pinned in [`mise.toml`](./mise.toml) and managed
with [mise](https://mise.jdx.dev/). Install once, then trust and
install the project's tools:

```sh
brew install mise                                # or see https://mise.jdx.dev/installing-mise.html
echo 'eval "$(mise activate zsh)"' >> ~/.zshrc   # bash/fish: see mise docs
mise trust && mise install                       # installs go, golangci-lint, tailwindcss, kind, kubectl, kubectx, flux
```

Then:

```sh
make build    # compile cmd/northwatch
make test     # run unit tests
make vet      # go vet
make lint     # golangci-lint
make css      # compile Tailwind CSS
make run      # build and run
```

## License

Apache 2.0 — see [`LICENSE`](LICENSE).
