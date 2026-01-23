package verifier

import (
	"context"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestFetchAndCompareDocuments_Context ensures that nothing hangs
// when a context is canceled during FetchAndCompareDocuments().
func (s *IntegrationTestSuite) TestFetchAndCompareDocuments_Context() {
	ctx := s.Context()

	for _, client := range mslices.Of(s.srcMongoClient, s.dstMongoClient) {
		docs := lo.RepeatBy(
			10_000,
			func(i int) bson.D {
				return bson.D{}
			},
		)

		_, err := client.Database(s.DBNameForTest()).Collection("stuff").
			InsertMany(ctx, lo.ToAnySlice(docs))

		s.Require().NoError(err)
	}

	task := tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Type:       tasks.VerifyDocuments,
		Status:     tasks.Processing,
		QueryFilter: tasks.QueryFilter{
			Namespace: s.DBNameForTest() + ".stuff",
			Partition: &partitions.Partition{
				Key: partitions.PartitionKey{
					Lower: bsontools.ToRawValue(bson.MinKey{}),
				},
				Upper: bsontools.ToRawValue(bson.MaxKey{}),
			},
		},
	}

	verifier := s.BuildVerifier()
	s.Require().NoError(verifier.startChangeHandling(ctx))

	for range 100 {
		cancelableCtx, cancel := contextplus.WithCancelCause(ctx)

		var done atomic.Bool
		go func() {
			// Needed or else a panic happens.
			verifier.workerTracker.Set(0, task)

			_, _, _, err := verifier.FetchAndCompareDocuments(
				cancelableCtx,
				0,
				&task,
			)
			if err != nil {
				s.Assert().ErrorIs(
					err,
					context.Canceled,
					"only failure should be context cancellation",
				)
			}
			done.Store(true)
		}()

		delay := time.Duration(100 * float64(time.Millisecond) * rand.Float64())
		time.Sleep(delay)
		cancel(errors.Errorf("canceled after %s", delay))

		s.Assert().Eventually(
			func() bool {
				return done.Load()
			},
			time.Minute,
			10*time.Millisecond,
			"cancellation after %s should not cause hang",
		)
	}
}
