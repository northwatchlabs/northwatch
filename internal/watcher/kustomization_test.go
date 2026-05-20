package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// stubKsResourceDiscovery is the minimal kustomizationResourceDiscovery
// impl used by the probe tests. The block channel lets the cancel
// test exercise the ctx.Done() race without depending on a real
// network round-trip.
type stubKsResourceDiscovery struct {
	list  *metav1.APIResourceList
	err   error
	block <-chan struct{}
}

func (s *stubKsResourceDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	if s.block != nil {
		<-s.block
	}
	return s.list, s.err
}

func TestKustomizationCRDPresentVia_ResourcePresent(t *testing.T) {
	disc := &stubKsResourceDiscovery{list: &metav1.APIResourceList{
		GroupVersion: KustomizationGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{
			{Name: KustomizationGVR.Resource},
		},
	}}
	ok, err := kustomizationCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Errorf("ok = false, want true (kustomizations resource present)")
	}
}

func TestKustomizationCRDPresentVia_ResourceAbsent(t *testing.T) {
	// Group/version registered but the kustomizations resource isn't
	// in the resource list. Treat as absent.
	disc := &stubKsResourceDiscovery{list: &metav1.APIResourceList{
		GroupVersion: KustomizationGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{
			{Name: "somethingelse"},
		},
	}}
	ok, err := kustomizationCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false (kustomizations resource missing)")
	}
}

func TestKustomizationCRDPresentVia_GroupVersionNotFound(t *testing.T) {
	// Non-Flux cluster: ServerResourcesForGroupVersion returns NotFound.
	// Treat as absent, not error.
	disc := &stubKsResourceDiscovery{err: apierrors.NewNotFound(
		schema.GroupResource{Group: KustomizationGVR.Group, Resource: ""},
		KustomizationGVR.GroupVersion().String(),
	)}
	ok, err := kustomizationCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil (NotFound is clean absent)", err)
	}
	if ok {
		t.Errorf("ok = true, want false on NotFound")
	}
}

func TestKustomizationCRDPresentVia_DiscoveryError(t *testing.T) {
	wantErr := errors.New("apiserver unreachable")
	disc := &stubKsResourceDiscovery{err: wantErr}
	ok, err := kustomizationCRDPresentVia(context.Background(), disc)
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
	if ok {
		t.Errorf("ok = true, want false on error")
	}
}

func TestKustomizationCRDPresentVia_CtxCancelled(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	disc := &stubKsResourceDiscovery{block: block}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	var (
		ok  bool
		err error
	)
	go func() {
		ok, err = kustomizationCRDPresentVia(ctx, disc)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not return after ctx cancel within 2s")
	}
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want wrap of context.Canceled", err)
	}
	if ok {
		t.Errorf("ok = true, want false on cancel")
	}
}

// newDynamicClientKs returns a fake dynamic client wired with the
// Kustomization GVR → list-kind mapping that dynamicinformer needs.
func newDynamicClientKs() *dynfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		KustomizationGVR: "KustomizationList",
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
}

func newKustomization(ns, name, readyStatus, readyReason string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(KustomizationGVR.GroupVersion().WithKind("Kustomization"))
	u.SetNamespace(ns)
	u.SetName(name)
	if readyStatus != "" {
		conds := []interface{}{
			map[string]interface{}{
				"type":   "Ready",
				"status": readyStatus,
				"reason": readyReason,
			},
		}
		_ = unstructured.SetNestedSlice(u.Object, conds, "status", "conditions")
	}
	return u
}

