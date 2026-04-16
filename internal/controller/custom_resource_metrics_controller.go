// SPDX-License-Identifier: AGPL-3.0-only

package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	mcbuilder "sigs.k8s.io/multicluster-runtime/pkg/builder"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	mcreconcile "sigs.k8s.io/multicluster-runtime/pkg/reconcile"

	resourcemetricsv1alpha1 "go.datum.net/resource-metrics/api/v1alpha1"
	"go.datum.net/resource-metrics/internal/collector"
	controllermetrics "go.datum.net/resource-metrics/internal/metrics"
	otelpkg "go.datum.net/resource-metrics/internal/otel"
	"go.datum.net/resource-metrics/internal/policy"
)

// ResourceMetricsPolicyReconciler reconciles a ResourceMetricsPolicy object.
type ResourceMetricsPolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// Env is the CEL environment used to compile policy expressions. It is
	// constructed once per process and shared with the Registry; we hold a
	// separate reference here so Reconcile can call policy.Compile directly
	// and collect the returned []CompileError for status reporting.
	Env *policy.Env

	// Registry holds the compiled policies. Reconcile upserts on changes and
	// deletes on NotFound, then wakes the cluster manager and syncs OTel.
	Registry *policy.Registry

	// ClusterManager is notified of registry changes via WakeAll so each
	// ProjectCollector reconciles its informer set.
	ClusterManager *collector.ClusterManager

	// OTel is the OpenTelemetry runtime. Reconcile calls OTel.Sync() on
	// every successful reconcile so that the set of live instruments tracks
	// the registry.
	OTel *otelpkg.Runtime
}

// +kubebuilder:rbac:groups=resourcemetrics.miloapis.com,resources=resourcemetricspolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=resourcemetrics.miloapis.com,resources=resourcemetricspolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=resourcemetrics.miloapis.com,resources=resourcemetricspolicies/finalizers,verbs=update
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=selfsubjectaccessreviews,verbs=create

