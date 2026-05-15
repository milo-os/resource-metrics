// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	v1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/policy"
)

// testGVR is what our fake policies reference.
var testGVR = schema.GroupVersionResource{
	Group:    "example.test",
	Version:  "v1",
	Resource: "widgets",
}

var otherGVR = schema.GroupVersionResource{
	Group:    "example.test",
	Version:  "v1",
	Resource: "gadgets",
}

func newWidget(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("example.test/v1")
	u.SetKind("Widget")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func newGadget(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("example.test/v1")
	u.SetKind("Gadget")
	u.SetName(name)
	u.SetNamespace("default")
	return u
}

func fakeDynamicClientWithLists() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	// Register list kinds for both GVRs so the fake can answer List.
	gvrToListKind := map[schema.GroupVersionResource]string{
		testGVR:  "WidgetList",
		otherGVR: "GadgetList",
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		gvrToListKind,
		newWidget("w1"),
		newGadget("g1"),
	)
}

// makePolicyWithFields builds a policy whose compiled RequiredFields will
// contain exactly the supplied dot-path fields (one gauge metric per field).
func makePolicyWithFields(gvr schema.GroupVersionResource, fields ...string) *v1alpha1.ResourceMetricsPolicy {
	metrics := make([]v1alpha1.MetricSpec, 0, len(fields))
	for _, f := range fields {
		expr := "object." + f
		metrics = append(metrics, v1alpha1.MetricSpec{Value: &expr})
	}
	return &v1alpha1.ResourceMetricsPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "p1"},
		Spec: v1alpha1.ResourceMetricsPolicySpec{
			Generators: []v1alpha1.GeneratorSpec{
				{
					Name: "gen",
					Resource: v1alpha1.ResourceReference{
						Group:    gvr.Group,
						Version:  gvr.Version,
						Resource: gvr.Resource,
					},
					Families: []v1alpha1.MetricFamilySpec{
						{Name: "test", Metrics: metrics},
					},
				},
			},
		},
	}
}

func makePolicy(name string, gvrs ...schema.GroupVersionResource) *v1alpha1.ResourceMetricsPolicy {
	gens := make([]v1alpha1.GeneratorSpec, 0, len(gvrs))
	for i, gvr := range gvrs {
		gens = append(gens, v1alpha1.GeneratorSpec{
			Name: "gen" + string(rune('a'+i)),
			Resource: v1alpha1.ResourceReference{
				Group:    gvr.Group,
				Version:  gvr.Version,
				Resource: gvr.Resource,
			},
		})
	}
	return &v1alpha1.ResourceMetricsPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ResourceMetricsPolicySpec{
			Generators: gens,
		},
	}
}

func newTestCollector(t *testing.T) (*ControlPlaneCollector, *policy.Registry) {
	t.Helper()

	env, err := policy.NewEnv()
	require.NoError(t, err)
	reg := policy.NewRegistry(env)

	dynClient := fakeDynamicClientWithLists()

	pc := newControlPlaneCollectorForTesting(
		"test",
		dynClient,
		nil, // no authz client -> preflight returns allowed
		reg,
		testr.New(t),
	)
	return pc, reg
}

func TestReconcile_AddsInformerForDesiredGVR(t *testing.T) {
	pc, reg := newTestCollector(t)

	if err := pc.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		stopCtx, cancel := cleanupContext()
		defer cancel()
		_ = pc.Stop(stopCtx)
	})

	// Upsert a policy that references testGVR.
	_, errs := reg.Upsert(makePolicy("p1", testGVR))
	require.Empty(t, errs)

	pc.Wake()

	// Poll until reconcile has installed the informer and it is synced.
	requireCondition(t, func() bool {
		pc.mu.RLock()
		defer pc.mu.RUnlock()
		inf, ok := pc.informers[testGVR]
		return ok && inf.synced
	}, "expected informer for %s to be synced", testGVR)

	// The lister should see the seeded widget.
	collected := pc.Collect()
	require.Len(t, collected, 1)
	require.Equal(t, testGVR, collected[0].GVR)
	require.Len(t, collected[0].Objects, 1)
	require.Equal(t, "w1", collected[0].Objects[0]["metadata"].(map[string]any)["name"])
}

func TestReconcile_RemovesInformerWhenPolicyDeleted(t *testing.T) {
	pc, reg := newTestCollector(t)
	require.NoError(t, pc.Start(t.Context()))
	t.Cleanup(func() {
		stopCtx, cancel := cleanupContext()
		defer cancel()
		_ = pc.Stop(stopCtx)
	})

	cp, errs := reg.Upsert(makePolicy("p1", testGVR))
	require.Empty(t, errs)
	pc.Wake()

	requireCondition(t, func() bool {
		pc.mu.RLock()
		defer pc.mu.RUnlock()
		inf, ok := pc.informers[testGVR]
		return ok && inf.synced
	}, "informer sync")

	reg.Delete(cp.Name)
	pc.Wake()

	requireCondition(t, func() bool {
		pc.mu.RLock()
		defer pc.mu.RUnlock()
		_, ok := pc.informers[testGVR]
		return !ok
	}, "expected informer for %s to be removed", testGVR)
}

