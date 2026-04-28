// SPDX-License-Identifier: AGPL-3.0-only

package otel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"go.datum.net/resource-metrics/internal/collector"
	"go.datum.net/resource-metrics/internal/metrics"
	"go.datum.net/resource-metrics/internal/policy"
)

// Fallback values for the per-family callback CycleBudget. They bound how
// much work a single OTel collection callback can do before short-circuiting;
// without this a pathological policy over a large cluster starves the scrape
// goroutine. Callers that want finer control set the corresponding fields on
// Runtime directly (via NewRuntimeWithOptions in the future).
const (
	defaultCycleBudgetDeadline = 2 * time.Second
	defaultCycleBudgetMaxEvals = int64(1_000_000)
)

// meterName is the instrumentation scope name for every metric we emit.
// Prometheus-backed OTel consumers flatten scope into the metric name space,
// so stability of this string matters across releases.
const meterName = "go.datum.net/resource-metrics"

// projectUpMetricName is the name of the operator-internal gauge that
// reports whether a project control plane is reachable and its informers
// are synced.
const projectUpMetricName = "resource_metrics_project_up"

// projectAttrKey is the attribute key used on every emitted series to
// identify which project the series belongs to. The Milo provider keys
// clusters by project name, so this value is both the cluster identifier
// and the canonical project identifier.
const projectAttrKey = "milo.project.name"

// CollectorView is the read-only slice of ControlPlaneCollector behaviour the
// OTel runtime depends on. Exposed as an interface so tests can supply a
// fake without standing up a real dynamic informer tree.
//
// The real *collector.ControlPlaneCollector satisfies this interface directly.
type CollectorView interface {
	Status() collector.ControlPlaneStatus
	Collect() []collector.CollectedObjects
}

// CollectorSource yields the current set of engaged project collectors.
// The real *collector.ClusterManager satisfies this interface directly via
// its Collectors() method; tests inject fakes.
type CollectorSource interface {
	Collectors() []CollectorView
}

// clusterManagerAdapter wraps a *collector.ClusterManager to widen its
// concrete []*ControlPlaneCollector return into the []CollectorView form the
// runtime consumes. This avoids touching the collector package.
type clusterManagerAdapter struct {
	inner *collector.ClusterManager
}

// Collectors returns the engaged collectors as CollectorViews. The slice
// is freshly allocated by ClusterManager.Collectors; we rewrap in-place.
func (a *clusterManagerAdapter) Collectors() []CollectorView {
	src := a.inner.Collectors()
	out := make([]CollectorView, 0, len(src))
	for _, c := range src {
		out = append(out, c)
	}
	return out
}

// NewCollectorSource adapts a *ClusterManager into the CollectorSource
// interface expected by the OTel runtime. Exported so cmd/main.go can wire
// the real manager without the runtime having to import collector types in
// its exported signature.
func NewCollectorSource(m *collector.ClusterManager) CollectorSource {
	return &clusterManagerAdapter{inner: m}
}

// familyEntry holds every piece of per-family state the runtime needs to
// reconcile on Sync: the observable instrument itself, the registration
// returned by RegisterCallback, and the last-known description so we can
// detect description drift.
type familyEntry struct {
	gauge        metric.Float64ObservableGauge
	registration metric.Registration
	description  string
}

// Runtime ties the MeterProvider, the policy Registry, and the set of
// engaged ControlPlaneCollectors together. It owns the per-family instruments
// and their per-family OTel callbacks, and exposes a single Sync() entry
// point that the policy reconciler calls after registry mutations.
type Runtime struct {
	meter         metric.Meter
	registry      *policy.Registry
	manager       CollectorSource
	defaultPrefix string
	logger        logr.Logger

	// cycleBudgetDeadline and cycleBudgetMaxEvals bound the work done by
	// each per-family callback. Zero values select the package defaults
	// above. Exposed as unexported fields so tests can tune them without
	// widening the constructor.
	cycleBudgetDeadline time.Duration
	cycleBudgetMaxEvals int64

	// mu guards families and the projectUp registration. It is held for
	// the entire Sync() call so concurrent Syncs do not race on callback
	// registration, but it is never acquired by callbacks themselves —
	// callbacks read from the registry and manager directly, both of
	// which are independently safe for concurrent use.
	mu           sync.Mutex
	families     map[string]*familyEntry
	projectUp    metric.Float64ObservableGauge
	projectUpReg metric.Registration
}

