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

// newAppDynamicClient returns a fake dynamic client wired with the
// ApplicationGVR → ApplicationList mapping that dynamicinformer needs.
func newAppDynamicClient() *dynfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{
		ApplicationGVR: "ApplicationList",
	}
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
}

func newApplication(ns, name, health string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(ApplicationGVR.GroupVersion().WithKind("Application"))
	u.SetNamespace(ns)
	u.SetName(name)
	if health != "" {
		_ = unstructured.SetNestedField(u.Object, health, "status", "health", "status")
	}
	return u
}

func setAppHealth(t *testing.T, u *unstructured.Unstructured, health string) {
	t.Helper()
	if err := unstructured.SetNestedField(u.Object, health, "status", "health", "status"); err != nil {
		t.Fatalf("set health: %v", err)
	}
}

func startApplicationWatcher(t *testing.T, cs dynamic.Interface, rs store.Store, specs []config.Spec) func() {
	t.Helper()
	w := NewApplicationWatcher(cs, rs, specs, quietLogger())
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

func TestApplicationWatcher_HealthTransitions(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "my-app", DisplayName: "My App",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")

	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusOperational {
		t.Errorf("[0] status = %q, want %q", got, component.StatusOperational)
	}

	cur, err := res.Get(ctx, "my-app", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	setAppHealth(t, cur, "Progressing")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update degraded: %v", err)
	}
	rs.waitForCalls(t, 2)
	if got := rs.snapshot()[1].Status; got != component.StatusDegraded {
		t.Errorf("[1] status = %q, want %q", got, component.StatusDegraded)
	}

	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	setAppHealth(t, cur, "Degraded")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update down: %v", err)
	}
	rs.waitForCalls(t, 3)
	if got := rs.snapshot()[2].Status; got != component.StatusDown {
		t.Errorf("[2] status = %q, want %q", got, component.StatusDown)
	}

	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	setAppHealth(t, cur, "Healthy")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update operational: %v", err)
	}
	rs.waitForCalls(t, 4)
	if got := rs.snapshot()[3].Status; got != component.StatusOperational {
		t.Errorf("[3] status = %q, want %q", got, component.StatusOperational)
	}
}

func TestApplicationWatcher_MissingMapsToDown(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "my-app",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")
	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Missing"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusDown {
		t.Errorf("status = %q, want %q", got, component.StatusDown)
	}
}

func TestApplicationWatcher_SuspendedPreservesStatus(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "my-app",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")

	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)

	// Healthy → Suspended must not produce a second upsert.
	cur, _ := res.Get(ctx, "my-app", metav1.GetOptions{})
	setAppHealth(t, cur, "Suspended")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update suspended: %v", err)
	}
	rs.assertCountStable(t, 1)

	// Suspended → Degraded resumes normal mapping and writes "down".
	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	setAppHealth(t, cur, "Degraded")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update degraded: %v", err)
	}
	rs.waitForCalls(t, 2)
	if got := rs.snapshot()[1].Status; got != component.StatusDown {
		t.Errorf("status after resume = %q, want %q", got, component.StatusDown)
	}
}

func TestApplicationWatcher_UnknownMapsToUnknown(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "my-app",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")

	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	if got := rs.snapshot()[0].Status; got != component.StatusOperational {
		t.Fatalf("[0] status = %q, want %q", got, component.StatusOperational)
	}

	// Healthy → Unknown surfaces the loss-of-signal: status must flip
	// to unknown, not stay at operational.
	cur, _ := res.Get(ctx, "my-app", metav1.GetOptions{})
	setAppHealth(t, cur, "Unknown")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update unknown: %v", err)
	}
	rs.waitForCalls(t, 2)
	if got := rs.snapshot()[1].Status; got != component.StatusUnknown {
		t.Errorf("[1] status = %q, want %q", got, component.StatusUnknown)
	}
}

func TestApplicationWatcher_UnwatchedIgnored(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "watched",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR)

	if _, err := res.Namespace("argocd").Create(ctx, newApplication("argocd", "other", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other: %v", err)
	}
	if _, err := res.Namespace("apps").Create(ctx, newApplication("apps", "watched", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create other-ns: %v", err)
	}
	if _, err := res.Namespace("argocd").Create(ctx, newApplication("argocd", "watched", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create watched: %v", err)
	}

	rs.waitForCalls(t, 1)
	rs.assertCountStable(t, 1)
	got := rs.snapshot()[0]
	if got.Name != "watched" || got.Namespace != "argocd" {
		t.Errorf("upserted wrong component: %+v", got)
	}
}

func TestApplicationWatcher_StatusUnchangedNoOp(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "my-app",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")
	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)

	// Bump an unrelated annotation; health stays Healthy, no upsert.
	cur, _ := res.Get(ctx, "my-app", metav1.GetOptions{})
	cur.SetAnnotations(map[string]string{"x": "1"})
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update same: %v", err)
	}
	rs.assertCountStable(t, 1)

	// Real change → upsert.
	cur, _ = res.Get(ctx, "my-app", metav1.GetOptions{})
	setAppHealth(t, cur, "Degraded")
	if _, err := res.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update changed: %v", err)
	}
	rs.waitForCalls(t, 2)
}

