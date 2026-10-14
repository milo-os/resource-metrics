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

	// itemEnv extends the base env with the "item" variable used by forEach
	// metrics. We build it once and reuse it across all generators.
	itemEnv, itemEnvErr := env.NewItemEnv()

	for _, gen := range policy.Spec.Generators {
		gc := &generatorCompiler{
			policy:     name,
			generator:  gen.Name,
			env:        env,
			itemEnv:    itemEnv,
			itemEnvErr: itemEnvErr,
			cg: &CompiledGenerator{
				Name:           gen.Name,
				Resource:       gen.Resource,
				Families:       make([]*CompiledFamily, 0, len(gen.Families)),
				RequiredFields: make([]string, 0),
			},
		}
		for _, fam := range gen.Families {
			gc.compileFamily(fam)
		}
		errs = append(errs, gc.errs...)
		// If the generator had expressions and every one of them failed, we
		// store a nil slot in cp.Generators instead of a husk — matching the
		// documented contract on CompiledGenerator and sparing downstream
		// callers from walking an empty tree that will never emit anything. A
		// generator with no expressions at all (e.g. no families defined)
		// stays non-nil because it still contributes a GVR to the desired
		// informer set. The nil slot preserves positional alignment with
		// spec.Generators so callers that correlate by index still line up.
		if gc.total > 0 && gc.successes == 0 {
			cp.Generators = append(cp.Generators, nil)
			continue
		}
		cp.Generators = append(cp.Generators, gc.cg)
	}

	return cp, errs
}

// generatorCompiler compiles the families of one generator. total counts
// every compilable expression in the generator (each metric's forEach and
// value, and each label); successes counts those that compiled.
type generatorCompiler struct {
	policy     types.NamespacedName
	generator  string
	env        *Env
	itemEnv    *Env
	itemEnvErr error
	cg         *CompiledGenerator
	total      int
	successes  int
	errs       []CompileError
}

func (c *generatorCompiler) compileFamily(fam v1alpha1.MetricFamilySpec) {
	cf := &CompiledFamily{
		Name:    fam.Name,
		Help:    fam.Help,
		Metrics: make([]*CompiledMetric, 0, len(fam.Metrics)),
	}
	for mi, m := range fam.Metrics {
		if cm := c.compileMetric(fam.Name, mi, m); cm != nil {
			cf.Metrics = append(cf.Metrics, cm)
		}
	}
	c.cg.Families = append(c.cg.Families, cf)
}

// compileMetric returns the compiled metric, or nil when its forEach did not
// compile.
func (c *generatorCompiler) compileMetric(family string, mi int, m v1alpha1.MetricSpec) *CompiledMetric {
	cm := &CompiledMetric{}
	// The env used to compile value and label expressions: the item env when
	// forEach is set, the base env otherwise.
	exprEnv := c.env
	if isSet(m.ForEach) {
		prog := c.compileForEach(family, mi, *m.ForEach)
		if prog == nil {
			return nil
		}
		cm.ForEachProgram = prog
		exprEnv = c.itemEnv
	}
	if isSet(m.Value) {
		failed := CompileError{Kind: CompileErrorKindValue, Index: mi}
		if prog := c.compile(exprEnv, *m.Value, family, failed); prog != nil {
			cm.ValueProgram = prog
		}
	}
	cm.Labels = c.compileLabels(exprEnv, family, mi, m.Labels)
	return cm
}

func (c *generatorCompiler) compileForEach(family string, mi int, expr string) *Program {
	failed := CompileError{Kind: CompileErrorKindForEach, Index: mi}
	if c.itemEnvErr != nil {
		c.total++
		c.fail(family, failed, c.itemEnvErr)
		return nil
	}
	return c.compile(c.env, expr, family, failed)
}

func (c *generatorCompiler) compileLabels(env *Env, family string, mi int, labels []v1alpha1.LabelSpec) []*CompiledLabel {
	compiled := make([]*CompiledLabel, 0, len(labels))
	for li, lbl := range labels {
		failed := CompileError{Kind: CompileErrorKindLabel, Index: li, MetricIndex: mi, Name: lbl.Name}
		if prog := c.compile(env, lbl.Value, family, failed); prog != nil {
			compiled = append(compiled, &CompiledLabel{Name: lbl.Name, ValueProgram: prog})
		}
	}
	return compiled
}

// compile compiles expr in env and counts it. On failure it records failed,
// located in family, with the compile error, and returns nil.
func (c *generatorCompiler) compile(env *Env, expr, family string, failed CompileError) *Program {
	c.total++
	prog, err := env.Compile(expr)
	if err != nil {
		c.fail(family, failed, err)
		return nil
	}
	c.cg.RequiredFields = append(c.cg.RequiredFields, prog.RequiredFields...)
	c.successes++
	return prog
}

func (c *generatorCompiler) fail(family string, failed CompileError, err error) {
	failed.Policy, failed.Generator, failed.Family, failed.Err = c.policy, c.generator, family, err
	c.errs = append(c.errs, failed)
}

func isSet(s *string) bool { return s != nil && *s != "" }