// SetCycleBudget overrides the per-family-callback CycleBudget parameters.
// d <= 0 selects the package default deadline; maxEvals <= 0 selects the
// package default eval cap. Safe to call before any Sync(); not safe under
// concurrent Sync calls.
func (r *Runtime) SetCycleBudget(d time.Duration, maxEvals int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cycleBudgetDeadline = d
	r.cycleBudgetMaxEvals = maxEvals
}

// effectiveCycleBudget returns the deadline and eval cap in force for the
// next callback, falling back to the package defaults when unset.
func (r *Runtime) effectiveCycleBudget() (time.Duration, int64) {
	d := r.cycleBudgetDeadline
	if d <= 0 {
		d = defaultCycleBudgetDeadline
	}
	m := r.cycleBudgetMaxEvals
	if m <= 0 {
		m = defaultCycleBudgetMaxEvals
	}
	return d, m
}

// NewRuntime constructs a Runtime and wires the operator-internal
// resource_metrics_project_up gauge. Family gauges are created lazily on
// the first Sync() call; this matches the lifecycle where the policy
// reconciler populates the registry before triggering reconciliation.
//
// The manager argument can be any CollectorSource; callers wiring a
// production *ClusterManager should pass NewCollectorSource(mgr).
func NewRuntime(mp *sdkmetric.MeterProvider, registry *policy.Registry, manager CollectorSource, defaultPrefix string, logger logr.Logger) (*Runtime, error) {
	if mp == nil {
		return nil, errors.New("otel runtime: nil MeterProvider")
	}
	if registry == nil {
		return nil, errors.New("otel runtime: nil Registry")
	}
	if manager == nil {
		return nil, errors.New("otel runtime: nil CollectorSource")
	}

	meter := mp.Meter(meterName)

	r := &Runtime{
		meter:         meter,
		registry:      registry,
		manager:       manager,
		defaultPrefix: defaultPrefix,
		logger:        logger.WithName("otel-runtime"),
		families:      make(map[string]*familyEntry),
	}

	projectUp, err := meter.Float64ObservableGauge(
		projectUpMetricName,
		metric.WithDescription("1 when the project control plane is reachable and informers are synced, 0 otherwise."),
	)
	if err != nil {
		return nil, fmt.Errorf("otel runtime: create %s gauge: %w", projectUpMetricName, err)
	}
	reg, err := meter.RegisterCallback(r.observeProjectUp, projectUp)
	if err != nil {
		return nil, fmt.Errorf("otel runtime: register %s callback: %w", projectUpMetricName, err)
	}
	r.projectUp = projectUp
	r.projectUpReg = reg
	return r, nil
}

// desiredFamily is the tuple of (effective metric name, help text) we want
// to have an OTel instrument for.
type desiredFamily struct {
	name        string
	description string
}

// computeDesired walks the current registry snapshot and returns the
// deduplicated set of families we need instruments for. The second return
// is only used by tests; production callers discard it.
func (r *Runtime) computeDesired() map[string]desiredFamily {
	desired := make(map[string]desiredFamily)
	for _, cp := range r.registry.Snapshot() {
		if cp == nil {
			continue
		}
		prefix := metrics.EffectivePrefix(cp.MetricNamePrefix, r.defaultPrefix)
		for _, gen := range cp.Generators {
			if gen == nil {
				continue
			}
			for _, fam := range gen.Families {
				if fam == nil {
					continue
				}
				name := metrics.FamilyMetricName(prefix, fam.Name)
				if existing, ok := desired[name]; ok {
					// A later policy declared a family that flattens to
					// the same effective name. If the description
					// differs, keep the first; this matches the
					// documented precedence rule and avoids thrashing
					// the instrument identity.
					if existing.description != fam.Help {
						r.logger.Info("duplicate effective metric name with divergent help; keeping first",
							"metric", name,
							"kept_help", existing.description,
							"discarded_help", fam.Help,
							"policy", cp.Name.String(),
						)
					}
					continue
				}
				desired[name] = desiredFamily{name: name, description: fam.Help}
			}
		}
	}
	return desired
}

