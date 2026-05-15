package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// DeploymentWatcher watches apps/v1.Deployment events for the
// configured set of components and reconciles their status into the
// store. Only Deployments whose (namespace, name) appears in the
// configured set produce store writes; all other Deployments in the
// cluster are ignored.
//
// Status mapping is replica-count only in v0.1; #19 will harden it
// with Progressing=True + debounce. Rules are evaluated top-to-bottom,
// so scale-to-zero (desired==0, ready==0) resolves to down rather
// than operational:
//   - readyReplicas == 0                         → down
//   - readyReplicas < spec.replicas              → degraded
//   - readyReplicas >= spec.replicas             → operational
//
// In-cluster RBAC: the watcher needs get, list, watch on
// apps/v1/deployments. The Helm chart (#24) will ship the appropriate
// ClusterRole and binding.
type DeploymentWatcher struct {
	client  kubernetes.Interface
	store   store.Store
	watched map[types.NamespacedName]config.Spec
	logger  *slog.Logger

	mu       sync.Mutex
	lastSeen map[types.NamespacedName]component.Status

	syncedCh   chan struct{}
	syncedOnce sync.Once
}

// NewDeploymentWatcher constructs the watcher from a clientset, a
// store, and the configured component specs. It filters specs to
// Kind == "Deployment" and builds the lookup table. An empty or nil
// specs slice is valid — the watcher runs but writes nothing because
// every event is filtered out. A nil logger falls back to
// slog.Default().
func NewDeploymentWatcher(
	client kubernetes.Interface,
	st store.Store,
	specs []config.Spec,
	logger *slog.Logger,
) *DeploymentWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	watched := make(map[types.NamespacedName]config.Spec)
	for _, s := range specs {
		if s.Kind != "Deployment" {
			continue
		}
		key := types.NamespacedName{Namespace: s.Namespace, Name: s.Name}
		watched[key] = s
	}
	return &DeploymentWatcher{
		client:   client,
		store:    st,
		watched:  watched,
		logger:   logger.With("watcher", "deployment"),
		lastSeen: make(map[types.NamespacedName]component.Status),
		syncedCh: make(chan struct{}),
	}
}

// Synced returns a channel that is closed once the watcher's informer
// cache has finished its initial list/watch sync. Useful for tests
// (avoids a race between Start and the first test mutation) and for
// startup ordering (callers can log "ready" only after sync).
func (w *DeploymentWatcher) Synced() <-chan struct{} {
	return w.syncedCh
}

// Start runs the informer until ctx is cancelled. It blocks; the
// caller is expected to launch it in its own goroutine. Returns nil
// on clean shutdown via ctx.Done(); returns a non-nil error if the
// informer cache fails to sync. Resync period is 0 (event-driven
// only) — #19 may revisit this when adding debounce.
func (w *DeploymentWatcher) Start(ctx context.Context) error {
	factory := informers.NewSharedInformerFactory(w.client, 0)
	informer := factory.Apps().V1().Deployments().Informer()

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			d, ok := obj.(*appsv1.Deployment)
			if !ok {
				return
			}
			w.handle(ctx, d)
		},
		UpdateFunc: func(_, newObj interface{}) {
			d, ok := newObj.(*appsv1.Deployment)
			if !ok {
				return
			}
			w.handle(ctx, d)
		},
		DeleteFunc: func(_ interface{}) {
			// Deletion leaves the last status in place (acceptance
			// criterion: "delete leaves status as down, no crash").
			// We don't clear lastSeen so a same-status recreate stays
			// a no-op; we don't upsert because there's no new state
			// to record.
		},
	}); err != nil {
		return fmt.Errorf("watcher: add event handler: %w", err)
	}

	factory.Start(ctx.Done())
	if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
		// WaitForCacheSync returns false when stopCh is closed before
		// HasSynced flips true. Treat that as a clean shutdown — the
		// informer poll period (100ms) is racy with rapid cancel
		// even when events were already flowing.
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("watcher: deployment cache sync failed")
	}
	w.syncedOnce.Do(func() { close(w.syncedCh) })
	w.logger.Info("synced", "watched", len(w.watched))

	<-ctx.Done()
	return nil
}

func (w *DeploymentWatcher) handle(ctx context.Context, d *appsv1.Deployment) {
	key := types.NamespacedName{Namespace: d.Namespace, Name: d.Name}
	spec, ok := w.watched[key]
	if !ok {
		return
	}

	status := computeDeploymentStatus(d)

	w.mu.Lock()
	prev, seen := w.lastSeen[key]
	if seen && prev == status {
		w.mu.Unlock()
		return
	}
	w.lastSeen[key] = status
	w.mu.Unlock()

	c := component.Component{
		Kind:        "Deployment",
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
		// Roll back lastSeen so the next event for this key retries.
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

// computeDeploymentStatus maps Deployment replica counts to a
// component.Status. spec.replicas defaults to 1 when unset (k8s
// default). Used as a package-private helper rather than a method so
// it's directly testable without constructing a DeploymentWatcher.
func computeDeploymentStatus(d *appsv1.Deployment) component.Status {
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	ready := d.Status.ReadyReplicas
	switch {
	case ready == 0:
		return component.StatusDown
	case ready < desired:
		return component.StatusDegraded
	default:
		return component.StatusOperational
	}
}

func displayNameOrName(s config.Spec) string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}
