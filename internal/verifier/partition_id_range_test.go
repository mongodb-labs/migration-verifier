package verifier

import (
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// processPartitionIDRangeTasks processes all of the given namespace’s
// partitionIDRange tasks and returns them, sorted by lower bound.
func (suite *IntegrationTestSuite) processPartitionIDRangeTasks(
	verifier *Verifier,
	namespace string,
) []tasks.Task {
	ctx := suite.T().Context()

	rangeTasks := suite.findPartitionIDRangeTasks(verifier, namespace)

	for _, task := range rangeTasks {
		suite.Require().NoError(
			verifier.ProcessPartitionIDRangeTask(ctx, 0, task),
			"must process task %v",
			task.PrimaryKey,
		)
	}

	return suite.findPartitionIDRangeTasks(verifier, namespace)
}

// waitForPartitioningReads waits until partitioning queries, which may read
// from secondaries, see all of the collection’s documents.
func (suite *IntegrationTestSuite) waitForPartitioningReads(
	coll *mongo.Collection,
	docsCount int,
) {
	ctx := suite.T().Context()

	suite.Require().Eventually(
		func() bool {
			count, err := partitions.ForPartitionAggregation(coll).CountDocuments(ctx, bson.D{})
			suite.Require().NoError(err)

			return count == int64(docsCount)
		},
		time.Minute,
		10*time.Millisecond,
		"partitioning reads should see all %d documents",
		docsCount,
	)
}

func (suite *IntegrationTestSuite) findPartitionIDRangeTasks(
	verifier *Verifier,
	namespace string,
) []tasks.Task {
	ctx := suite.T().Context()

	cursor, err := verifier.verificationTaskCollection().Find(
		ctx,
		bson.D{
			{"type", tasks.PartitionIDRange},
			{"query_filter.namespace", namespace},
		},
		options.Find().SetSort(bson.D{{"query_filter.partition._id.lowerBound", 1}}),
	)
	suite.Require().NoError(err)

	var rangeTasks []tasks.Task
	suite.Require().NoError(cursor.All(ctx, &rangeTasks))

	return rangeTasks
}

func (suite *IntegrationTestSuite) findChildPartitionTasks(
	verifier *Verifier,
	parentID bson.ObjectID,
) []tasks.Task {
	ctx := suite.T().Context()

	cursor, err := verifier.verificationTaskCollection().Find(
		ctx,
		bson.D{
			{"type", tasks.VerifyDocuments},
			{"parent_task_id", parentID},
		},
		options.Find().SetSort(bson.D{{"query_filter.partition.upperBound", 1}}),
	)
	suite.Require().NoError(err)

	var children []tasks.Task
	suite.Require().NoError(cursor.All(ctx, &children))

	return children
}

func (suite *IntegrationTestSuite) TestPartitionIDRange_MixedIDTypes() {
	ctx := suite.T().Context()

	verifier := suite.BuildVerifier()
	if !verifier.srcHasSampleRate() {
		suite.T().Skip("Need a source version with $sampleRate.")
	}

	dbName := suite.DBNameForTest()
	collName := "coll"
	ns := dbName + "." + collName
	coll := suite.srcMongoClient.Database(dbName).Collection(collName)

	// These _id types sort before, within, and after the ObjectID range.
	var docs []any
	for i := range 300 {
		payload := testutil.RandomString(100)

		docs = append(
			docs,
			bson.D{{"_id", i}, {"payload", payload}},
			bson.D{{"_id", fmt.Sprintf("str%04d", i)}, {"payload", payload}},
			bson.D{{"_id", bson.NewObjectID()}, {"payload", payload}},
			bson.D{{"_id", bson.NewDateTimeFromTime(time.UnixMilli(int64(i)))}, {"payload", payload}},
		)
	}

	_, err := coll.InsertMany(ctx, docs)
	suite.Require().NoError(err)
	suite.waitForPartitioningReads(coll, len(docs))

	verifier.partitionSizeInBytes = 10_000

	collBytes, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
		ctx,
		logger.NewDebugLogger(),
		coll,
	)
	suite.Require().NoError(err)

	collTask := &tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Type:       tasks.VerifyCollection,
		QueryFilter: tasks.QueryFilter{
			Namespace: ns,
		},
	}

	// Partitioning twice ensures that retrying a collection task doesn’t
	// duplicate its range tasks.
	for range 2 {
		suite.Require().NoError(
			verifier.partitionCollection(ctx, collTask, 0, collBytes, docsCount, isCapped),
		)
	}

	rangeTasks := suite.findPartitionIDRangeTasks(verifier, ns)
	suite.Require().Len(rangeTasks, 3, "range tasks")

	expectedRanges := idPartitionRanges()
	for i, rangeTask := range rangeTasks {
		suite.Assert().Equal(tasks.Added, rangeTask.Status)
		suite.Assert().Equal(expectedRanges[i][0], rangeTask.QueryFilter.Partition.Key.Lower, "range %d lower", i)
		suite.Assert().Equal(expectedRanges[i][1], rangeTask.QueryFilter.Partition.Upper, "range %d upper", i)
	}

	countVerifyDocsTasks := func() int64 {
		count, err := verifier.verificationTaskCollection().CountDocuments(
			ctx,
			bson.D{{"type", tasks.VerifyDocuments}},
		)
		suite.Require().NoError(err)

		return count
	}

	suite.Require().Zero(
		countVerifyDocsTasks(),
		"collection task should create no document tasks directly",
	)

	rangeTasks = suite.processPartitionIDRangeTasks(verifier, ns)

	var allChildren []tasks.Task

	for i, rangeTask := range rangeTasks {
		suite.Assert().Equal(tasks.Completed, rangeTask.Status, "range %d status", i)

		children := suite.findChildPartitionTasks(verifier, rangeTask.PrimaryKey)
		suite.assertContiguousPartitions(rangeTask, children)

		allChildren = append(allChildren, children...)
	}

	suite.Assert().EqualValues(
		len(allChildren),
		countVerifyDocsTasks(),
		"every document task should belong to a range task",
	)

	suite.Assert().Greater(len(allChildren), len(rangeTasks), "should create multiple partitions")

	// Every document should be in some partition.
	foundIDs := map[string]struct{}{}
	for _, child := range allChildren {
		for _, id := range suite.findIDsInPartition(verifier, child.QueryFilter.Partition) {
			foundIDs[string(id.Type)+string(id.Value)] = struct{}{}
		}
	}

	suite.Assert().Len(foundIDs, len(docs), "partitions should cover all documents")
}