// TestReconcile_RemovesEntryAfterCacheSyncFailure verifies that when a
// dynamic informer never finishes cache sync (because list returns a
// permanent error), reconcile cancels the per-GVR context and does NOT
// leave a stale entry in the informers map. Without this behavior the
// goroutine would leak until collector Stop.
func TestReconcile_RemovesEntryAfterCacheSyncFailure(t *testing.T) {
	// Short-circuit the 30s production cache-sync timeout for this test.
	orig := cacheSyncTimeout
	cacheSyncTimeout = 250 * time.Millisecond
	t.Cleanup(func() { cacheSyncTimeout = orig })

	env, err := policy.NewEnv()
	require.NoError(t, err)
	reg := policy.NewRegistry(env)

	scheme := runtime.NewScheme()
	gvrToListKind := map[schema.GroupVersionResource]string{
		testGVR: "WidgetList",
	}
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
	// Permanent list failure — this prevents the informer's reflector from
	// ever marking its cache as synced, and also makes probeList return an
	// error so the failure is classified as a generic (non-denied) one.
	dynClient.PrependReactor("list", "widgets", func(action clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("synthetic permanent list failure")
	})

	pc := newControlPlaneCollectorForTesting(
		"test",
		dynClient,
		nil, // no authz -> preflight allowed
		reg,
		testr.New(t),
	)
	require.NoError(t, pc.Start(t.Context()))
	t.Cleanup(func() {
		stopCtx, cancel := cleanupContext()
		defer cancel()
		_ = pc.Stop(stopCtx)
	})

	_, errs := reg.Upsert(makePolicy("p1", testGVR))
	require.Empty(t, errs)
	pc.Wake()

	// The reconcile that processes this wake will block for
	// cacheSyncTimeout, then return. After that, the informers map must
	// NOT contain testGVR.
	requireCondition(t, func() bool {
		pc.mu.RLock()
		defer pc.mu.RUnlock()
		_, ok := pc.informers[testGVR]
		return !ok
	}, "expected informer entry for %s to be absent after cache-sync failure", testGVR)

	// And the entry must remain absent after a subsequent wake (the failure
	// does not cause a permanent record — backoff comes from desired-set
	// recomputation on each reconcile).
	pc.mu.RLock()
	_, present := pc.informers[testGVR]
	pc.mu.RUnlock()
	require.False(t, present, "informer entry should not reappear in the map")
}

func TestReconcile_DuplicateGVRIsNoop(t *testing.T) {
	pc, reg := newTestCollector(t)
	require.NoError(t, pc.Start(t.Context()))
	t.Cleanup(func() {
		stopCtx, cancel := cleanupContext()
		defer cancel()
		_ = pc.Stop(stopCtx)
	})

	// Two policies referencing the same GVR -> one informer.
	_, errs := reg.Upsert(makePolicy("p1", testGVR))
	require.Empty(t, errs)
	_, errs = reg.Upsert(makePolicy("p2", testGVR))
	require.Empty(t, errs)
	pc.Wake()

	requireCondition(t, func() bool {
		pc.mu.RLock()
		defer pc.mu.RUnlock()
		inf, ok := pc.informers[testGVR]
		return ok && inf.synced
	}, "informer sync")

	// Capture the pointer and wake again; it must not be replaced.
	pc.mu.RLock()
	first := pc.informers[testGVR]
	pc.mu.RUnlock()

	pc.Wake()
	time.Sleep(100 * time.Millisecond)

	pc.mu.RLock()
	second := pc.informers[testGVR]
	pc.mu.RUnlock()

	require.Same(t, first, second, "informer entry should not be replaced on duplicate reconcile")
	require.Len(t, pc.informers, 1)
}

// newWidgetCollector builds a collector seeded with a single Widget (w1)
// carrying spec.replicas and spec.image, plus a started collector. Returns
// the dynamic client so tests can drive further watch events.
func newWidgetCollector(t *testing.T) (*ControlPlaneCollector, *policy.Registry, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	env, err := policy.NewEnv()
	require.NoError(t, err)
	reg := policy.NewRegistry(env)

	scheme := runtime.NewScheme()
	dynClient := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		scheme,
		map[schema.GroupVersionResource]string{testGVR: "WidgetList"},
		&unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "example.test/v1",
				"kind":       "Widget",
				"metadata":   map[string]any{"name": "w1", "namespace": "default"},
				"spec":       map[string]any{"replicas": int64(3), "image": "nginx"},
			},
		},
	)
	pc := newControlPlaneCollectorForTesting("test", dynClient, nil, reg, testr.New(t))
	require.NoError(t, pc.Start(t.Context()))
	t.Cleanup(func() {
		ctx, cancel := cleanupContext()
		defer cancel()
		_ = pc.Stop(ctx)
	})
	return pc, reg, dynClient
}

