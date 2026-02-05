package verifier

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/10gen/migration-verifier/internal/partitions"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/mmongo/cursor"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/option"
	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
)

func (suite *IntegrationTestSuite) skipUnlessCanPartitionNatural() [3]int {
	ctx := suite.T().Context()

	topology := suite.GetTopology(suite.srcMongoClient)
	if topology != util.TopologyReplset {
		suite.T().Skipf("source must be replset; instead we have %#q", topology)
	}

	version, err := mmongo.GetVersionArray(
		ctx,
		suite.srcMongoClient,
	)
	suite.Require().NoError(err)

	if err := mmongo.WhyFindCannotResume([2]int(version[:])); err != nil {
		suite.T().Skipf("no natural scan (%v)", err)
	}

	return version
}

func (suite *IntegrationTestSuite) TestFetchAndCompareAllDstDocsGone() {
	suite.skipUnlessCanPartitionNatural()

	ctx := suite.T().Context()
	t := suite.T()

	// Insert ~40 MiB of data into the collection.
	// Each document is roughly 220 bytes.
	docs := lo.RepeatBy(
		200_000,
		func(i int) bson.D {
			return bson.D{
				{"_id", i},
				{"str", strings.Repeat("x", 200)},
			}
		},
	)

	srcColl := suite.srcMongoClient.
		Database(suite.DBNameForTest()).
		Collection("coll")

	_, err := srcColl.InsertMany(ctx, docs)
	require.NoError(t, err)

	// No documents on the destination … but we need to create the collection.

	err = suite.dstMongoClient.
		Database(suite.DBNameForTest()).
		CreateCollection(ctx, "coll")
	require.NoError(t, err)

	verifier := suite.BuildVerifier()

	verifier.SetPartitioningScheme(partitions.SchemeNatural)
	verifier.SetPartitionSizeMB(1)

	// needed for comparison:
	require.NoError(t, verifier.startChangeHandling(ctx))

	task := &tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		Type:       tasks.VerifyCollection,
		QueryFilter: tasks.QueryFilter{
			Namespace: FullName(srcColl),
		},
	}

	results, _, _, err := verifier.FetchAndCompareDocuments(
		ctx,
		0,
		task,
	)
	require.NoError(t, err)

	assert.Len(t, results, len(docs), "every doc should trigger a result")

	slices.SortFunc(
		results,
		func(a, b VerificationResult) int {
			return cmp.Compare(
				lo.Must(bsontools.RawValueTo[int](a.ID)),
				lo.Must(bsontools.RawValueTo[int](b.ID)),
			)
		},
	)

	for i, r := range results {
		resultDocID, err := bsontools.RawValueTo[int32](r.ID)
		require.NoError(t, err)

		assert.EqualValues(t, docs[i][0].Value, resultDocID)

		assert.True(t, r.DocumentIsMissing(), "results[%d]: should be doc-missing")
		assert.EqualValues(t, ClusterTarget, r.Cluster)
	}
}