func (suite *IntegrationTestSuite) TestPartitionIDRange_Resume() {
	ctx := suite.T().Context()

	verifier := suite.BuildVerifier()
	if !verifier.srcHasSampleRate() {
		suite.T().Skip("Need a source version with $sampleRate.")
	}

	dbName := suite.DBNameForTest()
	collName := "coll"
	ns := dbName + "." + collName
	coll := suite.srcMongoClient.Database(dbName).Collection(collName)

	ids := lo.RepeatBy(1_000, func(_ int) bson.ObjectID { return bson.NewObjectID() })

	_, err := coll.InsertMany(
		ctx,
		lo.Map(
			ids,
			func(id bson.ObjectID, _ int) any {
				return bson.D{{"_id", id}, {"payload", testutil.RandomString(100)}}
			},
		),
	)
	suite.Require().NoError(err)
	suite.waitForPartitioningReads(coll, len(ids))

	verifier.partitionSizeInBytes = 10_000

	objIDRange := idPartitionRanges()[1]
	resumeFrom := bsontools.ToRawValue(ids[len(ids)/2])

	suite.Require().NoError(
		verifier.ensurePartitionIDRangeTask(
			ctx,
			&partitions.Partition{
				Key: partitions.PartitionKey{Lower: objIDRange[0]},
				Ns: &partitions.Namespace{
					DB:   dbName,
					Coll: collName,
				},
				Upper: objIDRange[1],
			},
			nil,
			ns,
		),
	)

	rangeTasks := suite.findPartitionIDRangeTasks(verifier, ns)
	suite.Require().Len(rangeTasks, 1)
	rangeTask := rangeTasks[0]

	// Simulate a prior, interrupted attempt that created one partition.
	_, err = verifier.insertPartitionVerificationTask(
		ctx,
		&partitions.Partition{
			Key: partitions.PartitionKey{Lower: objIDRange[0]},
			Ns: &partitions.Namespace{
				DB:   dbName,
				Coll: collName,
			},
			Upper: resumeFrom,
		},
		nil,
		ns,
		option.Some(rangeTask.PrimaryKey),
	)
	suite.Require().NoError(err)

	rangeTasks = suite.processPartitionIDRangeTasks(verifier, ns)
	suite.Require().Len(rangeTasks, 1)
	suite.Assert().Equal(tasks.Completed, rangeTasks[0].Status)

	children := suite.findChildPartitionTasks(verifier, rangeTask.PrimaryKey)
	suite.Require().GreaterOrEqual(len(children), 2, "should resume, creating more partitions")
	suite.assertContiguousPartitions(rangeTask, children)

	// Partitioning should resume where the prior attempt left off.
	suite.Assert().Equal(resumeFrom, children[0].QueryFilter.Partition.Upper)
	suite.Assert().Equal(resumeFrom, children[1].QueryFilter.Partition.Key.Lower)

	// Processing the completed range again (e.g., if the task’s status
	// update failed) should create no more partitions.
	suite.Require().NoError(
		verifier.ProcessPartitionIDRangeTask(ctx, 0, rangeTasks[0]),
	)
	suite.Assert().Len(
		suite.findChildPartitionTasks(verifier, rangeTask.PrimaryKey),
		len(children),
		"reprocessing a finished range should add no partitions",
	)
}

