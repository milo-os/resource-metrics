// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/record"
	crleaderelection "sigs.k8s.io/controller-runtime/pkg/leaderelection"
)

const testLeaseName = "resourcemetrics.miloapis.com"

// slowServerTiming is the timing a deployment sets for an API server with a
// slow tail of Lease writes: each lease request may then take 20 s.
var slowServerTiming = []string{
	"--leader-elect-lease-duration=60s", "--leader-elect-renew-deadline=40s", "--leader-elect-retry-period=5s",
}

func TestManagerOptions_LeaderElection(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	lease := bindLeaseFlags(fs)
	require.NoError(t, fs.Parse(nil))

	opts := managerOptions("0", ":8081", true, lease)

	assert.True(t, opts.LeaderElectionReleaseOnCancel, "a leader that stops keeps its lease until it expires")
	require.NotNil(t, opts.LeaseDuration, "the lease timing is not passed to the manager")
	require.NotNil(t, opts.RenewDeadline)
	require.NotNil(t, opts.RetryPeriod)
	// Unset, the timing is controller-runtime's own, so a deployment that sets
	// nothing keeps the lease it had.
	assert.Equal(t, 15*time.Second, *opts.LeaseDuration)
	assert.Equal(t, 10*time.Second, *opts.RenewDeadline)
	assert.Equal(t, 2*time.Second, *opts.RetryPeriod)

	require.NoError(t, fs.Parse(slowServerTiming))
	opts = managerOptions("0", ":8081", true, lease)
	assert.Equal(t, 60*time.Second, *opts.LeaseDuration, "the flag does not reach the manager")
	assert.Equal(t, 40*time.Second, *opts.RenewDeadline, "the flag does not reach the manager")
	assert.Equal(t, 5*time.Second, *opts.RetryPeriod, "the flag does not reach the manager")
}

// leaseServer serves one coordination.k8s.io/v1 Lease and answers each
// renewal (PUT) after writeDelay, as an API server under load does. A write
// whose client gave up before the answer is not applied.
type leaseServer struct {
	writeDelay time.Duration

	mu       sync.Mutex
	lease    *coordinationv1.Lease
	renewals int
}

func (s *leaseServer) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	switch req.Method {
	case http.MethodGet:
		s.get(w)
	case http.MethodPost:
		s.write(w, req, http.StatusCreated)
	case http.MethodPut:
		select {
		case <-time.After(s.writeDelay):
			s.write(w, req, http.StatusOK)
		case <-req.Context().Done():
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *leaseServer) get(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if s.lease == nil {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`))
		return
	}
	_ = json.NewEncoder(w).Encode(s.lease)
}

func (s *leaseServer) write(w http.ResponseWriter, req *http.Request, code int) {
	// The client may send protobuf; the universal deserializer reads either.
	var lease coordinationv1.Lease
	body, err := io.ReadAll(req.Body)
	if err == nil {
		_, _, err = clientgoscheme.Codecs.UniversalDeserializer().Decode(body, nil, &lease)
	}
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if code == http.StatusOK {
		s.renewals++
	}
	rv, _ := strconv.Atoi(lease.ResourceVersion)
	lease.ResourceVersion = strconv.Itoa(rv + 1)
	lease.Kind, lease.APIVersion = "Lease", "coordination.k8s.io/v1"
	s.lease = &lease
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&lease)
}

func (s *leaseServer) renewed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.renewals
}

type discardEvents struct{}

func (discardEvents) GetEventRecorderFor(string) record.EventRecorder { return &record.FakeRecorder{} }

// holdLease leads against srv with the given timing, through the lock the
// manager builds, until it has renewed `want` times or lost the lease.
// It reports whether it was still leading.
func holdLease(t *testing.T, srv *leaseServer, opts leaderelection.LeaderElectionConfig, want int) bool {
	t.Helper()
	host := httptest.NewServer(srv)
	defer host.Close()
	lock, err := crleaderelection.NewResourceLock(
		&rest.Config{Host: host.URL, QPS: -1}, discardEvents{},
		crleaderelection.Options{
			LeaderElection: true, LeaderElectionID: testLeaseName, LeaderElectionNamespace: "milo-system",
			RenewDeadline: opts.RenewDeadline,
		})
	require.NoError(t, err)

	lost := make(chan struct{})
	opts.Lock = lock
	opts.Callbacks = leaderelection.LeaderCallbacks{
		OnStartedLeading: func(context.Context) {},
		OnStoppedLeading: func() { close(lost) },
	}
	elector, err := leaderelection.NewLeaderElector(opts)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	go elector.Run(ctx)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-lost:
			return false
		case <-ctx.Done():
			t.Fatalf("neither %d renewals nor a lost lease within the test's time", want)
		case <-tick.C:
			if srv.renewed() >= want {
				return true
			}
		}
	}
}

// The Milo API server answers a slow tail of Lease writes. Each lease request
// times out at half the renew deadline, so with the default timing (15 s, 10 s,
// 2 s) a write slower than 5 s loses the lease; the timing a deployment sets
// through the flags holds it through writes up to 20 s.
func TestLeaseTiming_HoldsThroughSlowLeaseWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for real lease renewals")
	}
	timing := func(args []string) leaderelection.LeaderElectionConfig {
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		lease := bindLeaseFlags(fs)
		require.NoError(t, fs.Parse(args))
		opts := managerOptions("0", ":8081", true, lease)
		require.NotNil(t, opts.LeaseDuration, "the lease timing is not passed to the manager")
		require.NotNil(t, opts.RenewDeadline, "the lease timing is not passed to the manager")
		require.NotNil(t, opts.RetryPeriod, "the lease timing is not passed to the manager")
		return leaderelection.LeaderElectionConfig{
			LeaseDuration: *opts.LeaseDuration, RenewDeadline: *opts.RenewDeadline, RetryPeriod: *opts.RetryPeriod,
		}
	}
	timings := map[string]leaderelection.LeaderElectionConfig{
		"the defaults":            timing(nil),
		"the timing set by flags": timing(slowServerTiming),
	}
	for name, held := range map[string]bool{"the defaults": false, "the timing set by flags": true} {
		for _, delay := range []time.Duration{6 * time.Second, 12 * time.Second} {
			t.Run(strings.ReplaceAll(name, " ", "_")+"/write_"+delay.String(), func(t *testing.T) {
				t.Parallel()
				got := holdLease(t, &leaseServer{writeDelay: delay}, timings[name], 2)
				if held {
					assert.True(t, got, "the lease was lost to %s writes", delay)
				} else {
					assert.False(t, got, "the defaults held through %s writes, so this test cannot tell timings apart", delay)
				}
			})
		}
	}
}
