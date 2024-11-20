package verifier

import (
	"context"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const metaDBName = "VERIFIER_TEST_META"

type IntegrationTestSuite struct {
	suite.Suite
	srcConnStr, dstConnStr, metaConnStr             string
	srcMongoClient, dstMongoClient, metaMongoClient *mongo.Client
	initialDbNames                                  mapset.Set[string]
}

var _ suite.TestingSuite = &IntegrationTestSuite{}

func (suite *IntegrationTestSuite) SetupSuite() {
	ctx := context.Background()
	clientOpts := options.Client().ApplyURI(suite.srcConnStr).SetAppName("Verifier Test Suite").SetWriteConcern(writeconcern.Majority())
	var err error

	suite.srcMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().NoError(err)

	clientOpts = options.Client().ApplyURI(suite.dstConnStr).SetAppName("Verifier Test Suite").SetWriteConcern(writeconcern.Majority())
	suite.dstMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().NoError(err)

	clientOpts = options.Client().ApplyURI(suite.metaConnStr).SetAppName("Verifier Test Suite")
	suite.metaMongoClient, err = mongo.Connect(ctx, clientOpts)
	suite.Require().NoError(err)

	suite.initialDbNames = mapset.NewSet[string]()
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient, suite.metaMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().NoError(err)
		for _, dbName := range dbNames {
			suite.initialDbNames.Add(dbName)
		}
	}
}

func (suite *IntegrationTestSuite) SetupTest() {
	ctx := context.Background()

	dbname := suite.DBNameForTest()

	suite.Require().NoError(
		suite.srcMongoClient.Database(dbname).Drop(ctx),
		"should drop source db %#q",
		dbname,
	)

	suite.Require().NoError(
		suite.dstMongoClient.Database(dbname).Drop(ctx),
		"should drop destination db %#q",
		dbname,
	)

	suite.Require().NoError(
		suite.metaMongoClient.Database(metaDBName).Drop(ctx),
		"should drop destination db %#q",
		dbname,
	)
}

func (suite *IntegrationTestSuite) TearDownTest() {
	suite.T().Logf("Tearing down test %#q", suite.T().Name())

	ctx := context.Background()
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient, suite.metaMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().NoError(err)
		for _, dbName := range dbNames {
			if !suite.initialDbNames.Contains(dbName) {
				suite.T().Logf("Dropping database %#q, which seems to have been created during test %#q.", dbName, suite.T().Name())

				err = client.Database(dbName).Drop(ctx)
				suite.Require().NoError(err)
			}
		}
	}
}

func (suite *IntegrationTestSuite) BuildVerifier() *Verifier {
	qfilter := QueryFilter{Namespace: "keyhole.dealers"}
	task := VerificationTask{QueryFilter: qfilter}

	verifier := NewVerifier(VerifierSettings{})
	//verifier.SetStartClean(true)
	verifier.SetNumWorkers(3)
	verifier.SetGenerationPauseDelayMillis(0)
	verifier.SetWorkerSleepDelayMillis(0)

	ctx := context.Background()

	suite.Require().NoError(
		verifier.SetSrcURI(ctx, suite.srcConnStr),
		"should set source connection string",
	)
	suite.Require().NoError(
		verifier.SetDstURI(ctx, suite.dstConnStr),
		"should set destination connection string",
	)
	suite.Require().NoError(
		verifier.SetMetaURI(ctx, suite.metaConnStr),
		"should set metadata connection string",
	)
	verifier.SetLogger("stderr")
	verifier.SetMetaDBName(metaDBName)

	suite.Require().NoError(verifier.srcClientCollection(&task).Drop(ctx))
	suite.Require().NoError(verifier.dstClientCollection(&task).Drop(ctx))
	suite.Require().NoError(verifier.AddMetaIndexes(ctx))
	return verifier
}

func (suite *IntegrationTestSuite) DBNameForTest() string {
	name := suite.T().Name()
	return strings.ReplaceAll(
		strings.ReplaceAll(name, "/", "-"),
		".",
		"-",
	)
}
