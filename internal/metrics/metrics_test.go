package metrics

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/mongodb-labs/migration-tools/fstools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/mongodb-labs/migration-tools/oteltools"
	"github.com/rs/zerolog"
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
	dir := t.TempDir()

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

func TestCheckFileDir(t *testing.T) {
	require.NoError(t, checkFileDir(t.TempDir()))

	require.ErrorIs(t, checkFileDir(filepath.Join(t.TempDir(), "missing")), fs.ErrNotExist)

	notDir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDir, nil, 0o600))

	err := checkFileDir(notDir)
	require.Error(t, err)
	assert.ErrorContains(t, err, "regular file")
}

// An unusable metrics directory costs only the file: a warning says why, the
// handler still serves metrics, and nothing is created in the directory’s place.
func TestSetup_UnusableDirectoryKeepsHandler(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "missing")

		// The cause’s wording varies by OS, so get it from the OS.
		_, statErr := os.Stat(dir)
		var pathErr *fs.PathError
		require.ErrorAs(t, statErr, &pathErr)

		assertSetupWarnsAndServes(t, dir, pathErr.Err.Error())
		assert.NoDirExists(t, dir)
	})

	t.Run("regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "file")
		require.NoError(t, os.WriteFile(path, nil, 0o600))

		info, err := os.Stat(path)
		require.NoError(t, err)

		assertSetupWarnsAndServes(t, path, fstools.DescribeFileType(info.Mode()))
		assert.FileExists(t, path)
	})
}

// assertSetupWarnsAndServes sets up metrics with dir as the metrics directory.
// It asserts that Setup succeeds, that it logs a warning that names dir and
// includes cause, and that the handler still serves metrics.
func assertSetupWarnsAndServes(t *testing.T, dir, cause string) {
	t.Helper()

	var logs bytes.Buffer
	zl := zerolog.New(&logs)

	pipeline, err := Setup(logger.NewLogger(&zl, &logs), Settings{FileDir: option.Some(dir)})
	require.NoError(t, err)

	warning := findLine(t, logs.String(), `{"level":"warn"`)

	var entry struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(warning), &entry))
	assert.Contains(t, entry.Error, dir, "the warning names the directory")
	assert.Contains(t, entry.Error, cause, "the warning gives the cause")

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	pipeline.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	findLine(t, rec.Body.String(), "go_goroutines{")

	require.NoError(t, pipeline.Shutdown(t.Context()))
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
