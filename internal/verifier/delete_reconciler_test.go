package verifier

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// rawValueKey must distinguish between same-shape values of different BSON
// types, since MongoDB _id equality respects type (e.g. int32 1 != int64 1).
func TestRawValueKey_DistinguishesTypes(t *testing.T) {
	mkInt32 := func(v int32) bson.RawValue {
		raw, err := bson.Marshal(bson.D{{"v", v}})
		if err != nil {
			t.Fatalf("marshal int32: %v", err)
		}
		return bson.Raw(raw).Lookup("v")
	}
	mkInt64 := func(v int64) bson.RawValue {
		raw, err := bson.Marshal(bson.D{{"v", v}})
		if err != nil {
			t.Fatalf("marshal int64: %v", err)
		}
		return bson.Raw(raw).Lookup("v")
	}

	int32_1 := mkInt32(1)
	int64_1 := mkInt64(1)

	assert.NotEqual(t, rawValueKey(int32_1), rawValueKey(int64_1),
		"int32(1) and int64(1) must produce distinct keys")
	assert.Equal(t, rawValueKey(int32_1), rawValueKey(mkInt32(1)),
		"two int32(1) values must produce identical keys")
}

func TestRawValueKey_HandlesEmptyValue(t *testing.T) {
	rv := bson.RawValue{Type: bson.TypeNull}
	key := rawValueKey(rv)
	assert.Equal(t, 1, len(key), "null key should be just the type byte")
}
