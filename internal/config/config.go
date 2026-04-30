// SPDX-License-Identifier: AGPL-3.0-only

package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"

	multicluster "go.miloapis.com/milo/pkg/multicluster-runtime"
)

// redactedHeaderValue is the placeholder substituted for every Headers map
// value when a config is rendered for logging. Bearer tokens and similar
// credentials commonly flow through OTLP headers, so values are considered
// secret while keys remain visible so operators can see which headers are
// configured.
const redactedHeaderValue = "<redacted>"

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
// +k8s:defaulter-gen=true

// ResourceMetricsOperator holds configuration for the resource-metrics operator.
type ResourceMetricsOperator struct {
	metav1.TypeMeta

	// Discovery configures how project control planes are discovered.
	Discovery DiscoveryConfig `json:"discovery"`

	// Otel configures the OpenTelemetry export pipeline used to emit the
	// resource metrics produced by the registered ResourceMetricsPolicy
	// objects.
	Otel OtelConfig `json:"otel"`
}

// +k8s:deepcopy-gen=true

// OtelConfig configures the OpenTelemetry / OTLP export pipeline.
type OtelConfig struct {
	// Endpoint is the OTLP/gRPC endpoint (host:port) to export metrics
	// to. Defaults to "otel-collector-collector.otel-collector-system:4317".
	Endpoint string `json:"endpoint"`

	// Insecure disables TLS on the OTLP connection. Defaults to false.
	Insecure bool `json:"insecure"`

	// Headers are static OTLP headers attached to every export request,
	// typically used for authentication or tenant routing.
	Headers map[string]string `json:"headers,omitempty"`

	// CollectionInterval is the PeriodicReader interval. Defaults to 30s.
	CollectionInterval metav1.Duration `json:"collectionInterval"`

	// DefaultMetricPrefix is the controller-wide default metric name
	// prefix applied to every emitted metric family, unless overridden by
	// a policy's spec.metricNamePrefix. Defaults to "datum_".
	DefaultMetricPrefix string `json:"defaultMetricPrefix"`

	// ResourceAttributes are additional OTel resource attributes merged
	// into the MeterProvider resource. Keys and values are used verbatim.
	ResourceAttributes map[string]string `json:"resourceAttributes,omitempty"`
}

// +k8s:deepcopy-gen=true

// DiscoveryConfig configures cluster discovery for the operator.
type DiscoveryConfig struct {
	// Mode is the mode that the operator should use to discover clusters.
	//
	// Defaults to "single"
	Mode multicluster.Provider `json:"mode"`

	// InternalServiceDiscovery will result in the operator connecting to
	// internal service addresses for projects.
	InternalServiceDiscovery bool `json:"internalServiceDiscovery"`

	// DiscoveryKubeconfigPath is the path to the kubeconfig file to use for
	// project discovery. When not provided, the operator will use the
	// in-cluster config.
	DiscoveryKubeconfigPath string `json:"discoveryKubeconfigPath"`

	// ProjectKubeconfigPath is the path to the kubeconfig file to use as a
	// template when connecting to project control planes. When not provided,
	// the operator will use the in-cluster config.
	ProjectKubeconfigPath string `json:"projectKubeconfigPath"`

	// CollectRootControlPlane, when true and discovery.mode is "milo", also
	// collects metrics from the root/management control plane (the cluster
	// used for project discovery). Metrics from the root control plane are
	// emitted with milo.project.name = "root". Defaults to false.
	CollectRootControlPlane bool `json:"collectRootControlPlane"`
}

// SetDefaults_DiscoveryConfig sets default values for DiscoveryConfig.
func SetDefaults_DiscoveryConfig(obj *DiscoveryConfig) {
	if obj.Mode == "" {
		obj.Mode = multicluster.ProviderSingle
	}
}

// SetDefaults_OtelConfig sets default values for OtelConfig.
func SetDefaults_OtelConfig(obj *OtelConfig) {
	if obj.Endpoint == "" {
		obj.Endpoint = "otel-collector-collector.otel-collector-system:4317"
	}
	if obj.CollectionInterval.Duration == 0 {
		obj.CollectionInterval = metav1.Duration{Duration: 30 * time.Second}
	}
}

// DiscoveryRestConfig returns the REST config to use for project discovery.
func (c *DiscoveryConfig) DiscoveryRestConfig() (*rest.Config, error) {
	if c.DiscoveryKubeconfigPath == "" {
		return ctrl.GetConfig()
	}
	return clientcmd.BuildConfigFromFlags("", c.DiscoveryKubeconfigPath)
}

// ProjectRestConfig returns the REST config to use as a template when
// connecting to project control planes.
func (c *DiscoveryConfig) ProjectRestConfig() (*rest.Config, error) {
	if c.ProjectKubeconfigPath == "" {
		return ctrl.GetConfig()
	}
	return clientcmd.BuildConfigFromFlags("", c.ProjectKubeconfigPath)
}

func init() {
	SchemeBuilder.Register(&ResourceMetricsOperator{})
}

// String returns a human-readable rendering of the operator config suitable
// for logging. The values of OtelConfig.Headers are replaced with
// "<redacted>" because they frequently contain credentials such as
// "Authorization: Bearer ..." tokens. Header keys are preserved so operators
// can still see which headers are configured.
func (c *ResourceMetricsOperator) String() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("{Discovery:%s Otel:%s}", c.Discovery.String(), c.Otel.String())
}

// MarshalLog implements the logr marshaler convention so structured loggers
// that honor it will also get the redacted form.
func (c *ResourceMetricsOperator) MarshalLog() any {
	return c.String()
}

// String returns a human-readable rendering of the OtelConfig with the
// Headers map values redacted. Keys are preserved.
func (c OtelConfig) String() string {
	return fmt.Sprintf(
		"{Endpoint:%q Insecure:%t Headers:%s CollectionInterval:%s DefaultMetricPrefix:%q ResourceAttributes:%s}",
		c.Endpoint,
		c.Insecure,
		redactHeaders(c.Headers),
		c.CollectionInterval.Duration,
		c.DefaultMetricPrefix,
		formatStringMap(c.ResourceAttributes),
	)
}

// MarshalLog implements the logr marshaler convention. Returning the string
// form ensures structured loggers see the redacted rendering even if they
// would otherwise reflect over the struct.
func (c OtelConfig) MarshalLog() any {
	return c.String()
}

// String returns a human-readable rendering of the DiscoveryConfig.
func (c DiscoveryConfig) String() string {
	return fmt.Sprintf(
		"{Mode:%q InternalServiceDiscovery:%t DiscoveryKubeconfigPath:%q ProjectKubeconfigPath:%q CollectRootControlPlane:%t}",
		string(c.Mode),
		c.InternalServiceDiscovery,
		c.DiscoveryKubeconfigPath,
		c.ProjectKubeconfigPath,
		c.CollectRootControlPlane,
	)
}

// redactHeaders renders a headers map with keys preserved and values
// replaced by redactedHeaderValue. Keys are sorted for stable output.
func redactHeaders(h map[string]string) string {
	if len(h) == 0 {
		return "map[]"
	}
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("map[")
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s:%s", k, redactedHeaderValue)
	}
	b.WriteByte(']')
	return b.String()
}

// formatStringMap renders a map[string]string with stable key ordering. Used
// for non-sensitive string maps to keep String() output deterministic.
func formatStringMap(m map[string]string) string {
	if len(m) == 0 {
		return "map[]"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("map[")
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s:%s", k, m[k])
	}
	b.WriteByte(']')
	return b.String()
}
