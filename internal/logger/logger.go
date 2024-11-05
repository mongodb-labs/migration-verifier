package logger

import (
	"context"
	"io"
	"os"
	"path"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/natefinch/lumberjack.v2"
)

const (
	// DefaultLogLevel is the default log level
	DefaultLogLevel = zerolog.InfoLevel

	LogFileName = "migration-verifier.log"
)

// DefaultLogWriter is the default log io.Writer implementor
var DefaultLogWriter = os.Stderr

// Logger is a logger struct that we can Rotate
type Logger struct {
	*zerolog.Logger
	writer io.Writer
}

// NewSubLogger creates a sub Logger of the parent one, with the same writer
func NewSubLogger(ctx context.Context, parentLogger *Logger, childComponentName string, childComponent string) *Logger {
	subLogger := parentLogger.With().Str(childComponentName, childComponent).Logger()
	subLogger.WithContext(ctx)
	ret := &Logger{
		Logger: &subLogger,
		writer: parentLogger.writer,
	}
	return ret
}

// CreateFromContext creates a sub Logger of the parent one, with the same writer
func CreateFromContext(ctx context.Context, parentLogger *Logger) *Logger {
	subLogger := log.Ctx(ctx).With().Logger()
	ret := &Logger{
		Logger: &subLogger,
		writer: parentLogger.writer,
	}
	return ret
}

// NewLogger creates a New Logger
func NewLogger(logger *zerolog.Logger, writer io.Writer) *Logger {
	ret := &Logger{
		Logger: logger,
		writer: writer,
	}
	ret.Rotate()
	return ret
}

// NewDefaultLogger creates a new Logger with default log writer and level
func NewDefaultLogger() *Logger {
	logger := zerolog.New(DefaultLogWriter).Level(DefaultLogLevel).With().Timestamp().Logger()
	return &Logger{
		Logger: &logger,
		writer: DefaultLogWriter,
	}
}

// NewDebugLogger creates a new Logger with default log writer with debug level
func NewDebugLogger() *Logger {
	logger := zerolog.New(DefaultLogWriter).Level(zerolog.DebugLevel).With().Timestamp().Logger()
	return &Logger{
		Logger: &logger,
		writer: DefaultLogWriter,
	}
}

// Rotate will rotate the underlying Logger writer iff it is a *lumberjack.Logger
func (l *Logger) Rotate() {
	switch w := l.writer.(type) {
	case *lumberjack.Logger:
		_ = w.Rotate()
	}
}

// NewRotatingWriter creates a new io.Writer with an underlying lumberjack.Logger
func NewRotatingWriter(dirPath string) (io.Writer, error) {
	err := os.MkdirAll(dirPath, 0744)
	if err != nil {
		return nil, err
	}

	return &lumberjack.Logger{
		Filename: path.Join(dirPath, LogFileName),
	}, nil
}

// AddSubLoggerFieldInContext adds additional field to the sublogger inside context
func AddSubLoggerFieldInContext(ctx context.Context, childComponentName string, childComponent string) {
	l := zerolog.Ctx(ctx)
	l.UpdateContext(func(c zerolog.Context) zerolog.Context {
		return c.Str(childComponentName, childComponent)
	})
}