// Sync reconciles the set of live OTel instruments with the set of
// families desired by the current registry snapshot. It creates new
// instruments, drops instruments whose family disappeared, and re-creates
// instruments whose description changed.
//
// Sync is safe to call concurrently; concurrent calls serialise on r.mu.
func (r *Runtime) Sync() error {
	desired := r.computeDesired()

	r.mu.Lock()
	defer r.mu.Unlock()

	// 1. Drop families that no longer appear in the registry.
	for name, entry := range r.families {
		if _, ok := desired[name]; ok {
			continue
		}
		if entry.registration != nil {
			if err := entry.registration.Unregister(); err != nil {
				r.logger.V(1).Info("unregister stale family callback failed",
					"metric", name, "error", err.Error())
			}
		}
		delete(r.families, name)
		r.logger.V(1).Info("dropped stale metric family", "metric", name)
	}

	// 2. Add new families and re-create families whose help text drifted.
	var firstErr error
	for name, d := range desired {
		existing, ok := r.families[name]
		if ok && existing.description == d.description {
			continue
		}
		if ok {
			// Description changed. OTel SDKs treat instrument identity as
			// (name, unit, description, kind); re-creating here ensures
			// the exported metric's description matches the new policy.
			if existing.registration != nil {
				if err := existing.registration.Unregister(); err != nil {
					r.logger.V(1).Info("unregister drifted family callback failed",
						"metric", name, "error", err.Error())
				}
			}
			delete(r.families, name)
		}

		gauge, err := r.meter.Float64ObservableGauge(
			name,
			metric.WithDescription(d.description),
		)
		if err != nil {
			r.logger.Error(err, "create family gauge", "metric", name)
			if firstErr == nil {
				firstErr = fmt.Errorf("create family gauge %q: %w", name, err)
			}
			continue
		}

		// Capture name by value so the callback knows which family it is
		// computing for when the Reader invokes it.
		familyName := name
		cb := func(ctx context.Context, observer metric.Observer) error {
			r.observeFamily(ctx, observer, familyName, gauge)
			return nil
		}
		reg, err := r.meter.RegisterCallback(cb, gauge)
		if err != nil {
			r.logger.Error(err, "register family callback", "metric", name)
			if firstErr == nil {
				firstErr = fmt.Errorf("register family callback %q: %w", name, err)
			}
			continue
		}

		r.families[name] = &familyEntry{
			gauge:        gauge,
			registration: reg,
			description:  d.description,
		}
		r.logger.V(1).Info("registered metric family", "metric", name)
	}

	return firstErr
}

// observeProjectUp is the RegisterCallback body for the internal
// resource_metrics_project_up gauge. It walks every engaged collector and
// emits 1.0/0.0 for each.
func (r *Runtime) observeProjectUp(_ context.Context, observer metric.Observer) error {
	for _, c := range r.manager.Collectors() {
		status := c.Status()
		var v float64
		if status.ControlPlaneUp {
			v = 1.0
		}
		observer.ObserveFloat64(r.projectUp, v,
			metric.WithAttributes(attribute.String(projectAttrKey, strings.TrimPrefix(status.ClusterName, "/"))),
		)
	}
	return nil
}

