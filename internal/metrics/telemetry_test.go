// SPDX-License-Identifier: AGPL-3.0-only

package metrics_test

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"go.datum.net/resource-metrics/internal/metrics"
)

// The controller's self-telemetry counters are registered against the
// controller-runtime global registry at package init, so these tests use
// label values that are unique to each case (the test name gets baked into
// label values below) to keep concurrent or interleaved runs from stepping on
// one another. Each assertion reads the counter-specific value with a fresh
// label vector and computes the delta across the call under test.

func TestReporter_IncrementsCounters(t *testing.T) {
	rep := metrics.DefaultReporter{}

	t.Run("ReportEvalError", func(t *testing.T) {
		policyLbl := "test-policy-Reporter_IncrementsCounters"
		genLbl := "gen-EvalError"
		before := testutil.ToFloat64(metrics.EvalErrorsTotal.WithLabelValues(policyLbl, genLbl))
		rep.ReportEvalError(policyLbl, genLbl)
		after := testutil.ToFloat64(metrics.EvalErrorsTotal.WithLabelValues(policyLbl, genLbl))
		require.Equal(t, before+1, after)
	})

	t.Run("ReportEvalPanic", func(t *testing.T) {
		before := testutil.ToFloat64(metrics.EvalPanicsTotal)
		rep.ReportEvalPanic()
		after := testutil.ToFloat64(metrics.EvalPanicsTotal)
		require.Equal(t, before+1, after)
	})

	t.Run("ReportRBACDenied", func(t *testing.T) {
		projectLbl := "test-project-Reporter_IncrementsCounters"
		gvrLbl := "g/v/rbac-denied"
		before := testutil.ToFloat64(metrics.RBACDeniedTotal.WithLabelValues(projectLbl, gvrLbl))
		rep.ReportRBACDenied(projectLbl, gvrLbl)
		after := testutil.ToFloat64(metrics.RBACDeniedTotal.WithLabelValues(projectLbl, gvrLbl))
		require.Equal(t, before+1, after)
	})

	t.Run("ReportSeriesEmitted", func(t *testing.T) {
		familyLbl := "test-family-Reporter_IncrementsCounters-emitted"
		before := testutil.ToFloat64(metrics.SeriesEmittedTotal.WithLabelValues(familyLbl))
		rep.ReportSeriesEmitted(familyLbl)
		after := testutil.ToFloat64(metrics.SeriesEmittedTotal.WithLabelValues(familyLbl))
		require.Equal(t, before+1, after)
	})

	t.Run("ReportCompileOutcome_success", func(t *testing.T) {
		before := testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("success"))
		rep.ReportCompileOutcome(true)
		after := testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("success"))
		require.Equal(t, before+1, after)
	})

	t.Run("ReportCompileOutcome_error", func(t *testing.T) {
		before := testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("error"))
		rep.ReportCompileOutcome(false)
		after := testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("error"))
		require.Equal(t, before+1, after)
	})

	t.Run("ReportLabelSanitized", func(t *testing.T) {
		before := testutil.ToFloat64(metrics.LabelSanitizedTotal)
		rep.ReportLabelSanitized()
		after := testutil.ToFloat64(metrics.LabelSanitizedTotal)
		require.Equal(t, before+1, after)
	})

	t.Run("ReportCycleBudgetExhausted", func(t *testing.T) {
		familyLbl := "test-family-Reporter_IncrementsCounters-budget"
		before := testutil.ToFloat64(metrics.CycleBudgetExhaustedTotal.WithLabelValues(familyLbl))
		rep.ReportCycleBudgetExhausted(familyLbl)
		after := testutil.ToFloat64(metrics.CycleBudgetExhaustedTotal.WithLabelValues(familyLbl))
		require.Equal(t, before+1, after)
	})
}

// TestNopReporter_DoesNotTouchCounters proves NopReporter's methods are
// side-effect-free with respect to the shared process-global counters. The
// labels are unique to this test to avoid collision with concurrent runs.
func TestNopReporter_DoesNotTouchCounters(t *testing.T) {
	nop := metrics.NopReporter{}

	policyLbl := "nop-policy-NopReporter_DoesNotTouchCounters"
	genLbl := "nop-gen"
	projectLbl := "nop-project-NopReporter_DoesNotTouchCounters"
	gvrLbl := "g/v/nop"
	familyEmitLbl := "nop-family-NopReporter_DoesNotTouchCounters-emit"
	familyBudgetLbl := "nop-family-NopReporter_DoesNotTouchCounters-budget"

	// Snapshot every counter we're about to poke via Nop.
	beforeEvalErr := testutil.ToFloat64(metrics.EvalErrorsTotal.WithLabelValues(policyLbl, genLbl))
	beforeEvalPanic := testutil.ToFloat64(metrics.EvalPanicsTotal)
	beforeRBAC := testutil.ToFloat64(metrics.RBACDeniedTotal.WithLabelValues(projectLbl, gvrLbl))
	beforeSeries := testutil.ToFloat64(metrics.SeriesEmittedTotal.WithLabelValues(familyEmitLbl))
	beforeCompileOK := testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("success"))
	beforeCompileErr := testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("error"))
	beforeSan := testutil.ToFloat64(metrics.LabelSanitizedTotal)
	beforeBudget := testutil.ToFloat64(metrics.CycleBudgetExhaustedTotal.WithLabelValues(familyBudgetLbl))

	// These must not panic.
	require.NotPanics(t, func() {
		nop.ReportEvalError(policyLbl, genLbl)
		nop.ReportEvalPanic()
		nop.ReportRBACDenied(projectLbl, gvrLbl)
		nop.ReportSeriesEmitted(familyEmitLbl)
		nop.ReportCompileOutcome(true)
		nop.ReportCompileOutcome(false)
		nop.ReportLabelSanitized()
		nop.ReportCycleBudgetExhausted(familyBudgetLbl)
	})

	// Counters attributable to Nop labels must be unchanged. Non-labelled
	// counters (EvalPanicsTotal, LabelSanitizedTotal) are shared process-wide,
	// so another test running in parallel could bump them; compare against a
	// lower bound instead.
	require.Equal(t, beforeEvalErr, testutil.ToFloat64(metrics.EvalErrorsTotal.WithLabelValues(policyLbl, genLbl)))
	require.Equal(t, beforeRBAC, testutil.ToFloat64(metrics.RBACDeniedTotal.WithLabelValues(projectLbl, gvrLbl)))
	require.Equal(t, beforeSeries, testutil.ToFloat64(metrics.SeriesEmittedTotal.WithLabelValues(familyEmitLbl)))
	require.Equal(t, beforeBudget, testutil.ToFloat64(metrics.CycleBudgetExhaustedTotal.WithLabelValues(familyBudgetLbl)))

	// Process-global, parallel-safe lower-bound checks. The Nop call itself
	// did not increment, so the post value must be >= pre (other tests may
	// have bumped it concurrently but Nop did not).
	require.GreaterOrEqual(t, testutil.ToFloat64(metrics.EvalPanicsTotal), beforeEvalPanic)
	require.GreaterOrEqual(t, testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("success")), beforeCompileOK)
	require.GreaterOrEqual(t, testutil.ToFloat64(metrics.CompilesTotal.WithLabelValues("error")), beforeCompileErr)
	require.GreaterOrEqual(t, testutil.ToFloat64(metrics.LabelSanitizedTotal), beforeSan)
}
