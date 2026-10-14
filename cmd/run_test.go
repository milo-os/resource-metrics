// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func blockUntilCanceled(stopped chan<- struct{}) func(context.Context) error {
	return func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)
		return ctx.Err()
	}
}

func runWithTimeout(t *testing.T, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after the manager stopped")
		return nil
	}
}

func stopped(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestRunStopsWorkersWhenManagerReturnsCleanly(t *testing.T) {
	discoveryStopped := make(chan struct{})
	providerStopped := make(chan struct{})

	err := runWithTimeout(t, func() error {
		return runUntilManagerStops(context.Background(),
			func(context.Context) error { return nil },
			blockUntilCanceled(discoveryStopped),
			blockUntilCanceled(providerStopped),
		)
	})

	require.NoError(t, err)
	assert.True(t, stopped(discoveryStopped), "discovery was still running after the manager returned")
	assert.True(t, stopped(providerStopped), "the provider was still running after the manager returned")
}

func TestRunReturnsManagerError(t *testing.T) {
	lost := errors.New("leader election lost")

	err := runWithTimeout(t, func() error {
		return runUntilManagerStops(context.Background(),
			func(context.Context) error { return lost },
			blockUntilCanceled(make(chan struct{})),
		)
	})

	assert.ErrorIs(t, err, lost)
}

func TestRunStopsManagerWhenWorkerFails(t *testing.T) {
	managerStopped := make(chan struct{})
	failed := errors.New("discovery failed")

	err := runWithTimeout(t, func() error {
		return runUntilManagerStops(context.Background(),
			blockUntilCanceled(managerStopped),
			func(context.Context) error { return failed },
		)
	})

	assert.ErrorIs(t, err, failed)
	assert.True(t, stopped(managerStopped), "the manager was not stopped after a worker failed")
}

func TestRunStopsEverythingOnSignal(t *testing.T) {
	managerStopped := make(chan struct{})
	workerStopped := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runWithTimeout(t, func() error {
		return runUntilManagerStops(ctx, blockUntilCanceled(managerStopped), blockUntilCanceled(workerStopped))
	})

	require.NoError(t, err, "a shutdown signal is not an error")
	assert.True(t, stopped(managerStopped))
	assert.True(t, stopped(workerStopped))
}

type countingShutdown struct {
	calls   atomic.Int32
	bounded atomic.Bool
	order   *[]string
	name    string
}

func (s *countingShutdown) Shutdown(ctx context.Context) error {
	s.calls.Add(1)
	*s.order = append(*s.order, s.name)
	_, ok := ctx.Deadline()
	s.bounded.Store(ok)
	return nil
}

// A lost lease still drains the OTel pipeline, runtime before meter provider,
// within a deadline, before the process exits.
func TestRunDrainsOtelWhenManagerStops(t *testing.T) {
	var order []string
	runtime := &countingShutdown{order: &order, name: "runtime"}
	meterProvider := &countingShutdown{order: &order, name: "meter provider"}
	lost := errors.New("leader election lost")

	err := runWithTimeout(t, func() error {
		return runUntilManagerStops(context.Background(),
			func(context.Context) error { return lost },
			shutdownOnStop(runtime, meterProvider),
		)
	})

	assert.ErrorIs(t, err, lost)
	assert.Equal(t, []string{"runtime", "meter provider"}, order)
	assert.Equal(t, int32(1), runtime.calls.Load())
	assert.Equal(t, int32(1), meterProvider.calls.Load())
	assert.True(t, runtime.bounded.Load() && meterProvider.bounded.Load(), "the drain has no deadline")
}
