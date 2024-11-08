package verifier

import (
	"context"
	"strings"

	"github.com/10gen/migration-verifier/internal/partitions"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (suite *MultiDataVersionTestSuite) TestResetPrimaryTask() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)

	created, err := verifier.CheckIsPrimary()
	suite.Require().NoError(err)
	suite.Require().True(created)

	_, err = verifier.InsertCollectionVerificationTask("foo.bar")
	suite.Require().NoError(err)

	ctx := context.Background()

	err = verifier.doInMetaTransaction(
		ctx,
		func(_ context.Context, ctx mongo.SessionContext) error {
			return verifier.ResetInProgressTasks(ctx)
		},
	)
	suite.Require().NoError(err)

	tasksColl := verifier.verificationTaskCollection()
	cursor, err := tasksColl.Find(ctx, bson.M{})
	suite.Require().NoError(err)
	var taskDocs []bson.M
	suite.Require().NoError(cursor.All(ctx, &taskDocs))

	suite.Assert().Len(taskDocs, 1)
}

func (suite *MultiDataVersionTestSuite) TestResetNonPrimaryTasks() {
	ctx := context.Background()

	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)

	// Create a primary task, and set it to complete.
	created, err := verifier.CheckIsPrimary()
	suite.Require().NoError(err)
	suite.Require().True(created)

	suite.Require().NoError(verifier.UpdatePrimaryTaskComplete())

	ns1 := "foo.bar"
	ns2 := "qux.quux"

	// Create a collection-verification task, and set it to processing.
	collTask, err := verifier.InsertCollectionVerificationTask(ns1)
	suite.Require().NoError(err)

	collTask.Status = verificationTaskProcessing

	suite.Require().NoError(
		verifier.UpdateVerificationTask(collTask),
	)

	// Create three partition tasks with the same namespace as the
	// collection-verification task (status=[added, processing, completed]),
	// and another for a different namespace that’s completed.
	for _, taskParts := range []struct {
		Status    verificationTaskStatus
		Namespace string
	}{
		{verificationTaskAdded, ns1},
		{verificationTaskProcessing, ns1},
		{verificationTaskCompleted, ns1},
		{verificationTaskAdded, ns2},
		{verificationTaskProcessing, ns2},
		{verificationTaskCompleted, ns2},
	} {
		task, err := verifier.InsertPartitionVerificationTask(
			&partitions.Partition{
				Ns: &partitions.Namespace{
					DB:   strings.Split(taskParts.Namespace, ".")[0],
					Coll: strings.Split(taskParts.Namespace, ".")[1],
				},
			},
			nil,
			taskParts.Namespace,
		)
		suite.Require().NoError(err)

		task.Status = taskParts.Status
		suite.Require().NoError(
			verifier.UpdateVerificationTask(task),
		)
	}

	// Reset tasks
	err = verifier.doInMetaTransaction(
		ctx,
		func(_ context.Context, ctx mongo.SessionContext) error {
			return verifier.ResetInProgressTasks(ctx)
		},
	)
	suite.Require().NoError(err)

	// Contents should just be the primary task and
	// the completed partition-level.
	tasksColl := verifier.verificationTaskCollection()
	cursor, err := tasksColl.Find(
		ctx,
		bson.M{},
		options.Find().SetSort(bson.D{
			{"type", 1},
			{"query_filter.namespace", 1},
			{"status", 1},
		}),
	)
	suite.Require().NoError(err)
	var taskDocs []VerificationTask
	suite.Require().NoError(cursor.All(ctx, &taskDocs))

	suite.Require().Len(taskDocs, 5)

	// The tasks that should remain are:

	// the primary (completed)
	suite.Assert().Equal(
		verificationTaskPrimary,
		taskDocs[0].Type,
	)

	// the 2 ns2 partition tasks that weren’t completed (both “added”)
	suite.Assert().Equal(
		verificationTaskVerifyDocuments,
		taskDocs[1].Type,
	)
	suite.Assert().Equal(
		verificationTaskAdded,
		taskDocs[1].Status,
	)
	suite.Assert().Equal(
		ns2,
		taskDocs[1].QueryFilter.Namespace,
	)

	suite.Assert().Equal(
		verificationTaskVerifyDocuments,
		taskDocs[2].Type,
	)
	suite.Assert().Equal(
		verificationTaskAdded,
		taskDocs[2].Status,
	)
	suite.Assert().Equal(
		ns2,
		taskDocs[2].QueryFilter.Namespace,
	)

	// the ns2 partition task that *was* completed
	suite.Assert().Equal(
		verificationTaskVerifyDocuments,
		taskDocs[3].Type,
	)
	suite.Assert().Equal(
		verificationTaskCompleted,
		taskDocs[3].Status,
	)
	suite.Assert().Equal(
		ns2,
		taskDocs[3].QueryFilter.Namespace,
	)

	// ns1’s verify-collection task (added state)
	suite.Assert().Equal(
		verificationTaskVerifyCollection,
		taskDocs[4].Type,
	)
	suite.Assert().Equal(
		verificationTaskAdded,
		taskDocs[4].Status,
	)
	suite.Assert().Equal(
		ns1,
		taskDocs[4].QueryFilter.Namespace,
	)
}
