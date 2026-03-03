package verifier

import (
	"context"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (s *IntegrationTestSuite) TestFetchAndCompareDocuments_ManySmallProblems() {
	ctx := s.Context()

	srcColl := s.srcMongoClient.Database(s.DBNameForTest()).Collection("stuff")

	docsCount := 0

	batchesCount := 1_000
	perBatchCount := 1_000

	for range batchesCount {
		res, err := srcColl.InsertMany(
			ctx,
			mslices.Map1(
				lo.Range(perBatchCount),
				func(i int) bson.D {
					return bson.D{
						{"_id", docsCount + i},
					}
				},
			),
			options.InsertMany().SetOrdered(false),
		)

		s.Require().NoError(err)

		docsCount += len(res.InsertedIDs)
	}

	s.Require().NoError(
		s.dstMongoClient.Database(s.DBNameForTest()).CreateCollection(
			ctx,
			srcColl.Name(),
		),
	)

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

	results := lo.ChannelToSlice(verifier.FetchAndCompareDocuments(
		ctx,
		0,
		&task,
	))

	s.Assert().Greater(len(results), 1, "need multiple reports")

	var problemsCount int
	var totalDocs types.DocumentCount

	for _, result := range results {
		report, err := result.Get()
		s.Require().NoError(err)

		problemsCount += len(report.Problems)
		totalDocs += report.DocCount
	}

	s.Assert().EqualValues(batchesCount*perBatchCount, problemsCount, "total problems")
	s.Assert().EqualValues(batchesCount*perBatchCount, totalDocs, "total docs checked")
}

func (s *IntegrationTestSuite) TestFetchAndCompareDocuments_BigProblems() {
	ctx := s.Context()

	srcColl := s.srcMongoClient.Database(s.DBNameForTest()).Collection("stuff")

	docsCount := 0

	batchesCount := 100
	perBatchCount := 10

	for range batchesCount {
		res, err := srcColl.InsertMany(
			ctx,
			mslices.Map1(
				lo.Range(perBatchCount),
				func(i int) bson.D {
					return bson.D{
						{"_id", strings.Repeat(
							strconv.Itoa(docsCount+i),
							10_000,
						)},
					}
				},
			),
			options.InsertMany().SetOrdered(false),
		)

		s.Require().NoError(err)

		docsCount += len(res.InsertedIDs)
	}

	s.Require().NoError(
		s.dstMongoClient.Database(s.DBNameForTest()).CreateCollection(
			ctx,
			srcColl.Name(),
		),
	)

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

	results := lo.ChannelToSlice(verifier.FetchAndCompareDocuments(
		ctx,
		0,
		&task,
	))

	s.Assert().Greater(len(results), 1, "need multiple reports")

	var problemsCount int
	var totalDocs types.DocumentCount

	for _, result := range results {
		report, err := result.Get()
		s.Require().NoError(err)

		problemsCount += len(report.Problems)
		totalDocs += report.DocCount
	}

	s.Assert().EqualValues(batchesCount*perBatchCount, problemsCount, "total problems")
	s.Assert().EqualValues(batchesCount*perBatchCount, totalDocs, "total docs checked")
}

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
			reports := lo.ChannelToSlice(verifier.FetchAndCompareDocuments(
				cancelableCtx,
				0,
				&task,
			))

			done.Store(true)

			s.Require().Len(reports, 1)

			_, err := reports[0].Get()

			if err != nil {
				s.Assert().ErrorIs(
					err,
					context.Canceled,
					"only failure should be context cancellation",
				)
			}
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
