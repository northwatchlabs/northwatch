package watcher

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	testingclock "k8s.io/utils/clock/testing"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
	"github.com/northwatchlabs/northwatch/internal/incident"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// settleWindow caps how long the tests wait when asserting the
// *absence* of an event. Long enough that a delayed informer dispatch
// would land; short enough that the suite stays fast.
const settleWindow = 150 * time.Millisecond

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordStore is a store.Store double that records every
// UpsertComponent call and signals via a channel so tests can wait
// for events without polling.
type recordStore struct {
	mu       sync.Mutex
	calls    []component.Component
	state    map[string]component.Component
	upsertCh chan struct{}
}

func newRecordStore() *recordStore {
	return &recordStore{
		state:    make(map[string]component.Component),
		upsertCh: make(chan struct{}, 64),
	}
}

func (s *recordStore) Close() error                  { return nil }
func (s *recordStore) Ping(context.Context) error    { return nil }
func (s *recordStore) Migrate(context.Context) error { return nil }
func (s *recordStore) ListComponents(context.Context) ([]component.Component, error) {
	return nil, nil
}
func (s *recordStore) GetComponent(_ context.Context, id string) (component.Component, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.state[id]; ok {
		return c, nil
	}
	return component.Component{}, store.ErrNotFound
}
func (s *recordStore) UpsertComponent(_ context.Context, c component.Component) error {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.state[c.ID()] = c
	s.mu.Unlock()
	select {
	case s.upsertCh <- struct{}{}:
	default:
	}
	return nil
}
func (s *recordStore) HasActiveComponent(context.Context, string) (bool, error) {
	return false, nil
}
func (s *recordStore) SyncComponents(context.Context, []store.ComponentSpec, bool) (int, error) {
	return 0, nil
}
func (s *recordStore) CreateIncident(context.Context, incident.Incident, incident.Update) error {
	return nil
}
func (s *recordStore) GetIncident(context.Context, string) (incident.Incident, error) {
	return incident.Incident{}, store.ErrNotFound
}
func (s *recordStore) GetActiveIncident(context.Context) (incident.Incident, error) {
	return incident.Incident{}, store.ErrNotFound
}
func (s *recordStore) ListIncidents(context.Context, bool) ([]incident.Incident, error) {
	return nil, nil
}
func (s *recordStore) ResolveIncident(context.Context, string, time.Time, string, string) (incident.Incident, error) {
	return incident.Incident{}, store.ErrNotFound
}

func (s *recordStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *recordStore) snapshot() []component.Component {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]component.Component, len(s.calls))
	copy(out, s.calls)
	return out
}

// waitForCalls blocks until count() >= n, or fails the test on a
// 2-second deadline.
func (s *recordStore) waitForCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		if s.count() >= n {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for %d upserts; got %d", n, s.count())
		case <-s.upsertCh:
		}
	}
}

// assertCountStable asserts that the call count remains == expected
// for the full settleWindow. Use after waitForCalls(n) to prove
// "exactly n, not n+1" within a bounded wait.
func (s *recordStore) assertCountStable(t *testing.T, expected int) {
	t.Helper()
	deadline := time.After(settleWindow)
	for {
		select {
		case <-deadline:
			if got := s.count(); got != expected {
				t.Fatalf("count = %d after settle, want %d", got, expected)
			}
			return
		case <-s.upsertCh:
			if got := s.count(); got > expected {
				t.Fatalf("unexpected upsert: count = %d, want %d", got, expected)
			}
		}
	}
}

func newDeployment(ns, name string, desired, ready int32) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.DeploymentSpec{Replicas: &desired},
		Status: appsv1.DeploymentStatus{
			Replicas:      desired,
			ReadyReplicas: ready,
		},
	}
}

// updateDeploymentStatus fetches the named Deployment, replaces its
// status, and Updates. This pattern emits a clean Modified event
// through the fake clientset's watch channel — constructing fresh
// objects without ResourceVersion can race with the informer's
// DeltaFIFO under -race.
func updateDeploymentStatus(t *testing.T, cs *fake.Clientset, ns, name string, desired, ready int32) {
	t.Helper()
	ctx := context.Background()
	cur, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s/%s: %v", ns, name, err)
	}
	cur.Spec.Replicas = &desired
	cur.Status.Replicas = desired
	cur.Status.ReadyReplicas = ready
	if _, err := cs.AppsV1().Deployments(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update %s/%s: %v", ns, name, err)
	}
}

