package verifier

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/verifier/recheck"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func (suite *IntegrationTestSuite) TestFailedCompareThenReplace() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	suite.Require().NoError(
		verifier.InsertFailedCompareRecheckDocs(
			ctx,
			"the.namespace",
			[]bson.RawValue{mbson.ToRawValue("theDocID")},
			[]int32{1234},
		),
		"insert failed-comparison recheck",
	)

	recheckDocs := suite.fetchRecheckDocs(ctx, verifier)

	suite.Assert().Equal(
		[]recheck.Doc{
			{
				PrimaryKey: recheck.PrimaryKey{
					SrcDatabaseName:   "the",
					SrcCollectionName: "namespace",
					DocumentID:        mbson.ToRawValue("theDocID"),
				},
			},
		},
		recheckDocs,
		"recheck queue after insertion of failed-comparison",
	)

	event := ParsedEvent{
		OpType: "insert",
		DocID:  mbson.ToRawValue("theDocID"),
		Ns: &Namespace{
			DB:   "the",
			Coll: "namespace",
		},
		FullDocument: testutil.MustMarshal(bson.D{{"foo", 1}}),
		ClusterTime: &bson.Timestamp{
			T: uint32(time.Now().Unix()),
		},
	}

	err := verifier.HandleChangeStreamEvents(
		ctx,
		changeEventBatch{events: mslices.Of(event)},
		src,
	)
	suite.Require().NoError(err)

	recheckDocs = suite.fetchRecheckDocs(ctx, verifier)
	suite.Assert().Equal(
		[]recheck.Doc{
			{
				PrimaryKey: recheck.PrimaryKey{
					SrcDatabaseName:   "the",
					SrcCollectionName: "namespace",
					DocumentID:        mbson.ToRawValue("theDocID"),
				},
			},
		},
		recheckDocs,
		"recheck queue after insertion of change event",
	)
}

func (suite *IntegrationTestSuite) fetchRecheckDocs(ctx context.Context, verifier *Verifier) []recheck.Doc {
	metaColl := verifier.getRecheckQueueCollection(1 + verifier.generation)

	cursor, err := metaColl.Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$addFields", bson.D{
				{"_id.rand", "$$REMOVE"},
				{"dataSize", "$$REMOVE"},
			}}},
			{{"$group", bson.D{
				{"_id", "$_id"},
				{"doc", bson.D{{"$first", "$$ROOT"}}},
			}}},
			{{"$replaceRoot", bson.D{
				{"newRoot", "$doc"},
			}}},
		},
	)

	suite.Require().NoError(err, "find recheck docs")

	var results []recheck.Doc
	err = cursor.All(ctx, &results)
	suite.Require().NoError(err, "read recheck docs cursor")

	return results
}

func (suite *IntegrationTestSuite) TestRecheckResumability() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()
	verifier.SetVerifyAll(true)

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	suite.Require().NoError(runner.StartNextGeneration())
	suite.Require().NoError(runner.AwaitGenerationEnd())

	suite.Require().EqualValues(2, verifier.generation)

	verifier2 := suite.BuildVerifier()
	verifier2.SetVerifyAll(true)

	runner2 := RunVerifierCheck(ctx, suite.T(), verifier2)
	suite.Require().NoError(runner2.AwaitGenerationEnd())

	suite.Require().EqualValues(verifier.generation, verifier2.generation)
}