// TestNaturalPartitionSourceE2E confirms that we can partition and
// read document correctly.
func (suite *IntegrationTestSuite) TestNaturalPartitionSourceE2E() {
	version := suite.skipUnlessCanPartitionNatural()

	ctx := suite.T().Context()
	t := suite.T()

	coll := suite.srcMongoClient.
		Database(suite.DBNameForTest()).
		Collection("c", options.Collection().SetReadConcern(readconcern.Majority()))

	// Insert ~40 MiB of data into the collection.
	// Each document is roughly 220 bytes.
	docs := lo.RepeatBy(
		200_000,
		func(i int) bson.D {
			return bson.D{
				{"_id", i},
				{"str", strings.Repeat("x", 200)},
			}
		},
	)

	docs = append(
		docs,
		bson.D{
			{"_id", -1},
			{"x", strings.Repeat("x", (16<<20)-22)},
		},
	)

	directClient, _ := getDirectClientAndHostname(
		ctx,
		suite.T(),
		suite.srcMongoClient,
		suite.srcConnStr,
	)

	for _, clustered := range mslices.Of(false, true) {
		suite.Run(
			fmt.Sprintf("clustered %t", clustered),
			func() {
				if version[0] < 6 && clustered {
					suite.T().Skip("clustered requires v6+")
				}

				verifier := suite.BuildVerifier()
				verifier.SetPartitioningScheme(partitions.SchemeNatural)
				verifier.SetPartitionSizeMB(1)

				defer suite.Assert().NoError(verifier.verificationTaskCollection().Drop(ctx))

				suite.Require().NoError(coll.Drop(ctx))

				if clustered {
					suite.Require().NoError(
						coll.Database().CreateCollection(
							ctx,
							coll.Name(),
							options.CreateCollection().
								SetClusteredIndex(
									bson.D{
										{"key", bson.D{{"_id", 1}}},
										{"unique", true},
									},
								),
						),
					)
				}

				_, err := coll.InsertMany(ctx, docs)
				require.NoError(t, err)

				task := &tasks.Task{
					PrimaryKey: bson.NewObjectID(),
					Type:       tasks.VerifyCollection,
					QueryFilter: tasks.QueryFilter{
						Namespace: FullName(coll),
					},
				}

				collBytes, docsCount, isCapped, err := partitions.GetSizeAndDocumentCount(
					ctx,
					verifier.logger,
					coll,
				)
				suite.Require().NoError(err, "fetching & persisting collection size")

				err = verifier.partitionCollection(
					ctx,
					task,
					0,
					collBytes,
					docsCount,
					isCapped,
				)
				suite.Require().NoError(err)

				// Now delete half of the collection’s documents randomly:
				_, err = coll.DeleteMany(
					ctx,
					bson.D{{"$sampleRate", 0.5}},
				)
				suite.Require().NoError(err)

				cursor, err := coll.Find(ctx, bson.D{})
				suite.Require().NoError(err)

				var undeletedDocs []bson.D
				suite.Require().NoError(cursor.All(ctx, &undeletedDocs))

				cursor, err = verifier.verificationTaskCollection().Find(
					ctx,
					bson.D{
						{"type", tasks.VerifyDocuments},
					},
					options.Find().SetSort(
						bson.D{{"query_filter.partition._id.lowerBound", 1}},
					),
				)
				suite.Require().NoError(err)

				var theTasks []tasks.Task
				suite.Require().NoError(cursor.All(ctx, &theTasks))

				var taskDocCounter int

				for _, task := range theTasks {
					toCompare := make(chan []compare.DocWithTS, len(undeletedDocs))
					toDst := make(chan []compare.DocID, len(undeletedDocs))

					err = compare.ReadNaturalPartitionFromSource(
						ctx,
						verifier.logger,
						&MockSuccessNotifier{},
						directClient,
						verifier.verificationTaskCollection(),
						&task,
						option.None[bson.D](),
						compare.Binary,
						toCompare,
						toDst,
					)
					require.NoError(t, err, "should read")

					for batch := range toCompare {
						assert.LessOrEqual(t, len(batch), compare.ToComparatorBatchSize)

						for _, d := range batch {
							var doc bson.D
							suite.Require().NoError(bson.Unmarshal(d.Doc, &doc), "unmarshal")

							suite.Require().Equal(
								undeletedDocs[taskDocCounter],
								doc,
								"doc %d (initial docs: %+v)",
								taskDocCounter,
								lo.Slice(batch, 0, 5),
							)

							taskDocCounter++
						}
					}
				}
			},
		)
	}
}

