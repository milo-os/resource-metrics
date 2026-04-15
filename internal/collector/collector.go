// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	authorizationv1 "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	controllermetrics "go.datum.net/resource-metrics/internal/metrics"
	"go.datum.net/resource-metrics/internal/policy"
)

// defaultResyncPeriod is the resync period used by the dynamic informer
// factory. A long resync keeps load low; we mostly rely on watches.
const defaultResyncPeriod = 10 * time.Minute

// cacheSyncTimeout bounds how long we wait for a new informer's cache to
// sync. If a cluster is slow or a resource is missing we move on and the
// reconcile loop will pick it up on the next wake.
//
// Declared as a var (not const) so tests can shorten the wait. Production
// code must not mutate it.
var cacheSyncTimeout = 30 * time.Second

// CollectedObjects is the snapshot returned by ProjectCollector.Collect for a
// single GVR.
type CollectedObjects struct {
	GVR     schema.GroupVersionResource
	Objects []map[string]any
}

// GVRStatus reports the observed state of a single GVR's informer.
type GVRStatus struct {
	Synced  bool
	Denied  bool
	LastErr string
}

// ProjectStatus summarises a ProjectCollector for the health endpoint and
// OTel up/down emitter.
type ProjectStatus struct {
	ClusterName string
	ProjectUp   bool
	GVRStatuses map[schema.GroupVersionResource]GVRStatus
}

// gvrInformer tracks everything we need to know about one running informer.
type gvrInformer struct {
	informer informers.GenericInformer
	cancel   context.CancelFunc
	synced   bool
	denied   bool
	lastErr  error
}

// ProjectCollector owns a per-project dynamic informer tree, driven by the
// policy.Registry. Reconcile() walks the registry snapshot, starts informers
// for GVRs that are newly desired, and stops informers for GVRs that are no
// longer desired.
type ProjectCollector struct {
	clusterName   string
	dynamicClient dynamic.Interface
	restMapper    meta.RESTMapper
	factory       dynamicinformer.DynamicSharedInformerFactory
	authzClient   authorizationv1.AuthorizationV1Interface

	mu        sync.RWMutex
	informers map[schema.GroupVersionResource]*gvrInformer

	registry *policy.Registry
	wake     chan struct{}

	ctx    context.Context
	cancel context.CancelFunc

	logger logr.Logger

	// stopped is closed when the background reconcile loop has exited.
	stopped chan struct{}
}

// NewProjectCollector builds a ProjectCollector for the given engaged cluster.
// It constructs a dynamic client and a dynamic informer factory from the
// cluster's rest.Config. The collector is not started until Start is called.
func NewProjectCollector(cl cluster.Cluster, clusterName string, registry *policy.Registry, logger logr.Logger) (*ProjectCollector, error) {
	if cl == nil {
		return nil, errors.New("collector: nil cluster")
	}
	if registry == nil {
		return nil, errors.New("collector: nil registry")
	}
	cfg := cl.GetConfig()
	if cfg == nil {
		return nil, errors.New("collector: cluster has no rest config")
	}
	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("collector: build dynamic client: %w", err)
	}
	authz, err := authorizationv1.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("collector: build authorization client: %w", err)
	}
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, defaultResyncPeriod)

	return &ProjectCollector{
		clusterName:   clusterName,
		dynamicClient: dynClient,
		restMapper:    cl.GetRESTMapper(),
		factory:       factory,
		authzClient:   authz,
		informers:     make(map[schema.GroupVersionResource]*gvrInformer),
		registry:      registry,
		wake:          make(chan struct{}, 1),
		logger:        logger.WithValues("cluster", clusterName),
		stopped:       make(chan struct{}),
	}, nil
}

// newProjectCollectorForTesting wires a ProjectCollector around a prebuilt
// dynamic client, factory, and authz client. Used by tests that want to
// drive reconcile without a live cluster.Cluster.
func newProjectCollectorForTesting(
	clusterName string,
	dynClient dynamic.Interface,
	factory dynamicinformer.DynamicSharedInformerFactory,
	authz authorizationv1.AuthorizationV1Interface,
	registry *policy.Registry,
	logger logr.Logger,
) *ProjectCollector {
	return &ProjectCollector{
		clusterName:   clusterName,
		dynamicClient: dynClient,
		factory:       factory,
		authzClient:   authz,
		informers:     make(map[schema.GroupVersionResource]*gvrInformer),
		registry:      registry,
		wake:          make(chan struct{}, 1),
		logger:        logger.WithValues("cluster", clusterName),
		stopped:       make(chan struct{}),
	}
}

