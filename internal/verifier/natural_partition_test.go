package verifier

import (
	"strings"

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
)

func (suite *IntegrationTestSuite) TestPartitionCollectionNaturalOrder() {
	ctx := suite.T().Context()
	t := suite.T()

	for _, clustered := range mslices.Of(true, false) {
		topology := suite.GetTopology(suite.srcMongoClient)
		if topology != util.TopologyReplset {
			t.Skipf("source must be replset; instead we have %#q", topology)
		}

		version, err := mmongo.GetVersionArray(
			ctx,
			suite.srcMongoClient,
		)
		require.NoError(t, err)

		if version[0] < 6 && clustered {
			suite.T().Skip("clustered requires v6+")
		}

		if err := mmongo.WhyFindCannotResume([2]int(version[:])); err != nil {
			suite.T().Skipf("no natural scan (%v)", err)
		}

		logger, _ := getLoggerAndWriter("stdout")

		coll := suite.srcMongoClient.Database(suite.DBNameForTest()).Collection("c")

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
		_, err = coll.InsertMany(ctx, docs)
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

		for i, result := range results {
			partition, err := result.Get()
			require.NoError(t, err, "should create partition")

			// TODO: Ensure that partitions are non-overlapping.

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
			require.NotZero(t, partition.Hostname, "need hostname")
			require.Equal(t, hostname, partition.Hostname.MustGet(), "hostname")

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

func (suite *IntegrationTestSuite) TestReadNaturalPartitionFromSource() {
	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	if suite.GetTopology(suite.srcMongoClient) != util.TopologyReplset {
		suite.T().Skipf("Source must be a replica set.")
	}

	ctx := suite.T().Context()
	t := suite.T()

	version, err := mmongo.GetVersionArray(
		ctx,
		suite.srcMongoClient,
	)
	require.NoError(t, err)

	if err := mmongo.WhyFindCannotResume([2]int(version[:])); err != nil {
		suite.T().Skipf("no natural scan (%v)", err)
	}

	dbName := suite.DBNameForTest()

	logger, _ := getLoggerAndWriter("stdout")

	notifier := &MockSuccessNotifier{}

	client := suite.srcMongoClient

	helloRaw, err := util.GetHelloRaw(ctx, client)
	require.NoError(t, err)

	hostnameRV, err := helloRaw.LookupErr("me")
	require.NoError(t, err)

	hostname, err := bsontools.RawValueTo[string](hostnameRV)
	require.NoError(t, err)

	connstr, err := compare.SetDirectHostInConnectionString(
		suite.srcConnStr,
		hostname,
	)
	require.NoError(t, err)

	directClient, err := mongo.Connect(options.Client().ApplyURI(connstr))
	require.NoError(t, err)

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
					rt, err := cursor.GetResumeToken(cur)
					require.NoError(t, err)

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
									Natural:  true,
									Hostname: option.Some(hostname),
									Upper:    lo.Must(resumeTokens[3].LookupErr("$recordId")),
								},
							},
						}

						toCompare := make(chan compare.DocWithTS, 100)
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

						compared := lo.ChannelToSlice(toCompare)
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.Release()
							}

							for _, d := range dstFetches {
								d.Release()
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
									Natural:  true,
									Hostname: option.Some(hostname),
									Key: partitions.PartitionKey{
										Lower: bsontools.ToRawValue(resumeTokens[3]),
									},
									Upper: lo.Must(resumeTokens[7].LookupErr("$recordId")),
								},
							},
						}

						toCompare := make(chan compare.DocWithTS, 100)
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

						compared := lo.ChannelToSlice(toCompare)
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.Release()
							}

							for _, d := range dstFetches {
								d.Release()
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
									Natural:  true,
									Hostname: option.Some(hostname),
									Key: partitions.PartitionKey{
										Lower: bsontools.ToRawValue(resumeTokens[7]),
									},
								},
							},
						}

						toCompare := make(chan compare.DocWithTS, 100)
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

						compared := lo.ChannelToSlice(toCompare)
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.Release()
							}

							for _, d := range dstFetches {
								d.Release()
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
									Natural:  true,
									Hostname: option.Some(hostname),
									Key: partitions.PartitionKey{
										Lower: bsontools.ToRawValue(resumeTokens[7]),
									},
								},
							},
						}

						toCompare := make(chan compare.DocWithTS, 100)
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

						compared := lo.ChannelToSlice(toCompare)
						dstFetches := lo.Flatten(lo.ChannelToSlice(toDst))

						defer func() {
							for _, c := range compared {
								c.Release()
							}

							for _, d := range dstFetches {
								d.Release()
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
