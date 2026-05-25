# NorthWatch

Open-source, GitOps-native, Kubernetes-aware status page with first-class
incident management. Single Go binary, single config file, Apache 2.0.

NorthWatch derives component health directly from Kubernetes resources
(`status.conditions`, replica readiness) on `Deployment`, Flux
`HelmRelease`, Flux `Kustomization`, and ArgoCD `Application` — no
manual ping configuration. Run it as a container, install via Helm, or
run the binary directly against a kubeconfig.

> **v0.1.0 — MVP is live.** Container image:
> [`ghcr.io/northwatchlabs/northwatch:0.1.0`](https://github.com/northwatchlabs/northwatch/pkgs/container/northwatch).
> Binaries + checksums on the
> [v0.1.0 Release](https://github.com/northwatchlabs/northwatch/releases/tag/v0.1.0).

## Quickstart — Helm

The fastest path to the killer demo. Prerequisites:
[mise](https://mise.jdx.dev/) (drives every tool — see [Develop](#develop))
and Docker (for `kind`).

```sh
# 1. Bring up a local cluster + the workload to watch.
mise trust && mise install
kind create cluster --name nw-demo
kubectl --context kind-nw-demo apply -f examples/basic/sample-deployment.yaml

# 2. Install NorthWatch from the in-tree chart. Pin the published image
#    tag so you're running v0.1.0 exactly.
kubectl --context kind-nw-demo create namespace northwatch
helm --kube-context kind-nw-demo install nw deploy/helm/northwatch \
    --namespace northwatch \
    --set image.tag=0.1.0 \
    --set-file config=examples/basic/northwatch.yaml

# 3. Port-forward, then visit the status page in your browser.
kubectl --context kind-nw-demo -n northwatch port-forward svc/nw-northwatch 8080:8080 &
# http://localhost:8080
```

**API Gateway** shows as `operational`. Scale the workload to zero and
refresh; it flips to `down` within ~1s. Scale back; it returns to
`operational` once the rollout completes.

### Killer demo: incident banner

The chart auto-generates a bearer token. Retrieve it, then open and
resolve an incident:

```sh
TOKEN=$(kubectl --context kind-nw-demo -n northwatch get secret \
    nw-northwatch-api-token -o jsonpath='{.data.token}' | base64 --decode)

# Open an incident — a red banner appears on the status page.
RESPONSE=$(curl -fsS -X POST http://localhost:8080/incidents \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d '{"title":"Investigating elevated 5xx errors","component":"Deployment/default/api-gateway"}')
ID=$(printf '%s' "$RESPONSE" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')

# Resolve it — the banner clears.
curl -fsS -X POST "http://localhost:8080/incidents/$ID/resolve" \
    -H "Authorization: Bearer $TOKEN"
```

### Cleanup

```sh
kind delete cluster --name nw-demo
```

## Quickstart — binary

For development, smoke testing, or environments where containers aren't
the right tool. Run the binary against any reachable cluster.

```sh
mise trust && mise install
make build

kind create cluster --name nw-demo
kubectl --context kind-nw-demo apply -f examples/basic/sample-deployment.yaml

./northwatch serve \
    --config examples/basic/northwatch.yaml \
    --db /tmp/nw-demo.db \
    --kubeconfig ~/.kube/config \
    --kube-context kind-nw-demo \
    --allow-deactivate
```

Open <http://localhost:8080>. **API Gateway** shows as `operational`;
scaling the deployment exercises the status transitions described
above. The incident API requires `NORTHWATCH_API_TOKEN` to be set —
export a 16+ char value before launching `northwatch serve` to enable
write endpoints.

The component config (`examples/basic/northwatch.yaml`) also lists
`HelmRelease/flux-system/cert-manager`, an ArgoCD `Application`, and a
Flux `Kustomization`, which will show as `unknown` until those
controllers are installed. The CRD probes log
`"Flux HelmRelease CRDs not present, skipping helmrelease watcher"`
(and equivalent for ArgoCD / Kustomize) and continue — that's
expected.

### (Optional) Watch a HelmRelease

To exercise the HelmRelease watcher, install Flux's source and helm
controllers and apply a real release:

```sh
flux --context kind-nw-demo install \
    --components=source-controller,helm-controller

bash -c "kubectl --context kind-nw-demo apply -f - <<'EOF'
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
"
```

Point a config entry at `HelmRelease/default/podinfo`, restart
`northwatch`, and the release renders as `operational` once Flux
installs it. To watch it flip to `down`, patch the chart to a
non-existent version:

```sh
kubectl --context kind-nw-demo -n default patch hr podinfo \
    --type merge -p '{"spec":{"chart":{"spec":{"version":"999.0.0"}}}}'
```

### (Optional) Watch a Flux Kustomization

To exercise the Kustomization watcher, install Flux's source and
kustomize controllers and apply a real Kustomization. The recipe
below uses `stefanprodan/podinfo` because it ships a ready-to-use
`/kustomize/` path — no scratch repo needed:

```sh
flux --context kind-nw-demo install \
    --components=source-controller,kustomize-controller

bash -c "kubectl --context kind-nw-demo apply -f - <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: { name: podinfo, namespace: default }
spec:
  interval: 5m
  url: https://github.com/stefanprodan/podinfo
  ref: { branch: master }
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata: { name: podinfo, namespace: default }
spec:
  interval: 5m
  path: ./kustomize
  prune: true
  sourceRef: { kind: GitRepository, name: podinfo }
  targetNamespace: default
EOF
"
```

Point a config entry at `Kustomization/default/podinfo`, restart
`northwatch`, and the Kustomization renders as `operational` once
Flux reconciles it. To watch it flip to `down`, break the path:

```sh
kubectl --context kind-nw-demo -n default patch ks podinfo \
    --type merge -p '{"spec":{"path":"./does-not-exist"}}'
```

## What's shipped in v0.1.0

- Watchers for Kubernetes `Deployment`, Flux `HelmRelease` (v2,
  v2beta1, v2beta2), Flux `Kustomization`, and ArgoCD `Application`.
- Status mapping with kstatus-aware freshness and a 60s debounce so
  rolling updates don't flicker the page.
- Read-only public status page — server-rendered HTML + HTMX +
  Tailwind, embedded in the binary.
- Incident API — `POST /incidents`, `POST /incidents/{id}/resolve` —
  gated by a single bearer token (`NORTHWATCH_API_TOKEN` /
  `auth.token` in the chart).
- SQLite store with migrations.
- Multi-arch container image (linux/amd64 + arm64), distroless,
  non-root, cosign-signed.
- Helm chart at `deploy/helm/northwatch` — installs in <30s. See
  [`deploy/helm/northwatch/README.md`](deploy/helm/northwatch/README.md)
  for values reference.

The roadmap for v0.2.0+ (Postgres, external HTTP monitors,
notifications, CLI, OCI-published chart) lives in the
[GitHub milestones](https://github.com/northwatchlabs/northwatch/milestones).

See [`docs/conventions.md`](docs/conventions.md) for the foundational
naming decisions (Go module path, container registry, Helm OCI path)
that downstream tooling depends on.

## Develop

Tool versions are pinned in [`mise.toml`](./mise.toml) and managed
with [mise](https://mise.jdx.dev/). Install once, then trust and
install the project's tools:

```sh
brew install mise   # or see https://mise.jdx.dev/installing-mise.html
```

Activate `mise` in your shell so its shims land on `$PATH`. Pick the
line for your shell and append it to your rc file:

```sh
# zsh
echo 'eval "$(mise activate zsh)"' >> ~/.zshrc

# bash
echo 'eval "$(mise activate bash)"' >> ~/.bashrc

# fish
echo 'mise activate fish | source' >> ~/.config/fish/config.fish
```

Open a new shell (or `source` the rc file), then install the
project's tools:

```sh
mise trust && mise install   # installs go, golangci-lint, tailwindcss, kind, kubectl, kubectx, flux
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
