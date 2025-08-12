package dockey

import (
	"os"
	"testing"

	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestExtractDocKeyAgg(t *testing.T) {
	ctx := t.Context()

	require := require.New(t)

	cs := os.Getenv("MVTEST_DST")
	require.NotEmpty(t, cs, "need MVTEST_DST env")

	client, err := mongo.Connect(
		ctx,
		options.Client().ApplyURI(cs),
	)
	require.NoError(err, "should connect")

	clusterInfo, err := util.GetClusterInfo(
		ctx,
		logger.NewDefaultLogger(),
		client,
	)
	require.NoError(err, "should fetch topology")
	if clusterInfo.Topology != util.TopologySharded {
		t.Skip("Skipping against unsharded cluster.")
	}

	db := client.Database(t.Name())
	defer db.Drop(ctx)

	coll := db.Collection("Stuff")
	require.NoError(client.Database("admin").RunCommand(
		ctx,
		bson.D{
			{"shardCollection", db.Name() + "." + coll.Name()},
			{"key", bson.D{
				{"foo.bar.baz", 1},
			}},
		},
	).Err(),
	)

	computedDocKeyAgg := ExtractDocKeyAgg(
		mslices.Of("foo.bar.baz", "_id"),
		"$$ROOT.fullDocument",
	)

	ejson, _ := bson.MarshalExtJSON(computedDocKeyAgg, true, false)
	t.Logf("computed doc key agg: %s", string(ejson))

	for _, curCase := range testCases {
		changes, err := coll.Watch(
			ctx,
			mongo.Pipeline{
				{
					{"$addFields", bson.D{
						{"computedDocKey", computedDocKeyAgg},
					}},
				},
			},
		)
		require.NoError(err, "should open change stream")

		_, err = coll.InsertOne(ctx, curCase.doc)
		require.NoError(err, "should insert doc")

		for changes.Next(ctx) {
			var event struct {
				OperationType  string
				FullDocument   bson.D
				DocumentKey    bson.D
				ComputedDocKey bson.D
			}

			require.NoError(bson.Unmarshal(changes.Current, &event))

			if event.OperationType != "insert" {
				t.Logf("Ignoring irrelevant-seeing event: %v", changes.Current)
				continue
			}

			ejson, _ := bson.MarshalExtJSON(changes.Current, true, false)
			t.Logf("Event: %s", string(ejson))

			assert.Equal(
				t,
				event.DocumentKey,
				event.ComputedDocKey,
				"checking computed doc key for %v against server",
				curCase.doc,
			)

			break
		}

		require.NoError(changes.Err(), "change stream should not fail")

		changes.Close(ctx)
	}
}
