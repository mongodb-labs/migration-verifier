package verifier

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// This tests that sharded collections don’t trigger spurious mismatch reports.
// See REP-7144 for context.
func (s *IntegrationTestSuite) TestChannelShardedLargeDocs_DstLarger() {
	ctx := s.Context()

	srcColl := s.srcMongoClient.Database(s.DBNameForTest()).Collection("stuff")

	for _, client := range mslices.Of(s.srcMongoClient, s.dstMongoClient) {
		if s.GetTopology(client) != util.TopologySharded {
			continue
		}

		err := client.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"enableSharding", srcColl.Database().Name()},
			},
		).Err()

		s.Require().NoError(err, "enabling sharding on %#q", srcColl.Database().Name())

		err = client.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", FullName(srcColl)},
				{"key", bson.D{{"_id", "hashed"}}},
			},
		).Err()

		s.Require().NoError(err, "sharding %#q", FullName(srcColl))
	}

	dstColl := s.dstMongoClient.Database(s.DBNameForTest()).Collection(srcColl.Name())

	bigStr := strings.Repeat("x", 1_000_000)

	docIDs := make([]int, 0, 10_000)

	s.T().Log("Populating clusters …")

	eg, egCtx := contextplus.ErrGroup(ctx)
	eg.SetLimit(10)

	for range 50 {
		docs := lo.RepeatBy(
			10,
			func(_ int) bson.D {
				id := len(docIDs)
				docIDs = append(docIDs, id)
				return bson.D{
					{"_id", id},
					{"str", bigStr},
				}
			},
		)

		for i, coll := range mslices.Of(srcColl, dstColl) {
			eg.Go(func() error {
				docs := docs

				if i == 0 {
					docs = lo.Map(
						docs,
						func(d bson.D, _ int) bson.D {
							return bson.D{d[0]}
						},
					)
				}

				_, err := coll.InsertMany(egCtx, docs)
				return err
			})
		}
	}

	s.Require().NoError(eg.Wait())

	ensureAllContentMismatch := func(ctx context.Context, task tasks.Task) {
		subCtx, cancel := contextplus.WithCancelCause(ctx)
		defer cancel(fmt.Errorf("test over"))

		verifier := s.BuildVerifier()
		s.Require().NoError(verifier.startChangeHandling(subCtx))

		results := lo.ChannelToSlice(verifier.FetchAndCompareDocuments(
			subCtx,
			0,
			&task,
		))

		s.Assert().Len(results, 1)
		s.Require().NotEmpty(results)

		report, err := results[0].Get()
		s.Require().NoError(err)

		nonContentMismatches := lo.Filter(
			report.Problems,
			func(r compare.Result, _ int) bool {
				return r.DocumentIsMissing()
			},
		)

		if !s.Assert().Empty(nonContentMismatches, "all mismatches must be content-mismatch") {
			s.T().Logf("first: %+v", nonContentMismatches[0])
		}
	}

	s.Run(
		"_id range partition",
		func() {
			task := tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Type:       tasks.VerifyDocuments,
				Status:     tasks.Processing,
				QueryFilter: tasks.QueryFilter{
					Namespace: FullName(srcColl),
					Partition: &partitions.Partition{
						Key: partitions.PartitionKey{
							Lower: bsontools.ToRawValue(bson.MinKey{}),
						},
						Upper: bsontools.ToRawValue(bson.MaxKey{}),
					},
				},
			}

			ensureAllContentMismatch(ctx, task)
		},
	)

	s.Run(
		"natural partition",
		func() {
			if s.GetTopology(s.srcMongoClient) == util.TopologySharded {
				s.T().Skip("no natural partition with sharded source")
			}

			_, hostname := getDirectClientAndHostname(
				ctx,
				s.T(),
				s.srcMongoClient,
				s.srcConnStr,
			)

			task := tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Type:       tasks.VerifyDocuments,
				Status:     tasks.Processing,
				QueryFilter: tasks.QueryFilter{
					Namespace: FullName(srcColl),
					Partition: &partitions.Partition{
						Natural:         true,
						HostnameAndPort: option.Some(hostname),
						Upper:           bsontools.ToRawValue(int64(999_999_999_999)),
					},
				},
			}

			ensureAllContentMismatch(ctx, task)
		},
	)

	s.Run(
		"recheck",
		func() {
			task := tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Type:       tasks.VerifyDocuments,
				Status:     tasks.Processing,
				Ids: mslices.Map1(
					docIDs,
					bsontools.ToRawValue[int],
				),
				QueryFilter: tasks.QueryFilter{
					Namespace: FullName(srcColl),
				},
			}

			ensureAllContentMismatch(ctx, task)
		},
	)
}

