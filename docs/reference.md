# Reference

## Component kinds

| Kind | Source |
|---|---|
| `Deployment` | Kubernetes `apps/v1` Deployment |
| `HelmRelease` | Flux HelmRelease (`helm.toolkit.fluxcd.io`) |
| `Kustomization` | Flux Kustomization (`kustomize.toolkit.fluxcd.io`) |
| `Application` | ArgoCD Application (`argoproj.io`) |

## Status values

| Status | Meaning |
|---|---|
| `unknown` | NorthWatch has no trusted signal yet. |
| `operational` | The resource reports healthy or ready. |
| `degraded` | The resource is progressing, partially ready, or otherwise impaired but not down. |
| `down` | The resource reports a failed, stalled, missing, or unavailable state. |

## CLI

```text
northwatch [subcommand] [flags]
```

Subcommands:

| Subcommand | Description |
|---|---|
| `serve` | Run the status-page HTTP server. Used by default when no subcommand is passed. |
| `migrate` | Apply pending SQLite migrations and exit. |

`serve` flags:

| Flag | Environment variable | Default |
|---|---|---|
| `--addr` | `NORTHWATCH_ADDR` | `:8080` |
| `--db` | `NORTHWATCH_DB` | `./northwatch.db` |
| `--config` | `NORTHWATCH_CONFIG` | `northwatch.yaml` |
| `--allow-deactivate` | `NORTHWATCH_ALLOW_DEACTIVATE` | `false` |
| `--kubeconfig` | `NORTHWATCH_KUBECONFIG` | auto-detected |
| `--kube-context` | `NORTHWATCH_KUBE_CONTEXT` | current context |
| `--no-cluster` | `NORTHWATCH_NO_CLUSTER` | `false` |
| `--api-token` | `NORTHWATCH_API_TOKEN` | empty |
| `--poll-seconds` | `NORTHWATCH_POLL_SECONDS` | `5` |
| `--debounce-seconds` | `NORTHWATCH_DEBOUNCE_SECONDS` | `60` |

## HTTP routes

| Method | Path | Auth | Notes |
|---|---|---|---|
| `GET` | `/` | none | Public status page. |
| `GET` | `/healthz` | none | Process health check. |
| `GET` | `/api/components` | none | JSON component list. |
| `GET` | `/api/incidents` | none | Active incidents by default. Use `?include=resolved` to include resolved incidents. |
| `GET` | `/api/status` | none | Rendered status section used by HTMX polling. |
| `POST` | `/incidents` | bearer token | Create an incident. |
| `POST` | `/incidents/{id}/resolve` | bearer token | Resolve an incident. |
