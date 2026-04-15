// SPDX-License-Identifier: AGPL-3.0-only

package policy_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	v1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/policy"
)

func loadSamplePolicy(t *testing.T) *v1alpha1.ResourceMetricsPolicy {
	t.Helper()
	// Walk up to the repo root; tests run with cwd == package dir.
	cwd, err := os.Getwd()
	require.NoError(t, err)
	dir := cwd
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(dir, "config", "samples",
			"resourcemetrics_v1alpha1_resourcemetricspolicy.yaml")
		if _, err := os.Stat(candidate); err == nil {
			data, readErr := os.ReadFile(candidate)
			require.NoError(t, readErr)
			p := &v1alpha1.ResourceMetricsPolicy{}
			require.NoError(t, yaml.Unmarshal(data, p))
			return p
		}
		dir = filepath.Dir(dir)
	}
	t.Fatalf("could not locate sample policy from %s", cwd)
	return nil
}

func TestCompile_SamplePolicy(t *testing.T) {
	env := mustEnv(t)
	p := loadSamplePolicy(t)

	cp, errs := policy.Compile(env, p)
	require.Empty(t, errs, "expected no compile errors")
	require.NotNil(t, cp)
	require.Equal(t, "resourcemetricspolicy-sample", cp.Name.Name)
	require.Len(t, cp.Generators, 2)

	// workload-info
	g0 := cp.Generators[0]
	require.Equal(t, "workload-info", g0.Name)
	require.Len(t, g0.Families, 1)
	require.Equal(t, "workload_info", g0.Families[0].Name)
	require.Len(t, g0.Families[0].Metrics, 1)
	m0 := g0.Families[0].Metrics[0]
	require.Nil(t, m0.ValueProgram, "workload_info has no value; should default to 1.0")
	require.Len(t, m0.Labels, 3)

	// workload-ready-condition
	g1 := cp.Generators[1]
	require.Equal(t, "workload-ready-condition", g1.Name)
	require.Len(t, g1.Families, 1)
	require.Equal(t, "workload_status_condition", g1.Families[0].Name)
	require.Len(t, g1.Families[0].Metrics, 1)
	m1 := g1.Families[0].Metrics[0]
	require.NotNil(t, m1.ValueProgram, "ready-condition metric has an explicit value")
	require.Len(t, m1.Labels, 3)
}

func TestCompile_BadCELReportsLocation(t *testing.T) {
	env := mustEnv(t)
	bogus := "object.metadata.name && ??? ++ no"
	p := &v1alpha1.ResourceMetricsPolicy{}
	p.Name = "broken"
	p.Spec.Generators = []v1alpha1.GeneratorSpec{{
		Name: "gen-x",
		Resource: v1alpha1.ResourceReference{
			Group: "g", Version: "v", Resource: "rs",
		},
		Families: []v1alpha1.MetricFamilySpec{{
			Name: "fam-y",
			Type: "gauge",
			Metrics: []v1alpha1.MetricSpec{{
				Labels: []v1alpha1.LabelSpec{
					{Name: "ok", Value: "'ok'"},
					{Name: "lbl-z", Value: bogus},
				},
			}},
		}},
	}}

	cp, errs := policy.Compile(env, p)
	require.NotNil(t, cp)
	require.Len(t, errs, 1, "expected exactly one compile error")

	e := errs[0]
	require.Equal(t, "label", e.Kind)
	require.Equal(t, "gen-x", e.Generator)
	require.Equal(t, "fam-y", e.Family)
	require.Equal(t, "lbl-z", e.Name)
	require.Equal(t, 1, e.Index)       // second label
	require.Equal(t, 0, e.MetricIndex) // first metric
	require.Contains(t, e.Error(), "lbl-z")

	// Compiled policy still contains the good label.
	require.Len(t, cp.Generators, 1)
	require.Len(t, cp.Generators[0].Families, 1)
	require.Len(t, cp.Generators[0].Families[0].Metrics, 1)
	require.Len(t, cp.Generators[0].Families[0].Metrics[0].Labels, 1)
	require.Equal(t, "ok", cp.Generators[0].Families[0].Metrics[0].Labels[0].Name)
}

