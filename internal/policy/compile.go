// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
)

// Compile walks the policy spec and compiles every value and label expression
// into a CompiledPolicy. Errors are collected per expression; compilation
// continues past failures so a single bad expression does not prevent the
// rest of the policy from going live. Callers decide how to surface the
// returned []CompileError — the registry upserts the CompiledPolicy either
// way.
func Compile(env *Env, policy *v1alpha1.ResourceMetricsPolicy) (*CompiledPolicy, []CompileError) {
	name := types.NamespacedName{Name: policy.Name, Namespace: policy.Namespace}
	cp := &CompiledPolicy{
		Name:             name,
		Generation:       policy.Generation,
		MetricNamePrefix: policy.Spec.MetricNamePrefix,
		Generators:       make([]*CompiledGenerator, 0, len(policy.Spec.Generators)),
	}
	var errs []CompileError

	for _, gen := range policy.Spec.Generators {
		cg := &CompiledGenerator{
			Name:     gen.Name,
			Resource: gen.Resource,
			Families: make([]*CompiledFamily, 0, len(gen.Families)),
		}
		// genExprTotal counts every compilable expression in the generator
		// (each metric's value + each label). genExprSuccesses counts those
		// that compiled. If the generator had expressions and every one of
		// them failed, we store a nil slot in cp.Generators instead of a
		// husk — matching the documented contract on CompiledGenerator and
		// sparing downstream callers from walking an empty tree that will
		// never emit anything. A generator with no expressions at all
		// (e.g. no families defined) stays non-nil because it still
		// contributes a GVR to the desired informer set.
		genExprTotal := 0
		genExprSuccesses := 0
		for _, fam := range gen.Families {
			cf := &CompiledFamily{
				Name:    fam.Name,
				Help:    fam.Help,
				Metrics: make([]*CompiledMetric, 0, len(fam.Metrics)),
			}
			for mi, m := range fam.Metrics {
				cm := &CompiledMetric{
					Labels: make([]*CompiledLabel, 0, len(m.Labels)),
				}
				hasValueExpr := m.Value != nil && *m.Value != ""
				if hasValueExpr {
					genExprTotal++
					prog, err := env.Compile(*m.Value)
					if err != nil {
						errs = append(errs, CompileError{
							Policy:    name,
							Generator: gen.Name,
							Family:    fam.Name,
							Kind:      "value",
							Index:     mi,
							Err:       err,
						})
					} else {
						cm.ValueProgram = prog
						genExprSuccesses++
					}
				}
				for li, lbl := range m.Labels {
					genExprTotal++
					prog, err := env.Compile(lbl.Value)
					if err != nil {
						errs = append(errs, CompileError{
							Policy:      name,
							Generator:   gen.Name,
							Family:      fam.Name,
							Kind:        "label",
							Index:       li,
							MetricIndex: mi,
							Name:        lbl.Name,
							Err:         err,
						})
						continue
					}
					cm.Labels = append(cm.Labels, &CompiledLabel{
						Name:         lbl.Name,
						ValueProgram: prog,
					})
					genExprSuccesses++
				}
				cf.Metrics = append(cf.Metrics, cm)
			}
			cg.Families = append(cg.Families, cf)
		}
		if genExprTotal > 0 && genExprSuccesses == 0 {
			// Every expression in the generator failed to compile. Preserve
			// positional alignment with spec.Generators so callers that
			// correlate by index still line up.
			cp.Generators = append(cp.Generators, nil)
			continue
		}
		cp.Generators = append(cp.Generators, cg)
	}

	return cp, errs
}
