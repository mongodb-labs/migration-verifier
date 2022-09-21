package verifier

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func (suite *MultiMetaVersionTestSuite) TestLargeIDInsertions() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	overlyLarge := 7 * 1024 * 1024 // Three of these exceed our 16MB limit, but two do not
	id1 := strings.Repeat("a", overlyLarge)
	id2 := strings.Repeat("b", overlyLarge)
	id3 := strings.Repeat("c", overlyLarge)
	ids := []interface{}{id1, id2, id3}
	dataSizes := []int{overlyLarge, overlyLarge, overlyLarge}
	err := verifier.insertRecheckDocs(ctx, 0, "testDB", "testColl", ids, dataSizes)
	suite.Require().Nil(err)
	metaColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(recheckQueue)
	d1 := RecheckDoc{
		PrimaryKey: RecheckPrimaryKey{
			Generation:     0,
			DatabaseName:   "testDB",
			CollectionName: "testColl",
			DocumentID:     id1,
		},
		DataSize: overlyLarge}
	d2 := d1
	d2.PrimaryKey.DocumentID = id2
	d3 := d1
	d3.PrimaryKey.DocumentID = id3
	cursor, err := metaColl.Find(ctx, bson.D{})
	suite.Require().Nil(err)
	var results []RecheckDoc
	err = cursor.All(ctx, &results)
	suite.Require().Nil(err)
	suite.ElementsMatch([]interface{}{d1, d2, d3}, results)

	err = verifier.GenerateRecheckTasks(ctx, 0)
	suite.Require().Nil(err)
	taskColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
	cursor, err = taskColl.Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{"_id", 0}}))
	suite.Require().Nil(err)
	var actualTasks []VerificationTask
	err = cursor.All(ctx, &actualTasks)
	suite.Require().Nil(err)

	t1 := VerificationTask{
		Generation: 1,
		Ids:        []interface{}{id1, id2},
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerify,
		QueryFilter: QueryFilter{
			Namespace: "testDB.testColl",
			To:        "testDB.testColl",
		}}
	t2 := t1
	t2.Ids = []interface{}{id3}
	suite.ElementsMatch([]VerificationTask{t1, t2}, actualTasks)
}

func (suite *MultiMetaVersionTestSuite) TestLargeDataInsertions() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	verifier.partitionSizeInBytes = 1024 * 1024
	ctx := context.Background()

	id1 := "a"
	id2 := "b"
	id3 := "c"
	ids := []interface{}{id1, id2, id3}
	dataSizes := []int{400 * 1024, 700 * 1024, 1024}
	err := verifier.insertRecheckDocs(ctx, 0, "testDB", "testColl", ids, dataSizes)
	suite.Require().Nil(err)
	metaColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(recheckQueue)
	d1 := RecheckDoc{
		PrimaryKey: RecheckPrimaryKey{
			Generation:     0,
			DatabaseName:   "testDB",
			CollectionName: "testColl",
			DocumentID:     id1,
		},
		DataSize: dataSizes[0]}
	d2 := d1
	d2.PrimaryKey.DocumentID = id2
	d2.DataSize = dataSizes[1]
	d3 := d1
	d3.PrimaryKey.DocumentID = id3
	d3.DataSize = dataSizes[2]
	cursor, err := metaColl.Find(ctx, bson.D{})
	suite.Require().Nil(err)
	var results []RecheckDoc
	err = cursor.All(ctx, &results)
	suite.Require().Nil(err)
	suite.ElementsMatch([]interface{}{d1, d2, d3}, results)

	err = verifier.GenerateRecheckTasks(ctx, 0)
	suite.Require().Nil(err)
	taskColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
	cursor, err = taskColl.Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{"_id", 0}}))
	suite.Require().Nil(err)
	var actualTasks []VerificationTask
	err = cursor.All(ctx, &actualTasks)
	suite.Require().Nil(err)

	t1 := VerificationTask{
		Generation: 1,
		Ids:        []interface{}{id1, id2},
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerify,
		QueryFilter: QueryFilter{
			Namespace: "testDB.testColl",
			To:        "testDB.testColl",
		}}
	t2 := t1
	t2.Ids = []interface{}{id3}
	suite.ElementsMatch([]VerificationTask{t1, t2}, actualTasks)
}

