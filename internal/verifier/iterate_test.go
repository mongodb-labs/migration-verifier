package verifier

import (
	"strings"
	"testing"

	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (s *IntegrationTestSuite) TestIterateCursorToComparator() {
	ctx := s.Context()

	coll := s.srcMongoClient.Database(s.DBNameForTest()).Collection("stuff")

	docs := []bson.D{
		{
			{"_id", 123},
			{"bigStr", strings.Repeat("x", compare.ToComparatorByteLimit/3)},
		},
		{
			{"_id", 234},
			{"bigStr", strings.Repeat("x", compare.ToComparatorByteLimit/3)},
		},
		{
			{"_id", 345},
			{"bigStr", strings.Repeat("x", compare.ToComparatorByteLimit/3)},
		},
		{
			{"_id", 456},
			{"bigStr", strings.Repeat("x", compare.ToComparatorByteLimit/3)},
		},
	}

	_, err := coll.InsertMany(ctx, docs)
	s.Require().NoError(err)

	receiver := make(chan compare.ToComparatorMsg, 100)

	sess, err := s.srcMongoClient.StartSession()
	s.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)

	theCursor, err := coll.Find(sctx, bson.D{})
	s.Require().NoError(err)

	docsSent, err := iterateCursorToComparator(
		sctx,
		&testutil.MockSuccessNotifier{T: s.T()},
		theCursor,
		receiver,
	)
	s.Require().NoError(err)

	close(receiver)

	s.Assert().EqualValues(len(docs), docsSent, "should send all docs")

	gotBatches := lo.ChannelToSlice(receiver)
	s.Assert().Len(gotBatches, 2, "expected # of batches sent")

	s.Require().NotEmpty(gotBatches)
	firstBatch := docsFromComparatorMsg(s.T(), gotBatches[0])
	s.Assert().NotEmpty(firstBatch, "1st batch")

	s.Require().Greater(len(gotBatches), 1)
	s.Assert().Len(
		docsFromComparatorMsg(s.T(), gotBatches[1]),
		len(docs)-len(firstBatch),
		"2nd batch length",
	)
}

// docsFromComparatorMsg returns the documents in a message to the comparator.
// A batch-done message has none.
func docsFromComparatorMsg(t *testing.T, msg compare.ToComparatorMsg) []compare.DocWithTS {
	t.Helper()

	switch typed := msg.(type) {
	case compare.DocsToComparatorMsg:
		return typed
	case compare.BatchDoneComparatorMsg:
		return nil
	default:
		require.Fail(t, "unexpected message to comparator", "type: %T", msg)

		return nil
	}
}
