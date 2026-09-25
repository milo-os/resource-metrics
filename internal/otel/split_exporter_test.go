// SPDX-License-Identifier: AGPL-3.0-only

package otel

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	receiverMaxRecvBytes = 64 << 10
	testSeries           = 3000
)

// otlpReceiver is an in-process OTLP/gRPC metrics receiver that enforces a
// small max receive message size, the way an OpenTelemetry collector does.
type otlpReceiver struct {
	colmetricpb.UnimplementedMetricsServiceServer

	mu       sync.Mutex
	requests []*colmetricpb.ExportMetricsServiceRequest
}

func (r *otlpReceiver) Export(
	_ context.Context, req *colmetricpb.ExportMetricsServiceRequest,
) (*colmetricpb.ExportMetricsServiceResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, req)
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

func (r *otlpReceiver) received() []*colmetricpb.ExportMetricsServiceRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*colmetricpb.ExportMetricsServiceRequest(nil), r.requests...)
}

func startReceiver(t *testing.T, maxRecvBytes int) (*otlpReceiver, string) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := grpc.NewServer(grpc.MaxRecvMsgSize(maxRecvBytes))
	recv := &otlpReceiver{}
	colmetricpb.RegisterMetricsServiceServer(srv, recv)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return recv, lis.Addr().String()
}

// registerLargeGauge records testSeries gauge series whose attributes look
// like the per-project, per-resource series the operator emits in
// production, so one collection cycle encodes far above receiverMaxRecvBytes.
func registerLargeGauge(t *testing.T, mp *sdkmetric.MeterProvider) {
	t.Helper()
	meter := mp.Meter("resource-metrics-test")
	_, err := meter.Float64ObservableGauge(
		"datum_test_resource_info",
		metric.WithDescription("Synthetic per-project resource series."),
		metric.WithFloat64Callback(func(_ context.Context, o metric.Float64Observer) error {
			for i := range testSeries {
				o.Observe(1, metric.WithAttributes(
					attribute.String("milo.project.name", fmt.Sprintf("project-%05d", i)),
					attribute.String("resource.name", fmt.Sprintf("resource-%05d", i)),
					attribute.String("resource.uid", fmt.Sprintf("0b7c6f3e-9a1d-4f2b-8c5e-%012d", i)),
					attribute.String("resource.namespace", "default"),
				))
			}
			return nil
		}),
	)
	require.NoError(t, err)
}

func TestNewMeterProvider_SplitsExportUnderReceiverLimit(t *testing.T) {
	t.Parallel()

	recv, addr := startReceiver(t, receiverMaxRecvBytes)
	const budget = 16 << 10

	ctx := context.Background()
	mp, err := NewMeterProvider(ctx, ProviderOptions{
		Endpoint:              addr,
		Insecure:              true,
		CollectionInterval:    time.Hour,
		MaxExportRequestBytes: budget,
		ServiceName:           "resource-metrics",
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	registerLargeGauge(t, mp)

	require.NoError(t, mp.ForceFlush(ctx))

	requests := recv.received()
	require.Greater(t, len(requests), 1, "expected the cycle to be split into several requests")

	total := 0
	for i, req := range requests {
		require.LessOrEqualf(t, proto.Size(req), budget, "request %d exceeds the budget", i)
		for _, rm := range req.GetResourceMetrics() {
			require.NotEmpty(t, rm.GetResource().GetAttributes(), "every request must carry the resource")
			total += proto.Size(rm)
		}
	}
	require.Greater(t, total, receiverMaxRecvBytes, "payload must exceed the receiver limit to prove the split")

	seen := map[string]int{}
	for _, req := range requests {
		for _, rm := range req.GetResourceMetrics() {
			for _, sm := range rm.GetScopeMetrics() {
				for _, m := range sm.GetMetrics() {
					for _, dp := range m.GetGauge().GetDataPoints() {
						for _, kv := range dp.GetAttributes() {
							if kv.GetKey() == "milo.project.name" {
								seen[kv.GetValue().GetStringValue()]++
							}
						}
					}
				}
			}
		}
	}
	require.Len(t, seen, testSeries, "every series must arrive")
	for name, n := range seen {
		require.Equalf(t, 1, n, "series %s arrived %d times", name, n)
	}
}

// TestOTLPExport_GzipDoesNotBypassReceiveLimit shows why compression alone
// does not fix an oversized export: gRPC checks the receive limit against
// the decompressed message.
func TestOTLPExport_GzipDoesNotBypassReceiveLimit(t *testing.T) {
	t.Parallel()

	recv, addr := startReceiver(t, receiverMaxRecvBytes)
	ctx := context.Background()
	exp, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(addr),
		otlpmetricgrpc.WithInsecure(),
		otlpmetricgrpc.WithCompressor("gzip"),
		otlpmetricgrpc.WithRetry(otlpmetricgrpc.RetryConfig{Enabled: false}),
	)
	require.NoError(t, err)
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(
		sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(time.Hour)),
	))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	registerLargeGauge(t, mp)

	err = mp.ForceFlush(ctx)
	require.Error(t, err)
	require.Contains(t, err.Error(), "code = ResourceExhausted")
	require.Contains(t, err.Error(), "after decompression larger than max")
	require.Empty(t, recv.received())
}

type recordingExporter struct {
	sdkmetric.Exporter
	failOn  map[int]bool
	calls   int
	batches []*metricdata.ResourceMetrics
}

func (r *recordingExporter) Export(_ context.Context, rm *metricdata.ResourceMetrics) error {
	r.calls++
	if r.failOn[r.calls] {
		return fmt.Errorf("receiver rejected request %d", r.calls)
	}
	r.batches = append(r.batches, rm)
	return nil
}

