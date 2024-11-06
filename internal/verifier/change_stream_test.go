package verifier

import (
	"context"
	"github.com/rs/zerolog"
	"strings"
	"testing"
	"time"

	"github.com/pkg/errors"
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

// TestChangeStreamResumability creates a verifier, starts its change stream,
// terminates that verifier, updates the source cluster, starts a new
// verifier with change stream, and confirms that things look as they should.
func (suite *MultiSourceVersionTestSuite) TestChangeStreamResumability() {
	func() {
		verifier1 := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := verifier1.StartChangeStream(ctx)
		suite.Require().NoError(err)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := suite.srcMongoClient.
		Database("testDb").
		Collection("testColl").
		InsertOne(
			ctx,
			bson.D{{"_id", "heyhey"}},
		)
	suite.Require().NoError(err)

	verifier2 := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)

	suite.Require().Empty(
		suite.fetchVerifierRechecks(ctx, verifier2),
		"no rechecks should be enqueued before starting change stream",
	)

	newTime := suite.getClusterTime(ctx, suite.srcMongoClient)

	err = verifier2.StartChangeStream(ctx)
	suite.Require().NoError(err)

	suite.Require().NotNil(verifier2.srcStartAtTs)

	suite.Assert().False(
		verifier2.srcStartAtTs.After(newTime),
		"verifier2's change stream should be no later than this new session",
	)

	recheckDocs := []bson.M{}

	require.Eventually(
		suite.T(),
		func() bool {
			recheckDocs = suite.fetchVerifierRechecks(ctx, verifier2)

			return len(recheckDocs) > 0
		},
		time.Minute,
		500*time.Millisecond,
		"the verifier should enqueue a recheck",
	)

	suite.Assert().Equal(
		bson.M{
			"db":         "testDb",
			"coll":       "testColl",
			"generation": int32(0),
			"docID":      "heyhey",
		},
		recheckDocs[0]["_id"],
		"recheck doc should have expected ID",
	)
}

func (suite *MultiSourceVersionTestSuite) getClusterTime(ctx context.Context, client *mongo.Client) primitive.Timestamp {
	sess, err := client.StartSession()
	suite.Require().NoError(err, "should start session")

	sctx := mongo.NewSessionContext(ctx, sess)
	suite.Require().NoError(sess.Client().Ping(sctx, nil))

	newTime, err := getClusterTimeFromSession(sess)
	suite.Require().NoError(err, "should fetch cluster time")

	return newTime
}

func (suite *MultiSourceVersionTestSuite) fetchVerifierRechecks(ctx context.Context, verifier *Verifier) []bson.M {
	recheckDocs := []bson.M{}

	recheckColl := verifier.verificationDatabase().Collection(recheckQueue)
	cursor, err := recheckColl.Find(ctx, bson.D{})

	if !errors.Is(err, mongo.ErrNoDocuments) {
		suite.Require().NoError(err)
		suite.Require().NoError(cursor.All(ctx, &recheckDocs))
	}

	return recheckDocs
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
	err = verifier.StartChangeStream(ctx)
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
	err = verifier.StartChangeStream(ctx)
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
	suite.Require().Negative(origStartTs.Compare(*newStartTs))
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
	err = verifier.StartChangeStream(ctx)
	suite.Require().NoError(err)
	suite.Require().NotNil(verifier.srcStartAtTs)
	suite.Require().LessOrEqual(origStartTs.Compare(*verifier.srcStartAtTs), 0)
}

func (suite *MultiSourceVersionTestSuite) TestBatchInsertChangeEventRecheckDocs() {
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)

	ctx := context.Background()
	vCtx, cancel := context.WithCancel(ctx)

	// Don't do a checkpoint for this test.
	origInterval := minChangeStreamCheckpointInterval
	minChangeStreamCheckpointInterval = 10 * time.Hour
	defer func() {
		minChangeStreamCheckpointInterval = origInterval
	}()

	err := verifier.StartChangeStream(vCtx)
	suite.Require().NoError(err)

	// A large recheck document should be flushed immediately.
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		ctx,
		bson.D{{"_id", strings.Repeat("a", 4*1024*1024)}},
	)
	suite.Require().NoError(err)
	require.Eventually(
		suite.T(),
		func() bool {
			return len(suite.fetchVerifierRechecks(ctx, verifier)) == 1
		},
		time.Minute,
		500*time.Millisecond,
		"the verifier should flush a recheck",
	)
	suite.Require().Empty(verifier.changeEventRecheckBuf.buf["testDB.testColl"])

	// A small recheck document should be buffered in-memory.
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		ctx,
		bson.D{{"_id", 0}},
	)
	suite.Require().NoError(err)
	require.Eventually(
		suite.T(),
		func() bool {
			suite.Require().Len(suite.fetchVerifierRechecks(ctx, verifier), 1)
			return len(verifier.changeEventRecheckBuf.buf["testDB.testColl"]) == 1
		},
		time.Minute,
		500*time.Millisecond,
		"the verifier should buffer a recheck",
	)

	// Any recheck docs remaining in the buffer should be flushed before the change stream reader exits.
	cancel()
	require.Eventually(
		suite.T(),
		func() bool {
			return len(suite.fetchVerifierRechecks(ctx, verifier)) == 2
		},
		time.Minute,
		500*time.Millisecond,
		"the verifier should have flushed all recheck docs",
	)
	suite.Require().Empty(verifier.changeEventRecheckBuf.buf["testDB.testColl"])
}
