// SPDX-License-Identifier: AGPL-3.0-only

package policy

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	"github.com/google/cel-go/ext"
	"github.com/google/cel-go/interpreter"
)

// Evaluation deadlines and resource limits for user-supplied CEL expressions.
const (
	evalTimeout = 50 * time.Millisecond
	evalCost    = uint64(100_000_000)

	// DefaultLabelValueMaxBytes bounds the byte length of a single label value
	// after CEL coercion. Prometheus itself allows much larger values, but
	// policies that emit unbounded strings (annotations, free-form user
	// input) would otherwise balloon scrape memory.
	DefaultLabelValueMaxBytes = 1024
)

// Sentinel errors exposed so callers can distinguish evaluation outcomes.
var (
	ErrEvalPanic            = errors.New("cel: evaluator panicked")
	ErrEvalCostExceeded     = errors.New("cel: cost limit exceeded")
	ErrEvalTimeout          = errors.New("cel: evaluation timed out")
	ErrValueNotNumeric      = errors.New("cel: value expression did not produce a numeric result")
	ErrLabelNotConvertible  = errors.New("cel: label expression could not be converted to string")
	ErrCycleBudgetExceeded  = errors.New("cel: per-cycle evaluation budget exceeded")
	ErrLabelTooLong         = errors.New("cel: label value exceeded the configured length limit")
	ErrLabelHasControlChars = errors.New("cel: label value contained control characters")
)

// OnLabelSanitized is called once per label value that had at least one
// control character replaced or was truncated to fit the length limit. The
// production binary sets this to increment a Prometheus counter; tests
// leave it at nil so they do not mutate package-level counters.
//
// Assigning this variable is not safe to do concurrently with evaluation;
// callers set it once at process startup.
var OnLabelSanitized func()

// CycleBudget bounds the total cost of CEL evaluations within a single
// collection cycle (one OTel callback invocation). Unlike the per-expression
// cost and deadline that CEL enforces itself, CycleBudget caps the
// cross-expression fan-out: N_objects x M_metrics x K_labels evaluations
// can starve the scrape goroutine even when each individual expression is
// cheap.
//
// A nil *CycleBudget disables budget enforcement. The zero value is not
// useful — construct instances with NewCycleBudget.
//
// CycleBudget is safe for concurrent use: Consume uses atomic decrement and
// a single pre-computed deadline, so multiple goroutines can share one
// budget across parallel evaluations within the same cycle.
type CycleBudget struct {
	deadline  time.Time
	remaining atomic.Int64
}

// NewCycleBudget returns a budget that permits at most maxEvals Consume
// calls and whose deadline expires d from now. A non-positive maxEvals or
// a non-positive d yields an immediately-exhausted budget so that callers
// that misconfigure their limits fail closed instead of silently disabling
// enforcement.
func NewCycleBudget(d time.Duration, maxEvals int64) *CycleBudget {
	b := &CycleBudget{}
	if d > 0 {
		b.deadline = time.Now().Add(d)
	} else {
		// Already past.
		b.deadline = time.Now().Add(-time.Nanosecond)
	}
	if maxEvals > 0 {
		b.remaining.Store(maxEvals)
	}
	return b
}

// Consume decrements the remaining budget by one. It returns
// ErrCycleBudgetExceeded if the budget is exhausted (either because no
// evaluations remain or because the deadline has passed). A nil receiver
// returns nil so callers can unconditionally call Consume without a
// nil-check.
func (b *CycleBudget) Consume() error {
	if b == nil {
		return nil
	}
	// Deadline check first: a slow cycle with plenty of budget remaining
	// should still fail fast once the wall-clock limit is hit.
	if !b.deadline.IsZero() && time.Now().After(b.deadline) {
		return fmt.Errorf("%w: deadline exceeded", ErrCycleBudgetExceeded)
	}
	// atomic decrement; if we go non-negative we had budget; otherwise
	// another goroutine raced us past zero.
	if b.remaining.Add(-1) < 0 {
		return fmt.Errorf("%w: evaluation count exhausted", ErrCycleBudgetExceeded)
	}
	return nil
}

// Remaining returns the number of evaluations still available. Exposed for
// tests and operator diagnostics; callers must not treat this as a
// consumption check — use Consume instead.
func (b *CycleBudget) Remaining() int64 {
	if b == nil {
		return 0
	}
	return b.remaining.Load()
}

