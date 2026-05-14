package api_test

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestChangeEventCounts_ExtJSONKeys verifies that ChangeEventCounts marshals
// to extended JSON with lowercase keys, matching the webserver's output
// (which uses bson.MarshalExtJSON).
func TestChangeEventCounts_ExtJSONKeys(t *testing.T) {
	counts := api.ChangeEventCounts{
		Insert:  1,
		Update:  2,
		Replace: 3,
		Delete:  4,

		Create:                   5,
		Modify:                   6,
		CreateIndexes:            7,
		DropIndexes:              8,
		ShardCollection:          9,
		ReshardCollection:        10,
		RefineCollectionShardKey: 11,
	}

	payload, err := bson.MarshalExtJSON(counts, true, false)
	require.NoError(t, err)

	var got map[string]any
	require.NoError(t, bson.UnmarshalExtJSON(payload, true, &got))

	assert.Equal(t, map[string]any{
		"insert":  int64(1),
		"update":  int64(2),
		"replace": int64(3),
		"delete":  int64(4),

		"create":                   int64(5),
		"modify":                   int64(6),
		"createIndexes":            int64(7),
		"dropIndexes":              int64(8),
		"shardCollection":          int64(9),
		"reshardCollection":        int64(10),
		"refineCollectionShardKey": int64(11),
	}, got)
}
