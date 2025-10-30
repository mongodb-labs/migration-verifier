package mbson

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestRawElements(t *testing.T) {
	srcD := bson.D{
		{"foo", "xxx"},
		{"bar", "baz"},
	}

	mydoc := lo.Must(bson.Marshal(srcD))

	received := bson.D{}

	for el, err := range RawElements(mydoc) {
		require.NoError(t, err, "should iterate")

		received = append(received, bson.E{
			lo.Must(el.KeyErr()),
			lo.Must(el.ValueErr()).StringValue(),
		})
	}

	assert.Equal(t, srcD, received, "should iterate all fields")

	// Now make the document invalid
	mydoc = mydoc[:len(mydoc)-3]

	received = received[:0]

	var iterErr error
	for _, err := range RawElements(mydoc) {
		if err != nil {
			iterErr = err
			break
		}
	}

	assert.Error(t, iterErr, "should fail somewhere in the iteration")
}