// This tests that sharded collections don’t trigger spurious mismatch reports.
// See REP-7144 for context.
func (s *IntegrationTestSuite) TestChannelShardedLargeDocs() {
	ctx := s.Context()

	srcColl := s.srcMongoClient.Database(s.DBNameForTest()).Collection("stuff")

	for _, client := range mslices.Of(s.srcMongoClient, s.dstMongoClient) {
		if s.GetTopology(client) != util.TopologySharded {
			continue
		}

		err := client.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"enableSharding", srcColl.Database().Name()},
			},
		).Err()

		s.Require().NoError(err, "enabling sharding on %#q", srcColl.Database().Name())

		err = client.Database("admin").RunCommand(
			ctx,
			bson.D{
				{"shardCollection", FullName(srcColl)},
				{"key", bson.D{{"_id", "hashed"}}},
			},
		).Err()

		s.Require().NoError(err, "sharding %#q", FullName(srcColl))
	}

	dstColl := s.dstMongoClient.Database(s.DBNameForTest()).Collection(srcColl.Name())

	bigStr := strings.Repeat("x", 1_000_000)

	docIDs := make([]int, 0, 10_000)

	s.T().Log("Populating clusters …")

	eg, egCtx := contextplus.ErrGroup(ctx)
	eg.SetLimit(10)

	for range 50 {
		docs := lo.RepeatBy(
			10,
			func(_ int) bson.D {
				id := len(docIDs)
				docIDs = append(docIDs, id)
				return bson.D{
					{"_id", id},
					{"str", bigStr},
				}
			},
		)

		for _, coll := range mslices.Of(srcColl, dstColl) {
			eg.Go(func() error {
				_, err := coll.InsertMany(egCtx, docs)
				return err
			})
		}
	}

	s.Require().NoError(eg.Wait())

	ensureNoProblems := func(ctx context.Context, task tasks.Task) {
		subCtx, cancel := contextplus.WithCancelCause(ctx)
		defer cancel(fmt.Errorf("test over"))

		verifier := s.BuildVerifier()
		s.Require().NoError(verifier.startChangeHandling(subCtx))

		results := lo.ChannelToSlice(verifier.FetchAndCompareDocuments(
			subCtx,
			0,
			&task,
		))

		s.Assert().Len(results, 1)
		s.Require().NotEmpty(results)

		report, err := results[0].Get()
		s.Require().NoError(err)

		s.Assert().Empty(report.Problems)
	}

	s.Run(
		"_id range partition",
		func() {
			task := tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Type:       tasks.VerifyDocuments,
				Status:     tasks.Processing,
				QueryFilter: tasks.QueryFilter{
					Namespace: FullName(srcColl),
					Partition: &partitions.Partition{
						Key: partitions.PartitionKey{
							Lower: bsontools.ToRawValue(bson.MinKey{}),
						},
						Upper: bsontools.ToRawValue(bson.MaxKey{}),
					},
				},
			}

			ensureNoProblems(ctx, task)
		},
	)

	s.Run(
		"natural partition",
		func() {
			if s.GetTopology(s.srcMongoClient) == util.TopologySharded {
				s.T().Skip("no natural partition with sharded source")
			}

			_, hostname := getDirectClientAndHostname(
				ctx,
				s.T(),
				s.srcMongoClient,
				s.srcConnStr,
			)

			task := tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Type:       tasks.VerifyDocuments,
				Status:     tasks.Processing,
				QueryFilter: tasks.QueryFilter{
					Namespace: FullName(srcColl),
					Partition: &partitions.Partition{
						Natural:         true,
						HostnameAndPort: option.Some(hostname),
						Upper:           bsontools.ToRawValue(int64(999_999_999_999)),
					},
				},
			}

			ensureNoProblems(ctx, task)
		},
	)

	s.Run(
		"recheck",
		func() {
			task := tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Type:       tasks.VerifyDocuments,
				Status:     tasks.Processing,
				Ids: mslices.Map1(
					docIDs,
					bsontools.ToRawValue[int],
				),
				QueryFilter: tasks.QueryFilter{
					Namespace: FullName(srcColl),
				},
			}

			ensureNoProblems(ctx, task)
		},
	)
}

