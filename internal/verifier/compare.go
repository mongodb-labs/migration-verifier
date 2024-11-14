package verifier

import (
	"bytes"
	"context"

	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"golang.org/x/exp/slices"
	"golang.org/x/sync/errgroup"
)

func (verifier *Verifier) FetchAndCompareDocuments(
	givenCtx context.Context,
	task *VerificationTask,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	var results []VerificationResult
	var docCount types.DocumentCount
	var byteCount types.ByteCount

	retryer := retry.New(retry.DefaultDurationLimit)

	err := retryer.RunForTransientErrorsOnly(
		givenCtx,
		verifier.logger,
		func(_ *retry.Info) error {
			results = []VerificationResult{}
			docCount = 0
			byteCount = 0

			// This function spawns three threads: one to read from the source,
			// another to read from the destination, and a third one to receive the
			// docs from the other 2 threads and compare them. It’s done this way,
			// rather than fetch-everything-then-compare, to minimize memory usage.
			errGroup, ctx := errgroup.WithContext(givenCtx)

			srcChannel, dstChannel := verifier.getFetcherChannels(ctx, errGroup, task)

			errGroup.Go(func() error {
				var err error
				results, docCount, byteCount, err = verifier.compareDocsFromChannels(
					ctx,
					task,
					srcChannel,
					dstChannel,
				)

				return err
			})

			return errGroup.Wait()
		},
	)

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
	var docCount types.DocumentCount
	var byteCount types.ByteCount

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
	var err error

	// We always read src & dst back & forth. This ensures that, if one side
	// lags the other significantly, we won’t keep caching the faster side’s
	// documents and thus consume more & more memory.
	for err == nil && (!srcClosed || !dstClosed) {
		if !srcClosed {
			select {
			case <-ctx.Done():
				return nil, 0, 0, ctx.Err()
			case doc, alive := <-srcChannel:
				if !alive {
					srcClosed = true
					break
				}
				docCount++
				byteCount += types.ByteCount(len(doc))

				err = handleNewDoc(doc, true)

				if err != nil {
					err = errors.Wrapf(
						err,
						"comparer thread failed to handle source doc with ID %v",
						doc.Lookup("_id"),
					)
				}
			}
		}

		if !dstClosed {
			select {
			case <-ctx.Done():
				return nil, 0, 0, ctx.Err()
			case doc, alive := <-dstChannel:
				if !alive {
					dstClosed = true
					break
				}

				err = handleNewDoc(doc, false)

				if err != nil {
					err = errors.Wrapf(
						err,
						"comparer thread failed to handle destination doc with ID %v",
						doc.Lookup("_id"),
					)
				}
			}
		}
	}

	if err != nil {
		return nil, 0, 0, errors.Wrap(err, "comparer thread failed")
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

	return results, docCount, byteCount, nil
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
			verifier.srcBuildInfo,
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
			verifier.dstBuildInfo,
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
