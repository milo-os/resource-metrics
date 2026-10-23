// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"

	"go.datum.net/resource-metrics/internal/controller"
)

type watchRecordingCluster struct {
	cluster.Cluster
	watched bool
}

func (c *watchRecordingCluster) GetCache() cache.Cache {
	c.watched = true
	return nil
}

func TestPolicyControllerDoesNotWatchProviderClusters(t *testing.T) {
	mgr, err := mcmanager.New(&rest.Config{Host: "https://127.0.0.1:1"}, &stubProvider{}, manager.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	require.NoError(t, err)
	require.NoError(t, (&controller.ResourceMetricsPolicyReconciler{}).SetupWithManager(mgr))

	project := &watchRecordingCluster{}
	require.NoError(t, mgr.Engage(context.Background(), "project-a", project))
	require.False(t, project.watched, "policy controller opened a watch on a provider cluster")
}
