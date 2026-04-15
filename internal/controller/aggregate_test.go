// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime/schema"

	resourcemetricsv1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/collector"
)

// aggregateMissingPermissions is the pure extraction of the MissingPermissions
// aggregation logic out of Reconcile. Covering it here as a go-test (rather
// than stubbing ClusterManager inside the envtest harness) keeps the envtest
// surface focused on the reconcile-level happy path while giving us precise
// coverage of the filter + dedupe + sort contract.
//
// TODO: an end-to-end envtest that simulates a denial via a fake
// ClusterManager would exercise the reconcile-status writeback path too, but
// the current ClusterManager/ProjectCollector concrete types aren't easy to
// stub without a larger refactor — see the package comment on collector.go.

func TestAggregateMissingPermissions_FiltersToReferencedGVRs(t *testing.T) {
	referenced := schema.GroupVersionResource{Group: "compute.miloapis.com", Version: "v1alpha1", Resource: "workloads"}
	unreferenced := schema.GroupVersionResource{Group: "networking.miloapis.com", Version: "v1alpha1", Resource: "networks"}

	gens := []resourcemetricsv1alpha1.GeneratorSpec{{
		Name: "only-workloads",
		Resource: resourcemetricsv1alpha1.ResourceReference{
			Group: referenced.Group, Version: referenced.Version, Resource: referenced.Resource,
		},
	}}

	statuses := []collector.ProjectStatus{{
		ClusterName: "proj-a",
		GVRStatuses: map[schema.GroupVersionResource]collector.GVRStatus{
			referenced:   {Denied: true},
			unreferenced: {Denied: true},
		},
	}}

	got := aggregateMissingPermissions(gens, statuses)
	require.Len(t, got, 1, "denied GVRs that no policy references should be filtered out")
	require.Equal(t, referenced.Group, got[0].Group)
	require.Equal(t, referenced.Version, got[0].Version)
	require.Equal(t, referenced.Resource, got[0].Resource)
}

func TestAggregateMissingPermissions_DedupesAcrossProjects(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "compute.miloapis.com", Version: "v1alpha1", Resource: "workloads"}

	gens := []resourcemetricsv1alpha1.GeneratorSpec{{
		Name:     "g",
		Resource: resourcemetricsv1alpha1.ResourceReference{Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource},
	}}

	statuses := []collector.ProjectStatus{
		{
			ClusterName: "proj-a",
			GVRStatuses: map[schema.GroupVersionResource]collector.GVRStatus{gvr: {Denied: true}},
		},
		{
			ClusterName: "proj-b",
			GVRStatuses: map[schema.GroupVersionResource]collector.GVRStatus{gvr: {Denied: true}},
		},
		{
			ClusterName: "proj-c",
			GVRStatuses: map[schema.GroupVersionResource]collector.GVRStatus{gvr: {Denied: true}},
		},
	}

	got := aggregateMissingPermissions(gens, statuses)
	require.Len(t, got, 1, "same GVR denied by 3 projects should appear once")
}

func TestAggregateMissingPermissions_SortsDeterministically(t *testing.T) {
	a := schema.GroupVersionResource{Group: "a.example.com", Version: "v1", Resource: "aaa"}
	b := schema.GroupVersionResource{Group: "a.example.com", Version: "v1", Resource: "bbb"}
	c := schema.GroupVersionResource{Group: "b.example.com", Version: "v1", Resource: "aaa"}
	d := schema.GroupVersionResource{Group: "a.example.com", Version: "v2", Resource: "aaa"}

	toRef := func(g schema.GroupVersionResource) resourcemetricsv1alpha1.ResourceReference {
		return resourcemetricsv1alpha1.ResourceReference{Group: g.Group, Version: g.Version, Resource: g.Resource}
	}
	gens := []resourcemetricsv1alpha1.GeneratorSpec{
		{Name: "ga", Resource: toRef(a)},
		{Name: "gb", Resource: toRef(b)},
		{Name: "gc", Resource: toRef(c)},
		{Name: "gd", Resource: toRef(d)},
	}

	statuses := []collector.ProjectStatus{{
		ClusterName: "proj",
		GVRStatuses: map[schema.GroupVersionResource]collector.GVRStatus{
			c: {Denied: true},
			a: {Denied: true},
			d: {Denied: true},
			b: {Denied: true},
		},
	}}

	got := aggregateMissingPermissions(gens, statuses)
	require.Len(t, got, 4)
	// Expected order: sort by group, version, resource.
	// a (v1, aaa), b (v1, bbb) -> by resource; then d (a.example.com, v2, aaa); then c (b.example.com, v1, aaa)
	require.Equal(t, "a.example.com", got[0].Group)
	require.Equal(t, "v1", got[0].Version)
	require.Equal(t, "aaa", got[0].Resource)

	require.Equal(t, "a.example.com", got[1].Group)
	require.Equal(t, "v1", got[1].Version)
	require.Equal(t, "bbb", got[1].Resource)

	require.Equal(t, "a.example.com", got[2].Group)
	require.Equal(t, "v2", got[2].Version)
	require.Equal(t, "aaa", got[2].Resource)

	require.Equal(t, "b.example.com", got[3].Group)
	require.Equal(t, "v1", got[3].Version)
	require.Equal(t, "aaa", got[3].Resource)
}

func TestAggregateMissingPermissions_IgnoresUndeniedStatuses(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "compute.miloapis.com", Version: "v1alpha1", Resource: "workloads"}

	gens := []resourcemetricsv1alpha1.GeneratorSpec{{
		Name:     "g",
		Resource: resourcemetricsv1alpha1.ResourceReference{Group: gvr.Group, Version: gvr.Version, Resource: gvr.Resource},
	}}

	statuses := []collector.ProjectStatus{{
		ClusterName: "proj-ok",
		GVRStatuses: map[schema.GroupVersionResource]collector.GVRStatus{
			gvr: {Synced: true, Denied: false},
		},
	}}

	got := aggregateMissingPermissions(gens, statuses)
	require.Empty(t, got, "a GVR that synced successfully must not appear in MissingPermissions")
}

func TestAggregateMissingPermissions_EmptyInputs(t *testing.T) {
	// No generators -> no output.
	require.Empty(t, aggregateMissingPermissions(nil, []collector.ProjectStatus{{
		ClusterName: "proj", GVRStatuses: map[schema.GroupVersionResource]collector.GVRStatus{
			{Group: "g", Version: "v", Resource: "r"}: {Denied: true},
		},
	}}))

	// No project statuses -> no output.
	require.Empty(t, aggregateMissingPermissions(
		[]resourcemetricsv1alpha1.GeneratorSpec{{Name: "g", Resource: resourcemetricsv1alpha1.ResourceReference{
			Group: "g", Version: "v", Resource: "r",
		}}},
		nil,
	))
}