func (suite *IntegrationTestSuite) TestPartitionCollectionNaturalOrder() {
	version := suite.skipUnlessCanPartitionNatural()

	ctx := suite.T().Context()
	t := suite.T()

	logger, _ := getLoggerAndWriter("stdout")

	coll := suite.srcMongoClient.Database(suite.DBNameForTest()).Collection("c")

	// Insert ~100 MiB of data into the collection.
	// Each document is roughly 220 bytes.
	docs := lo.RepeatBy(
		50_000,
		func(i int) bson.D {
			return bson.D{
				{"_id", i},
				{"str", strings.Repeat("x", 200)},
			}
		},
	)

	for _, clustered := range mslices.Of(true, false) {
		if version[0] < 6 && clustered {
			suite.T().Skip("clustered requires v6+")
		}

		suite.Require().NoError(coll.Drop(ctx))

		if clustered {
			suite.Require().NoError(
				coll.Database().CreateCollection(
					ctx,
					coll.Name(),
					options.CreateCollection().
						SetClusteredIndex(
							bson.D{
								{"key", bson.D{{"_id", 1}}},
								{"unique", true},
							},
						),
				),
			)
		}

		_, err := coll.InsertMany(ctx, docs)
		require.NoError(t, err)

		// Partition the collection in 100-KB chunks.
		pChan, err := partitions.PartitionCollectionNaturalOrder(
			ctx,
			coll,
			100_000,
			logger,
		)
		require.NoError(t, err)

		hello, err := util.GetHelloRaw(ctx, suite.srcMongoClient)
		require.NoError(t, err)

		hostnameRV, err := hello.LookupErr("me")
		require.NoError(t, err)

		hostname, err := bsontools.RawValueTo[string](hostnameRV)
		require.NoError(t, err)

		results := lo.ChannelToSlice(pChan)

		// It *could* happen that we get a single partition, but it seems
		// very unlikely to happen anytime soon.
		assert.GreaterOrEqual(t, len(results), 2)

		tasksColl := coll.Database().Collection("tasks")

		lastUpperBound := bsontools.ToRawValue(bson.Null{})

		for i, result := range results {
			partition, err := result.Get()
			require.NoError(t, err, "should create partition")

			if lastUpperBound.Type == bson.TypeNull {
				assert.Equal(
					t,
					bson.TypeNull,
					partition.Key.Lower.Type,
					"",
				)
			} else {
				lower, err := bsontools.RawValueTo[bson.Raw](partition.Key.Lower)
				require.NoError(t, err)

				lowerRecID, err := lower.LookupErr(partitions.RecordID)
				require.NoError(t, err)

				assert.True(
					t,
					lastUpperBound.Equal(lowerRecID),
					"lower bound rec ID (%s) should match last upper bound (%s)",
					lowerRecID.String(),
					lastUpperBound.String(),
				)
			}

			lastUpperBound = partition.Upper

			task := tasks.Task{
				PrimaryKey: bson.NewObjectID(),
				Type:       tasks.VerifyDocuments,
				QueryFilter: tasks.QueryFilter{
					Partition: &partition,
				},
			}

			_, err = tasksColl.InsertOne(ctx, task)
			require.NoError(t, err, "should insert task")

			require.True(t, partition.Natural, "must be natural partition")
			require.NotZero(t, partition.HostnameAndPort, "need hostname")
			require.Equal(t, hostname, partition.HostnameAndPort.MustGet(), "hostname")

			if i == 0 {
				assert.Equal(t, bson.TypeNull, partition.Key.Lower.Type)
			} else {
				require.Equal(t, bson.TypeEmbeddedDocument, partition.Key.Lower.Type)
			}

			// Even the last partition should have a non-nil upper bound because
			// this is a nonempty collection.
			require.Equal(
				t,
				lo.Ternary(clustered, bson.TypeBinary, bson.TypeInt64),
				partition.Upper.Type,
			)
		}
	}
}

func getDirectClientAndHostname(
	ctx context.Context,
	t *testing.T,
	client *mongo.Client,
	connstr string,
) (*mongo.Client, string) {
	helloRaw, err := util.GetHelloRaw(ctx, client)
	require.NoError(t, err)

	hostnameRV, err := helloRaw.LookupErr("me")
	require.NoError(t, err)

	hostname, err := bsontools.RawValueTo[string](hostnameRV)
	require.NoError(t, err)

	connstr, err = compare.SetDirectHostInConnectionString(
		connstr,
		hostname,
	)
	require.NoError(t, err)

	directClient, err := mongo.Connect(options.Client().ApplyURI(connstr))
	require.NoError(t, err)

	return directClient, hostname
}

