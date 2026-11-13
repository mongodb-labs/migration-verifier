package oteltools

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mongodb-labs/migration-tools/future"
	"github.com/prometheus/client_golang/prometheus"
	prometheusclient "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// EndOfExport is the line that follows each export in the output. It is
// OpenMetrics’ end-of-exposition marker, which Prometheus text parsers read as
// a comment.
const EndOfExport = "# EOF"

// exportWorker is the goroutine that a streamExporter controls. It writes a
// gatherer’s metrics to the output.
type exportWorker struct {
	gatherer prometheus.Gatherer
	writer   io.Writer
	logger   *slog.Logger

	// gaveUp is set when stopAndFlush stops waiting for the last export.
	gaveUp *atomic.Bool
}

// run writes an export every interval until stop is closed. It then writes one
// last export, unless stopAndFlush has given up waiting for it, and sets the
// result, which is that last export’s error.
//
// A failed periodic export is logged, and later exports are still written, so
// a persistent failure is logged every interval. If a Write wrote part of an
// export, the next export’s first line joins the partial line, which leaves
// one line that doesn’t parse.
func (ew *exportWorker) run(
	interval time.Duration,
	stop <-chan struct{},
	setResult future.Setter[error],
) {
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-timer.C:
		case <-stop:
		}

		// Stopping takes priority: if the timer fired too, select may have
		// picked it, and a periodic export must not start after stopAndFlush
		// has stopped waiting.
		select {
		case <-stop:
			// One last export, then we’re done. (exportOnce writes nothing
			// if stopAndFlush has given up.) We do this so that, if the
			// shutdown is due to a problem, this last export can provide
			// useful diagnostic information. It’s also consistent with the
			// OTel SDK’s PeriodicReader.
			setResult(ew.exportOnce())

			return
		default:
		}

		if err := ew.exportOnce(); err != nil {
			ew.logger.Warn("Failed to write periodic metrics export.", "error", err)
		}

		timer.Reset(interval)
	}
}

// exportOnce gathers the current metrics and writes them as one export: a
// Prometheus text exposition followed by an EndOfExport line. It writes
// nothing if there are no metrics, or if stopAndFlush has given up waiting.
//
// The export is written in a single Write call, so a rotating writer never
// splits it across files.
func (ew *exportWorker) exportOnce() error {
	families, err := ew.gatherer.Gather()
	if err != nil {
		// Gather returns whatever it could collect along with the error,
		// so write that rather than nothing.
		ew.logger.Warn("Failed to gather some metrics for periodic export.", "error", err)
	}

	export := ew.encodeExport(families)
	if len(export) == 0 {
		return nil
	}

	// Gathering and encoding take time, during which stopAndFlush may have
	// given up and returned, so check right before writing. (Shutdown can
	// still give up between this check and the Write; nothing short of
	// blocking Shutdown on the Write can prevent that.)
	if ew.gaveUp.Load() {
		return nil
	}

	wrote, err := ew.writer.Write(export)

	if err == nil && wrote != len(export) {
		err = fmt.Errorf("%w: only %d of %d bytes", io.ErrShortWrite, wrote, len(export))
	}

	if err != nil {
		return fmt.Errorf("writing metrics export: %w", err)
	}

	return nil
}

// encodeExport returns the gathered metrics as one Prometheus exposition
// with an EndOfExport line. It returns nothing if there are no metrics.
func (ew *exportWorker) encodeExport(families []*prometheusclient.MetricFamily) []byte {
	if len(families) == 0 {
		return nil
	}

	// Scraped metrics normally carry no timestamps because the scraper
	// stamps them. The output may be read long after the fact, so stamp
	// every sample with the time it was gathered.
	nowMS := time.Now().UnixMilli()
	for _, family := range families {
		for _, m := range family.GetMetric() {
			m.TimestampMs = new(nowMS)
		}
	}

	var buf bytes.Buffer
	encoder := expfmt.NewEncoder(
		&buf,
		expfmt.NewFormat(expfmt.TypeTextPlain).WithEscapingScheme(model.UnderscoreEscaping),
	)

	for _, family := range families {
		if err := encoder.Encode(family); err != nil {
			ew.logger.Warn(
				"Failed to encode metric for periodic export.",
				"metric", family.GetName(),
				"error", err,
			)
		}
	}

	if buf.Len() == 0 {
		return nil
	}

	buf.WriteString(EndOfExport + "\n")

	return buf.Bytes()
}
