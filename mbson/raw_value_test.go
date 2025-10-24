package mbson

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestToRawValue_Int(t *testing.T) {
	ints := []int{
		0,
		-1,
		math.MaxInt32 - 1,
		math.MaxInt32,
		math.MaxInt32 + 1,
		math.MaxInt64,
	}

	for _, cur := range ints {
		viaMarshal := MustConvertToRawValue(cur)

		viaUs := ToRawValue(cur)

		assert.Equal(t, viaMarshal, viaUs, "%d", cur)
	}
}
