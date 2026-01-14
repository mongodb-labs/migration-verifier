package verifier

import (
	"strings"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
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
