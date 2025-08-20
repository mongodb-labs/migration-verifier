package verifier

import (
	"github.com/10gen/migration-verifier/dockey"
	"github.com/10gen/migration-verifier/dockey/test"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/exp/slices"
)

func (suite *IntegrationTestSuite) TestExtractTrueDocKeyAgg() {
	suite.testExtractTrueDocKeyAgg(false)
}

func (suite *IntegrationTestSuite) TestExtractTrueDocKeyAgg_Reverse() {
	suite.testExtractTrueDocKeyAgg(true)
}

func (suite *IntegrationTestSuite) testExtractTrueDocKeyAgg(reverseYN bool) {
	t := suite.T()

	ctx := t.Context()

	require := require.New(t)

	client := suite.srcMongoClient

	db := client.Database(suite.DBNameForTest())
	defer func() {
		err := db.Drop(ctx)
		if err != nil {
			t.Logf("WARNING: Failed to drop DB %#q: %v", db.Name(), err)
		} else {
			t.Logf("Dropped DB %#q", db.Name())
		}
	}()

	// Insert into a real collection so that we can test on
	// server versions that lack the $documents aggregation stage.
	coll := db.Collection("stuff")

	_, err := coll.InsertMany(
		ctx,
		lo.Map(
			test.TestCases,
			func(tc test.TestCase, _ int) any {
				return tc.Doc
			},
		),
	)
	require.NoError(err, "should insert")

	fieldNames := slices.Clone(test.FieldNames)
	if reverseYN {
		slices.Reverse(fieldNames)
	}

	computedDocKeyAgg := dockey.ExtractTrueDocKeyAgg(
		fieldNames,
		"$$ROOT",
	)

	cursor, err := coll.Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$replaceWith", computedDocKeyAgg}},
		},
	)
	require.NoError(err, "should open cursor to agg")
	defer cursor.Close(ctx)

	var computedDocKeys []bson.D
	require.NoError(cursor.All(ctx, &computedDocKeys))
	require.Len(computedDocKeys, len(test.TestCases))

	for c, curCase := range test.TestCases {
		expectedDocKey := slices.Clone(curCase.DocKey)
		if reverseYN {
			slices.Reverse(expectedDocKey)
		}

		assert.Equal(
			suite.T(),
			expectedDocKey,
			computedDocKeys[c],
			"doc key for %+v",
			curCase.Doc,
		)
	}
}
