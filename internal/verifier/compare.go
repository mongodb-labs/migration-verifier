package verifier

import (
	"bytes"
	"context"
	"time"

	"github.com/10gen/migration-verifier/internal/types"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
)

const readTimeout = 10 * time.Minute

func (verifier *Verifier) FetchAndCompareDocuments(
	givenCtx context.Context,
	task *VerificationTask,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	// This function spawns three threads: one to read from the source,
	// another to read from the destination, and a third one to receive the
	// docs from the other 2 threads and compare them. It’s done this way,
	// rather than fetch-everything-then-compare, to minimize memory usage.
	errGroup, groupCtx := errgroup.WithContext(givenCtx)

	srcChannel, dstChannel := verifier.getFetcherChannels(groupCtx, errGroup, task)

	results := []VerificationResult{}
	var docCount types.DocumentCount
	var byteCount types.ByteCount

	errGroup.Go(func() error {
		var err error
		results, docCount, byteCount, err = verifier.compareDocsFromChannels(
			groupCtx,
			task,
			srcChannel,
			dstChannel,
		)

		return err
	})

	err := errGroup.Wait()

	return results, docCount, byteCount, err
}

func (verifier *Verifier) compareDocsFromChannels(
	ctx context.Context,
	task *VerificationTask,
	srcChannel, dstChannel <-chan bson.Raw,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	results := []VerificationResult{}
	var srcDocCount types.DocumentCount
	var srcByteCount types.ByteCount

	mapKeyFieldNames := make([]string, 1+len(task.QueryFilter.ShardKeys))
	mapKeyFieldNames[0] = "_id"
	copy(mapKeyFieldNames[1:], task.QueryFilter.ShardKeys)

	namespace := task.QueryFilter.Namespace

	srcCache := map[string]bson.Raw{}
	dstCache := map[string]bson.Raw{}

	// This is the core document-handling logic. It either:
	//
	// a) caches the new document if its mapKey is unseen, or
	// b) compares the new doc against its previously-received, cached
	//    counterpart and records any mismatch.
	handleNewDoc := func(doc bson.Raw, isSrc bool) error {
		mapKey := getMapKey(doc, mapKeyFieldNames)

		var ourMap, theirMap map[string]bson.Raw

		if isSrc {
			ourMap = srcCache
			theirMap = dstCache
		} else {
			ourMap = dstCache
			theirMap = srcCache
		}
		// See if we've already cached a document with this
		// mapKey from the other channel.
		theirDoc, exists := theirMap[mapKey]

		// If there is no such cached document, then cache the newly-received
		// document in our map then proceed to the next document.
		//
		// (We'll remove the cache entry when/if the other channel yields a
		// document with the same mapKey.)
		if !exists {
			ourMap[mapKey] = doc
			return nil
		}

		// We have two documents! First we remove the cache entry. This saves
		// memory, but more importantly, it lets us know, once we exhaust the
		// channels, which documents were missing on one side or the other.
		delete(theirMap, mapKey)

		// Now we determine which document came from whom.
		var srcDoc, dstDoc bson.Raw
		if isSrc {
			srcDoc = doc
			dstDoc = theirDoc
		} else {
			srcDoc = theirDoc
			dstDoc = doc
		}

		// Finally we compare the documents and save any mismatch report(s).
		mismatches, err := verifier.compareOneDocument(srcDoc, dstDoc, namespace)
		if err != nil {
			return errors.Wrap(err, "failed to compare documents")
		}

		results = append(results, mismatches...)

		return nil
	}

	var srcClosed, dstClosed bool

	readTimer := time.NewTimer(0)
	defer func() {
		if !readTimer.Stop() {
			<-readTimer.C
		}
	}()

	// We always read src & dst together. This ensures that, if one side
	// lags the other significantly, we won’t keep caching the faster side’s
	// documents and thus consume more & more memory.
	for !srcClosed || !dstClosed {
		simpleTimerReset(readTimer, readTimeout)

		var srcDoc, dstDoc bson.Raw

		eg, egCtx := errgroup.WithContext(ctx)

		if !srcClosed {
			eg.Go(func() error {
				var alive bool
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				case <-readTimer.C:
					return errors.Errorf(
						"failed to read from source after %s",
						readTimeout,
					)
				case srcDoc, alive = <-srcChannel:
					if !alive {
						srcClosed = true
						break
					}

					srcDocCount++
					srcByteCount += types.ByteCount(len(srcDoc))
				}

				return nil
			})
		}

		if !dstClosed {
			eg.Go(func() error {
				var alive bool
				select {
				case <-egCtx.Done():
					return egCtx.Err()
				case <-readTimer.C:
					return errors.Errorf(
						"failed to read from destination after %s",
						readTimeout,
					)
				case dstDoc, alive = <-dstChannel:
					if !alive {
						dstClosed = true
						break
					}
				}

				return nil
			})
		}

		if err := eg.Wait(); err != nil {
			return nil, 0, 0, errors.Wrap(
				err,
				"failed to read documents",
			)
		}

		if srcDoc != nil {
			err := handleNewDoc(srcDoc, true)

			if err != nil {
				return nil, 0, 0, errors.Wrapf(
					err,
					"comparer thread failed to handle %#q's source doc (task: %s) with ID %v",
					namespace,
					task.PrimaryKey,
					srcDoc.Lookup("_id"),
				)
			}
		}

		if dstDoc != nil {
			err := handleNewDoc(dstDoc, false)

			if err != nil {
				return nil, 0, 0, errors.Wrapf(
					err,
					"comparer thread failed to handle %#q's destination doc (task: %s) with ID %v",
					namespace,
					task.PrimaryKey,
					dstDoc.Lookup("_id"),
				)
			}
		}
	}

	// We got here because both srcChannel and dstChannel are closed,
	// which means we have processed all documents with the same mapKey
	// between source & destination.
	//
	// At this point, any documents left in the cache maps are simply
	// missing on the other side. We add results for those.

	// We might as well pre-grow the slice:
	results = slices.Grow(results, len(srcCache)+len(dstCache))

	for _, doc := range srcCache {
		results = append(
			results,
			VerificationResult{
				ID:        doc.Lookup("_id"),
				Details:   Missing,
				Cluster:   ClusterTarget,
				NameSpace: namespace,
				dataSize:  len(doc),
			},
		)
	}

	for _, doc := range dstCache {
		results = append(
			results,
			VerificationResult{
				ID:        doc.Lookup("_id"),
				Details:   Missing,
				Cluster:   ClusterSource,
				NameSpace: namespace,
				dataSize:  len(doc),
			},
		)
	}

	return results, srcDocCount, srcByteCount, nil
}

