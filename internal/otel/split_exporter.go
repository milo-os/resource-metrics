// SPDX-License-Identifier: AGPL-3.0-only

package otel

import (
	"context"
	"errors"
	"fmt"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/instrumentation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
)

// DefaultMaxExportRequestBytes is the export request budget used when
// ProviderOptions does not set one. It matches the gRPC default receive
// limit, so any receiver left at its defaults accepts every request.
const DefaultMaxExportRequestBytes = 4 << 20

// Protobuf overhead added to every size estimate. Each is an upper bound on
// field tags, length prefixes and fixed-width fields, so the estimate stays
// at or above the real encoded size.
const (
	messageOverhead    = 16
	attributeOverhead  = 16
	dataPointOverhead  = 64
	arrayElemOverhead  = 4
	numericValueLength = 10
)

// splittingExporter caps the encoded size of every export request. The
// OTLP exporter turns one Export call into one request, and the periodic
// reader hands it every data point of a collection cycle, so the request
// grows with the number of projects. This wrapper splits a cycle into
// requests that each fit maxBytes and sends all of them, even when some
// fail.
type splittingExporter struct {
	sdkmetric.Exporter
	maxBytes int
}

func newSplittingExporter(next sdkmetric.Exporter, maxBytes int) *splittingExporter {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxExportRequestBytes
	}
	return &splittingExporter{Exporter: next, maxBytes: maxBytes}
}

