package verifier

import (
	"strings"

	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/mmongo/cursor"
	"github.com/samber/lo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

func (s *IntegrationTestSuite) TestIterateCursorToChannel() {
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

	receiver := make(chan []compare.DocWithTS, 100)

	sess, err := s.srcMongoClient.StartSession()
	s.Require().NoError(err)
	sctx := mongo.NewSessionContext(ctx, sess)

	theCursor, err := coll.Find(sctx, bson.D{})
	s.Require().NoError(err)

	docsSent, err := iterateCursorToChannel(
		sctx,
		&testutil.MockSuccessNotifier{},
		cursor.NewAbstract(theCursor),
		receiver,
	)
	s.Require().NoError(err)

	close(receiver)

	s.Assert().EqualValues(len(docs), docsSent, "should send all docs")

	gotBatches := lo.ChannelToSlice(receiver)
	s.Assert().Len(gotBatches, 2, "expected # of batches sent")

	s.Require().NotEmpty(gotBatches)
	s.Assert().NotEmpty(gotBatches[0], "1st batch")

	s.Require().Greater(len(gotBatches), 1)
	s.Assert().Len(gotBatches[1], len(docs)-len(gotBatches[0]), "2nd batch")
}
