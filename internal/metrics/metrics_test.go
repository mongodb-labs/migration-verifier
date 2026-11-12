package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/mongodb-labs/migration-tools/oteltools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetup_ServesRuntimeMetricsWithResourceLabels(t *testing.T) {
	pipeline, err := Setup(logger.NewDebugLogger(), Settings{})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pipeline.Shutdown(t.Context())) })

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	pipeline.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()

	// The runtime series carry the resource attributes as labels.
	for _, prefix := range []string{"go_goroutines{", "process_cpu_seconds_total{"} {
		series := findLine(t, body, prefix)
		assert.Contains(t, series, `service_name="`+ServiceName+`"`)
		assert.Contains(t, series, `process_pid="`+strconv.Itoa(os.Getpid())+`"`)
		assert.Contains(t, series, "service_version=")
		assert.Contains(t, series, "host_name=")
	}
}

func TestSetup_WritesMetricsFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "metrics")

	pipeline, err := Setup(logger.NewDebugLogger(), Settings{FileDir: option.Some(dir)})
	require.NoError(t, err)

	// Shutdown writes a final export and closes the file.
	require.NoError(t, pipeline.Shutdown(t.Context()))

	// The verifier may shut down twice: on a signal, then on exit.
	require.NoError(t, pipeline.Shutdown(t.Context()))

	file, err := os.Open(filepath.Join(dir, FileName))
	require.NoError(t, err)
	defer file.Close()

	contents, err := io.ReadAll(file)
	require.NoError(t, err)

	assert.True(
		t,
		strings.HasSuffix(string(contents), "\n"+oteltools.EndOfExport+"\n"),
		"the file ends with a complete export",
	)

	// Every sample in the export has a timestamp.
	series := findLine(t, string(contents), "go_goroutines{")
	assert.Len(t, strings.Fields(series[strings.LastIndexByte(series, '}')+1:]), 2, series)
}

func TestSetup_NoFileWhenUnset(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	pipeline, err := Setup(logger.NewDebugLogger(), Settings{})
	require.NoError(t, err)
	require.NoError(t, pipeline.Shutdown(t.Context()))

	assert.NoFileExists(t, filepath.Join(dir, FileName))
}

func findLine(t *testing.T, text, prefix string) string {
	t.Helper()

	for line := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}

	require.Failf(t, "line not found", "no line starting with %#q in:\n%s", prefix, text)

	return ""
}
