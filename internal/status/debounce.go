package status

import (
	"sync"
	"time"

	"k8s.io/utils/clock"

	"github.com/northwatchlabs/northwatch/internal/component"
)

// RetryFunc is invoked by the Debouncer when a pending downward
// transition's window elapses. It receives the same key originally
// passed to Apply. Implementations should re-resolve the upstream
// object (via an informer lister) and re-enter the watcher's
// handle() loop. nil is allowed at construction (no retry
// scheduling).
type RetryFunc func(key string)

// Debouncer applies a grace period to downward status transitions
// per-key. All exported methods are safe for concurrent use.
type Debouncer struct {
	window time.Duration
	clk    clock.WithDelayedExecution
	retry  RetryFunc

	mu       sync.Mutex
	pending  map[string]*pendingEntry
	stopped  bool
	inflight sync.WaitGroup
}

type pendingEntry struct {
	target component.Status
	since  time.Time
	timer  clock.Timer
}

// NewDebouncer constructs a Debouncer with the given window. A zero
// window disables debouncing entirely (Apply returns write=true
// immediately for any non-trivial transition). retry may be nil; in
// that case Apply still returns retryAfter > 0 on first downward,
// but no timer is scheduled.
func NewDebouncer(window time.Duration, clk clock.WithDelayedExecution, retry RetryFunc) *Debouncer {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &Debouncer{
		window:  window,
		clk:     clk,
		retry:   retry,
		pending: make(map[string]*pendingEntry),
	}
}

// Apply decides the (write, status, retryAfter) tuple for the
// transition (prev → next) for key at time now. Side-effect on
// pending state is described in the spec's state table.
func (d *Debouncer) Apply(
	key string,
	prev, next component.Status,
	now time.Time,
) (write bool, status component.Status, retryAfter time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.stopped {
		return false, prev, 0
	}

	// Zero-window shortcut: skip the state machine entirely.
	if d.window == 0 {
		d.clearPendingLocked(key)
		if prev == next {
			return false, prev, 0
		}
		return true, next, 0
	}

	// prev == next: no write. Clear any stale pending whose target
	// differs from next (the recovery-clears-pending case where
	// prev/next both happen to read the same value from the store
	// because the previous downward was suppressed).
	if prev == next {
		if p, ok := d.pending[key]; ok && p.target != next {
			d.clearPendingLocked(key)
		}
		return false, prev, 0
	}

	// Unknown involvement is immediate in both directions.
	if prev == component.StatusUnknown || next == component.StatusUnknown {
		d.clearPendingLocked(key)
		return true, next, 0
	}

	if isUpward(prev, next) {
		d.clearPendingLocked(key)
		return true, next, 0
	}

	// Downward transition.
	p, exists := d.pending[key]
	if !exists || p.target != next {
		// Different downward target invalidates any existing entry.
		d.clearPendingLocked(key)
		d.recordPendingLocked(key, next, now)
		return false, prev, d.window
	}
	elapsed := now.Sub(p.since)
	if elapsed >= d.window {
		d.clearPendingLocked(key)
		return true, next, 0
	}
	return false, prev, d.window - elapsed
}

// recordPendingLocked must be called with d.mu held. It schedules
// the per-key retry timer via the injected clock so fake-clock
// tests can drive timing deterministically.
func (d *Debouncer) recordPendingLocked(key string, target component.Status, now time.Time) {
	entry := &pendingEntry{target: target, since: now}
	if d.retry != nil && d.window > 0 {
		entry.timer = d.clk.AfterFunc(d.window, func() { d.fire(key) })
	}
	d.pending[key] = entry
}

// fire is the timer callback. Phase 1 (under lock): verify the
// Debouncer hasn't been stopped and that the pending entry still
// exists; register with the WaitGroup. Phase 2 (outside lock and
// off the timer goroutine): invoke the retry callback. Splitting
// the phases is required — invoking retry under the lock would
// deadlock when retry calls back into Apply(). Dispatching on a
// fresh goroutine is also required because some clock
// implementations (notably k8s.io/utils/clock/testing.FakeClock)
// fire AfterFunc synchronously while holding their own lock, and
// the retry callback typically reads the clock.
func (d *Debouncer) fire(key string) {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	if _, exists := d.pending[key]; !exists {
		d.mu.Unlock()
		return
	}
	r := d.retry
	d.inflight.Add(1)
	d.mu.Unlock()

	go func() {
		defer d.inflight.Done()
		if r != nil {
			r(key)
		}
	}()
}

// Forget clears any pending state for key and cancels its timer.
// Used by the watcher's retry callback when the lister reports the
// object no longer exists (deleted between pending-record and
// timer-fire). Without it, the pending entry would survive
// indefinitely and a same-ID recreate could appear to satisfy
// "elapsed >= window" on its first downward observation. Safe to
// call with an unknown key (no-op) and safe to call after Stop()
// (no-op).
func (d *Debouncer) Forget(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return
	}
	d.clearPendingLocked(key)
}

// clearPendingLocked must be called with d.mu held.
func (d *Debouncer) clearPendingLocked(key string) {
	if p, ok := d.pending[key]; ok {
		if p.timer != nil {
			p.timer.Stop()
		}
		delete(d.pending, key)
	}
}

// Stop cancels all outstanding per-key timers, marks the Debouncer
// stopped, and waits for any callback already past phase 1 to
// complete. After Stop() returns: no new timer will fire, no retry
// callback is in progress, and subsequent Apply() calls return
// (write=false, status=prev, retryAfter=0) without mutating state
// or scheduling timers. Safe to call once; second call is a no-op.
func (d *Debouncer) Stop() {
	d.mu.Lock()
	if d.stopped {
		d.mu.Unlock()
		return
	}
	for k, p := range d.pending {
		if p.timer != nil {
			p.timer.Stop()
		}
		delete(d.pending, k)
	}
	d.pending = nil
	d.stopped = true
	d.mu.Unlock()

	d.inflight.Wait()
}

// isUpward reports whether next is closer to operational than prev.
// Excludes the unknown rows (handled separately by Apply). Ranking
// (most-bad to least-bad): down < degraded < operational.
func isUpward(prev, next component.Status) bool {
	return statusRank(next) > statusRank(prev)
}

func statusRank(s component.Status) int {
	switch s {
	case component.StatusDown:
		return 0
	case component.StatusDegraded:
		return 1
	case component.StatusOperational:
		return 2
	default:
		return -1 // unknown — not used in directional comparisons.
	}
}
