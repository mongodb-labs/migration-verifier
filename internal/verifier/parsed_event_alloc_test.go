package verifier

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestParsedEventUnmarshal_NoExtraAllocs is a tripwire: if a future change
// adds heap allocation to the per-change-event hot path (e.g. by
// reintroducing an iter.Seq2 closure, switching ParsedEvent.Ns back to
// *Namespace, or making ClusterTime a pointer again), this fails. The
// budget is the count measured against the current implementation;
// bumping it requires a deliberate decision because this fires once per
// change event, on both source and destination, for the duration of the
// migration.
func TestParsedEventUnmarshal_NoExtraAllocs(t *testing.T) {
	innerDoc := bson.Raw(lo.Must(bson.Marshal(bson.M{"_id": "abc", "x": int32(1)})))
	raw := bson.Raw(lo.Must(bson.Marshal(bson.M{
		"operationType": "insert",
		"ns":            bson.M{"db": "mydb", "coll": "mycoll"},
		"_docID":        "abc",
		"fullDocument":  innerDoc,
		"_fullDocLen":   int64(len(innerDoc)),
		"clusterTime":   bson.Timestamp{T: 1, I: 2},
	})))

	var pe ParsedEvent
	require.NoError(t, (&pe).UnmarshalFromBSON(raw))

	const budget = 3

	avg := testing.AllocsPerRun(1000, func() {
		var pe ParsedEvent
		_ = (&pe).UnmarshalFromBSON(raw)
	})

	require.LessOrEqualf(
		t, avg, float64(budget),
		"ParsedEvent.UnmarshalFromBSON: %.2f allocs/op exceeds budget of %d. "+
			"If you intentionally added an alloc, bump the budget; "+
			"otherwise this is a regression.",
		avg, budget,
	)
}
