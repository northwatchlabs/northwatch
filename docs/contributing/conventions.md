# Project Conventions

This document captures one-shot, irreversible-in-practice naming
decisions that the rest of the codebase, distribution, and downstream
consumers depend on. Once a public artifact ships under one of these
names — a tagged Go module, a published container image, a Helm chart
release, a CRD installed in a user's cluster — renaming it breaks every
consumer. Treat the values below as immutable; new conventions get
added, existing ones do not get edited.

## Foundational namespaces

| Concern               | Value                                          |
|-----------------------|------------------------------------------------|
| Go module path        | `github.com/northwatchlabs/northwatch`         |
| CRD API group         | `northwatch.dev/v1alpha1`                      |
| Container registry    | `ghcr.io/northwatchlabs/northwatch`            |
| Helm chart OCI path   | `oci://ghcr.io/northwatchlabs/charts/northwatch` |

### Go module path

`github.com/northwatchlabs/northwatch` — set in `go.mod` and reflected
in every internal import path. The module path is part of every
`import` statement in every consumer; renaming it breaks `go get` and
every downstream import.

### CRD API group

`northwatch.dev/v1alpha1` — locked now, applied in v0.3 (Phase 3) when
CRDs land. The MVP (v0.1.0) reads YAML config; CRDs are deferred but
the API group is fixed here so first-shipped CRDs match the eventual
GA group.

The owned domain is `northwatch.dev`. Do not use `northwatch.io`,
`northwatchlabs.dev`, or any variant — once a CRD installs in a user's
cluster the group is part of every `apiVersion` field in every stored
object, and a rename requires a conversion webhook plus migration.

### Container registry

`ghcr.io/northwatchlabs/northwatch` — the single image distributed by
this project. The Dockerfile and the GitHub Actions publish workflow
both target this path. Tags follow the release tag (`v0.1.0`, etc.)
plus `latest` on the most recent stable release.

### Helm chart OCI path

`oci://ghcr.io/northwatchlabs/charts/northwatch` — published as an OCI
artifact under the same org's GHCR namespace. Users install with:

```sh
helm install northwatch oci://ghcr.io/northwatchlabs/charts/northwatch --version <x.y.z>
```

The chart name (`northwatch`) and the OCI repository path are part of
every `helm install` / `helm upgrade` invocation; renaming either
forces every consumer to re-target.

## Adding a new convention

If a new naming decision has the same property — once it's public,
renaming breaks consumers — add it here in the same PR that introduces
it. Examples that would qualify: a second container image, a metrics
namespace prefix exposed to Prometheus, a public HTTP API path prefix.

Conventions that are easy to change later (internal package layout,
unexported types, log field names) do not belong here.
