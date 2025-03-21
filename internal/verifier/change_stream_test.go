package verifier

import (
	"context"
	"io"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (suite *IntegrationTestSuite) TestChangeStreamFilter_NoNamespaces() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()

	filter := verifier.srcChangeStreamReader.GetChangeStreamFilter()

	_, err := suite.srcMongoClient.
		Database("realUserDatabase").
		Collection("foo").
		InsertOne(ctx, bson.D{{"id", 123}})
	suite.Require().NoError(err)

	cs, err := suite.srcMongoClient.Watch(ctx, filter)
	suite.Require().NoError(err)

	defer cs.Close(ctx)

	dbsToIgnore := []string{
		metaDBName,
		"mongosync_reserved_for_internal_use",
		"mongosync_internal_foo",
	}
	for _, dbname := range dbsToIgnore {
		_, err := suite.srcMongoClient.
			Database(dbname).
			Collection("foo").
			InsertOne(ctx, bson.D{})
		suite.Require().NoError(err)
	}

	_, err = suite.srcMongoClient.
		Database("realUserDatabase").
		Collection("foo").
		UpdateOne(
			ctx,
			bson.D{{"id", 123}},
			bson.D{{"$set", bson.D{{"foo", 123}}}})
	suite.Require().NoError(err)

	timedCtx, cancel := context.WithTimeoutCause(
		ctx,
		time.Minute,
		errors.New("should have gotten an event"),
	)
	defer cancel()
	cs.Next(timedCtx) // No need to check this since we check cs.Err()
	suite.Require().NoError(cs.Err())

	event := bson.M{}
	suite.Require().NoError(cs.Decode(&event))

	suite.Assert().Equal(
		"update",
		event["operationType"],
		"only the update should show in the change stream (event: %v)",
		event,
	)
	suite.Assert().NotContains(
		event,
		"updateDescription",
		"update description should be filtered out",
	)

	/*
	   suite.Assert().Contains(

	   	,
	   	bson.D{
	   		{"$match", bson.D{{"ns.db", bson.D{{"$ne", metaDBName}}}}},
	   	},

	   )
	   verifier.srcChangeStreamReader.namespaces = []string{"foo.bar", "foo.baz", "test.car", "test.chaz"}
	   suite.Assert().Contains(

	   	verifier.srcChangeStreamReader.GetChangeStreamFilter(),
	   	bson.D{{"$match", bson.D{
	   		{"$or", []bson.D{
	   			{{"ns", bson.D{{"db", "foo"}, {"coll", "bar"}}}},
	   			{{"ns", bson.D{{"db", "foo"}, {"coll", "baz"}}}},
	   			{{"ns", bson.D{{"db", "test"}, {"coll", "car"}}}},
	   			{{"ns", bson.D{{"db", "test"}, {"coll", "chaz"}}}},
	   		}},
	   	}}},

	   )
	*/
}

func (suite *IntegrationTestSuite) TestChangeStreamFilter_WithNamespaces() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()
	verifier.srcChangeStreamReader.namespaces = []string{
		"foo.bar",
		"foo.baz",
		"test.car",
		"test.chaz",
	}

	filter := verifier.srcChangeStreamReader.GetChangeStreamFilter()

	cs, err := suite.srcMongoClient.Watch(ctx, filter)
	suite.Require().NoError(err)

	defer cs.Close(ctx)

	dbsToIgnore := []string{
		metaDBName,
		"mongosync_reserved_for_internal_use",
		"mongosync_internal_foo",
	}
	for _, dbname := range dbsToIgnore {
		_, err := suite.srcMongoClient.
			Database(dbname).
			Collection("foo").
			InsertOne(ctx, bson.D{})
		suite.Require().NoError(err)
	}

	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)

	for _, ns := range verifier.srcChangeStreamReader.namespaces {
		dbAndColl := strings.Split(ns, ".")

		_, err := suite.srcMongoClient.
			Database(dbAndColl[0]).
			Collection(dbAndColl[1]).
			InsertOne(sctx, bson.D{})
		suite.Require().NoError(err)
	}

	changeStreamStopTime := sess.OperationTime()

	suite.T().Logf("Insert op time: %v", changeStreamStopTime)

	events := []bson.M{}

	for {
		gotEvent := cs.TryNext(ctx)
		suite.Require().NoError(cs.Err())
		csOpTime, err := extractTimestampFromResumeToken(cs.ResumeToken())
		suite.Require().NoError(err, "should get timestamp from resume token")

		if gotEvent {
			var newEvent bson.M
			suite.Require().NoError(cs.Decode(&newEvent))
			events = append(events, newEvent)
		}

		if csOpTime.After(*changeStreamStopTime) {
			break
		}
	}

	suite.Assert().Len(
		events,
		len(verifier.srcChangeStreamReader.namespaces),
		"should have 1 event per in-filter namespace",
	)
	suite.Assert().True(
		lo.EveryBy(
			events,
			func(evt bson.M) bool {
				return evt["operationType"] == "insert"
			},
		),
		"each event should be insert: %v", events,
	)
}

