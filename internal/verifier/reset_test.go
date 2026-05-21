package verifier

import (
	"fmt"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options" //nolint:staticcheck
)

func (suite *IntegrationTestSuite) TestResetPrimaryTask() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()

	created, err := verifier.CreatePrimaryTaskIfNeeded(ctx)
	suite.Require().NoError(err)
	suite.Require().True(created)

	_, err = verifier.InsertCollectionVerificationTask(ctx, "foo.bar")
	suite.Require().NoError(err)

	err = verifier.ResetInProgressTasks(ctx)
	suite.Require().NoError(err)

	tasksColl := verifier.verificationTaskCollection()
	cursor, err := tasksColl.Find(ctx, bson.M{})
	suite.Require().NoError(err)
	var taskDocs []bson.M
	suite.Require().NoError(cursor.All(ctx, &taskDocs))

	suite.Assert().Len(taskDocs, 1)
}

func (suite *IntegrationTestSuite) TestResetNonPrimaryTasks() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()

	// Create a primary task, and set it to complete.
	created, err := verifier.CreatePrimaryTaskIfNeeded(ctx)
	suite.Require().NoError(err)
	suite.Require().True(created)

	suite.Require().NoError(verifier.UpdatePrimaryTaskComplete(ctx))

	ns1 := "foo.bar"
	ns2 := "qux.quux"

	// Create a collection-verification task, and set it to processing.
	collTask, err := verifier.InsertCollectionVerificationTask(ctx, ns1)
	suite.Require().NoError(err)

	collTask.Status = tasks.Processing

	suite.Require().NoError(
		verifier.UpdateVerificationTask(ctx, collTask),
	)

	// Create three partition tasks with the same namespace as the
	// collection-verification task (status=[added, processing, completed]),
	// and another for a different namespace that’s completed.
	for _, taskParts := range []struct {
		Status    tasks.Status
		Namespace string
	}{
		{tasks.Added, ns1},
		{tasks.Processing, ns1},
		{tasks.Completed, ns1},
		{tasks.Added, ns2},
		{tasks.Processing, ns2},
		{tasks.Completed, ns2},
	} {
		task, err := verifier.InsertPartitionVerificationTask(
			ctx,
			&partitions.Partition{
				Ns: &partitions.Namespace{
					DB:   strings.Split(taskParts.Namespace, ".")[0],
					Coll: strings.Split(taskParts.Namespace, ".")[1],
				},
				Key: partitions.PartitionKey{
					Lower: bsontools.ToRawValue(bson.MinKey{}),
				},
				Upper: bsontools.ToRawValue(bson.MaxKey{}),
			},
			nil,
			taskParts.Namespace,
		)
		suite.Require().NoError(err)

		task.Status = taskParts.Status
		suite.Require().NoError(
			verifier.UpdateVerificationTask(ctx, task),
		)
	}

	// Reset tasks
	err = verifier.ResetInProgressTasks(ctx)
	suite.Require().NoError(err)

	orderedTypes := mslices.Of(
		tasks.Primary,
		tasks.VerifyDocuments,
		tasks.VerifyCollection,
	)

	// Contents should just be the primary task and
	// the completed partition-level.
	tasksColl := verifier.verificationTaskCollection()
	cursor, err := tasksColl.Aggregate(
		ctx,
		append(
			append(
				mongo.Pipeline{
					{{"$match", bson.D{
						{"type", bson.D{{"$in", orderedTypes}}},
					}}},
				},
				bson.D{
					{"$sort", bson.D{
						{"query_filter.namespace", 1},
						{"status", 1},
					}},
				},
			),
			testutil.SortByListAgg("type", orderedTypes)...,
		),
	)
	suite.Require().NoError(err)

	var taskDocs []tasks.Task
	suite.Require().NoError(cursor.All(ctx, &taskDocs))

	suite.Require().Len(taskDocs, 5)

	// The tasks that should remain are:

	// the primary (completed)
	suite.Assert().Equal(
		tasks.Primary,
		taskDocs[0].Type,
	)

	// the 2 ns2 partition tasks that weren’t completed (both “added”)
	suite.Assert().Equal(
		tasks.VerifyDocuments,
		taskDocs[1].Type,
	)
	suite.Assert().Equal(
		tasks.Added,
		taskDocs[1].Status,
	)
	suite.Assert().Equal(
		ns2,
		taskDocs[1].QueryFilter.Namespace,
	)

	suite.Assert().Equal(
		tasks.VerifyDocuments,
		taskDocs[2].Type,
	)
	suite.Assert().Equal(
		tasks.Added,
		taskDocs[2].Status,
	)
	suite.Assert().Equal(
		ns2,
		taskDocs[2].QueryFilter.Namespace,
	)

	// the ns2 partition task that *was* completed
	suite.Assert().Equal(
		tasks.VerifyDocuments,
		taskDocs[3].Type,
	)
	suite.Assert().Equal(
		tasks.Completed,
		taskDocs[3].Status,
	)
	suite.Assert().Equal(
		ns2,
		taskDocs[3].QueryFilter.Namespace,
	)

	// ns1’s verify-collection task (added state)
	suite.Assert().Equal(
		tasks.VerifyCollection,
		taskDocs[4].Type,
	)
	suite.Assert().Equal(
		tasks.Added,
		taskDocs[4].Status,
	)
	suite.Assert().Equal(
		ns1,
		taskDocs[4].QueryFilter.Namespace,
	)
}

