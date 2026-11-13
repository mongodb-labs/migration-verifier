// Package metrics sets up migration-verifier’s OpenTelemetry metrics.
//
// The metrics are diagnostic output for MongoDB’s own troubleshooting, not a
// supported interface. Any metric, label, unit, or name, as well as the
// metrics file’s format, may change or disappear in any release without
// notice.
package metrics

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/10gen/migration-verifier/buildvar"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/mongodb-labs/migration-tools/oteltools"
	slogzerolog "github.com/samber/slog-zerolog/v2"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	// ServiceName is the service.name resource attribute.
	ServiceName = "migration-verifier"

	// FileName is the name of the metrics file within its directory.
	FileName = "migration_verifier_metrics.log"

	// fileMaxSizeMB is the size at which the metrics file is rotated. It
	// matches mongosync’s metrics file.
	fileMaxSizeMB = 200
)

// Settings configures Setup.
type Settings struct {
	// FileDir is the directory for the metrics file. It must already exist.
	// If unset, no metrics file is written.
	FileDir option.Option[string]
}

// Pipeline is the verifier’s metrics pipeline. It owns the metrics file, if
// any, which the underlying oteltools.Pipeline doesn’t close.
type Pipeline struct {
	*oteltools.Pipeline

	file option.Option[io.Closer]
}

// Shutdown shuts down the underlying pipeline, which writes the last export
// to the metrics file, and then closes the file. If the underlying pipeline
// fails to shut down, the file stays open.
func (p *Pipeline) Shutdown(ctx context.Context) error {
	if err := p.Pipeline.Shutdown(ctx); err != nil {
		// If ctx ended, a Write may still be in progress, so leave the file
		// open. (If a Write failed instead, nothing more is written, but
		// the process is exiting anyway.)
		return err
	}

	if file, has := p.file.Get(); has {
		if err := file.Close(); err != nil {
			return fmt.Errorf("closing metrics file: %w", err)
		}
	}

	return nil
}

// Setup creates the verifier’s metrics pipeline. Its handler serves the
// metrics in Prometheus text format, and if settings.FileDir is set, it
// writes them to a rotating file in that directory.
//
// The pipeline always includes the Go runtime and process metrics. Every
// series carries the process’s resource attributes (service name and
// version, host name, and process ID) as labels.
func Setup(lgr *logger.Logger, settings Settings) (*Pipeline, error) {
	var out option.Option[io.Writer]
	var file option.Option[io.Closer]

	if dir, has := settings.FileDir.Get(); has {
		// lumberjack opens the file only on the first write, so check the
		// directory now to fail at startup rather than at the first export.
		info, err := os.Stat(dir)
		if err != nil {
			return nil, fmt.Errorf("checking metrics directory: %w", err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%#q is %s; must be a directory", dir, describeFileType(info.Mode()))
		}

		rotatingFile := &lumberjack.Logger{
			Filename: filepath.Join(dir, FileName),
			MaxSize:  fileMaxSizeMB,
			Compress: true,
		}

		out = option.Some[io.Writer](rotatingFile)
		file = option.Some[io.Closer](rotatingFile)
	}

	pipeline, err := oteltools.Setup(oteltools.Settings{
		Logger:         slog.New(slogzerolog.Option{Logger: lgr.Logger}.NewZerologHandler()),
		Resource:       newResource(lgr),
		ResourceLabels: attribute.NewAllowKeysFilter(resourceLabelKeys()...),
		MetricsOut:     out,
	})
	if err != nil {
		return nil, fmt.Errorf("setting up metrics pipeline: %w", err)
	}

	return &Pipeline{Pipeline: pipeline, file: file}, nil
}

// resourceLabelKeys are the resource attributes that become labels on every
// series. It is an allow-list so that attributes added by the SDK or the
// environment don’t become labels unexpectedly.
func resourceLabelKeys() []attribute.Key {
	return []attribute.Key{
		semconv.ServiceNameKey,
		semconv.ServiceVersionKey,
		semconv.HostNameKey,
		semconv.ProcessPIDKey,
	}
}

func newResource(lgr *logger.Logger) *resource.Resource {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(buildvar.Revision),
		semconv.ProcessPID(os.Getpid()),
	}

	if hostname, err := os.Hostname(); err != nil {
		lgr.Warn().Err(err).Msg("Failed to get host name for metrics; omitting it.")
	} else {
		attrs = append(attrs, semconv.HostName(hostname))
	}

	return resource.NewWithAttributes(semconv.SchemaURL, attrs...)
}

// describeFileType describes a file type for an error message.
func describeFileType(mode fs.FileMode) string {
	switch {
	case mode.IsRegular():
		return "regular file"
	case mode.IsDir():
		return "directory"
	case mode&fs.ModeSymlink != 0:
		return "symbolic link"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeCharDevice != 0:
		return "character device"
	case mode&fs.ModeDevice != 0:
		return "device"
	default:
		return fmt.Sprintf("type %s", mode.Type())
	}
}
