package chanutil

import (
	"strconv"
	"testing"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIngestMap(t *testing.T) {
	ctx := t.Context()

	strChan := make(chan string, 100)

	intChan, canceler := StartIngestMap(ctx, strChan, strconv.Itoa)

	func() {
		for i := range 90 {
			intChan <- i
		}
	}()

	require.NoError(t, canceler(ctx))

	close(strChan)
	strs := lo.ChannelToSlice(strChan)

	assert.Equal(
		t,
		mslices.Map1(lo.Range(90), strconv.Itoa),
		strs,
		"should have expected strings",
	)
}
