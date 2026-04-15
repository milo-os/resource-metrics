// SPDX-License-Identifier: AGPL-3.0-only

package metrics_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/metrics"
	"go.datum.net/resource-metrics/internal/policy"
)

func buildSimplePolicy(t *testing.T) (*policy.CompiledPolicy, *policy.CompiledGenerator) {
	t.Helper()
	env, err := policy.NewEnv()
	require.NoError(t, err)

	p := &v1alpha1.ResourceMetricsPolicy{}
	p.Name = "p1"
	p.Spec.Generators = []v1alpha1.GeneratorSpec{{
		Name:     "workload-info",
		Resource: v1alpha1.ResourceReference{Group: "compute.miloapis.com", Version: "v1alpha1", Resource: "workloads"},
		Families: []v1alpha1.MetricFamilySpec{{
			Name: "workload_info",
			Type: "gauge",
			Metrics: []v1alpha1.MetricSpec{{
				Labels: []v1alpha1.LabelSpec{
					{Name: "name", Value: "object.metadata.name"},
					{Name: "namespace", Value: "object.metadata.namespace"},
				},
			}},
		}},
	}}

	cp, errs := policy.Compile(env, p)
	require.Empty(t, errs)
	require.Len(t, cp.Generators, 1)
	return cp, cp.Generators[0]
}

func TestBuild_EmitsPerObject(t *testing.T) {
	cp, gen := buildSimplePolicy(t)

	objects := []map[string]any{
		{"metadata": map[string]any{"name": "a", "namespace": "ns1"}},
		{"metadata": map[string]any{"name": "b", "namespace": "ns2"}},
	}

	type emit struct {
		family string
		m      metrics.Measurement
	}
	var emits []emit
	errs := metrics.Build(cp, gen, objects, func(family string, m metrics.Measurement) {
		emits = append(emits, emit{family: family, m: m})
	}, nil, nil)

	require.Empty(t, errs)
	require.Len(t, emits, 2)

	require.Equal(t, "workload_info", emits[0].family)
	require.Equal(t, 1.0, emits[0].m.Value)
	require.Equal(t, []metrics.LabelPair{
		{Name: "name", Value: "a"},
		{Name: "namespace", Value: "ns1"},
	}, emits[0].m.Labels)

	require.Equal(t, "workload_info", emits[1].family)
	require.Equal(t, 1.0, emits[1].m.Value)
	require.Equal(t, []metrics.LabelPair{
		{Name: "name", Value: "b"},
		{Name: "namespace", Value: "ns2"},
	}, emits[1].m.Labels)
}

func TestBuild_OneBadObjectSkipped(t *testing.T) {
	cp, gen := buildSimplePolicy(t)

	// The second object's namespace is a *map* rather than a string; when
	// object.metadata.namespace is evaluated the result is a map and our
	// label coercion will fail. The first object should still emit.
	// (Numeric namespaces actually convert to string fine via CEL, so we
	// use a clearly-unconvertible type here.)
	objects := []map[string]any{
		{"metadata": map[string]any{"name": "good", "namespace": "ns1"}},
		{"metadata": map[string]any{"name": "bad", "namespace": map[string]any{"nope": 1}}},
	}

	var emitted []metrics.Measurement
	errs := metrics.Build(cp, gen, objects, func(_ string, m metrics.Measurement) {
		emitted = append(emitted, m)
	}, nil, nil)

	require.Len(t, emitted, 1, "the good object should still emit")
	require.Equal(t, "good", emitted[0].Labels[0].Value)

	require.Len(t, errs, 1)
	require.Equal(t, "bad", errs[0].Object)
	require.Equal(t, "workload-info", errs[0].Generator)
	require.Equal(t, "workload_info", errs[0].Family)
}

func TestBuild_ValueErrorSkipsSeries(t *testing.T) {
	env, err := policy.NewEnv()
	require.NoError(t, err)

	valExpr := "object.status.value"
	p := &v1alpha1.ResourceMetricsPolicy{}
	p.Name = "with-value"
	p.Spec.Generators = []v1alpha1.GeneratorSpec{{
		Name:     "g",
		Resource: v1alpha1.ResourceReference{Group: "g", Version: "v", Resource: "r"},
		Families: []v1alpha1.MetricFamilySpec{{
			Name: "f",
			Type: "gauge",
			Metrics: []v1alpha1.MetricSpec{{
				Value: &valExpr,
				Labels: []v1alpha1.LabelSpec{
					{Name: "name", Value: "object.metadata.name"},
				},
			}},
		}},
	}}
	cp, cerrs := policy.Compile(env, p)
	require.Empty(t, cerrs)
	gen := cp.Generators[0]

	objects := []map[string]any{
		// Value is a map: not numeric, not bool, not string, not convertible.
		{"metadata": map[string]any{"name": "x"}, "status": map[string]any{"value": map[string]any{}}},
		// Value is a valid number.
		{"metadata": map[string]any{"name": "y"}, "status": map[string]any{"value": int64(7)}},
	}

	var emitted []metrics.Measurement
	errs := metrics.Build(cp, gen, objects, func(_ string, m metrics.Measurement) {
		emitted = append(emitted, m)
	}, nil, nil)
	require.Len(t, errs, 1)
	require.Equal(t, "x", errs[0].Object)
	require.Len(t, emitted, 1)
	require.Equal(t, 7.0, emitted[0].Value)
}
