// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import (
	"errors"
	"fmt"

	"go.datum.net/resource-metrics/internal/policy"
)

// LabelPair is a single metric label.
type LabelPair struct {
	Name  string
	Value string
}

// Measurement is one metric series produced for one object.
type Measurement struct {
	FamilyName string
	Value      float64
	Labels     []LabelPair
}

// BuildError describes a single per-series evaluation failure. One BuildError
// corresponds to one skipped series — the rest of the build continues.
type BuildError struct {
	Policy    string
	Generator string
	Family    string
	// Object is a short identifier for the object that triggered the
	// failure (metadata.name or similar). Left to the caller to populate;
	// Build tries metadata.name then falls back to the empty string.
	Object string
	Err    error
}

func (e BuildError) Error() string {
	return fmt.Sprintf("policy %q generator %q family %q object %q: %v",
		e.Policy, e.Generator, e.Family, e.Object, e.Err)
}

func (e BuildError) Unwrap() error { return e.Err }

// Build evaluates every (family, metric, object) combination in the given
// generator and invokes emit once per successful measurement. A failure in any
// label or value expression for a given series is recorded as a BuildError and
// the series is skipped — sibling series continue to emit.
//
// emit is called synchronously from Build; it must be safe to call from the
// calling goroutine and should not block.
//
// report, if non-nil, is invoked once per BuildError so callers can increment
// controller-level counters without plumbing the errors list back up to
// themselves. The error is still included in the returned slice regardless
// of whether report is nil.
//
// budget, if non-nil, caps the total number of CEL evaluations across all
// label and value expressions in this Build call (and, if the same budget
// is shared across Build calls, across those too). When the budget is
// exhausted Build stops iterating and returns the accumulated errors; the
// caller is responsible for logging the budget-exhaustion signal. A nil
// budget disables enforcement, which is the behaviour existing non-OTel
// callers rely on.
func Build(
	compiled *policy.CompiledPolicy,
	generator *policy.CompiledGenerator,
	objects []map[string]any,
	emit func(family string, m Measurement),
	report func(policyName, generator string, err error),
	budget *policy.CycleBudget,
) []BuildError {
	if compiled == nil || generator == nil || emit == nil {
		return nil
	}
	var errs []BuildError

	recordErr := func(be BuildError) {
		errs = append(errs, be)
		if report != nil {
			report(be.Policy, be.Generator, be.Err)
		}
	}

	for _, obj := range objects {
		objName := objectName(obj)
		for _, family := range generator.Families {
			if family == nil {
				continue
			}
			for _, metric := range family.Metrics {
				if metric == nil {
					continue
				}

				if metric.ForEachProgram != nil {
					items, err := policy.EvalForEach(metric.ForEachProgram, obj, budget)
					if err != nil {
						recordErr(BuildError{
							Policy:    compiled.Name.String(),
							Generator: generator.Name,
							Family:    family.Name,
							Object:    objName,
							Err:       err,
						})
						if errors.Is(err, policy.ErrCycleBudgetExceeded) {
							return errs
						}
						continue
					}
					for _, item := range items {
						labels, labelErr := evalLabelsWithItem(obj, item, metric.Labels, budget)
						if labelErr != nil {
							recordErr(BuildError{
								Policy:    compiled.Name.String(),
								Generator: generator.Name,
								Family:    family.Name,
								Object:    objName,
								Err:       labelErr,
							})
							if errors.Is(labelErr, policy.ErrCycleBudgetExceeded) {
								return errs
							}
							continue
						}
						var (
							value float64
							verr  error
						)
						if metric.ValueProgram == nil {
							value = 1.0
						} else {
							value, verr = policy.EvalValueWithItem(metric.ValueProgram, obj, item, budget)
							if verr != nil {
								recordErr(BuildError{
									Policy:    compiled.Name.String(),
									Generator: generator.Name,
									Family:    family.Name,
									Object:    objName,
									Err:       verr,
								})
								if errors.Is(verr, policy.ErrCycleBudgetExceeded) {
									return errs
								}
								continue
							}
						}
						emit(family.Name, Measurement{
							FamilyName: family.Name,
							Value:      value,
							Labels:     labels,
						})
					}
					continue
				}

				labels, labelErr := evalLabels(obj, metric.Labels, budget)
				if labelErr != nil {
					recordErr(BuildError{
						Policy:    compiled.Name.String(),
						Generator: generator.Name,
						Family:    family.Name,
						Object:    objName,
						Err:       labelErr,
					})
					if errors.Is(labelErr, policy.ErrCycleBudgetExceeded) {
						return errs
					}
					continue
				}

				var (
					value float64
					err   error
				)
				if metric.ValueProgram == nil {
					value = 1.0
				} else {
					value, err = policy.EvalValue(metric.ValueProgram, obj, budget)
					if err != nil {
						recordErr(BuildError{
							Policy:    compiled.Name.String(),
							Generator: generator.Name,
							Family:    family.Name,
							Object:    objName,
							Err:       err,
						})
						if errors.Is(err, policy.ErrCycleBudgetExceeded) {
							return errs
						}
						continue
					}
				}

				emit(family.Name, Measurement{
					FamilyName: family.Name,
					Value:      value,
					Labels:     labels,
				})
			}
		}
	}
	return errs
}

func evalLabels(obj map[string]any, labels []*policy.CompiledLabel, budget *policy.CycleBudget) ([]LabelPair, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	out := make([]LabelPair, 0, len(labels))
	for _, lbl := range labels {
		if lbl == nil || lbl.ValueProgram == nil {
			continue
		}
		v, err := policy.EvalLabel(lbl.ValueProgram, obj, budget)
		if err != nil {
			return nil, fmt.Errorf("label %q: %w", lbl.Name, err)
		}
		out = append(out, LabelPair{Name: lbl.Name, Value: v})
	}
	return out, nil
}

func evalLabelsWithItem(obj map[string]any, item any, labels []*policy.CompiledLabel, budget *policy.CycleBudget) ([]LabelPair, error) {
	if len(labels) == 0 {
		return nil, nil
	}
	out := make([]LabelPair, 0, len(labels))
	for _, lbl := range labels {
		if lbl == nil || lbl.ValueProgram == nil {
			continue
		}
		v, err := policy.EvalLabelWithItem(lbl.ValueProgram, obj, item, budget)
		if err != nil {
			return nil, fmt.Errorf("label %q: %w", lbl.Name, err)
		}
		out = append(out, LabelPair{Name: lbl.Name, Value: v})
	}
	return out, nil
}

// objectName best-effort extracts metadata.name from a decoded object map.
func objectName(obj map[string]any) string {
	md, ok := obj["metadata"].(map[string]any)
	if !ok {
		return ""
	}
	if n, ok := md["name"].(string); ok {
		return n
	}
	return ""
}
