// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"sync"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
)

// Registry holds the set of compiled ResourceMetricsPolicy objects currently
// in effect. It is safe for concurrent use. CompiledPolicy values it hands out
// are immutable — callers may iterate them without holding the lock.
type Registry struct {
	env      *Env
	mu       sync.RWMutex
	compiled map[types.NamespacedName]*CompiledPolicy
}

// NewRegistry returns an empty Registry. An Env must be supplied because the
// Upsert path re-compiles every expression on change.
func NewRegistry(env *Env) *Registry {
	return &Registry{
		env:      env,
		compiled: make(map[types.NamespacedName]*CompiledPolicy),
	}
}

// Upsert compiles the policy and stores the result. It stores partial results
// even if some expressions fail to compile so the caller can degrade
// gracefully instead of dropping an entire policy for one typo. The returned
// []CompileError identifies exactly which expressions failed.
func (r *Registry) Upsert(policy *v1alpha1.ResourceMetricsPolicy) (*CompiledPolicy, []CompileError) {
	cp, errs := Compile(r.env, policy)

	r.mu.Lock()
	r.compiled[cp.Name] = cp
	r.mu.Unlock()

	return cp, errs
}

// Delete removes the named policy from the registry. It is a no-op if the
// policy is not present.
func (r *Registry) Delete(name types.NamespacedName) {
	r.mu.Lock()
	delete(r.compiled, name)
	r.mu.Unlock()
}

// Snapshot returns the set of currently-compiled policies as a slice. The
// slice is a copy; the *CompiledPolicy pointers inside it are shared. This is
// safe because CompiledPolicy is immutable after Compile.
func (r *Registry) Snapshot() []*CompiledPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]*CompiledPolicy, 0, len(r.compiled))
	for _, cp := range r.compiled {
		out = append(out, cp)
	}
	return out
}

// Get returns the compiled form of the named policy, or nil if not present.
func (r *Registry) Get(name types.NamespacedName) *CompiledPolicy {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.compiled[name]
}
