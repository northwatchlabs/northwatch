package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

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

// helmReleaseV2GVR is the v2 GVR used across the watcher tests. The
// probe-resolution tests in this file exercise fallback to v2beta2 /
// v2beta1, but the dynamic-informer tests only need a single GVR.
var helmReleaseV2GVR = schema.GroupVersionResource{
	Group:    helmReleaseGroup,
	Version:  "v2",
	Resource: helmReleaseResource,
}

// newDynamicClient returns a fake dynamic client wired with the GVR
// → list-kind mapping that dynamicinformer needs to construct
// informers. Without the list-kind mapping the informer panics on
// startup with "no kind \"<gvr>List\" is registered for the
// internal version".
func newDynamicClient() *dynfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		helmReleaseV2GVR: "HelmReleaseList",
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
}

func newHelmRelease(ns, name, readyStatus, readyReason string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(helmReleaseV2GVR.GroupVersion().WithKind("HelmRelease"))
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetGeneration(1)
	if readyStatus != "" {
		conds := []interface{}{
			map[string]interface{}{
				"type":               "Ready",
				"status":             readyStatus,
				"reason":             readyReason,
				"observedGeneration": int64(1),
			},
		}
		_ = unstructured.SetNestedSlice(u.Object, conds, "status", "conditions")
	}
	return u
}

func startHelmReleaseWatcher(t *testing.T, cs dynamic.Interface, rs store.Store, specs []config.Spec) func() {
	t.Helper()
	w := NewHelmReleaseWatcher(cs, rs, specs, quietLogger(), 0, nil, helmReleaseV2GVR)
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

func TestHelmReleaseWatcher_ReadyTransitions(t *testing.T) {
	cs := newDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "flux-system", Name: "my-app", DisplayName: "My App",
	}}
	stop := startHelmReleaseWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(helmReleaseV2GVR).Namespace("flux-system")

	// operational: Ready=True
	if _, err := res.Create(ctx, newHelmRelease("flux-system", "my-app", "True", "ReconciliationSucceeded"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusOperational {
		t.Errorf("[0] status = %q, want %q", got, component.StatusOperational)
	}

	// degraded: Ready=False, reason=Progressing
	cur, err := res.Get(ctx, "my-app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "Progressing", "observedGeneration": int64(1)},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update degraded: %v", err)
	}
	rs.waitForCalls(t, 2)
	if got := rs.snapshot()[1].Status; got != component.StatusDegraded {
		t.Errorf("[1] status = %q, want %q", got, component.StatusDegraded)
	}

	// down: Ready=False, reason=InstallFailed
	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "InstallFailed", "observedGeneration": int64(1)},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update down: %v", err)
	}
	rs.waitForCalls(t, 3)
	if got := rs.snapshot()[2].Status; got != component.StatusDown {
		t.Errorf("[2] status = %q, want %q", got, component.StatusDown)
	}

	// operational again: Ready=True
	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "observedGeneration": int64(1)},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update operational: %v", err)
	}
	rs.waitForCalls(t, 4)
	if got := rs.snapshot()[3].Status; got != component.StatusOperational {
		t.Errorf("[3] status = %q, want %q", got, component.StatusOperational)
	}
}

func TestHelmReleaseWatcher_UnwatchedIgnored(t *testing.T) {
	cs := newDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "flux-system", Name: "watched",
	}}
	stop := startHelmReleaseWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(helmReleaseV2GVR)

	if _, err := res.Namespace("flux-system").Create(ctx, newHelmRelease("flux-system", "other", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if _, err := res.Namespace("apps").Create(ctx, newHelmRelease("apps", "watched", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other-ns: %v", err)
	}
	if _, err := res.Namespace("flux-system").Create(ctx, newHelmRelease("flux-system", "watched", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create watched: %v", err)
	}

	rs.waitForCalls(t, 1)
	rs.assertCountStable(t, 1)
	got := rs.snapshot()[0]
	if got.Name != "watched" || got.Namespace != "flux-system" {
		t.Errorf("upserted wrong component: %+v", got)
	}
}

func TestHelmReleaseWatcher_StatusUnchangedNoOp(t *testing.T) {
	cs := newDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "flux-system", Name: "my-app",
	}}
	stop := startHelmReleaseWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(helmReleaseV2GVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newHelmRelease("flux-system", "my-app", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)

	// Same Ready=True, different reason — status is still operational,
	// so this should not produce a second upsert.
	cur, _ := res.Get(ctx, "my-app", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded", "observedGeneration": int64(1)},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update same: %v", err)
	}
	rs.assertCountStable(t, 1)

	// Real change → upsert.
	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "InstallFailed", "observedGeneration": int64(1)},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update changed: %v", err)
	}
	rs.waitForCalls(t, 2)
}

