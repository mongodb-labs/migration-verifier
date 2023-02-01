package verifier

import (
	"io"
	"os"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/rs/zerolog/log"
)

// DefaultLogWriter is the default log io.Writer implementor.
var DefaultLogWriter = os.Stderr

func GetLogWriter(logPath string) io.Writer {
	if logPath == "stderr" {
		return DefaultLogWriter
	}

	if w, err := logger.NewRotatingWriter(logPath); err == nil {
		return w
	}

	log.Fatal().Msgf("Failed to open logPath: %s", logPath)
	return nil
}
