package incident

import "errors"

// ErrUnknownComponent is returned by Service.CreateIncident when the
// component_id does not match any row in the components table.
// Handlers map this to HTTP 400.
var ErrUnknownComponent = errors.New("incident: unknown component")

// ErrTitleRequired is returned by Service.CreateIncident when the
// title is empty after whitespace trimming. Handlers map this to
// HTTP 400.
var ErrTitleRequired = errors.New("incident: title required")

// ErrNotFound is the shared "row not found" sentinel for store lookups.
// The store package re-exports this as store.ErrNotFound so existing
// callers that check errors.Is(err, store.ErrNotFound) keep working.
// Living here (in the leaf incident package) breaks what would
// otherwise be a circular dependency between incident.Service and
// the store package.
var ErrNotFound = errors.New("not found")
