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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"

	resourcemetricsv1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/collector"
	otelpkg "go.datum.net/resource-metrics/internal/otel"
	"go.datum.net/resource-metrics/internal/policy"
)

type emptyProvider struct{}

func (emptyProvider) Get(context.Context, multicluster.ClusterName) (cluster.Cluster, error) {
	return nil, nil
}

func (emptyProvider) IndexField(context.Context, client.Object, string, client.IndexerFunc) error {
	return nil
}

var _ = Describe("ResourceMetricsPolicy controller with a cluster provider", func() {
	It("reconciles policies on the local cluster", func() {
		env, err := policy.NewEnv()
		Expect(err).NotTo(HaveOccurred())
		registry := policy.NewRegistry(env)
		clusterMgr := collector.NewClusterManager(registry, logr.Discard())
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader()))
		otelRt, err := otelpkg.NewRuntime(mp, registry, otelpkg.NewCollectorSource(clusterMgr), "datum_", logr.Discard())
		Expect(err).NotTo(HaveOccurred())

		localMgr, err := mcmanager.New(cfg, emptyProvider{}, manager.Options{
			Scheme:                 k8sClient.Scheme(),
			Metrics:                metricsserver.Options{BindAddress: "0"},
			HealthProbeBindAddress: "0",
			Controller:             ctrlconfig.Controller{SkipNameValidation: ptr.To(true)},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect((&ResourceMetricsPolicyReconciler{
			Client:         localMgr.GetLocalManager().GetClient(),
			Scheme:         k8sClient.Scheme(),
			Env:            env,
			Registry:       registry,
			ClusterManager: clusterMgr,
			OTel:           otelRt,
		}).SetupWithManager(localMgr)).To(Succeed())

		mgrCtx, stop := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- localMgr.Start(mgrCtx) }()
		DeferCleanup(func() {
			stop()
			Eventually(done, 10*time.Second).Should(Receive(BeNil()))
			_ = otelRt.Shutdown(context.Background())
			_ = mp.Shutdown(context.Background())
		})

		value := "object.spec.replicas"
		pol := &resourcemetricsv1alpha1.ResourceMetricsPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "local-watch-policy"},
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
						}},
					}},
				}},
			},
		}
		Expect(k8sClient.Create(ctx, pol)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(context.Background(), pol) })

		Eventually(func(g Gomega) {
			fresh := &resourcemetricsv1alpha1.ResourceMetricsPolicy{}
			g.Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(pol), fresh)).To(Succeed())
			g.Expect(conditionStatus(fresh, resourcemetricsv1alpha1.ConditionReady)).To(Equal(metav1.ConditionTrue))
		}, 10*time.Second, 100*time.Millisecond).Should(Succeed())
	})
})
