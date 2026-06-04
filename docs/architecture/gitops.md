# GitOps Model

NorthWatch treats config as desired state. A component appears on the
status page because it is declared in `northwatch.yaml`, and its health
comes from the live Kubernetes resource with the same kind, namespace,
and name.

```yaml
components:
  - kind: Deployment
    namespace: default
    name: api-gateway
    displayName: "API Gateway"
```

Supported kinds today:

- `Deployment`
- `HelmRelease`
- `Kustomization`
- `Application`

`displayName` is optional. When it is omitted, NorthWatch uses `name`.

## Startup reconciliation

On boot, `cmd/northwatch` loads the config and calls
`Store.SyncComponents` with the desired component set.

The sync runs in one SQLite transaction:

- desired rows that do not exist are inserted as `unknown`
- desired rows that already exist keep their status and become active
- display names are updated from config
- rows no longer declared can be soft-deactivated

Soft-deactivation is gated. If config would remove active components,
NorthWatch refuses to boot unless `--allow-deactivate` or
`NORTHWATCH_ALLOW_DEACTIVATE=true` is set. This protects operators from
accidentally wiping the public watched set with a bad config file.

## Runtime reconciliation

After startup, watchers reconcile status from cluster events. The config
does not carry health. It only declares the resources NorthWatch should
pay attention to.

Each watcher filters events to configured namespaced names. Unconfigured
objects are ignored, even if the informer sees them. Configured objects
are mapped into NorthWatch component statuses and written to the store
after debounce rules are applied.

## Optional controllers

GitOps controllers are optional. A cluster can run only Deployments and
NorthWatch still serves normally.

Flux and ArgoCD watchers start only when their CRDs are present:

- Flux HelmRelease CRD present -> start HelmRelease watcher
- Flux Kustomization CRD present -> start Kustomization watcher
- ArgoCD Application CRD present -> start Application watcher

If a configured component refers to a kind whose CRD is absent, that
component remains at its stored status, commonly `unknown`, until a
matching watcher exists and observes it.

## No manual pings

NorthWatch does not ask operators to configure arbitrary HTTP checks for
Kubernetes workloads in the MVP. The wedge is resource-native health:
controller status is the source of truth.

External monitors are planned for a later phase. Keep the current
GitOps docs focused on declared resources, controller reconciliation,
and status conditions.