func (suite *IntegrationTestSuite) startSrcChangeStreamReaderAndHandler(ctx context.Context, verifier *Verifier) {
	err := verifier.srcChangeStreamReader.StartChangeStream(ctx)
	suite.Require().NoError(err)
	go func() {
		err := verifier.RunChangeEventHandler(ctx, verifier.srcChangeStreamReader)
		if errors.Is(err, context.Canceled) {
			return
		}
		suite.Require().NoError(err)
	}()
}

// TestChangeStreamResumability creates a verifier, starts its change stream,
// terminates that verifier, updates the source cluster, starts a new
// verifier with change stream, and confirms that things look as they should.
func (suite *IntegrationTestSuite) TestChangeStreamResumability() {
	suite.Require().NoError(
		suite.srcMongoClient.
			Database(suite.DBNameForTest()).
			CreateCollection(suite.Context(), "testColl"),
	)

	func() {
		verifier1 := suite.BuildVerifier()
		ctx, cancel := context.WithCancel(suite.Context())
		defer cancel()
		suite.startSrcChangeStreamReaderAndHandler(ctx, verifier1)
	}()

	ctx, cancel := context.WithCancel(suite.Context())
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

	suite.startSrcChangeStreamReaderAndHandler(ctx, verifier2)

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

	newTime, err := util.GetClusterTimeFromSession(sess)
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

func (suite *IntegrationTestSuite) TestChangeStreamDDLError() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	db := suite.srcMongoClient.
		Database(suite.DBNameForTest())

	verifier := suite.BuildVerifier()

	suite.Require().NoError(
		db.CreateCollection(ctx, "mycoll"),
	)

	verifier.SetSrcNamespaces([]string{db.Name() + ".mycoll"})
	verifier.SetDstNamespaces([]string{db.Name() + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(
		verifierRunner.AwaitGenerationEnd(),
	)

	suite.Require().NoError(db.Collection("mycoll").Drop(ctx))

	err := verifierRunner.Await()

	suite.Require().ErrorContains(err, "drop")
	suite.Require().ErrorContains(err, db.Name())
	suite.Require().ErrorContains(err, "mycoll")
}

func (suite *IntegrationTestSuite) TestChangeStreamLag() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	db := suite.srcMongoClient.
		Database(suite.DBNameForTest())

	suite.Require().NoError(
		db.CreateCollection(ctx, "mycoll"),
	)

	verifier := suite.BuildVerifier()

	verifier.SetSrcNamespaces([]string{db.Name() + ".mycoll"})
	verifier.SetDstNamespaces([]string{db.Name() + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(
		verifierRunner.AwaitGenerationEnd(),
	)

	_, err := db.Collection("mycoll").InsertOne(ctx, bson.D{})
	suite.Require().NoError(err)

	// On sharded clusters sometimes the event hasn’t shown yet.
	suite.Require().Eventually(
		func() bool {
			suite.Require().NoError(
				verifierRunner.StartNextGeneration(),
			)
			suite.Require().NoError(
				verifierRunner.AwaitGenerationEnd(),
			)

			return verifier.srcChangeStreamReader.GetLag().IsSome()
		},
		time.Minute,
		100*time.Millisecond,
	)

	// NB: The lag will include whatever time elapsed above before
	// verifier read the event, so it can be several seconds.
	suite.Assert().Less(
		verifier.srcChangeStreamReader.GetLag().MustGet(),
		10*time.Minute,
		"verifier lag is as expected",
	)
}

func (suite *IntegrationTestSuite) TestStartAtTimeNoChanges() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	// Each of these takes ~1s, so don’t do too many of them.
	for range 5 {
		verifier := suite.BuildVerifier()
		ctx := suite.Context()
		sess, err := suite.srcMongoClient.StartSession()
		suite.Require().NoError(err)
		sctx := mongo.NewSessionContext(ctx, sess)
		_, err = suite.srcMongoClient.
			Database(suite.DBNameForTest()).
			Collection("testColl").
			InsertOne(sctx, bson.D{})
		suite.Require().NoError(err, "should insert doc")

		insertTs, err := util.GetClusterTimeFromSession(sess)
		suite.Require().NoError(err, "should get cluster time")

		suite.startSrcChangeStreamReaderAndHandler(ctx, verifier)

		startAtTs := verifier.srcChangeStreamReader.startAtTs
		suite.Require().NotNil(startAtTs, "startAtTs should be set")

		verifier.srcChangeStreamReader.writesOffTs.Set(insertTs)

		<-verifier.srcChangeStreamReader.doneChan

		suite.Require().False(
			verifier.srcChangeStreamReader.startAtTs.Before(*startAtTs),
			"new startAtTs (%+v) should be no earlier than last one (%+v)",
			verifier.srcChangeStreamReader.startAtTs,
			*startAtTs,
		)
	}
}

func (suite *IntegrationTestSuite) TestStartAtTimeWithChanges() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()
	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 0}})
	suite.Require().NoError(err)

	origSessionTime := sess.OperationTime()
	suite.Require().NotNil(origSessionTime)
	suite.startSrcChangeStreamReaderAndHandler(ctx, verifier)

	// srcStartAtTs derives from the change stream’s resume token, which can
	// postdate our session time but should not precede it.
	suite.Require().False(
		verifier.srcChangeStreamReader.startAtTs.Before(*origSessionTime),
		"srcStartAtTs should be >= the insert’s optime",
	)

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

	verifier.srcChangeStreamReader.writesOffTs.Set(*postEventsSessionTime)
	<-verifier.srcChangeStreamReader.doneChan

	suite.Assert().Equal(
		*postEventsSessionTime,
		*verifier.srcChangeStreamReader.startAtTs,
		"verifier.srcStartAtTs should now be our session timestamp",
	)
}