func TestApplicationWatcher_DeleteLeavesStatus(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "my-app",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")
	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Degraded"), metav1.CreateOptions{}); err != nil {
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

func TestApplicationWatcher_CrossKindIsolation(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	// Spec with matching (ns, name) but Kind=HelmRelease must NOT
	// match an Application event.
	specs := []config.Spec{{
		Kind: "HelmRelease", Namespace: "argocd", Name: "my-app",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")
	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.assertCountStable(t, 0)
}

func TestApplicationWatcher_DisplayNamePropagation(t *testing.T) {
	cs := newAppDynamicClient()
	rs := newRecordStore()
	specs := []config.Spec{{
		Kind: "Application", Namespace: "argocd", Name: "my-app", DisplayName: "Search API",
	}}
	stop := startApplicationWatcher(t, cs, rs, specs)
	defer stop()

	ctx := context.Background()
	res := cs.Resource(ApplicationGVR).Namespace("argocd")
	if _, err := res.Create(ctx, newApplication("argocd", "my-app", "Healthy"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	rs.waitForCalls(t, 1)
	got := rs.snapshot()[0]
	if got.DisplayName != "Search API" {
		t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Search API")
	}
	if got.Kind != "Application" {
		t.Errorf("Kind = %q, want %q", got.Kind, "Application")
	}
}

// stubResourceDiscovery is the minimal applicationResourceDiscovery
// impl used by the probe tests. The block channel lets the cancel
// test exercise the ctx.Done() race without depending on a real
// network round-trip.
type stubResourceDiscovery struct {
	list  *metav1.APIResourceList
	err   error
	block <-chan struct{}
}

func (s *stubResourceDiscovery) ServerResourcesForGroupVersion(string) (*metav1.APIResourceList, error) {
	if s.block != nil {
		<-s.block
	}
	return s.list, s.err
}

func TestApplicationCRDPresentVia_ResourcePresent(t *testing.T) {
	disc := &stubResourceDiscovery{list: &metav1.APIResourceList{
		GroupVersion: ApplicationGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{
			{Name: "rollouts"},
			{Name: ApplicationGVR.Resource},
		},
	}}
	ok, err := applicationCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !ok {
		t.Errorf("ok = false, want true (applications resource present)")
	}
}

func TestApplicationCRDPresentVia_ResourceAbsent(t *testing.T) {
	// Rollouts-only cluster: argoproj.io/v1alpha1 is served, but
	// only the Rollouts CRDs are registered — no `applications`.
	disc := &stubResourceDiscovery{list: &metav1.APIResourceList{
		GroupVersion: ApplicationGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{
			{Name: "rollouts"},
			{Name: "analysisruns"},
		},
	}}
	ok, err := applicationCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if ok {
		t.Errorf("ok = true, want false (applications resource missing)")
	}
}

func TestApplicationCRDPresentVia_GroupVersionNotFound(t *testing.T) {
	// Non-ArgoCD, non-Rollouts cluster: ServerResourcesForGroupVersion
	// returns a NotFound. Treat as absent, not error.
	disc := &stubResourceDiscovery{err: apierrors.NewNotFound(
		schema.GroupResource{Group: ApplicationGVR.Group, Resource: ""},
		ApplicationGVR.GroupVersion().String(),
	)}
	ok, err := applicationCRDPresentVia(context.Background(), disc)
	if err != nil {
		t.Fatalf("err = %v, want nil (NotFound is clean absent)", err)
	}
	if ok {
		t.Errorf("ok = true, want false on NotFound")
	}
}

func TestApplicationCRDPresentVia_DiscoveryError(t *testing.T) {
	wantErr := errors.New("apiserver unreachable")
	disc := &stubResourceDiscovery{err: wantErr}
	ok, err := applicationCRDPresentVia(context.Background(), disc)
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want wrap of %v", err, wantErr)
	}
	if ok {
		t.Errorf("ok = true, want false on error")
	}
}

func TestApplicationCRDPresentVia_CtxCancelled(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	disc := &stubResourceDiscovery{block: block}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	var (
		ok  bool
		err error
	)
	go func() {
		ok, err = applicationCRDPresentVia(ctx, disc)
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

func TestComputeApplicationStatus(t *testing.T) {
	cases := []struct {
		name   string
		health string
		want   component.Status
		wantOK bool
	}{
		{name: "Healthy → operational", health: "Healthy", want: component.StatusOperational, wantOK: true},
		{name: "Progressing → degraded", health: "Progressing", want: component.StatusDegraded, wantOK: true},
		{name: "Degraded → down", health: "Degraded", want: component.StatusDown, wantOK: true},
		{name: "Missing → down", health: "Missing", want: component.StatusDown, wantOK: true},
		{name: "Unknown → unknown", health: "Unknown", want: component.StatusUnknown, wantOK: true},
		{name: "Suspended → preserve previous", health: "Suspended", want: "", wantOK: false},
		{name: "unrecognized value → unknown", health: "Foobar", want: component.StatusUnknown, wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := newApplication("argocd", "my-app", tc.health)
			got, ok := computeApplicationStatus(u)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("got (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}

	t.Run("no health field → unknown, ok", func(t *testing.T) {
		u := newApplication("argocd", "my-app", "")
		got, ok := computeApplicationStatus(u)
		if got != component.StatusUnknown || !ok {
			t.Errorf("got (%q, %v), want (%q, true)", got, ok, component.StatusUnknown)
		}
	})
}