// Start launches the background reconcile loop. It returns immediately. The
// loop exits when ctx is cancelled or Stop is called.
func (c *ProjectCollector) Start(ctx context.Context) error {
	c.mu.Lock()
	if c.ctx != nil {
		c.mu.Unlock()
		return errors.New("collector: already started")
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.mu.Unlock()

	go c.runLoop()

	// Do an initial reconcile even if the registry is empty so the map is
	// consistent and subsequent wakes do not race with a first-time caller.
	c.Wake()
	return nil
}

func (c *ProjectCollector) runLoop() {
	defer close(c.stopped)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.wake:
			c.reconcile()
		}
	}
}

// Wake signals the reconcile loop. Non-blocking: if a wake is already
// pending the signal is coalesced.
func (c *ProjectCollector) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// desiredGVRs computes the union of GVRs referenced by the current registry
// snapshot.
func (c *ProjectCollector) desiredGVRs() map[schema.GroupVersionResource]struct{} {
	out := make(map[schema.GroupVersionResource]struct{})
	for _, cp := range c.registry.Snapshot() {
		if cp == nil {
			continue
		}
		for _, gen := range cp.Generators {
			if gen == nil {
				continue
			}
			out[schema.GroupVersionResource{
				Group:    gen.Resource.Group,
				Version:  gen.Resource.Version,
				Resource: gen.Resource.Resource,
			}] = struct{}{}
		}
	}
	return out
}

// reconcile brings the set of live informers into agreement with
// desiredGVRs(). It is idempotent and safe to call repeatedly. Heavy work
// (SSAR, cache sync) happens without the map lock held; the map is mutated
// under a brief write lock at the end.
func (c *ProjectCollector) reconcile() {
	// Snapshot live set under read lock.
	c.mu.RLock()
	if c.ctx == nil {
		c.mu.RUnlock()
		return
	}
	present := make(map[schema.GroupVersionResource]*gvrInformer, len(c.informers))
	for gvr, inf := range c.informers {
		present[gvr] = inf
	}
	ctx := c.ctx
	c.mu.RUnlock()

	desired := c.desiredGVRs()

	// Plan additions and removals.
	var toAdd []schema.GroupVersionResource
	var toRemove []schema.GroupVersionResource
	for gvr := range desired {
		if _, ok := present[gvr]; !ok {
			toAdd = append(toAdd, gvr)
		}
	}
	for gvr := range present {
		if _, ok := desired[gvr]; !ok {
			toRemove = append(toRemove, gvr)
		}
	}

	// Apply removals first: cancel and delete from the map.
	if len(toRemove) > 0 {
		c.mu.Lock()
		for _, gvr := range toRemove {
			if inf, ok := c.informers[gvr]; ok {
				inf.cancel()
				delete(c.informers, gvr)
				c.logger.V(1).Info("stopped informer", "gvr", gvr.String())
			}
		}
		c.mu.Unlock()
	}

	// Apply additions: SSAR preflight + start informer + wait for sync.
	for _, gvr := range toAdd {
		entry := c.startInformer(ctx, gvr)
		if entry == nil {
			// startInformer reported a cache-sync failure and already
			// cancelled the informer goroutine. Do not record the entry;
			// the next reconcile will re-attempt because the GVR is still
			// desired — that's the intended backoff.
			continue
		}
		c.mu.Lock()
		// Re-check: a concurrent reconcile could already have populated this.
		if existing, ok := c.informers[gvr]; ok {
			// Another reconcile beat us to it. Cancel ours and keep the
			// existing entry so we don't double-run informers.
			if entry.cancel != nil {
				entry.cancel()
			}
			_ = existing
			c.mu.Unlock()
			continue
		}
		c.informers[gvr] = entry
		c.mu.Unlock()
	}
}

// startInformer performs the preflight and, on success, launches an informer
// for the given GVR. It returns:
//
//   - a non-nil entry for the happy path (synced) and for SSAR denials (so
//     reconcile records the denial and does not retry every wake);
//   - nil when the informer cache failed to sync (timeout or probe list
//     error). In that case the per-GVR goroutine has already been cancelled
//     and the caller must not record the entry — the next reconcile will
//     re-attempt because the GVR is still desired.
func (c *ProjectCollector) startInformer(parent context.Context, gvr schema.GroupVersionResource) *gvrInformer {
	log := c.logger.WithValues("gvr", gvr.String())

	allowed, ssarErr := c.preflight(parent, gvr)
	if ssarErr != nil {
		log.Info("ssar preflight failed", "error", ssarErr.Error())
		controllermetrics.RBACDeniedTotal.WithLabelValues(c.clusterName, gvr.String()).Inc()
		return &gvrInformer{denied: true, lastErr: ssarErr, cancel: noopCancel}
	}
	if !allowed {
		log.Info("ssar preflight denied list/watch access")
		controllermetrics.RBACDeniedTotal.WithLabelValues(c.clusterName, gvr.String()).Inc()
		return &gvrInformer{denied: true, lastErr: errors.New("ssar: list or watch not allowed"), cancel: noopCancel}
	}

	generic := c.factory.ForResource(gvr)
	informer := generic.Informer()

	runCtx, cancel := context.WithCancel(parent)

	// Run the informer on its own goroutine driven by runCtx.
	go informer.Run(runCtx.Done())

	// Wait for the cache to sync, bounded by cacheSyncTimeout.
	syncCtx, syncCancel := context.WithTimeout(runCtx, cacheSyncTimeout)
	defer syncCancel()
	synced := cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced)

	if !synced {
		err := syncCtx.Err()
		if err == nil {
			err = errors.New("cache: failed to sync")
		}
		// Classify the failure: if a direct list says NotFound/Forbidden,
		// count an RBAC denial. Either way we tear down the informer and
		// drop the entry so we don't leak a goroutine across reconciles.
		denied := isNotFoundOrForbidden(c.probeList(parent, gvr))
		if denied {
			controllermetrics.RBACDeniedTotal.WithLabelValues(c.clusterName, gvr.String()).Inc()
		}
		log.Info("informer cache sync failed; cancelling and dropping entry", "error", err.Error(), "denied", denied)
		// Cancel the informer goroutine so it doesn't linger in the map —
		// or outside it. The next reconcile will re-attempt.
		cancel()
		return nil
	}

	log.V(1).Info("informer started")
	return &gvrInformer{
		informer: generic,
		cancel:   cancel,
		synced:   true,
	}
}

