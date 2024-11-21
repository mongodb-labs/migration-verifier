package verifier

import (
	"context"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/stretchr/testify/suite"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

type TestTopology string

const (
	metaDBName                   = "VERIFIER_TEST_META"
	topologyEnvVar               = "MVTEST_TOPOLOGY"
	TopologyReplset TestTopology = "replset"
	TopologySharded TestTopology = "sharded"
)

type IntegrationTestSuite struct {
	suite.Suite
	srcConnStr, dstConnStr, metaConnStr             string
	srcMongoClient, dstMongoClient, metaMongoClient *mongo.Client
	testContext                                     context.Context
	contextCanceller                                context.CancelCauseFunc
	initialDbNames                                  mapset.Set[string]
}

var _ suite.TestingSuite = &IntegrationTestSuite{}

// Context returns a Context that the suite will cancel after the test.
// Always use this rather than context.Background() in tests!
func (suite *IntegrationTestSuite) Context() context.Context {
	suite.Require().NotNil(
		suite.testContext,
		"context must exist (i.e., be fetched only within a test)",
	)

	return suite.testContext
}

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
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().NoError(err, "should list database names")
		for _, dbName := range dbNames {
			suite.initialDbNames.Add(dbName)
		}
	}
}

func (suite *IntegrationTestSuite) SetupTest() {
	ctx, canceller := context.WithCancelCause(context.Background())

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

	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient} {
		dbNames, err := client.ListDatabaseNames(ctx, bson.D{})
		suite.Require().NoError(err, "should list database names")
		for _, dbName := range dbNames {
			if strings.HasPrefix(dbName, suite.DBNameForTest()) {
				suite.T().Logf("Dropping database %#q because it seems to be left over from an earlier run of this test.", dbName)
				suite.Require().NoError(client.Database(dbName).Drop(ctx))
			}

			suite.initialDbNames.Add(dbName)
		}
	}

	suite.testContext, suite.contextCanceller = ctx, canceller
}

func (suite *IntegrationTestSuite) TearDownTest() {
	suite.T().Logf("Tearing down test %#q", suite.T().Name())

	suite.contextCanceller(errors.Errorf("tearing down test %#q", suite.T().Name()))
	suite.testContext, suite.contextCanceller = nil, nil

	ctx := context.Background()
	for _, client := range []*mongo.Client{suite.srcMongoClient, suite.dstMongoClient} {
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

func (suite *IntegrationTestSuite) GetSrcTopology() TestTopology {
	hello := struct {
		Msg string
	}{}

	resp := suite.srcMongoClient.Database("admin").RunCommand(
		suite.Context(),
		bson.D{{"hello", 1}},
	)

	suite.Require().NoError(
		resp.Decode(&hello),
		"should fetch & decode hello",
	)

	return lo.Ternary(hello.Msg == "isdbgrid", TopologySharded, "")
}

func (suite *IntegrationTestSuite) BuildVerifier() *Verifier {
	qfilter := QueryFilter{Namespace: "keyhole.dealers"}
	task := VerificationTask{QueryFilter: qfilter}

	verifier := NewVerifier(VerifierSettings{})
	//verifier.SetStartClean(true)
	verifier.SetNumWorkers(3)
	verifier.SetGenerationPauseDelayMillis(0)
	verifier.SetWorkerSleepDelayMillis(0)

	ctx := suite.Context()

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

func (suite *IntegrationTestSuite) DBNameForTest(suffixes ...string) string {
	name := suite.T().Name()
	return strings.ReplaceAll(
		strings.ReplaceAll(name, "/", "-"),
		".",
		"-",
	) + strings.Join(suffixes, "")
}
