package mbson

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestIterator(t *testing.T) {
	var buffer []byte

	docs := []bson.D{
		{
			{"foo", 1234.0},
			{"bar", 324.0},
			{"baz", primitive.Timestamp{234, 435}},
		},
	}

	for _, doc := range docs {
		raw, err := bson.Marshal(doc)
		require.NoError(t, err)

		buffer = append(buffer, raw...)
	}

	iter := NewIterator(bytes.NewReader(buffer))

	var gotDocs []bson.D

	for {
		rawOpt, err := iter.Next()
		require.NoError(t, err)

		raw, has := rawOpt.Get()
		if !has {
			break
		}

		var d bson.D
		require.NoError(t, bson.Unmarshal(raw, &d))

		gotDocs = append(gotDocs, d)
	}

	assert.Equal(t, docs, gotDocs, "round-trip through iterator")
}
