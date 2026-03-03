package chanutil

import (
	"strconv"
	"testing"

	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
)

func TestIngestMap(t *testing.T) {
	ctx := t.Context()

	strChan := make(chan string, 100)

	intChan, done := IngestMap(ctx, strChan, strconv.Itoa)

	func() {
		for i := range 90 {
			intChan <- i
		}
	}()

	close(intChan)
	<-done

	close(strChan)
	strs := lo.ChannelToSlice(strChan)

	assert.Equal(
		t,
		mslices.Map1(lo.Range(90), strconv.Itoa),
		strs,
		"should have expected strings",
	)
}