// startWatcher launches a watcher goroutine, blocks until its
// informer cache is synced, and returns a cleanup func the test
// should defer. Errors from Start are surfaced through a channel
// (never via t.Errorf from the goroutine) so timeouts can join the
// goroutine before failing the test — otherwise a late t.Errorf
// would log after the test has finished.
func startWatcher(t *testing.T, cs *fake.Clientset, rs store.Store, specs []config.Spec) func() {
	t.Helper()
	w := NewDeploymentWatcher(cs, rs, specs, quietLogger(), 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- w.Start(ctx) }()

	select {
	case <-w.Synced():
	case <-time.After(2 * time.Second):
		cancel()
		select {
		case err := <-errCh:
			t.Fatalf("watcher did not sync within 2s (Start returned %v)", err)
		case <-time.After(2 * time.Second):
			t.Fatal("watcher did not sync within 2s and did not exit after cancel")
		}
	}

	return func() {
		cancel()
		select {
		case err := <-errCh:
			if err != nil {
				t.Errorf("Start returned error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("watcher did not stop within 2s of cancel")
		}
	}
}

func TestDeploymentWatcher_ReplicaTransitions(t *testing.T) {
	cs := fake.NewSimpleClientset()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Deployment", Namespace: "default", Name: "api", DisplayName: "API",
	}}
	stop := startWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	// operational: 3/3
	if _, err := cs.AppsV1().Deployments("default").
		Create(ctx, newDeployment("default", "api", 3, 3), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusOperational {
		t.Errorf("[0] status = %q, want %q", got, component.StatusOperational)
	}

	// degraded: 3/2
	updateDeploymentStatus(t, cs, "default", "api", 3, 2)
	rs.waitForCalls(t, 2)
	if got := rs.snapshot()[1].Status; got != component.StatusDegraded {
		t.Errorf("[1] status = %q, want %q", got, component.StatusDegraded)
	}

	// down: 3/0
	updateDeploymentStatus(t, cs, "default", "api", 3, 0)
	rs.waitForCalls(t, 3)
	if got := rs.snapshot()[2].Status; got != component.StatusDown {
		t.Errorf("[2] status = %q, want %q", got, component.StatusDown)
	}

	// operational again: 3/3
	updateDeploymentStatus(t, cs, "default", "api", 3, 3)
	rs.waitForCalls(t, 4)
	if got := rs.snapshot()[3].Status; got != component.StatusOperational {
		t.Errorf("[3] status = %q, want %q", got, component.StatusOperational)
	}
}

func TestDeploymentWatcher_UnwatchedIgnored(t *testing.T) {
	cs := fake.NewSimpleClientset()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Deployment", Namespace: "default", Name: "watched", DisplayName: "Watched",
	}}
	stop := startWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	// Different name, same namespace — must be ignored.
	if _, err := cs.AppsV1().Deployments("default").
		Create(ctx, newDeployment("default", "other", 1, 1), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	// Same name, different namespace — must be ignored.
	if _, err := cs.AppsV1().Deployments("staging").
		Create(ctx, newDeployment("staging", "watched", 1, 1), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create staging: %v", err)
	}
	// Watched: should produce exactly one upsert.
	if _, err := cs.AppsV1().Deployments("default").
		Create(ctx, newDeployment("default", "watched", 1, 1), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create watched: %v", err)
	}

	rs.waitForCalls(t, 1)
	rs.assertCountStable(t, 1)
	got := rs.snapshot()[0]
	if got.Name != "watched" || got.Namespace != "default" {
		t.Errorf("unexpected component upserted: %+v", got)
	}
}

func TestDeploymentWatcher_StatusUnchangedNoOp(t *testing.T) {
	cs := fake.NewSimpleClientset()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Deployment", Namespace: "default", Name: "api", DisplayName: "API",
	}}
	stop := startWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	if _, err := cs.AppsV1().Deployments("default").
		Create(ctx, newDeployment("default", "api", 3, 3), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)

	// Identical status update — must NOT trigger a second upsert.
	updateDeploymentStatus(t, cs, "default", "api", 3, 3)
	rs.assertCountStable(t, 1)

	// Now a real status change — should trigger an upsert.
	updateDeploymentStatus(t, cs, "default", "api", 3, 0)
	rs.waitForCalls(t, 2)
}

func TestDeploymentWatcher_DeleteLeavesStatus(t *testing.T) {
	cs := fake.NewSimpleClientset()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Deployment", Namespace: "default", Name: "api", DisplayName: "API",
	}}
	stop := startWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	if _, err := cs.AppsV1().Deployments("default").
		Create(ctx, newDeployment("default", "api", 3, 0), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusDown {
		t.Fatalf("initial status = %q, want %q", got, component.StatusDown)
	}

	if err := cs.AppsV1().Deployments("default").
		Delete(ctx, "api", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Deletion must not produce any further upsert.
	rs.assertCountStable(t, 1)
	// The recorded last status remains "down" — acceptance criterion.
	if got := rs.snapshot()[0].Status; got != component.StatusDown {
		t.Errorf("recorded status after delete = %q, want %q", got, component.StatusDown)
	}
}

func TestDeploymentWatcher_CrossKindIsolation(t *testing.T) {
	cs := fake.NewSimpleClientset()
	rs := newRecordStore()
	// Spec's (ns, name) matches the Deployment below, but Kind is
	// HelmRelease — the Deployment must NOT match the watched set.
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "default", Name: "api",
	}}
	stop := startWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	if _, err := cs.AppsV1().Deployments("default").
		Create(ctx, newDeployment("default", "api", 1, 1), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.assertCountStable(t, 0)
}

