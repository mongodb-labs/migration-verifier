package localdb

import (
	"cmp"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"testing"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/samber/mo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecheckMany(t *testing.T) {
	dir, err := os.MkdirTemp("", "")
	defer os.RemoveAll(dir)

	require.NoError(t, err)

	ldb, err := New(logger.NewDefaultLogger(), dir)
	require.NoError(t, err)

	rechecks := lo.RepeatBy(
		1_000_000,
		func(i int) Recheck {
			return Recheck{
				DB:    "someDB",
				Coll:  "someColl",
				DocID: mbson.MustConvertToRawValue(rand.Float64()),
				Size:  types.ByteCount(i),
			}
		},
	)

	t.Logf("Inserting %d rechecks …", len(rechecks))

	err = ldb.InsertRechecks(0, rechecks)
	require.NoError(t, err)

	t.Logf("Rechecks inserted. Reading …")

	reader := ldb.GetRecheckReader(t.Context(), 0)
	readerResults := lo.ChannelToSlice(reader)

	t.Logf("Recheck reader done.")

	gotRechecks := lo.Map(
		readerResults,
		func(r mo.Result[Recheck], _ int) Recheck {
			return r.MustGet()
		},
	)
	t.Logf("unboxed; sorting")

	for _, curRechecks := range mslices.Of(rechecks, gotRechecks) {
		slices.SortFunc(
			curRechecks,
			func(a, b Recheck) int {
				return cmp.Or(
					cmp.Compare(a.DB, b.DB),
					cmp.Compare(a.Coll, b.Coll),
					cmp.Compare(
						string(a.DocID.Type)+string(a.DocID.Value),
						string(b.DocID.Type)+string(b.DocID.Value),
					),
				)
			},
		)
	}

	t.Logf("comparing")

	assert.Equal(t, rechecks, gotRechecks)

	t.Logf("done comparing")

	assert.NoError(
		t,
		ldb.ClearAllRechecksForGeneration(0),
		"should clear all rechecks",
	)
}

func TestRecheck(t *testing.T) {
	ctx := t.Context()

	logLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.TraceLevel)
	defer zerolog.SetGlobalLevel(logLevel)

	dir, err := os.MkdirTemp("", "")
	defer os.RemoveAll(dir)

	require.NoError(t, err)

	ldb, err := New(logger.NewDefaultLogger(), dir)
	require.NoError(t, err)

	count, err := ldb.GetRechecksCount(0)
	require.NoError(t, err)

	assert.Zero(t, count, "rechecks should start at 0")

	allRechecks := []Recheck{
		{
			DB:    "db1",
			Coll:  "coll1",
			DocID: mbson.MustConvertToRawValue("id1"),
			Size:  123,
		},
		{
			DB:    "db1",
			Coll:  "coll2",
			DocID: mbson.MustConvertToRawValue("id2"),
			Size:  234,
		},
		{
			DB:    "db2",
			Coll:  "coll3",
			DocID: mbson.MustConvertToRawValue("id2"),
			Size:  345,
		},
	}

	err = ldb.InsertRechecks(0, allRechecks)
	require.NoError(t, err)

	count, err = ldb.GetRechecksCount(0)
	require.NoError(t, err)
	assert.EqualValues(t, 3, count, "rechecks count")

	t.Run(
		"cancellation of recheck reader",
		func(t *testing.T) {
			readCtx, cancel := contextplus.WithCancelCause(ctx)
			cause := fmt.Errorf("just cuz")
			cancel(cause)
			reader := ldb.GetRecheckReader(readCtx, 0)

			for recheck := range reader {
				assert.NoError(t, recheck.Error(), "context cancellation should not cause error")
			}
		},
	)

	t.Run(
		"rechecks as expected",
		func(t *testing.T) {
			reader := ldb.GetRecheckReader(ctx, 0)
			gotRechecks := lo.Map(
				lo.ChannelToSlice(reader),
				func(r mo.Result[Recheck], _ int) Recheck {
					return r.MustGet()
				},
			)

			assert.ElementsMatch(t, allRechecks, gotRechecks)
		},
	)

	t.Run(
		"no dupe rechecks",
		func(t *testing.T) {
			err := ldb.InsertRechecks(0, allRechecks)
			require.NoError(t, err)

			reader := ldb.GetRecheckReader(ctx, 0)
			gotRechecks := lo.Map(
				lo.ChannelToSlice(reader),
				func(r mo.Result[Recheck], _ int) Recheck {
					return r.MustGet()
				},
			)

			assert.ElementsMatch(t, allRechecks, gotRechecks)
		},
	)

}
