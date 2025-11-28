package mbson

import (
	"math"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestInt(t *testing.T) {
	ints := []int{
		0,
		-1,
		math.MaxInt32 - 1,
		math.MaxInt32,
		math.MaxInt32 + 1,
		math.MaxInt64,
		math.MinInt32 - 1,
		math.MinInt32,
		math.MinInt32 + 1,
		math.MinInt64,
	}

	for _, cur := range ints {
		viaMarshal := MustConvertToRawValue(cur)

		viaUs := ToRawValue(cur)

		assert.Equal(t, viaMarshal, viaUs, "%d", cur)
	}
}

func TestInt32(t *testing.T) {
	ints := []int32{
		0,
		-1,
		math.MaxInt32 - 1,
		math.MaxInt32,
		math.MinInt32,
		math.MinInt32 + 1,
	}

	for _, cur := range ints {
		viaMarshal := MustConvertToRawValue(cur)

		viaUs := ToRawValue(cur)

		assert.Equal(t, viaMarshal, viaUs, "%d", cur)

		assert.Equal(t, cur, lo.Must(CastRawValue[int32](viaMarshal)), "round-trip")
	}
}

func TestInt64(t *testing.T) {
	ints := []int64{
		0,
		-1,
		math.MaxInt32 - 1,
		math.MaxInt32,
		math.MaxInt32 + 1,
		math.MaxInt64,
		math.MinInt32 - 1,
		math.MinInt32,
		math.MinInt32 + 1,
		math.MinInt64,
	}

	for _, cur := range ints {
		viaMarshal := MustConvertToRawValue(cur)

		viaUs := ToRawValue(cur)

		assert.Equal(t, viaMarshal, viaUs, "%d", cur)
	}
}

func TestString(t *testing.T) {
	vals := []string{
		"",
		"0",
		"abc",
		"ábç",
	}

	for _, cur := range vals {
		viaMarshal := MustConvertToRawValue(cur)

		viaUs := ToRawValue(cur)

		assert.Equal(t, viaMarshal, viaUs, "%d", cur)

		assert.Equal(t, cur, lo.Must(CastRawValue[string](viaUs)))
	}
}

func TestRaw(t *testing.T) {
	vals := []bson.Raw{
		lo.Must(bson.Marshal(bson.D{})),
		lo.Must(bson.Marshal(bson.D{{"", nil}})),
		lo.Must(bson.Marshal(bson.D{{"a", 1.2}})),
	}

	for _, cur := range vals {
		viaMarshal := MustConvertToRawValue(cur)

		assert.Equal(t, cur, lo.Must(CastRawValue[bson.Raw](viaMarshal)))
	}
}

func TestTimestamp(t *testing.T) {
	vals := []bson.Timestamp{
		{0, 0},
		{1, 1},
		{math.MaxUint32, math.MaxUint32},
	}

	for _, cur := range vals {
		viaMarshal := MustConvertToRawValue(cur)

		assert.Equal(t, cur, lo.Must(CastRawValue[bson.Timestamp](viaMarshal)))
	}
}

func TestObjectID(t *testing.T) {
	vals := []bson.ObjectID{
		bson.NewObjectID(),
		{},
	}

	for _, cur := range vals {
		viaMarshal := MustConvertToRawValue(cur)

		assert.Equal(t, viaMarshal, ToRawValue(cur))

		assert.Equal(t, cur, lo.Must(CastRawValue[bson.ObjectID](viaMarshal)))
	}
}