func (suite *IntegrationTestSuite) TestRecheckResumability_Mismatch() {
	ctx := suite.Context()

	srcColl := suite.srcMongoClient.
		Database(suite.DBNameForTest()).
		Collection("stuff")

	ns := srcColl.Database().Name() + "." + srcColl.Name()

	dstColl := suite.dstMongoClient.
		Database(srcColl.Database().Name()).
		Collection(srcColl.Name())

	for _, coll := range mslices.Of(srcColl, dstColl) {
		suite.Require().NoError(
			coll.Database().CreateCollection(ctx, coll.Name()),
		)
	}

	verifier := suite.BuildVerifier()
	verifier.SetSrcNamespaces(mslices.Of(ns))
	verifier.SetDstNamespaces(mslices.Of(ns))
	verifier.SetNamespaceMap()

	runner := RunVerifierCheck(ctx, suite.T(), verifier)
	suite.Require().NoError(runner.AwaitGenerationEnd())

	for range 10 {
		suite.Require().NoError(runner.StartNextGeneration())
		suite.Require().NoError(runner.AwaitGenerationEnd())
	}

	_, err := srcColl.InsertOne(ctx, bson.D{{"_id", "on src"}})
	suite.Require().NoError(err)

	_, err = dstColl.InsertOne(ctx, bson.D{{"_id", "on dst"}})
	suite.Require().NoError(err)

	suite.T().Logf("Running verifier until it shows mismatch (generation=%d) ...", verifier.generation)
	for {
		verificationStatus, err := verifier.GetVerificationStatus(ctx)
		suite.Require().NoError(err)

		recheckDocs := suite.fetchPendingVerifierRechecks(ctx, verifier)

		if verificationStatus.FailedTasks != 0 && len(recheckDocs) == 2 {
			break
		}

		suite.Require().NoError(runner.StartNextGeneration())
		suite.Require().NoError(runner.AwaitGenerationEnd())
	}

	suite.T().Logf("Starting a 2nd verifier and confirming that it sees the mismatches.")

	verifier2 := suite.BuildVerifier()
	verifier2.SetSrcNamespaces(mslices.Of(ns))
	verifier2.SetDstNamespaces(mslices.Of(ns))
	verifier2.SetNamespaceMap()

	runner2 := RunVerifierCheck(ctx, suite.T(), verifier2)
	suite.Require().NoError(runner2.AwaitGenerationEnd())

	suite.Require().EqualValues(verifier.generation, verifier2.generation)
	verificationStatus, err := verifier.GetVerificationStatus(ctx)
	suite.Require().NoError(err)

	suite.Require().EqualValues(
		1,
		verificationStatus.FailedTasks,
		"restarted verifier should immediately see mismatches",
	)

	recheckDocs := suite.fetchPendingVerifierRechecks(ctx, verifier2)
	suite.Require().Len(recheckDocs, 2, "expect # of rechecks: %+v", recheckDocs)
}

func (suite *IntegrationTestSuite) TestDuplicateRecheck() {
	ctx := suite.Context()

	verifier := suite.BuildVerifier()

	zerolog.SetGlobalLevel(zerolog.TraceLevel)

	docsCount := 100

	ids := lo.Range(docsCount)
	err := insertRecheckDocs(
		ctx,
		verifier,
		suite.T().Name(), "testColl",
		lo.ToAnySlice(ids),
		lo.RepeatBy(docsCount, func(_ int) int32 { return 16 }),
	)
	suite.Require().NoError(err, "should insert the first time")

	err = insertRecheckDocs(
		ctx,
		verifier,
		suite.T().Name(), "testColl",
		lo.ToAnySlice(ids),
		lo.RepeatBy(docsCount, func(_ int) int32 { return 16 }),
	)
	suite.Require().NoError(err, "should insert the second time")
}

func (suite *IntegrationTestSuite) TestManyManyRechecks() {
	if len(os.Getenv("CI")) > 0 {
		suite.T().Skip("Skipping this test in CI. (It causes GitHub Action to self-terminate.)")
	}

	verifier := suite.BuildVerifier()
	verifier.SetNumWorkers(10)
	ctx := suite.Context()

	docsCount := 12_000_000

	suite.T().Logf("Inserting %d rechecks …", docsCount)

	ids := lo.Range(docsCount)
	err := insertRecheckDocs(
		ctx,
		verifier,
		suite.T().Name(), "testColl",
		lo.ToAnySlice(ids),
		lo.RepeatBy(docsCount, func(_ int) int32 { return 16 }),
	)
	suite.Require().NoError(err)

	verifier.generation++

	suite.T().Logf("Generating recheck tasks …")
	err = verifier.GenerateRecheckTasks(ctx)
	suite.Require().NoError(err)
}