// TestResetProcessRecheckQueueDeletesDocTasks verifies that when
// ResetInProgressTasks finds an in-progress ProcessRecheckQueue task,
// it deletes all VerifyDocuments tasks for that generation.
func (suite *IntegrationTestSuite) TestResetProcessRecheckQueueDeletesDocTasks() {
	ctx := suite.Context()

	dbName := suite.DBNameForTest()
	ns := dbName + ".coll"

	// Create a document mismatch: source has a doc that destination lacks.
	srcColl := suite.srcMongoClient.Database(dbName).Collection("coll")

	suite.Require().NoError(
		suite.dstMongoClient.Database(dbName).CreateCollection(ctx, "coll"),
	)

	_, err := srcColl.InsertOne(ctx, bson.D{{"_id", 1}, {"x", 42}})
	suite.Require().NoError(err)

	// Run verifier 1 through gen 0 → doc mismatch detected → rechecks enqueued.
	v1 := suite.BuildVerifier()
	v1.SetSrcNamespaces([]string{ns})
	v1.SetDstNamespaces([]string{ns})
	v1.SetNamespaceMap()

	v1Ctx, v1Cancel := contextplus.WithCancelCause(ctx)
	defer v1Cancel(ctx.Err())

	runner := RunVerifierCheck(v1Ctx, suite.T(), v1)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	// Start gen 1: this creates ProcessRecheckQueue + VerifyDocuments recheck
	// tasks. Let it fully complete so all tasks exist.
	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	// Kill verifier 1.
	v1Cancel(fmt.Errorf("stopping verifier 1"))

	tasksColl := v1.verificationTaskCollection()

	// Confirm gen-1 VerifyDocuments tasks exist.
	docTaskCount, err := tasksColl.CountDocuments(ctx, bson.M{
		"generation": 1,
		"type":       tasks.VerifyDocuments,
	})
	suite.Require().NoError(err)
	suite.Require().NotZero(docTaskCount, "gen-1 doc tasks should exist before reset")

	// Simulate a crash: flip the ProcessRecheckQueue task back to Processing.
	updated, err := tasksColl.UpdateOne(
		ctx,
		bson.M{
			"generation": 1,
			"type":       tasks.ProcessRecheckQueue,
		},
		bson.M{"$set": bson.M{"status": tasks.Processing}},
	)
	suite.Require().NoError(err)
	suite.Require().EqualValues(1, updated.ModifiedCount, "expect modification")

	// Build verifier 2 and call ResetInProgressTasks directly
	// (without running a full generation, which would re-process the tasks).
	v2 := suite.BuildVerifier()
	v2.generation = 1
	suite.Require().NoError(v2.ResetInProgressTasks(ctx))

	// The gen-1 VerifyDocuments tasks should have been deleted by the reset.
	docTaskCount, err = tasksColl.CountDocuments(ctx, bson.M{
		"generation": 1,
		"type":       tasks.VerifyDocuments,
	})
	suite.Require().NoError(err)
	suite.Assert().Zero(docTaskCount, "gen-1 doc tasks should be deleted after ProcessRecheckQueue reset")

	// The ProcessRecheckQueue task itself should have been reset to Added.
	var prqTask tasks.Task
	err = tasksColl.FindOne(ctx, bson.M{
		"generation": 1,
		"type":       tasks.ProcessRecheckQueue,
	}).Decode(&prqTask)
	suite.Require().NoError(err)
	suite.Assert().Equal(tasks.Added, prqTask.Status, "ProcessRecheckQueue should be reset to Added")
}

