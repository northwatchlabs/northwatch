package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
	"github.com/northwatchlabs/northwatch/internal/status"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// ApplicationGVR is the dynamic GroupVersionResource for ArgoCD's
// argoproj.io/v1alpha1 Application custom resource.
var ApplicationGVR = schema.GroupVersionResource{
	Group:    "argoproj.io",
	Version:  "v1alpha1",
	Resource: "applications",
}

// ApplicationCRDPresent returns true when the ArgoCD Application
// CRD (argoproj.io/v1alpha1.applications) is served by the cluster.
// Use to gate watcher construction so clusters without ArgoCD boot
// cleanly instead of failing the informer sync on a missing CRD.
//
// The probe checks the resource list for ApplicationGVR.GroupVersion,
// not just the group/version's presence. That distinction matters:
// argoproj.io/v1alpha1 is shared by Argo CD, Argo Rollouts, and Argo
// Events, so a Rollouts-only cluster has the group/version registered
// without any `applications` resource. A group-level probe would say
// "present" and the informer would then fail to sync.
//
// A copied rest.Config caps discovery at pingTimeout so a stalled
// endpoint can't hang serve startup or SIGTERM handling, and the
// in-flight request is raced against ctx.Done() via a goroutine in
// applicationCRDPresentVia.
func ApplicationCRDPresent(ctx context.Context, cfg *rest.Config) (bool, error) {
	probeCfg := rest.CopyConfig(cfg)
	probeCfg.Timeout = pingTimeout

	disc, err := discovery.NewDiscoveryClientForConfig(probeCfg)
	if err != nil {
		return false, fmt.Errorf("build discovery client: %w", err)
	}
	return applicationCRDPresentVia(ctx, disc)
}

// applicationResourceDiscovery is the small surface
// applicationCRDPresentVia needs — narrower than
// discovery.DiscoveryInterface so tests can drive it with a tiny stub.
type applicationResourceDiscovery interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// applicationCRDPresentVia is the testable seam. A NotFound from
// ServerResourcesForGroupVersion means the group/version is not
// served at all (the common non-ArgoCD, non-Rollouts cluster) and is
// treated as a clean "absent" result rather than a probe failure.
func applicationCRDPresentVia(ctx context.Context, disc applicationResourceDiscovery) (bool, error) {
	type result struct {
		list *metav1.APIResourceList
		err  error
	}
	resultCh := make(chan result, 1)
	gv := ApplicationGVR.GroupVersion().String()
	go func() {
		list, err := disc.ServerResourcesForGroupVersion(gv)
		resultCh <- result{list: list, err: err}
	}()

	var list *metav1.APIResourceList
	select {
	case <-ctx.Done():
		return false, fmt.Errorf("application CRD probe cancelled: %w", ctx.Err())
	case r := <-resultCh:
		if r.err != nil {
			if apierrors.IsNotFound(r.err) {
				return false, nil
			}
			return false, fmt.Errorf("list server resources for %s: %w", gv, r.err)
		}
		list = r.list
	}

	for _, res := range list.APIResources {
		if res.Name == ApplicationGVR.Resource {
			return true, nil
		}
	}
	return false, nil
}

// ApplicationWatcher watches ArgoCD Application events for the
// configured component set, maps them via status.MapApplication, and
// routes the (prev, next) transition through its own Debouncer before
// writing to the store. Status mapping rules and the debounce protocol
// live in internal/status/.
//
// Suspended is the one mapping outcome that short-circuits the
// debouncer entirely: status.MapApplication returns ok=false, handle()
// returns immediately without calling Debouncer.Apply, and any
// existing pending entry is preserved. When ArgoCD resumes syncing
// and the next event produces a real status, that pending entry (if
// any) carries forward unchanged.
//
// In-cluster RBAC: needs get, list, watch on
// argoproj.io/v1alpha1/applications. The Helm chart (#24) will ship
// the appropriate ClusterRole.
type ApplicationWatcher struct {
	client  dynamic.Interface
	store   store.Store
	watched map[types.NamespacedName]config.Spec
	logger  *slog.Logger
	window  time.Duration
	clk     clock.WithDelayedExecution

	// populated in Start once the informer factory exists
	lister    cache.GenericLister
	debouncer *status.Debouncer
	ctx       context.Context

	syncedCh   chan struct{}
	syncedOnce sync.Once
}