func (suite *IntegrationTestSuite) TestReadNaturalPartitionFromSource() {
	suite.skipUnlessCanPartitionNatural()

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	ctx := suite.T().Context()
	t := suite.T()

	version, err := mmongo.GetVersionArray(
		ctx,
		suite.srcMongoClient,
	)
	require.NoError(t, err)

	dbName := suite.DBNameForTest()

	logger, _ := getLoggerAndWriter("stdout")

	notifier := &MockSuccessNotifier{}

	client := suite.srcMongoClient

	directClient, hostname := getDirectClientAndHostname(ctx, t, client, suite.srcConnStr)

	for _, clustered := range mslices.Of(true, false) {
		suite.Run(
			lo.Ternary(clustered, "clustered", "nonclustered"),
			func() {
				if clustered && version[0] < 6 {
					suite.T().Skip("clustered requires v6+")
				}

				coll := client.Database(dbName).Collection("plain")

				require.NoError(t, coll.Drop(ctx))

				if clustered {
					require.NoError(
						t,
						coll.Database().CreateCollection(
							ctx,
							coll.Name(),
							options.CreateCollection().SetClusteredIndex(
								bson.D{
									{"key", bson.D{{"_id", 1}}},
									{"unique", true},
								},
							),
						),
					)
				}

				docs := lo.RepeatBy(
					10,
					func(index int) bson.D {
						return bson.D{{"_id", int32(index)}}
					},
				)

				for i := range 10 {
					_, err := coll.InsertOne(ctx, docs[i])
					require.NoError(t, err)
				}

				topRecordOpt, err := partitions.GetTopRecordID(
					ctx,
					coll,
				)
				suite.Require().NoError(err)

				resumeTokens := []bson.Raw{}

				resp := coll.Database().RunCommand(
					ctx,
					bson.D{
						{"find", coll.Name()},
						{"batchSize", 1},
						{"$_requestResumeToken", true},
						{"hint", bson.D{{"$natural", 1}}},
					},
				)

				cur, err := cursor.New(coll.Database(), resp, nil)
				require.NoError(t, err, "should open cursor")
				for !cur.IsFinished() {
					rtOpt, err := cursor.GetResumeToken(cur)
					require.NoError(t, err)

					rt := rtOpt.MustGetf("must have resume token")

					resumeTokens = append(resumeTokens, rt)

					require.NoError(t, cur.GetNext(ctx, bson.E{"batchSize", 1}))
				}

				suite.Run(
					"0 to 4",
					func() {
						task := &tasks.Task{
							PrimaryKey: bson.NewObjectID(),
							Type:       tasks.VerifyDocuments,
							Status:     tasks.Processing,
							QueryFilter: tasks.QueryFilter{
								Namespace: FullName(coll),
								Partition: &partitions.Partition{
									Natural:         true,
									HostnameAndPort: option.Some(hostname),
									Upper:           lo.Must(resumeTokens[3].LookupErr(partitions.RecordID)),
								},
							},
						}

						toCompare := make(chan []compare.DocWithTS, 100)
						toDst := make(chan []compare.DocID, 100)

						err = compare.ReadNaturalPartitionFromSource(
							ctx,
							logger,
							&MockSuccessNotifier{},
							directClient,
							coll,
							task,
							option.None[bson.D](),
							compare.Binary,
							toCompare,
							toDst,
						)
						require.NoError(t, err, "should read")

						compared := lo.Flatten(lo.ChannelToSlice(toCompare))
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.PutInPool()
							}

							for _, d := range dstFetches {
								d.PutInPool()
							}
						}()

						assert.Equal(
							t,
							docs[:4],
							lo.Map(
								compared,
								func(d compare.DocWithTS, _ int) bson.D {
									var doc bson.D
									require.NoError(t, bson.Unmarshal(d.Doc, &doc))

									return doc
								},
							),
							"compare thread should receive expected docs",
						)

						assert.Equal(
							t,
							lo.Range(4),
							lo.Map(
								dstFetches,
								func(d compare.DocID, _ int) int {
									id, err := bsontools.RawValueTo[int](d.ID)
									require.NoError(t, err)

									return id
								},
							),
							"dst reader thread should receive expected doc IDs",
						)
					},
				)

				suite.Run(
					"4 to 8",
					func() {
						task := &tasks.Task{
							PrimaryKey: bson.NewObjectID(),
							Type:       tasks.VerifyDocuments,
							Status:     tasks.Processing,
							QueryFilter: tasks.QueryFilter{
								Namespace: FullName(coll),
								Partition: &partitions.Partition{
									Natural:         true,
									HostnameAndPort: option.Some(hostname),
									Key: partitions.PartitionKey{
										Lower: bsontools.ToRawValue(resumeTokens[3]),
									},
									Upper: lo.Must(resumeTokens[7].LookupErr(partitions.RecordID)),
								},
							},
						}

						toCompare := make(chan []compare.DocWithTS, 100)
						toDst := make(chan []compare.DocID, 100)

						err = compare.ReadNaturalPartitionFromSource(
							ctx,
							logger,
							notifier,
							directClient,
							coll,
							task,
							option.None[bson.D](),
							compare.Binary,
							toCompare,
							toDst,
						)
						require.NoError(t, err, "should read")

						compared := lo.Flatten(lo.ChannelToSlice(toCompare))
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.PutInPool()
							}

							for _, d := range dstFetches {
								d.PutInPool()
							}
						}()

						assert.Equal(
							t,
							docs[4:8],
							lo.Map(
								compared,
								func(d compare.DocWithTS, _ int) bson.D {
									var doc bson.D
									require.NoError(t, bson.Unmarshal(d.Doc, &doc))

									return doc
								},
							),
							"compare thread should receive expected docs",
						)

						assert.Equal(
							t,
							lo.RepeatBy(4, func(i int) int {
								return 4 + i
							}),
							lo.Map(
								dstFetches,
								func(d compare.DocID, _ int) int {
									id, err := bsontools.RawValueTo[int](d.ID)
									require.NoError(t, err)

									return id
								},
							),
							"dst reader thread should receive expected doc IDs",
						)
					},
				)

				suite.Run(
					"8 to end",
					func() {
						task := &tasks.Task{
							PrimaryKey: bson.NewObjectID(),
							Type:       tasks.VerifyDocuments,
							Status:     tasks.Processing,
							QueryFilter: tasks.QueryFilter{
								Namespace: FullName(coll),
								Partition: &partitions.Partition{
									Natural:         true,
									HostnameAndPort: option.Some(hostname),
									Key: partitions.PartitionKey{
										Lower: bsontools.ToRawValue(resumeTokens[7]),
									},
									Upper: topRecordOpt.MustGetf("must have top record ID"),
								},
							},
						}

						toCompare := make(chan []compare.DocWithTS, 100)
						toDst := make(chan []compare.DocID, 100)

						err = compare.ReadNaturalPartitionFromSource(
							ctx,
							logger,
							notifier,
							directClient,
							coll,
							task,
							option.None[bson.D](),
							compare.Binary,
							toCompare,
							toDst,
						)
						require.NoError(t, err, "should read")

						compared := lo.Flatten(lo.ChannelToSlice(toCompare))
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.PutInPool()
							}

							for _, d := range dstFetches {
								d.PutInPool()
							}
						}()

						assert.Equal(
							t,
							docs[8:],
							lo.Map(
								compared,
								func(d compare.DocWithTS, _ int) bson.D {
									var doc bson.D
									require.NoError(t, bson.Unmarshal(d.Doc, &doc))

									return doc
								},
							),
							"compare thread should receive expected docs",
						)

						assert.Equal(
							t,
							lo.RepeatBy(len(docs)-8, func(i int) int {
								return 8 + i
							}),
							lo.Map(
								dstFetches,
								func(d compare.DocID, _ int) int {
									id, err := bsontools.RawValueTo[int](d.ID)
									require.NoError(t, err)

									return id
								},
							),
							"dst reader thread should receive expected doc IDs",
						)
					},
				)

				deleted, err := coll.DeleteMany(ctx, bson.D{{"_id", bson.D{{"$lt", 9}}}})
				require.EqualValues(t, 9, deleted.DeletedCount)

				suite.Run(
					"8 to end with missing resume token",
					func() {
						task := &tasks.Task{
							PrimaryKey: bson.NewObjectID(),
							Type:       tasks.VerifyDocuments,
							Status:     tasks.Processing,
							QueryFilter: tasks.QueryFilter{
								Namespace: FullName(coll),
								Partition: &partitions.Partition{
									Natural:         true,
									HostnameAndPort: option.Some(hostname),
									Key: partitions.PartitionKey{
										Lower: bsontools.ToRawValue(resumeTokens[7]),
									},
									Upper: topRecordOpt.MustGetf("must have top record ID"),
								},
							},
						}

						toCompare := make(chan []compare.DocWithTS, 100)
						toDst := make(chan []compare.DocID, 100)

						err = compare.ReadNaturalPartitionFromSource(
							ctx,
							logger,
							notifier,
							directClient,
							coll,
							task,
							option.None[bson.D](),
							compare.Binary,
							toCompare,
							toDst,
						)
						require.NoError(t, err, "should read")

						compared := lo.Flatten(lo.ChannelToSlice(toCompare))
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.PutInPool()
							}

							for _, d := range dstFetches {
								d.PutInPool()
							}
						}()

						assert.Equal(
							t,
							docs[9:],
							lo.Map(
								compared,
								func(d compare.DocWithTS, _ int) bson.D {
									var doc bson.D
									require.NoError(t, bson.Unmarshal(d.Doc, &doc))

									return doc
								},
							),
							"compare thread should receive expected docs",
						)

						assert.Equal(
							t,
							lo.RepeatBy(len(docs)-9, func(i int) int {
								return 9 + i
							}),
							lo.Map(
								dstFetches,
								func(d compare.DocID, _ int) int {
									id, err := bsontools.RawValueTo[int](d.ID)
									require.NoError(t, err)

									return id
								},
							),
							"dst reader thread should receive expected doc IDs",
						)
					},
				)
			},
		)

	}
}
