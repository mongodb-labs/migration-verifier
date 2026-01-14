package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type eventBatch struct {
	events      []ParsedEvent
	resumeToken bson.Raw
}

const (
	minResumeTokenPersistInterval = 10 * time.Second
)

// RunChangeEventPersistor persists rechecks from change event batches.
// It needs to be started after the reader starts and should run in its own
// goroutine.
func (verifier *Verifier) RunChangeEventPersistor(
	ctx context.Context,
	reader changeReader,
) error {
	clusterName := reader.getWhichCluster()
	persistCallback := reader.persistResumeToken
	in := reader.getReadChannel()

	var err error

	var lastPersistedTime time.Time
	persistResumeTokenIfNeeded := func(ctx context.Context, token bson.Raw) {
		if time.Since(lastPersistedTime) >= minResumeTokenPersistInterval {
			persistErr := persistCallback(ctx, token)
			if persistErr != nil {
				verifier.logger.Warn().
					Str("changeReader", string(clusterName)).
					Err(persistErr).
					Msg("Failed to persist resume token. Because of this, if the verifier restarts, it will have to re-process already-handled change events. This error may be transient, but if it recurs, investigate.")
			} else {
				lastPersistedTime = time.Now()
			}
		}
	}

HandlerLoop:
	for err == nil {
		select {
		case <-ctx.Done():
			err = util.WrapCtxErrWithCause(ctx)

			verifier.logger.Debug().
				Err(err).
				Str("changeReader", string(clusterName)).
				Msg("Change event handler failed.")
		case batch, more := <-in:
			if !more {
				verifier.logger.Debug().
					Str("changeReader", string(clusterName)).
					Msg("Change event batch channel has been closed.")

				break HandlerLoop
			}

			// Record even empty batches since this helps to compute events per second.
			reader.noteBatchSize(len(batch.events))

			verifier.logger.Trace().
				Str("changeReader", string(clusterName)).
				Int("batchSize", len(batch.events)).
				Any("batch", batch.events).
				Stringer("resumeToken", batch.resumeToken).
				Msg("Handling change event batch.")

			if len(batch.events) > 0 {
				err = errors.Wrap(
					verifier.PersistChangeEvents(ctx, batch, reader),
					"persisting rechecks for change events",
				)
			}

			if err == nil && batch.resumeToken != nil {
				persistResumeTokenIfNeeded(ctx, batch.resumeToken)
			}
		}
	}

	return err
}

// PersistChangeEvents performs the necessary work for change events after receiving a batch.
func (verifier *Verifier) PersistChangeEvents(ctx context.Context, batch eventBatch, reader changeReader) error {
	if len(batch.events) == 0 {
		return nil
	}

	dbNames := make([]string, 0, len(batch.events))
	collNames := make([]string, 0, len(batch.events))
	docIDs := make([]bson.RawValue, 0, len(batch.events))
	dataSizes := make([]int32, 0, len(batch.events))
	opTimes := make([]bson.Timestamp, 0, len(batch.events))

	latestTimestamp := bson.Timestamp{}

	eventOrigin := reader.getWhichCluster()

	for _, changeEvent := range batch.events {
		if !supportedEventOpTypes.Contains(changeEvent.OpType) {
			panic(fmt.Sprintf("Unsupported optype in event; should have failed already! event=%+v", changeEvent))
		}

		if changeEvent.ClusterTime == nil {
			verifier.logger.Warn().
				Any("event", changeEvent).
				Msg("Change event unexpectedly lacks a clusterTime?!?")
		} else if changeEvent.ClusterTime.After(latestTimestamp) {
			latestTimestamp = *changeEvent.ClusterTime
		}

		var srcDBName, srcCollName string

		// Recheck Docs are keyed by source namespaces.
		// We need to retrieve the source namespaces if change events are from the destination.
		switch eventOrigin {
		case dst:
			if verifier.nsMap.Len() == 0 {
				// Namespace is not remapped. Source namespace is the same as the destination.
				srcDBName = changeEvent.Ns.DB
				srcCollName = changeEvent.Ns.Coll
			} else {
				if changeEvent.Ns.DB == verifier.metaDBName {
					continue
				}

				dstNs := fmt.Sprintf("%s.%s", changeEvent.Ns.DB, changeEvent.Ns.Coll)
				srcNs, exist := verifier.nsMap.GetSrcNamespace(dstNs)
				if !exist {
					return errors.Errorf("no source namespace matches the destination namepsace %#q", dstNs)
				}
				srcDBName, srcCollName = mmongo.SplitNamespace(srcNs)
			}
		case src:
			srcDBName = changeEvent.Ns.DB
			srcCollName = changeEvent.Ns.Coll
		default:
			panic(fmt.Sprintf("unknown event origin: %s", eventOrigin))
		}

		dbNames = append(dbNames, srcDBName)
		collNames = append(collNames, srcCollName)
		docIDs = append(docIDs, changeEvent.DocID)
		opTimes = append(opTimes, *changeEvent.ClusterTime)

		var dataSize int32
		if changeEvent.FullDocLen.OrZero() > 0 {
			dataSize = int32(changeEvent.FullDocLen.OrZero())
		} else if changeEvent.FullDocument == nil {
			// This happens for deletes and for some updates.
			// The document is probably, but not necessarily, deleted.
			dataSize = defaultUserDocumentSize
		} else {
			// This happens for inserts, replaces, and most updates.
			dataSize = int32(len(changeEvent.FullDocument))
		}

		dataSizes = append(dataSizes, dataSize)

		if err := reader.getEventRecorder().AddEvent(&changeEvent); err != nil {
			return errors.Wrapf(
				err,
				"failed to augment stats with %s change event (%+v)",
				eventOrigin,
				changeEvent,
			)
		}
	}

	latestTimestampTime := time.Unix(int64(latestTimestamp.T), 0)

	verifier.logger.Trace().
		Str("origin", string(eventOrigin)).
		Int("count", len(docIDs)).
		Any("latestTimestamp", latestTimestamp).
		Time("latestTimestampTime", latestTimestampTime).
		Msg("Persisting rechecks for change events.")

	return verifier.insertRecheckDocs(
		ctx,
		dbNames,
		collNames,
		docIDs,
		dataSizes,
		nil,
		option.Some(eventOrigin),
		opTimes,
	)
}
