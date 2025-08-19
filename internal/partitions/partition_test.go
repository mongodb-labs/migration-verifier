package partitions

import (
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func (suite *UnitTestSuite) TestPartitionLowerBoundFromCurrent() {
	expectLowerBound := int32(5)
	current := bson.D{bson.E{"_id", expectLowerBound}, {"anotherField", "hello"}}
	rawCurrent, err := bson.Marshal(current)
	require.NoError(suite.T(), err)
	suite.Run("normal partition", func() {
		partition, _ := makeTestPartition()
		lowerBound, err := partition.lowerBoundFromCurrent(rawCurrent)
		require.NoError(suite.T(), err)
		require.NotNil(suite.T(), lowerBound)
		assert.Equal(suite.T(), expectLowerBound, lowerBound)
	})
	suite.Run("capped partition", func() {
		partition := makeTestCappedPartition()
		lowerBound, err := partition.lowerBoundFromCurrent(rawCurrent)
		require.NoError(suite.T(), err)
		require.Nil(suite.T(), lowerBound)
	})
}

func (suite *UnitTestSuite) TestVersioning() {
	partition, expectedExprFilter := makeTestPartition()

	// No version given, default to no bracketing
	qp, err := partition.GetQueryParameters(nil, nil)
	suite.Require().NoError(err)
	findOptions := qp.ToFindOptions()
	filter := getFilterFromFindOptions(findOptions)
	suite.Require().Equal(expectedExprFilter, filter)

	// 6.0
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{6, 0, 0}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().Equal(expectedExprFilter, filter)

	// 5.3.0.9
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{5, 3, 0, 9}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().Equal(expectedExprFilter, filter)

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
	suite.Require().NotEqual(expectedExprFilter, filter)

	// 4.4
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{4, 4, 0, 0}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().NotEqual(expectedExprFilter, filter)

	// 4.2
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{VersionArray: []int{4, 2, 0, 0}}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().NotEqual(expectedExprFilter, filter)

	// No version array -- assume old, require type bracketing.
	qp, err = partition.GetQueryParameters(&util.ClusterInfo{}, nil)
	suite.Require().NoError(err)
	findOptions = qp.ToFindOptions()
	filter = getFilterFromFindOptions(findOptions)
	suite.Require().NotEqual(expectedExprFilter, filter)
}

func getFilterFromFindOptions(opts bson.D) any {
	for _, el := range opts {
		if el.Key == "filter" {
			return el.Value
		}
	}

	return nil
}

func makeTestPartition() (Partition, bson.D) {
	partition := Partition{
		Key: PartitionKey{
			SourceUUID:  util.NewUUID(),
			Lower:       primitive.ObjectID([12]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}),
			MongosyncID: "",
		},
		Ns:    &Namespace{DB: "testDB", Coll: "testColl"},
		Upper: primitive.ObjectID([12]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 0, 1, 2}),
	}
	return partition, makeExpectedFilter(partition.Key.Lower, partition.Upper)
}

func makeExpectedFilter(lower, upper any) bson.D {
	return bson.D{{"$and", bson.A{
		bson.D{{"$and", []bson.D{
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
		}}},
	}}}
}

func makeTestCappedPartition() Partition {
	partition, _ := makeTestPartition()
	partition.IsCapped = true
	return partition
}
