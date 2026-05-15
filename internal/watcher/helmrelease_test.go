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

// newDynamicClient returns a fake dynamic client wired with the GVR
// → list-kind mapping that dynamicinformer needs to construct
// informers. Without the list-kind mapping the informer panics on
// startup with "no kind \"<gvr>List\" is registered for the
// internal version".
func newDynamicClient() *dynfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		HelmReleaseGVR: "HelmReleaseList",
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
}

func newHelmRelease(ns, name, readyStatus, readyReason string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(HelmReleaseGVR.GroupVersion().WithKind("HelmRelease"))
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

func startHelmReleaseWatcher(t *testing.T, cs dynamic.Interface, rs store.Store, specs []config.Spec) func() {
	t.Helper()
	w := NewHelmReleaseWatcher(cs, rs, specs, quietLogger())
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
	res := cs.Resource(HelmReleaseGVR).Namespace("flux-system")

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
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "Progressing"},
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
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "InstallFailed"},
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

func TestHelmReleaseWatcher_UnwatchedIgnored(t *testing.T) {
	cs := newDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "flux-system", Name: "watched",
	}}
	stop := startHelmReleaseWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(HelmReleaseGVR)

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
	res := cs.Resource(HelmReleaseGVR).Namespace("flux-system")
	if _, err := res.Create(ctx, newHelmRelease("flux-system", "my-app", "True", "ok"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)

	// Same Ready=True, different reason — status is still operational,
	// so this should not produce a second upsert.
	cur, _ := res.Get(ctx, "my-app", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "ReconciliationSucceeded"},
	}, "status", "conditions")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update same: %v", err)
	}
	rs.assertCountStable(t, 1)

	// Real change → upsert.
	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	_ = unstructured.SetNestedSlice(cur.Object, []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "InstallFailed"},
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
	res := cs.Resource(HelmReleaseGVR).Namespace("flux-system")
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
	res := cs.Resource(HelmReleaseGVR).Namespace("flux-system")
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
	res := cs.Resource(HelmReleaseGVR).Namespace("flux-system")
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
// the error and ctx.Done() arms of helmReleaseCRDPresentVia without
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

func TestHelmReleaseCRDPresentVia_Present(t *testing.T) {
	disc := &stubDiscovery{groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{
		{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{Version: "v1"}}},
		{Name: HelmReleaseGVR.Group, Versions: []metav1.GroupVersionForDiscovery{
			{Version: "v2beta1"}, {Version: HelmReleaseGVR.Version},
		}},
	}}}
	ok, err := helmReleaseCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Errorf("ok = false, want true (Flux v2 group present)")
	}
}

func TestHelmReleaseCRDPresentVia_AbsentGroup(t *testing.T) {
	disc := &stubDiscovery{groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{
		{Name: "apps", Versions: []metav1.GroupVersionForDiscovery{{Version: "v1"}}},
	}}}
	ok, err := helmReleaseCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false (Flux group missing)")
	}
}

func TestHelmReleaseCRDPresentVia_GroupPresentWrongVersion(t *testing.T) {
	disc := &stubDiscovery{groups: &metav1.APIGroupList{Groups: []metav1.APIGroup{
		{Name: HelmReleaseGVR.Group, Versions: []metav1.GroupVersionForDiscovery{
			{Version: "v2beta1"}, {Version: "v2beta2"},
		}},
	}}}
	ok, err := helmReleaseCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false (only legacy versions present, no v2)")
	}
}

func TestHelmReleaseCRDPresentVia_DiscoveryError(t *testing.T) {
	wantErr := errors.New("apiserver unreachable")
	disc := &stubDiscovery{err: wantErr}
	ok, err := helmReleaseCRDPresentVia(context.Background(), disc)
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
	if ok {
		t.Errorf("ok = true, want false on error")
	}
}

func TestHelmReleaseCRDPresentVia_CtxCancelled(t *testing.T) {
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
		ok, err = helmReleaseCRDPresentVia(ctx, disc)
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

func TestComputeHelmReleaseStatus(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(u *unstructured.Unstructured)
		want   component.Status
	}{
		{
			name:   "Ready=True → operational",
			mutate: func(u *unstructured.Unstructured) {},
			want:   component.StatusOperational,
		},
		{
			name: "Ready=False, reason=Progressing → degraded",
			mutate: func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(u.Object, []interface{}{
					map[string]interface{}{"type": "Ready", "status": "False", "reason": "Progressing"},
				}, "status", "conditions")
			},
			want: component.StatusDegraded,
		},
		{
			name: "Ready=False, reason=InstallFailed → down",
			mutate: func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(u.Object, []interface{}{
					map[string]interface{}{"type": "Ready", "status": "False", "reason": "InstallFailed"},
				}, "status", "conditions")
			},
			want: component.StatusDown,
		},
		{
			name: "Ready=Unknown → unknown",
			mutate: func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(u.Object, []interface{}{
					map[string]interface{}{"type": "Ready", "status": "Unknown", "reason": "InProgress"},
				}, "status", "conditions")
			},
			want: component.StatusUnknown,
		},
		{
			name: "no conditions → unknown",
			mutate: func(u *unstructured.Unstructured) {
				unstructured.RemoveNestedField(u.Object, "status", "conditions")
			},
			want: component.StatusUnknown,
		},
		{
			name: "only non-Ready conditions → unknown",
			mutate: func(u *unstructured.Unstructured) {
				_ = unstructured.SetNestedSlice(u.Object, []interface{}{
					map[string]interface{}{"type": "Released", "status": "True"},
				}, "status", "conditions")
			},
			want: component.StatusUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newHelmRelease("flux-system", "my-app", "True", "ok")
			tc.mutate(u)
			if got := computeHelmReleaseStatus(u); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
