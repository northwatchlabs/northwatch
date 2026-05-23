# NorthWatch Helm chart

Install [NorthWatch](https://github.com/northwatchlabs/northwatch) into a
Kubernetes cluster. NorthWatch is a status page that watches workloads
(`Deployment`, Flux `HelmRelease`, Flux `Kustomization`, ArgoCD
`Application`) and serves a public read-only page.

## TL;DR

```sh
kubectl create namespace northwatch
helm install nw ./deploy/helm/northwatch --namespace northwatch
kubectl -n northwatch port-forward svc/nw-northwatch 8080:8080
# open http://localhost:8080/
```

## Compatibility

This chart targets the **Helm 3 + Helm 4 common feature set**. CI runs
`helm lint` and the install smoke against both Helm 3.21.0 and Helm
4.2.0 on every PR, so either CLI works.

`kubeVersion` is `>=1.28.0-0`.

## Values

| Key | Default | Notes |
|---|---|---|
| `image.repository` | `ghcr.io/northwatchlabs/northwatch` | |
| `image.tag` | `""` | Falls back to `.Chart.AppVersion` when empty. |
| `image.pullPolicy` | `IfNotPresent` | |
| `replicas` | `1` | Single-replica MVP; leader election lands in v0.2+. |
| `serviceAccount.create` | `true` | |
| `rbac.create` | `true` | ClusterRole grants `get,list,watch` on Deployments, HelmReleases, Kustomizations, Applications. |
| `auth.existingSecret` | `""` | If set, references a pre-existing Secret holding the API token. |
| `auth.existingSecretKey` | `token` | Key inside `existingSecret`. |
| `auth.token` | `""` | Literal token written into a chart-managed Secret. Must be empty or at least 16 characters — shorter values fail schema validation. |
| `service.port` | `8080` | ClusterIP only; wire your own Ingress. |
| `resources.requests` | `50m` CPU / `64Mi` mem | Override per environment. |
| `resources.limits` | `500m` CPU / `256Mi` mem | |
| `config` | example component | YAML block rendered into `/etc/northwatch/northwatch.yaml`. See `examples/basic/northwatch.yaml`. |

## API token

The chart resolves the API token in this order:

1. `auth.existingSecret` — the chart does not render its own Secret;
   the Deployment references `secretKeyRef: {name: <existingSecret>,
   key: <existingSecretKey>}`.
2. `auth.token` — the chart renders a Secret with the literal value.
3. Neither set — the chart auto-generates a 32-char alphanumeric token
   on first install and preserves it across `helm upgrade` via
   `lookup`.

Retrieve the chart-managed token:

```sh
kubectl -n <ns> get secret <release>-northwatch-api-token \
  -o jsonpath='{.data.token}' | base64 -d
```

## Persistence

The chart uses `emptyDir` for the SQLite database at
`/var/lib/northwatch`. **Data is lost when the pod restarts.** PVC
support is planned for v0.2.

## Uninstall

```sh
helm uninstall nw --namespace northwatch
```

Cluster-scoped RBAC (`ClusterRole`, `ClusterRoleBinding`) is created
and deleted by Helm with the release — no manual cleanup required.

## Smoke test

```sh
make helm-smoke      # Helm 4 (the mise default)
make helm-smoke-v3   # Helm 3 (downloaded into a temp dir; no mise change)
```

Both create a kind cluster, build and load the chart's default image,
install with no `--set image.*` overrides, apply
`examples/basic/sample-deployment.yaml`, and assert
`/api/components` reports the watched component `operational` before
tearing the cluster down.
