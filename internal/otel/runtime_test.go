// SPDX-License-Identifier: AGPL-3.0-only

package otel

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"k8s.io/apimachinery/pkg/runtime/schema"

	v1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/collector"
	"go.datum.net/resource-metrics/internal/policy"
)

// fakeCollector is a CollectorView that returns predetermined status and
// object lists. It lets the OTel runtime exercise its callback logic
// without a live dynamic informer tree.
type fakeCollector struct {
	status  collector.ProjectStatus
	objects []collector.CollectedObjects
}

func (f *fakeCollector) Status() collector.ProjectStatus       { return f.status }
func (f *fakeCollector) Collect() []collector.CollectedObjects { return f.objects }

// fakeSource is a CollectorSource backed by a static slice of fakes.
type fakeSource struct {
	collectors []CollectorView
}

func (f *fakeSource) Collectors() []CollectorView { return f.collectors }

// samplePolicy returns a compiled policy with two families targeting one
// GVR. The first family is an info-style family with labels only; the
// second evaluates a numeric CEL value expression.
func samplePolicy(t *testing.T, env *policy.Env) *policy.CompiledPolicy {
	t.Helper()
	value := "object.spec.replicas"
	pol := &v1alpha1.ResourceMetricsPolicy{}
	pol.Name = "sample"
	pol.Spec = v1alpha1.ResourceMetricsPolicySpec{
		MetricNamePrefix: "", // will use the controller default
		Generators: []v1alpha1.GeneratorSpec{{
			Name: "workloads",
			Resource: v1alpha1.ResourceReference{
				Group:    "compute.miloapis.com",
				Version:  "v1alpha1",
				Resource: "workloads",
			},
			Families: []v1alpha1.MetricFamilySpec{
				{
					Name: "workload_info",
					Help: "Exposes metadata about each Workload resource.",
					Type: "gauge",
					Metrics: []v1alpha1.MetricSpec{{
						Labels: []v1alpha1.LabelSpec{
							{Name: "name", Value: "object.metadata.name"},
						},
					}},
				},
				{
					Name: "workload_replicas",
					Help: "Replica count for each Workload.",
					Type: "gauge",
					Metrics: []v1alpha1.MetricSpec{{
						Value: &value,
						Labels: []v1alpha1.LabelSpec{
							{Name: "name", Value: "object.metadata.name"},
						},
					}},
				},
			},
		}},
	}

	cp, errs := policy.Compile(env, pol)
	require.Empty(t, errs, "sample policy compiled with errors: %v", errs)
	return cp
}

func newTestRuntime(t *testing.T, source CollectorSource) (*Runtime, *sdkmetric.ManualReader) {
	t.Helper()

	env, err := policy.NewEnv()
	require.NoError(t, err)
	reg := policy.NewRegistry(env)

	cp := samplePolicy(t, env)
	reg.Upsert(asPolicyObject(t, cp))

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	rt, err := NewRuntime(mp, reg, source, "datum_", testr.New(t))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = rt.Shutdown(context.Background())
		_ = mp.Shutdown(context.Background())
	})
	return rt, reader
}

// asPolicyObject reverses samplePolicy's compilation so that we can feed
// the registry's Upsert path (which takes a *v1alpha1.ResourceMetricsPolicy
// rather than a *CompiledPolicy). This keeps the registry state machine
// identical to production.
func asPolicyObject(t *testing.T, cp *policy.CompiledPolicy) *v1alpha1.ResourceMetricsPolicy {
	t.Helper()
	value := "object.spec.replicas"
	return &v1alpha1.ResourceMetricsPolicy{
		Spec: v1alpha1.ResourceMetricsPolicySpec{
			MetricNamePrefix: cp.MetricNamePrefix,
			Generators: []v1alpha1.GeneratorSpec{{
				Name: "workloads",
				Resource: v1alpha1.ResourceReference{
					Group:    "compute.miloapis.com",
					Version:  "v1alpha1",
					Resource: "workloads",
				},
				Families: []v1alpha1.MetricFamilySpec{
					{
						Name: "workload_info",
						Help: "Exposes metadata about each Workload resource.",
						Type: "gauge",
						Metrics: []v1alpha1.MetricSpec{{
							Labels: []v1alpha1.LabelSpec{
								{Name: "name", Value: "object.metadata.name"},
							},
						}},
					},
					{
						Name: "workload_replicas",
						Help: "Replica count for each Workload.",
						Type: "gauge",
						Metrics: []v1alpha1.MetricSpec{{
							Value: &value,
							Labels: []v1alpha1.LabelSpec{
								{Name: "name", Value: "object.metadata.name"},
							},
						}},
					},
				},
			}},
		},
	}
}

func workloadObject(name string, replicas int64) map[string]any {
	return map[string]any{
		"apiVersion": "compute.miloapis.com/v1alpha1",
		"kind":       "Workload",
		"metadata": map[string]any{
			"name":      name,
			"namespace": "default",
		},
		"spec": map[string]any{
			"replicas": replicas,
		},
	}
}

