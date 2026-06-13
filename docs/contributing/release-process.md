# Release Process

NorthWatch ships from GitHub Actions. Contributors usually interact
with the verification workflows on pull requests; maintainers cut
releases by pushing `v*` tags.

## Pull request verification

The main PR workflow runs:

- `go vet ./...`
- `go test -race ./...`
- golangci-lint
- cross-platform builds for linux and darwin on amd64 and arm64
- a goreleaser snapshot build

- MkDocs strict build when docs change
- container image build, smoke test, non-root runtime assertion, and
  multi-arch build validation
- Helm chart lint and smoke tests with Helm 3 and Helm 4
- product e2e tests in kind

Run the focused local equivalent before pushing changes. For docs-only
changes, `mise run docs-build` is usually enough. For Go behavior,
run `make test` plus the narrower commands relevant to the touched
surface.

## Binaries

`.goreleaser.yaml` builds the `northwatch` binary from
`./cmd/northwatch` for:

- linux/amd64
- linux/arm64
- darwin/amd64
- darwin/arm64

Release archives are `tar.gz` files and the release includes
`checksums.txt`. GitHub releases are created as drafts.

The release workflow runs goreleaser on pushed `v*` tags. Manual
workflow dispatch runs a snapshot dry run and skips publishing.

## Container image

The image workflow builds from `deploy/docker/Dockerfile` and publishes
to:

```text
ghcr.io/northwatchlabs/northwatch
```

Published tags include branch, SHA, semver, major/minor, and `latest`
for the default branch. Published images are signed with cosign keyless
OIDC.

The Dockerfile builds a static Go binary with `CGO_ENABLED=0`, copies
it into a distroless non-root runtime image, and defaults SQLite to:

```text
/var/lib/northwatch/northwatch.db
```

## Helm chart

The in-tree chart lives at `deploy/helm/northwatch`. Chart verification
uses `helm lint`, builds a matching NorthWatch image from the chart
`appVersion`, installs the chart into kind, waits for the deployment,
checks `/healthz`, waits on the `/readyz` readiness probe through the
deployment, checks component status through `/api/components`,
and verifies cluster-scoped RBAC is removed on uninstall.

The project convention for the future OCI chart path is:

```text
oci://ghcr.io/northwatchlabs/charts/northwatch
```

See [Project conventions](conventions.md) before changing public names,
registry paths, module paths, or chart paths.

## Docs site

Docs are built with MkDocs Material. Pull requests run
`mkdocs build --strict` through `mise run docs-build` when docs-related
paths change.

Merges to `main` deploy the docs site through:

```sh
mise run docs-deploy
```

The deploy workflow publishes to GitHub Pages with `mkdocs gh-deploy`.
