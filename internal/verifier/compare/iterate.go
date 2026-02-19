package compare

import (
	"cmp"
	"context"
	"slices"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo/cursor"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// IterateCursorToChannel returns the # of documents sent to the channel.
//
// NB: This DOES NOT close the given channel.
func IterateCursorToChannel(
	sctx context.Context,
	state retry.SuccessNotifier,
	cursor cursor.Abstract,
	writer chan<- []DocWithTS,
) (int, error) {
	sess := mongo.SessionFromContext(sctx)
	if sess == nil {
		panic("need a session")
	}

	var docsCount int
	var bytesEnqueued types.ByteCount

	var docsWithTSCache []DocWithTS

	flush := func() error {
		err := chanutil.WriteWithDoneCheck(
			sctx,
			writer,
			slices.Clone(docsWithTSCache),
		)

		if err != nil {
			return errors.Wrapf(
				err,
				"sending %d documents (%s) to compare thread",
				len(docsWithTSCache),
				reportutils.FmtBytes(bytesEnqueued),
			)
		}

		state.NoteSuccess(
			"sent %d documents (%s) to compare thread",
			len(docsWithTSCache),
			reportutils.FmtBytes(bytesEnqueued),
		)

		docsWithTSCache = docsWithTSCache[:0]
		bytesEnqueued = 0

		return nil
	}

	for cursor.Next(sctx) {
		state.NoteSuccess("received a document")

		docsCount++

		clusterTime, err := util.GetClusterTimeFromSession(sess)
		if err != nil {
			return 0, errors.Wrap(err, "reading cluster time from session")
		}

		needFlush := cmp.Or(
			len(docsWithTSCache) == ToComparatorBatchSize,
			int(bytesEnqueued)+len(cursor.Current()) > ToComparatorByteLimit,
		)

		if needFlush {
			if err := flush(); err != nil {
				return 0, err
			}
		}

		docsWithTSCache = append(
			docsWithTSCache,
			NewDocWithTSFromPool(cursor.Current(), clusterTime),
		)

		bytesEnqueued += types.ByteCount(len(cursor.Current()))
	}

	if cursor.Err() != nil {
		return 0, errors.Wrap(cursor.Err(), "failed to iterate cursor")
	}

	state.NoteSuccess("exhausted cursor")

	if len(docsWithTSCache) > 0 {
		if err := flush(); err != nil {
			return 0, err
		}
	}

	return docsCount, nil
}
