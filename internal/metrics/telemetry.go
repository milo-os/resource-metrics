// SPDX-License-Identifier: AGPL-3.0-only

// Package metrics' telemetry.go exposes the controller's own operational
// metrics (as opposed to the user-defined metrics built by builder.go). These
// counters describe controller health: compile failures, RBAC denials,
// evaluation panics, and so on. They are registered against the
// controller-runtime metrics registry so that they appear on the standard
// /metrics endpoint alongside the controller-runtime-provided metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Controller self-telemetry. These counters are intentionally labelled
// narrowly (policy, generator, project, gvr, family) so that operators can
// attribute problems to the offending policy or project CP. They never have
// per-object cardinality.
var (
	// CompilesTotal counts CEL compile attempts per policy reconcile. The
	// "outcome" label is "success" or "error".
	CompilesTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "resource_metrics_compiles_total",
			Help: "Total CEL expression compile attempts, partitioned by outcome.",
		},
		[]string{"outcome"},
	)

	// EvalErrorsTotal counts per-series CEL evaluation failures, bucketed by
	// policy and generator. Object-level failures still collapse here so the
	// label cardinality stays bounded by policy count.
	EvalErrorsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "resource_metrics_eval_errors_total",
			Help: "Total CEL evaluation failures during metric series production.",
		},
		[]string{"policy", "generator"},
	)

	// EvalPanicsTotal counts recovered panics inside CEL evaluation. This
	// should be near-zero in healthy operation; non-zero values point at a
	// cel-go bug or an input shape we didn't expect.
	EvalPanicsTotal = promauto.With(ctrlmetrics.Registry).NewCounter(
		prometheus.CounterOpts{
			Name: "resource_metrics_eval_panics_total",
			Help: "Total recovered panics inside CEL evaluation.",
		},
	)

	// RBACDeniedTotal counts SSAR preflight denials / list-access failures
	// per project and GVR. The gvr label is "group/version/resource" for
	// the canonical string form.
	RBACDeniedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "resource_metrics_rbac_denied_total",
			Help: "Total RBAC denials observed at informer start, by project and GVR.",
		},
		[]string{"project", "gvr"},
	)

	// InformerRestartsTotal is exposed but not yet incremented. The
	// informer lifecycle in this version does not distinguish a restart
	// from an initial start; wiring this counter is tracked for a future
	// pass.
	// TODO(phase7): increment when the informer supervisor is added.
	InformerRestartsTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "resource_metrics_informer_restarts_total",
			Help: "Total informer restarts, by project and GVR.",
		},
		[]string{"project", "gvr"},
	)

	// SeriesEmittedTotal counts Measurements produced by the OTel
	// per-family callbacks. The family label is the effective metric name.
	SeriesEmittedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "resource_metrics_series_emitted_total",
			Help: "Total metric series emitted per scrape, partitioned by family.",
		},
		[]string{"family"},
	)

	// OtelExportFailuresTotal counts push failures from the OTLP metric
	// exporter. It is available for wiring by the Runtime; until the SDK
	// surfaces a stable error hook we leave it at zero.
	// TODO(phase7): wire against the periodic reader's error handler.
	OtelExportFailuresTotal = promauto.With(ctrlmetrics.Registry).NewCounter(
		prometheus.CounterOpts{
			Name: "resource_metrics_otel_export_failures_total",
			Help: "Total OTLP metric export failures observed by the periodic reader.",
		},
	)

	// LabelSanitizedTotal counts label values that had control characters
	// replaced with spaces or were truncated to the configured max length.
	// A non-zero value indicates a policy is reading an untrusted field;
	// operators can use this to tighten policy label expressions.
	LabelSanitizedTotal = promauto.With(ctrlmetrics.Registry).NewCounter(
		prometheus.CounterOpts{
			Name: "resource_metrics_label_sanitized_total",
			Help: "Total label values that had control characters replaced or were truncated.",
		},
	)

	// CycleBudgetExhaustedTotal counts per-family callback invocations that
	// hit the cross-expression evaluation budget (either the eval count or
	// the wall-clock deadline). Non-zero values indicate a pathological
	// policy combined with a large collector object set; increase the
	// budget or tighten the policy.
	CycleBudgetExhaustedTotal = promauto.With(ctrlmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "resource_metrics_cycle_budget_exhausted_total",
			Help: "Total per-callback CEL evaluation budget exhaustions, by family.",
		},
		[]string{"family"},
	)
)

// Reporter is an injectable hook for production code paths that want to
// emit per-event counters without taking a hard dependency on the concrete
// Prometheus types. Tests supply a no-op to keep production counters from
// advancing during unit runs.
type Reporter interface {
	ReportEvalError(policy, generator string)
	ReportEvalPanic()
	ReportRBACDenied(project, gvr string)
	ReportSeriesEmitted(family string)
	ReportCompileOutcome(success bool)
	ReportLabelSanitized()
	ReportCycleBudgetExhausted(family string)
}

// DefaultReporter updates the package-level counters defined above. It is
// the implementation wired into main.
type DefaultReporter struct{}

func (DefaultReporter) ReportEvalError(policy, generator string) {
	EvalErrorsTotal.WithLabelValues(policy, generator).Inc()
}

func (DefaultReporter) ReportEvalPanic() { EvalPanicsTotal.Inc() }

func (DefaultReporter) ReportRBACDenied(project, gvr string) {
	RBACDeniedTotal.WithLabelValues(project, gvr).Inc()
}

func (DefaultReporter) ReportSeriesEmitted(family string) {
	SeriesEmittedTotal.WithLabelValues(family).Inc()
}

func (DefaultReporter) ReportCompileOutcome(success bool) {
	if success {
		CompilesTotal.WithLabelValues("success").Inc()
	} else {
		CompilesTotal.WithLabelValues("error").Inc()
	}
}

func (DefaultReporter) ReportLabelSanitized() { LabelSanitizedTotal.Inc() }

func (DefaultReporter) ReportCycleBudgetExhausted(family string) {
	CycleBudgetExhaustedTotal.WithLabelValues(family).Inc()
}

// NopReporter discards every report call. Tests that exercise code paths
// that report counters should use NopReporter so they do not spuriously
// advance the production registry.
type NopReporter struct{}

func (NopReporter) ReportEvalError(string, string)    {}
func (NopReporter) ReportEvalPanic()                  {}
func (NopReporter) ReportRBACDenied(string, string)   {}
func (NopReporter) ReportSeriesEmitted(string)        {}
func (NopReporter) ReportCompileOutcome(bool)         {}
func (NopReporter) ReportLabelSanitized()             {}
func (NopReporter) ReportCycleBudgetExhausted(string) {}
