# NorthWatch

NorthWatch is a single-binary status page for Kubernetes and GitOps
workloads. It reads a small YAML config, watches the matching cluster
resources, derives component health from resource status, and renders a
public status page with incident banners.

This documentation is for contributors working on NorthWatch itself.
It explains the current architecture, the status-derivation path, the
data model, the GitOps contract, and the local development loop.

## Start here

- [Architecture overview](architecture/overview.md) explains the main
  packages and runtime process.
- [Status derivation](architecture/status-derivation.md) explains how
  Kubernetes and GitOps resource status becomes component health.
- [Data model](architecture/data-model.md) explains the SQLite tables
  and domain objects.
- [GitOps model](architecture/gitops.md) explains how `northwatch.yaml`
  is reconciled.
- [Local setup](development/local-setup.md) covers tool bootstrap and
  common development commands.
- [Release process](development/release-process.md) summarizes CI,
  images, binaries, Helm, and docs publishing.

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
later roadmap items. Do not build contributor docs around those
surfaces until the code exists.
