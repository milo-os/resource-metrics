// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
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

// CollectedObjects is the snapshot returned by ControlPlaneCollector.Collect for a
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

// ControlPlaneStatus summarises a ControlPlaneCollector for the health endpoint and
// OTel up/down emitter.
type ControlPlaneStatus struct {
	ClusterName    string
	ControlPlaneUp bool
	GVRStatuses    map[schema.GroupVersionResource]GVRStatus
}

// gvrInformer tracks everything we need to know about one running informer.
// fields and stop are non-nil iff a goroutine was started for this GVR;
// denied/probe-rejected entries have only the bookkeeping fields populated.
type gvrInformer struct {
	informer informers.GenericInformer
	fields   *sharedRequiredFields
	stop     context.CancelFunc
	synced   bool
	denied   bool
	lastErr  error
}

// ControlPlaneCollector owns a dynamic informer tree for a single control plane,
// driven by the policy.Registry. Reconcile() walks the registry snapshot, starts
// informers for GVRs that are newly desired, and stops informers for GVRs that
// are no longer desired. Per-GVR DynamicSharedInformerFactory instances are
// constructed lazily in buildInformer so an informer can be torn down and
// rebuilt — needed when a policy's field set expands and the existing cache
// contents are no longer a superset of what policies require.
type ControlPlaneCollector struct {
	clusterName   string
	dynamicClient dynamic.Interface
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

// NewControlPlaneCollector builds a ControlPlaneCollector for the given engaged cluster.
// It constructs a dynamic client from the cluster's rest.Config; per-GVR
// informer factories are constructed lazily inside buildInformer. The
// collector is not started until Start is called.
func NewControlPlaneCollector(cl cluster.Cluster, clusterName string, registry *policy.Registry, logger logr.Logger) (*ControlPlaneCollector, error) {
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

	return &ControlPlaneCollector{
		clusterName:   clusterName,
		dynamicClient: dynClient,
		authzClient:   authz,
		informers:     make(map[schema.GroupVersionResource]*gvrInformer),
		registry:      registry,
		wake:          make(chan struct{}, 1),
		logger:        logger.WithValues("cluster", clusterName),
		stopped:       make(chan struct{}),
	}, nil
}

// newControlPlaneCollectorForTesting wires a ControlPlaneCollector around a
// prebuilt dynamic client and authz client. Used by tests that want to drive
// reconcile without a live cluster.Cluster.
func newControlPlaneCollectorForTesting(
	clusterName string,
	dynClient dynamic.Interface,
	authz authorizationv1.AuthorizationV1Interface,
	registry *policy.Registry,
	logger logr.Logger,
) *ControlPlaneCollector {
	return &ControlPlaneCollector{
		clusterName:   clusterName,
		dynamicClient: dynClient,
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
func (c *ControlPlaneCollector) Start(ctx context.Context) error {
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

func (c *ControlPlaneCollector) runLoop() {
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
func (c *ControlPlaneCollector) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// desiredGVRs computes the union of GVRs referenced by the current registry
// snapshot.
func (c *ControlPlaneCollector) desiredGVRs() map[schema.GroupVersionResource][]string {
	out := make(map[schema.GroupVersionResource][]string)

	for _, cp := range c.registry.Snapshot() {
		if cp == nil {
			continue
		}
		for _, gen := range cp.Generators {
			if gen == nil {
				continue
			}
			gvr := schema.GroupVersionResource{
				Group:    gen.Resource.Group,
				Version:  gen.Resource.Version,
				Resource: gen.Resource.Resource,
			}
			out[gvr] = append(out[gvr], gen.RequiredFields...)
		}
	}
	for gvr, fields := range out {
		slices.Sort(fields)
		out[gvr] = slices.Compact(fields)
	}
	return out
}

// reconcile brings the set of live informers into agreement with
// desiredGVRs(). It is idempotent and safe to call repeatedly. Heavy work
// (SSAR, cache sync) happens without the map lock held; the map is mutated
// under a brief write lock at the end.
func (c *ControlPlaneCollector) reconcile() {
	// Snapshot live set under read lock.
	c.mu.RLock()
	if c.ctx == nil {
		c.mu.RUnlock()
		return
	}
	present := maps.Clone(c.informers)
	ctx := c.ctx
	c.mu.RUnlock()

	desired := c.desiredGVRs()

	// Plan additions, rebuilds, and removals.
	var (
		toAdd     []schema.GroupVersionResource
		toRebuild = make([]schema.GroupVersionResource, 0, len(present))
		toRemove  []schema.GroupVersionResource
	)
	for gvr := range desired {
		inf, ok := present[gvr]
		if !ok {
			toAdd = append(toAdd, gvr)
			continue
		}
		// Denied entry: nothing to update.
		if inf.fields == nil {
			continue
		}
		// If the desired field set is a subset of what the cache
		// already holds, the existing cache contents are already
		// sufficient; just publish the new field set so future watch
		// events trim to it. Otherwise the cache is missing fields
		// that policies now reference — schedule a rebuild so a fresh
		// list-then-watch repopulates entries with the wider field
		// set, leaving the old informer serving until the new one is
		// synced (two-phase swap).
		if subset(desired[gvr], inf.fields.Load()) {
			inf.fields.Set(desired[gvr])
			continue
		}
		c.logger.V(1).Info("field set expanded; scheduling rebuild",
			"gvr", gvr.String(), "fields", desired[gvr])
		toRebuild = append(toRebuild, gvr)
	}
	for gvr := range present {
		if _, ok := desired[gvr]; !ok {
			toRemove = append(toRemove, gvr)
		}
	}

	// Apply removals: stop the goroutine (if any) and drop the entry.
	if len(toRemove) > 0 {
		c.mu.Lock()
		for _, gvr := range toRemove {
			if inf, ok := c.informers[gvr]; ok {
				if inf.stop != nil {
					inf.stop()
				}
				delete(c.informers, gvr)
				c.logger.V(1).Info("stopped informer", "gvr", gvr.String())
			}
		}
		c.mu.Unlock()
	}

	// Apply additions: preflight + start informer + wait for sync.
	for _, gvr := range toAdd {
		entry := c.startInformer(ctx, gvr, desired[gvr])
		if entry == nil {
			// Transient failure (sync timeout or apiserver flake).
			// The GVR stays desired; the next reconcile retries.
			continue
		}
		c.mu.Lock()
		c.informers[gvr] = entry
		c.mu.Unlock()
	}

	// Apply rebuilds: build the new informer alongside the old one and
	// swap once the new one is synced. The old informer keeps serving
	// Collect() until the swap.
	for _, gvr := range toRebuild {
		entry := c.startInformer(ctx, gvr, desired[gvr])
		if entry == nil {
			// Transient: leave the old informer in place.
			continue
		}
		if !entry.synced {
			// Denied/failed mid-rebuild: keep old serving with narrower fields.
			continue
		}

		c.mu.Lock()
		old := c.informers[gvr]
		c.informers[gvr] = entry
		c.mu.Unlock()
		if old != nil && old.stop != nil {
			old.stop()
		}
	}
}

// sharedRequiredFields publishes the active required-field set to an
// informer's cache transform. Reconcile narrows via Set; widening rebuilds the
// informer instead.
type sharedRequiredFields struct {
	p atomic.Pointer[[]string]
}

func (r *sharedRequiredFields) Set(fields []string) { r.p.Store(&fields) }

func (r *sharedRequiredFields) Load() []string {
	if fp := r.p.Load(); fp != nil {
		return *fp
	}
	return nil
}

// subset reports whether every element of a is present in b. Both slices
// must be sorted; desiredGVRs guarantees that.
func subset(a, b []string) bool {
	for _, x := range a {
		if _, found := slices.BinarySearch(b, x); !found {
			return false
		}
	}
	return true
}

// trimTransform prunes fields that are not required to evaluate
// the CEL program from the object before it gets cached. Existing cache entries
// are not re-trimmed when fields change; updates take effect on the next watch
// event.
func trimTransform(fields *sharedRequiredFields) cache.TransformFunc {
	return func(obj any) (any, error) {
		u, ok := obj.(*unstructured.Unstructured)
		if !ok {
			return obj, nil
		}
		policy.TrimObjectInPlace(u.Object, fields.Load())
		return u, nil
	}
}

// startInformer performs the preflight, launches a fresh informer for the
// given GVR, and waits for its cache to sync. It does NOT touch the
// collector's maps — installing the result is the caller's responsibility.
//
// Returns:
//   - a non-nil entry with synced=true on success.
//   - a non-nil entry with denied=true on permanent denial (SSAR, probe-list,
//     or sync-then-denied). fields and stop are nil; no goroutine is running.
//   - nil on transient failure (sync timeout, apiserver flake). Any informer
//     goroutine this call started has been stopped.
func (c *ControlPlaneCollector) startInformer(parent context.Context, gvr schema.GroupVersionResource, requiredFields []string) *gvrInformer {
	log := c.logger.WithValues("gvr", gvr.String())

	allowed, ssarErr := c.preflight(parent, gvr)
	if ssarErr != nil {
		log.Info("ssar preflight failed", "error", ssarErr.Error())
		controllermetrics.RBACDeniedTotal.WithLabelValues(c.clusterName, gvr.String()).Inc()
		return &gvrInformer{denied: true, lastErr: ssarErr}
	}
	if !allowed {
		log.Info("ssar preflight denied list/watch access")
		controllermetrics.RBACDeniedTotal.WithLabelValues(c.clusterName, gvr.String()).Inc()
		return &gvrInformer{denied: true, lastErr: errors.New("ssar: list or watch not allowed")}
	}

	// Fast pre-check: if the GVR doesn't exist on the cluster at all,
	// fail immediately instead of blocking cacheSyncTimeout seconds
	// waiting for the reflector to give up. probeList does a single
	// LIST with limit=1; on a kind cluster this returns 404 in <10ms
	// for non-existent resources.
	if probeErr := c.probeList(parent, gvr); isNotFoundOrForbidden(probeErr) {
		log.Info("probe list detected unavailable GVR; recording denial without waiting for sync")
		controllermetrics.RBACDeniedTotal.WithLabelValues(c.clusterName, gvr.String()).Inc()
		return &gvrInformer{denied: true, lastErr: probeErr}
	}

	factory := dynamicinformer.NewDynamicSharedInformerFactory(c.dynamicClient, defaultResyncPeriod)
	generic := factory.ForResource(gvr)
	informer := generic.Informer()

	runCtx, runCancel := context.WithCancel(c.ctx)
	fields := &sharedRequiredFields{}
	fields.Set(requiredFields)
	if err := informer.SetTransform(trimTransform(fields)); err != nil {
		// SetTransform only fails if Run was already called. We just
		// built the informer; that shouldn't be possible here.
		panic(fmt.Sprintf("collector: SetTransform on fresh informer: %v", err))
	}

	log.V(1).Info("starting informer goroutine")
	go informer.Run(runCtx.Done())

	// Wait for the cache to sync, bounded by cacheSyncTimeout. The informer
	// goroutine runs on runCtx (derived from c.ctx); this timeout only
	// gates this attempt, not the goroutine's lifetime.
	syncCtx, syncCancel := context.WithTimeout(parent, cacheSyncTimeout)
	defer syncCancel()
	log.V(1).Info("waiting for cache sync")
	synced := cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced)
	log.V(1).Info("cache sync done", "synced", synced)

	if !synced {
		err := syncCtx.Err()
		if err == nil {
			err = errors.New("cache: failed to sync")
		}
		// Classify the failure: if a direct list says NotFound/Forbidden,
		// the GVR isn't available to us (CRD missing or RBAC denied at
		// the apiserver level past SSAR — e.g. milo's per-project
		// authorization that admin tokens still bypass for SSAR). Treat
		// both as a "denial" so the policy reconciler surfaces it via
		// missingPermissions; otherwise the controller would silently
		// retry forever and the user would have no signal that the
		// generator is non-functional.
		probeErr := c.probeList(parent, gvr)
		denied := isNotFoundOrForbidden(probeErr)
		log.Info("informer cache sync failed", "error", err.Error(), "denied", denied)
		// Tear down the informer we just started — caller never sees it.
		runCancel()
		if denied {
			controllermetrics.RBACDeniedTotal.WithLabelValues(c.clusterName, gvr.String()).Inc()
			lastErr := probeErr
			if lastErr == nil {
				lastErr = err
			}
			return &gvrInformer{denied: true, lastErr: lastErr}
		}
		// Transient sync failure (e.g. apiserver flake).
		return nil
	}

	log.V(1).Info("informer started")
	return &gvrInformer{
		informer: generic,
		fields:   fields,
		stop:     runCancel,
		synced:   true,
	}
}

// probeList performs a small list request against the live API. It is used
// only to classify failure modes when WaitForCacheSync times out. The
// returned error is inspected by the caller.
func (c *ControlPlaneCollector) probeList(ctx context.Context, gvr schema.GroupVersionResource) error {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := c.dynamicClient.Resource(gvr).List(probeCtx, listOptionsOne())
	return err
}

// Collect returns a snapshot of every synced, non-denied informer. Entries
// that are not synced or denied are skipped. The returned slice is owned by
// the caller.
func (c *ControlPlaneCollector) Collect() []CollectedObjects {
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
func (c *ControlPlaneCollector) Stop(ctx context.Context) error {
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
func (c *ControlPlaneCollector) Status() ControlPlaneStatus {
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
		// the purpose of ControlPlaneUp.
		if !inf.synced && !inf.denied && inf.lastErr != nil {
			anyFatal = true
		}
		statuses[gvr] = s
	}

	return ControlPlaneStatus{
		ClusterName:    c.clusterName,
		ControlPlaneUp: anySynced && !anyFatal,
		GVRStatuses:    statuses,
	}
}

// ClusterName returns the cluster name this collector is bound to.
func (c *ControlPlaneCollector) ClusterName() string { return c.clusterName }

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

// isNotFoundOrForbidden classifies an API error as "denied" for the purpose
// of marking an informer as denied rather than retrying forever.
func isNotFoundOrForbidden(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsNotFound(err) || apierrors.IsForbidden(err) ||
		meta.IsNoMatchError(err) || apierrors.IsMethodNotSupported(err)
}
