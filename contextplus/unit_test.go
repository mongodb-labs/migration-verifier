package contextplus

import (
	"reflect"
	"runtime/debug"
	"strings"
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

func (s *UnitTestSuite) TestParallel() {
	methodFinder := reflect.TypeOf(s)

	for i := range methodFinder.NumMethod() {
		method := methodFinder.Method(i)

		if !strings.HasPrefix(method.Name, "ParallelTest") {
			continue
		}
	}
}

func recoverAndFailOnPanic(t *testing.T) {
	t.Helper()
	r := recover()
	failOnPanic(t, r)
}

func failOnPanic(t *testing.T, r interface{}) {
	t.Helper()
	if r != nil {
		t.Errorf("test panicked: %v\n%s", r, debug.Stack())
		t.FailNow()
	}
}
