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
)

func TestUnmarshalCursor(t *testing.T) {
	ctx := t.Context()

	connStr := os.Getenv("MVTEST_META")
	if connStr == "" {
		t.Skipf("No MVTEST_META found; skipping.")
	}

	client, err := mongo.Connect(
		options.Client().ApplyURI(connStr),
	)
	require.NoError(t, err)

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

type unmarshaler struct {
	Foo int32
}

func (u *unmarshaler) UnmarshalFromBSON(in []byte) error {
	u.Foo = lo.Must(bson.Raw(in).LookupErr("foo")).Int32()

	return nil
}
