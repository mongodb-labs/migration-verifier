package partitions

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (suite *UnitTestSuite) TestVersioning() {
	partition, expectedExprFilter := suite.makeTestPartition()

	// No version given, default to no bracketing
	qp, err := partition.GetQueryParameters(nil, nil)
	suite.Require().NoError(err)
	findOptions := qp.ToFindOptions()
	filter := getFilterFromFindOptions(findOptions)
	suite.Require().Equal(
		expectedExprFilter,
		roundTripBSON(suite.T(), filter),
	)

	// 6.0
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{6, 0, 0}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().Equal(
		expectedExprFilter,
		roundTripBSON(suite.T(), filter),
	)

	// 5.3.0.9
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{5, 3, 0, 9}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().Equal(
		expectedExprFilter,
		roundTripBSON(suite.T(), filter),
	)

	// 7.1.3.5
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{7, 1, 3, 5}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().Equal(expectedExprFilter, filter)

	// 4.4 (int64)
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{4, 4, 0, 0}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().NotEqual(
		expectedExprFilter,
		roundTripBSON(suite.T(), filter),
	)

	// 4.4
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{4, 4, 0, 0}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().NotEqual(
		expectedExprFilter,
		roundTripBSON(suite.T(), filter),
	)

	// 4.2
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{4, 2, 0, 0}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().NotEqual(
		expectedExprFilter,
		roundTripBSON(suite.T(), filter),
	)

	// No version array -- assume old, require type bracketing.
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().NotEqual(
		expectedExprFilter,
		roundTripBSON(suite.T(), filter),
	)
}

func getFilterFromFindOptions(opts bson.D) any {
	for _, el := range opts {
		if el.Key == "filter" {
			return el.Value
		}
	}

	return nil
}

func (suite *UnitTestSuite) makeTestPartition() (Partition, bson.D) {
	partition := Partition{
		Key: PartitionKey{
			SourceUUID: util.NewUUID(),
			Lower:      primitive.ObjectID([12]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}),
		},
		Ns:    &Namespace{DB: "testDB", Coll: "testColl"},
		Upper: primitive.ObjectID([12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 1, 2}),
	}
	return partition, suite.makeExpectedFilter(partition.Key.Lower, partition.Upper)
}

func (suite *UnitTestSuite) makeExpectedFilter(lower, upper any) bson.D {
	return roundTripBSON(suite.T(), bson.D{{"$and", []bson.D{
		// All _id values >= lower bound.
		{{"$expr", bson.D{
			{"$gte", bson.A{
				"$_id",
				bson.D{{"$literal", lower}},
			}},
		}}},
		// All _id values <= upper bound.
		{{"$expr", bson.D{
			{"$lte", bson.A{
				"$_id",
				bson.D{{"$literal", upper}},
			}},
		}}},
	}}})
}

func roundTripBSON[T any](t *testing.T, val T) T {
	raw, err := bson.Marshal(val)
	require.NoError(t, err, "should marshal %T: %v", val, val)

	var rt T
	require.NoError(t, bson.Unmarshal(raw, &rt))

	return rt
}
