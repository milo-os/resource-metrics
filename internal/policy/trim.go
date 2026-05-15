// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"slices"
	"strings"
)

var identityFields = []string{
	"apiVersion",
	"kind",
	"metadata.name",
	"metadata.namespace",
	"metadata.resourceVersion",
	"metadata.uid",
}

// TrimObjectInPlace prunes obj in place, keeping only keys that match fields
// or the always-kept identity set. It is intended to be used as a
// cache.TransformFunc. Pruning is idempotent (deleting a key that's already
// gone is a no-op), and no allocations occur for obj itself.
//
// Callers that expect to own the object must deep-copy the map first.
func TrimObjectInPlace(obj map[string]any, fields []string) {
	trimAt(obj, slices.Concat(fields, identityFields))
}

func trimAt(m map[string]any, paths []string) {
	leaf, branch := groupPaths(paths)
	for k, v := range m {
		if _, keep := leaf[k]; keep {
			continue
		}
		if sub, recurse := branch[k]; recurse {
			if child, ok := v.(map[string]any); ok {
				trimAt(child, sub)
				continue
			}
		}
		delete(m, k)
	}
}

func groupPaths(paths []string) (map[string]struct{}, map[string][]string) {
	leaf, branch := map[string]struct{}{}, map[string][]string{}
	for _, p := range paths {
		first, rest, isNested := strings.Cut(p, ".")
		if !isNested {
			leaf[first] = struct{}{}
			delete(branch, first)
			continue
		}
		if _, ok := leaf[first]; ok {
			continue
		}
		branch[first] = append(branch[first], rest)
	}
	return leaf, branch
}
