package verifier

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/internal/reportutils"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"
	"golang.org/x/exp/slices"
)

const readTimeout = 10 * time.Minute

type docWithTs struct {
	doc bson.Raw
	ts  primitive.Timestamp
}

func (verifier *Verifier) FetchAndCompareDocuments(
	givenCtx context.Context,
	workerNum int,
	task *VerificationTask,
) (
	[]VerificationResult,
	types.DocumentCount,
	types.ByteCount,
	error,
) {
	var srcChannel, dstChannel <-chan docWithTs
	var readSrcCallback, readDstCallback func(context.Context, *retry.FuncInfo) error

	results := []VerificationResult{}
	var docCount types.DocumentCount
	var byteCount types.ByteCount

	retryer := retry.New().WithDescription(
		"comparing task %v's documents (namespace: %s)",
		task.PrimaryKey,
		task.QueryFilter.Namespace,
	)

	err := retryer.
		WithBefore(func() {
			srcChannel, dstChannel, readSrcCallback, readDstCallback = verifier.getFetcherChannelsAndCallbacks(task)
		}).
		WithErrorCodes(util.CursorKilledErrCode).
		WithCallback(
			func(ctx context.Context, fi *retry.FuncInfo) error {
				return readSrcCallback(ctx, fi)
			},
			"reading from source",
		).
		WithCallback(
			func(ctx context.Context, fi *retry.FuncInfo) error {
				return readDstCallback(ctx, fi)
			},
			"reading from destination",
		).
		WithCallback(
			func(ctx context.Context, fi *retry.FuncInfo) error {
				var err error
				results, docCount, byteCount, err = verifier.compareDocsFromChannels(
					ctx,
					workerNum,
					fi,
					task,
					srcChannel,
					dstChannel,
				)

				return err
			},
			"comparing documents",
		).Run(givenCtx, verifier.logger)

	return results, docCount, byteCount, err
}