func TestDeploymentWatcher_DisplayNamePropagation(t *testing.T) {
	cs := fake.NewSimpleClientset()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind:        "Deployment",
		Namespace:   "default",
		Name:        "api",
		DisplayName: "API Gateway",
	}}
	stop := startWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	if _, err := cs.AppsV1().Deployments("default").
		Create(ctx, newDeployment("default", "api", 1, 1), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	got := rs.snapshot()[0]
	if got.DisplayName != "API Gateway" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "API Gateway")
	}
	if got.Kind != "Deployment" {
		t.Errorf("Kind = %q, want %q", got.Kind, "Deployment")
	}
}

// require fails the test fatally on err. Lightweight local helper —
// avoids dragging testify in for the two debounce tests below.
func require(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// newTestStore returns a *recordStore that tracks the most recent
// upsert per ID — enough for the debounce tests that need
// GetComponent to round-trip last-written state. Using the
// lightweight in-memory double instead of OpenSQLite(":memory:")
// avoids modernc.org/sqlite's per-connection-isolated database
// behavior (an 8-connection pool can land Get on a connection that
// never saw the schema migration).
func newTestStore(t *testing.T, _ context.Context) *recordStore {
	t.Helper()
	return newRecordStore()
}

// updateDeployment ensures the named deployment exists (creating it
// on the first call) and replaces its spec/status with the supplied
// template's. The template's namespace/name are coerced to the
// supplied ns/name so callers can pass a fresh fixture without
// repeating identity.
func updateDeployment(t *testing.T, cs *fake.Clientset, ns, name string, tmpl *appsv1.Deployment) {
	t.Helper()
	ctx := context.Background()
	cur, err := cs.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// First call — create it.
		obj := tmpl.DeepCopy()
		obj.Namespace = ns
		obj.Name = name
		if _, cerr := cs.AppsV1().Deployments(ns).Create(ctx, obj, metav1.CreateOptions{}); cerr != nil {
			t.Fatalf("create %s/%s: %v", ns, name, cerr)
		}
		return
	}
	cur.Spec = tmpl.Spec
	cur.Status = tmpl.Status
	if _, err := cs.AppsV1().Deployments(ns).Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update %s/%s: %v", ns, name, err)
	}
}

// deployWithReplicas builds a Deployment template with no
// Progressing condition — replica-count fallback only.
func deployWithReplicas(desired, ready int32) *appsv1.Deployment {
	d := desired
	return &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: &d},
		Status: appsv1.DeploymentStatus{ReadyReplicas: ready},
	}
}

// deployWithProgressing builds a Deployment template with a
// Progressing condition of the supplied status/reason.
func deployWithProgressing(s corev1.ConditionStatus, reason string, desired, ready int32) *appsv1.Deployment {
	d := deployWithReplicas(desired, ready)
	d.Status.Conditions = []appsv1.DeploymentCondition{
		{Type: appsv1.DeploymentProgressing, Status: s, Reason: reason},
	}
	return d
}

