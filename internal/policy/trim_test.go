// SPDX-License-Identifier: AGPL-3.0-only

package policy_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"go.datum.net/resource-metrics/internal/policy"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func makeTestObject() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name":            "foo",
			"namespace":       "bar",
			"resourceVersion": "42",
			"uid":             "uid-123",
			"generation":      int64(3),
			"labels":          map[string]any{"app": "myapp"},
			"annotations":     map[string]any{"note": "value"},
			"managedFields": []any{
				map[string]any{"manager": "kubectl", "operation": "Apply"},
			},
		},
		"spec": map[string]any{
			"replicas": int64(3),
			"image":    "nginx",
			"template": map[string]any{
				"spec": map[string]any{
					"containers": []any{map[string]any{"name": "nginx"}},
					"volumes":    []any{},
				},
			},
		},
		"status": map[string]any{"ready": true, "conditions": []any{}},
	}
}

func TestTrimObjectInPlace(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		want   string
	}{
		{
			name:   "pick fields/0",
			fields: []string{"status.ready"},
			want: `{
				"metadata": {"name":"foo","namespace":"bar","resourceVersion":"42","uid":"uid-123"},
				"status": {"ready": true}
			}`,
		},
		{
			name:   "pick fields/1",
			fields: []string{"spec.replicas"},
			want: `{
				"metadata": {"name":"foo","namespace":"bar","resourceVersion":"42","uid":"uid-123"},
				"spec": {"replicas": 3}
			}`,
		},
		{
			name:   "pick fields/2",
			fields: []string{"metadata.labels"},
			want: `{
				"metadata": {
					"name": "foo",
					"namespace": "bar",
					"resourceVersion": "42",
					"uid": "uid-123",
					"labels": {"app": "myapp"}
				}
			}`,
		},
		{
			name:   "no fields preserves identity",
			fields: nil,
			want:   `{"metadata": {"name":"foo","namespace":"bar","resourceVersion":"42","uid":"uid-123"}}`,
		},
		{
			name:   "missing field is ignored",
			fields: []string{"status.nonexistent"},
			want: `{
				"metadata": {"name":"foo","namespace":"bar","resourceVersion":"42","uid":"uid-123"},
				"status": {}
			}`,
		},
		{
			name:   "nested paths",
			fields: []string{"spec.template.spec.containers"},
			want: `{
				"metadata": {"name":"foo","namespace":"bar","resourceVersion":"42","uid":"uid-123"},
				"spec": {"template": {"spec": {"containers": [{"name": "nginx"}]}}}
			}`,
		},
		{
			name:   "leaf path keeps the whole object",
			fields: []string{"spec.replicas", "spec"},
			want: `{
				"metadata": {"name":"foo","namespace":"bar","resourceVersion":"42","uid":"uid-123"},
				"spec": {
					"replicas": 3,
					"image": "nginx",
					"template": {"spec": {"containers": [{"name": "nginx"}], "volumes": []}}
				}
			}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			obj := makeTestObject()
			policy.TrimObjectInPlace(obj, tc.fields)
			require.JSONEq(t, tc.want, mustJSON(t, obj))
		})
	}

	t.Run("TrimObjectInPlace is idempotent", func(t *testing.T) {
		obj := makeTestObject()
		policy.TrimObjectInPlace(obj, []string{"spec.replicas"})
		first := mustJSON(t, obj)
		policy.TrimObjectInPlace(obj, []string{"spec.replicas"})
		require.JSONEq(t, first, mustJSON(t, obj))
	})

	t.Run("incorrect lookup is ignored", func(t *testing.T) {
		obj := map[string]any{"spec": int64(42)}
		policy.TrimObjectInPlace(obj, []string{"spec.replicas"})
		require.JSONEq(t, `{}`, mustJSON(t, obj))
	})
}
