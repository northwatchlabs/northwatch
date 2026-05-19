package status

import (
	"testing"
	"time"

	testingclock "k8s.io/utils/clock/testing"

	"github.com/northwatchlabs/northwatch/internal/component"
)

func TestDebouncerApply_StateTable(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const window = 60 * time.Second

	tests := []struct {
		name           string
		prev, next     component.Status
		wantWrite      bool
		wantStatus     component.Status
		wantRetryAfter time.Duration
	}{
		{"no-op when prev==next", component.StatusOperational, component.StatusOperational, false, component.StatusOperational, 0},
		{"unknown → anything (recovered) immediate", component.StatusUnknown, component.StatusDown, true, component.StatusDown, 0},
		{"anything → unknown (signal lost) immediate", component.StatusOperational, component.StatusUnknown, true, component.StatusUnknown, 0},
		{"upward → operational immediate", component.StatusDown, component.StatusOperational, true, component.StatusOperational, 0},
		{"upward down → degraded immediate", component.StatusDown, component.StatusDegraded, true, component.StatusDegraded, 0},
		{"downward operational → degraded first observation", component.StatusOperational, component.StatusDegraded, false, component.StatusOperational, window},
		{"downward operational → down first observation", component.StatusOperational, component.StatusDown, false, component.StatusOperational, window},
		{"downward degraded → down first observation", component.StatusDegraded, component.StatusDown, false, component.StatusDegraded, window},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clk := testingclock.NewFakeClock(t0)
			d := NewDebouncer(window, clk, nil)
			defer d.Stop()

			write, status, retry := d.Apply("k", tc.prev, tc.next, clk.Now())
			if write != tc.wantWrite {
				t.Errorf("write = %v, want %v", write, tc.wantWrite)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %q, want %q", status, tc.wantStatus)
			}
			if retry != tc.wantRetryAfter {
				t.Errorf("retryAfter = %v, want %v", retry, tc.wantRetryAfter)
			}
		})
	}
}

func TestDebouncerApply_ZeroWindowAlwaysWrites(t *testing.T) {
	t.Parallel()
	clk := testingclock.NewFakeClock(time.Now())
	d := NewDebouncer(0, clk, nil)
	defer d.Stop()

	write, status, _ := d.Apply("k", component.StatusOperational, component.StatusDown, clk.Now())
	if !write || status != component.StatusDown {
		t.Fatalf("zero-window downward got (write=%v, status=%q), want (true, down)", write, status)
	}
}

