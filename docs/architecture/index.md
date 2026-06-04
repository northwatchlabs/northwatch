# Architecture

NorthWatch keeps the MVP architecture deliberately small:

- one Go command in `cmd/northwatch`
- one config file, parsed by `internal/config`
- one persistence interface in `internal/store`
- resource-specific Kubernetes watchers in `internal/watcher`
- status mapping and debouncing in `internal/status`
- HTTP routes and templates in `internal/server` and `internal/ui`

Read these pages in order when changing runtime behavior:

1. [Overview](overview.md)
2. [Status derivation](status-derivation.md)
3. [Data model](data-model.md)
4. [GitOps model](gitops.md)