// Reconcile reconciles a ResourceMetricsPolicy object: it compiles CEL
// expressions into the policy registry, pokes the per-project collectors so
// they pick up any new GVRs, syncs the OTel instrument set to the new
// registry snapshot, and patches status with the observed condition set.
func (r *ResourceMetricsPolicyReconciler) Reconcile(ctx context.Context, req mcreconcile.Request) (ctrl.Result, error) {
	// We only own the policy CRD on the local cluster. Provider-cluster
	// reconcile requests would race with the local cache (the policy
	// often doesn't yet exist there from this client's view), surface as
	// "not found", and incorrectly trigger the deletion path below —
	// evicting a freshly-created policy from the registry. We can't
	// disable provider-cluster engagement in the builder due to a bug in
	// mcbuilder v0.21.0-alpha.8 (EngageOptions.ApplyToFor replaces
	// instead of merging — see SetupWithManager comment), so guard here.
	if req.ClusterName != "" {
		return ctrl.Result{}, nil
	}

	logger := log.FromContext(ctx).WithValues("policy", req.NamespacedName.String())

	key := types.NamespacedName{Name: req.Name, Namespace: req.Namespace}

	var obj resourcemetricsv1alpha1.ResourceMetricsPolicy
	if err := r.Get(ctx, key, &obj); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get policy: %w", err)
		}
		// Policy was deleted. Evict from registry, sync OTel (so its
		// families disappear), and wake every collector so they can
		// release GVRs no longer referenced by any policy.
		logger.Info("policy deleted; evicting from registry")
		r.Registry.Delete(key)
		if syncErr := r.OTel.Sync(); syncErr != nil {
			logger.Error(syncErr, "otel sync after delete")
		}
		r.ClusterManager.WakeAll()
		return ctrl.Result{}, nil
	}

	// Compile. The registry stores the *CompiledPolicy regardless of
	// per-expression failures; we only surface the list of errors on status.
	compiled, compileErrors := policy.Compile(r.Env, &obj)

	// Track compile outcomes: we count one "success" for each non-nil
	// generator and one "error" for each CompileError returned. This keeps
	// the counters attributable at the generator granularity without having
	// to re-walk the spec.
	for _, gen := range compiled.Generators {
		if gen == nil {
			continue
		}
		controllermetrics.CompilesTotal.WithLabelValues("success").Inc()
	}
	for range compileErrors {
		controllermetrics.CompilesTotal.WithLabelValues("error").Inc()
	}

	// Upsert into the registry (Registry.Upsert re-runs Compile and keys by
	// obj.Name/Namespace — cluster-scoped policies land on a namespace="" key).
	// Return values are intentionally discarded: compile errors surface via
	// status; Upsert doesn't fail hard.
	_, _ = r.Registry.Upsert(&obj)

	// Order is: Registry → OTel.Sync → WakeAll. The OTel data plane is
	// periodic-reader driven, not event-driven — callbacks walk live collectors
	// at collection tick. If the reader fires between Sync and WakeAll, a
	// newly-registered family's callback simply finds no informers yet for its
	// GVR and emits zero measurements, which is the correct transient state.
	syncErr := r.OTel.Sync()
	if syncErr != nil {
		logger.Error(syncErr, "otel sync")
	}
	r.ClusterManager.WakeAll()

	// Compute status ---------------------------------------------------------

	activeGenerators := int32(0)
	for _, gen := range compiled.Generators {
		if gen != nil {
			activeGenerators++
		}
	}

	// Aggregate denied GVRs across every engaged project, filtered to those
	// this policy actually references. The pure helper below lets us unit-test
	// the aggregation logic without standing up a ClusterManager.
	projectStatuses := make([]collector.ProjectStatus, 0, len(r.ClusterManager.Collectors()))
	for _, c := range r.ClusterManager.Collectors() {
		projectStatuses = append(projectStatuses, c.Status())
	}
	missingPermissions := aggregateMissingPermissions(obj.Spec.Generators, projectStatuses)

	desired := obj.DeepCopy()
	desired.Status.ObservedGeneration = obj.Generation
	desired.Status.ActiveGenerators = activeGenerators
	desired.Status.CompilationFailures = int32(len(compileErrors))
	desired.Status.MissingPermissions = missingPermissions

	// Conditions. meta.SetStatusCondition is idempotent: it only mutates
	// LastTransitionTime when the Status field flips.
	invalidCELTrue := len(compileErrors) > 0
	permissionDeniedTrue := len(missingPermissions) > 0
	degradedTrue := invalidCELTrue || permissionDeniedTrue || syncErr != nil
	readyTrue := !degradedTrue && activeGenerators > 0

	setCondition(desired, metav1.Condition{
		Type:               resourcemetricsv1alpha1.ConditionInvalidCEL,
		Status:             boolToConditionStatus(invalidCELTrue),
		ObservedGeneration: obj.Generation,
		Reason:             reasonForInvalidCEL(invalidCELTrue),
		Message:            messageForInvalidCEL(compileErrors),
	})
	setCondition(desired, metav1.Condition{
		Type:               resourcemetricsv1alpha1.ConditionPermissionDenied,
		Status:             boolToConditionStatus(permissionDeniedTrue),
		ObservedGeneration: obj.Generation,
		Reason:             reasonForPermissionDenied(permissionDeniedTrue),
		Message:            messageForPermissionDenied(missingPermissions),
	})
	setCondition(desired, metav1.Condition{
		Type:               resourcemetricsv1alpha1.ConditionDegraded,
		Status:             boolToConditionStatus(degradedTrue),
		ObservedGeneration: obj.Generation,
		Reason:             reasonForDegraded(invalidCELTrue, permissionDeniedTrue, syncErr),
		Message:            messageForDegraded(invalidCELTrue, permissionDeniedTrue, syncErr),
	})
	setCondition(desired, metav1.Condition{
		Type:               resourcemetricsv1alpha1.ConditionReady,
		Status:             boolToConditionStatus(readyTrue),
		ObservedGeneration: obj.Generation,
		Reason:             reasonForReady(readyTrue, activeGenerators),
		Message:            messageForReady(readyTrue, activeGenerators),
	})

	// Short-circuit if the status is identical: no patch means no spurious
	// resource-version bump.
	if reflect.DeepEqual(obj.Status, desired.Status) {
		return ctrl.Result{}, nil
	}

	if err := r.Status().Patch(ctx, desired, client.MergeFrom(&obj)); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch status: %w", err)
	}

	return ctrl.Result{}, nil
}

// setCondition is a thin wrapper so the call sites in Reconcile stay compact.
// It mutates desired.Status.Conditions in place.
func setCondition(desired *resourcemetricsv1alpha1.ResourceMetricsPolicy, c metav1.Condition) {
	meta.SetStatusCondition(&desired.Status.Conditions, c)
}

