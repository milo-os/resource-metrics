// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestResourceMetricsOperator_String_RedactsHeaders verifies that rendering
// a ResourceMetricsOperator via String() redacts sensitive OTLP header
// values (e.g. bearer tokens) while preserving header keys so operators can
// still see which headers are configured.
func TestResourceMetricsOperator_String_RedactsHeaders(t *testing.T) {
	t.Parallel()

	const bearerValue = "Bearer abc"
	const tenantValue = "foo"

	cfg := &ResourceMetricsOperator{
		Otel: OtelConfig{
			Endpoint: "otel-collector-collector.otel-collector-system:4317",
			Headers: map[string]string{
				"Authorization": bearerValue,
				"X-Tenant":      tenantValue,
			},
			CollectionInterval:  metav1.Duration{Duration: 30 * time.Second},
			DefaultMetricPrefix: "datum_",
		},
	}

	got := cfg.String()

	// Header keys must be preserved so operators can see which headers are
	// configured.
	for _, key := range []string{"Authorization", "X-Tenant"} {
		if !strings.Contains(got, key) {
			t.Errorf("String() output missing header key %q; got: %s", key, got)
		}
	}

	// Header values (including any token) must not leak into the rendering.
	for _, secret := range []string{bearerValue, "abc", tenantValue} {
		if strings.Contains(got, secret) {
			t.Errorf("String() output leaked header value %q; got: %s", secret, got)
		}
	}

	if !strings.Contains(got, "<redacted>") {
		t.Errorf("String() output missing redaction marker; got: %s", got)
	}

	// The non-sensitive fields should still surface so the log is useful.
	if !strings.Contains(got, "otel-collector-collector.otel-collector-system:4317") {
		t.Errorf("String() output missing endpoint; got: %s", got)
	}
}

// TestOtelConfig_String_RedactsHeaders exercises the OtelConfig String()
// method directly to make sure redaction happens at the type level and not
// only when wrapped by ResourceMetricsOperator.
func TestOtelConfig_String_RedactsHeaders(t *testing.T) {
	t.Parallel()

	cfg := OtelConfig{
		Endpoint: "example:4317",
		Headers: map[string]string{
			"Authorization": "Bearer super-secret-token",
		},
	}

	got := cfg.String()

	if strings.Contains(got, "super-secret-token") {
		t.Errorf("OtelConfig.String() leaked token; got: %s", got)
	}
	if !strings.Contains(got, "Authorization") {
		t.Errorf("OtelConfig.String() missing header key; got: %s", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Errorf("OtelConfig.String() missing redaction marker; got: %s", got)
	}
}

// TestOtelConfig_String_EmptyHeaders ensures the empty-map path does not
// panic and renders a stable representation.
func TestOtelConfig_String_EmptyHeaders(t *testing.T) {
	t.Parallel()

	cfg := OtelConfig{Endpoint: "example:4317"}

	got := cfg.String()

	if !strings.Contains(got, "Headers:map[]") {
		t.Errorf("OtelConfig.String() empty headers rendering unexpected; got: %s", got)
	}
}

// TestResourceMetricsOperator_MarshalLog_ReturnsRedactedString verifies the
// logr-style MarshalLog hook yields the same redacted string so structured
// loggers that honor the convention do not leak secrets.
func TestResourceMetricsOperator_MarshalLog_ReturnsRedactedString(t *testing.T) {
	t.Parallel()

	cfg := &ResourceMetricsOperator{
		Otel: OtelConfig{
			Headers: map[string]string{"Authorization": "Bearer abc"},
		},
	}

	v := cfg.MarshalLog()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("MarshalLog() returned %T, want string", v)
	}
	if strings.Contains(s, "abc") {
		t.Errorf("MarshalLog() leaked token; got: %s", s)
	}
	if !strings.Contains(s, "<redacted>") {
		t.Errorf("MarshalLog() missing redaction marker; got: %s", s)
	}
}

// TestResourceMetricsOperator_String_NilReceiver guards against a nil
// receiver panicking — defensive, since logging code often runs on partial
// state.
func TestResourceMetricsOperator_String_NilReceiver(t *testing.T) {
	t.Parallel()

	var cfg *ResourceMetricsOperator
	if got := cfg.String(); got != "<nil>" {
		t.Errorf("(*ResourceMetricsOperator)(nil).String() = %q, want %q", got, "<nil>")
	}
}