// TestRuntime_SyncRegistersFamilyGauges verifies that after Sync() the
// ManualReader collects gauge instruments for each expected family name
// together with the operator-internal project_up gauge.
func TestRuntime_SyncRegistersFamilyGauges(t *testing.T) {
	source := &fakeSource{
		collectors: []CollectorView{&fakeCollector{
			status: collector.ProjectStatus{
				ClusterName: "proj-alpha",
				ProjectUp:   true,
			},
			objects: []collector.CollectedObjects{{
				GVR: schema.GroupVersionResource{
					Group:    "compute.miloapis.com",
					Version:  "v1alpha1",
					Resource: "workloads",
				},
				Objects: []map[string]any{
					workloadObject("wl-a", 3),
					workloadObject("wl-b", 5),
				},
			}},
		}},
	}

	rt, reader := newTestRuntime(t, source)
	require.NoError(t, rt.Sync())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.NotEmpty(t, rm.ScopeMetrics, "manual reader returned no scope metrics")

	// Flatten metric names for assertions.
	seen := map[string]*metricdata.Metrics{}
	for i := range rm.ScopeMetrics {
		for j := range rm.ScopeMetrics[i].Metrics {
			m := &rm.ScopeMetrics[i].Metrics[j]
			seen[m.Name] = m
		}
	}
	require.Contains(t, seen, "datum_workload_info")
	require.Contains(t, seen, "datum_workload_replicas")
	require.Contains(t, seen, projectUpMetricName)

	// The replicas family must carry the numeric CEL value and the
	// datum.project attribute.
	replicas := seen["datum_workload_replicas"]
	gauge, ok := replicas.Data.(metricdata.Gauge[float64])
	require.True(t, ok, "workload_replicas data is not a float64 gauge: %T", replicas.Data)
	require.Len(t, gauge.DataPoints, 2)

	byName := map[string]float64{}
	for _, dp := range gauge.DataPoints {
		name, _ := dp.Attributes.Value(attribute.Key("name"))
		byName[name.AsString()] = dp.Value
		proj, present := dp.Attributes.Value(attribute.Key(projectAttrKey))
		require.True(t, present, "datum.project attribute missing from data point")
		require.Equal(t, "proj-alpha", proj.AsString())
	}
	require.Equal(t, 3.0, byName["wl-a"])
	require.Equal(t, 5.0, byName["wl-b"])

	// project_up must report 1.0 for the up collector.
	pu, ok := seen[projectUpMetricName].Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	require.Len(t, pu.DataPoints, 1)
	require.Equal(t, 1.0, pu.DataPoints[0].Value)
}

// TestRuntime_SuppressesSeriesWhenProjectDown verifies the documented
// suppression rule: if a project collector reports ProjectUp=false, no
// per-family series are emitted for that project (project_up still is).
func TestRuntime_SuppressesSeriesWhenProjectDown(t *testing.T) {
	source := &fakeSource{
		collectors: []CollectorView{&fakeCollector{
			status: collector.ProjectStatus{
				ClusterName: "proj-down",
				ProjectUp:   false,
			},
			objects: []collector.CollectedObjects{{
				GVR: schema.GroupVersionResource{
					Group:    "compute.miloapis.com",
					Version:  "v1alpha1",
					Resource: "workloads",
				},
				Objects: []map[string]any{workloadObject("wl-a", 3)},
			}},
		}},
	}

	rt, reader := newTestRuntime(t, source)
	require.NoError(t, rt.Sync())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))

	seen := map[string]*metricdata.Metrics{}
	for i := range rm.ScopeMetrics {
		for j := range rm.ScopeMetrics[i].Metrics {
			m := &rm.ScopeMetrics[i].Metrics[j]
			seen[m.Name] = m
		}
	}

	// project_up is still emitted (with value 0).
	pu, ok := seen[projectUpMetricName]
	require.True(t, ok, "project_up gauge should be emitted even when project is down")
	gauge, ok := pu.Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	require.Len(t, gauge.DataPoints, 1)
	require.Equal(t, 0.0, gauge.DataPoints[0].Value)

	// The family gauges are registered (they exist on the MeterProvider)
	// but they must have no data points.
	if m, ok := seen["datum_workload_info"]; ok {
		g, ok := m.Data.(metricdata.Gauge[float64])
		require.True(t, ok)
		require.Empty(t, g.DataPoints)
	}
	if m, ok := seen["datum_workload_replicas"]; ok {
		g, ok := m.Data.(metricdata.Gauge[float64])
		require.True(t, ok)
		require.Empty(t, g.DataPoints)
	}
}

