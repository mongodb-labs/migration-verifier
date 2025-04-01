package util

import (
	"log"

	"github.com/10gen/migration-verifier/internal/logger"
)

// Invariant asserts the predicate is true, and if not, logs the message and exits.
func Invariant(logger *logger.Logger, predicate bool, message string, args ...any) {
	if !predicate {
		if logger == nil {
			log.Fatalf(message, args...)
		}
		logger.Fatal().Msgf(message, args...)
	}
}