func (suite *IntegrationTestSuite) TestNoStartAtTime() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()
	sess, err := suite.srcMongoClient.StartSession()
	suite.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)
	_, err = suite.srcMongoClient.Database("testDb").Collection("testColl").InsertOne(
		sctx, bson.D{{"_id", 0}})
	suite.Require().NoError(err)
	origStartTs := sess.OperationTime()
	suite.Require().NotNil(origStartTs)
	suite.startSrcChangeStreamReaderAndHandler(ctx, verifier)
	suite.Require().NotNil(verifier.srcChangeStreamReader.startAtTs)
	suite.Require().LessOrEqual(origStartTs.Compare(*verifier.srcChangeStreamReader.startAtTs), 0)
}

func (suite *IntegrationTestSuite) TestWithChangeEventsBatching() {
	ctx := suite.Context()

	db := suite.srcMongoClient.Database(suite.DBNameForTest())
	coll1 := db.Collection("testColl1")
	coll2 := db.Collection("testColl2")

	for _, coll := range mslices.Of(coll1, coll2) {
		suite.Require().NoError(db.CreateCollection(ctx, coll.Name()))
	}

	verifier := suite.BuildVerifier()

	suite.startSrcChangeStreamReaderAndHandler(ctx, verifier)

	_, err := coll1.InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	_, err = coll1.InsertOne(ctx, bson.D{{"_id", 2}})
	suite.Require().NoError(err)

	_, err = coll2.InsertOne(ctx, bson.D{{"_id", 1}})
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

func (suite *IntegrationTestSuite) TestWritesOffCursorKilledResilience() {
	ctx := suite.Context()

	coll := suite.srcMongoClient.
		Database(suite.DBNameForTest()).
		Collection("mycoll")

	suite.Require().NoError(
		coll.Database().CreateCollection(
			ctx,
			coll.Name(),
		),
	)

	suite.Require().NoError(
		suite.dstMongoClient.
			Database(coll.Database().Name()).
			CreateCollection(
				ctx,
				coll.Name(),
			),
	)

	for range 100 {
		verifier := suite.BuildVerifier()

		docs := lo.RepeatBy(1_000, func(_ int) bson.D { return bson.D{} })
		_, err := coll.InsertMany(
			ctx,
			lo.ToAnySlice(docs),
		)
		suite.Require().NoError(err)

		suite.Require().NoError(verifier.WritesOff(ctx))

		suite.Require().NoError(
			testutil.KillApplicationChangeStreams(
				suite.Context(),
				suite.T(),
				suite.srcMongoClient,
				clientAppName,
			),
		)
	}
}

func (suite *IntegrationTestSuite) TestCursorKilledResilience() {
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
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	suite.Require().NoError(
		testutil.KillApplicationChangeStreams(
			suite.Context(),
			suite.T(),
			suite.srcMongoClient,
			clientAppName,
		),
	)

	_, err := coll.InsertOne(
		ctx,
		bson.D{{"_id", "after kill"}},
	)
	suite.Require().NoError(err)

	suite.Require().NoError(verifier.WritesOff(ctx))

	suite.Require().NoError(verifierRunner.Await())

	failedTasks, incompleteTasks, err := FetchFailedAndIncompleteTasks(
		ctx,
		verifier.logger,
		verifier.verificationTaskCollection(),
		verificationTaskVerifyDocuments,
		verifier.generation,
	)
	suite.Require().NoError(err)

	suite.Assert().Zero(incompleteTasks, "no incomplete tasks")
	suite.Require().Len(failedTasks, 1, "expect one failed task")
}

func (suite *IntegrationTestSuite) TestManyInsertsBeforeWritesOff() {
	suite.testInsertsBeforeWritesOff(10_000)
}

func (suite *IntegrationTestSuite) TestOneInsertBeforeWritesOff() {
	suite.testInsertsBeforeWritesOff(1)
}

func (suite *IntegrationTestSuite) testInsertsBeforeWritesOff(docsCount int) {
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
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

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
		verifier.logger,
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

func (suite *IntegrationTestSuite) TestCreateForbidden() {
	ctx := suite.Context()
	buildInfo, err := util.GetClusterInfo(ctx, suite.srcMongoClient)
	suite.Require().NoError(err)

	if buildInfo.VersionArray[0] < 6 {
		suite.T().Skipf("This test requires server v6+. (Found: %v)", buildInfo.VersionArray)
	}

	verifier := suite.BuildVerifier()

	// start verifier
	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)

	// wait for generation 0 to end
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	db := suite.srcMongoClient.Database(suite.DBNameForTest())
	coll := db.Collection("mycoll")
	suite.Require().NoError(
		db.CreateCollection(ctx, coll.Name()),
	)

	// The error from the create event will come either at WritesOff
	// or when we finalize the change stream.
	err = verifier.WritesOff(ctx)
	if err == nil {
		err = verifierRunner.Await()
	}

	suite.Require().Error(err, "should detect forbidden create event")

	eventErr := UnknownEventError{}
	suite.Require().ErrorAs(err, &eventErr)
	suite.Assert().Equal("create", eventErr.Event.Lookup("operationType").StringValue())
}

func (suite *IntegrationTestSuite) TestTolerateDestinationCollMod() {
	ctx := suite.Context()
	buildInfo, err := util.GetClusterInfo(ctx, suite.dstMongoClient)
	suite.Require().NoError(err)

	if buildInfo.VersionArray[0] < 6 {
		suite.T().Skipf("This test requires dst server v6+. (Found: %v)", buildInfo.VersionArray)
	}

	db := suite.srcMongoClient.Database(suite.DBNameForTest())
	coll := db.Collection("mycoll")
	suite.Require().NoError(
		db.CreateCollection(
			ctx,
			coll.Name(),
			options.CreateCollection().
				SetCapped(true).
				SetSizeInBytes(123123).
				SetMaxDocuments(1000),
		),
	)

	verifier := suite.BuildVerifier()

	logBuffer := &strings.Builder{}

	multiOut := io.MultiWriter(
		logger.DefaultLogWriter,
		logBuffer,
	)

	zlog := verifier.logger.Logger.Output(multiOut)
	verifier.logger = logger.NewLogger(&zlog, multiOut)

	// start verifier
	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)

	// wait for generation 0 to end
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	suite.Require().NoError(
		suite.dstMongoClient.
			Database(suite.DBNameForTest()).
			RunCommand(
				ctx,
				bson.D{
					{"collMod", "mycoll"},
					{"cappedSize", 1001},
				},
			).Err(),
		"should alter capped size",
	)

	err = verifier.WritesOff(ctx)
	if err == nil {
		err = verifierRunner.Await()
	}

	suite.Require().NoError(err, "should get no error")

	suite.Assert().Contains(
		logBuffer.String(),
		"cappedSize",
		"modify event should be recorded in log",
	)
}