// OnEvalPanic is called exactly once per recovered evaluator panic. The
// production binary sets this to increment a Prometheus counter; tests
// leave it at nil so they do not mutate package-level counters.
//
// Assigning this variable is not safe to do concurrently with evaluation;
// callers set it once at process startup.
var OnEvalPanic func()

// EvalTypeError is returned when a value expression produces a result of an
// unsupported type.
type EvalTypeError struct {
	Type string
}

func (e *EvalTypeError) Error() string {
	return fmt.Sprintf("cel: unsupported value type %q", e.Type)
}

func (e *EvalTypeError) Is(target error) bool {
	return target == ErrValueNotNumeric
}

// Env wraps a *cel.Env configured for ResourceMetricsPolicy expressions. It is
// created once per process — the underlying cel.Env is safe to use from
// multiple goroutines.
type Env struct {
	env *cel.Env
}

// NewEnv builds the CEL environment with the extensions used by
// ResourceMetricsPolicy expressions. Not enabled: Sets, Protos, NativeTypes.
func NewEnv() (*Env, error) {
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		ext.Strings(),
		ext.Encoders(),
		ext.Math(),
		ext.Lists(),
	)
	if err != nil {
		return nil, fmt.Errorf("cel: new env: %w", err)
	}
	return &Env{env: env}, nil
}

// Compile parses, type-checks, and programs the given expression. The returned
// Program has a runtime cost limit applied so pathological comprehensions will
// be cancelled mid-evaluation rather than pegging a core.
func (e *Env) Compile(expr string) (cel.Program, error) {
	ast, issues := e.env.Parse(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("cel: parse: %w", issues.Err())
	}
	checked, issues := e.env.Check(ast)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("cel: check: %w", issues.Err())
	}
	prog, err := e.env.Program(checked,
		cel.CostLimit(evalCost),
		// Check ctx.Done() every 100 comprehension iterations so a pathological
		// expression hits our 50ms deadline instead of running for seconds.
		cel.InterruptCheckFrequency(64),
	)
	if err != nil {
		return nil, fmt.Errorf("cel: program: %w", err)
	}
	return prog, nil
}

// evalWithRecover runs prog with a bounded context and translates panics into
// ErrEvalPanic. Returns the raw ref.Val on success; callers coerce as needed.
func evalWithRecover(prog cel.Program, object map[string]any) (out ref.Val, err error) {
	if prog == nil {
		return nil, errors.New("cel: nil program")
	}
	defer func() {
		if r := recover(); r != nil {
			out = nil
			err = fmt.Errorf("%w: %v", ErrEvalPanic, r)
			if OnEvalPanic != nil {
				OnEvalPanic()
			}
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), evalTimeout)
	defer cancel()
	val, _, evalErr := prog.ContextEval(ctx, map[string]any{"object": object})
	if evalErr != nil {
		return nil, classifyEvalError(ctx, evalErr)
	}
	return val, nil
}

// classifyEvalError maps a cel-go evaluation error onto one of our sentinels
// where we can, otherwise returns the original error.
func classifyEvalError(ctx context.Context, err error) error {
	// cel-go surfaces cost-limit cancellation as an EvalCancelledError with a
	// CostLimitExceeded cause.
	var cancelled interpreter.EvalCancelledError
	if errors.As(err, &cancelled) {
		if cancelled.Cause == interpreter.CostLimitExceeded {
			return fmt.Errorf("%w: %v", ErrEvalCostExceeded, err)
		}
		// Context deadline or explicit cancel.
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%w: %v", ErrEvalTimeout, err)
		}
	}
	// Belt-and-suspenders: string-match cost-limit wording in case cel-go
	// wrapping changes shape.
	if strings.Contains(err.Error(), "actual cost limit exceeded") ||
		strings.Contains(err.Error(), "cost limit exceeded") {
		return fmt.Errorf("%w: %v", ErrEvalCostExceeded, err)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("%w: %v", ErrEvalTimeout, err)
	}
	return err
}

