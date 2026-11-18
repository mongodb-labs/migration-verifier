package verifier

import (
	"testing"

	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// The comparator requires both sides to finish the same natural batch.
func TestCompareDocsFromChannels_NaturalBatchNumbers(t *testing.T) {
	verifier := NewVerifier(VerifierSettings{}, "stderr")
	verifier.docCompareMethod = compare.Binary

	task := tasks.Task{
		PrimaryKey:  bson.NewObjectID(),
		QueryFilter: tasks.QueryFilter{Namespace: "testdb.testns"},
	}

	// makeChannel returns a closed channel that holds one document and then
	// a batch-done message with the given batch number.
	makeChannel := func(batchNum compare.BatchDoneComparatorMsg) <-chan compare.ToComparatorMsg {
		ch := make(chan compare.ToComparatorMsg, 2)

		ch <- compare.DocsToComparatorMsg{
			compare.NewDocWithTSFromPool(
				testutil.MustMarshal(bson.D{{"_id", 1}}),
				bson.Timestamp{T: 1},
			),
		}
		ch <- batchNum
		close(ch)

		return ch
	}

	compareDocs := func(srcBatchNum, dstBatchNum compare.BatchDoneComparatorMsg) (DocCompareReport, error) {
		return verifier.compareDocsFromChannels(
			t.Context(),
			0,
			&testutil.MockSuccessNotifier{T: t},
			&task,
			makeChannel(srcBatchNum),
			makeChannel(dstBatchNum),
			make(chan DocCompareReport, 10),
		)
	}

	t.Run("same batch", func(t *testing.T) {
		report, err := compareDocs(0, 0)
		require.NoError(t, err)

		assert.Empty(t, report.Problems)
		assert.EqualValues(t, 1, report.DocCount)
	})

	t.Run("different batches", func(t *testing.T) {
		assert.Panics(
			t,
			func() { _, _ = compareDocs(0, 1) },
			"the sides finished different batches",
		)
	})
}
