// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"context"
	"fmt"

	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// preflight issues SelfSubjectAccessReviews asking whether the current
// credentials may both list AND watch the given GVR cluster-wide. Both
// verbs are required because the informer machinery performs a watch
// after its initial list; if only list is allowed, the informer silently
// fails during sync. It returns:
//
//   - (true, nil)  — API says allowed for both verbs.
//   - (false, nil) — API says at least one verb is not allowed.
//   - (false, err) — an SSAR call itself failed; caller should treat this
//     as a denial and surface err as lastErr.
//
// SSAR only answers RBAC questions. It cannot tell us that a CRD is not
// installed — that is detected separately when the informer cache-sync
// times out and a subsequent list returns NotFound.
func (c *ProjectCollector) preflight(ctx context.Context, gvr schema.GroupVersionResource) (bool, error) {
	if c.authzClient == nil {
		// In tests we may not wire an authz client; treat missing authz as
		// "allow" so reconcile() can exercise the happy path.
		return true, nil
	}
	// Run list first, then watch. If watch is denied but list is allowed,
	// we want to surface the watch denial (it's the stricter failure that
	// actually breaks the informer).
	listAllowed, err := c.checkVerb(ctx, gvr, "list")
	if err != nil {
		return false, err
	}
	watchAllowed, err := c.checkVerb(ctx, gvr, "watch")
	if err != nil {
		return false, err
	}
	if !watchAllowed {
		// Prefer the watch-denial message when list was allowed but watch
		// was not — that's the non-obvious failure mode worth surfacing.
		return false, nil
	}
	if !listAllowed {
		return false, nil
	}
	return true, nil
}

// checkVerb issues a single SSAR for the given verb against the GVR.
func (c *ProjectCollector) checkVerb(ctx context.Context, gvr schema.GroupVersionResource, verb string) (bool, error) {
	review := &authorizationv1.SelfSubjectAccessReview{
		Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Verb:     verb,
				Group:    gvr.Group,
				Version:  gvr.Version,
				Resource: gvr.Resource,
			},
		},
	}
	resp, err := c.authzClient.SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("ssar %s: %w", verb, err)
	}
	return resp.Status.Allowed, nil
}

// listOptionsOne returns a ListOptions that requests at most one item,
// used by the informer-sync failure classifier to detect NotFound / 403.
func listOptionsOne() metav1.ListOptions {
	return metav1.ListOptions{Limit: 1}
}