// EvalValue evaluates a CEL expression expected to produce a numeric metric
// value and coerces the result to float64. Bools become 0/1 and parseable
// strings are accepted. A nil program is a hard error — call sites should
// treat nil as constant-1.0 before calling here.
//
// If budget is non-nil, it is consumed exactly once before evaluation. A
// nil budget disables per-cycle budget enforcement so existing callers do
// not need to thread one through.
func EvalValue(prog cel.Program, object map[string]any, budget *CycleBudget) (float64, error) {
	if err := budget.Consume(); err != nil {
		return 0, err
	}
	val, err := evalWithRecover(prog, object)
	if err != nil {
		return 0, err
	}
	if val == nil {
		return 0, &EvalTypeError{Type: "nil"}
	}
	switch v := val.(type) {
	case types.Double:
		return float64(v), nil
	case types.Int:
		return float64(v), nil
	case types.Uint:
		return float64(v), nil
	case types.Bool:
		if bool(v) {
			return 1.0, nil
		}
		return 0.0, nil
	case types.String:
		f, perr := strconv.ParseFloat(string(v), 64)
		if perr != nil {
			return 0, fmt.Errorf("%w: %q is not a number", ErrValueNotNumeric, string(v))
		}
		return f, nil
	}
	// Null and anything else we haven't handled.
	return 0, &EvalTypeError{Type: fmt.Sprintf("%T", val)}
}

// EvalLabel evaluates a CEL expression expected to produce a label value and
// coerces the result to string. The returned string is sanitized: C0 control
// characters (\x00..\x1f) and DEL (\x7f) are replaced with a single space,
// and the result is truncated to DefaultLabelValueMaxBytes bytes.
//
// Sanitization is silent: a noisy annotation is intentionally not allowed to
// drop the entire series. Operators get visibility via the package-level
// OnLabelSanitized hook, wired to resource_metrics_label_sanitized_total in
// main.
//
// If budget is non-nil, it is consumed exactly once before evaluation. A
// nil budget disables per-cycle budget enforcement so existing callers do
// not need to thread one through.
func EvalLabel(prog cel.Program, object map[string]any, budget *CycleBudget) (string, error) {
	if err := budget.Consume(); err != nil {
		return "", err
	}
	val, err := evalWithRecover(prog, object)
	if err != nil {
		return "", err
	}
	raw, err := coerceLabelString(val)
	if err != nil {
		return "", err
	}
	return sanitizeLabelValue(raw, DefaultLabelValueMaxBytes), nil
}

// coerceLabelString performs only the CEL->string conversion. Sanitization
// is done afterwards by sanitizeLabelValue.
func coerceLabelString(val ref.Val) (string, error) {
	if val == nil {
		return "", nil
	}
	if s, ok := val.(types.String); ok {
		return string(s), nil
	}
	if _, ok := val.(types.Null); ok {
		return "", nil
	}
	// Numeric / bool / anything with a ConvertToType implementation.
	converted := val.ConvertToType(types.StringType)
	if err, ok := converted.(*types.Err); ok {
		return "", fmt.Errorf("%w: %v", ErrLabelNotConvertible, err)
	}
	raw := converted.Value()
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%w: got %T", ErrLabelNotConvertible, raw)
	}
	return s, nil
}

// sanitizeLabelValue replaces every C0 control byte (0x00..0x1f) and DEL
// (0x7f) in s with a single space, then truncates the result to max bytes.
// It returns the cleaned string. If any substitution or truncation occurs,
// OnLabelSanitized is invoked once (not per-character).
//
// Note that truncation operates on bytes: we round down to the previous
// UTF-8 boundary so we never emit an invalid code point at the tail.
// Downstream Prometheus exposition is UTF-8 sensitive and a split
// multi-byte rune would break scrapers.
func sanitizeLabelValue(s string, max int) string {
	if max <= 0 {
		max = DefaultLabelValueMaxBytes
	}

	changed := false

	// Fast path: scan without allocating when nothing needs sanitization.
	needsSanitize := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c == 0x7f {
			needsSanitize = true
			break
		}
	}

	cleaned := s
	if needsSanitize {
		b := make([]byte, len(s))
		for i := 0; i < len(s); i++ {
			c := s[i]
			if c < 0x20 || c == 0x7f {
				b[i] = ' '
			} else {
				b[i] = c
			}
		}
		cleaned = string(b)
		changed = true
	}

	if len(cleaned) > max {
		// Truncate to max bytes but step back to a UTF-8 boundary so we
		// never leave a split multi-byte rune dangling at the tail.
		end := max
		for end > 0 && (cleaned[end]&0xC0) == 0x80 {
			end--
		}
		cleaned = cleaned[:end]
		changed = true
	}

	if changed && OnLabelSanitized != nil {
		OnLabelSanitized()
	}
	return cleaned
}