// w1Spec returns w1's spec map from the collected output, or nil if absent.
func w1Spec(out []CollectedObjects) map[string]any {
	for _, co := range out {
		for _, obj := range co.Objects {
			if obj["metadata"].(map[string]any)["name"] != "w1" {
				continue
			}
			if spec, ok := obj["spec"].(map[string]any); ok {
				return spec
			}
		}
	}
	return nil
}

func TestReconcile_PolicyFieldUpdates(t *testing.T) {
	t.Run("widening rebuilds and repopulates the cache", func(t *testing.T) {
		pc, reg, _ := newWidgetCollector(t)

		// Narrow policy first; cache trims to spec.replicas + identity.
		_, errs := reg.Upsert(makePolicyWithFields(testGVR, "spec.replicas"))
		require.Empty(t, errs)
		pc.Wake()
		requireCondition(t, func() bool {
			pc.mu.RLock()
			defer pc.mu.RUnlock()
			inf, ok := pc.informers[testGVR]
			return ok && inf.synced
		}, "informer for %s must sync", testGVR)

		spec := w1Spec(pc.Collect())
		require.Contains(t, spec, "replicas")
		require.NotContains(t, spec, "image", "spec.image must be trimmed by the narrow policy")

		pc.mu.RLock()
		infBefore := pc.informers[testGVR]
		pc.mu.RUnlock()
		require.NotNil(t, infBefore)

		// Widen to include spec.image — the cached object no longer covers
		// the required fields, so reconcile must rebuild.
		_, errs = reg.Upsert(makePolicyWithFields(testGVR, "spec.replicas", "spec.image"))
		require.Empty(t, errs)
		pc.Wake()

		// Rebuild swaps in a fresh *gvrInformer.
		requireCondition(t, func() bool {
			pc.mu.RLock()
			defer pc.mu.RUnlock()
			inf, ok := pc.informers[testGVR]
			return ok && inf != infBefore
		}, "informer must be rebuilt after field-set expansion")

		// Fresh list repopulates w1 with spec.image, even though w1 itself
		// has not changed on the cluster.
		requireCondition(t, func() bool {
			spec := w1Spec(pc.Collect())
			return spec != nil && spec["image"] == "nginx"
		}, "w1 must regain spec.image after rebuild")
	})

	t.Run("narrowing takes the subset fast path and converges", func(t *testing.T) {
		pc, reg, dynClient := newWidgetCollector(t)

		// Wide policy first; cache carries spec.replicas + spec.image.
		_, errs := reg.Upsert(makePolicyWithFields(testGVR, "spec.replicas", "spec.image"))
		require.Empty(t, errs)
		pc.Wake()
		requireCondition(t, func() bool {
			pc.mu.RLock()
			defer pc.mu.RUnlock()
			inf, ok := pc.informers[testGVR]
			return ok && inf.synced
		}, "informer for %s must sync", testGVR)

		pc.mu.RLock()
		infBefore := pc.informers[testGVR]
		pc.mu.RUnlock()
		require.NotNil(t, infBefore)

		// Narrow to spec.replicas only. No rebuild
		_, errs = reg.Upsert(makePolicyWithFields(testGVR, "spec.replicas"))
		require.Empty(t, errs)
		pc.Wake()

		requireCondition(t, func() bool {
			loaded := infBefore.fields.Load()
			return len(loaded) == 1 && loaded[0] == "spec.replicas"
		}, "atomic field set must reflect the narrowed policy")

		pc.mu.RLock()
		infAfter := pc.informers[testGVR]
		pc.mu.RUnlock()
		require.Same(t, infBefore, infAfter, "narrowing must not rebuild the informer")

		// Trigger a watch event. The transform should now drop spec.image
		// from w1's cached entry, converging on the narrower field set.
		_, err := dynClient.Resource(testGVR).Namespace("default").Update(
			t.Context(),
			&unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "example.test/v1",
					"kind":       "Widget",
					"metadata":   map[string]any{"name": "w1", "namespace": "default"},
					"spec":       map[string]any{"replicas": int64(4), "image": "nginx"},
				},
			},
			metav1.UpdateOptions{},
		)
		require.NoError(t, err)

		requireCondition(t, func() bool {
			spec := w1Spec(pc.Collect())
			if spec == nil {
				return false
			}
			_, hasImage := spec["image"]
			return !hasImage
		}, "spec.image must be trimmed from w1's cache entry on the next watch event")
	})
}
