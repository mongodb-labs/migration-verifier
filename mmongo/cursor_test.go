package mmongo

import (
	"os"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func TestGetBatch(t *testing.T) {
	ctx := t.Context()

	client := getClientFromEnv(t)

	coll := client.Database(t.Name()).Collection(
		"coll",
		options.Collection().
			SetWriteConcern(writeconcern.Majority()).
			SetReadConcern(readconcern.Majority()),
	)

	sess, err := client.StartSession(options.Session().SetCausalConsistency(true))
	require.NoError(t, err)

	sctx := mongo.NewSessionContext(ctx, sess)

	docsCount := 1_000
	const batchSize = 100

	_, err = coll.InsertMany(
		sctx,
		lo.RepeatBy(
			docsCount,
			func(index int) any {
				return bson.D{}
			},
		),
	)
	require.NoError(t, err)

	cursor, err := coll.Find(
		ctx,
		bson.D{},
		options.Find().SetBatchSize(batchSize),
	)
	require.NoError(t, err)

	cursor.SetBatchSize(batchSize)

	var docs []bson.Raw
	var buf []byte
	for range docsCount / batchSize {
		docs = docs[:0]
		buf = buf[:0]

		docs, buf, err = GetBatch(ctx, cursor, docs, buf)
		require.NoError(t, err)

		assert.Len(t, docs, 100, "should get expected batch")
	}

	assert.False(t, cursor.TryNext(ctx), "cursor should be done")
	require.NoError(t, cursor.Err(), "should be no error")
}

func TestUnmarshalCursor(t *testing.T) {
	ctx := t.Context()

	client := getClientFromEnv(t)

	cursor, err := client.Database("admin").Aggregate(
		ctx,
		mongo.Pipeline{
			{{"$documents", lo.RepeatBy(
				30,
				func(index int) bson.D {
					return bson.D{{"foo", index}}
				},
			)}},
		},
		options.Aggregate().SetBatchSize(10),
	)
	require.NoError(t, err)

	batch, err := UnmarshalCursor(ctx, cursor, []unmarshaler{})
	require.NoError(t, err)

	assert.Equal(
		t,
		lo.RepeatBy(
			30,
			func(index int) unmarshaler {
				return unmarshaler{int32(index)}
			},
		),
		batch,
		"should be as expected",
	)
}

func getClientFromEnv(t *testing.T) *mongo.Client {
	connStr := os.Getenv("MVTEST_META")
	if connStr == "" {
		t.Skipf("No MVTEST_META found; skipping.")
	}

	client, err := mongo.Connect(
		options.Client().ApplyURI(connStr),
	)
	require.NoError(t, err)

	return client
}

type unmarshaler struct {
	Foo int32
}

func (u *unmarshaler) UnmarshalFromBSON(in []byte) error {
	u.Foo = lo.Must(bson.Raw(in).LookupErr("foo")).Int32()

	return nil
}
