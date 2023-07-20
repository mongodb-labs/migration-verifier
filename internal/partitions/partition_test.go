package partitions

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// Test that a partion can generate a correct filter with a lower bound for collection copy.
func (suite *UnitTestSuite) TestPartitionFindCmd() {
	suite.Run("normal partition", func() {
		partition, expectedFilter := makeTestPartition()
		startAt := &primitive.Timestamp{42, 43}
		expectedFind := bson.D{
			{"find", partition.Ns.Coll},
			{"collectionUUID", partition.Key.SourceUUID},
			{"readConcern", bson.D{
				{"level", "majority"},
				{"afterClusterTime", startAt},
			}},
			{"noCursorTimeout", true},
			{"hint", bson.D{{"_id", 1}}},
			{"filter", expectedFilter},
		}
		actual := partition.FindCmd(suite.Logger(), startAt)
		assertBSONEqual(suite.T(), expectedFind, actual)
	})
	suite.Run("capped partition", func() {
		partition := makeTestCappedPartition()
		startAt := &primitive.Timestamp{42, 43}

		expectedFind := bson.D{
			{"find", partition.Ns.Coll},
			{"collectionUUID", partition.Key.SourceUUID},
			{"readConcern", bson.D{
				{"level", "majority"},
				{"afterClusterTime", startAt},
			}},
			{"noCursorTimeout", true},
			{"sort", bson.D{{"$natural", 1}}},
		}
		actual := partition.FindCmd(suite.Logger(), startAt)
		assertBSONEqual(suite.T(), expectedFind, actual)
	})
}

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
	partition, expectedFilter := makeTestPartition()
	expectedFilterWithTypeBracketing := makeExpectedFilterWithTypeBracketing(partition.Key.Lower, partition.Upper)
	// No version given, default to no bracketing
	findOptions := partition.GetFindOptions(nil, nil)
	filter := findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilter, filter)

	// 6.0 (int64)
	findOptions = partition.GetFindOptions(&bson.M{"versionArray": bson.A{int64(6), int64(0), int64(0), int64(0)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilter, filter)

	// 6.0
	findOptions = partition.GetFindOptions(&bson.M{"versionArray": bson.A{int32(6), int32(0), int32(0), int32(0)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilter, filter)

	// 5.3.0.9
	findOptions = partition.GetFindOptions(&bson.M{"versionArray": bson.A{int32(5), int32(3), int32(0), int32(9)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilter, filter)

	// 7.1.3.5
	findOptions = partition.GetFindOptions(&bson.M{"versionArray": bson.A{int32(7), int32(1), int32(3), int32(5)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilter, filter)

	// 4.4 (int64)
	findOptions = partition.GetFindOptions(&bson.M{"versionArray": bson.A{int64(4), int64(4), int64(0), int64(0)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilterWithTypeBracketing, filter)

	// 4.4
	findOptions = partition.GetFindOptions(&bson.M{"versionArray": bson.A{int32(4), int32(4), int32(0), int32(0)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilterWithTypeBracketing, filter)

	// 4.2
	findOptions = partition.GetFindOptions(&bson.M{"versionArray": bson.A{int32(4), int32(2), int32(0), int32(0)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilterWithTypeBracketing, filter)

	// No version array -- assume old, require type bracketing.
	findOptions = partition.GetFindOptions(&bson.M{"notVersionArray": bson.A{6, int32(0), int32(0), int32(0)}}, nil)
	filter = findOptions.Map()["filter"]
	suite.Require().Equal(expectedFilterWithTypeBracketing, filter)
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

func makeExpectedFilter(lower, upper interface{}) bson.D {
	return bson.D{{"$and", bson.A{
		bson.D{{"$and", bson.A{
			// All _id values >= lower bound.
			bson.D{{"$expr", bson.D{
				{"$gte", bson.A{
					"$_id",
					bson.D{{"$literal", lower}},
				}},
			}}},
			// All _id values <= upper bound.
			bson.D{{"$expr", bson.D{
				{"$lte", bson.A{
					"$_id",
					bson.D{{"$literal", upper}},
				}},
			}}},
		}}},
	}}}
}

func makeExpectedFilterWithTypeBracketing(lower, upper interface{}) bson.D {
	return bson.D{{"$and", bson.A{
		bson.D{{"$and", bson.A{
			// All _id values >= lower bound.
			bson.D{{"_id", bson.D{{"$gte", lower}}}},
			// All _id values <= upper bound.
			bson.D{{"_id", bson.D{{"$lte", upper}}}},
		}}},
	}}}
}

func makeTestCappedPartition() Partition {
	partition, _ := makeTestPartition()
	partition.IsCapped = true
	return partition
}

func assertBSONEqual(t *testing.T, expected, actual interface{}) {
	expectedJSON, err := bson.MarshalExtJSONIndent(expected, false, false, "", "    ")
	require.NoError(t, err)

	actualJSON, err := bson.MarshalExtJSONIndent(actual, false, false, "", "    ")
	require.NoError(t, err)

	assert.Equal(t, string(expectedJSON), string(actualJSON))
}