func TestDebouncer_TimerFiresRetry(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const window = 60 * time.Second

	retried := make(chan string, 1)
	clk := testingclock.NewFakeClock(t0)
	d := NewDebouncer(window, clk, func(key string) {
		retried <- key
	})
	defer d.Stop()

	// First downward: schedules timer.
	write, _, _ := d.Apply("X", component.StatusOperational, component.StatusDown, clk.Now())
	if write {
		t.Fatalf("first downward should not write")
	}

	// Advance past window; FakeClock fires AfterFunc on a goroutine.
	clk.Step(window + time.Millisecond)

	select {
	case got := <-retried:
		if got != "X" {
			t.Fatalf("retry got %q, want %q", got, "X")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timer did not fire retry within 2s of fake-clock step")
	}
}

func TestDebouncer_TimerCanceledOnUpward(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const window = 60 * time.Second

	retried := make(chan struct{}, 1)
	clk := testingclock.NewFakeClock(t0)
	d := NewDebouncer(window, clk, func(string) { retried <- struct{}{} })
	defer d.Stop()

	d.Apply("X", component.StatusOperational, component.StatusDown, clk.Now())

	// Recover before window.
	clk.Step(10 * time.Second)
	write, status, _ := d.Apply("X", component.StatusDown, component.StatusOperational, clk.Now())
	if !write || status != component.StatusOperational {
		t.Fatalf("recovery should write operational; got (%v, %q)", write, status)
	}

	// Step past original deadline — no retry should fire.
	clk.Step(window)
	select {
	case <-retried:
		t.Fatal("retry should have been cancelled by recovery")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestDebouncer_TransientDowngradeThenRecovery_NoStalePending(t *testing.T) {
	// Regression for the recovery-clears-pending bug:
	// 1. operational → degraded at t=0 (records pending).
	// 2. recover at t=10s: prev==operational, next==operational (store
	//    still says operational because the downgrade was suppressed).
	//    Apply must clear the stale pending.
	// 3. Advance past t=60s: NO callback fires.
	// 4. operational → degraded at t=100s must be treated as a NEW
	//    first observation, not as already-elapsed.
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const window = 60 * time.Second

	retried := make(chan struct{}, 4)
	clk := testingclock.NewFakeClock(t0)
	d := NewDebouncer(window, clk, func(string) { retried <- struct{}{} })
	defer d.Stop()

	// Step 1
	d.Apply("X", component.StatusOperational, component.StatusDegraded, clk.Now())

	// Step 2 (recovery — prev==next from the store's perspective)
	clk.Step(10 * time.Second)
	d.Apply("X", component.StatusOperational, component.StatusOperational, clk.Now())

	// Step 3
	clk.Step(window)
	select {
	case <-retried:
		t.Fatal("retry should have been cancelled by recovery")
	case <-time.After(100 * time.Millisecond):
	}

	// Step 4 — new degraded observation: must be first-observation.
	clk.Step(30 * time.Second) // now at t=100s
	write, _, retryAfter := d.Apply("X", component.StatusOperational, component.StatusDegraded, clk.Now())
	if write {
		t.Fatal("second downward should NOT write immediately — pending was stale")
	}
	if retryAfter != window {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, window)
	}
}

func TestDebouncer_NoDeadlockOnRetryReentry(t *testing.T) {
	// retry callback re-enters Apply(). If the Debouncer held its
	// mutex across the callback, this would deadlock.
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const window = 60 * time.Second

	clk := testingclock.NewFakeClock(t0)
	done := make(chan struct{})
	var d *Debouncer
	d = NewDebouncer(window, clk, func(key string) {
		// Simulate the watcher's handle() re-call: read prev from
		// "store" (test side: always operational), compute next,
		// call Apply. The elapsed pending must now satisfy the
		// window and return write=true.
		write, status, _ := d.Apply(key, component.StatusOperational, component.StatusDown, clk.Now())
		if !write || status != component.StatusDown {
			t.Errorf("re-entered Apply: got (%v, %q), want (true, down)", write, status)
		}
		close(done)
	})
	defer d.Stop()

	d.Apply("X", component.StatusOperational, component.StatusDown, clk.Now())
	clk.Step(window + time.Millisecond)

	select {
	case <-done:
		// success
	case <-time.After(2 * time.Second):
		t.Fatal("retry callback deadlocked or never invoked")
	}
}

func TestDebouncer_StopWaitsForInflightCallbacks(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const window = 60 * time.Second

	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	clk := testingclock.NewFakeClock(t0)
	d := NewDebouncer(window, clk, func(string) {
		close(callbackStarted)
		<-releaseCallback
	})

	d.Apply("X", component.StatusOperational, component.StatusDown, clk.Now())
	clk.Step(window + time.Millisecond)
	<-callbackStarted // phase 2 is running

	stopReturned := make(chan struct{})
	go func() {
		d.Stop()
		close(stopReturned)
	}()

	// Stop must NOT return while the callback is blocked.
	select {
	case <-stopReturned:
		t.Fatal("Stop returned while a callback was still in phase 2")
	case <-time.After(200 * time.Millisecond):
		// expected — Stop is blocked on inflight.Wait()
	}

	close(releaseCallback)
	<-stopReturned // now Stop returns
}

func TestDebouncer_ApplyAfterStopIsNoOp(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := testingclock.NewFakeClock(t0)
	retried := make(chan struct{}, 1)
	d := NewDebouncer(60*time.Second, clk, func(string) { retried <- struct{}{} })

	d.Stop()

	write, status, retryAfter := d.Apply("X", component.StatusOperational, component.StatusDown, clk.Now())
	if write {
		t.Fatal("Apply after Stop must not write")
	}
	if status != component.StatusOperational {
		t.Fatalf("status = %q, want prev (operational)", status)
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v, want 0", retryAfter)
	}

	// No timer should have been scheduled.
	clk.Step(2 * time.Minute)
	select {
	case <-retried:
		t.Fatal("retry fired after Stop")
	case <-time.After(100 * time.Millisecond):
		// expected
	}
}

func TestDebouncer_ConcurrentApplyAndStop(t *testing.T) {
	// Race-detector-oriented: hammer Apply from one goroutine while
	// another calls Stop. Must not panic; must terminate.
	t.Parallel()
	clk := testingclock.NewFakeClock(time.Now())
	d := NewDebouncer(60*time.Second, clk, func(string) {})

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			d.Apply("X", component.StatusOperational, component.StatusDown, clk.Now())
		}
		close(done)
	}()
	d.Stop()
	<-done
}

func TestDebouncer_ForgetClearsDeletedObject(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	const window = 60 * time.Second

	clk := testingclock.NewFakeClock(t0)
	d := NewDebouncer(window, clk, nil)
	defer d.Stop()

	// Record pending downgrade at t=0.
	d.Apply("X", component.StatusOperational, component.StatusDegraded, clk.Now())

	// Simulate retry callback discovering the object is gone.
	clk.Step(window + time.Millisecond)
	d.Forget("X")

	// A future same-ID recreate at t = window + 1m must be treated as
	// a first observation, NOT as already-elapsed.
	clk.Step(time.Minute)
	write, _, retryAfter := d.Apply("X", component.StatusOperational, component.StatusDegraded, clk.Now())
	if write {
		t.Fatal("after Forget, recreate must NOT bypass the window")
	}
	if retryAfter != window {
		t.Fatalf("retryAfter = %v, want %v", retryAfter, window)
	}
}

func TestDebouncer_ForgetOnUnknownKeyIsNoOp(t *testing.T) {
	t.Parallel()
	d := NewDebouncer(60*time.Second, testingclock.NewFakeClock(time.Now()), nil)
	defer d.Stop()
	d.Forget("nonexistent") // must not panic
}

func TestDebouncer_ForgetAfterStopIsNoOp(t *testing.T) {
	t.Parallel()
	d := NewDebouncer(60*time.Second, testingclock.NewFakeClock(time.Now()), nil)
	d.Stop()
	d.Forget("X") // must not panic on nil pending map
}
