// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"testing"

	"github.com/go-logr/logr/testr"
	"github.com/stretchr/testify/require"

	"go.datum.net/resource-metrics/internal/policy"
)

// TestClusterManager_BasicLifecycle exercises construction, WakeAll on an
// empty map, and Disengage of an unknown cluster. A full Engage test needs a
// live cluster.Cluster — the project's envtest harness (Phase 8) covers that.
// Attempting to stub a cluster.Cluster here is more work than it saves: the
// only consumer of the fake would be the ClusterManager.Engage happy path,
// which is already exercised end-to-end by the reconcile_test.go via
// newProjectCollectorForTesting.
func TestClusterManager_BasicLifecycle(t *testing.T) {
	env, err := policy.NewEnv()
	require.NoError(t, err)
	registry := policy.NewRegistry(env)

	m := NewClusterManager(registry, testr.New(t))
	require.NotNil(t, m)
	require.Empty(t, m.Collectors())

	// WakeAll on empty map must be safe.
	m.WakeAll()

	// Disengage of an unknown cluster must be a no-op.
	m.Disengage("unknown")
	require.Empty(t, m.Collectors())

	// Get on an unknown cluster returns nil.
	require.Nil(t, m.Get("unknown"))
}
