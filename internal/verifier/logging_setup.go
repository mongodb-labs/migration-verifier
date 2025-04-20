package verifier

import (
	"io"
	"os"

	"github.com/mongodb-labs/migration-verifier/internal/logger"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func getLogWriter(logPath string) io.Writer {
	var writer io.Writer

	switch logPath {
	case "stdout":
		writer = os.Stdout

	case "stderr":
		writer = os.Stderr

	default:
		if w, err := logger.NewRotatingWriter(logPath); err == nil {
			return w
		}

		log.Fatal().Msgf("Failed to open logPath: %s", logPath)
	}

	return zerolog.SyncWriter(writer)
}

func getLoggerAndWriter(logPath string) (*logger.Logger, io.Writer) {
	writer := getLogWriter(logPath)

	consoleWriter := zerolog.ConsoleWriter{
		Out:        writer,
		TimeFormat: timeFormat,
	}

	l := zerolog.New(consoleWriter).With().Timestamp().Logger()
	return logger.NewLogger(&l, writer), writer
}
