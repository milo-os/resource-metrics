// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// stubManager records Engage calls and satisfies mcmanager.Manager.
type stubManager struct {
	mcmanager.Manager
	engaged []struct {
		name string
		cl   cluster.Cluster
	}
	engageErr error
}

func (s *stubManager) Engage(ctx context.Context, name string, cl cluster.Cluster) error {
	if s.engageErr != nil {
		return s.engageErr
	}
	s.engaged = append(s.engaged, struct {
		name string
		cl   cluster.Cluster
	}{name, cl})
	return nil
}

// stubProvider records Run calls and satisfies runnableProvider.
type stubProvider struct {
	multicluster.Provider
	runCalled bool
	runErr    error
}

func (s *stubProvider) Run(ctx context.Context, mgr mcmanager.Manager) error {
	s.runCalled = true
	return s.runErr
}

// stubCluster satisfies cluster.Cluster (nil implementation is fine for identity checks).
type stubCluster struct {
	cluster.Cluster
}

func TestRootClusterEngager_EngagesRootThenDelegates(t *testing.T) {
	rootCl := &stubCluster{}
	inner := &stubProvider{}
	mgr := &stubManager{}

	engager := &rootClusterEngager{
		runnableProvider: inner,
		rootCluster:      rootCl,
	}

	err := engager.Run(context.Background(), mgr)
	require.NoError(t, err)

	require.Len(t, mgr.engaged, 1, "expected exactly one Engage call")
	assert.Equal(t, "root", mgr.engaged[0].name)
	assert.Equal(t, rootCl, mgr.engaged[0].cl)
	assert.True(t, inner.runCalled, "inner provider Run should have been called")
}

func TestRootClusterEngager_EngageErrorStopsRun(t *testing.T) {
	rootCl := &stubCluster{}
	inner := &stubProvider{}
	engageErr := errors.New("engage failed")
	mgr := &stubManager{engageErr: engageErr}

	engager := &rootClusterEngager{
		runnableProvider: inner,
		rootCluster:      rootCl,
	}

	err := engager.Run(context.Background(), mgr)
	assert.ErrorIs(t, err, engageErr)
	assert.False(t, inner.runCalled, "inner provider Run should NOT be called when Engage fails")
}

func TestRootClusterEngager_DelegatesInnerError(t *testing.T) {
	rootCl := &stubCluster{}
	innerErr := errors.New("inner run failed")
	inner := &stubProvider{runErr: innerErr}
	mgr := &stubManager{}

	engager := &rootClusterEngager{
		runnableProvider: inner,
		rootCluster:      rootCl,
	}

	err := engager.Run(context.Background(), mgr)
	assert.ErrorIs(t, err, innerErr)
	assert.True(t, inner.runCalled)
}

func TestWrapProviderWithCollector_WrapsProvider(t *testing.T) {
	inner := &stubProvider{}
	result := wrapProviderWithCollector(inner, nil)
	_, ok := result.(*collectorProviderWrapper)
	assert.True(t, ok, "wrapProviderWithCollector should return a *collectorProviderWrapper")
}
