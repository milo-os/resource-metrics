// SPDX-License-Identifier: AGPL-3.0-only

// Package otel contains the OpenTelemetry pipeline used by the
// resource-metrics operator to export per-project resource metrics over
// OTLP/gRPC.
//
// This file is intentionally narrow: it owns MeterProvider construction
// and nothing else. Instrument creation, callback wiring, and registry
// reconciliation live in runtime.go.
package otel

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
)

// defaultCollectionInterval is the interval used when ProviderOptions does
// not specify one. OTel's own default is 60s; we prefer 30s so that gauges
// track the informer-based source of truth closely enough to be useful for
// alerting without overwhelming the collector.
const defaultCollectionInterval = 30 * time.Second

// ProviderOptions configures the OTel MeterProvider created by
// NewMeterProvider. Zero values are acceptable for every field; sensible
// defaults are filled in below.
type ProviderOptions struct {
	// Endpoint is the OTLP/gRPC endpoint (host:port). If empty the OTel SDK
	// default (localhost:4317) is used via the exporter's built-in default.
	Endpoint string

	// Insecure disables TLS on the exporter connection. TLS is on by
	// default; this flag exists for test collectors and local bringup.
	Insecure bool

	// Headers are static OTLP headers, typically used for routing or
	// authentication tokens.
	Headers map[string]string

	// CollectionInterval controls the PeriodicReader interval. Zero means
	// defaultCollectionInterval.
	CollectionInterval time.Duration

	// ServiceName, ServiceVersion, and ServiceInstanceID are merged into
	// the resource attribute set as service.* semconv attributes. Any
	// that are empty are simply omitted.
	ServiceName       string
	ServiceVersion    string
	ServiceInstanceID string

	// ResourceAttributes are additional resource attributes merged into
	// the MeterProvider resource. Callers use this to plumb operator-wide
	// deployment metadata (cluster, environment, etc.).
	ResourceAttributes []attribute.KeyValue
}

// NewMeterProvider builds a MeterProvider that exports via OTLP/gRPC on a
// PeriodicReader. The caller owns the returned MeterProvider and is
// responsible for calling Shutdown on it during process teardown.
//
// The exporter uses TLS by default; pass Insecure: true to disable TLS for
// local bring-up or in-cluster plaintext collectors.
func NewMeterProvider(ctx context.Context, opts ProviderOptions) (*sdkmetric.MeterProvider, error) {
	// Build the resource attribute set. We merge any caller-provided
	// attributes with the canonical service.* attributes from the OTel
	// v1.27.0 semconv package.
	attrs := make([]attribute.KeyValue, 0, len(opts.ResourceAttributes)+3)
	if opts.ServiceName != "" {
		attrs = append(attrs, semconv.ServiceName(opts.ServiceName))
	}
	if opts.ServiceVersion != "" {
		attrs = append(attrs, semconv.ServiceVersion(opts.ServiceVersion))
	}
	if opts.ServiceInstanceID != "" {
		attrs = append(attrs, semconv.ServiceInstanceID(opts.ServiceInstanceID))
	}
	attrs = append(attrs, opts.ResourceAttributes...)

	res, err := resource.New(ctx,
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(attrs...),
	)
	if err != nil {
		// resource.New returns a partial Resource together with an error
		// when some detectors fail. We prefer to preserve the partial
		// resource rather than fail hard on a non-fatal detector miss, but
		// if there is no resource at all (res == nil) we must fail.
		if res == nil {
			return nil, fmt.Errorf("otel: build resource: %w", err)
		}
		// Otherwise fall through with the partial resource.
	}

	// Build the OTLP/gRPC exporter. TLS is on by default; WithInsecure()
	// is only applied when the caller asked for plaintext.
	exporterOpts := []otlpmetricgrpc.Option{}
	if opts.Endpoint != "" {
		exporterOpts = append(exporterOpts, otlpmetricgrpc.WithEndpoint(opts.Endpoint))
	}
	if len(opts.Headers) > 0 {
		exporterOpts = append(exporterOpts, otlpmetricgrpc.WithHeaders(opts.Headers))
	}
	if opts.Insecure {
		exporterOpts = append(exporterOpts, otlpmetricgrpc.WithInsecure())
	}

	exporter, err := otlpmetricgrpc.New(ctx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("otel: build otlp grpc exporter: %w", err)
	}

	interval := opts.CollectionInterval
	if interval <= 0 {
		interval = defaultCollectionInterval
	}

	reader := sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(interval))

	return sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	), nil
}