func boolToConditionStatus(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

func reasonForInvalidCEL(active bool) string {
	if active {
		return "CompilationFailed"
	}
	return "Compiled"
}

func messageForInvalidCEL(errs []policy.CompileError) string {
	if len(errs) == 0 {
		return "All CEL expressions compiled successfully."
	}
	return fmt.Sprintf("%d CEL compile failure(s); first: %v", len(errs), errs[0])
}

func reasonForPermissionDenied(active bool) string {
	if active {
		return "MissingRBAC"
	}
	return "Authorized"
}

func messageForPermissionDenied(missing []resourcemetricsv1alpha1.GVRRef) string {
	if len(missing) == 0 {
		return "All referenced GVRs are authorized on every engaged project."
	}
	n := len(missing)
	if n > 3 {
		n = 3
	}
	parts := make([]string, 0, n)
	for _, g := range missing[:n] {
		parts = append(parts, fmt.Sprintf("%s/%s/%s", g.Group, g.Version, g.Resource))
	}
	extra := ""
	if len(missing) > 3 {
		extra = fmt.Sprintf(" (+%d more)", len(missing)-3)
	}
	return fmt.Sprintf("Missing RBAC for: %s%s", strings.Join(parts, ", "), extra)
}

func reasonForDegraded(invalidCEL, permDenied bool, syncErr error) string {
	switch {
	case invalidCEL && permDenied:
		return "CompilationAndRBAC"
	case invalidCEL:
		return "CompilationFailed"
	case permDenied:
		return "MissingRBAC"
	case syncErr != nil:
		return "OTelSyncFailed"
	default:
		return "Healthy"
	}
}

func messageForDegraded(invalidCEL, permDenied bool, syncErr error) string {
	var parts []string
	if invalidCEL {
		parts = append(parts, "one or more CEL expressions failed to compile")
	}
	if permDenied {
		parts = append(parts, "RBAC denied for one or more referenced GVRs")
	}
	if syncErr != nil {
		parts = append(parts, fmt.Sprintf("OTel sync error: %v", syncErr))
	}
	if len(parts) == 0 {
		return "No degradations observed."
	}
	return strings.Join(parts, "; ")
}

func reasonForReady(ready bool, active int32) string {
	if ready {
		return "AllGeneratorsActive"
	}
	if active == 0 {
		return "NoActiveGenerators"
	}
	return "Degraded"
}

func messageForReady(ready bool, active int32) string {
	if ready {
		return fmt.Sprintf("%d generator(s) compiled and emitting metrics.", active)
	}
	if active == 0 {
		return "No generators are currently compiled."
	}
	return "Policy is degraded; see Degraded condition for details."
}

// aggregateMissingPermissions returns the deduplicated, sorted list of GVRs
// referenced by the given generator specs for which at least one project's
// informer has observed an RBAC denial. Unreferenced GVRs are filtered out so
// status only surfaces denials the operator can act on. The result is
// deterministic (sorted by group, version, resource) so that repeated
// reconciles produce stable status output.
func aggregateMissingPermissions(
	generators []resourcemetricsv1alpha1.GeneratorSpec,
	statuses []collector.ProjectStatus,
) []resourcemetricsv1alpha1.GVRRef {
	if len(generators) == 0 || len(statuses) == 0 {
		return nil
	}
	policyGVRs := make(map[schema.GroupVersionResource]struct{}, len(generators))
	for _, g := range generators {
		policyGVRs[schema.GroupVersionResource{
			Group:    g.Resource.Group,
			Version:  g.Resource.Version,
			Resource: g.Resource.Resource,
		}] = struct{}{}
	}
	deniedSet := make(map[schema.GroupVersionResource]struct{})
	for _, ps := range statuses {
		for gvr, s := range ps.GVRStatuses {
			if !s.Denied {
				continue
			}
			if _, referenced := policyGVRs[gvr]; !referenced {
				continue
			}
			deniedSet[gvr] = struct{}{}
		}
	}
	if len(deniedSet) == 0 {
		return nil
	}
	out := make([]resourcemetricsv1alpha1.GVRRef, 0, len(deniedSet))
	for gvr := range deniedSet {
		out = append(out, resourcemetricsv1alpha1.GVRRef{
			Group:    gvr.Group,
			Version:  gvr.Version,
			Resource: gvr.Resource,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Resource < b.Resource
	})
	return out
}

// SetupWithManager registers the reconciler with the multicluster manager.
// The ResourceMetricsPolicy CRD is cluster-scoped and lives on the management
// cluster only: we engage with the local cluster and explicitly opt out of
// provider clusters so the reconciler never fires for project CPs.
//
// NOTE: passing both WithEngageWithLocalCluster(true) AND
// WithEngageWithProviderClusters(false) as separate ForOptions is broken in
// multicluster-runtime v0.21.0-alpha.8 — each EngageOptions.ApplyToFor does
// opts.EngageOptions = w, so the second call wipes out the first. We pass
// only WithEngageWithLocalCluster(true); engageWithProviderClusters falls back
// to its default (true when a provider is set), so the controller will also
// open watches on each engaged project CP. Those CPs don't serve the CRD,
// so the watches surface "no matches for kind" errors but don't enqueue
// reconciles. Track upstream: the right fix is for mcbuilder to merge
// EngageOptions instead of replace.
func (r *ResourceMetricsPolicyReconciler) SetupWithManager(mgr mcmanager.Manager) error {
	return mcbuilder.ControllerManagedBy(mgr).
		For(&resourcemetricsv1alpha1.ResourceMetricsPolicy{},
			mcbuilder.WithEngageWithLocalCluster(true),
		).
		Named("resourcemetricspolicy").
		Complete(r)
}