func (suite *IntegrationTestSuite) TestRecheckDocsWithDstChangeEvents() {
	ctx := suite.Context()

	srcDBName := suite.DBNameForTest("src")
	dstDBName := suite.DBNameForTest("dst")

	db := suite.dstMongoClient.Database(dstDBName)
	coll1 := db.Collection("dstColl1")
	coll2 := db.Collection("dstColl2")

	for _, coll := range mslices.Of(coll1, coll2) {
		suite.Require().NoError(db.CreateCollection(ctx, coll.Name()))
	}

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{srcDBName + ".srcColl1", srcDBName + ".srcColl2"})
	verifier.SetDstNamespaces([]string{dstDBName + ".dstColl1", dstDBName + ".dstColl2"})
	verifier.SetNamespaceMap()

	suite.Require().NoError(verifier.dstChangeStreamReader.StartChangeStream(ctx))
	go func() {
		err := verifier.RunChangeEventHandler(ctx, verifier.dstChangeStreamReader)
		if errors.Is(err, context.Canceled) {
			return
		}
		suite.Require().NoError(err)
	}()

	_, err := coll1.InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)
	_, err = coll1.InsertOne(ctx, bson.D{{"_id", 2}})
	suite.Require().NoError(err)

	_, err = coll2.InsertOne(ctx, bson.D{{"_id", 1}})
	suite.Require().NoError(err)

	var rechecks []RecheckDoc
	require.Eventually(
		suite.T(),
		func() bool {
			recheckColl := verifier.verificationDatabase().Collection(recheckQueue)
			cursor, err := recheckColl.Find(ctx, bson.D{})
			if errors.Is(err, mongo.ErrNoDocuments) {
				return false
			}

			suite.Require().NoError(err)
			suite.Require().NoError(cursor.All(ctx, &rechecks))
			return len(rechecks) == 3
		},
		time.Minute,
		500*time.Millisecond,
		"the verifier should flush a recheck doc after a batch",
	)

	coll1RecheckCount, coll2RecheckCount := 0, 0
	for _, recheck := range rechecks {
		suite.Require().Equal(srcDBName, recheck.PrimaryKey.SrcDatabaseName)
		switch recheck.PrimaryKey.SrcCollectionName {
		case "srcColl1":
			coll1RecheckCount++
		case "srcColl2":
			coll2RecheckCount++
		default:
			suite.T().Fatalf("unknown collection name: %v", recheck.PrimaryKey.SrcCollectionName)
		}
	}
	suite.Require().Equal(2, coll1RecheckCount)
	suite.Require().Equal(1, coll2RecheckCount)
}

