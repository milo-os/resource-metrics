// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceMetricsPolicySpec defines the desired state of ResourceMetricsPolicy.
type ResourceMetricsPolicySpec struct {
	// Generators defines the set of resource metric generators.
	// +listType=map
	// +listMapKey=name
	// +kubebuilder:validation:MinItems=1
	Generators []GeneratorSpec `json:"generators"`

	// MetricNamePrefix, when set, overrides the controller's
	// --default-metric-prefix flag for metrics emitted by this policy.
	// Must start with a letter, underscore, or colon and contain only
	// letters, digits, underscores, and colons.
	// +optional
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_:][a-zA-Z0-9_:]*$`
	// +kubebuilder:validation:MaxLength=32
	MetricNamePrefix string `json:"metricNamePrefix,omitempty"`
}

// GeneratorSpec defines a single resource metric generator.
type GeneratorSpec struct {
	// Name is a unique name for this generator.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z][a-z0-9-]*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Resource identifies the Kubernetes API resource to monitor.
	// +kubebuilder:validation:Required
	Resource ResourceReference `json:"resource"`

	// Families defines the metric families to emit for each resource instance.
	// +listType=map
	// +listMapKey=name
	Families []MetricFamilySpec `json:"families,omitempty"`
}

// ResourceReference identifies a Kubernetes API resource by group, version, and plural name.
type ResourceReference struct {
	// Group is the API group of the resource (e.g. "compute.miloapis.com").
	// Empty string targets core resources (configmaps, pods, namespaces, …).
	Group string `json:"group"`
	// Version is the API version (e.g. "v1alpha1").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// Resource is the plural resource name (e.g. "workloads").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Resource string `json:"resource"`
}

// MetricFamilySpec defines a Prometheus metric family emitted per resource instance.
type MetricFamilySpec struct {
	// Name is the base metric name (e.g. "workload_info").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_:][a-zA-Z0-9_:]*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Help is the help string for this metric family.
	// +optional
	Help string `json:"help,omitempty"`

	// Type is the Prometheus metric type. Only "gauge" is supported in v1.
	// +kubebuilder:validation:Enum=gauge
	// +kubebuilder:default=gauge
	Type string `json:"type,omitempty"`

	// Metrics defines how to produce individual metric series from each resource.
	// Metrics in a family have no stable identity, so this list is treated as
	// atomic for server-side apply.
	// +listType=atomic
	// +kubebuilder:validation:MinItems=1
	Metrics []MetricSpec `json:"metrics,omitempty"`
}

// MetricSpec defines a single metric series within a family.
type MetricSpec struct {
	// Labels defines the labels to attach to this metric series.
	Labels []LabelSpec `json:"labels,omitempty"`

	// Value is a CEL expression evaluated against the resource object
	// that produces the metric value. Defaults to 1 if omitted.
	// +optional
	Value *string `json:"value,omitempty"`
}

// LabelSpec defines a single label on a metric series.
type LabelSpec struct {
	// Name is the label name. Must match the Prometheus label name syntax.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_]*$`
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`

	// Value is a CEL expression evaluated against the resource object.
	// +kubebuilder:validation:Required
	Value string `json:"value"`
}

// GVRRef identifies a Kubernetes API resource by group, version, and plural name.
type GVRRef struct {
	// Group is the API group of the resource.
	// Empty string targets core resources (configmaps, pods, namespaces, …).
	Group string `json:"group"`
	// Version is the API version.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Version string `json:"version"`
	// Resource is the plural resource name.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Resource string `json:"resource"`
}

// ResourceMetricsPolicyStatus defines the observed state of ResourceMetricsPolicy.
type ResourceMetricsPolicyStatus struct {
	// ObservedGeneration reflects the .metadata.generation the controller
	// has most recently acted upon.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ActiveGenerators is the number of generators currently compiled and
	// actively emitting metrics for this policy.
	// +optional
	ActiveGenerators int32 `json:"activeGenerators,omitempty"`

	// CompilationFailures is the number of generators that failed to compile
	// (typically due to invalid CEL).
	// +optional
	CompilationFailures int32 `json:"compilationFailures,omitempty"`

	// MissingPermissions lists the GVRs for which the controller lacks the
	// RBAC permissions required to watch or list on at least one engaged
	// project control plane.
	// +optional
	// +listType=atomic
	MissingPermissions []GVRRef `json:"missingPermissions,omitempty"`

	// Conditions represent the latest observations of the resource's state.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:metadata:annotations="discovery.miloapis.com/parent-contexts=Platform"

// ResourceMetricsPolicy is the Schema for the resourcemetricspolicies API.
type ResourceMetricsPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceMetricsPolicySpec   `json:"spec,omitempty"`
	Status ResourceMetricsPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceMetricsPolicyList contains a list of ResourceMetricsPolicy.
type ResourceMetricsPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceMetricsPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceMetricsPolicy{}, &ResourceMetricsPolicyList{})
}
