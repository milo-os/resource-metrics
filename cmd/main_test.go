// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// stubAware records Engage calls and satisfies multicluster.Aware.
type stubAware struct {
	engaged []struct {
		name multicluster.ClusterName
		cl   cluster.Cluster
	}
	engageErr error
}

func (s *stubAware) Engage(ctx context.Context, name multicluster.ClusterName, cl cluster.Cluster) error {
	if s.engageErr != nil {
		return s.engageErr
	}
	s.engaged = append(s.engaged, struct {
		name multicluster.ClusterName
		cl   cluster.Cluster
	}{name, cl})
	return nil
}

// stubProvider records Start calls and satisfies runnableProvider.
type stubProvider struct {
	multicluster.Provider
	startCalled bool
	startErr    error
}

func (s *stubProvider) Start(ctx context.Context, aware multicluster.Aware) error {
	s.startCalled = true
	return s.startErr
}

// stubCluster satisfies cluster.Cluster (nil implementation is fine for identity checks).
type stubCluster struct {
	cluster.Cluster
}

func TestRootClusterEngager_EngagesRootThenDelegates(t *testing.T) {
	rootCl := &stubCluster{}
	inner := &stubProvider{}
	mgr := &stubAware{}

	engager := &rootClusterEngager{
		runnableProvider: inner,
		rootCluster:      rootCl,
	}

	err := engager.Start(context.Background(), mgr)
	require.NoError(t, err)

	require.Len(t, mgr.engaged, 1, "expected exactly one Engage call")
	assert.Equal(t, multicluster.ClusterName("root"), mgr.engaged[0].name)
	assert.Equal(t, rootCl, mgr.engaged[0].cl)
	assert.True(t, inner.startCalled, "inner provider Run should have been called")
}

func TestRootClusterEngager_EngageErrorStopsRun(t *testing.T) {
	rootCl := &stubCluster{}
	inner := &stubProvider{}
	engageErr := errors.New("engage failed")
	mgr := &stubAware{engageErr: engageErr}

	engager := &rootClusterEngager{
		runnableProvider: inner,
		rootCluster:      rootCl,
	}

	err := engager.Start(context.Background(), mgr)
	assert.ErrorIs(t, err, engageErr)
	assert.False(t, inner.startCalled, "inner provider Run should NOT be called when Engage fails")
}

func TestRootClusterEngager_DelegatesInnerError(t *testing.T) {
	rootCl := &stubCluster{}
	innerErr := errors.New("inner run failed")
	inner := &stubProvider{startErr: innerErr}
	mgr := &stubAware{}

	engager := &rootClusterEngager{
		runnableProvider: inner,
		rootCluster:      rootCl,
	}

	err := engager.Start(context.Background(), mgr)
	assert.ErrorIs(t, err, innerErr)
	assert.True(t, inner.startCalled)
}

func TestWrapProviderWithCollector_WrapsProvider(t *testing.T) {
	inner := &stubProvider{}
	result := wrapProviderWithCollector(inner, nil)
	_, ok := result.(*collectorProviderWrapper)
	assert.True(t, ok, "wrapProviderWithCollector should return a *collectorProviderWrapper")
}
