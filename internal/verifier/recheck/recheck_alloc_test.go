package recheck

import (
	"testing"

	"github.com/10gen/migration-verifier/mbson"
	"github.com/mongodb-labs/migration-tools/option"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestDocUnmarshal_NoExtraAllocs is a tripwire: if a future change adds heap
// allocation to recheck-task building (e.g. by reintroducing an iter.Seq2
// closure inside Doc.UnmarshalFromBSON or PrimaryKey.UnmarshalFromBSON, which
// it transitively calls), this fails. The budget is the count measured
// against the current implementation. Doc.UnmarshalFromBSON is called per
// recheck doc when we cursor through the recheck queue, so allocs here
// multiply by the queue size at every generation.
func TestDocUnmarshal_NoExtraAllocs(t *testing.T) {
	doc := Doc{
		PrimaryKey: PrimaryKey{
			SrcDatabaseName:   "mydb",
			SrcCollectionName: "mycoll",
			DocumentID:        option.Some(mbson.ToRawValue(int64(42))),
		},
		DataSize:     100,
		ChangeOpTime: option.Some(bson.Timestamp{T: 1, I: 2}),
	}
	raw := doc.MarshalToBSON()

	var rt Doc
	require.NoError(t, (&rt).UnmarshalFromBSON(raw))

	const budget = 2

	avg := testing.AllocsPerRun(1000, func() {
		var rt Doc
		_ = (&rt).UnmarshalFromBSON(raw)
	})

	require.LessOrEqualf(
		t, avg, float64(budget),
		"Doc.UnmarshalFromBSON: %.2f allocs/op exceeds budget of %d. "+
			"If you intentionally added an alloc, bump the budget; "+
			"otherwise this is a regression.",
		avg, budget,
	)
}
