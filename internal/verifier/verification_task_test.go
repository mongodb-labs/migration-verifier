package verifier

import (
	"time"

	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mbson"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func (suite *IntegrationTestSuite) TestEnsureCreateRecheckTaskIfNeeded() {
	ctx := suite.Context()

	suite.Run("no recheck queue docs → no task created", func() {
		verifier := suite.BuildVerifier()
		verifier.mux.Lock()
		verifier.generation = 1
		err := verifier.ensureCreateRecheckTaskIfNeeded(ctx)
		verifier.mux.Unlock()
		suite.Require().NoError(err)

		count, err := verifier.verificationTaskCollection().CountDocuments(
			ctx,
			bson.M{"type": tasks.ProcessRecheckQueue},
		)
		suite.Require().NoError(err)
		suite.Assert().Zero(count, "no ProcessRecheckQueue task should exist when queue is empty")
	})

	suite.Run("recheck queue has docs → task created with correct generation and status", func() {
		verifier := suite.BuildVerifier()

		// Seed the gen-1 recheck queue with one doc (simulates a failed comparison).
		suite.Require().NoError(
			verifier.InsertFailedCompareRecheckDocs(
				ctx,
				"foo.bar",
				[]bson.RawValue{mbson.ToRawValue("someID")},
				[]int32{100},
				[]bson.DateTime{bson.NewDateTimeFromTime(time.Now())},
			),
		)

		verifier.mux.Lock()
		verifier.generation = 1
		err := verifier.ensureCreateRecheckTaskIfNeeded(ctx)
		verifier.mux.Unlock()
		suite.Require().NoError(err)

		var task tasks.Task
		err = verifier.verificationTaskCollection().FindOne(
			ctx,
			bson.M{"type": tasks.ProcessRecheckQueue},
		).Decode(&task)
		suite.Require().NoError(err, "ProcessRecheckQueue task should have been created")
		suite.Assert().Equal(1, task.Generation)
		suite.Assert().Equal(tasks.Added, task.Status)
	})

	suite.Run("idempotent: calling twice does not create a second task", func() {
		verifier := suite.BuildVerifier()

		suite.Require().NoError(
			verifier.InsertFailedCompareRecheckDocs(
				ctx,
				"foo.bar",
				[]bson.RawValue{mbson.ToRawValue("someID")},
				[]int32{100},
				[]bson.DateTime{bson.NewDateTimeFromTime(time.Now())},
			),
		)

		verifier.mux.Lock()
		verifier.generation = 1
		err := verifier.ensureCreateRecheckTaskIfNeeded(ctx)
		suite.Require().NoError(err)
		err = verifier.ensureCreateRecheckTaskIfNeeded(ctx)
		verifier.mux.Unlock()
		suite.Require().NoError(err)

		count, err := verifier.verificationTaskCollection().CountDocuments(
			ctx,
			bson.M{"type": tasks.ProcessRecheckQueue},
		)
		suite.Require().NoError(err)
		suite.Assert().Equal(int64(1), count, "upsert must not create duplicate tasks")
	})

	suite.Run("existing processing task is reset to added", func() {
		verifier := suite.BuildVerifier()

		suite.Require().NoError(
			verifier.InsertFailedCompareRecheckDocs(
				ctx,
				"foo.bar",
				[]bson.RawValue{mbson.ToRawValue("someID")},
				[]int32{100},
				[]bson.DateTime{bson.NewDateTimeFromTime(time.Now())},
			),
		)

		// Create the task in Processing state first (simulates a resume scenario).
		verifier.mux.Lock()
		verifier.generation = 1

		_, err := verifier.verificationTaskCollection().InsertOne(ctx, tasks.Task{
			PrimaryKey: bson.NewObjectID(),
			Generation: 1,
			Type:       tasks.ProcessRecheckQueue,
			Status:     tasks.Processing,
		})
		suite.Require().NoError(err)

		err = verifier.ensureCreateRecheckTaskIfNeeded(ctx)
		verifier.mux.Unlock()
		suite.Require().NoError(err)

		var task tasks.Task
		err = verifier.verificationTaskCollection().FindOne(
			ctx,
			bson.M{"type": tasks.ProcessRecheckQueue},
		).Decode(&task)
		suite.Require().NoError(err)
		suite.Assert().Equal(tasks.Added, task.Status, "task should be reset to Added")
	})

	for _, finishedStatus := range []tasks.Status{tasks.Completed, tasks.Failed} {
		suite.Run("existing "+string(finishedStatus)+" task is left alone", func() {
			verifier := suite.BuildVerifier()

			// All subtests share the same metadata DB, so clear it before
			// starting to avoid interference from prior subtests.
			suite.Require().NoError(verifier.verificationTaskCollection().Drop(ctx))

			suite.Require().NoError(
				verifier.InsertFailedCompareRecheckDocs(
					ctx,
					"foo.bar",
					[]bson.RawValue{mbson.ToRawValue("someID")},
					[]int32{100},
					[]bson.DateTime{bson.NewDateTimeFromTime(time.Now())},
				),
			)

			verifier.mux.Lock()
			verifier.generation = 1

			_, err := verifier.verificationTaskCollection().InsertOne(ctx, tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Generation: 1,
				Type:       tasks.ProcessRecheckQueue,
				Status:     finishedStatus,
			})
			suite.Require().NoError(err)

			err = verifier.ensureCreateRecheckTaskIfNeeded(ctx)
			verifier.mux.Unlock()
			suite.Require().NoError(err)

			var task tasks.Task
			err = verifier.verificationTaskCollection().FindOne(
				ctx,
				bson.M{"type": tasks.ProcessRecheckQueue},
			).Decode(&task)
			suite.Require().NoError(err)
			suite.Assert().Equal(finishedStatus, task.Status, "finished task must not be reset")

			count, err := verifier.verificationTaskCollection().CountDocuments(
				ctx,
				bson.M{"type": tasks.ProcessRecheckQueue},
			)
			suite.Require().NoError(err)
			suite.Assert().Equal(int64(1), count, "must not insert a second task alongside the finished one")
		})
	}
}