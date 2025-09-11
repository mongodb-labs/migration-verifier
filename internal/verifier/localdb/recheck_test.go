package localdb

import (
	"fmt"
	"os"
	"testing"

	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/samber/lo"
	"github.com/samber/mo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
)

func TestRecheck(t *testing.T) {
	ctx := t.Context()

	dir, err := os.MkdirTemp("", "")
	defer os.RemoveAll(dir)

	require.NoError(t, err)

	ldb, err := New(logger.NewDefaultLogger(), dir)
	require.NoError(t, err)

	count, err := ldb.GetRechecksCount(0)
	require.NoError(t, err)

	assert.Zero(t, count, "rechecks should start at 0")

	err = ldb.InsertRechecks(
		0,
		mslices.Of("db1", "db1", "db2"),
		mslices.Of("coll1", "coll2", "coll3"),
		lo.Map(
			mslices.Of("id1", "id2", "id2"),
			func(str string, _ int) bson.RawValue {
				bType, bVal := lo.Must2(bson.MarshalValue(str))
				return bson.RawValue{
					Type:  bType,
					Value: bVal,
				}
			},
		),
		mslices.Of(123, 234, 345),
	)
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

	expectedRechecks := []Recheck{
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

			assert.ElementsMatch(t, expectedRechecks, gotRechecks)
		},
	)

	t.Run(
		"no dupe rechecks",
		func(t *testing.T) {
			err := ldb.InsertRechecks(
				0,
				mslices.Of("db1", "db1"),
				mslices.Of("coll1", "coll2"),
				mslices.Of(
					expectedRechecks[0].DocID,
					expectedRechecks[1].DocID,
				),
				mslices.Of(123, 234),
			)
			require.NoError(t, err)

			reader := ldb.GetRecheckReader(ctx, 0)
			gotRechecks := lo.Map(
				lo.ChannelToSlice(reader),
				func(r mo.Result[Recheck], _ int) Recheck {
					return r.MustGet()
				},
			)

			assert.ElementsMatch(t, expectedRechecks, gotRechecks)
		},
	)

}
