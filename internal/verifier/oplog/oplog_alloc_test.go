package oplog

import (
	"testing"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestOpUnmarshal_NoExtraAllocs is a tripwire: if a future change adds heap
// allocation to the per-oplog-entry hot path (e.g. by reintroducing an
// iter.Seq2 closure or a *bson.Timestamp pointer field), this fails. The
// budget is the count measured against the current implementation; bumping
// it requires a deliberate decision because every alloc here multiplies by
// the oplog read rate (in tailOplog mode this fires per oplog entry, plus
// once per inner op of every applyOps).
func TestOpUnmarshal_NoExtraAllocs(t *testing.T) {
	op := Op{
		Op:     "i",
		TS:     bson.Timestamp{T: 1, I: 2},
		Ns:     "mydb.mycoll",
		DocLen: 777,
		DocID:  mbson.ToRawValue("someid"),
	}
	raw := bson.Raw(lo.Must(bson.Marshal(op)))

	// Warm up so any one-time package-init allocs don't taint the measurement.
	var rt Op
	require.NoError(t, (&rt).UnmarshalFromBSON(raw))

	const budget = 2

	avg := testing.AllocsPerRun(1000, func() {
		var rt Op
		_ = (&rt).UnmarshalFromBSON(raw)
	})

	t.Logf("Op.UnmarshalFromBSON: %.2f allocs/op (budget %d)", avg, budget)

	require.LessOrEqualf(
		t, avg, float64(budget),
		"Op.UnmarshalFromBSON: %.2f allocs/op exceeds budget of %d. "+
			"If you intentionally added an alloc, bump the budget; "+
			"otherwise this is a regression.",
		avg, budget,
	)
}
