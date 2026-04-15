// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"fmt"

	"github.com/google/cel-go/cel"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
)

// CompiledPolicy is the immutable, compiled form of a ResourceMetricsPolicy.
// Once returned by Compile, fields must not be mutated — the registry hands
// *CompiledPolicy pointers to readers without locking.
type CompiledPolicy struct {
	Name             types.NamespacedName
	Generation       int64
	MetricNamePrefix string
	Generators       []*CompiledGenerator
}

// CompiledGenerator is the compiled form of a GeneratorSpec.
// If compilation of every family in the generator fails, Generators[i] in the
// parent policy may be nil — check for that at the call site.
type CompiledGenerator struct {
	Name     string
	Resource v1alpha1.ResourceReference
	Families []*CompiledFamily
}

// CompiledFamily is the compiled form of a MetricFamilySpec.
type CompiledFamily struct {
	Name    string
	Help    string
	Metrics []*CompiledMetric
}

// CompiledMetric is the compiled form of a MetricSpec.
// A nil ValueProgram means the metric value is a constant 1.0.
type CompiledMetric struct {
	ValueProgram cel.Program
	Labels       []*CompiledLabel
}

// CompiledLabel is the compiled form of a LabelSpec.
type CompiledLabel struct {
	Name         string
	ValueProgram cel.Program
}

// CompileError describes a single CEL compilation failure in a policy.
// Kind is "value" or "label". Index identifies which metric within the family
// (for Kind=="value") or which label within the metric (for Kind=="label").
type CompileError struct {
	Policy    types.NamespacedName
	Generator string
	Family    string
	Kind      string
	// Index is the index of the offending metric within the family
	// (Kind=="value") or of the offending label within the metric
	// (Kind=="label"). For label errors it identifies the label position.
	Index int
	// MetricIndex is only populated for label errors, identifying which
	// metric within the family the broken label belongs to. It is zero for
	// value errors. A combined (metric,label) pair fully locates the issue.
	MetricIndex int
	// Name is the label name (only meaningful for label errors).
	Name string
	Err  error
}

// Error returns a human-readable description of the compile error.
func (e CompileError) Error() string {
	switch e.Kind {
	case "label":
		return fmt.Sprintf("policy %s: generator %q family %q metric[%d] label[%d] %q: %v",
			e.Policy, e.Generator, e.Family, e.MetricIndex, e.Index, e.Name, e.Err)
	case "value":
		return fmt.Sprintf("policy %s: generator %q family %q metric[%d] value: %v",
			e.Policy, e.Generator, e.Family, e.Index, e.Err)
	default:
		return fmt.Sprintf("policy %s: generator %q family %q: %v",
			e.Policy, e.Generator, e.Family, e.Err)
	}
}

// Unwrap returns the underlying cause.
func (e CompileError) Unwrap() error { return e.Err }
