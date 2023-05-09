package documentmap

import (
	"context"
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
)

type UnitTestSuite struct {
	suite.Suite
	logger *logger.Logger
}

func TestUnitTestSuite(t *testing.T) {
	ts := new(UnitTestSuite)
	ts.logger = logger.NewDebugLogger()
	suite.Run(t, ts)
}

func convertDocsToRaws(docs []bson.D) []bson.Raw {
	raws := make([]bson.Raw, len(docs))

	for i, doc := range docs {
		raw, err := bson.Marshal(doc)

		// Shouldn't happen:
		if err != nil {
			panic("Failed to marshal: " + err.Error())
		}

		raws[i] = raw
	}

	return raws
}

func (s *UnitTestSuite) TestBasics() {
	docs1 := []bson.D{
		{{"_id", 0}, {"foo", "bar"}},
		{{"_id", 1}, {"foo", "baz"}},
		{{"_id", 1}, {"foo", "qux"}},
	}
	raw1 := convertDocsToRaws(docs1)

	docs2 := []bson.D{
		{{"_id", 0}, {"foo", "bar"}},
		{{"_id", 1}, {"foo", "baz"}},
		{{"_id", 1}, {"foo", "quux"}},
	}
	raw2 := convertDocsToRaws(docs2)

	cur1 := testutil.DocsToCursor(docs1)
	cur2 := testutil.DocsToCursor(docs2)

	dmap1 := New(s.logger, "foo")
	err := dmap1.ImportFromCursor(context.Background(), cur1)
	s.Require().NoError(err)

	s.Assert().Equal(types.DocumentCount(3), dmap1.Count(), "docs count")

	expectedSize := 0
	for _, doc := range raw1 {
		expectedSize += len(doc)
	}
	s.Assert().Equal(types.ByteCount(expectedSize), dmap1.TotalDocsBytes(), "bytes count")

	dmap2 := dmap1.CloneEmpty()
	err = dmap2.ImportFromCursor(context.Background(), cur2)
	s.Require().NoError(err)

	only1, only2, common := dmap1.CompareToMap(dmap2)

	s.Assert().Equal(
		[]MapKey{dmap1.getMapKey(&raw1[2])},
		only1,
		"docs exclusively in 1st map",
	)

	s.Assert().Equal(
		[]MapKey{dmap1.getMapKey(&raw2[2])},
		only2,
		"docs exclusively in 2nd map",
	)

	s.Assert().ElementsMatch(
		[]MapKey{dmap1.getMapKey(&raw1[0]), dmap1.getMapKey(&raw1[1])},
		common,
		"docs common to both maps",
	)
}