// observeFamily is the per-family callback body. It evaluates only the
// compiled policies that contribute to the given family name and emits
// one observation per produced Measurement.
//
// The function never returns an error: OTel treats a returned error as a
// total callback failure, which would drop all series for the family. We
// prefer to log and continue so a single broken CEL expression does not
// take down an otherwise healthy metric.
func (r *Runtime) observeFamily(_ context.Context, observer metric.Observer, effectiveName string, gauge metric.Float64ObservableGauge) {
	policies := r.registry.Snapshot()
	collectors := r.manager.Collectors()

	// One budget per callback invocation, shared across every (project,
	// policy, generator) tuple contributing to this family. A pathological
	// combination that would otherwise wedge the scrape goroutine short-
	// circuits below with a single V(1) log and a counter bump.
	deadline, maxEvals := r.effectiveCycleBudget()
	budget := policy.NewCycleBudget(deadline, maxEvals)
	budgetLogged := false

	type cacheKey struct{ group, version, resource string }
	for _, c := range collectors {
		status := c.Status()
		// If the project CP is unreachable, suppress every series for
		// this collector. This is the documented "suppress on CP down"
		// rule: we never emit synthetic zeroes; we simply omit series
		// so downstream alerts can fire on the up/down gauge alone.
		if !status.ControlPlaneUp {
			continue
		}

		// Cache the per-GVR object slice so multiple families that
		// target the same GVR don't walk the informer store twice.
		collected := c.Collect()
		byGVR := make(map[cacheKey][]map[string]any, len(collected))
		for _, co := range collected {
			byGVR[cacheKey{co.GVR.Group, co.GVR.Version, co.GVR.Resource}] = co.Objects
		}

		for _, cp := range policies {
			if cp == nil {
				continue
			}
			prefix := metrics.EffectivePrefix(cp.MetricNamePrefix, r.defaultPrefix)
			for _, gen := range cp.Generators {
				if gen == nil {
					continue
				}
				// Cheap pre-filter: does this generator contribute to
				// the callback's family at all? If not, skip the whole
				// Build invocation.
				relevant := false
				for _, fam := range gen.Families {
					if fam == nil {
						continue
					}
					if metrics.FamilyMetricName(prefix, fam.Name) == effectiveName {
						relevant = true
						break
					}
				}
				if !relevant {
					continue
				}

				objs := byGVR[cacheKey{gen.Resource.Group, gen.Resource.Version, gen.Resource.Resource}]
				if len(objs) == 0 {
					continue
				}

				emit := func(familyName string, m metrics.Measurement) {
					// metrics.Build calls emit with the raw family name
					// from the spec; we must re-apply the prefix to
					// decide whether this Measurement belongs to the
					// family this callback is computing.
					effective := metrics.FamilyMetricName(prefix, familyName)
					if effective != effectiveName {
						return
					}
					observer.ObserveFloat64(gauge, m.Value,
						metric.WithAttributes(attrsFor(m, status.ClusterName)...),
					)
					metrics.SeriesEmittedTotal.WithLabelValues(effective).Inc()
				}

				report := func(policyName, generator string, _ error) {
					metrics.EvalErrorsTotal.WithLabelValues(policyName, generator).Inc()
				}

				if errs := metrics.Build(cp, gen, objs, emit, report, budget); len(errs) > 0 {
					for _, e := range errs {
						r.logger.V(2).Info("metric build error",
							"policy", e.Policy,
							"generator", e.Generator,
							"family", e.Family,
							"object", e.Object,
							"error", e.Err.Error(),
						)
						if errors.Is(e.Err, policy.ErrCycleBudgetExceeded) && !budgetLogged {
							r.logger.V(1).Info("cycle budget exhausted; dropping remaining series for this callback",
								"family", effectiveName,
							)
							metrics.CycleBudgetExhaustedTotal.WithLabelValues(effectiveName).Inc()
							budgetLogged = true
						}
					}
				}
				if budgetLogged {
					return
				}
			}
		}
	}
}

// attrsFor builds the OTel attribute set for one Measurement: the project
// identity plus every LabelPair from the measurement rendered as a string
// attribute.
//
// projectUID is deliberately not included: the multicluster-runtime
// cluster.Cluster interface does not expose the Milo project UID today
// (see runtime_test.go and the Phase 4 report for the deferred follow-up).
func attrsFor(m metrics.Measurement, projectName string) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 1+len(m.Labels))
	attrs = append(attrs, attribute.String(projectAttrKey, strings.TrimPrefix(projectName, "/")))
	for _, lp := range m.Labels {
		attrs = append(attrs, attribute.String(lp.Name, lp.Value))
	}
	return attrs
}

// Shutdown unregisters every callback the Runtime has registered. The
// MeterProvider shutdown itself is the caller's responsibility; this
// method exists so the process can unhook observers cleanly before the
// MP's export loop stops (so last observations are not mutated by a
// half-dismantled registry).
func (r *Runtime) Shutdown(_ context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var firstErr error
	if r.projectUpReg != nil {
		if err := r.projectUpReg.Unregister(); err != nil {
			firstErr = err
		}
		r.projectUpReg = nil
	}
	for name, entry := range r.families {
		if entry.registration != nil {
			if err := entry.registration.Unregister(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		delete(r.families, name)
	}
	return firstErr
}