// probeList performs a small list request against the live API. It is used
// only to classify failure modes when WaitForCacheSync times out. The
// returned error is inspected by the caller.
func (c *ProjectCollector) probeList(ctx context.Context, gvr schema.GroupVersionResource) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.dynamicClient.Resource(gvr).List(probeCtx, listOptionsOne())
	return err
}

// Collect returns a snapshot of every synced, non-denied informer. Entries
// that are not synced or denied are skipped. The returned slice is owned by
// the caller.
func (c *ProjectCollector) Collect() []CollectedObjects {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := make([]CollectedObjects, 0, len(c.informers))
	for gvr, inf := range c.informers {
		if inf == nil || !inf.synced || inf.denied {
			continue
		}
		objs, err := inf.informer.Lister().List(labels.Everything())
		if err != nil {
			c.logger.V(1).Info("lister error", "gvr", gvr.String(), "error", err.Error())
			continue
		}
		items := make([]map[string]any, 0, len(objs))
		for _, o := range objs {
			if m := toUnstructuredContent(o); m != nil {
				items = append(items, m)
			}
		}
		out = append(out, CollectedObjects{GVR: gvr, Objects: items})
	}
	return out
}

// Stop cancels the collector's context, which in turn cancels every per-GVR
// informer goroutine, and waits for the reconcile loop to exit or for ctx
// to expire.
func (c *ProjectCollector) Stop(ctx context.Context) error {
	c.mu.Lock()
	cancel := c.cancel
	stopped := c.stopped
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if stopped == nil {
		return nil
	}

	select {
	case <-stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Status returns a snapshot of the collector state for the health endpoint
// and OTel up/down emitter.
func (c *ProjectCollector) Status() ProjectStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	statuses := make(map[schema.GroupVersionResource]GVRStatus, len(c.informers))
	anySynced := false
	anyFatal := false
	for gvr, inf := range c.informers {
		s := GVRStatus{
			Synced: inf.synced,
			Denied: inf.denied,
		}
		if inf.lastErr != nil {
			s.LastErr = inf.lastErr.Error()
		}
		if inf.synced {
			anySynced = true
		}
		// Treat a non-denied, unsynced informer with an error as fatal for
		// the purpose of ProjectUp.
		if !inf.synced && !inf.denied && inf.lastErr != nil {
			anyFatal = true
		}
		statuses[gvr] = s
	}

	return ProjectStatus{
		ClusterName: c.clusterName,
		ProjectUp:   anySynced && !anyFatal,
		GVRStatuses: statuses,
	}
}

// ClusterName returns the cluster name this collector is bound to.
func (c *ProjectCollector) ClusterName() string { return c.clusterName }

// toUnstructuredContent best-effort extracts the map form of an informer
// store object. Dynamic informers emit *unstructured.Unstructured.
func toUnstructuredContent(obj runtime.Object) map[string]any {
	switch v := obj.(type) {
	case *unstructured.Unstructured:
		if v == nil {
			return nil
		}
		return v.UnstructuredContent()
	case runtime.Unstructured:
		return v.UnstructuredContent()
	}
	return nil
}

// noopCancel is assigned to denied informers so that deletion code paths can
// unconditionally call cancel() without a nil check.
func noopCancel() {}

// isNotFoundOrForbidden classifies an API error as "denied" for the purpose
// of marking an informer as denied rather than retrying forever.
func isNotFoundOrForbidden(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsNotFound(err) || apierrors.IsForbidden(err) ||
		meta.IsNoMatchError(err) || apierrors.IsMethodNotSupported(err)
}
