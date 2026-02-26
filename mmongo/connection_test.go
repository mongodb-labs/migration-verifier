package mmongo

import (
	"testing"

	"github.com/mongodb-labs/migration-tools/bsontools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestGetDirectClient(t *testing.T) {
	ctx := t.Context()

	client := getClientFromEnvOrSkip(t)

	status := struct {
		Members []struct {
			Name string
		}
	}{}

	raw, err := client.Database("admin").RunCommand(
		ctx,
		bson.D{{"replSetGetStatus", 1}},
	).Raw()
	if err != nil {
		t.Skipf("failed to fetch replset status: %v", err)
	}

	require.NoError(t, bson.Unmarshal(raw, &status))

	baseConnStr := getConnStrFromEnv().MustGetf("need connstr from env")

	for _, member := range status.Members {
		client, err := GetDirectClient(baseConnStr, member.Name)
		require.NoError(t, err, "connect to %#q", member.Name)

		resp, err := client.Database("admin").
			RunCommand(ctx, bson.D{{"isMaster", 1}}).
			Raw()
		require.NoError(t, err, "send isMaster")

		me, err := bsontools.RawLookup[string](resp, "me")
		require.NoError(t, err, "need “me”")

		assert.Equal(t, member.Name, me)
	}
}
