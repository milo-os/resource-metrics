// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	resourcemetricsv1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/collector"
	otelpkg "go.datum.net/resource-metrics/internal/otel"
	"go.datum.net/resource-metrics/internal/policy"
)

// These tests drive ResourceMetricsPolicyReconciler.Reconcile directly
// against an envtest API server, bypassing the controller-runtime event
// loop so we can synchronously verify status patches without racing
// informers.

var _ = Describe("ResourceMetricsPolicyReconciler", func() {
	var (
		reconciler *ResourceMetricsPolicyReconciler
		registry   *policy.Registry
		clusterMgr *collector.ClusterManager
		mp         *sdkmetric.MeterProvider
	)

	BeforeEach(func() {
		env, err := policy.NewEnv()
		Expect(err).NotTo(HaveOccurred())
		registry = policy.NewRegistry(env)
		clusterMgr = collector.NewClusterManager(registry, logr.Discard())

		reader := sdkmetric.NewManualReader()
		mp = sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		otelRt, err := otelpkg.NewRuntime(
			mp,
			registry,
			otelpkg.NewCollectorSource(clusterMgr),
			"datum_",
			logr.Discard(),
		)
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(func() {
			_ = otelRt.Shutdown(context.Background())
			_ = mp.Shutdown(context.Background())
		})

		reconciler = &ResourceMetricsPolicyReconciler{
			Client:         k8sClient,
			Scheme:         k8sClient.Scheme(),
			Env:            env,
			Registry:       registry,
			ClusterManager: clusterMgr,
			OTel:           otelRt,
		}
	})

	makeValidPolicy := func(name string) *resourcemetricsv1alpha1.ResourceMetricsPolicy {
		value := "object.spec.replicas"
		return &resourcemetricsv1alpha1.ResourceMetricsPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: resourcemetricsv1alpha1.ResourceMetricsPolicySpec{
				MetricNamePrefix: "datum_",
				Generators: []resourcemetricsv1alpha1.GeneratorSpec{{
					Name: "workloads",
					Resource: resourcemetricsv1alpha1.ResourceReference{
						Group:    "compute.miloapis.com",
						Version:  "v1alpha1",
						Resource: "workloads",
					},
					Families: []resourcemetricsv1alpha1.MetricFamilySpec{{
						Name: "workload_replicas",
						Type: "gauge",
						Metrics: []resourcemetricsv1alpha1.MetricSpec{{
							Value: &value,
							Labels: []resourcemetricsv1alpha1.LabelSpec{
								{Name: "name", Value: "object.metadata.name"},
							},
						}},
					}},
				}},
			},
		}
	}

	It("reports Ready=True for a valid policy", func() {
		pol := makeValidPolicy("valid-policy")
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pol) })

		_, err := reconciler.Reconcile(ctx, mcreconcile.Request{
			Request: ctrlreconcile.Request{NamespacedName: types.NamespacedName{Name: pol.Name}},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			fresh := &resourcemetricsv1alpha1.ResourceMetricsPolicy{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pol), fresh)).To(Succeed())
			g.Expect(fresh.Status.ActiveGenerators).To(Equal(int32(1)))
			g.Expect(fresh.Status.CompilationFailures).To(Equal(int32(0)))
			// ObservedGeneration must track the spec generation so clients
			// relying on the standard status-lag check can detect when the
			// controller has caught up.
			g.Expect(fresh.Status.ObservedGeneration).To(Equal(fresh.Generation))
			g.Expect(conditionStatus(fresh, resourcemetricsv1alpha1.ConditionReady)).To(Equal(metav1.ConditionTrue))
			g.Expect(conditionStatus(fresh, resourcemetricsv1alpha1.ConditionInvalidCEL)).To(Equal(metav1.ConditionFalse))
			g.Expect(conditionStatus(fresh, resourcemetricsv1alpha1.ConditionDegraded)).To(Equal(metav1.ConditionFalse))
		}, 5*time.Second, 100*time.Millisecond).Should(Succeed())

		// Registry snapshot should contain the compiled policy.
		Expect(registry.Snapshot()).To(HaveLen(1))
	})

	It("reports InvalidCEL=True for a policy with a bad CEL expression", func() {
		// This is the "bogus CEL" case from the acceptance criteria. The
		// expression below references a CEL function that does not exist,
		// so Compile returns a parse/check error.
		bogus := "!!!not-a-valid-expression!!!"
		pol := &resourcemetricsv1alpha1.ResourceMetricsPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "bogus-policy"},
			Spec: resourcemetricsv1alpha1.ResourceMetricsPolicySpec{
				Generators: []resourcemetricsv1alpha1.GeneratorSpec{{
					Name: "workloads",
					Resource: resourcemetricsv1alpha1.ResourceReference{
						Group:    "compute.miloapis.com",
						Version:  "v1alpha1",
						Resource: "workloads",
					},
					Families: []resourcemetricsv1alpha1.MetricFamilySpec{{
						Name: "workload_broken",
						Type: "gauge",
						Metrics: []resourcemetricsv1alpha1.MetricSpec{{
							Value: &bogus,
						}},
					}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pol) })

		_, err := reconciler.Reconcile(ctx, mcreconcile.Request{
			Request: ctrlreconcile.Request{NamespacedName: types.NamespacedName{Name: pol.Name}},
		})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func(g Gomega) {
			fresh := &resourcemetricsv1alpha1.ResourceMetricsPolicy{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pol), fresh)).To(Succeed())
			g.Expect(fresh.Status.CompilationFailures).To(BeNumerically(">", 0))
			// ObservedGeneration must advance even on degraded outcomes: the
			// controller has acted on this generation, it just didn't like
			// what it found.
			g.Expect(fresh.Status.ObservedGeneration).To(Equal(fresh.Generation))
			g.Expect(conditionStatus(fresh, resourcemetricsv1alpha1.ConditionInvalidCEL)).To(Equal(metav1.ConditionTrue))
			g.Expect(conditionStatus(fresh, resourcemetricsv1alpha1.ConditionDegraded)).To(Equal(metav1.ConditionTrue))
			g.Expect(conditionStatus(fresh, resourcemetricsv1alpha1.ConditionReady)).To(Equal(metav1.ConditionFalse))
		}, 5*time.Second, 100*time.Millisecond).Should(Succeed())
	})

	It("evicts the policy from the registry on deletion", func() {
		pol := makeValidPolicy("soon-deleted")
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())

		_, err := reconciler.Reconcile(ctx, mcreconcile.Request{
			Request: ctrlreconcile.Request{NamespacedName: types.NamespacedName{Name: pol.Name}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(registry.Snapshot()).To(HaveLen(1))

		Expect(k8sClient.Delete(ctx, pol)).To(Succeed())
		// Reconcile after deletion: the Get call should 404 and trigger
		// Registry.Delete.
		_, err = reconciler.Reconcile(ctx, mcreconcile.Request{
			Request: ctrlreconcile.Request{NamespacedName: types.NamespacedName{Name: pol.Name}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(registry.Snapshot()).To(BeEmpty())
	})
})

// conditionStatus returns the Status of the named condition, or
// ConditionUnknown if the condition is not present.
func conditionStatus(pol *resourcemetricsv1alpha1.ResourceMetricsPolicy, condType string) metav1.ConditionStatus {
	for _, c := range pol.Status.Conditions {
		if c.Type == condType {
			return c.Status
		}
	}
	return metav1.ConditionUnknown
}