// TestResetCollectionTaskGen1PreservesDocRecheckTasks verifies that when
// ResetInProgressTasks finds an in-progress VerifyCollection task at
// generation 1+, it does NOT delete VerifyDocuments tasks for that generation.
// (This is different from gen 0, where the doc tasks are deleted.)
func (suite *IntegrationTestSuite) TestResetCollectionTaskGen1PreservesDocRecheckTasks() {
	ctx := suite.Context()

	dbName := suite.DBNameForTest()
	ns := dbName + ".coll"

	// Create both a document mismatch AND an index mismatch so that gen 1
	// will have both a VerifyCollection task (from the index mismatch) and
	// VerifyDocuments recheck tasks (from the doc mismatch).
	srcColl := suite.srcMongoClient.Database(dbName).Collection("coll")

	suite.Require().NoError(
		suite.dstMongoClient.Database(dbName).CreateCollection(ctx, "coll"),
	)

	_, err := srcColl.InsertOne(ctx, bson.D{{"_id", 1}, {"x", 42}})
	suite.Require().NoError(err)

	// Index on source but not on destination → metadata mismatch.
	_, err = srcColl.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"x", 1}},
		Options: options.Index().SetName("x_1"),
	})
	suite.Require().NoError(err)

	// Run verifier 1 through gen 0 → detects both mismatches.
	v1 := suite.BuildVerifier()
	v1.SetSrcNamespaces([]string{ns})
	v1.SetDstNamespaces([]string{ns})
	v1.SetNamespaceMap()

	v1Ctx, v1Cancel := contextplus.WithCancelCause(ctx)
	defer v1Cancel(ctx.Err())

	runner := RunVerifierCheck(v1Ctx, suite.T(), v1)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	// Start gen 1: ProcessRecheckQueue creates VerifyDocuments recheck tasks,
	// and there’s also a VerifyCollection task (from the gen-0 metadata mismatch).
	// We need to let gen 1 fully complete so all tasks have been created.
	suite.Require().Eventually(
		func() bool {
			suite.Require().NoError(runner.StartNextGeneration())
			suite.Require().NoError(runner.AwaitGenerationEnd())

			// Wait until the gen-1 VerifyCollection task exists.
			count, err := v1.verificationTaskCollection().CountDocuments(ctx, bson.M{
				"generation": 1,
				"type":       tasks.VerifyCollection,
			})
			suite.Require().NoError(err)

			return count > 0
		},
		time.Minute,
		100*time.Millisecond,
		"gen-1 VerifyCollection task should be created",
	)

	// Kill verifier 1.
	v1Cancel(fmt.Errorf("stopping verifier 1"))

	tasksColl := v1.verificationTaskCollection()

	// Confirm gen-1 VerifyDocuments tasks exist.
	docTaskCount, err := tasksColl.CountDocuments(ctx, bson.M{
		"generation": 1,
		"type":       tasks.VerifyDocuments,
	})
	suite.Require().NoError(err)
	suite.Require().NotZero(docTaskCount, "gen-1 doc recheck tasks should exist before reset")

	// Simulate a crash: flip the gen-1 VerifyCollection task to Processing.
	updated, err := tasksColl.UpdateOne(
		ctx,
		bson.M{
			"generation": 1,
			"type":       tasks.VerifyCollection,
		},
		bson.M{"$set": bson.M{"status": tasks.Processing}},
	)
	suite.Require().NoError(err)
	suite.Require().EqualValues(1, updated.ModifiedCount, "expect modified")

	// Build verifier 2 and call ResetInProgressTasks directly
	// (without running a full generation, which would re-process the tasks).
	v2 := suite.BuildVerifier()
	v2.generation = 1
	suite.Require().NoError(v2.ResetInProgressTasks(ctx))

	// The gen-1 VerifyDocuments tasks must NOT have been deleted.
	docTaskCountAfter, err := tasksColl.CountDocuments(ctx, bson.M{
		"generation": 1,
		"type":       tasks.VerifyDocuments,
	})
	suite.Require().NoError(err)
	suite.Assert().Equal(
		docTaskCount,
		docTaskCountAfter,
		"gen-1 doc recheck tasks should be preserved when resetting a gen-1 VerifyCollection task",
	)

	// The VerifyCollection task should have been reset to Added.
	var collTask tasks.Task
	err = tasksColl.FindOne(ctx, bson.M{
		"generation": 1,
		"type":       tasks.VerifyCollection,
	}).Decode(&collTask)
	suite.Require().NoError(err)
	suite.Assert().Equal(tasks.Added, collTask.Status, "VerifyCollection should be reset to Added")
}
