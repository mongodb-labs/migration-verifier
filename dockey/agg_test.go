package dockey

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAggPanic(t *testing.T) {
	assert.Panics(
		t,
		func() {
			ExtractTrueDocKeyAgg(
				[]string{"foo", "bar", "foo"},
				"$$ROOT",
			)
		},
		"duplicate field name should cause panic",
	)
}
