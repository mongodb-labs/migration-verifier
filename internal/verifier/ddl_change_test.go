package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (suite *IntegrationTestSuite) TestDDLChangeEvents_Shard() {
	if suite.GetTopology(suite.srcMongoClient) != util.TopologySharded {
		suite.T().Skip("need sharded source")
	}

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	dbName := suite.DBNameForTest()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		db := client.Database(dbName)
		coll := db.Collection("mycoll")
		suite.Require().NoError(db.CreateCollection(ctx, coll.Name()))
	}

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	suite.Require().NoError(
		suite.srcMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}}},
			},
		).Err(),
		"should shard collection",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch",
	)

	suite.Require().NoError(
		suite.dstMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}}},
			},
		).Err(),
		"should shard collection on dst",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see metadata matches after dst shardCollection",
	)
}

func (suite *IntegrationTestSuite) TestDDLChangeEvents_ShardKeyMismatch() {
	if suite.GetTopology(suite.srcMongoClient) != util.TopologySharded {
		suite.T().Skip("need sharded source")
	}

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	dbName := suite.DBNameForTest()

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		_, err := client.Database(dbName).Collection("mycoll").Indexes().CreateMany(
			ctx,
			[]mongo.IndexModel{
				{
					Keys: bson.D{{"foo", 1}},
				},
				{
					Keys: bson.D{{"foo", 1}, {"bar", 1}},
				},
			},
		)
		suite.Require().NoError(err, "should create indexes on %v", client)
	}

	suite.Require().NoError(
		suite.srcMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}}},
			},
		).Err(),
		"should shard source ",
	)

	suite.Require().NoError(
		suite.dstMongoClient.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", dbName + ".mycoll"},
				{"key", bson.D{{"foo", 1}, {"bar", 1}}},
			},
		).Err(),
		"should shard destination with different shard key",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch",
	)
}

func (suite *IntegrationTestSuite) TestDDLChangeEvents_Index() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()

	dbName := suite.DBNameForTest()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		db := client.Database(dbName)
		coll := db.Collection("mycoll")
		suite.Require().NoError(db.CreateCollection(ctx, coll.Name()))
	}

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	indexKey := bson.D{
		{"foo", 1},
		{"barbar", 1},
	}

	indexName, err := suite.srcMongoClient.Database(dbName).Collection("mycoll").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: indexKey,
		},
	)
	suite.Require().NoError(err, "should create src index")

	suite.T().Logf("created index: %s", indexName)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch",
	)

	_, err = suite.dstMongoClient.Database(dbName).Collection("mycoll").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: indexKey,
		},
	)
	suite.Require().NoError(err, "should create dst index")

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks == 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see no metadata mismatches after dst index creation",
	)

	suite.Require().NoError(
		suite.srcMongoClient.Database(dbName).RunCommand(
			ctx,
			bson.D{
				{"collMod", "mycoll"},
				{"index", bson.D{
					{"name", indexName},
					{"hidden", true},
				}},
			},
		).Err(),
		"should hide index on source",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see a metadata mismatch after hiding source index",
	)

	suite.Require().NoError(
		suite.dstMongoClient.Database(dbName).RunCommand(
			ctx,
			bson.D{
				{"collMod", "mycoll"},
				{"index", bson.D{
					{"name", indexName},
					{"hidden", true},
				}},
			},
		).Err(),
		"should hide index on destination",
	)

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.MetadataMismatchTasks == 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should see no metadata mismatches after hiding dst index",
	)
}

// TestDDLRecheck_NullIDNoInteraction verifies that a DDL recheck (no document
// ID) and a document recheck whose _id is BSON null are treated as entirely
// separate concerns: the DDL recheck must produce a VerifyCollection task and
// the null-_id recheck must produce a VerifyDocuments task whose sole ID is
// null.  Neither must bleed into the other.
func (suite *IntegrationTestSuite) TestDDLRecheck_NullIDNoInteraction() {
	ctx := suite.Context()
	verifier := suite.BuildVerifier()

	const dbName, collName = "testDB", "testColl"

	// Enqueue a DDL recheck (no document ID) — simulates a createIndexes event.
	suite.Require().NoError(
		verifier.insertRecheckDocs(
			ctx,
			[]string{dbName},
			[]string{collName},
			[]option.Option[bson.RawValue]{option.None[bson.RawValue]()},
			[]int32{0},
			nil,
			option.Some(src),
			[]bson.Timestamp{{T: 123}},
		),
		"insert DDL recheck",
	)

	// Enqueue a document recheck for _id=null.
	suite.Require().NoError(
		verifier.insertRecheckDocs(
			ctx,
			[]string{dbName},
			[]string{collName},
			[]option.Option[bson.RawValue]{option.Some(bson.RawValue{Type: bson.TypeNull})},
			[]int32{10},
			nil,
			option.Some(src),
			[]bson.Timestamp{{T: 124}},
		),
		"insert null-_id document recheck",
	)

	verifier.generation++
	suite.Require().NoError(
		verifier.GenerateRecheckTasks(ctx, &testutil.MockSuccessNotifier{}),
	)

	taskColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
	cursor, err := taskColl.Find(ctx, bson.D{})
	suite.Require().NoError(err)

	var foundTasks []tasks.Task
	suite.Require().NoError(cursor.All(ctx, &foundTasks))

	suite.Require().Len(foundTasks, 2, "should produce exactly one task per recheck type")

	var collTask, docTask *tasks.Task
	for i := range foundTasks {
		switch foundTasks[i].Type {
		case tasks.VerifyCollection:
			collTask = &foundTasks[i]
		case tasks.VerifyDocuments:
			docTask = &foundTasks[i]
		}
	}

	suite.Require().NotNil(collTask, "DDL recheck should produce a VerifyCollection task")
	suite.Require().NotNil(docTask, "null-_id recheck should produce a VerifyDocuments task")

	if suite.Assert().Len(docTask.Ids, 1, "document task should contain only the null _id") {
		suite.Assert().Equal(bson.TypeNull, docTask.Ids[0].Type,
			"document task's ID should have BSON type null")
	}
}

