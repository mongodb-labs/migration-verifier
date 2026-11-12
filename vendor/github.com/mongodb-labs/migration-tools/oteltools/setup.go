// Package oteltools sets up OpenTelemetry metrics export for migration tools.
//
// Setup builds a metrics pipeline that serves Prometheus metrics over HTTP
// and, optionally, writes the same metrics to an io.Writer, such as a file or
// a pipe. Callers create instruments from the pipeline’s meter provider with
// the OpenTelemetry API.
//
// The OTel Prometheus exporter does all conversion from OTel to Prometheus.
// The writer gets the same registry that the HTTP handler serves, so the two
// always contain the same series with the same names.
//
// # Synopsis
//
// An application that counts compared documents, serves the count at
// /metrics, and also writes it to a file:
//
//		pipeline, err := oteltools.Setup(oteltools.Settings{
//			MetricsOut: option.Some[io.Writer](metricsFile),
//		})
//		if err != nil {
//			return fmt.Errorf("setup otel: %w", err)
//		}
//
//		http.Handle("/metrics", pipeline.Handler())
//
//		meter := pipeline.MeterProvider().Meter("my-tool")
//
//		// This should never actually fail, so a runtime assertion would also
//		// be appropriate here.
//		comparedDocs, err := meter.Int64Counter("compared_docs")
//		if err != nil {
//			return fmt.Errorf("setup compared_docs: %w", err)
//		}
//
//		comparedDocs.Add(ctx, 1)
//
//		// … and before exiting, which writes a last export:
//		if err := pipeline.Shutdown(ctx); err != nil {
//	     // Do *not* close the file if the shutdown failed.
//
//			slog.Warn("shutdown otel", "err", err)
//		} else {
//			// Successful shutdown
//
//			_ = metricsFile.Close()
//		}
//
// Both outputs then contain a counter named compared_docs_total. They also
// contain the Go runtime (go_*) and process (process_*) metrics that Setup
// always adds.
//
// # Export format
//
// The output is a sequence of exports. Each export is one Prometheus text
// exposition, the same series that the HTTP handler serves, except that every
// sample has a timestamp: the time the export was gathered, in Unix
// milliseconds. An EndOfExport line (“# EOF”) follows each export, such as:
//
//	# HELP go_goroutines Number of goroutines that currently exist.
//	# TYPE go_goroutines gauge
//	go_goroutines{service_name="svc"} 12 1762873562389
//	…
//	# EOF
//
// Prometheus text parsers read the EndOfExport lines as comments. Each export
// can go to a Prometheus text parser on its own. The whole output can go
// straight to an importer that reads Prometheus text line by line, such as
// VictoriaMetrics’ /api/v1/import/prometheus endpoint. (A strict parser
// rejects the whole output, because each export repeats the HELP and TYPE
// lines.) Because every sample has a timestamp, an importer stores each
// sample at the time it was gathered, not the time it was imported.
//
// Output after the last EndOfExport line is an export that was cut short, for
// example by a crash. Its final line may be incomplete.
//
// Nothing in this package alters global state. In particular, it never calls
// otel.SetMeterProvider or any other otel.Set* function, and it never
// registers with prometheus.DefaultRegisterer. Callers receive the meter
// provider and the HTTP handler and decide what to do with them.
//
// All logging goes through log/slog.
package oteltools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/mongodb-labs/migration-tools/option"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/otlptranslator"
	"go.opentelemetry.io/otel/attribute"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	otelmetric "go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/resource"
)

// DefaultExportInterval is how often metrics are written to Settings.MetricsOut
// unless Settings.ExportInterval says otherwise.
const DefaultExportInterval = 30 * time.Second

var errNonPositiveExportInterval = errors.New("export interval must be positive")

// Settings configures Setup.
type Settings struct {
	// Logger receives this package’s log messages. If nil, they are
	// discarded.
	Logger *slog.Logger

	// Resource describes the process (service name, host name, and so on).
	// If nil, the provider has an empty resource. Setup never reads
	// resource attributes from the environment.
	Resource *resource.Resource

	// ResourceLabels selects the resource attributes to put as labels on
	// every series. Use an allow-list such as attribute.NewAllowKeysFilter
	// so that unexpected resource attributes don’t become labels. If nil,
	// resource attributes appear only in the target_info series.
	ResourceLabels attribute.Filter

	// MetricsOut, if set, receives one export every ExportInterval, in the
	// format that the package documentation describes. Each export, with
	// its EndOfExport line, is written in a single Write call.
	//
	// If a Write fails, the pipeline writes nothing more, and
	// Pipeline.Shutdown returns that Write’s error.
	//
	// The pipeline never closes the writer. If Pipeline.Shutdown succeeds,
	// the pipeline writes nothing more, so the caller can close the writer
	// then. If Shutdown fails because its context ended, one Write may still
	// be in progress, or may begin just after Shutdown returns if the export
	// was about to write; no other export writes anything. Closing the writer
	// may unblock that Write, if the writer supports that (as os.File pipes
	// do).
	MetricsOut option.Option[io.Writer]

	// ExportInterval is how often metrics are written to MetricsOut.
	// If unset, DefaultExportInterval is used. Setup fails if it is set to
	// zero or a negative duration.
	ExportInterval option.Option[time.Duration]
}