// TestRuntime_SyncIsIdempotent verifies that repeated calls to Sync() with
// an unchanged registry snapshot do not grow the family set or churn
// callback registrations. Reviewer flagged this as a gap in Phase 4: a
// second Sync that recreated instruments would silently duplicate
// observations on the next collect.
func TestRuntime_SyncIsIdempotent(t *testing.T) {
	source := &fakeSource{
		collectors: []CollectorView{&fakeCollector{
			status: collector.ProjectStatus{ClusterName: "proj-alpha", ProjectUp: true},
			objects: []collector.CollectedObjects{{
				GVR: schema.GroupVersionResource{
					Group:    "compute.miloapis.com",
					Version:  "v1alpha1",
					Resource: "workloads",
				},
				Objects: []map[string]any{workloadObject("wl-a", 1)},
			}},
		}},
	}

	rt, reader := newTestRuntime(t, source)

	require.NoError(t, rt.Sync())
	firstCount := len(rt.families)
	require.NotZero(t, firstCount, "expected Sync to register at least one family")

	// Second sync with no registry change must not change the family set.
	require.NoError(t, rt.Sync())
	require.Equal(t, firstCount, len(rt.families),
		"family count must be stable across repeated Sync() calls")

	// A third sync, still unchanged, mustn't duplicate the data points
	// emitted per family — a classic bug when callbacks are re-registered
	// without unregistering the previous one.
	require.NoError(t, rt.Sync())
	require.Equal(t, firstCount, len(rt.families))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == projectUpMetricName {
				g, ok := m.Data.(metricdata.Gauge[float64])
				require.True(t, ok)
				require.Len(t, g.DataPoints, 1,
					"project_up should emit one point per collector, not %d",
					len(g.DataPoints))
			}
			if m.Name == "datum_workload_replicas" {
				g, ok := m.Data.(metricdata.Gauge[float64])
				require.True(t, ok)
				require.Len(t, g.DataPoints, 1,
					"workload_replicas must emit one point per object across syncs")
			}
		}
	}
}

// TestRuntime_ShutdownUnregistersCallbacks verifies that after Shutdown the
// per-family gauges stop emitting data points (because their callbacks
// are unregistered).
func TestRuntime_ShutdownUnregistersCallbacks(t *testing.T) {
	source := &fakeSource{
		collectors: []CollectorView{&fakeCollector{
			status: collector.ProjectStatus{ClusterName: "proj-alpha", ProjectUp: true},
			objects: []collector.CollectedObjects{{
				GVR: schema.GroupVersionResource{
					Group:    "compute.miloapis.com",
					Version:  "v1alpha1",
					Resource: "workloads",
				},
				Objects: []map[string]any{workloadObject("wl-a", 1)},
			}},
		}},
	}

	rt, reader := newTestRuntime(t, source)
	require.NoError(t, rt.Sync())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	require.NotEmpty(t, rm.ScopeMetrics)

	require.NoError(t, rt.Shutdown(context.Background()))

	var rm2 metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm2))
	for _, sm := range rm2.ScopeMetrics {
		for _, m := range sm.Metrics {
			if g, ok := m.Data.(metricdata.Gauge[float64]); ok {
				require.Empty(t, g.DataPoints,
					"metric %q still has data points after shutdown", m.Name)
			}
		}
	}
}

// countReplicaDataPoints collects once and returns the number of data
// points emitted for the workload_replicas family (budget coverage signal).
func countReplicaDataPoints(t *testing.T, reader *sdkmetric.ManualReader) int {
	t.Helper()
	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "datum_workload_replicas" {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[float64])
			require.True(t, ok)
			return len(g.DataPoints)
		}
	}
	return 0
}

// TestRuntime_CycleBudgetShortCircuits verifies the Issue #3 fix: a tiny
// per-callback budget causes the family callback to stop emitting early,
// yielding strictly fewer data points than an unbudgeted (default-budget)
// run on the same inputs.
//
// We configure a collector with many workloads, first run with the default
// (generous) budget to establish a baseline, then tighten the budget to a
// handful of evaluations and verify that the emission count drops.
func TestRuntime_CycleBudgetShortCircuits(t *testing.T) {
	const nObjects = 50
	objs := make([]map[string]any, 0, nObjects)
	for i := 0; i < nObjects; i++ {
		objs = append(objs, workloadObject(
			"wl-"+time.Duration(i).String(), int64(i)))
	}
	source := &fakeSource{
		collectors: []CollectorView{&fakeCollector{
			status: collector.ProjectStatus{
				ClusterName: "proj-alpha",
				ProjectUp:   true,
			},
			objects: []collector.CollectedObjects{{
				GVR: schema.GroupVersionResource{
					Group:    "compute.miloapis.com",
					Version:  "v1alpha1",
					Resource: "workloads",
				},
				Objects: objs,
			}},
		}},
	}

	// Baseline run: default budget is 1M evals / 2s which is vastly more
	// than the ~100 evaluations this test produces, so every workload
	// should emit for the workload_replicas family.
	rt, reader := newTestRuntime(t, source)
	require.NoError(t, rt.Sync())
	baseline := countReplicaDataPoints(t, reader)
	require.Equal(t, nObjects, baseline,
		"with the default budget every workload should emit")

	// Tight-budget run: allow at most 3 evaluations across the whole
	// family callback. The callback must short-circuit well before all 50
	// objects have been processed.
	rt2, reader2 := newTestRuntime(t, source)
	rt2.SetCycleBudget(2*time.Second, 3)
	require.NoError(t, rt2.Sync())
	tight := countReplicaDataPoints(t, reader2)
	require.Less(t, tight, baseline,
		"tiny budget (3 evals) should yield strictly fewer data points than baseline (%d)", baseline)
}
