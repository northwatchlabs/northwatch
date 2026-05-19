package watcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appsv1listers "k8s.io/client-go/listers/apps/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/clock"

	"github.com/northwatchlabs/northwatch/internal/component"
	"github.com/northwatchlabs/northwatch/internal/config"
	"github.com/northwatchlabs/northwatch/internal/status"
	"github.com/northwatchlabs/northwatch/internal/store"
)

// DeploymentWatcher watches apps/v1.Deployment events for the
// configured set of components, maps them via status.MapDeployment,
// and routes the (prev, next) transition through its own Debouncer
// before writing to the store. Status mapping rules and the
// debounce protocol live in internal/status/.
//
// In-cluster RBAC: needs get, list, watch on apps/v1/deployments.
type DeploymentWatcher struct {
	client  kubernetes.Interface
	store   store.Store
	watched map[types.NamespacedName]config.Spec
	logger  *slog.Logger
	window  time.Duration
	clk     clock.WithDelayedExecution

	// populated in Start once the informer factory exists
	lister    appsv1listers.DeploymentLister
	debouncer *status.Debouncer
	ctx       context.Context

	syncedCh   chan struct{}
	syncedOnce sync.Once
}

// NewDeploymentWatcher constructs the watcher. window=0 disables
// debouncing (Apply writes immediately). clk=nil defaults to
// clock.RealClock{}. A nil logger falls back to slog.Default().
func NewDeploymentWatcher(
	client kubernetes.Interface,
	st store.Store,
	specs []config.Spec,
	logger *slog.Logger,
	window time.Duration,
	clk clock.WithDelayedExecution,
) *DeploymentWatcher {
	if logger == nil {
		logger = slog.Default()
	}
	if clk == nil {
		clk = clock.RealClock{}
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
		window:   window,
		clk:      clk,
		syncedCh: make(chan struct{}),
	}
}

// Synced returns a channel closed once the informer's initial sync
// completes.
func (w *DeploymentWatcher) Synced() <-chan struct{} { return w.syncedCh }

// Start runs the informer until ctx is cancelled. Blocks; the caller
// launches in its own goroutine. Returns nil on clean shutdown via
// ctx.Done(); returns a non-nil error if the informer cache fails to
// sync.
func (w *DeploymentWatcher) Start(ctx context.Context) error {
	w.ctx = ctx
	factory := informers.NewSharedInformerFactory(w.client, 0)
	informer := factory.Apps().V1().Deployments().Informer()
	w.lister = factory.Apps().V1().Deployments().Lister()
	w.debouncer = status.NewDebouncer(w.window, w.clk, w.retry)
	defer w.debouncer.Stop()

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if d, ok := obj.(*appsv1.Deployment); ok {
				w.handle(ctx, d)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if d, ok := newObj.(*appsv1.Deployment); ok {
				w.handle(ctx, d)
			}
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
		// WaitForCacheSync returns false when stopCh is closed before
		// HasSynced flips true. Treat that as a clean shutdown.
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

	next := status.MapDeployment(d)
	id := (component.Component{
		Kind: "Deployment", Namespace: spec.Namespace, Name: spec.Name,
	}).ID()

	prev, err := w.fetchPrev(ctx, id)
	if err != nil {
		w.logger.Error("get prev failed", "err", err, "id", id)
		return
	}

	write, finalStatus, retryAfter := w.debouncer.Apply(id, prev, next, w.clk.Now())
	if write {
		c := component.Component{
			Kind:        "Deployment",
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

func (w *DeploymentWatcher) fetchPrev(ctx context.Context, id string) (component.Status, error) {
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
// "Deployment/<namespace>/<name>" — the canonical Component ID.
func (w *DeploymentWatcher) retry(id string) {
	parts := strings.SplitN(id, "/", 3)
	if len(parts) != 3 || parts[0] != "Deployment" {
		return
	}
	d, err := w.lister.Deployments(parts[1]).Get(parts[2])
	if err != nil || d == nil {
		// Object gone from the lister cache. Clear pending entry so
		// a same-ID recreate is treated as a fresh observation.
		w.debouncer.Forget(id)
		return
	}
	w.handle(w.ctx, d)
}

func displayNameOrName(s config.Spec) string {
	if s.DisplayName != "" {
		return s.DisplayName
	}
	return s.Name
}