// Pipeline is a running metrics pipeline.
type Pipeline struct {
	provider *sdkmetric.MeterProvider
	handler  http.Handler
	stream   option.Option[*streamExporter]

	shutdownOnce sync.Once
	shutdownErr  error
}

// MeterProvider returns the provider that creates the meters whose
// instruments feed the pipeline. It returns the API interface rather than the
// SDK type so that callers shut down through Pipeline.Shutdown, which also
// writes the last export to Settings.MetricsOut.
func (p *Pipeline) MeterProvider() otelmetric.MeterProvider {
	return p.provider
}

// Handler returns an HTTP handler that serves the metrics in Prometheus text
// format, typically at /metrics. It reads only a registry that Setup
// created, never the Prometheus default registry.
func (p *Pipeline) Handler() http.Handler {
	return p.handler
}

// Shutdown writes the last export to MetricsOut, if set, and shuts down the
// meter provider. Call it before the program exits. Later calls return the
// first call’s result.
//
// If ctx ends before the writes to MetricsOut finish, Shutdown stops waiting
// for them and returns an error that wraps the context’s error. The last
// export then writes nothing unless it has already reached its Write, which
// may finish after Shutdown returns. See Settings.MetricsOut for what that
// means for the writer.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	p.shutdownOnce.Do(func() {
		var errs []error

		// The final write gathers from the provider, so it must come before
		// the provider shuts down.
		if stream, has := p.stream.Get(); has {
			if err := stream.stopAndFlush(ctx); err != nil {
				errs = append(errs, fmt.Errorf("writing final metrics: %w", err))
			}
		}

		if err := p.provider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutting down meter provider: %w", err))
		}

		p.shutdownErr = errors.Join(errs...)
	})

	return p.shutdownErr
}

// Setup creates a meter provider whose only reader is the OTel Prometheus
// exporter, registered on a private registry. The returned pipeline’s
// Handler serves that registry, and if settings.MetricsOut is set, a
// goroutine writes the same registry to it every ExportInterval.
//
// The registry also has the Prometheus client’s Go runtime (go_*) and
// process (process_*) collectors, which prometheus.DefaultRegisterer would
// otherwise provide.
//
// Exemplars are disabled because neither output uses them, and the default
// filter costs a context lookup on every measurement.
func Setup(settings Settings) (*Pipeline, error) {
	if interval, has := settings.ExportInterval.Get(); has && interval <= 0 {
		return nil, fmt.Errorf("%w: %s", errNonPositiveExportInterval, interval)
	}

	logger := cmp.Or(settings.Logger, slog.New(slog.DiscardHandler))

	registry := prometheus.NewRegistry()

	res := cmp.Or(settings.Resource, resource.Empty())

	// The OTel Prometheus exporter adds resource labels only to its own
	// series, so give the runtime collectors’ series the same labels.
	labels, err := resourceLabels(res, settings.ResourceLabels)
	if err != nil {
		return nil, err
	}

	if err := registerRuntimeCollectors(
		prometheus.WrapRegistererWith(labels, registry),
	); err != nil {
		return nil, err
	}

	promOpts := []otelprometheus.Option{otelprometheus.WithRegisterer(registry)}
	if settings.ResourceLabels != nil {
		promOpts = append(
			promOpts,
			otelprometheus.WithResourceAsConstantLabels(settings.ResourceLabels),
		)
	}

	promExporter, err := otelprometheus.New(promOpts...)
	if err != nil {
		return nil, fmt.Errorf("creating Prometheus exporter: %w", err)
	}

	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithExemplarFilter(exemplar.AlwaysOffFilter),
		sdkmetric.WithReader(promExporter),
	)

	pipeline := &Pipeline{
		provider: provider,
		handler: promhttp.HandlerFor(
			registry,
			promhttp.HandlerOpts{
				ErrorLog:      slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
				ErrorHandling: promhttp.ContinueOnError,
			},
		),
	}

	if writer, has := settings.MetricsOut.Get(); has {
		interval := settings.ExportInterval.OrElse(DefaultExportInterval)

		pipeline.stream = option.Some(startStreamExporter(registry, writer, logger, interval))
	}

	return pipeline, nil
}

// resourceLabels returns the resource attributes that filter allows, as
// Prometheus labels named the way the OTel Prometheus exporter names them for
// WithResourceAsConstantLabels. If filter is nil, it returns no labels, just
// as the exporter then puts resource attributes only on target_info.
func resourceLabels(res *resource.Resource, filter attribute.Filter) (prometheus.Labels, error) {
	labels := prometheus.Labels{}
	if filter == nil {
		return labels, nil
	}

	// The OTel Prometheus exporter escapes label names (UTF8Allowed: false)
	// because Setup uses its default translation strategy, which escapes.
	namer := otlptranslator.LabelNamer{UTF8Allowed: false}

	allowed, _ := res.Set().Filter(filter)
	for _, kv := range allowed.ToSlice() {
		name, err := namer.Build(string(kv.Key))
		if err != nil {
			return nil, fmt.Errorf("naming label for resource attribute %#q: %w", kv.Key, err)
		}

		labels[name] = kv.Value.String()
	}

	return labels, nil
}

func registerRuntimeCollectors(registerer prometheus.Registerer) error {
	for _, c := range []prometheus.Collector{
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registerer.Register(c); err != nil {
			return fmt.Errorf("registering runtime collector: %w", err)
		}
	}

	return nil
}
