package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

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

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
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
// configured component set and reconciles status from
// status.health.status. Only Applications whose (namespace, name)
// appears in the configured set produce store writes; all others are
// ignored.
//
// Status mapping (see computeApplicationStatus):
//   - Healthy                  → operational
//   - Progressing              → degraded
//   - Degraded, Missing        → down
//   - Unknown                  → unknown
//   - Suspended                → preserve previous (no upsert)
//   - field missing            → unknown (initial state)
//   - unrecognized             → unknown (forward-compat)
//
// Only Suspended preserves the previously recorded status: it's an
// intentional operator action ("I paused syncing"), and flipping the
// status page on that signal would be noise. Unknown is the opposite
// — it means ArgoCD lost the signal — so we surface it as `unknown`
// rather than silently holding a stale `operational`. Same logic for
// future unrecognized health values: never lie about state we don't
// have.
//
// In-cluster RBAC: needs get, list, watch on
// argoproj.io/v1alpha1/applications. The Helm chart (#24) will ship
// the appropriate ClusterRole.
type ApplicationWatcher struct {
	client  dynamic.Interface
	store   store.Store
	watched map[types.NamespacedName]config.Spec
	logger  *slog.Logger

	mu       sync.Mutex
	lastSeen map[types.NamespacedName]component.Status

	syncedCh   chan struct{}
	syncedOnce sync.Once
}

// NewApplicationWatcher mirrors NewHelmReleaseWatcher: it filters
// specs to Kind == "Application" and builds the lookup map. An empty
// specs slice is valid — the watcher runs but writes nothing.
func NewApplicationWatcher(
	client dynamic.Interface,
	st store.Store,
	specs []config.Spec,
	logger *slog.Logger,
) *ApplicationWatcher {
	if logger == nil {
		logger = slog.Default()
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
		lastSeen: make(map[types.NamespacedName]component.Status),
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
	factory := dynamicinformer.NewDynamicSharedInformerFactory(w.client, 0)
	informer := factory.ForResource(ApplicationGVR).Informer()

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
			// Same semantics as the other watchers: the last status
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

	status, ok := computeApplicationStatus(u)
	if !ok {
		// Suspended: preserve whatever status is already recorded.
		// Leave lastSeen alone so the next real transition is still
		// observed.
		return
	}

	w.mu.Lock()
	prev, seen := w.lastSeen[key]
	if seen && prev == status {
		w.mu.Unlock()
		return
	}
	w.lastSeen[key] = status
	w.mu.Unlock()

	c := component.Component{
		Kind:        "Application",
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

// computeApplicationStatus maps status.health.status to a
// component.Status. The second return is false only for `Suspended`
// — callers skip the upsert in that case to preserve the previously
// recorded status.
//
// Explicit `Unknown` and unrecognized future values map to
// StatusUnknown (with ok=true) rather than preserve-previous. ArgoCD
// reports Unknown when it has lost the health signal (repo-server
// hiccup, failed refresh, missing health check), and preserving a
// stale `operational` while we genuinely don't know the state would
// be a status-page lie.
//
// Returns (StatusUnknown, true) when the field is missing entirely
// (typical for an Application that ArgoCD has not yet observed) so
// the initial state is recorded explicitly rather than left at the
// SyncComponents default.
func computeApplicationStatus(u *unstructured.Unstructured) (component.Status, bool) {
	health, found, err := unstructured.NestedString(u.Object, "status", "health", "status")
	if err != nil || !found || health == "" {
		return component.StatusUnknown, true
	}
	switch health {
	case "Healthy":
		return component.StatusOperational, true
	case "Progressing":
		return component.StatusDegraded, true
	case "Degraded", "Missing":
		return component.StatusDown, true
	case "Unknown":
		return component.StatusUnknown, true
	case "Suspended":
		return "", false
	default:
		return component.StatusUnknown, true
	}
}