func (suite *IntegrationTestSuite) TestLargeIDInsertions() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	overlyLarge := int32(7 * 1024 * 1024) // Three of these exceed our 16MB limit, but two do not
	id1 := strings.Repeat("a", int(overlyLarge))
	id2 := strings.Repeat("b", int(overlyLarge))
	id3 := strings.Repeat("c", int(overlyLarge))
	ids := []any{id1, id2, id3}
	dataSizes := []int32{overlyLarge, overlyLarge, overlyLarge}
	err := insertRecheckDocs(ctx, verifier, "testDB", "testColl", ids, dataSizes)
	suite.Require().NoError(err)

	d1 := recheck.Doc{
		PrimaryKey: recheck.PrimaryKey{
			SrcDatabaseName:   "testDB",
			SrcCollectionName: "testColl",
			DocumentID:        mbson.ToRawValue(id1),
		},
	}
	d2 := d1
	d2.PrimaryKey.DocumentID = mbson.ToRawValue(id2)
	d3 := d1
	d3.PrimaryKey.DocumentID = mbson.ToRawValue(id3)

	results := suite.fetchRecheckDocs(ctx, verifier)
	suite.ElementsMatch([]any{d1, d2, d3}, results)

	verifier.generation++
	err = verifier.GenerateRecheckTasks(ctx)
	suite.Require().NoError(err)
	taskColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
	cursor, err := taskColl.Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{"_id", 0}}))
	suite.Require().NoError(err)
	var foundTasks []VerificationTask
	err = cursor.All(ctx, &foundTasks)
	suite.Require().NoError(err)

	t1 := VerificationTask{
		Generation: 1,
		Ids:        []any{id1},
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyDocuments,
		QueryFilter: QueryFilter{
			Namespace: "testDB.testColl",
			To:        "testDB.testColl",
		},
		SourceDocumentCount: 1,
		SourceByteCount:     types.ByteCount(overlyLarge),
	}

	t2 := t1
	t2.Ids = []any{id2}

	t3 := t1
	t3.Ids = []any{id3}

	suite.ElementsMatch([]VerificationTask{t1, t2, t3}, foundTasks)
}

func (suite *IntegrationTestSuite) TestLargeDataInsertions() {
	verifier := suite.BuildVerifier()
	verifier.partitionSizeInBytes = 1024 * 1024
	ctx := suite.Context()

	id1 := "a"
	id2 := "b"
	id3 := "c"
	ids := []any{id1, id2, id3}
	dataSizes := []int32{400 * 1024, 700 * 1024, 1024}
	err := insertRecheckDocs(ctx, verifier, "testDB", "testColl", ids, dataSizes)
	suite.Require().NoError(err)
	d1 := recheck.Doc{
		PrimaryKey: recheck.PrimaryKey{
			SrcDatabaseName:   "testDB",
			SrcCollectionName: "testColl",
			DocumentID:        mbson.ToRawValue(id1),
		},
	}
	d2 := d1
	d2.PrimaryKey.DocumentID = mbson.ToRawValue(id2)
	d3 := d1
	d3.PrimaryKey.DocumentID = mbson.ToRawValue(id3)

	results := suite.fetchRecheckDocs(ctx, verifier)
	suite.ElementsMatch([]any{d1, d2, d3}, results)

	verifier.generation++
	err = verifier.GenerateRecheckTasks(ctx)
	suite.Require().NoError(err)
	taskColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
	cursor, err := taskColl.Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{"_id", 0}}))
	suite.Require().NoError(err)
	var actualTasks []VerificationTask
	err = cursor.All(ctx, &actualTasks)
	suite.Require().NoError(err)

	t1 := VerificationTask{
		Generation: 1,
		Ids:        []any{id1, id2},
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyDocuments,
		QueryFilter: QueryFilter{
			Namespace: "testDB.testColl",
			To:        "testDB.testColl",
		},
		SourceDocumentCount: 2,
		SourceByteCount:     1126400,
	}

	t2 := t1
	t2.Ids = []any{id3}
	t2.SourceDocumentCount = 1
	t2.SourceByteCount = 1024

	suite.ElementsMatch([]VerificationTask{t1, t2}, actualTasks)
}