// TestCompile_PartialSuccessOneGeneratorFails exercises the contract that
// Compile keeps the surviving generator compiled even when a sibling generator
// fails outright: the slice length still equals the spec, with a nil slot for
// the failed generator and collected errors pointing only at the bad one.
func TestCompile_PartialSuccessOneGeneratorFails(t *testing.T) {
	env := mustEnv(t)

	validName := "compute.miloapis.com"
	p := &v1alpha1.ResourceMetricsPolicy{}
	p.Name = "partial"
	p.Namespace = ""
	p.Spec.Generators = []v1alpha1.GeneratorSpec{
		{
			Name: "good-gen",
			Resource: v1alpha1.ResourceReference{
				Group: validName, Version: "v1alpha1", Resource: "workloads",
			},
			Families: []v1alpha1.MetricFamilySpec{{
				Name: "good_family",
				Type: "gauge",
				Metrics: []v1alpha1.MetricSpec{{
					Labels: []v1alpha1.LabelSpec{{
						Name:  "name",
						Value: "object.metadata.name",
					}},
				}},
			}},
		},
		{
			// Every expression in this generator is bogus, so the
			// resulting CompiledGenerator slot should be nil.
			Name: "bad-gen",
			Resource: v1alpha1.ResourceReference{
				Group: validName, Version: "v1alpha1", Resource: "workloads",
			},
			Families: []v1alpha1.MetricFamilySpec{{
				Name: "bad_family",
				Type: "gauge",
				Metrics: []v1alpha1.MetricSpec{{
					Value: ptrString("*&^"),
					Labels: []v1alpha1.LabelSpec{{
						Name:  "broken",
						Value: "??? bad",
					}},
				}},
			}},
		},
	}

	cp, errs := policy.Compile(env, p)
	require.NotNil(t, cp)
	require.Len(t, cp.Generators, 2)

	// Surviving generator has a compiled label.
	require.NotNil(t, cp.Generators[0])
	require.Equal(t, "good-gen", cp.Generators[0].Name)
	require.Len(t, cp.Generators[0].Families, 1)
	require.Len(t, cp.Generators[0].Families[0].Metrics, 1)
	require.Len(t, cp.Generators[0].Families[0].Metrics[0].Labels, 1)
	require.Equal(t, "name", cp.Generators[0].Families[0].Metrics[0].Labels[0].Name)

	// Failed generator slot is nil.
	require.Nil(t, cp.Generators[1], "fully-failed generator should be nil")

	// All errors point at the bad generator only.
	require.NotEmpty(t, errs)
	for _, e := range errs {
		require.Equal(t, "bad-gen", e.Generator)
		require.Equal(t, "bad_family", e.Family)
		require.Equal(t, "partial", e.Policy.Name)
		require.NotNil(t, e.Err)
	}

	// We expect both the bogus value and the bogus label to surface.
	var sawValue, sawLabel bool
	for _, e := range errs {
		switch e.Kind {
		case "value":
			sawValue = true
			require.Equal(t, 0, e.Index, "value error indexes the metric")
		case "label":
			sawLabel = true
			require.Equal(t, "broken", e.Name)
			require.Equal(t, 0, e.MetricIndex)
			require.Equal(t, 0, e.Index)
		}
	}
	require.True(t, sawValue, "expected a value compile error")
	require.True(t, sawLabel, "expected a label compile error")

	// After registry upsert, the snapshot reflects the partial policy with
	// the surviving generator compiled.
	r := policy.NewRegistry(env)
	_, upsertErrs := r.Upsert(p)
	require.NotEmpty(t, upsertErrs)

	snap := r.Snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "partial", snap[0].Name.Name)
	require.Len(t, snap[0].Generators, 2)
	require.NotNil(t, snap[0].Generators[0])
	require.Nil(t, snap[0].Generators[1])
	require.Equal(t, "good-gen", snap[0].Generators[0].Name)
}

func ptrString(s string) *string { return &s }

func TestRegistry_UpsertSnapshotDelete(t *testing.T) {
	env := mustEnv(t)
	r := policy.NewRegistry(env)

	p := &v1alpha1.ResourceMetricsPolicy{}
	p.Name = "one"
	p.Spec.Generators = []v1alpha1.GeneratorSpec{{
		Name: "g",
		Resource: v1alpha1.ResourceReference{
			Group: "g", Version: "v", Resource: "rs",
		},
		Families: []v1alpha1.MetricFamilySpec{{
			Name: "f",
			Type: "gauge",
			Metrics: []v1alpha1.MetricSpec{{
				Labels: []v1alpha1.LabelSpec{{Name: "n", Value: "'x'"}},
			}},
		}},
	}}

	cp, errs := r.Upsert(p)
	require.Empty(t, errs)
	require.NotNil(t, cp)

	snap := r.Snapshot()
	require.Len(t, snap, 1)
	require.Equal(t, "one", snap[0].Name.Name)

	r.Delete(cp.Name)
	require.Empty(t, r.Snapshot())
}
