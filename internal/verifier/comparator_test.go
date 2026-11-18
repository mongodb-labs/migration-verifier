package verifier

import (
	"errors"
	"testing"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/samber/lo"
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

// When one reader finishes a natural batch first, the comparator pauses it
// until the other reader catches up, then resumes it for the next batch.
// The channels are unbuffered, as in production, so a paused reader blocks
// until the comparator reads from it again.
func TestCompareDocsFromChannels_NaturalBatchCatchUp(t *testing.T) {
	verifier := NewVerifier(VerifierSettings{}, "stderr")
	verifier.docCompareMethod = compare.Binary

	task := tasks.Task{
		PrimaryKey:  bson.NewObjectID(),
		QueryFilter: tasks.QueryFilter{Namespace: "testdb.testns"},
	}

	docs := func(ids ...int32) compare.ToComparatorMsg {
		return compare.DocsToComparatorMsg(lo.Map(
			ids,
			func(id int32, i int) compare.DocWithTS {
				return compare.NewDocWithTSFromPool(
					testutil.MustMarshal(bson.D{{"_id", id}}),
					bson.Timestamp{T: 1, I: uint32(i)},
				)
			},
		))
	}

	cases := []struct {
		label    string
		src, dst []compare.ToComparatorMsg

		// The IDs of the documents that should be reported, each once.
		expectedProblemIDs []int64
	}{
		{
			// A destination with the same _id on 2 shards returns a surplus
			// copy, so the destination sends more payloads than the source.
			label: "destination has more payloads",
			src: []compare.ToComparatorMsg{
				docs(1, 2, 3),
				compare.BatchDoneComparatorMsg(0),
				docs(4),
				compare.BatchDoneComparatorMsg(1),
			},
			dst: []compare.ToComparatorMsg{
				docs(1),
				docs(2),
				docs(3),
				docs(2),
				compare.BatchDoneComparatorMsg(0),
				docs(4),
				compare.BatchDoneComparatorMsg(1),
			},
			expectedProblemIDs: []int64{2},
		},
		{
			// The destination lacks documents, so it sends fewer payloads.
			label: "source has more payloads",
			src: []compare.ToComparatorMsg{
				docs(1),
				docs(2),
				docs(3),
				compare.BatchDoneComparatorMsg(0),
				docs(4),
				compare.BatchDoneComparatorMsg(1),
			},
			dst: []compare.ToComparatorMsg{
				docs(1),
				compare.BatchDoneComparatorMsg(0),
				docs(4),
				compare.BatchDoneComparatorMsg(1),
			},
			expectedProblemIDs: []int64{2, 3},
		},
	}

	for _, curCase := range cases {
		t.Run(curCase.label, func(t *testing.T) {
			// A stall fails the test quickly instead of hanging it.
			ctx, cancel := contextplus.WithTimeoutCause(
				t.Context(),
				30*time.Second,
				errors.New("comparison stalled"),
			)
			defer cancel()

			srcChannel := make(chan compare.ToComparatorMsg)
			dstChannel := make(chan compare.ToComparatorMsg)

			eg, egCtx := contextplus.ErrGroup(ctx)

			sendAll := func(ch chan<- compare.ToComparatorMsg, msgs []compare.ToComparatorMsg) func() error {
				return func() error {
					defer close(ch)

					for _, msg := range msgs {
						if err := chanutil.WriteWithDoneCheck(egCtx, ch, msg); err != nil {
							return err
						}
					}

					return nil
				}
			}

			eg.Go(sendAll(srcChannel, curCase.src))
			eg.Go(sendAll(dstChannel, curCase.dst))

			reportsChan := make(chan DocCompareReport, 10)

			report, err := verifier.compareDocsFromChannels(
				egCtx,
				0,
				&testutil.MockSuccessNotifier{T: t},
				&task,
				srcChannel,
				dstChannel,
				reportsChan,
			)
			require.NoError(t, err, "comparison should finish")

			// The paused reader could send its next batch only if the
			// comparator resumed reading from it.
			require.NoError(t, eg.Wait(), "both readers should send everything")

			close(reportsChan)
			problems := report.Problems
			for flushed := range reportsChan {
				problems = append(problems, flushed.Problems...)
			}

			assert.EqualValues(t, 4, report.DocCount, "source documents compared")
			assert.ElementsMatch(
				t,
				curCase.expectedProblemIDs,
				lo.Map(problems, func(p compare.Result, _ int) int64 {
					return p.ID.AsInt64()
				}),
				"each unmatched document is reported once",
			)
		})
	}
}
