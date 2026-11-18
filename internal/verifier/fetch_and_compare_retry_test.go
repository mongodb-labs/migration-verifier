package verifier

import (
	"context"
	"testing"

	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/testutil"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/compare"
	"github.com/10gen/migration-verifier/internal/verifier/tasks"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// This ensures that, when an attempt’s comparator finishes but a reader then
// fails transiently, only the retry’s final report reaches the consumer.
// The consumer treats a problem-free report as the end of the task, so an
// early one would end the task before the retry finishes.
func TestFetchAndCompareDocuments_FinalReportOnlyFromSuccessfulAttempt(t *testing.T) {
	verifier := NewVerifier(VerifierSettings{}, "stderr")

	// Set directly to avoid checking server version.
	verifier.docCompareMethod = compare.Binary

	task := tasks.Task{
		PrimaryKey: bson.NewObjectID(),
		QueryFilter: tasks.QueryFilter{
			Namespace: "db.coll",
			ShardKeys: []string{},
		},
	}

	retryDocs := []bson.D{
		{{"_id", 1}},
		{{"_id", 2}},
	}

	var attempts int
	var comparatorExited chan struct{}

	getFetchers := func(*tasks.Task) (
		<-chan compare.ToComparatorMsg,
		<-chan compare.ToComparatorMsg,
		func(context.Context, retry.SuccessNotifier) error,
		func(context.Context, retry.SuccessNotifier) error,
		error,
	) {
		attempts++

		srcChan := make(chan compare.ToComparatorMsg)
		dstChan := make(chan compare.ToComparatorMsg)

		exited := make(chan struct{})
		comparatorExited = exited

		if attempts == 1 {
			// Both readers close their channels without sending anything, so
			// the comparator finishes without problems. Only after that does
			// the source reader report a transient error.
			readSrc := func(ctx context.Context, _ retry.SuccessNotifier) error {
				close(srcChan)

				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-exited:
				}

				return mongo.CommandError{
					Code:    util.CursorKilledErrCode,
					Message: "injected transient failure",
				}
			}

			readDst := func(context.Context, retry.SuccessNotifier) error {
				close(dstChan)
				return nil
			}

			return srcChan, dstChan, readSrc, readDst, nil
		}

		sendDocs := func(ctx context.Context, ch chan compare.ToComparatorMsg) error {
			defer close(ch)

			docs := lo.Map(
				retryDocs,
				func(doc bson.D, i int) compare.DocWithTS {
					return compare.NewDocWithTSFromPool(
						testutil.MustMarshal(doc),
						bson.Timestamp{T: 1, I: uint32(i)},
					)
				},
			)

			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- compare.ToComparatorMsg{DocsWithTS: docs}:
				return nil
			}
		}

		readSrc := func(ctx context.Context, _ retry.SuccessNotifier) error {
			return sendDocs(ctx, srcChan)
		}

		readDst := func(ctx context.Context, _ retry.SuccessNotifier) error {
			return sendDocs(ctx, dstChan)
		}

		return srcChan, dstChan, readSrc, readDst, nil
	}

	resultsChan := verifier.fetchAndCompareDocuments(
		t.Context(),
		0,
		&task,
		getFetchers,
		func() { close(comparatorExited) },
	)

	results := lo.ChannelToSlice(resultsChan)

	require.Equal(t, 2, attempts, "should retry once")
	require.Len(t, results, 1, "should send only the retry’s final report")

	report, err := results[0].Get()
	require.NoError(t, err)

	assert.Empty(t, report.Problems)
	assert.EqualValues(t, len(retryDocs), report.DocCount, "report should be from the retry")
}
