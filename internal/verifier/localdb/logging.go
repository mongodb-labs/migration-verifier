package localdb

import (
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/dgraph-io/badger/v4"
	"github.com/rs/zerolog"
)

type badgerLogger struct {
	l *logger.Logger
}

var _ badger.Logger = &badgerLogger{}

func (bl *badgerLogger) Errorf(tmpl string, args ...any) {
	bl.log(zerolog.ErrorLevel, "error", tmpl, args...)
}

func (bl *badgerLogger) Warningf(tmpl string, args ...any) {
	bl.log(zerolog.DebugLevel, "warn", tmpl, args...)
}

// What BadgerDB considers “info” is not really so for us.
func (bl *badgerLogger) Infof(tmpl string, args ...any) {
	bl.log(zerolog.TraceLevel, "info", tmpl, args...)
}

func (bl *badgerLogger) Debugf(tmpl string, args ...any) {
	bl.log(zerolog.TraceLevel, "debug", tmpl, args...)
}

func (bl *badgerLogger) log(lv zerolog.Level, badgerLevel string, tmpl string, args ...any) {
	bl.l.Logger.WithLevel(lv).Msgf("localDB/badger ("+badgerLevel+"): "+tmpl, args...)
}
