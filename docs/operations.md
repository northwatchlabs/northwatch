# Operations

This page covers the day-to-day behavior operators need to know when
running NorthWatch.

## Health check

NorthWatch exposes:

```text
GET /healthz
```

The endpoint returns `200 OK` with `ok` when the HTTP process is
running. It does not prove every configured Kubernetes watcher has
observed its resource.

## Status refresh

Watchers write status into SQLite. The browser does not receive push
updates; it polls `/api/status` through HTMX. The default interval is 5
seconds and can be changed with `--poll-seconds` or
`NORTHWATCH_POLL_SECONDS`.

Downward transitions are debounced for 60 seconds by default. Upward
transitions write immediately. Set `--debounce-seconds=0` to disable
debounce for local testing.

## Incident writes

Incident writes require `Authorization: Bearer <token>`. The token is
configured with `--api-token`, `NORTHWATCH_API_TOKEN`, or the Helm chart
`auth` values.

For binary/env/flag runs, omit the API token to disable incident writes.
For Helm installs, omitting both `auth.existingSecret` and `auth.token`
keeps writes enabled because the chart generates a token. If the token is
configured with `--api-token`, `NORTHWATCH_API_TOKEN`, or Helm auth
values, it must be non-empty and at least 16 characters;
configured-empty tokens fail startup.

Create an incident:

```sh
curl -fsS -X POST http://localhost:8080/incidents \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"title":"Investigating elevated 5xx errors","component":"Deployment/default/api-gateway"}'
```

Resolve an incident:

```sh
curl -fsS -X POST "http://localhost:8080/incidents/$ID/resolve" \
  -H "Authorization: Bearer $TOKEN"
```

## Config changes

Additive config changes are safe to roll out normally. Existing
components keep their current status when the process restarts.

When removing components, first confirm the new config is intentional.
Then restart with `--allow-deactivate` or
`NORTHWATCH_ALLOW_DEACTIVATE=true` so NorthWatch can mark removed
components inactive.

## Missing CRDs

Flux and ArgoCD watchers start only when the corresponding CRDs are
served by the cluster. Missing CRDs are not fatal. NorthWatch logs the
skipped watcher and continues serving other components.
