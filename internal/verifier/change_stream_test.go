package verifier

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestChangeStreamFilter(t *testing.T) {
	verifier := Verifier{}
	verifier.SetMetaDBName("metadb")
	require.Equal(t, []bson.D{{{"$match", bson.D{{"ns.db", bson.D{{"$ne", "metadb"}}}}}}},
		verifier.GetChangeStreamFilter())
	verifier.srcNamespaces = []string{"foo.bar", "foo.baz", "test.car", "test.chaz"}
	require.Equal(t, []bson.D{
		{{"$match", bson.D{
			{"$or", bson.A{
				bson.D{{"ns", bson.D{{"db", "foo"}, {"coll", "bar"}}}},
				bson.D{{"ns", bson.D{{"db", "foo"}, {"coll", "baz"}}}},
				bson.D{{"ns", bson.D{{"db", "test"}, {"coll", "car"}}}},
				bson.D{{"ns", bson.D{{"db", "test"}, {"coll", "chaz"}}}},
			}},
		}}},
	}, verifier.GetChangeStreamFilter())
}

func (suite *MultiSourceVersionTestSuite) TestStartAtTimeNoChanges() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 0}})
	suite.Require().NoError(err)
	origStartTs := sess.OperationTime()
	suite.Require().NotNil(origStartTs)
	err = verifier.StartChangeStream(ctx, origStartTs)
	suite.Require().NoError(err)
	suite.Require().Equal(verifier.srcStartAtTs, origStartTs)
	verifier.changeStreamEnderChan <- struct{}{}
	<-verifier.changeStreamDoneChan
	suite.Require().Equal(verifier.srcStartAtTs, origStartTs)
}

func (suite *MultiSourceVersionTestSuite) TestStartAtTimeWithChanges() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 0}})
	suite.Require().NoError(err)
	origStartTs := sess.OperationTime()
	suite.Require().NotNil(origStartTs)
	err = verifier.StartChangeStream(ctx, origStartTs)
	suite.Require().NoError(err)
	suite.Require().Equal(verifier.srcStartAtTs, origStartTs)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 2}})
	suite.Require().NoError(err)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").ReplaceOne(
		sctx, bson.D{{"_id", 1}}, bson.D{{"_id", 1}, {"a", "2"}})
	suite.Require().NoError(err)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").DeleteOne(
		sctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	newStartTs := sess.OperationTime()
	suite.Require().NotNil(newStartTs)
	suite.Require().Less(primitive.CompareTimestamp(*origStartTs, *newStartTs), 0)
	verifier.changeStreamEnderChan <- struct{}{}
	<-verifier.changeStreamDoneChan
	suite.Require().Equal(verifier.srcStartAtTs, newStartTs)
}

func (suite *MultiSourceVersionTestSuite) TestNoStartAtTime() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 0}})
	suite.Require().NoError(err)
	origStartTs := sess.OperationTime()
	suite.Require().NotNil(origStartTs)
	err = verifier.StartChangeStream(ctx, nil)
	suite.Require().NoError(err)
	suite.Require().NotNil(verifier.srcStartAtTs)
	suite.Require().LessOrEqual(primitive.CompareTimestamp(
		*origStartTs, *verifier.srcStartAtTs), 0)
}
