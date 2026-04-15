// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"context"
	"testing"
	"time"
)

// requireCondition polls cond every 25ms until it returns true or timeout
// elapses. On timeout it fails the test with the supplied message.
func requireCondition(t *testing.T, timeout time.Duration, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf(format, args...)
}

// contextWithTimeout is a small shim so tests can keep their cleanup
// sequences short.
func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}
