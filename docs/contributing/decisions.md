# Decision Log

This file captures major NorthWatch decisions that future product,
architecture, documentation, and agent work must preserve.

Only durable, high-cost decisions belong here. Do not record routine
implementation choices, short-lived issue plans, minor refactors, or
details that are already obvious from code. If a decision is limited to
one issue or PR, keep it in that issue, PR, spec, or implementation plan.

Use [Project conventions](conventions.md) for public naming decisions
that become hard to rename once shipped, such as module paths, API
groups, registries, and published artifact names.

## When To Update This File

Update this file in the same PR that makes or changes a major durable
decision. A decision usually belongs here when at least one of these is
true:

- future issues must follow it even when the original issue is closed
- reversing it would break users, operators, contributors, or public
  artifacts
- it defines product scope, architecture boundaries, supported
  workflows, documentation ownership, or release/distribution policy
- it settles a repeated ambiguity that agents or contributors would
  otherwise rediscover

Keep entries short. Link to issues, PRs, docs, or code for detail.

## Active Decisions

### D001: Kubernetes Resource Status Is The Product Wedge

- Date: 2026-05-03
- Status: active
- Decision: NorthWatch derives component health from Kubernetes and
  GitOps resource state first. Manual HTTP/TCP/DNS checks are deferred
  roadmap work, not part of the v0.1 product wedge.
- Rationale: The differentiator is a status page that understands the
  resources platform teams already operate. Requiring operators to
  duplicate Kubernetes health as manual ping checks weakens that value.
- Implications:
  - `northwatch.yaml` declares resources to watch; it does not carry
    health.
  - Kubernetes controller status is the source of truth for watched
    workloads.
  - External monitors should remain additive and later-phase.
- Sources: [GitOps Model](../concepts/gitops.md),
  [Status Derivation](../concepts/status-derivation.md), issues
  [#16](https://github.com/northwatchlabs/northwatch/issues/16),
  [#17](https://github.com/northwatchlabs/northwatch/issues/17),
  [#50](https://github.com/northwatchlabs/northwatch/issues/50).

### D002: NorthWatch Ships As One Go Binary With One Config File

- Date: 2026-05-03
- Status: active
- Decision: NorthWatch is a single Go binary driven by one YAML config
  file. It embeds the web UI and serves the public status page directly.
- Rationale: The MVP should be easy to run locally, install with Helm,
  package as a container, and reason about operationally.
- Implications:
  - No separate frontend service or TypeScript client is part of the
    MVP.
  - Runtime config is centered on `northwatch.yaml`, CLI flags, and
    environment variables.
  - The server-rendered UI and static assets are embedded in the binary.
- Sources: [Runtime Model](../concepts/overview.md), README, PRs
  [#27](https://github.com/northwatchlabs/northwatch/pull/27),
  [#28](https://github.com/northwatchlabs/northwatch/pull/28).

### D003: SQLite Is The Current Persistence Backend

- Date: 2026-05-13
- Status: active
- Decision: SQLite is the only persistence backend in v0.1. Postgres is
  deferred to a later release.
- Rationale: SQLite keeps the first install simple while still allowing
  durable components, incidents, timelines, status history, and embedded
  migrations.
- Implications:
  - Schema changes must use migrations.
  - Code should keep the store boundary clear enough for a later
    Postgres backend, but should not prebuild speculative Postgres
    behavior.
  - Component removal is soft-deactivation guarded by
    `--allow-deactivate`, preserving incident and status history.
- Sources: [Data Model](../concepts/data-model.md),
  [GitOps Model](../concepts/gitops.md), PRs
  [#29](https://github.com/northwatchlabs/northwatch/pull/29),
  [#33](https://github.com/northwatchlabs/northwatch/pull/33).

### D004: Status Uses Four Public States With Debounced Downward Moves

- Date: 2026-05-15
- Status: active
- Decision: Public component health is expressed as `unknown`,
  `operational`, `degraded`, or `down`. Watchers map native resource
  conditions into those states and debounce downward transitions.
- Rationale: The status page needs a small stable vocabulary while
  avoiding false alarms during normal Kubernetes and GitOps rollouts.
- Implications:
  - New watched resource kinds must map into the same four states unless
    a separate durable decision changes the public status model.
  - Downward transitions are held for the debounce window; upward and
    `unknown` transitions write immediately.
  - Flux-style resources should use kstatus-aware `Stalled`, `Ready`,
    `Reconciling`, and condition freshness semantics.
- Sources: [Status Derivation](../concepts/status-derivation.md),
  issues [#57](https://github.com/northwatchlabs/northwatch/issues/57),
  [#70](https://github.com/northwatchlabs/northwatch/issues/70).

### D005: Public Docs Are User And Operator First

- Date: 2026-06-04
- Status: active
- Decision: The GitHub Pages documentation is primarily for people
  installing, configuring, running, and operating NorthWatch. Contributor
  material remains in the docs site under Contributing, but it is
  secondary to user/operator tasks and concepts.
- Rationale: Public docs should optimize for adoption and operation.
  Contributor process details add noise when mixed into user workflows.
- Implications:
  - The docs homepage and top-level nav should lead with user/operator
    tasks.
  - Contributor workflow belongs under `docs/contributing/`, README
    contributor links, GitHub issues, and agent workflow files.
  - The GitHub Wiki is not canonical documentation.
- Sources: issue
  [#86](https://github.com/northwatchlabs/northwatch/issues/86), PR
  [#87](https://github.com/northwatchlabs/northwatch/pull/87).

### D006: Public Naming Conventions Are Canonical And Rarely Changed

- Date: 2026-05-09
- Status: active
- Decision: Foundational public names are locked in
  [Project conventions](conventions.md), including the Go module path,
  future CRD API group, container image path, and Helm OCI path.
- Rationale: Once public artifacts ship, renaming them breaks users and
  downstream tooling.
- Implications:
  - Do not introduce alternate module paths, API groups, registries, or
    chart paths in code or docs.
  - Add new public naming conventions to
    [Project conventions](conventions.md), not this file.
- Sources: issue
  [#8](https://github.com/northwatchlabs/northwatch/issues/8), PR
  [#26](https://github.com/northwatchlabs/northwatch/pull/26).

## Superseded Decisions

None yet.
