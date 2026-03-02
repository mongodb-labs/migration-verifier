package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/option"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestMismatchesInfoMarshal(t *testing.T) {
	results := []VerificationResult{
		{},
		{
			ID: mbson.ToRawValue("hello"),
		},
		{
			SrcTimestamp: option.Some(bson.Timestamp{12, 23}),
		},
		{
			DstTimestamp: option.Some(bson.Timestamp{12, 23}),
		},
		{
			ID:           mbson.ToRawValue("hello"),
			Field:        "yeah",
			Cluster:      "hoho",
			Details:      "aaa",
			NameSpace:    "xxxxx",
			SrcTimestamp: option.Some(bson.Timestamp{12, 23}),
			DstTimestamp: option.Some(bson.Timestamp{23, 24}),
		},
	}

	for _, result := range results {
		mi := MismatchInfo{
			Task:   bson.NewObjectID(),
			Detail: result,
		}

		raw := mi.MarshalToBSON()

		var rt MismatchInfo
		require.NoError(t, bson.Unmarshal(raw, &rt))

		assert.Equal(t, mi, rt, "should round-trip")
	}
}
