// SPDX-License-Identifier: AGPL-3.0-only

package collector

import (
	"context"
	"testing"
	"time"
)

const (
	// pollTimeout bounds how long requireCondition waits for cond to go
	// true. 5s matches the slowest informer / cache-sync helper in this
	// package.
	pollTimeout = 5 * time.Second
	// cleanupTimeout bounds Stop()-style cleanup contexts built by the
	// tests. Kept snug so a hung Stop() fails fast.
	cleanupTimeout = 5 * time.Second
)

// requireCondition polls cond every 25ms until it returns true or
// pollTimeout elapses. On timeout it fails the test with the supplied
// message.
func requireCondition(t *testing.T, cond func() bool, format string, args ...any) {
	t.Helper()
	deadline := time.Now().Add(pollTimeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf(format, args...)
}

// cleanupContext is a small shim so tests can keep their cleanup
// sequences short.
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), cleanupTimeout)
}