func (suite *IntegrationTestSuite) TestPartitionIDRange_Reset() {
	ctx := suite.T().Context()

	verifier := suite.BuildVerifier()

	created, err := verifier.CreatePrimaryTaskIfNeeded(ctx)
	suite.Require().NoError(err)
	suite.Require().True(created)
	suite.Require().NoError(verifier.UpdatePrimaryTaskComplete(ctx))

	// ns1’s collection task is in progress. ns2’s is done, but one of its
	// range tasks is in progress.
	ns1 := mslices.Of("foo", "bar")
	ns2 := mslices.Of("qux", "quux")

	taskColl := verifier.verificationTaskCollection()

	for _, ns := range mslices.Of(ns1, ns2) {
		fullNs := ns[0] + "." + ns[1]

		collTask, err := verifier.InsertCollectionVerificationTask(ctx, fullNs)
		suite.Require().NoError(err)

		collTask.Status = lo.Ternary(fullNs == "foo.bar", tasks.Processing, tasks.Completed)
		suite.Require().NoError(verifier.UpdateVerificationTask(ctx, collTask))

		for _, idRange := range idPartitionRanges() {
			suite.Require().NoError(
				verifier.ensurePartitionIDRangeTask(
					ctx,
					&partitions.Partition{
						Key:   partitions.PartitionKey{Lower: idRange[0]},
						Ns:    &partitions.Namespace{DB: ns[0], Coll: ns[1]},
						Upper: idRange[1],
					},
					nil,
					fullNs,
				),
			)
		}

		rangeTasks := suite.findPartitionIDRangeTasks(verifier, fullNs)
		suite.Require().Len(rangeTasks, 3)

		for i, status := range mslices.Of(tasks.Completed, tasks.Processing, tasks.Added) {
			_, err := taskColl.UpdateOne(
				ctx,
				bson.D{{"_id", rangeTasks[i].PrimaryKey}},
				bson.D{{"$set", bson.D{{"status", status}}}},
			)
			suite.Require().NoError(err)

			if status == tasks.Added {
				continue
			}

			_, err = verifier.insertPartitionVerificationTask(
				ctx,
				&partitions.Partition{
					Key:   partitions.PartitionKey{Lower: rangeTasks[i].QueryFilter.Partition.Key.Lower},
					Ns:    &partitions.Namespace{DB: ns[0], Coll: ns[1]},
					Upper: rangeTasks[i].QueryFilter.Partition.Upper,
				},
				nil,
				fullNs,
				option.Some(rangeTasks[i].PrimaryKey),
			)
			suite.Require().NoError(err)
		}
	}

	suite.Require().NoError(verifier.ResetInProgressTasks(ctx))

	countTasks := func(fullNs string, taskType tasks.Type) int64 {
		count, err := taskColl.CountDocuments(
			ctx,
			bson.D{
				{"type", taskType},
				{"query_filter.namespace", fullNs},
			},
		)
		suite.Require().NoError(err)

		return count
	}

	// Resetting ns1’s collection task should remove all of its
	// partitioning, including the range tasks.
	suite.Assert().Zero(countTasks("foo.bar", tasks.PartitionIDRange), "ns1 range tasks")
	suite.Assert().Zero(countTasks("foo.bar", tasks.VerifyDocuments), "ns1 document tasks")

	// ns2’s in-progress range task should be reset but keep its
	// partitions so that it can resume.
	rangeTasks := suite.findPartitionIDRangeTasks(verifier, "qux.quux")
	suite.Require().Len(rangeTasks, 3)
	suite.Assert().Equal(
		mslices.Of(tasks.Completed, tasks.Added, tasks.Added),
		lo.Map(rangeTasks, func(t tasks.Task, _ int) tasks.Status { return t.Status }),
		"ns2 range task statuses",
	)
	suite.Assert().EqualValues(2, countTasks("qux.quux", tasks.VerifyDocuments), "ns2 document tasks")
}

