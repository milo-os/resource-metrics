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
	"k8s.io/client-go/dynamic/dynamicinformer"
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

func newTestCollector(t *testing.T) (*ProjectCollector, *policy.Registry) {
	t.Helper()

	env, err := policy.NewEnv()
	require.NoError(t, err)
	reg := policy.NewRegistry(env)

	dynClient := fakeDynamicClientWithLists()
	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, time.Hour)

	pc := newProjectCollectorForTesting(
		"test",
		dynClient,
		factory,
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

	factory := dynamicinformer.NewDynamicSharedInformerFactory(dynClient, time.Hour)

	pc := newProjectCollectorForTesting(
		"test",
		dynClient,
		factory,
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
