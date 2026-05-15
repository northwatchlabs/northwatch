package watcher

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
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
	upsertCh chan struct{}
}

func newRecordStore() *recordStore {
	return &recordStore{upsertCh: make(chan struct{}, 64)}
}

func (s *recordStore) Close() error                  { return nil }
func (s *recordStore) Migrate(context.Context) error { return nil }
func (s *recordStore) ListComponents(context.Context) ([]component.Component, error) {
	return nil, nil
}
func (s *recordStore) GetComponent(context.Context, string) (component.Component, error) {
	return component.Component{}, store.ErrNotFound
}
func (s *recordStore) UpsertComponent(_ context.Context, c component.Component) error {
	s.mu.Lock()
	s.calls = append(s.calls, c)
	s.mu.Unlock()
	select {
	case s.upsertCh <- struct{}{}:
	default:
	}
	return nil
}
func (s *recordStore) SyncComponents(context.Context, []store.ComponentSpec, bool) (int, error) {
	return 0, nil
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
	w := NewDeploymentWatcher(cs, rs, specs, quietLogger())
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

func TestComputeDeploymentStatus_NilReplicasDefaultsToOne(t *testing.T) {
	d := &appsv1.Deployment{
		Spec:   appsv1.DeploymentSpec{Replicas: nil},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	if got := computeDeploymentStatus(d); got != component.StatusOperational {
		t.Errorf("got %q, want %q (nil replicas should default to 1)", got, component.StatusOperational)
	}
}
