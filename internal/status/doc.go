// Package status maps upstream Kubernetes objects (Deployment,
// HelmRelease, Application) to component.Status, and applies a
// per-key debouncer so transient downward blips during rolling
// updates do not surface as degraded/down on the status page.
//
// The package has no dependency on internal/watcher; watchers
// depend on this package, not the other way around. This is
// deliberate: the mapper functions are pure, and the Debouncer
// owns only timer state, so both are trivial to test in isolation.
package status
