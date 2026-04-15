// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/cluster"

	"go.datum.net/resource-metrics/internal/policy"
)

// ClusterManager owns the set of per-project ProjectCollectors. It is driven
// by multicluster-runtime Engage / Disengage events and by the policy
// reconciler (via WakeAll on registry changes).
type ClusterManager struct {
	mu         sync.RWMutex
	collectors map[string]*ProjectCollector
	registry   *policy.Registry
	logger     logr.Logger
}

// NewClusterManager constructs a ClusterManager ready to receive Engage
// callbacks. It does not start anything on its own.
func NewClusterManager(registry *policy.Registry, logger logr.Logger) *ClusterManager {
	return &ClusterManager{
		collectors: make(map[string]*ProjectCollector),
		registry:   registry,
		logger:     logger.WithName("collector-manager"),
	}
}

// Engage is called when a project control plane becomes ready. It creates a
// ProjectCollector, stores it, starts it, and wakes it once so the initial
// reconcile picks up the current registry snapshot.
//
// The signature matches multicluster.Aware.Engage so a thin adapter in
// cmd/main.go can forward provider engage events here.
func (m *ClusterManager) Engage(ctx context.Context, clusterName string, cl cluster.Cluster) error {
	m.mu.Lock()
	if existing, ok := m.collectors[clusterName]; ok {
		// Idempotent: if we already have one, do nothing. The provider may
		// re-engage the same cluster (e.g. on reconnect); we prefer to keep
		// the existing collector rather than tear it down.
		m.mu.Unlock()
		existing.Wake()
		return nil
	}
	m.mu.Unlock()

	pc, err := NewProjectCollector(cl, clusterName, m.registry, m.logger)
	if err != nil {
		return fmt.Errorf("collector manager: engage %q: %w", clusterName, err)
	}

	if err := pc.Start(ctx); err != nil {
		return fmt.Errorf("collector manager: start %q: %w", clusterName, err)
	}

	m.mu.Lock()
	// Race: another Engage may have completed while we were constructing.
	if existing, ok := m.collectors[clusterName]; ok {
		m.mu.Unlock()
		// Tear down the duplicate and keep the established one.
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pc.Stop(stopCtx)
		existing.Wake()
		return nil
	}
	m.collectors[clusterName] = pc
	m.mu.Unlock()

	pc.Wake()
	m.logger.Info("engaged cluster", "cluster", clusterName)
	return nil
}

// Disengage tears down the collector for the given cluster. It is safe to
// call for a cluster that was never engaged.
func (m *ClusterManager) Disengage(clusterName string) {
	m.mu.Lock()
	pc, ok := m.collectors[clusterName]
	if ok {
		delete(m.collectors, clusterName)
	}
	m.mu.Unlock()

	if !ok {
		return
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pc.Stop(stopCtx); err != nil {
		m.logger.Info("collector stop returned error", "cluster", clusterName, "error", err.Error())
	}
	m.logger.Info("disengaged cluster", "cluster", clusterName)
}

// WakeAll signals every engaged collector that the registry has changed.
// It is called by the policy reconciler after a successful Upsert/Delete.
func (m *ClusterManager) WakeAll() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, pc := range m.collectors {
		pc.Wake()
	}
}

// Collectors returns a snapshot slice of the currently engaged
// ProjectCollectors. Iteration is safe without additional locking.
func (m *ClusterManager) Collectors() []*ProjectCollector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*ProjectCollector, 0, len(m.collectors))
	for _, pc := range m.collectors {
		out = append(out, pc)
	}
	return out
}

// Get returns the collector for a cluster, or nil if not engaged.
func (m *ClusterManager) Get(clusterName string) *ProjectCollector {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.collectors[clusterName]
}