func (suite *IntegrationTestSuite) TestLargeEvents() {
	ctx := suite.Context()

	docID := 123

	makeDoc := func(char string, len int) bson.D {
		return bson.D{{"_id", docID}, {"str", strings.Repeat(char, len)}}
	}

	smallDoc := testutil.MustMarshal(makeDoc("a", 1))
	suite.T().Logf("small size: %v", len(smallDoc))
	maxBSONSize := 16 * 1024 * 1024

	maxStringLen := maxBSONSize - len(smallDoc) - 1

	db := suite.srcMongoClient.Database(suite.DBNameForTest())
	suite.Require().NoError(db.CreateCollection(ctx, "mystuff"))

	verifier := suite.BuildVerifier()
	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	coll := db.Collection("mystuff")
	_, err := coll.InsertOne(
		ctx,
		makeDoc("a", maxStringLen),
	)
	suite.Require().NoError(err, "should insert")

	updated, err := coll.UpdateByID(
		ctx,
		docID,
		bson.D{
			{"$set", bson.D{
				// smallDoc happens to be the minimum length to subtract
				// in order to satisfy the server’s requirements on
				// document sizes in updates.
				{"str", strings.Repeat("b", maxStringLen-len(smallDoc))},
			}},
		},
	)
	suite.Require().NoError(err, "should update")
	suite.Require().EqualValues(1, updated.ModifiedCount)

	replaced, err := coll.ReplaceOne(
		ctx,
		bson.D{{"_id", docID}},
		makeDoc("c", maxStringLen-len(smallDoc)),
	)
	suite.Require().NoError(err, "should replace")
	suite.Require().EqualValues(1, replaced.ModifiedCount)

	suite.Require().NoError(verifier.WritesOff(ctx))
	suite.Require().NoError(verifierRunner.Await())
}

// TestDropMongosyncDB verifies that writes to Mongosync's
// metadata don’t affect migration-verifier.
func (suite *IntegrationTestSuite) TestDropMongosyncDB() {
	ctx := suite.Context()

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	verifier := suite.BuildVerifier()

	dbs := []string{
		"mongosync_reserved_for_internal_use",
		"mongosync_internal_foo",
	}

	for _, dbname := range dbs {
		suite.Require().NoError(
			suite.dstMongoClient.
				Database(dbname).
				CreateCollection(ctx, "foo"),
		)
	}

	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	for _, dbname := range dbs {
		_, err := suite.dstMongoClient.
			Database(dbname).
			Collection("foo").
			InsertOne(ctx, bson.D{})
		suite.Require().NoError(err)

		suite.Require().NoError(
			suite.dstMongoClient.
				Database(dbname).
				Drop(ctx),
		)
	}

	suite.Require().NoError(verifier.WritesOff(ctx))

	suite.Require().NoError(runner.Await())
}
