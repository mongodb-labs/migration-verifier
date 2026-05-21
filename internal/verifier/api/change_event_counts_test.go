package api_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/10gen/migration-verifier/internal/verifier/api"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestChangeEventCounts_MarshalZerologObject(t *testing.T) {
	logAndParse := func(counts api.ChangeEventCounts) map[string]any {
		var buf bytes.Buffer
		logger := zerolog.New(&buf)
		logger.Info().Object("counts", counts).Send()

		var logLine map[string]any
		require.NoError(t, json.Unmarshal(buf.Bytes(), &logLine))

		countsMap, ok := logLine["counts"].(map[string]any)
		require.True(t, ok, "log line should have a 'counts' object")
		return countsMap
	}

	t.Run("zero fields omitted", func(t *testing.T) {
		got := logAndParse(api.ChangeEventCounts{Insert: 3})
		assert.Equal(t, map[string]any{"insert": float64(3)}, got)
	})

	t.Run("all fields present when non-zero", func(t *testing.T) {
		got := logAndParse(api.ChangeEventCounts{
			Insert:  1,
			Update:  2,
			Replace: 3,
			Delete:  4,
		})
		assert.Equal(t, map[string]any{
			"insert":  float64(1),
			"update":  float64(2),
			"replace": float64(3),
			"delete":  float64(4),
		}, got)
	})

	t.Run("all zero produces empty object", func(t *testing.T) {
		got := logAndParse(api.ChangeEventCounts{})
		assert.Empty(t, got)
	})
}

// TestChangeEventCounts_ExtJSONKeys verifies that ChangeEventCounts marshals
// to extended JSON with lowercase keys, matching the webserver's output
// (which uses bson.MarshalExtJSON).
func TestChangeEventCounts_ExtJSONKeys(t *testing.T) {
	counts := api.ChangeEventCounts{
		Insert:  1,
		Update:  2,
		Replace: 3,
		Delete:  4,
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
	}, got)
}