func gaugePoints(prefix string, n int) []metricdata.DataPoint[float64] {
	points := make([]metricdata.DataPoint[float64], n)
	for i := range points {
		points[i] = metricdata.DataPoint[float64]{
			Attributes: attribute.NewSet(attribute.String("series", fmt.Sprintf("%s-%04d", prefix, i))),
			Value:      float64(i),
		}
	}
	return points
}

func sampleResourceMetrics() *metricdata.ResourceMetrics {
	return &metricdata.ResourceMetrics{
		Resource: resource.NewSchemaless(attribute.String("service.name", "resource-metrics")),
		ScopeMetrics: []metricdata.ScopeMetrics{
			{
				Scope: instrumentation.Scope{Name: "scope-a"},
				Metrics: []metricdata.Metrics{
					{Name: "gauge_a", Data: metricdata.Gauge[float64]{DataPoints: gaugePoints("a", 400)}},
					{Name: "empty", Data: metricdata.Gauge[float64]{}},
					{Name: "sum_b", Data: metricdata.Sum[int64]{
						Temporality: metricdata.CumulativeTemporality,
						IsMonotonic: true,
						DataPoints: []metricdata.DataPoint[int64]{
							{Attributes: attribute.NewSet(attribute.String("series", "b-0000")), Value: 7},
							{Attributes: attribute.NewSet(attribute.String("series", "b-0001")), Value: 8},
						},
					}},
				},
			},
			{
				Scope: instrumentation.Scope{Name: "scope-c"},
				Metrics: []metricdata.Metrics{
					{Name: "gauge_c", Data: metricdata.Gauge[float64]{DataPoints: gaugePoints("c", 250)}},
				},
			},
		},
	}
}

func seriesOf(batches []*metricdata.ResourceMetrics) []string {
	var out []string
	for _, rm := range batches {
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				switch d := m.Data.(type) {
				case metricdata.Gauge[float64]:
					for _, p := range d.DataPoints {
						v, _ := p.Attributes.Value("series")
						out = append(out, sm.Scope.Name+"/"+m.Name+"/"+v.AsString())
					}
				case metricdata.Sum[int64]:
					for _, p := range d.DataPoints {
						v, _ := p.Attributes.Value("series")
						out = append(out, sm.Scope.Name+"/"+m.Name+"/"+v.AsString())
					}
				}
			}
		}
	}
	return out
}

func estimatedSize(rm *metricdata.ResourceMetrics) int {
	size := messageOverhead + resourceSize(rm.Resource)
	for _, sm := range rm.ScopeMetrics {
		size += messageOverhead + scopeSize(sm.Scope)
		for _, m := range sm.Metrics {
			size += messageOverhead + len(m.Name) + len(m.Description) + len(m.Unit)
			for _, s := range dataPointSizes(m.Data) {
				size += s
			}
		}
	}
	return size
}

func TestSplitResourceMetrics_KeepsEveryPointInOrderUnderBudget(t *testing.T) {
	t.Parallel()

	src := sampleResourceMetrics()
	want := seriesOf([]*metricdata.ResourceMetrics{src})
	const budget = 2 << 10

	batches := splitResourceMetrics(src, budget)
	require.Greater(t, len(batches), 1)
	require.Equal(t, want, seriesOf(batches))
	for i, b := range batches {
		require.LessOrEqualf(t, estimatedSize(b), budget, "batch %d over budget", i)
		require.Same(t, src.Resource, b.Resource)
	}
	require.Len(t, src.ScopeMetrics[0].Metrics[0].Data.(metricdata.Gauge[float64]).DataPoints, 400,
		"source must not be modified")
}

func TestSplitResourceMetrics_OversizedPointTravelsAlone(t *testing.T) {
	t.Parallel()

	big := strings.Repeat("x", 4096)
	src := &metricdata.ResourceMetrics{
		ScopeMetrics: []metricdata.ScopeMetrics{{
			Metrics: []metricdata.Metrics{{
				Name: "g",
				Data: metricdata.Gauge[float64]{DataPoints: []metricdata.DataPoint[float64]{
					{Attributes: attribute.NewSet(attribute.String("series", "small-0"))},
					{Attributes: attribute.NewSet(attribute.String("series", big))},
					{Attributes: attribute.NewSet(attribute.String("series", "small-1"))},
				}},
			}},
		}},
	}

	batches := splitResourceMetrics(src, 1024)
	require.Len(t, batches, 3)
	for _, b := range batches {
		require.Equal(t, 1, resourceDataPoints(b))
	}
}

func TestSplittingExporter_FailedRequestDoesNotDropOthers(t *testing.T) {
	t.Parallel()

	src := sampleResourceMetrics()
	all := seriesOf([]*metricdata.ResourceMetrics{src})
	wantBatches := splitResourceMetrics(src, 2<<10)
	require.Greater(t, len(wantBatches), 2)

	rec := &recordingExporter{failOn: map[int]bool{2: true}}
	err := newSplittingExporter(rec, 2<<10).Export(context.Background(), src)

	require.Error(t, err)
	require.Contains(t, err.Error(), fmt.Sprintf("1 of %d metric export requests failed", len(wantBatches)))
	require.Contains(t, err.Error(), "receiver rejected request 2")
	require.Equal(t, len(wantBatches), rec.calls, "every request must be attempted")

	lost := seriesOf(wantBatches[1:2])
	require.Equal(t, len(all)-len(lost), len(seriesOf(rec.batches)))
}

func TestSplittingExporter_SmallExportIsUnchanged(t *testing.T) {
	t.Parallel()

	src := sampleResourceMetrics()
	rec := &recordingExporter{}
	require.NoError(t, newSplittingExporter(rec, 0).Export(context.Background(), src))
	require.Len(t, rec.batches, 1)
	require.Same(t, src, rec.batches[0])
}