func (suite *IntegrationTestSuite) TestMultipleNamespaces() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	id1 := "a"
	id2 := "b"
	id3 := "c"
	ids := []any{id1, id2, id3}
	dataSizes := []int32{1000, 1000, 1000}
	err := insertRecheckDocs(ctx, verifier, "testDB1", "testColl1", ids, dataSizes)
	suite.Require().NoError(err)
	err = insertRecheckDocs(ctx, verifier, "testDB1", "testColl2", ids, dataSizes)
	suite.Require().NoError(err)
	err = insertRecheckDocs(ctx, verifier, "testDB2", "testColl1", ids, dataSizes)
	suite.Require().NoError(err)
	err = insertRecheckDocs(ctx, verifier, "testDB2", "testColl2", ids, dataSizes)
	suite.Require().NoError(err)

	verifier.generation++
	err = verifier.GenerateRecheckTasks(ctx)
	suite.Require().NoError(err)
	taskColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
	cursor, err := taskColl.Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{"_id", 0}}))
	suite.Require().NoError(err)
	var actualTasks []VerificationTask
	err = cursor.All(ctx, &actualTasks)
	suite.Require().NoError(err)

	t1 := VerificationTask{
		Generation: 1,
		Ids:        []any{id1, id2, id3},
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerifyDocuments,
		QueryFilter: QueryFilter{
			Namespace: "testDB1.testColl1",
			To:        "testDB1.testColl1",
		},
		SourceDocumentCount: 3,
		SourceByteCount:     3000,
	}
	t2, t3, t4 := t1, t1, t1
	t2.QueryFilter.Namespace = "testDB2.testColl1"
	t3.QueryFilter.To = "testDB1.testColl2"
	t4.QueryFilter.Namespace = "testDB2.testColl2"
	t2.QueryFilter.To = "testDB2.testColl1"
	t3.QueryFilter.Namespace = "testDB1.testColl2"
	t4.QueryFilter.To = "testDB2.testColl2"
	suite.ElementsMatch([]VerificationTask{t1, t2, t3, t4}, actualTasks)
}

func (suite *IntegrationTestSuite) TestGenerationalClear() {
	verifier := suite.BuildVerifier()
	ctx := suite.Context()

	id1 := "a"
	id2 := "b"
	ids := []any{id1, id2}
	dataSizes := []int32{1000, 1000}
	err := insertRecheckDocs(ctx, verifier, "testDB", "testColl", ids, dataSizes)
	suite.Require().NoError(err)

	d1 := recheck.Doc{
		PrimaryKey: recheck.PrimaryKey{
			SrcDatabaseName:   "testDB",
			SrcCollectionName: "testColl",
			DocumentID:        mbson.ToRawValue(id1),
		},
	}
	d2 := d1
	d2.PrimaryKey.DocumentID = mbson.ToRawValue(id2)

	results := suite.fetchRecheckDocs(ctx, verifier)
	suite.Assert().ElementsMatch([]any{d1, d2}, results)

	verifier.mux.Lock()

	verifier.generation++

	err = verifier.DropCurrentGenRecheckQueue(ctx)
	suite.Require().NoError(err)

	// This never happens in real life but is needed for this test.
	verifier.generation--

	results = suite.fetchRecheckDocs(ctx, verifier)
	suite.Assert().ElementsMatch([]any{}, results)
}

func insertRecheckDocs(
	ctx context.Context,
	verifier *Verifier,
	dbName, collName string,
	documentIDs []any,
	dataSizes []int32,
) error {
	dbNames := make([]string, len(documentIDs))
	collNames := make([]string, len(documentIDs))

	for i := range documentIDs {
		dbNames[i] = dbName
		collNames[i] = collName
	}

	rawIDs := lo.Map(
		documentIDs,
		func(idAny any, _ int) bson.RawValue {
			btype, buf := lo.Must2(bson.MarshalValue(idAny))
			return bson.RawValue{
				Type:  btype,
				Value: buf,
			}
		},
	)

	return verifier.insertRecheckDocs(ctx, dbNames, collNames, rawIDs, dataSizes)
}
