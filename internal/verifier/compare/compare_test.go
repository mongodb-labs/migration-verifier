package compare

import (
	"slices"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestDocWithTS(t *testing.T) {
	doc := lo.Must(bson.Marshal(bson.D{{"foo", 123}}))
	origDoc := bson.Raw(slices.Clone(doc))

	d := NewDocWithTSFromPool(
		doc,
		bson.Timestamp{123, 234},
	)
	defer d.BackToPool()

	doc[0] = 0xff

	assert.Equal(t, origDoc, d.Doc, "document is cloned")
}
