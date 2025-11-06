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
		func() { _, _ = bson.Marshal(pk) },
		"plain Marshal() panics",
	)

	raw := lo.Must(pk.MarshalToBSON())

	assert.NoError(t, bson.Unmarshal(raw, &bson.D{}), "marshal outputs BSON")

	var rt PrimaryKey
	assert.Panics(
		t,
		func() { _ = bson.Unmarshal(raw, &rt) },
		"plain Unmarshal() panics",
	)

	require.NoError(t, (&rt).UnmarshalFromBSON(raw))

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
		func() { _, _ = bson.Marshal(doc) },
		"plain Marshal() panics",
	)

	raw := lo.Must(doc.MarshalToBSON())

	assert.NoError(t, bson.Unmarshal(raw, &bson.D{}), "marshal outputs BSON")

	var rt Doc

	assert.Panics(
		t,
		func() { _ = bson.Unmarshal(raw, &rt) },
		"plain Unmarshal() panics",
	)

	require.NoError(t, (&rt).UnmarshalFromBSON(raw))

	assert.Equal(t, doc, rt, "doc should round-trip")
}
