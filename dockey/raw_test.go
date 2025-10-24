package dockey

import (
	"slices"
	"testing"

	"github.com/10gen/migration-verifier/dockey/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestExtractTrueDocKeyFromDoc(t *testing.T) {
	for _, reverseYN := range []bool{false, true} {
		fieldNames := slices.Clone(test.FieldNames)

		if reverseYN {
			slices.Reverse(fieldNames)
		}

		for _, curCase := range test.TestCases {
			raw, err := bson.Marshal(curCase.Doc)
			require.NoError(t, err)

			computedRaw, err := ExtractTrueDocKeyFromDoc(
				fieldNames,
				raw,
			)
			require.NoError(t, err)

			var computedDocKey bson.D
			require.NoError(t, bson.Unmarshal(computedRaw, &computedDocKey))

			expectedDocKey := slices.Clone(curCase.DocKey)
			if reverseYN {
				slices.Reverse(expectedDocKey)
			}

			assert.Equal(
				t,
				expectedDocKey,
				computedDocKey,
				"doc key for %v (fieldNames: %v)",
				bson.Raw(raw),
				fieldNames,
			)
		}
	}
}
