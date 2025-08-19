package dockey

import (
	"testing"

	"github.com/10gen/migration-verifier/dockey/test"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestExtractTrueDocKeyFromDoc(t *testing.T) {
	for _, curCase := range test.TestCases {
		raw, err := bson.Marshal(curCase.Doc)
		require.NoError(t, err)

		computedRaw, err := ExtractTrueDocKeyFromDoc(
			mslices.Of("_id", "foo.bar.baz"),
			raw,
		)
		require.NoError(t, err)

		var computedDocKey bson.D
		require.NoError(t, bson.Unmarshal(computedRaw, &computedDocKey))

		assert.Equal(
			t,
			curCase.DocKey,
			computedDocKey,
			"doc key for %v (got %v; expected %v)",
			bson.Raw(raw),
		)
	}
}