// TestDeploymentWatcher_RollingUpdateNoFlicker simulates a short
// rolling update entirely within the debounce window. The watcher
// must keep the component at operational for the duration; no
// degraded write hits the store.
func TestDeploymentWatcher_RollingUpdateNoFlicker(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const window = 60 * time.Second
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := testingclock.NewFakeClock(t0)
	st := newTestStore(t, ctx)
	require(t, st.UpsertComponent(ctx, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "nginx",
		DisplayName: "nginx", Status: component.StatusOperational,
	}))

	specs := []config.Spec{{Kind: "Deployment", Namespace: "default", Name: "nginx"}}
	client := fake.NewSimpleClientset()
	w := NewDeploymentWatcher(client, st, specs, quietLogger(), window, clk)

	errCh := make(chan error, 1)
	go func() { errCh <- w.Start(ctx) }()
	select {
	case <-w.Synced():
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not sync within 2s")
	}

	// Rolling update begins: ready=1/3 (degraded). Several events.
	for i := 0; i < 5; i++ {
		updateDeployment(t, client, "default", "nginx", deployWithReplicas(3, 1))
		clk.Step(6 * time.Second) // 30s total
	}

	// Within window: store should still say operational.
	got, err := st.GetComponent(ctx, "Deployment/default/nginx")
	require(t, err)
	if got.Status != component.StatusOperational {
		t.Fatalf("during rollout: got %q, want operational", got.Status)
	}

	// Rollout completes.
	updateDeployment(t, client, "default", "nginx", deployWithReplicas(3, 3))
	// Poll briefly — informer dispatch is async.
	deadline := time.After(2 * time.Second)
	for {
		got, err = st.GetComponent(ctx, "Deployment/default/nginx")
		require(t, err)
		if got.Status == component.StatusOperational {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("after rollout: got %q, want operational", got.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestDeploymentWatcher_StuckRolloutFlipsAfterWindow simulates a
// rollout stuck at ready < desired for longer than the window. The
// watcher MUST surface this as degraded once the window elapses.
func TestDeploymentWatcher_StuckRolloutFlipsAfterWindow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const window = 60 * time.Second
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := testingclock.NewFakeClock(t0)
	st := newTestStore(t, ctx)
	require(t, st.UpsertComponent(ctx, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "nginx",
		DisplayName: "nginx", Status: component.StatusOperational,
	}))

	specs := []config.Spec{{Kind: "Deployment", Namespace: "default", Name: "nginx"}}
	client := fake.NewSimpleClientset()
	w := NewDeploymentWatcher(client, st, specs, quietLogger(), window, clk)

	errCh := make(chan error, 1)
	go func() { errCh <- w.Start(ctx) }()
	select {
	case <-w.Synced():
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not sync within 2s")
	}

	// Stuck rollout: Progressing=True/ReplicaSetUpdated, ready < desired.
	stuck := deployWithProgressing(corev1.ConditionTrue, "ReplicaSetUpdated", 3, 1)
	updateDeployment(t, client, "default", "nginx", stuck)

	// Wait briefly for the informer to deliver the event and the
	// Debouncer to record the pending downward target.
	time.Sleep(50 * time.Millisecond)

	// Advance past window — timer fires, retry callback re-handles.
	clk.Step(window + time.Second)

	// Poll briefly for the upsert (the retry runs on a goroutine).
	deadline := time.After(2 * time.Second)
	for {
		got, err := st.GetComponent(ctx, "Deployment/default/nginx")
		require(t, err)
		if got.Status == component.StatusDegraded {
			return // success
		}
		select {
		case <-deadline:
			t.Fatalf("expected degraded after window; got %q", got.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestDeploymentWatcher_ProgressDeadlineExceededFlipsAfterWindow
// verifies that a Deployment surfacing
// Progressing=False/ProgressDeadlineExceeded — which MapDeployment
// maps directly to `down` — is still routed through the debouncer
// and only writes to the store after the window elapses.
func TestDeploymentWatcher_ProgressDeadlineExceededFlipsAfterWindow(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const window = 60 * time.Second
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk := testingclock.NewFakeClock(t0)
	st := newTestStore(t, ctx)
	require(t, st.UpsertComponent(ctx, component.Component{
		Kind: "Deployment", Namespace: "default", Name: "nginx",
		DisplayName: "nginx", Status: component.StatusOperational,
	}))

	specs := []config.Spec{{Kind: "Deployment", Namespace: "default", Name: "nginx"}}
	client := fake.NewSimpleClientset()
	w := NewDeploymentWatcher(client, st, specs, quietLogger(), window, clk)

	errCh := make(chan error, 1)
	go func() { errCh <- w.Start(ctx) }()
	select {
	case <-w.Synced():
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not sync within 2s")
	}

	// Progress deadline exceeded: Progressing=False/ProgressDeadlineExceeded.
	// MapDeployment returns `down`; debouncer records pending.
	failed := deployWithProgressing(corev1.ConditionFalse, "ProgressDeadlineExceeded", 3, 0)
	updateDeployment(t, client, "default", "nginx", failed)

	// Wait briefly for the informer to deliver the event and the
	// Debouncer to record the pending downward target.
	time.Sleep(50 * time.Millisecond)

	// Advance past window — timer fires, retry callback re-handles.
	clk.Step(window + time.Second)

	// Poll briefly for the upsert (the retry runs on a goroutine).
	deadline := time.After(2 * time.Second)
	for {
		got, err := st.GetComponent(ctx, "Deployment/default/nginx")
		require(t, err)
		if got.Status == component.StatusDown {
			return // success
		}
		select {
		case <-deadline:
			t.Fatalf("expected down after window; got %q", got.Status)
		case <-time.After(20 * time.Millisecond):
		}
	}
}
