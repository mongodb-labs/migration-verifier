package verifier

import (
	"io"
	"os"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/term"
)

func getLogWriter(logPath string) (io.Writer, io.Writer) {
	var writer io.Writer

	switch logPath {
	case "stdout":
		writer = os.Stdout

	case "stderr":
		writer = os.Stderr

	default:
		if w, err := logger.NewRotatingWriter(logPath); err == nil {
			return w, w
		}

		log.Fatal().Msgf("Failed to open logPath: %s", logPath)
	}

	return writer, zerolog.SyncWriter(writer)
}

func getLoggerAndWriter(logPath string) (*logger.Logger, io.Writer) {
	rawWriter, writer := getLogWriter(logPath)

	consoleWriter := zerolog.ConsoleWriter{
		Out:        writer,
		TimeFormat: timeFormat,
		NoColor:    shouldSuppressColor(rawWriter),
	}

	l := zerolog.New(consoleWriter).With().Timestamp().Logger()
	return logger.NewLogger(&l, writer), writer
}

// Returns true unless the writer is a TTY.
func shouldSuppressColor(writer io.Writer) bool {
	osFile, isOsFile := writer.(*os.File)
	if !isOsFile {
		return true
	}

	return !term.IsTerminal(int(osFile.Fd()))
}
