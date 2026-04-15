// SPDX-License-Identifier: AGPL-3.0-only

package metrics

import "strings"

// EffectivePrefix returns the policy-level prefix if set, otherwise the
// controller default. Either or both may be empty; if both are empty the
// result is empty.
func EffectivePrefix(policyPrefix, controllerDefault string) string {
	if policyPrefix != "" {
		return policyPrefix
	}
	return controllerDefault
}

// FamilyMetricName joins a prefix and a family name, collapsing any extra
// underscores at the boundary. If prefix is empty the family name is returned
// verbatim.
func FamilyMetricName(prefix, familyName string) string {
	if prefix == "" {
		return familyName
	}
	p := strings.TrimRight(prefix, "_")
	f := strings.TrimLeft(familyName, "_")
	return p + "_" + f
}
