package verifier

import (
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestOplogReader_SourceDDL verifies that source DDL crashes the oplog reader.
func (suite *IntegrationTestSuite) TestOplogReader_SourceDDL() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()

	var reader changeReader = verifier.newOplogReader(
		nil,
		src,
		verifier.srcClient,
		*verifier.srcClusterInfo,
	)

	dbName := suite.DBNameForTest()

	coll := verifier.srcClient.Database(dbName).Collection("coll")

	eg, egCtx := contextplus.ErrGroup(ctx)
	suite.Require().NoError(reader.start(egCtx, eg))
	lo.Must(coll.InsertOne(ctx, bson.D{{"_id", "hey"}}))

	batchReceiver := reader.getReadChannel()

	select {
	case <-ctx.Done():
		suite.Require().NoError(ctx.Err())
	case _, isOpen := <-batchReceiver:
		suite.Assert().False(isOpen, "channel should close")
	}

	err := eg.Wait()
	suite.Assert().ErrorAs(err, &UnknownEventError{})

	// Confirm that the error text is wrapped:
	suite.Assert().Contains(err.Error(), "reading")
}

// TestOplogReader_Documents verifies that the oplog reader sees & publishes
// document changes on the source.
func (suite *IntegrationTestSuite) TestOplogReader_Documents() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()

	dbName := suite.DBNameForTest()

	coll := verifier.srcClient.Database(dbName).Collection("coll")

	outFilterColl := verifier.srcClient.
		Database(suite.DBNameForTest()).
		Collection("coll2")

	lo.Must(coll.InsertOne(ctx, bson.D{{"_id", "hey"}}))
	lo.Must(outFilterColl.InsertOne(ctx, bson.D{{"_id", "hey"}}))

	if suite.GetTopology(verifier.srcClient) == util.TopologySharded {
		suite.T().Skipf("oplog mode is only for unsharded clusters")
	}

	var reader changeReader = verifier.newOplogReader(
		mslices.Of(FullName(coll)),
		src,
		verifier.srcClient,
		*verifier.srcClusterInfo,
	)

	batchReceiver := reader.getReadChannel()

	var lastResumeTokenTS bson.Timestamp

	getBatch := func() eventBatch {
		batch, isOpen := <-batchReceiver
		suite.Require().True(isOpen, "channel should still be open")

		rtTS, err := mbson.Lookup[bson.Timestamp](batch.resumeToken, "ts")
		suite.Require().NoError(err)

		lastResumeTokenTS = rtTS

		suite.Require().False(
			rtTS.Before(*lo.LastOrEmpty(batch.events).ClusterTime),
			"resume token must not predate the last event",
		)

		return batch
	}

	eg, egCtx := contextplus.ErrGroup(ctx)
	suite.Require().NoError(reader.start(egCtx, eg))

	// NB: This should be the first event we see because the most
	// recent op before this was for an out-filter namespace.
	suite.Run(
		"insert one",
		func() {

			raw := lo.Must(bson.Marshal(bson.D{{"_id", "ho"}}))
			lo.Must(coll.InsertOne(ctx, raw))
			batch := getBatch()
			event := batch.events[0]

			suite.Assert().Equal(
				NewNamespace(dbName, coll.Name()),
				event.Ns,
			)
			suite.Assert().Equal("insert", event.OpType)
			suite.Assert().Equal("ho", lo.Must(mbson.CastRawValue[string](event.DocID)))
			suite.Assert().EqualValues(len(raw), event.FullDocLen.MustGet(), "doc length")
		},
	)

	suite.Run(
		"update one",
		func() {
			lo.Must(coll.UpdateOne(
				ctx,
				bson.D{{"_id", "hey"}},
				bson.D{{"$set", bson.D{{"foo", "bar"}}}},
			))

			batch := getBatch()
			event := batch.events[0]

			suite.Assert().Equal(
				NewNamespace(dbName, coll.Name()),
				event.Ns,
			)
			suite.Assert().Equal("update", event.OpType)
			suite.Assert().Equal("hey", lo.Must(mbson.CastRawValue[string](event.DocID)))
			suite.Assert().EqualValues(defaultUserDocumentSize, event.FullDocLen.MustGet())
		},
	)

	suite.Run(
		"replace one",
		func() {
			raw := lo.Must(bson.Marshal(bson.D{{"_id", "ho"}, {"a", "b"}}))

			lo.Must(coll.ReplaceOne(ctx, bson.D{{"_id", "ho"}}, raw))
			batch := getBatch()
			event := batch.events[0]

			suite.Assert().Equal(
				NewNamespace(dbName, coll.Name()),
				event.Ns,
			)
			suite.Assert().Equal("replace", event.OpType)
			suite.Assert().Equal("ho", lo.Must(mbson.CastRawValue[string](event.DocID)))
			suite.Assert().EqualValues(len(raw), event.FullDocLen.MustGet(), "doc length")
		},
	)

	suite.Run(
		"delete one",
		func() {
			// Now check that the reader understands bulk inserts.
			lo.Must(coll.DeleteOne(ctx, bson.D{{"_id", "hey"}}))
			batch := getBatch()
			event := batch.events[0]

			suite.Assert().Equal(
				NewNamespace(dbName, coll.Name()),
				event.Ns,
			)
			suite.Assert().Equal("delete", event.OpType)
			suite.Assert().Equal("hey", lo.Must(mbson.CastRawValue[string](event.DocID)))
			suite.Assert().EqualValues(defaultUserDocumentSize, event.FullDocLen.MustGet())
		},
	)

	bulkDocs := []bson.D{
		{{"_id", 1.25}},
		{{"_id", 1.5}},
		{{"_id", 1.75}},
		{{"_id", 2.25}},
	}

	suite.Run(
		"bulk insert",
		func() {
			lo.Must(coll.InsertMany(ctx, lo.ToAnySlice(bulkDocs)))

			docLen := len(lo.Must(bson.Marshal(bulkDocs[0])))

			events := []ParsedEvent{}

			for len(events) < 4 {
				batch := getBatch()
				events = append(events, batch.events...)
			}

			suite.Require().Len(events, 4)

			for i, event := range events {
				suite.Assert().Equal("insert", event.OpType)
				suite.Assert().EqualValues(docLen, event.FullDocLen.MustGet())

				suite.Assert().Equal(
					bulkDocs[i][0].Value,
					lo.Must(mbson.CastRawValue[float64](event.DocID)),
					"events[%d].DocID", i,
				)
			}
		},
	)

	suite.Run(
		"bulk update",
		func() {
			docIDs := lo.Map(
				bulkDocs,
				func(d bson.D, _ int) any {
					return d[0].Value
				},
			)

			lo.Must(coll.UpdateMany(
				ctx,
				bson.D{{"_id", bson.D{{"$in", docIDs}}}},
				bson.D{{"$set", bson.D{{"aa", "bb"}}}},
			))

			events := []ParsedEvent{}

			for len(events) < 4 {
				batch := getBatch()
				events = append(events, batch.events...)
			}

			suite.Require().Len(events, 4)

			for _, event := range events {
				suite.Assert().Equal("update", event.OpType)
				suite.Assert().EqualValues(defaultUserDocumentSize, event.FullDocLen.MustGet())
			}

			eventDocIDs := lo.Map(
				events,
				func(event ParsedEvent, _ int) any {
					return lo.Must(mbson.CastRawValue[float64](event.DocID))
				},
			)

			suite.Assert().ElementsMatch(docIDs, eventDocIDs)
		},
	)

	suite.Run(
		"bulk delete",
		func() {
			docIDs := lo.Map(
				bulkDocs,
				func(d bson.D, _ int) any {
					return d[0].Value
				},
			)

			lo.Must(coll.DeleteMany(ctx, bson.D{{"_id", bson.D{{"$in", docIDs}}}}))

			events := []ParsedEvent{}

			for len(events) < 4 {
				batch := getBatch()
				events = append(events, batch.events...)
			}

			suite.Require().Len(events, 4)

			for _, event := range events {
				suite.Assert().Equal("delete", event.OpType)
				suite.Assert().EqualValues(defaultUserDocumentSize, event.FullDocLen.MustGet())
			}

			eventDocIDs := lo.Map(
				events,
				func(event ParsedEvent, _ int) any {
					return lo.Must(mbson.CastRawValue[float64](event.DocID))
				},
			)

			suite.Assert().ElementsMatch(docIDs, eventDocIDs)
		},
	)

	reader.setWritesOff(lastResumeTokenTS)
	suite.Require().NoError(eg.Wait())
}
