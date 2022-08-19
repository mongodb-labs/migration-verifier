package partitions

import (
	"testing"

	"github.com/10gen/mongosync/internal/testutil"
	"github.com/stretchr/testify/suite"
)

type UnitTestSuite struct {
	testutil.CoreTestSuiteWithClient
}

func TestUnitTestSuite(t *testing.T) {
	ts := new(UnitTestSuite)
	suite.Run(t, ts)
}
