package mmongo

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMaybeAddDirectConnection(t *testing.T) {
	cases := [][2]string{
		{
			"mongodb://johndoe:password@shardtest1-shard-00-00.1bcy5.mongodb.net:27016/?authSource=admin&ssl=true",
			"mongodb://johndoe:password@shardtest1-shard-00-00.1bcy5.mongodb.net:27016/?authSource=admin&ssl=true&directConnection=true",
		},
	}

	for _, cur := range cases {
		changed, got, err := MaybeAddDirectConnection(cur[0])
		require.NoError(t, err, "in: %#q", cur[0])

		assert.Equal(t, cur[0] == cur[1], changed, "changed-ness")
		assert.Equal(t, cur[1], got, "check expected")
	}
}
