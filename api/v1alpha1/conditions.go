// SPDX-License-Identifier: AGPL-3.0-only

package v1alpha1

// Condition type constants for ResourceMetricsPolicy.Status.Conditions.
const (
	// ConditionReady indicates that the policy is fully reconciled and all
	// generators are compiled and emitting metrics.
	ConditionReady = "Ready"

	// ConditionDegraded indicates that the policy is partially functional:
	// some generators are active, but one or more are failing.
	ConditionDegraded = "Degraded"

	// ConditionInvalidCEL indicates that one or more CEL expressions in the
	// policy failed to compile.
	ConditionInvalidCEL = "InvalidCEL"

	// ConditionPermissionDenied indicates that the controller lacks RBAC
	// permissions to watch or list at least one resource referenced by the
	// policy on at least one engaged project control plane.
	ConditionPermissionDenied = "PermissionDenied"
)
