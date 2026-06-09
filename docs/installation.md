# Installation

NorthWatch can run from the Helm chart, from the published container
image, or as a local binary. The Helm chart is the normal Kubernetes
installation path.

## Helm chart

The chart lives in `deploy/helm/northwatch` and installs the
`ghcr.io/northwatchlabs/northwatch` image. For a released image, pin the
image tag explicitly:

```sh
kubectl create namespace northwatch
helm install nw deploy/helm/northwatch \
  --namespace northwatch \
  --set image.tag=0.1.0 \
  --set-file config=examples/basic/northwatch.yaml
```

The chart renders `config` into `/etc/northwatch/northwatch.yaml`,
creates a ClusterIP service on port `8080`, and grants `get`, `list`,
and `watch` permissions for the supported Kubernetes and GitOps
resources.

The chart values reference lives in the repository at
[`deploy/helm/northwatch/README.md`](https://github.com/northwatchlabs/northwatch/blob/main/deploy/helm/northwatch/README.md).

## API token

Read endpoints are public. Incident write endpoints require a bearer
token.

The chart resolves the token in this order:

1. `auth.existingSecret`
2. `auth.token`
3. an auto-generated chart-managed Secret

When using `auth.existingSecret`, the referenced key must contain a
non-empty token value. A missing key prevents the pod from starting, and
an empty value causes NorthWatch to fail startup instead of silently
disabling incident writes.

Retrieve the chart-managed token:

```sh
kubectl -n northwatch get secret nw-northwatch-api-token \
  -o jsonpath='{.data.token}' | base64 --decode
```

## Binary

Release archives are attached to GitHub releases. The binary can also be
built from source:

```sh
mise trust
mise install
make build
```

Run against a local kubeconfig:

```sh
./northwatch serve \
  --config examples/basic/northwatch.yaml \
  --db ./northwatch.db \
  --kubeconfig ~/.kube/config \
  --kube-context kind-nw-demo \
  --api-token dev-token-123456
```

Open <http://localhost:8080>.

## Database path

The binary default SQLite path is
`$XDG_DATA_HOME/northwatch/northwatch.db`, falling back to
`~/.local/share/northwatch/northwatch.db`. The container image defaults
to `/var/lib/northwatch/northwatch.db`.

The current Helm chart uses `emptyDir` for that directory, so data is
lost when the pod restarts. PVC support is planned for a later release.