func (suite *MultiMetaVersionTestSuite) TestMultipleNamespaces() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	id1 := "a"
	id2 := "b"
	id3 := "c"
	ids := []interface{}{id1, id2, id3}
	dataSizes := []int{1000, 1000, 1000}
	err := verifier.insertRecheckDocs(ctx, 0, "testDB1", "testColl1", ids, dataSizes)
	suite.Require().Nil(err)
	err = verifier.insertRecheckDocs(ctx, 0, "testDB1", "testColl2", ids, dataSizes)
	suite.Require().Nil(err)
	err = verifier.insertRecheckDocs(ctx, 0, "testDB2", "testColl1", ids, dataSizes)
	suite.Require().Nil(err)
	err = verifier.insertRecheckDocs(ctx, 0, "testDB2", "testColl2", ids, dataSizes)
	suite.Require().Nil(err)

	err = verifier.GenerateRecheckTasks(ctx, 0)
	suite.Require().Nil(err)
	taskColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(verificationTasksCollection)
	cursor, err := taskColl.Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{"_id", 0}}))
	suite.Require().Nil(err)
	var actualTasks []VerificationTask
	err = cursor.All(ctx, &actualTasks)
	suite.Require().Nil(err)

	t1 := VerificationTask{
		Generation: 1,
		Ids:        []interface{}{id1, id2, id3},
		Status:     verificationTaskAdded,
		Type:       verificationTaskVerify,
		QueryFilter: QueryFilter{
			Namespace: "testDB1.testColl1",
			To:        "testDB1.testColl1",
		}}
	t2, t3, t4 := t1, t1, t1
	t2.QueryFilter.Namespace = "testDB2.testColl1"
	t3.QueryFilter.To = "testDB1.testColl2"
	t4.QueryFilter.Namespace = "testDB2.testColl2"
	t2.QueryFilter.To = "testDB2.testColl1"
	t3.QueryFilter.Namespace = "testDB1.testColl2"
	t4.QueryFilter.To = "testDB2.testColl2"
	suite.ElementsMatch([]VerificationTask{t1, t2, t3, t4}, actualTasks)
}

func (suite *MultiMetaVersionTestSuite) TestGenerationalClear() {
	verifier := buildVerifier(suite.T(), suite.srcMongoInstance, suite.dstMongoInstance, suite.metaMongoInstance)
	ctx := context.Background()

	id1 := "a"
	id2 := "b"
	ids := []interface{}{id1, id2}
	dataSizes := []int{1000, 1000}
	err := verifier.insertRecheckDocs(ctx, 0, "testDB", "testColl", ids, dataSizes)
	suite.Require().Nil(err)
	err = verifier.insertRecheckDocs(ctx, 1, "testDB", "testColl", ids, dataSizes)
	suite.Require().Nil(err)
	err = verifier.insertRecheckDocs(ctx, 2, "testDB", "testColl", ids, dataSizes)
	suite.Require().Nil(err)

	metaColl := suite.metaMongoClient.Database(verifier.metaDBName).Collection(recheckQueue)
	d1 := RecheckDoc{
		PrimaryKey: RecheckPrimaryKey{
			Generation:     0,
			DatabaseName:   "testDB",
			CollectionName: "testColl",
			DocumentID:     id1,
		},
		DataSize: dataSizes[0]}
	d2 := d1
	d2.PrimaryKey.DocumentID = id2
	d2.DataSize = dataSizes[1]
	d3 := d1
	d3.PrimaryKey.Generation = 1
	d4 := d2
	d4.PrimaryKey.Generation = 1
	d5 := d1
	d5.PrimaryKey.Generation = 2
	d6 := d2
	d6.PrimaryKey.Generation = 2
	cursor, err := metaColl.Find(ctx, bson.D{})
	suite.Require().Nil(err)
	var results []RecheckDoc
	err = cursor.All(ctx, &results)
	suite.Require().Nil(err)
	suite.ElementsMatch([]interface{}{d1, d2, d3, d4, d5, d6}, results)

	err = verifier.ClearRecheckDocs(ctx, 1)
	suite.Require().Nil(err)
	cursor, err = metaColl.Find(ctx, bson.D{})
	suite.Require().Nil(err)
	err = cursor.All(ctx, &results)
	suite.Require().Nil(err)
	suite.ElementsMatch([]interface{}{d1, d2, d5, d6}, results)

	err = verifier.ClearRecheckDocs(ctx, 0)
	suite.Require().Nil(err)
	cursor, err = metaColl.Find(ctx, bson.D{})
	suite.Require().Nil(err)
	err = cursor.All(ctx, &results)
	suite.Require().Nil(err)
	suite.ElementsMatch([]interface{}{d5, d6}, results)

	err = verifier.ClearRecheckDocs(ctx, 2)
	suite.Require().Nil(err)
	cursor, err = metaColl.Find(ctx, bson.D{})
	suite.Require().Nil(err)
	err = cursor.All(ctx, &results)
	suite.Require().Nil(err)
	suite.ElementsMatch([]interface{}{}, results)
}