func TestHelmReleaseWatcher_DeleteLeavesStatus(t *testing.T) {
	cs := newDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "flux-system", Name: "my-app",
	}}
	stop := startHelmReleaseWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(helmReleaseV2GVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newHelmRelease("flux-system", "my-app", "False", "InstallFailed"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusDown {
		t.Fatalf("initial status = %q, want %q", got, component.StatusDown)
	}

	if err := res.Delete(ctx, "my-app", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rs.assertCountStable(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusDown {
		t.Errorf("recorded status after delete = %q, want %q", got, component.StatusDown)
	}
}

func TestHelmReleaseWatcher_CrossKindIsolation(t *testing.T) {
	cs := newDynamicClient()
	rs := newRecordStore()
	// Spec with matching (ns, name) but Kind=Deployment must NOT
	// match a HelmRelease event.
	specs := []config.Spec{{
		Kind: "Deployment", Namespace: "flux-system", Name: "my-app",
	}}
	stop := startHelmReleaseWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(helmReleaseV2GVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newHelmRelease("flux-system", "my-app", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.assertCountStable(t, 0)
}

func TestHelmReleaseWatcher_DisplayNamePropagation(t *testing.T) {
	cs := newDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "flux-system", Name: "my-app", DisplayName: "Cert Manager",
	}}
	stop := startHelmReleaseWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(helmReleaseV2GVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newHelmRelease("flux-system", "my-app", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	got := rs.snapshot()[0]
	if got.DisplayName != "Cert Manager" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Cert Manager")
	}
	if got.Kind != "HelmRelease" {
		t.Errorf("Kind = %q, want %q", got.Kind, "HelmRelease")
	}
}

// stubDiscovery is a minimal ServerGroupsInterface impl used by the
// CRD-probe tests. Returning an error or blocking lets us exercise
// the error and ctx.Done() arms of resolveHelmReleaseGVRVia without
// reaching for client-go's discovery fakes (which assume a real
// REST round-trip).
type stubDiscovery struct {
	groups *metav1.APIGroupList
	err    error
	block  <-chan struct{} // when non-nil, ServerGroups blocks on it
}

func (s *stubDiscovery) ServerGroups() (*metav1.APIGroupList, error) {
	if s.block != nil {
		<-s.block
	}
	return s.groups, s.err
}

// helmReleaseGroups builds an APIGroupList where the Flux HelmRelease
// group serves exactly the given versions. The order is preserved so
// tests can assert the resolver's preference order rather than the
// server's discovery order.
func helmReleaseGroups(versions ...string) *metav1.APIGroupList {
	gvs := make([]metav1.GroupVersionForDiscovery, 0, len(versions))
	for _, v := range versions {
		gvs = append(gvs, metav1.GroupVersionForDiscovery{Version: v})
	}
	return &metav1.APIGroupList{Groups: []metav1.APIGroup{
		{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{Version: "v1"}}},
		{Name: helmReleaseGroup, Versions: gvs},
	}}
}

func TestResolveHelmReleaseGVRVia_PrefersV2(t *testing.T) {
	disc := &stubDiscovery{groups: helmReleaseGroups("v2beta1", "v2beta2", "v2")}
	gvr, ok, err := resolveHelmReleaseGVRVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true (all versions present)")
	}
	if gvr.Version != "v2" {
		t.Errorf("gvr.Version = %q, want %q", gvr.Version, "v2")
	}
}

func TestResolveHelmReleaseGVRVia_FallsBackToV2Beta2(t *testing.T) {
	disc := &stubDiscovery{groups: helmReleaseGroups("v2beta1", "v2beta2")}
	gvr, ok, err := resolveHelmReleaseGVRVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true (v2beta2 served)")
	}
	if gvr.Version != "v2beta2" {
		t.Errorf("gvr.Version = %q, want %q", gvr.Version, "v2beta2")
	}
}

func TestResolveHelmReleaseGVRVia_FallsBackToV2Beta1(t *testing.T) {
	disc := &stubDiscovery{groups: helmReleaseGroups("v2beta1")}
	gvr, ok, err := resolveHelmReleaseGVRVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true (v2beta1 served)")
	}
	if gvr.Version != "v2beta1" {
		t.Errorf("gvr.Version = %q, want %q", gvr.Version, "v2beta1")
	}
}

func TestResolveHelmReleaseGVRVia_AbsentGroup(t *testing.T) {
	disc := &stubDiscovery{groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{
		{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{Version: "v1"}}},
	}}}
	_, ok, err := resolveHelmReleaseGVRVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false (Flux group missing)")
	}
}

func TestResolveHelmReleaseGVRVia_NoKnownVersionServed(t *testing.T) {
	// Group present, but only some unrelated future version. The
	// resolver must return not-found rather than picking the unknown
	// version blindly.
	disc := &stubDiscovery{groups: helmReleaseGroups("v3alpha1")}
	_, ok, err := resolveHelmReleaseGVRVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false (no candidate version served)")
	}
}

func TestResolveHelmReleaseGVRVia_DiscoveryError(t *testing.T) {
	wantErr := errors.New("apiserver unreachable")
	disc := &stubDiscovery{err: wantErr}
	_, ok, err := resolveHelmReleaseGVRVia(context.Background(), disc)
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
	if ok {
		t.Errorf("ok = true, want false on error")
	}
}

func TestResolveHelmReleaseGVRVia_CtxCancelled(t *testing.T) {
	// Block ServerGroups forever; cancel the context immediately.
	// The probe must return without waiting for the in-flight call.
	block := make(chan struct{})
	defer close(block) // unblock the goroutine on test exit
	disc := &stubDiscovery{block: block}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	var (
		ok  bool
		err error
	)
	go func() {
		_, ok, err = resolveHelmReleaseGVRVia(ctx, disc)
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