func simpleTimerReset(t *time.Timer, dur time.Duration) {
	if !t.Stop() {
		<-t.C
	}

	t.Reset(dur)
}

func (verifier *Verifier) getFetcherChannels(
	ctx context.Context,
	errGroup *errgroup.Group,
	task *VerificationTask,
) (<-chan bson.Raw, <-chan bson.Raw) {
	srcChannel := make(chan bson.Raw)
	dstChannel := make(chan bson.Raw)

	errGroup.Go(func() error {
		cursor, err := verifier.getDocumentsCursor(
			ctx,
			verifier.srcClientCollection(task),
			verifier.srcClusterInfo,
			verifier.srcStartAtTs,
			task,
		)

		if err == nil {
			err = errors.Wrap(
				iterateCursorToChannel(ctx, cursor, srcChannel),
				"failed to read source documents",
			)
		} else {
			err = errors.Wrap(
				err,
				"failed to find source documents",
			)
		}

		return err
	})

	errGroup.Go(func() error {
		cursor, err := verifier.getDocumentsCursor(
			ctx,
			verifier.dstClientCollection(task),
			verifier.dstClusterInfo,
			nil, //startAtTs
			task,
		)

		if err == nil {
			err = errors.Wrap(
				iterateCursorToChannel(ctx, cursor, dstChannel),
				"failed to read destination documents",
			)
		} else {
			err = errors.Wrap(
				err,
				"failed to find destination documents",
			)
		}

		return err
	})

	return srcChannel, dstChannel
}

func iterateCursorToChannel(ctx context.Context, cursor *mongo.Cursor, writer chan<- bson.Raw) error {
	for cursor.Next(ctx) {
		writer <- slices.Clone(cursor.Current)
	}

	close(writer)

	return errors.Wrap(cursor.Err(), "failed to iterate cursor")
}

func getMapKey(doc bson.Raw, fieldNames []string) string {
	var keyBuffer bytes.Buffer
	for _, keyName := range fieldNames {
		value := doc.Lookup(keyName)
		keyBuffer.Grow(1 + len(value.Value))
		keyBuffer.WriteByte(byte(value.Type))
		keyBuffer.Write(value.Value)
	}

	return keyBuffer.String()
}
