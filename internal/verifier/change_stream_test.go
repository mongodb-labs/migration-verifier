package verifier

import (
	"context"
	"testing"
	"time"

	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestChangeStreamFilter(t *testing.T) {
	verifier := Verifier{}
	verifier.initializeChangeStreamReaders()
	verifier.SetMetaDBName("metadb")
	require.Equal(t, []bson.D{{{"$match", bson.D{{"ns.db", bson.D{{"$ne", "metadb"}}}}}}},
		verifier.srcChangeStreamReader.GetChangeStreamFilter())
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
	}, verifier.srcChangeStreamReader.GetChangeStreamFilter())
}

// TestChangeStreamResumability creates a verifier, starts its change stream,
// terminates that verifier, updates the source cluster, starts a new
// verifier with change stream, and confirms that things look as they should.
func (suite *IntegrationTestSuite) TestChangeStreamResumability() {
	func() {
		verifier1 := suite.BuildVerifier()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		err := verifier1.srcChangeStreamReader.StartChangeStream(ctx)
		suite.Require().NoError(err)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := suite.srcMongoClient.
		Database(suite.DBNameForTest()).
		Collection("testColl").
		InsertOne(
			ctx,
			bson.D{{"_id", "heyhey"}},
		)
	suite.Require().NoError(err)

	verifier2 := suite.BuildVerifier()

	suite.Require().Empty(
		suite.fetchVerifierRechecks(ctx, verifier2),
		"no rechecks should be enqueued before starting change stream",
	)

	newTime := suite.getClusterTime(ctx, suite.srcMongoClient)

	err = verifier2.srcChangeStreamReader.StartChangeStream(ctx)
	suite.Require().NoError(err)

	suite.Require().NotNil(verifier2.srcChangeStreamReader.startAtTs)

	suite.Assert().False(
		verifier2.srcChangeStreamReader.startAtTs.After(newTime),
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
			"db":         suite.DBNameForTest(),
			"coll":       "testColl",
			"generation": int32(0),
			"docID":      "heyhey",
		},
		recheckDocs[0]["_id"],
		"recheck doc should have expected ID",
	)
}

func (suite *IntegrationTestSuite) getClusterTime(ctx context.Context, client *mongo.Client) primitive.Timestamp {
	sess, err := client.StartSession()
	suite.Require().NoError(err, "should start session")

	sctx := mongo.NewSessionContext(ctx, sess)
	suite.Require().NoError(sess.Client().Ping(sctx, nil))

	newTime, err := getClusterTimeFromSession(sess)
	suite.Require().NoError(err, "should fetch cluster time")

	return newTime
}

func (suite *IntegrationTestSuite) fetchVerifierRechecks(ctx context.Context, verifier *Verifier) []bson.M {
	recheckDocs := []bson.M{}

	recheckColl := verifier.verificationDatabase().Collection(recheckQueue)
	cursor, err := recheckColl.Find(ctx, bson.D{})

	if !errors.Is(err, mongo.ErrNoDocuments) {
		suite.Require().NoError(err)
		suite.Require().NoError(cursor.All(ctx, &recheckDocs))
	}

	return recheckDocs
}

func (suite *IntegrationTestSuite) TestStartAtTimeNoChanges() {
	verifier := suite.BuildVerifier()
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
	err = verifier.srcChangeStreamReader.StartChangeStream(ctx)
	suite.Require().NoError(err)
	suite.Require().Equal(verifier.srcChangeStreamReader.startAtTs, origStartTs)
	verifier.srcChangeStreamReader.ChangeStreamWritesOffTsChan <- *origStartTs
	<-verifier.srcChangeStreamReader.ChangeStreamDoneChan
	suite.Require().Equal(verifier.srcChangeStreamReader.startAtTs, origStartTs)
}

func (suite *IntegrationTestSuite) TestStartAtTimeWithChanges() {
	verifier := suite.BuildVerifier()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 0}})
	suite.Require().NoError(err)

	origSessionTime := sess.OperationTime()
	suite.Require().NotNil(origSessionTime)
	err = verifier.srcChangeStreamReader.StartChangeStream(ctx)
	suite.Require().NoError(err)
	suite.Require().Equal(verifier.srcChangeStreamReader.startAtTs, origSessionTime)

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

	postEventsSessionTime := sess.OperationTime()
	suite.Require().NotNil(postEventsSessionTime)
	suite.Require().Negative(
		origSessionTime.Compare(*postEventsSessionTime),
		"session time after events should exceed the original",
	)

	verifier.srcChangeStreamReader.ChangeStreamWritesOffTsChan <- *postEventsSessionTime
	<-verifier.srcChangeStreamReader.ChangeStreamDoneChan

	suite.Assert().Equal(
		*postEventsSessionTime,
		*verifier.srcChangeStreamReader.startAtTs,
		"verifier.srcStartAtTs should now be our session timestamp",
	)
}

func (suite *IntegrationTestSuite) TestNoStartAtTime() {
	verifier := suite.BuildVerifier()
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
	err = verifier.srcChangeStreamReader.StartChangeStream(ctx)
	suite.Require().NoError(err)
	suite.Require().NotNil(verifier.srcChangeStreamReader.startAtTs)
	suite.Require().LessOrEqual(origStartTs.Compare(*verifier.srcChangeStreamReader.startAtTs), 0)
}

func (suite *IntegrationTestSuite) TestWithChangeEventsBatching() {
	verifier := suite.BuildVerifier()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	suite.Require().NoError(verifier.srcChangeStreamReader.StartChangeStream(ctx))

	_, err := suite.srcMongoClient.Database("testDb").Collection("testColl1").InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl1").InsertOne(ctx, bson.D{{"_id", 2}})
	suite.Require().NoError(err)

	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl2").InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)

	var rechecks []bson.M
	require.Eventually(
		suite.T(),
		func() bool {
			rechecks = suite.fetchVerifierRechecks(ctx, verifier)
			return len(rechecks) == 3
		},
		time.Minute,
		500*time.Millisecond,
		"the verifier should flush a recheck doc after a batch",
	)
}

func (suite *IntegrationTestSuite) TestEventBeforeWritesOff() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()

	db := suite.srcMongoClient.Database(suite.DBNameForTest())
	coll := db.Collection("mycoll")
	suite.Require().NoError(
		db.CreateCollection(ctx, coll.Name()),
	)

	// start verifier
	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)

	// wait for generation 0 to end
	verifierRunner.AwaitGenerationEnd()

	docsCount := 10_000
	docs := lo.RepeatBy(docsCount, func(_ int) bson.D { return bson.D{} })
	_, err := coll.InsertMany(
		ctx,
		lo.ToAnySlice(docs),
	)
	suite.Require().NoError(err)

	suite.Require().NoError(verifier.WritesOff(ctx))

	suite.Require().NoError(verifierRunner.Await())

	generation := verifier.generation
	failedTasks, incompleteTasks, err := FetchFailedAndIncompleteTasks(
		ctx,
		verifier.verificationTaskCollection(),
		verificationTaskVerifyDocuments,
		generation,
	)
	suite.Require().NoError(err)

	suite.Require().Empty(incompleteTasks, "all tasks should be finished")

	totalFailed := lo.Reduce(
		failedTasks,
		func(sofar int, task VerificationTask, _ int) int {
			return sofar + len(task.Ids)
		},
		0,
	)

	suite.Assert().Equal(docsCount, totalFailed, "all source docs should be missing")
}