func startKustomizationWatcher(t *testing.T, cs dynamic.Interface, rs store.Store, specs []config.Spec) func() {
	t.Helper()
	w := NewKustomizationWatcher(cs, rs, specs, quietLogger(), 0, nil, KustomizationGVR)
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

func TestKustomizationWatcher_ReadyTransitions(t *testing.T) {
	cs := newDynamicClientKs()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Kustomization", Namespace: "flux-system", Name: "infra", DisplayName: "Infra",
	}}
	stop := startKustomizationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(KustomizationGVR).Namespace("flux-system")

	// operational: Ready=True
	if _, err := res.Create(ctx, newKustomization("flux-system", "infra", "True", "ReconciliationSucceeded"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusOperational {
		t.Errorf("[0] status = %q, want %q", got, component.StatusOperational)
	}

	// degraded: Ready=False, reason=Progressing
	cur, err := res.Get(ctx, "infra", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "Progressing"},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update degraded: %v", err)
	}
	rs.waitForCalls(t, 2)
	if got := rs.snapshot()[1].Status; got != component.StatusDegraded {
		t.Errorf("[1] status = %q, want %q", got, component.StatusDegraded)
	}

	// down: Ready=False, reason=BuildFailed (real kustomize-controller reason)
	cur, _ = res.Get(ctx, "infra", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "BuildFailed"},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update down: %v", err)
	}
	rs.waitForCalls(t, 3)
	if got := rs.snapshot()[2].Status; got != component.StatusDown {
		t.Errorf("[2] status = %q, want %q", got, component.StatusDown)
	}

	// operational again
	cur, _ = res.Get(ctx, "infra", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded"},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update operational: %v", err)
	}
	rs.waitForCalls(t, 4)
	if got := rs.snapshot()[3].Status; got != component.StatusOperational {
		t.Errorf("[3] status = %q, want %q", got, component.StatusOperational)
	}
}

func TestKustomizationWatcher_UnwatchedIgnored(t *testing.T) {
	cs := newDynamicClientKs()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Kustomization", Namespace: "flux-system", Name: "watched",
	}}
	stop := startKustomizationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(KustomizationGVR)

	if _, err := res.Namespace("flux-system").Create(ctx, newKustomization("flux-system", "other", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if _, err := res.Namespace("apps").Create(ctx, newKustomization("apps", "watched", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other-ns: %v", err)
	}
	if _, err := res.Namespace("flux-system").Create(ctx, newKustomization("flux-system", "watched", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create watched: %v", err)
	}

	rs.waitForCalls(t, 1)
	rs.assertCountStable(t, 1)
	got := rs.snapshot()[0]
	if got.Name != "watched" || got.Namespace != "flux-system" {
		t.Errorf("upserted wrong component: %+v", got)
	}
}

func TestKustomizationWatcher_StatusUnchangedNoOp(t *testing.T) {
	cs := newDynamicClientKs()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Kustomization", Namespace: "flux-system", Name: "infra",
	}}
	stop := startKustomizationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(KustomizationGVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newKustomization("flux-system", "infra", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)

	// Same Ready=True, different reason — status is still operational,
	// so this should not produce a second upsert.
	cur, _ := res.Get(ctx, "infra", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded"},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update same: %v", err)
	}
	rs.assertCountStable(t, 1)

	// Real change → upsert.
	cur, _ = res.Get(ctx, "infra", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "BuildFailed"},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update changed: %v", err)
	}
	rs.waitForCalls(t, 2)
}

func TestKustomizationWatcher_DeleteLeavesStatus(t *testing.T) {
	cs := newDynamicClientKs()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Kustomization", Namespace: "flux-system", Name: "infra",
	}}
	stop := startKustomizationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(KustomizationGVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newKustomization("flux-system", "infra", "False", "BuildFailed"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusDown {
		t.Fatalf("initial status = %q, want %q", got, component.StatusDown)
	}

	if err := res.Delete(ctx, "infra", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rs.assertCountStable(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusDown {
		t.Errorf("recorded status after delete = %q, want %q", got, component.StatusDown)
	}
}

func TestKustomizationWatcher_CrossKindIsolation(t *testing.T) {
	cs := newDynamicClientKs()
	rs := newRecordStore()
	// Spec with matching (ns, name) but Kind=HelmRelease must NOT
	// match a Kustomization event.
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "flux-system", Name: "infra",
	}}
	stop := startKustomizationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(KustomizationGVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newKustomization("flux-system", "infra", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.assertCountStable(t, 0)
}

func TestKustomizationWatcher_DisplayNamePropagation(t *testing.T) {
	cs := newDynamicClientKs()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Kustomization", Namespace: "flux-system", Name: "infra", DisplayName: "Infra Bundle",
	}}
	stop := startKustomizationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(KustomizationGVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newKustomization("flux-system", "infra", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	got := rs.snapshot()[0]
	if got.DisplayName != "Infra Bundle" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Infra Bundle")
	}
	if got.Kind != "Kustomization" {
		t.Errorf("Kind = %q, want %q", got.Kind, "Kustomization")
	}
}
