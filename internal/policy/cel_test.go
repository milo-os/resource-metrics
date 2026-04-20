// SPDX-License-Identifier: AGPL-3.0-only

package policy_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.datum.net/resource-metrics/internal/policy"
)

func mustEnv(t *testing.T) *policy.Env {
	t.Helper()
	env, err := policy.NewEnv()
	require.NoError(t, err)
	return env
}

func TestEvalValue_Numeric(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("1 + 2")
	require.NoError(t, err)

	got, err := policy.EvalValue(prog, map[string]any{}, nil)
	require.NoError(t, err)
	require.Equal(t, 3.0, got)
}

func TestEvalValue_BoolTrue(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.status.ready")
	require.NoError(t, err)

	obj := map[string]any{
		"status": map[string]any{"ready": true},
	}
	got, err := policy.EvalValue(prog, obj, nil)
	require.NoError(t, err)
	require.Equal(t, 1.0, got)
}

func TestEvalValue_Exists(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile(
		"object.status.conditions.exists(c, c.type == 'Ready' && c.status == 'True') ? 1.0 : 0.0")
	require.NoError(t, err)

	with := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Ready", "status": "True"},
				map[string]any{"type": "Available", "status": "False"},
			},
		},
	}
	without := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Available", "status": "False"},
			},
		},
	}

	v, err := policy.EvalValue(prog, with, nil)
	require.NoError(t, err)
	require.Equal(t, 1.0, v)

	v, err = policy.EvalValue(prog, without, nil)
	require.NoError(t, err)
	require.Equal(t, 0.0, v)
}

func TestEvalValue_StringNotParseable(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("'hello'")
	require.NoError(t, err)

	_, err = policy.EvalValue(prog, map[string]any{}, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, policy.ErrValueNotNumeric),
		"expected ErrValueNotNumeric, got %v", err)
}

func TestEvalValue_NilFieldPanicRecovers(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.status.nope.deeper")
	require.NoError(t, err)

	// Should return an error (not panic). It may come back as either a
	// CEL no-such-key error or, if something inside the evaluator does
	// panic on our map shape, as ErrEvalPanic. Both outcomes are
	// acceptable; a naked panic is not.
	_, err = policy.EvalValue(prog, map[string]any{"status": map[string]any{}}, nil)
	require.Error(t, err)
}

func TestEvalValue_CostExceeded(t *testing.T) {
	if testing.Short() {
		t.Skip("long-running cost-budget test; skipped in -short")
	}
	env := mustEnv(t)
	// Four nested all() comprehensions over a 100-element list is 10^8
	// iterations, which blows past either the 100M cost budget or the 50ms
	// eval deadline. We accept either signal below.
	prog, err := env.Compile(
		"object.xs.all(a, object.xs.all(b, object.xs.all(c, object.xs.all(d, a + b + c + d >= 0))))")
	require.NoError(t, err)

	xs := make([]any, 100)
	for i := range xs {
		xs[i] = int64(i)
	}
	_, err = policy.EvalValue(prog, map[string]any{"xs": xs}, nil)
	require.Error(t, err)
	require.True(t,
		errors.Is(err, policy.ErrEvalCostExceeded) ||
			errors.Is(err, policy.ErrEvalTimeout) ||
			strings.Contains(err.Error(), "cost limit"),
		"expected cost-limit or timeout error, got %v", err)
}

func TestEvalLabel_Namespace(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.metadata.namespace")
	require.NoError(t, err)

	obj := map[string]any{
		"metadata": map[string]any{"namespace": "foo"},
	}
	got, err := policy.EvalLabel(prog, obj, nil)
	require.NoError(t, err)
	require.Equal(t, "foo", got)
}

func TestEvalLabel_StringConstant(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("'Ready'")
	require.NoError(t, err)

	got, err := policy.EvalLabel(prog, map[string]any{}, nil)
	require.NoError(t, err)
	require.Equal(t, "Ready", got)
}

func TestEvalLabel_NumericCoercion(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.replicas")
	require.NoError(t, err)

	got, err := policy.EvalLabel(prog, map[string]any{"replicas": int64(3)}, nil)
	require.NoError(t, err)
	require.Equal(t, "3", got)
}

// TestEvalLabel_SanitizesControlChars verifies the Issue #1 fix: a CEL
// expression that yields a string containing an embedded newline or other
// C0 control character comes back with those bytes replaced by spaces. A
// hostile annotation must not be able to split a Prometheus exposition
// line or inject a null into an OTel attribute.
func TestEvalLabel_SanitizesControlChars(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.metadata.annotations['x']")
	require.NoError(t, err)

	// Observe the OnLabelSanitized hook while the test runs. We restore
	// the previous value on exit so package-level state does not leak
	// between tests run in the same process.
	prev := policy.OnLabelSanitized
	t.Cleanup(func() { policy.OnLabelSanitized = prev })
	var sanitizedCount int
	policy.OnLabelSanitized = func() { sanitizedCount++ }

	obj := map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				// Newline, carriage return, NUL, tab, and DEL all need to
				// disappear. Normal UTF-8 runes must survive.
				"x": "ok\nnext\rrow\x00with\ttabs\x7fdone-αβ",
			},
		},
	}
	got, err := policy.EvalLabel(prog, obj, nil)
	require.NoError(t, err)

	require.NotContains(t, got, "\n", "sanitized label contained newline")
	require.NotContains(t, got, "\r", "sanitized label contained CR")
	require.NotContains(t, got, "\x00", "sanitized label contained NUL")
	require.NotContains(t, got, "\t", "sanitized label contained tab")
	require.NotContains(t, got, "\x7f", "sanitized label contained DEL")
	require.Equal(t, "ok next row with tabs done-αβ", got)
	require.Equal(t, 1, sanitizedCount,
		"OnLabelSanitized should fire exactly once per sanitized label")
}