// Export sends rm as one or more requests, each estimated to encode to at
// most maxBytes. A failed request does not stop the ones after it; the
// returned error joins every failure.
func (e *splittingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	batches := splitResourceMetrics(rm, e.maxBytes)
	if len(batches) <= 1 {
		return e.Exporter.Export(ctx, rm)
	}

	var errs []error
	for i, batch := range batches {
		if err := e.Exporter.Export(ctx, batch); err != nil {
			errs = append(errs, fmt.Errorf("request %d of %d (%d data points): %w",
				i+1, len(batches), resourceDataPoints(batch), err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%d of %d metric export requests failed: %w",
			len(errs), len(batches), errors.Join(errs...))
	}
	return nil
}

type splitter struct {
	maxBytes     int
	resource     *resource.Resource
	resourceSize int

	batches   []*metricdata.ResourceMetrics
	cur       *metricdata.ResourceMetrics
	curSize   int
	curPoints int
}

// splitResourceMetrics partitions src into ResourceMetrics whose estimated
// encoded size fits maxBytes, keeping every data point exactly once and in
// order. A single data point larger than maxBytes travels alone. Metrics
// with no data points are dropped. src is not modified.
func splitResourceMetrics(src *metricdata.ResourceMetrics, maxBytes int) []*metricdata.ResourceMetrics {
	s := &splitter{
		maxBytes:     maxBytes,
		resource:     src.Resource,
		resourceSize: messageOverhead + resourceSize(src.Resource),
	}
	s.reset()

	for _, sm := range src.ScopeMetrics {
		scopeSize := messageOverhead + scopeSize(sm.Scope)
		var scope *metricdata.ScopeMetrics
		for _, m := range sm.Metrics {
			sizes := dataPointSizes(m.Data)
			header := messageOverhead + len(m.Name) + len(m.Description) + len(m.Unit)
			for start := 0; start < len(sizes); {
				need := header
				if scope == nil {
					need += scopeSize
				}
				end := start
				for end < len(sizes) {
					fits := s.curSize+need+sizes[end] <= s.maxBytes
					alone := s.curPoints == 0 && end == start
					if !fits && !alone {
						break
					}
					need += sizes[end]
					end++
				}
				if end == start {
					s.flush()
					scope = nil
					continue
				}
				if scope == nil {
					s.cur.ScopeMetrics = append(s.cur.ScopeMetrics, metricdata.ScopeMetrics{Scope: sm.Scope})
					scope = &s.cur.ScopeMetrics[len(s.cur.ScopeMetrics)-1]
				}
				scope.Metrics = append(scope.Metrics, withDataPoints(m, start, end))
				s.curSize += need
				s.curPoints += end - start
				start = end
				if start < len(sizes) {
					s.flush()
					scope = nil
				}
			}
		}
	}
	s.flush()
	return s.batches
}

func (s *splitter) reset() {
	s.cur = &metricdata.ResourceMetrics{Resource: s.resource}
	s.curSize = s.resourceSize
	s.curPoints = 0
}

func (s *splitter) flush() {
	if s.curPoints > 0 {
		s.batches = append(s.batches, s.cur)
	}
	s.reset()
}

// dataPointSizes returns the estimated encoded size of each data point that
// can be sent on its own. Aggregations this package cannot slice are
// returned as a single unit covering the whole metric.
func dataPointSizes(data metricdata.Aggregation) []int {
	switch d := data.(type) {
	case metricdata.Gauge[float64]:
		return numberPointSizes(d.DataPoints)
	case metricdata.Gauge[int64]:
		return numberPointSizes(d.DataPoints)
	case metricdata.Sum[float64]:
		return numberPointSizes(d.DataPoints)
	case metricdata.Sum[int64]:
		return numberPointSizes(d.DataPoints)
	case metricdata.Histogram[float64]:
		return histogramPointSizes(d.DataPoints)
	case metricdata.Histogram[int64]:
		return histogramPointSizes(d.DataPoints)
	case nil:
		return nil
	default:
		return []int{wholeMetricSize(d)}
	}
}

// withDataPoints returns a copy of m carrying only data points [start, end).
func withDataPoints(m metricdata.Metrics, start, end int) metricdata.Metrics {
	switch d := m.Data.(type) {
	case metricdata.Gauge[float64]:
		d.DataPoints = d.DataPoints[start:end:end]
		m.Data = d
	case metricdata.Gauge[int64]:
		d.DataPoints = d.DataPoints[start:end:end]
		m.Data = d
	case metricdata.Sum[float64]:
		d.DataPoints = d.DataPoints[start:end:end]
		m.Data = d
	case metricdata.Sum[int64]:
		d.DataPoints = d.DataPoints[start:end:end]
		m.Data = d
	case metricdata.Histogram[float64]:
		d.DataPoints = d.DataPoints[start:end:end]
		m.Data = d
	case metricdata.Histogram[int64]:
		d.DataPoints = d.DataPoints[start:end:end]
		m.Data = d
	}
	return m
}

func numberPointSizes[N int64 | float64](points []metricdata.DataPoint[N]) []int {
	sizes := make([]int, len(points))
	for i, p := range points {
		sizes[i] = dataPointOverhead + attributesSize(p.Attributes) + exemplarsSize(p.Exemplars)
	}
	return sizes
}

func histogramPointSizes[N int64 | float64](points []metricdata.HistogramDataPoint[N]) []int {
	sizes := make([]int, len(points))
	for i, p := range points {
		buckets := (len(p.Bounds) + len(p.BucketCounts)) * numericValueLength
		sizes[i] = dataPointOverhead + 3*numericValueLength + buckets +
			attributesSize(p.Attributes) + exemplarsSize(p.Exemplars)
	}
	return sizes
}

func exemplarsSize[N int64 | float64](exemplars []metricdata.Exemplar[N]) int {
	size := 0
	for _, e := range exemplars {
		size += dataPointOverhead + len(e.SpanID) + len(e.TraceID)
		for _, kv := range e.FilteredAttributes {
			size += keyValueSize(kv)
		}
	}
	return size
}

// wholeMetricSize bounds aggregations that are never split. It walks their
// data points through the same estimators so large ones are still counted.
func wholeMetricSize(data metricdata.Aggregation) int {
	size := 0
	switch d := data.(type) {
	case metricdata.ExponentialHistogram[float64]:
		for _, p := range d.DataPoints {
			buckets := (len(p.PositiveBucket.Counts) + len(p.NegativeBucket.Counts)) * numericValueLength
			size += 2*dataPointOverhead + buckets + attributesSize(p.Attributes) + exemplarsSize(p.Exemplars)
		}
	case metricdata.ExponentialHistogram[int64]:
		for _, p := range d.DataPoints {
			buckets := (len(p.PositiveBucket.Counts) + len(p.NegativeBucket.Counts)) * numericValueLength
			size += 2*dataPointOverhead + buckets + attributesSize(p.Attributes) + exemplarsSize(p.Exemplars)
		}
	case metricdata.Summary:
		for _, p := range d.DataPoints {
			size += dataPointOverhead + len(p.QuantileValues)*(messageOverhead+2*numericValueLength) +
				attributesSize(p.Attributes)
		}
	}
	return size
}

func resourceSize(r *resource.Resource) int {
	if r == nil {
		return 0
	}
	size := len(r.SchemaURL())
	if set := r.Set(); set != nil {
		size += attributesSize(*set)
	}
	return size
}

func scopeSize(s instrumentation.Scope) int {
	return len(s.Name) + len(s.Version) + len(s.SchemaURL) + attributesSize(s.Attributes)
}

func attributesSize(set attribute.Set) int {
	size := 0
	for iter := set.Iter(); iter.Next(); {
		size += keyValueSize(iter.Attribute())
	}
	return size
}

func keyValueSize(kv attribute.KeyValue) int {
	size := attributeOverhead + len(kv.Key)
	switch kv.Value.Type() {
	case attribute.STRING:
		size += len(kv.Value.AsString())
	case attribute.STRINGSLICE:
		for _, v := range kv.Value.AsStringSlice() {
			size += arrayElemOverhead + len(v)
		}
	case attribute.BOOLSLICE:
		size += len(kv.Value.AsBoolSlice()) * arrayElemOverhead
	case attribute.INT64SLICE:
		size += len(kv.Value.AsInt64Slice()) * (arrayElemOverhead + numericValueLength)
	case attribute.FLOAT64SLICE:
		size += len(kv.Value.AsFloat64Slice()) * (arrayElemOverhead + numericValueLength)
	default:
		size += numericValueLength
	}
	return size
}

func resourceDataPoints(rm *metricdata.ResourceMetrics) int {
	n := 0
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			n += len(dataPointSizes(m.Data))
		}
	}
	return n
}
