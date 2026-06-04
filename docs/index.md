# NorthWatch

NorthWatch is a single-binary status page for Kubernetes and GitOps
workloads. It reads a small YAML config, watches the matching cluster
resources, derives component health from resource status, and renders a
public status page with incident banners.

This documentation is for people installing, configuring, running, and
operating NorthWatch. It covers the user-facing behavior of the product:
what NorthWatch watches, how it maps Kubernetes state into status page
health, how to configure the binary or Helm chart, and how to operate the
incident workflow.

Contributors should start in [Contributing](contributing/index.md).

## Start here

- [Getting Started](getting-started.md) walks through a local demo with
  kind and the in-tree Helm chart.
- [Installation](installation.md) covers the Helm chart and binary run
  modes.
- [Configuration](configuration.md) explains `northwatch.yaml`, runtime
  flags, environment variables, and the incident API token.
- [Operations](operations.md) covers day-to-day checks, status behavior,
  incident writes, and safe config changes.
- [Concepts](concepts/index.md) explains the runtime model, status
  derivation, data model, and GitOps contract.
- [Reference](reference.md) lists the current CLI flags, environment
  variables, HTTP routes, and supported component kinds.

## Current product scope

NorthWatch v0.1 ships:

- watched components declared in `northwatch.yaml`
- source kinds: `Deployment`, Flux `HelmRelease`, Flux
  `Kustomization`, and ArgoCD `Application`
- status states: `unknown`, `operational`, `degraded`, `down`
- SQLite persistence with embedded migrations
- server-rendered HTML with HTMX polling
- bearer-token-protected incident create and resolve endpoints
- a container image and an in-tree Helm chart

Postgres, external monitors, notification channels, a separate CLI,
subscribers, NorthWatch-owned CRDs, and operator-managed resources are
later roadmap items.
