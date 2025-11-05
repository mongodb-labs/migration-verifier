package recheck

import (
	"math/rand/v2"
	"testing"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPrimaryKeyBSON(t *testing.T) {
	pk := PrimaryKey{
		SrcDatabaseName:   "mydb",
		SrcCollectionName: "mycoll",
		DocumentID:        mbson.ToRawValue(123123),
		Rand:              rand.Int32(),
	}

	assert.Panics(
		t,
		func() { bson.Marshal(pk) },
		"plain Marshal() panics",
	)

	raw := lo.Must(pk.MarshalToBSON())

	var rt PrimaryKey
	require.NoError(t, bson.Unmarshal(raw, &rt))

	assert.Equal(t, pk, rt, "should round-trip")
}

func TestDocBSON(t *testing.T) {
	doc := Doc{
		PrimaryKey: PrimaryKey{
			SrcDatabaseName:   "mydb",
			SrcCollectionName: "mycoll",
			DocumentID:        mbson.ToRawValue("heyhey"),
		},
	}

	assert.Panics(
		t,
		func() { bson.Marshal(doc) },
		"plain Marshal() panics",
	)

	raw := lo.Must(doc.MarshalToBSON())

	var rt Doc
	require.NoError(t, bson.Unmarshal(raw, &rt))

	assert.Equal(t, doc, rt, "doc should round-trip")
}