// TestEvalLabel_TruncatesLongValues verifies that label values longer than
// the configured max are truncated to that max in bytes (not runes).
func TestEvalLabel_TruncatesLongValues(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.long")
	require.NoError(t, err)

	prev := policy.OnLabelSanitized
	t.Cleanup(func() { policy.OnLabelSanitized = prev })
	var sanitizedCount int
	policy.OnLabelSanitized = func() { sanitizedCount++ }

	long := strings.Repeat("a", 5000)
	got, err := policy.EvalLabel(prog, map[string]any{"long": long}, nil)
	require.NoError(t, err)

	require.LessOrEqual(t, len(got), policy.DefaultLabelValueMaxBytes)
	require.Equal(t, policy.DefaultLabelValueMaxBytes, len(got),
		"a 5000-byte input must be truncated to exactly the max")
	require.Equal(t, 1, sanitizedCount, "truncation should increment the sanitize hook")
}

// TestCycleBudget_ExhaustsAfterN verifies that CycleBudget enforces its
// eval count: after maxEvals Consume calls the next one returns
// ErrCycleBudgetExceeded.
func TestCycleBudget_ExhaustsAfterN(t *testing.T) {
	b := policy.NewCycleBudget(10*time.Second, 3)

	// First three Consume calls succeed (count of 3 means three successful
	// decrements from 3 -> 2 -> 1 -> 0 are all allowed; only the fourth
	// drops below zero and should fail).
	require.NoError(t, b.Consume(), "1st Consume on budget of 3 must succeed")
	require.NoError(t, b.Consume(), "2nd Consume on budget of 3 must succeed")
	require.NoError(t, b.Consume(), "3rd Consume on budget of 3 must succeed")

	// Fourth exhausts the budget.
	err := b.Consume()
	require.Error(t, err)
	require.True(t, errors.Is(err, policy.ErrCycleBudgetExceeded),
		"expected ErrCycleBudgetExceeded, got %v", err)

	// Fifth also fails — the budget stays exhausted, it does not recover.
	err = b.Consume()
	require.Error(t, err)
	require.True(t, errors.Is(err, policy.ErrCycleBudgetExceeded))

	// A nil receiver always succeeds so callers can unconditionally call
	// Consume without a nil check.
	var nilBudget *policy.CycleBudget
	require.NoError(t, nilBudget.Consume())
}

// TestCycleBudget_DeadlineExceeded verifies the wall-clock branch: a
// budget whose deadline is already in the past fails on the very first
// Consume, regardless of the remaining eval count.
func TestCycleBudget_DeadlineExceeded(t *testing.T) {
	// Negative duration => deadline in the past.
	b := policy.NewCycleBudget(-1*time.Second, 1000)
	err := b.Consume()
	require.Error(t, err)
	require.True(t, errors.Is(err, policy.ErrCycleBudgetExceeded),
		"a past deadline must produce ErrCycleBudgetExceeded, got %v", err)
}

func mustItemEnv(t *testing.T) *policy.Env {
	t.Helper()
	base := mustEnv(t)
	env, err := base.NewItemEnv()
	require.NoError(t, err)
	return env
}

func TestEvalForEach_ReturnsList(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.status.conditions")
	require.NoError(t, err)

	obj := map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Ready"},
				map[string]any{"type": "Available"},
			},
		},
	}
	items, err := policy.EvalForEach(prog, obj, nil)
	require.NoError(t, err)
	require.Len(t, items, 2)
}

func TestEvalForEach_EmptyListIsNotError(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.items")
	require.NoError(t, err)

	obj := map[string]any{"items": []any{}}
	items, err := policy.EvalForEach(prog, obj, nil)
	require.NoError(t, err)
	require.Empty(t, items)
}

func TestEvalForEach_NonListReturnsErr(t *testing.T) {
	env := mustEnv(t)
	prog, err := env.Compile("object.metadata.name")
	require.NoError(t, err)

	obj := map[string]any{"metadata": map[string]any{"name": "foo"}}
	_, err = policy.EvalForEach(prog, obj, nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, policy.ErrForEachNotList),
		"expected ErrForEachNotList, got %v", err)
}

func TestEvalValueWithItem_ResolvesItemField(t *testing.T) {
	env := mustItemEnv(t)
	prog, err := env.Compile("double(item.replicas)")
	require.NoError(t, err)

	obj := map[string]any{}
	item := map[string]any{"replicas": int64(3)}
	got, err := policy.EvalValueWithItem(prog, obj, item, nil)
	require.NoError(t, err)
	require.Equal(t, 3.0, got)
}

func TestEvalLabelWithItem_ResolvesItemField(t *testing.T) {
	env := mustItemEnv(t)
	prog, err := env.Compile("item.type")
	require.NoError(t, err)

	obj := map[string]any{}
	item := map[string]any{"type": "Ready"}
	got, err := policy.EvalLabelWithItem(prog, obj, item, nil)
	require.NoError(t, err)
	require.Equal(t, "Ready", got)
}

