// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// newAuthzFakeWithVerbDecisions builds a fake Kubernetes clientset that
// answers SelfSubjectAccessReview creates using the provided per-verb
// allow map. If a verb is not present the review is denied.
func newAuthzFakeWithVerbDecisions(allow map[string]bool) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("create", "selfsubjectaccessreviews", func(action clienttesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(clienttesting.CreateAction)
		if !ok {
			return true, nil, errors.New("expected CreateAction for SSAR")
		}
		review, ok := ca.GetObject().(*authorizationv1.SelfSubjectAccessReview)
		if !ok {
			return true, nil, errors.New("expected SelfSubjectAccessReview object")
		}
		resp := review.DeepCopy()
		verb := ""
		if resp.Spec.ResourceAttributes != nil {
			verb = resp.Spec.ResourceAttributes.Verb
		}
		resp.Status = authorizationv1.SubjectAccessReviewStatus{
			Allowed: allow[verb],
		}
		return true, resp, nil
	})
	return cs
}

// TestPreflight_WatchDenied verifies that preflight requires both list AND
// watch; if only list is allowed but watch is denied, preflight must
// return (false, nil). Without this check the informer would fail silently
// during sync.
func TestPreflight_WatchDenied(t *testing.T) {
	cs := newAuthzFakeWithVerbDecisions(map[string]bool{
		"list":  true,
		"watch": false,
	})
	pc := &ControlPlaneCollector{
		authzClient: cs.AuthorizationV1(),
	}
	gvr := schema.GroupVersionResource{Group: "example.test", Version: "v1", Resource: "widgets"}

	allowed, err := pc.preflight(t.Context(), gvr)
	require.NoError(t, err)
	require.False(t, allowed, "preflight must deny when watch is not allowed, even if list is")
}

// TestPreflight_BothAllowed confirms the happy path returns (true, nil).
func TestPreflight_BothAllowed(t *testing.T) {
	cs := newAuthzFakeWithVerbDecisions(map[string]bool{
		"list":  true,
		"watch": true,
	})
	pc := &ControlPlaneCollector{
		authzClient: cs.AuthorizationV1(),
	}
	gvr := schema.GroupVersionResource{Group: "example.test", Version: "v1", Resource: "widgets"}

	allowed, err := pc.preflight(t.Context(), gvr)
	require.NoError(t, err)
	require.True(t, allowed)
}

// TestPreflight_ListDenied confirms that list denial still denies the
// whole preflight (previous behavior preserved).
func TestPreflight_ListDenied(t *testing.T) {
	cs := newAuthzFakeWithVerbDecisions(map[string]bool{
		"list":  false,
		"watch": true,
	})
	pc := &ControlPlaneCollector{
		authzClient: cs.AuthorizationV1(),
	}
	gvr := schema.GroupVersionResource{Group: "example.test", Version: "v1", Resource: "widgets"}

	allowed, err := pc.preflight(t.Context(), gvr)
	require.NoError(t, err)
	require.False(t, allowed)
}
