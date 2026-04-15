// SPDX-License-Identifier: AGPL-3.0-only

package metrics_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"go.datum.net/resource-metrics/internal/metrics"
)

func TestEffectivePrefix(t *testing.T) {
	cases := []struct {
		name       string
		policy     string
		controller string
		want       string
	}{
		{"policy wins over controller", "my_policy", "default", "my_policy"},
		{"controller used when policy empty", "", "default", "default"},
		{"empty if both empty", "", "", ""},
		{"policy wins even if long", "a_very_long_prefix", "def", "a_very_long_prefix"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, metrics.EffectivePrefix(c.policy, c.controller))
		})
	}
}

func TestFamilyMetricName(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		family string
		want   string
	}{
		{"simple join", "datum", "workload_info", "datum_workload_info"},
		{"empty prefix returns family", "", "workload_info", "workload_info"},
		{"collapses trailing underscore on prefix", "datum_", "workload_info", "datum_workload_info"},
		{"collapses leading underscore on family", "datum", "_workload_info", "datum_workload_info"},
		{"collapses both sides", "datum_", "_workload_info", "datum_workload_info"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, metrics.FamilyMetricName(c.prefix, c.family))
		})
	}
}