// assertContiguousPartitions asserts that the given partitions, sorted by
// upper bound, exactly span the given partitionIDRange task’s range.
func (suite *IntegrationTestSuite) assertContiguousPartitions(
	rangeTask tasks.Task,
	children []tasks.Task,
) {
	suite.Require().NotEmpty(children, "range task %v should have partitions", rangeTask.PrimaryKey)

	suite.Assert().Equal(
		rangeTask.QueryFilter.Partition.Key.Lower,
		children[0].QueryFilter.Partition.Key.Lower,
		"first partition should start at range’s lower bound",
	)

	suite.Assert().Equal(
		rangeTask.QueryFilter.Partition.Upper,
		children[len(children)-1].QueryFilter.Partition.Upper,
		"last partition should end at range’s upper bound",
	)

	for i := 1; i < len(children); i++ {
		suite.Assert().Equal(
			children[i-1].QueryFilter.Partition.Upper,
			children[i].QueryFilter.Partition.Key.Lower,
			"partition %d should start where its predecessor ends",
			i,
		)
	}
}

func (suite *IntegrationTestSuite) findIDsInPartition(
	verifier *Verifier,
	partition *partitions.Partition,
) []bson.RawValue {
	ctx := suite.T().Context()

	predicates, err := partitions.FilterIdBounds(
		verifier.srcClusterInfo,
		partition.Key.Lower,
		partition.Upper,
	)
	suite.Require().NoError(err)

	cursor, err := suite.srcMongoClient.
		Database(partition.Ns.DB).
		Collection(partition.Ns.Coll).
		Find(
			ctx,
			bson.D{{"$and", predicates}},
			options.Find().SetProjection(bson.D{{"_id", 1}}),
		)
	suite.Require().NoError(err)

	var docs []bson.Raw
	suite.Require().NoError(cursor.All(ctx, &docs))

	return lo.Map(
		docs,
		func(doc bson.Raw, _ int) bson.RawValue {
			return doc.Lookup("_id")
		},
	)
}