func (verifier *Verifier) compareDocsFromChannels(
	ctx context.Context,
	workerNum int,
	fi *retry.FuncInfo,
	task *VerificationTask,
	srcChannel, dstChannel <-chan docWithTs,
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

	srcCache := map[string]docWithTs{}
	dstCache := map[string]docWithTs{}

	// This is the core document-handling logic. It either:
	//
	// a) caches the new document if its mapKey is unseen, or
	// b) compares the new doc against its previously-received, cached
	//    counterpart and records any mismatch.
	handleNewDoc := func(curDocWithTs docWithTs, isSrc bool) error {
		mapKey := getMapKey(curDocWithTs.doc, mapKeyFieldNames)

		var ourMap, theirMap map[string]docWithTs

		if isSrc {
			ourMap = srcCache
			theirMap = dstCache
		} else {
			ourMap = dstCache
			theirMap = srcCache
		}
		// See if we've already cached a document with this
		// mapKey from the other channel.
		theirDocWithTs, exists := theirMap[mapKey]

		// If there is no such cached document, then cache the newly-received
		// document in our map then proceed to the next document.
		//
		// (We'll remove the cache entry when/if the other channel yields a
		// document with the same mapKey.)
		if !exists {
			ourMap[mapKey] = curDocWithTs
			return nil
		}

		// We have two documents! First we remove the cache entry. This saves
		// memory, but more importantly, it lets us know, once we exhaust the
		// channels, which documents were missing on one side or the other.
		delete(theirMap, mapKey)

		// Now we determine which document came from whom.
		var srcDoc, dstDoc docWithTs
		if isSrc {
			srcDoc = curDocWithTs
			dstDoc = theirDocWithTs
		} else {
			srcDoc = theirDocWithTs
			dstDoc = curDocWithTs
		}

		// Finally we compare the documents and save any mismatch report(s).
		mismatches, err := verifier.compareOneDocument(srcDoc.doc, dstDoc.doc, namespace)
		if err != nil {
			return errors.Wrap(err, "failed to compare documents")
		}

		for i := range mismatches {
			mismatches[i].SrcTimestamp = option.Some(srcDoc.ts)
			mismatches[i].DstTimestamp = option.Some(dstDoc.ts)
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

		var srcDocWithTs, dstDocWithTs docWithTs

		eg, egCtx := contextplus.ErrGroup(ctx)

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
				case srcDocWithTs, alive = <-srcChannel:
					if !alive {
						srcClosed = true
						break
					}

					fi.NoteSuccess("received document from source")

					srcDocCount++
					srcByteCount += types.ByteCount(len(srcDocWithTs.doc))
					verifier.workerTracker.SetDetail(
						workerNum,
						fmt.Sprintf(
							"%s documents (%s)",
							reportutils.FmtReal(srcDocCount),
							reportutils.FmtBytes(srcByteCount),
						),
					)
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
				case dstDocWithTs, alive = <-dstChannel:
					if !alive {
						dstClosed = true
						break
					}

					fi.NoteSuccess("received document from destination")
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

		if srcDocWithTs.doc != nil {
			err := handleNewDoc(srcDocWithTs, true)

			if err != nil {
				return nil, 0, 0, errors.Wrapf(
					err,
					"comparer thread failed to handle %#q's source doc (task: %s) with ID %v",
					namespace,
					task.PrimaryKey,
					srcDocWithTs.doc.Lookup("_id"),
				)
			}
		}

		if dstDocWithTs.doc != nil {
			err := handleNewDoc(dstDocWithTs, false)

			if err != nil {
				return nil, 0, 0, errors.Wrapf(
					err,
					"comparer thread failed to handle %#q's destination doc (task: %s) with ID %v",
					namespace,
					task.PrimaryKey,
					dstDocWithTs.doc.Lookup("_id"),
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

	for _, docWithTs := range srcCache {
		results = append(
			results,
			VerificationResult{
				ID:           docWithTs.doc.Lookup("_id"),
				Details:      Missing,
				Cluster:      ClusterTarget,
				NameSpace:    namespace,
				dataSize:     len(docWithTs.doc),
				SrcTimestamp: option.Some(docWithTs.ts),
			},
		)
	}

	for _, docWithTs := range dstCache {
		results = append(
			results,
			VerificationResult{
				ID:           docWithTs.doc.Lookup("_id"),
				Details:      Missing,
				Cluster:      ClusterSource,
				NameSpace:    namespace,
				dataSize:     len(docWithTs.doc),
				DstTimestamp: option.Some(docWithTs.ts),
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

func (verifier *Verifier) getFetcherChannelsAndCallbacks(
	task *VerificationTask,
) (
	<-chan docWithTs,
	<-chan docWithTs,
	func(context.Context, *retry.FuncInfo) error,
	func(context.Context, *retry.FuncInfo) error,
) {
	srcChannel := make(chan docWithTs)
	dstChannel := make(chan docWithTs)

	readSrcCallback := func(ctx context.Context, state *retry.FuncInfo) error {
		sess, err := verifier.srcClient.StartSession()
		if err != nil {
			return errors.Wrapf(err, "starting session")
		}

		sctx := mongo.NewSessionContext(ctx, sess)

		cursor, err := verifier.getDocumentsCursor(
			sctx,
			verifier.srcClientCollection(task),
			verifier.srcClusterInfo,
			verifier.srcChangeStreamReader.startAtTs,
			task,
		)

		if err == nil {
			state.NoteSuccess("opened src find cursor")

			err = errors.Wrap(
				iterateCursorToChannel(sctx, state, cursor, srcChannel),
				"failed to read source documents",
			)
		} else {
			err = errors.Wrap(
				err,
				"failed to find source documents",
			)
		}

		return err
	}

	readDstCallback := func(ctx context.Context, state *retry.FuncInfo) error {
		sess, err := verifier.srcClient.StartSession()
		if err != nil {
			return errors.Wrapf(err, "starting session")
		}

		sctx := mongo.NewSessionContext(ctx, sess)

		cursor, err := verifier.getDocumentsCursor(
			sctx,
			verifier.dstClientCollection(task),
			verifier.dstClusterInfo,
			verifier.dstChangeStreamReader.startAtTs,
			task,
		)

		if err == nil {
			state.NoteSuccess("opened dst find cursor")

			err = errors.Wrap(
				iterateCursorToChannel(sctx, state, cursor, dstChannel),
				"failed to read destination documents",
			)
		} else {
			err = errors.Wrap(
				err,
				"failed to find destination documents",
			)
		}

		return err
	}

	return srcChannel, dstChannel, readSrcCallback, readDstCallback
}

func iterateCursorToChannel(
	sctx mongo.SessionContext,
	state *retry.FuncInfo,
	cursor *mongo.Cursor,
	writer chan<- docWithTs,
) error {
	defer close(writer)

	for cursor.Next(sctx) {
		state.NoteSuccess("received a document")

		clusterTime, err := util.GetClusterTimeFromSession(sctx)
		if err != nil {
			return errors.Wrap(err, "reading cluster time from session")
		}

		err = chanutil.WriteWithDoneCheck(
			sctx,
			writer,
			docWithTs{
				doc: slices.Clone(cursor.Current),
				ts:  clusterTime,
			},
		)

		if err != nil {
			return errors.Wrapf(err, "sending document to compare thread")
		}
	}

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

func (verifier *Verifier) getDocumentsCursor(ctx mongo.SessionContext, collection *mongo.Collection, clusterInfo *util.ClusterInfo,
	startAtTs *primitive.Timestamp, task *VerificationTask) (*mongo.Cursor, error) {
	var findOptions bson.D
	runCommandOptions := options.RunCmd()
	var andPredicates bson.A

	if task.IsRecheck() {
		andPredicates = append(andPredicates, bson.D{{"_id", bson.M{"$in": task.Ids}}})
		andPredicates = verifier.maybeAppendGlobalFilterToPredicates(andPredicates)
		findOptions = bson.D{
			bson.E{"filter", bson.D{{"$and", andPredicates}}},
		}
	} else {
		findOptions = task.QueryFilter.Partition.GetFindOptions(clusterInfo, verifier.maybeAppendGlobalFilterToPredicates(andPredicates))
	}
	if verifier.readPreference.Mode() != readpref.PrimaryMode {
		runCommandOptions = runCommandOptions.SetReadPreference(verifier.readPreference)
		if startAtTs != nil {

			// We never want to read before the change stream start time,
			// or for the last generation, the change stream end time.
			findOptions = append(
				findOptions,
				bson.E{"readConcern", bson.D{
					{"afterClusterTime", *startAtTs},
				}},
			)
		}
	}
	findCmd := append(bson.D{{"find", collection.Name()}}, findOptions...)

	// Suppress this log for recheck tasks because the list of IDs can be
	// quite long.
	if !task.IsRecheck() {
		verifier.logger.Debug().
			Any("task", task.PrimaryKey).
			Str("findCmd", fmt.Sprintf("%s", findCmd)).
			Str("options", fmt.Sprintf("%v", *runCommandOptions)).
			Msg("getDocuments findCmd.")
	}

	return collection.Database().RunCommandCursor(ctx, findCmd, runCommandOptions)
}
