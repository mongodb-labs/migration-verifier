package verifier

import (
	"context"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
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

func (suite *MultiDataVersionTestSuite) TestResetCollectionTasks() {
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

	// Create four partition tasks: three with the same namespace as the
	// collection-verification task (status=[added, processing, completed]),
	// and another for a different namespace that’s completed.
	for _, status := range []verificationTaskStatus{
		verificationTaskAdded,
		verificationTaskProcessing,
		verificationTaskCompleted,
	} {
		task, err := verifier.InsertPartitionVerificationTask(
			&partitions.Partition{
				Ns: &partition.NameSpace{
					DB:   "foo",
					Coll: "bar",
				},
			},
			nil,
			"foo.bar",
		)
	}

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
