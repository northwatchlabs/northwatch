package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

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

// HelmReleaseGVR is the dynamic GroupVersionResource for Flux's
// helm.toolkit.fluxcd.io/v2 HelmRelease custom resource.
var HelmReleaseGVR = schema.GroupVersionResource{
	Group:    "helm.toolkit.fluxcd.io",
	Version:  "v2",
	Resource: "helmreleases",
}

// HelmReleaseCRDPresent returns true if helm.toolkit.fluxcd.io/v2 is
// registered on the cluster's API server. Use to gate watcher
// construction so clusters without Flux installed boot cleanly
// instead of crashing on a missing CRD.
//
// The probe runs against a copied rest.Config with pingTimeout so a
// stalled discovery endpoint can't hang serve startup or SIGTERM
// handling — the shared config is intentionally un-timed because
// list/watch streams must outlive the timeout. ctx is honored even
// though client-go v0.35's discovery API has no context-accepting
// variant; the in-flight request is raced against ctx.Done() via a
// goroutine in helmReleaseCRDPresentVia.
func HelmReleaseCRDPresent(ctx context.Context, cfg *rest.Config) (bool, error) {
	probeCfg := rest.CopyConfig(cfg)
	probeCfg.Timeout = pingTimeout

	disc, err := discovery.NewDiscoveryClientForConfig(probeCfg)
	if err != nil {
		return false, fmt.Errorf("build discovery client: %w", err)
	}
	return helmReleaseCRDPresentVia(ctx, disc)
}

// helmReleaseCRDPresentVia is the testable seam: it accepts a
// pre-built ServerGroupsInterface so tests can drive it with a fake
// without spinning up an HTTP server. The cancellation race against
// ctx.Done() lives here so the goroutine semantics are exercised by
// unit tests.
func helmReleaseCRDPresentVia(ctx context.Context, disc discovery.ServerGroupsInterface) (bool, error) {
	type result struct {
		groups *metav1.APIGroupList
		err    error
	}
	resultCh := make(chan result, 1)
	go func() {
		groups, err := disc.ServerGroups()
		resultCh <- result{groups: groups, err: err}
	}()

	var groups *metav1.APIGroupList
	select {
	case <-ctx.Done():
		return false, fmt.Errorf("helmrelease CRD probe cancelled: %w", ctx.Err())
	case r := <-resultCh:
		if r.err != nil {
			return false, fmt.Errorf("list server groups: %w", r.err)
		}
		groups = r.groups
	}

	for _, g := range groups.Groups {
		if g.Name != HelmReleaseGVR.Group {
			continue
		}
		for _, v := range g.Versions {
			if v.Version == HelmReleaseGVR.Version {
				return true, nil
			}
		}
	}
	return false, nil
}

// HelmReleaseWatcher watches HelmRelease events for the configured
// component set, maps them via status.MapHelmRelease, and routes the
// (prev, next) transition through its own Debouncer before writing
// to the store. Status mapping rules and the debounce protocol live
// in internal/status/.
//
// In-cluster RBAC: needs get, list, watch on
// helm.toolkit.fluxcd.io/v2/helmreleases. The Helm chart (#24) will
// ship the appropriate ClusterRole.
type HelmReleaseWatcher struct {
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

// NewHelmReleaseWatcher mirrors NewDeploymentWatcher: it filters
// specs to Kind == "HelmRelease" and builds the lookup map. An empty
// specs slice is valid — the watcher runs but writes nothing.
// window=0 disables debouncing (Apply writes immediately). clk=nil
// defaults to clock.RealClock{}. A nil logger falls back to
// slog.Default().
func NewHelmReleaseWatcher(
	client dynamic.Interface,
	st store.Store,
	specs []config.Spec,
	logger *slog.Logger,
	window time.Duration,
	clk clock.WithDelayedExecution,
) *HelmReleaseWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.RealClock{}
	}
	watched := make(map[types.NamespacedName]config.Spec)
	for _, s := range specs {
		if s.Kind != "HelmRelease" {
			continue
		}
		key := types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
		watched[key] = s
	}
	return &HelmReleaseWatcher{
		client:   client,
		store:    st,
		watched:  watched,
		logger:   logger.With("watcher", "helmrelease"),
		window:   window,
		clk:      clk,
		syncedCh: make(chan struct{}),
	}
}

// Synced returns a channel that is closed once the watcher's informer
// cache has finished its initial list/watch sync.
func (w *HelmReleaseWatcher) Synced() <-chan struct{} {
	return w.syncedCh
}

// Start runs the dynamic informer until ctx is cancelled. Blocks; the
// caller is expected to launch it in its own goroutine.
func (w *HelmReleaseWatcher) Start(ctx context.Context) error {
	w.ctx = ctx
	factory := dynamicinformer.NewDynamicSharedInformerFactory(w.client, 0)
	informer := factory.ForResource(HelmReleaseGVR).Informer()
	w.lister = factory.ForResource(HelmReleaseGVR).Lister()
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
		return errors.New("watcher: helmrelease cache sync failed")
	}
	w.syncedOnce.Do(func() { close(w.syncedCh) })
	w.logger.Info("synced", "watched", len(w.watched))

	<-ctx.Done()
	return nil
}

func (w *HelmReleaseWatcher) handle(ctx context.Context, u *unstructured.Unstructured) {
	key := types.NamespacedName{Namespace: u.GetNamespace(), Name: u.GetName()}
	spec, ok := w.watched[key]
	if !ok {
		return
	}

	next := status.MapHelmRelease(u)
	id := (component.Component{
		Kind: "HelmRelease", Namespace: spec.Namespace, Name: spec.Name,
	}).ID()

	prev, err := w.fetchPrev(ctx, id)
	if err != nil {
		w.logger.Error("get prev failed", "err", err, "id", id)
		return
	}

	write, finalStatus, retryAfter := w.debouncer.Apply(id, prev, next, w.clk.Now())
	if write {
		c := component.Component{
			Kind:        "HelmRelease",
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

func (w *HelmReleaseWatcher) fetchPrev(ctx context.Context, id string) (component.Status, error) {
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
// "HelmRelease/<namespace>/<name>" — the canonical Component ID.
func (w *HelmReleaseWatcher) retry(id string) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 || parts[0] != "HelmRelease" {
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
