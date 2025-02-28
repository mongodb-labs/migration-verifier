package contextplus

import (
	"context"
	"testing"

	"github.com/stretchr/testify/suite"
)

type UnitTestSuite struct {
	suite.Suite
}

func TestUnitTestSuite(t *testing.T) {
	ts := new(UnitTestSuite)
	suite.Run(t, ts)
}

// Context returns a new `C`.
func (_ *UnitTestSuite) Context() *C {
	return New(context.Background())
}
