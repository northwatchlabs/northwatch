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

// KustomizationGVR is the dynamic GroupVersionResource for Flux's
// kustomize.toolkit.fluxcd.io/v1 Kustomization custom resource. Flux
// 2.0 (July 2023) stabilized v1; pre-v1 versions are EOL and not
// supported here. If clusters running ancient Flux need to be
// supported later, add a multi-version resolver paralleling
// ResolveHelmReleaseGVR.
var KustomizationGVR = schema.GroupVersionResource{
	Group:    "kustomize.toolkit.fluxcd.io",
	Version:  "v1",
	Resource: "kustomizations",
}

// KustomizationCRDPresent returns true when the Flux Kustomization
// CRD (kustomize.toolkit.fluxcd.io/v1.kustomizations) is served by
// the cluster. Use to gate watcher construction so clusters without
// Flux's kustomize-controller boot cleanly instead of failing the
// informer sync on a missing CRD.
//
// The probe checks the resource list for KustomizationGVR.GroupVersion
// and verifies kustomizations appears in the resources — not just
// that the group/version is registered — so a cluster with the group
// registered for some unrelated resource doesn't false-positive.
//
// A copied rest.Config caps discovery at pingTimeout so a stalled
// endpoint can't hang serve startup or SIGTERM handling, and the
// in-flight request is raced against ctx.Done() via a goroutine in
// kustomizationCRDPresentVia.
func KustomizationCRDPresent(ctx context.Context, cfg *rest.Config) (bool, error) {
	probeCfg := rest.CopyConfig(cfg)
	probeCfg.Timeout = pingTimeout

	disc, err := discovery.NewDiscoveryClientForConfig(probeCfg)
	if err != nil {
		return false, fmt.Errorf("build discovery client: %w", err)
	}
	return kustomizationCRDPresentVia(ctx, disc)
}

// kustomizationResourceDiscovery is the small surface
// kustomizationCRDPresentVia needs — narrower than
// discovery.DiscoveryInterface so tests can drive it with a tiny stub.
type kustomizationResourceDiscovery interface {
	ServerResourcesForGroupVersion(groupVersion string) (*metav1.APIResourceList, error)
}

// kustomizationCRDPresentVia is the testable seam. A NotFound from
// ServerResourcesForGroupVersion means the group/version is not
// served at all (the common non-Flux cluster) and is treated as a
// clean "absent" result rather than a probe failure.
func kustomizationCRDPresentVia(ctx context.Context, disc kustomizationResourceDiscovery) (bool, error) {
	type result struct {
		list *metav1.APIResourceList
		err  error
	}
	resultCh := make(chan result, 1)
	gv := KustomizationGVR.GroupVersion().String()
	go func() {
		list, err := disc.ServerResourcesForGroupVersion(gv)
		resultCh <- result{list: list, err: err}
	}()

	var list *metav1.APIResourceList
	select {
	case <-ctx.Done():
		return false, fmt.Errorf("kustomization CRD probe cancelled: %w", ctx.Err())
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
		if res.Name == KustomizationGVR.Resource {
			return true, nil
		}
	}
	return false, nil
}

// KustomizationWatcher watches Kustomization events for the
// configured component set, maps them via status.MapKustomization,
// and routes the (prev, next) transition through its own Debouncer
// before writing to the store. Status mapping rules and the debounce
// protocol live in internal/status/.
//
// In-cluster RBAC: needs get, list, watch on
// kustomize.toolkit.fluxcd.io/v1/kustomizations. The Helm chart (#24)
// will ship the appropriate ClusterRole.
//
// Cache-sync failures (e.g., forbidden list/watch) currently abort
// the watcher and propagate up through serveCmd's errCh, mirroring
// the existing HelmRelease/Application/Deployment behavior. Issue
// #58 tracks switching to log-and-degrade across all four watchers.
type KustomizationWatcher struct {
	client  dynamic.Interface
	store   store.Store
	watched map[types.NamespacedName]config.Spec
	logger  *slog.Logger
	window  time.Duration
	clk     clock.WithDelayedExecution
	gvr     schema.GroupVersionResource

	lister    cache.GenericLister
	debouncer *status.Debouncer
	ctx       context.Context

	syncedCh   chan struct{}
	syncedOnce sync.Once
}

// NewKustomizationWatcher mirrors NewHelmReleaseWatcher: it filters
// specs to Kind == "Kustomization" and builds the lookup map. An
// empty filtered set is valid — the watcher runs but writes nothing.
// window=0 disables debouncing (Apply writes immediately). clk=nil
// defaults to clock.RealClock{}. A nil logger falls back to
// slog.Default().
func NewKustomizationWatcher(
	client dynamic.Interface,
	st store.Store,
	specs []config.Spec,
	logger *slog.Logger,
	window time.Duration,
	clk clock.WithDelayedExecution,
	gvr schema.GroupVersionResource,
) *KustomizationWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.RealClock{}
	}
	watched := make(map[types.NamespacedName]config.Spec)
	for _, s := range specs {
		if s.Kind != "Kustomization" {
			continue
		}
		key := types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
		watched[key] = s
	}
	return &KustomizationWatcher{
		client:   client,
		store:    st,
		watched:  watched,
		logger:   logger.With("watcher", "kustomization"),
		window:   window,
		clk:      clk,
		gvr:      gvr,
		syncedCh: make(chan struct{}),
	}
}

// Synced returns a channel that is closed once the watcher's informer
// cache has finished its initial list/watch sync.
func (w *KustomizationWatcher) Synced() <-chan struct{} {
	return w.syncedCh
}

// Start runs the dynamic informer until ctx is cancelled. Blocks; the
// caller is expected to launch it in its own goroutine.
func (w *KustomizationWatcher) Start(ctx context.Context) error {
	w.ctx = ctx
	factory := dynamicinformer.NewDynamicSharedInformerFactory(w.client, 0)
	informer := factory.ForResource(w.gvr).Informer()
	w.lister = factory.ForResource(w.gvr).Lister()
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
		return errors.New("watcher: kustomization cache sync failed")
	}
	w.syncedOnce.Do(func() { close(w.syncedCh) })
	w.logger.Info("synced", "watched", len(w.watched))

	<-ctx.Done()
	return nil
}

func (w *KustomizationWatcher) handle(ctx context.Context, u *unstructured.Unstructured) {
	key := types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()}
	spec, ok := w.watched[key]
	if !ok {
		return
	}

	next := status.MapKustomization(u)
	id := (component.Component{
		Kind: "Kustomization", Namespace: spec.Namespace, Name: spec.Name,
	}).ID()

	prev, err := w.fetchPrev(ctx, id)
	if err != nil {
		w.logger.Error("get prev failed", "err", err, "id", id)
		return
	}

	write, finalStatus, retryAfter := w.debouncer.Apply(id, prev, next, w.clk.Now())
	if write {
		c := component.Component{
			Kind:        "Kustomization",
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

func (w *KustomizationWatcher) fetchPrev(ctx context.Context, id string) (component.Status, error) {
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
// "Kustomization/<namespace>/<name>" — the canonical Component ID.
func (w *KustomizationWatcher) retry(id string) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 || parts[0] != "Kustomization" {
		return
	}
	obj, err := w.lister.ByNamespace(parts[1]).Get(parts[2])
	if err != nil || obj == nil {
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