// This tests behavior when there are enough small documents to necessitate
// multiple problem reports. (This hits the comparator’s docs-cached max.)
// This only runs for replset sources.
func (s *IntegrationTestSuite) TestFetchAndCompareDocuments_ManySmallProblems() {
	if s.GetTopology(s.srcMongoClient) == util.TopologySharded {
		s.T().Skip("needs unsharded source")
	}

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

	_, hostname := getDirectClientAndHostname(
		ctx,
		s.T(),
		s.srcMongoClient,
		s.srcConnStr,
	)

	task := tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Type:       tasks.VerifyDocuments,
		Status:     tasks.Processing,
		QueryFilter: tasks.QueryFilter{
			Namespace: s.DBNameForTest() + ".stuff",
			Partition: &partitions.Partition{
				Natural:         true,
				HostnameAndPort: option.Some(hostname),
				Upper:           bsontools.ToRawValue(int64(999_999_999_999)),
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

// This tests behavior when there are enough large documents to necessitate
// multiple problem reports. (This hits the comparator’s bytes-cached max.)
func (s *IntegrationTestSuite) TestFetchAndCompareDocuments_BigProblems() {
	if s.GetTopology(s.srcMongoClient) == util.TopologySharded {
		s.T().Skip("needs unsharded source")
	}

	ctx := s.Context()

	srcColl := s.srcMongoClient.Database(s.DBNameForTest()).Collection("stuff")

	docsCount := 0

	batchesCount := 100
	perBatchCount := 100

	for range batchesCount {
		res, err := srcColl.InsertMany(
			ctx,
			mslices.Map1(
				lo.Range(perBatchCount),
				func(i int) bson.D {
					return bson.D{
						{"bigStr", strings.Repeat(
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

	_, hostname := getDirectClientAndHostname(
		ctx,
		s.T(),
		s.srcMongoClient,
		s.srcConnStr,
	)

	task := tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Type:       tasks.VerifyDocuments,
		Status:     tasks.Processing,
		QueryFilter: tasks.QueryFilter{
			Namespace: s.DBNameForTest() + ".stuff",
			Partition: &partitions.Partition{
				Natural:         true,
				HostnameAndPort: option.Some(hostname),
				Upper:           bsontools.ToRawValue(int64(999_999_999_999)),
			},
		},
	}

	verifier := s.BuildVerifier()

	// For this test to work we need binary comparison … or else memory usage
	// will be too modest to trip the failure.
	verifier.SetDocCompareMethod(compare.Binary)

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

			// When a context is canceled but a channel is ready it’s
			// indeterminate whether the channel or the context “wins”. Thus
			// it’s possible to have no report, or 1 report.
			switch len(reports) {
			case 0:
				// Nothing to do
			case 1:
				_, err := reports[0].Get()
				if err != nil {
					s.Assert().ErrorIs(
						err,
						context.Canceled,
						"only failure should be context cancellation",
					)
				}
			default:
				s.Assert().Fail("expected <=1 report but found %+v", reports)
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
