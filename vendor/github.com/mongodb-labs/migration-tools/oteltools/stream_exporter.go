package oteltools

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mongodb-labs/migration-tools/contextplus"
	"github.com/mongodb-labs/migration-tools/future"
	"github.com/prometheus/client_golang/prometheus"
)

// streamExporter is the handle on a goroutine that periodically writes a
// gatherer’s metrics to Settings.MetricsOut. Because the goroutine reads the
// same registry that the HTTP handler serves, the output always matches the
// handler’s.
type streamExporter struct {
	// stop tells the goroutine to stop when closed.
	stop chan struct{}

	// result is the final write’s error, set when the goroutine exits.
	result *future.Future[error]

	// gaveUp tells the goroutine that stopAndFlush has stopped waiting for
	// it, so it must write nothing more.
	gaveUp *atomic.Bool
}

func startStreamExporter(
	gatherer prometheus.Gatherer,
	writer io.Writer,
	logger *slog.Logger,
	interval time.Duration,
) *streamExporter {
	stop := make(chan struct{})
	result, setResult := future.New[error]()
	gaveUp := &atomic.Bool{}

	worker := &exportWorker{
		gatherer: gatherer,
		writer:   writer,
		logger:   logger,
		gaveUp:   gaveUp,
	}

	go worker.run(interval, stop, setResult)

	return &streamExporter{
		stop:   stop,
		result: result,
		gaveUp: gaveUp,
	}
}

// stopAndFlush stops the periodic writes and then writes one last export, so
// that the output includes everything recorded up to now. Call it only once.
//
// A Write can’t be canceled, so if ctx ends before the writes finish,
// stopAndFlush returns without waiting for them. A Write may then still be in
// progress, or about to begin, but an export that hasn’t reached its Write
// writes nothing.
func (se *streamExporter) stopAndFlush(ctx context.Context) error {
	close(se.stop)

	select {
	case <-se.result.Ready():
	case <-ctx.Done():
	}

	// If the final write and context are both done, favor the write.
	select {
	case <-se.result.Ready():
		return se.result.Get()
	default:
		se.gaveUp.Store(true)

		return fmt.Errorf("awaiting last write: %w", contextplus.New(ctx).Err())
	}
}
