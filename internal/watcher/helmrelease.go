package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
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
// component set and reconciles status from the Ready condition. Only
// HelmReleases whose (namespace, name) appears in the configured set
// produce store writes; all others are ignored.
//
// Status mapping reads status.conditions[type=Ready]; #19 will harden
// this with debounce and multi-condition merging:
//   - Ready=True                       → operational
//   - Ready=False, reason=Progressing  → degraded
//   - Ready=False, other reason        → down
//   - Ready missing/Unknown            → unknown
//
// In-cluster RBAC: needs get, list, watch on
// helm.toolkit.fluxcd.io/v2/helmreleases. The Helm chart (#24) will
// ship the appropriate ClusterRole.
type HelmReleaseWatcher struct {
	client  dynamic.Interface
	store   store.Store
	watched map[types.NamespacedName]config.Spec
	logger  *slog.Logger

	mu       sync.Mutex
	lastSeen map[types.NamespacedName]component.Status

	syncedCh   chan struct{}
	syncedOnce sync.Once
}

// NewHelmReleaseWatcher mirrors NewDeploymentWatcher: it filters
// specs to Kind == "HelmRelease" and builds the lookup map. An empty
// specs slice is valid — the watcher runs but writes nothing.
func NewHelmReleaseWatcher(
	client dynamic.Interface,
	st store.Store,
	specs []config.Spec,
	logger *slog.Logger,
) *HelmReleaseWatcher {
	if logger == nil {
		logger = slog.Default()
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
		lastSeen: make(map[types.NamespacedName]component.Status),
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
	factory := dynamicinformer.NewDynamicSharedInformerFactory(w.client, 0)
	informer := factory.ForResource(HelmReleaseGVR).Informer()

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
			// Same semantics as DeploymentWatcher: the last status
			// sticks; we don't clear lastSeen or write a recovery.
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

	status := computeHelmReleaseStatus(u)

	w.mu.Lock()
	prev, seen := w.lastSeen[key]
	if seen && prev == status {
		w.mu.Unlock()
		return
	}
	w.lastSeen[key] = status
	w.mu.Unlock()

	c := component.Component{
		Kind:        "HelmRelease",
		Namespace:   spec.Namespace,
		Name:        spec.Name,
		DisplayName: displayNameOrName(spec),
		Status:      status,
	}
	if err := w.store.UpsertComponent(ctx, c); err != nil {
		w.logger.Error("upsert failed",
			"err", err,
			"id", c.ID(),
			"status", string(status),
		)
		w.mu.Lock()
		if seen {
			w.lastSeen[key] = prev
		} else {
			delete(w.lastSeen, key)
		}
		w.mu.Unlock()
		return
	}
	w.logger.Info("reconciled",
		"id", c.ID(),
		"status", string(status),
	)
}

// computeHelmReleaseStatus maps status.conditions[type=Ready] to a
// component.Status. Returns unknown when the Ready condition is
// missing or its status field is not True/False — typical for a
// HelmRelease that Flux has not yet observed.
func computeHelmReleaseStatus(u *unstructured.Unstructured) component.Status {
	conds, found, err := unstructured.NestedSlice(u.Object, "status", "conditions")
	if err != nil || !found {
		return component.StatusUnknown
	}
	for _, c := range conds {
		m, ok := c.(map[string]interface{})
		if !ok {
			continue
		}
		if t, _ := m["type"].(string); t != "Ready" {
			continue
		}
		s, _ := m["status"].(string)
		switch s {
		case "True":
			return component.StatusOperational
		case "False":
			reason, _ := m["reason"].(string)
			if reason == "Progressing" {
				return component.StatusDegraded
			}
			return component.StatusDown
		default:
			return component.StatusUnknown
		}
	}
	return component.StatusUnknown
}