// NewApplicationWatcher mirrors NewHelmReleaseWatcher: it filters
// specs to Kind == "Application" and builds the lookup map. An empty
// specs slice is valid — the watcher runs but writes nothing.
// window=0 disables debouncing (Apply writes immediately). clk=nil
// defaults to clock.RealClock{}. A nil logger falls back to
// slog.Default().
func NewApplicationWatcher(
	client dynamic.Interface,
	st store.Store,
	specs []config.Spec,
	logger *slog.Logger,
	window time.Duration,
	clk clock.WithDelayedExecution,
) *ApplicationWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.RealClock{}
	}
	watched := make(map[types.NamespacedName]config.Spec)
	for _, s := range specs {
		if s.Kind != "Application" {
			continue
		}
		key := types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
		watched[key] = s
	}
	return &ApplicationWatcher{
		client:   client,
		store:    st,
		watched:  watched,
		logger:   logger.With("watcher", "application"),
		window:   window,
		clk:      clk,
		syncedCh: make(chan struct{}),
	}
}

// Synced returns a channel that is closed once the watcher's informer
// cache has finished its initial list/watch sync.
func (w *ApplicationWatcher) Synced() <-chan struct{} {
	return w.syncedCh
}

// Start runs the dynamic informer until ctx is cancelled. Blocks; the
// caller is expected to launch it in its own goroutine.
func (w *ApplicationWatcher) Start(ctx context.Context) error {
	w.ctx = ctx
	factory := dynamicinformer.NewDynamicSharedInformerFactory(w.client, 0)
	informer := factory.ForResource(ApplicationGVR).Informer()
	w.lister = factory.ForResource(ApplicationGVR).Lister()
	w.debouncer = status.NewDebouncer(w.window, w.clk, w.retry)
	defer w.debouncer.Stop()

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			u, ok := obj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			w.handle(ctx, u)
		},
		UpdateFunc: func(_, newObj interface{}) {
			u, ok := newObj.(*unstructured.Unstructured)
			if !ok {
				return
			}
			w.handle(ctx, u)
		},
		DeleteFunc: func(_ interface{}) {
			// Last status sticks. Stale pending state for a deleted
			// object is cleared by the retry callback's lister-miss
			// branch when the timer fires.
		},
	}); err != nil {
		return fmt.Errorf("watcher: add event handler: %w", err)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("watcher: application cache sync failed")
	}
	w.syncedOnce.Do(func() { close(w.syncedCh) })
	w.logger.Info("synced", "watched", len(w.watched))

	<-ctx.Done()
	return nil
}

func (w *ApplicationWatcher) handle(ctx context.Context, u *unstructured.Unstructured) {
	key := types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()}
	spec, ok := w.watched[key]
	if !ok {
		return
	}

	next, ok := status.MapApplication(u)
	if !ok {
		// Suspended: preserve previous status (no store write). We
		// also do NOT call Debouncer.Apply here, so any pending
		// downgrade recorded BEFORE the Suspended state is preserved
		// unchanged — when ArgoCD resumes syncing and the next event
		// produces a real status, the existing pending entry (if
		// any) carries forward.
		return
	}

	id := (component.Component{
		Kind: "Application", Namespace: spec.Namespace, Name: spec.Name,
	}).ID()

	prev, err := w.fetchPrev(ctx, id)
	if err != nil {
		w.logger.Error("get prev failed", "err", err, "id", id)
		return
	}

	write, finalStatus, retryAfter := w.debouncer.Apply(id, prev, next, w.clk.Now())
	if write {
		c := component.Component{
			Kind:        "Application",
			Namespace:   spec.Namespace,
			Name:        spec.Name,
			DisplayName: displayNameOrName(spec),
			Status:      finalStatus,
		}
		if err := w.store.UpsertComponent(ctx, c); err != nil {
			w.logger.Error("upsert failed",
				"err", err,
				"id", id,
				"status", string(finalStatus),
			)
			return
		}
		w.logger.Info("reconciled", "id", id, "status", string(finalStatus))
		return
	}
	if retryAfter > 0 {
		w.logger.Debug("debounced",
			"id", id,
			"prev", string(prev),
			"next", string(next),
			"retry_after", retryAfter,
		)
	}
}

func (w *ApplicationWatcher) fetchPrev(ctx context.Context, id string) (component.Status, error) {
	got, err := w.store.GetComponent(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return component.StatusUnknown, nil
	}
	if err != nil {
		return "", err
	}
	return got.Status, nil
}

// retry is the Debouncer's per-key callback. id format is
// "Application/<namespace>/<name>" — the canonical Component ID.
func (w *ApplicationWatcher) retry(id string) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 || parts[0] != "Application" {
		return
	}
	obj, err := w.lister.ByNamespace(parts[1]).Get(parts[2])
	if err != nil || obj == nil {
		// Object gone from the lister cache. Clear pending entry so
		// a same-ID recreate is treated as a fresh observation.
		w.debouncer.Forget(id)
		return
	}
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		w.debouncer.Forget(id)
		return
	}
	w.handle(w.ctx, u)
}
