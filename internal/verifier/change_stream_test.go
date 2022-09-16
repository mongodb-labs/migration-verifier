package verifier

import (
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestChangeStreamFilter(t *testing.T) {
	verifier := Verifier{}
	verifier.SetMetaDBName("metadb")
	require.Equal(t, []bson.D{{{"$match", bson.D{{"ns.db", bson.D{{"$ne", "metadb"}}}}}}},
		verifier.GetChangeStreamFilter())
	verifier.srcNamespaces = []string{"foo.bar", "foo.baz", "test.car", "test.chaz"}
	require.Equal(t, []bson.D{
		{{"$match", bson.D{
			{"$or", bson.A{
				bson.D{{"ns", bson.D{{"db", "foo"}, {"coll", "bar"}}}},
				bson.D{{"ns", bson.D{{"db", "foo"}, {"coll", "baz"}}}},
				bson.D{{"ns", bson.D{{"db", "test"}, {"coll", "car"}}}},
				bson.D{{"ns", bson.D{{"db", "test"}, {"coll", "chaz"}}}},
			}},
		}}},
	}, verifier.GetChangeStreamFilter())
}