// TestDDLChangeEvents_NewCollectionWithDocs verifies that when a collection is
// created on the source after the verifier has started, and documents are
// inserted into it, the verifier detects the mismatch (source has the
// collection and documents, destination does not).  It then verifies that once
// the destination is brought into sync the mismatch is resolved.
func (suite *IntegrationTestSuite) TestDDLChangeEvents_NewCollectionWithDocs() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	// Neither collection exists yet — gen 0 should pass cleanly.
	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	suite.T().Log("--- gen 0 passed with no collections present")

	// Create the collection and insert documents on source only.
	srcColl := suite.srcMongoClient.Database(dbName).Collection("mycoll")
	suite.Require().NoError(
		suite.srcMongoClient.Database(dbName).CreateCollection(ctx, "mycoll"),
	)
	_, err := srcColl.InsertMany(ctx, []any{
		bson.D{{"_id", 1}, {"x", "a"}},
		bson.D{{"_id", 2}, {"x", "b"}},
	})
	suite.Require().NoError(err, "insert documents on source")

	suite.T().Log("--- created src collection with documents")

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.FailedTasks > 0 || status.MetadataMismatchTasks > 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should detect mismatch when source has collection+docs that destination lacks",
	)

	suite.T().Log("--- mismatch detected")

	// Fix destination: create matching collection with the same documents.
	dstColl := suite.dstMongoClient.Database(dbName).Collection("mycoll")
	suite.Require().NoError(
		suite.dstMongoClient.Database(dbName).CreateCollection(ctx, "mycoll"),
	)
	_, err = dstColl.InsertMany(ctx, []any{
		bson.D{{"_id", 1}, {"x", "a"}},
		bson.D{{"_id", 2}, {"x", "b"}},
	})
	suite.Require().NoError(err, "insert matching documents on destination")

	suite.T().Log("--- created dst collection with matching documents")

	suite.Assert().Eventually(
		func() bool {
			status, err := verifier.GetVerificationStatus(ctx)
			suite.Require().NoError(err)

			if status.FailedTasks == 0 && status.MetadataMismatchTasks == 0 {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

			return false
		},
		time.Minute,
		time.Second,
		"should resolve all mismatches once destination matches source",
	)
}

// TestDDLChangeEvents_LastRecheckedTimestampUpdated verifies that after a DDL
// change event triggers a VerifyCollection recheck and that task is processed,
// srcLastRecheckedTS is updated to the DDL event's optime, while
// dstLastRecheckedTS stays zero (no dst DDL event occurred).
func (suite *IntegrationTestSuite) TestDDLChangeEvents_LastRecheckedTimestampUpdated() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.Context()
	dbName := suite.DBNameForTest()

	for _, client := range mslices.Of(suite.srcMongoClient, suite.dstMongoClient) {
		suite.Require().NoError(client.Database(dbName).CreateCollection(ctx, "mycoll"))
	}

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces([]string{dbName + ".mycoll"})
	verifier.SetDstNamespaces([]string{dbName + ".mycoll"})
	verifier.SetNamespaceMap()

	verifierRunner := RunVerifierCheck(suite.Context(), suite.T(), verifier)
	suite.Require().NoError(verifierRunner.AwaitGenerationEnd())

	// Create an index on the source only — triggers a DDL change event.
	_, err := suite.srcMongoClient.Database(dbName).Collection("mycoll").Indexes().CreateOne(
		ctx,
		mongo.IndexModel{
			Keys: bson.D{{"x", 1}},
		},
	)
	suite.Require().NoError(err, "should create src index")

	// Run generations until srcLastRecheckedTS becomes non-zero, confirming
	// that the DDL VerifyCollection task propagated the event optime through
	// work() → NoteCompareOfOptime.
	suite.Assert().Eventually(
		func() bool {
			var srcTS bson.Timestamp
			verifier.srcLastRecheckedTS.Load(func(t bson.Timestamp) {
				srcTS = t
			})
			if !srcTS.IsZero() {
				return true
			}

			suite.Require().NoError(verifierRunner.StartNextGeneration())
			suite.Require().NoError(verifierRunner.AwaitGenerationEnd())
			return false
		},
		time.Minute,
		time.Second,
		"srcLastRecheckedTS should be non-zero after DDL recheck is processed",
	)

	// No dst DDL event occurred, so dstLastRecheckedTS must remain zero.
	var dstTS bson.Timestamp
	verifier.dstLastRecheckedTS.Load(func(t bson.Timestamp) {
		dstTS = t
	})
	suite.Assert().True(dstTS.IsZero(), "dstLastRecheckedTS should remain zero when only src has a DDL event")
}
