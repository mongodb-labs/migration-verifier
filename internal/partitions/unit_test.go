package partitions

import (
	"context"
	"os"
	"testing"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/suite"
)

type UnitTestSuite struct {
	suite.Suite
	logger *logger.Logger
	ctx    *contextplus.C
}

func TestUnitTestSuite(t *testing.T) {
	ts := &UnitTestSuite{
		ctx: contextplus.Background(),
	}

	suite.Run(t, ts)
}

func (suite *UnitTestSuite) SetupSuite() {
	suite.logger = newLogger()
	log.Logger = *suite.logger.Logger
	zerolog.DefaultContextLogger = suite.logger.Logger
}

// Everything below was copied from mongosync testutil.

// Context returns a new context with the logger set in it.
func (suite *UnitTestSuite) Context() context.Context {
	return suite.logger.Logger.WithContext(suite.ctx)
}

// Logger returns the logger for the suite.
func (suite *UnitTestSuite) Logger() *logger.Logger {
	return suite.logger
}

func newLogger() *logger.Logger {
	if os.Getenv("MONGOSYNC_TEST_DEBUG") != "" {
		return logger.NewDebugLogger()
	}
	return logger.NewDefaultLogger()
}
