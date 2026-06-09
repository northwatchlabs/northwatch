# Configuration

NorthWatch has two configuration surfaces:

- `northwatch.yaml` declares the components shown on the status page.
- Runtime flags and environment variables configure the server,
  database, Kubernetes credentials, polling, debounce, and incident API.

## Component config

`northwatch.yaml` contains a `components` list:

```yaml
components:
  - kind: Deployment
    namespace: default
    name: api-gateway
    displayName: "API Gateway"
```

Supported `kind` values:

- `Deployment`
- `HelmRelease`
- `Kustomization`
- `Application`

`namespace` and `name` must match the Kubernetes object. `displayName`
is optional; when omitted, NorthWatch uses `name`.

## GitOps controller resources

Flux and ArgoCD controllers are optional. If the relevant CRD is absent,
NorthWatch logs that the watcher was skipped and continues serving the
remaining components. A configured component for a missing CRD commonly
stays `unknown` until the controller is installed and the resource is
observed.

## Runtime flags

Common `serve` flags:

| Flag | Environment variable | Default |
|---|---|---|
| `--addr` | `NORTHWATCH_ADDR` | `:8080` |
| `--db` | `NORTHWATCH_DB` | `$XDG_DATA_HOME/northwatch/northwatch.db` |
| `--config` | `NORTHWATCH_CONFIG` | `northwatch.yaml` |
| `--allow-deactivate` | `NORTHWATCH_ALLOW_DEACTIVATE` | `false` |
| `--kubeconfig` | `NORTHWATCH_KUBECONFIG` | auto-detected |
| `--kube-context` | `NORTHWATCH_KUBE_CONTEXT` | current context |
| `--no-cluster` | `NORTHWATCH_NO_CLUSTER` | `false` |
| `--api-token` | `NORTHWATCH_API_TOKEN` | empty |
| `--poll-seconds` | `NORTHWATCH_POLL_SECONDS` | `5` |
| `--debounce-seconds` | `NORTHWATCH_DEBOUNCE_SECONDS` | `60` |

`--api-token` and `NORTHWATCH_API_TOKEN` must be omitted or at least 16
characters. Omission disables write endpoints; public reads still work.
A configured empty token is invalid and NorthWatch refuses to boot.

## Safe component removal

On startup, NorthWatch reconciles the configured component set with the
SQLite database. New components are inserted as `unknown`, existing
components keep their status, and display names are updated.

If a config change would deactivate an existing component, NorthWatch
refuses to boot unless `--allow-deactivate` or
`NORTHWATCH_ALLOW_DEACTIVATE=true` is set. This prevents a bad config
from accidentally removing the public watched set.
