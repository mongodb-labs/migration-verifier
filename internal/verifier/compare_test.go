package verifier

import (
	"context"
	"math/rand"
	"sync/atomic"
	"time"

	"github.com/mongodb-labs/migration-verifier/contextplus"
	"github.com/mongodb-labs/migration-verifier/internal/partitions"
	"github.com/mongodb-labs/migration-verifier/mslices"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// TestFetchAndCompareDocuments_ContextCancellation ensures that nothing hangs
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

	task := VerificationTask{
		PrimaryKey: primitive.NewObjectID(),
		Type:       verificationTaskVerifyDocuments,
		Status:     verificationTaskProcessing,
		QueryFilter: QueryFilter{
			Namespace: s.DBNameForTest() + ".stuff",
			Partition: &partitions.Partition{
				Key: partitions.PartitionKey{
					Lower: primitive.MinKey{},
				},
				Upper: primitive.MaxKey{},
			},
		},
	}

	verifier := s.BuildVerifier()

	for range 100 {
		cancelableCtx, cancel := contextplus.WithCancelCause(ctx)

		var done atomic.Bool
		go func() {
			_, _, _, err := verifier.FetchAndCompareDocuments(
				cancelableCtx,
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
