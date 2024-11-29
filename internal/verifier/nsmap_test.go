package verifier

import (
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

func (s *UnitTestSuite) Test_EmptyNsMap() {
	nsMap := NewNSMap()
	srcNamespaces := []string{"srcDB.A", "srcDB.B"}
	dstNamespaces := []string{"dstDB.B", "dstDB.A"}
	nsMap.PopulateWithNamespaces(srcNamespaces, dstNamespaces)
	s.Require().Equal(2, nsMap.Len())

	_, ok := nsMap.GetDstNamespace("non-existent.coll")
	s.Require().False(ok)

	for i, srcNs := range srcNamespaces {
		gotNs, ok := nsMap.GetDstNamespace(srcNs)
		s.Require().True(ok)
		s.Require().Equal(dstNamespaces[i], gotNs)
	}

	for i, dstNs := range dstNamespaces {
		gotNs, ok := nsMap.GetSrcNamespace(dstNs)
		s.Require().True(ok)
		s.Require().Equal(srcNamespaces[i], gotNs)
	}
}
