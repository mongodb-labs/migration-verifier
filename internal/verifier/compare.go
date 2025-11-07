package verifier

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/chanutil"
	"github.com/10gen/migration-verifier/contextplus"
	"github.com/10gen/migration-verifier/dockey"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/types"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/option"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"golang.org/x/exp/slices"
)

const (
	readTimeout = 10 * time.Minute

	// When comparing documents via hash, we store the document key as an
	// embedded document. This is the name of the field that stores the
	// document key.
	docKeyInHashedCompare = "k"
)

type docWithTs struct {
	doc bson.Raw
	ts  bson.Timestamp
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

	mapKeyFieldNames := task.QueryFilter.GetDocKeyFields()

	namespace := task.QueryFilter.Namespace

	srcCache := map[string]docWithTs{}
	dstCache := map[string]docWithTs{}

	// This is the core document-handling logic. It either:
	//
	// a) caches the new document if its mapKey is unseen, or
	// b) compares the new doc against its previously-received, cached
	//    counterpart and records any mismatch.
	handleNewDoc := func(curDocWithTs docWithTs, isSrc bool) error {
		docKeyValues, err := getDocKeyValues(
			verifier.docCompareMethod,
			curDocWithTs.doc,
			mapKeyFieldNames,
		)
		if err != nil {
			return errors.Wrapf(err, "extracting doc key (fields: %v) values from doc %+v", mapKeyFieldNames, curDocWithTs.doc)
		}

		mapKey := getMapKey(docKeyValues)

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

					if len(srcDocWithTs.doc) == 0 {
						panic(fmt.Sprintf("got empty src doc! (ts: %v)", srcDocWithTs.ts))
					}

					//fmt.Printf("----- got src doc: %+v %#q\n", srcDocWithTs.doc, string(srcDocWithTs.doc))

					fi.NoteSuccess("received document from source")

					srcDocCount++
					srcByteCount += types.ByteCount(len(srcDocWithTs.doc))
					verifier.workerTracker.SetSrcCounts(
						workerNum,
						srcDocCount,
						srcByteCount,
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
				ID: getDocIdFromComparison(
					verifier.docCompareMethod,
					docWithTs.doc,
				),
				Details:      Missing,
				Cluster:      ClusterTarget,
				NameSpace:    namespace,
				dataSize:     int32(len(docWithTs.doc)),
				SrcTimestamp: option.Some(docWithTs.ts),
			},
		)
	}

	for _, docWithTs := range dstCache {
		results = append(
			results,
			VerificationResult{
				ID: getDocIdFromComparison(
					verifier.docCompareMethod,
					docWithTs.doc,
				),
				Details:      Missing,
				Cluster:      ClusterSource,
				NameSpace:    namespace,
				dataSize:     int32(len(docWithTs.doc)),
				DstTimestamp: option.Some(docWithTs.ts),
			},
		)
	}

	return results, srcDocCount, srcByteCount, nil
}

func getDocIdFromComparison(
	docCompareMethod DocCompareMethod,
	doc bson.Raw,
) bson.RawValue {
	switch docCompareMethod {
	case DocCompareBinary, DocCompareIgnoreOrder:
		return doc.Lookup("_id")
	case DocCompareToHashedIndexKey:
		return doc.Lookup(docKeyInHashedCompare, "_id")
	default:
		panic("bad doc compare method: " + docCompareMethod)
	}
}

func getDocKeyValues(
	docCompareMethod DocCompareMethod,
	doc bson.Raw,
	fieldNames []string,
) ([]bson.RawValue, error) {
	var docKey bson.Raw

	switch docCompareMethod {
	case DocCompareBinary, DocCompareIgnoreOrder:
		// If we have the full document, create the document key manually:
		var err error
		docKey, err = dockey.ExtractTrueDocKeyFromDoc(fieldNames, doc)
		if err != nil {
			return nil, err
		}
	case DocCompareToHashedIndexKey:
		// If we have a hash, then the aggregation should have extracted the
		// document key for us.
		docKeyVal, err := doc.LookupErr(docKeyInHashedCompare)
		if err != nil {
			return nil, errors.Wrapf(err, "fetching %#q from doc %v", docKeyInHashedCompare, doc)
		}

		var isDoc bool
		docKey, isDoc = docKeyVal.DocumentOK()
		if !isDoc {
			return nil, fmt.Errorf(
				"%#q in doc %v is type %s but should be %s",
				docKeyInHashedCompare,
				doc,
				docKeyVal.Type,
				bson.TypeEmbeddedDocument,
			)
		}
	}

	var values []bson.RawValue
	els, err := docKey.Elements()
	if err != nil {
		return nil, errors.Wrapf(err, "parsing doc key (%+v) of doc %+v", docKey, doc)
	}

	for _, el := range els {
		val, err := el.ValueErr()
		if err != nil {
			return nil, errors.Wrapf(err, "parsing doc key element (%+v) of doc %+v", el, doc)
		}

		values = append(values, val)
	}

	return values, nil
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
		// We open a session here so that we can read the session’s cluster
		// time, which we store along with any document mismatches we may see.
		//
		// Ideally the driver would just expose the individual server responses’
		// cluster times, but alas.
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
		sess, err := verifier.dstClient.StartSession()
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
	sctx context.Context,
	state *retry.FuncInfo,
	cursor *mongo.Cursor,
	writer chan<- docWithTs,
) error {
	defer close(writer)

	sess := mongo.SessionFromContext(sctx)

	var batch []bson.Raw
	for {
		//batch = make([]bson.Raw, 0, len(batch))
		batch = batch[:0]
		buffer := make([]byte, 0, 16<<20) // max BSON payload

		batch, _, err := mmongo.GetBatch(sctx, cursor, batch, buffer)
		if err != nil {
			return errors.Wrap(err, "iterating cursor")
		}

		if len(batch) == 0 {
			return nil
		}

		state.NoteSuccess("received a batch of %d document(s)", len(batch))

		//fmt.Printf("----- the batch (%d): %+v\n", len(batch), batch)

		clusterTime, err := util.GetClusterTimeFromSession(sess)
		if err != nil {
			return errors.Wrap(err, "reading cluster time from session")
		}

		for _, doc := range batch {
			payload := docWithTs{
				doc: doc,
				ts:  clusterTime,
			}

			//fmt.Printf("----- sending: %+v\n", payload)

			err = chanutil.WriteWithDoneCheck(
				sctx,
				writer,
				payload,
			)

			if err != nil {
				return errors.Wrapf(err, "sending document to compare thread")
			}
		}
	}

}

func getMapKey(docKeyValues []bson.RawValue) string {
	var keyBuffer bytes.Buffer
	for _, value := range docKeyValues {
		keyBuffer.Grow(1 + len(value.Value))
		keyBuffer.WriteByte(byte(value.Type))
		keyBuffer.Write(value.Value)
	}

	return keyBuffer.String()
}

func (verifier *Verifier) getDocumentsCursor(ctx context.Context, collection *mongo.Collection, clusterInfo *util.ClusterInfo,
	startAtTs *bson.Timestamp, task *VerificationTask) (*mongo.Cursor, error) {
	var findOptions bson.D
	runCommandOptions := options.RunCmd()
	var andPredicates bson.A

	var aggOptions bson.D

	if task.IsRecheck() {
		andPredicates = append(andPredicates, bson.D{{"_id", bson.M{"$in": task.Ids}}})
		andPredicates = verifier.maybeAppendGlobalFilterToPredicates(andPredicates)
		filter := bson.D{{"$and", andPredicates}}

		switch verifier.docCompareMethod.QueryFunction() {
		case DocQueryFunctionFind:
			findOptions = bson.D{
				bson.E{"filter", filter},
			}
		case DocQueryFunctionAggregate:
			aggOptions = bson.D{
				{"pipeline", transformPipelineForToHashedIndexKey(
					mongo.Pipeline{{{"$match", filter}}},
					task,
				)},
			}
		default:
			panic("bad doc compare query func: " + verifier.docCompareMethod.QueryFunction())
		}
	} else {
		pqp, err := task.QueryFilter.Partition.GetQueryParameters(
			clusterInfo,
			verifier.maybeAppendGlobalFilterToPredicates(andPredicates),
		)
		if err != nil {
			return nil, errors.Wrapf(err, "getting query parameters for task: %+v", task)
		}

		switch verifier.docCompareMethod.QueryFunction() {
		case DocQueryFunctionFind:
			findOptions = pqp.ToFindOptions()
		case DocQueryFunctionAggregate:
			aggOptions = pqp.ToAggOptions()

			if verifier.docCompareMethod != DocCompareToHashedIndexKey {
				panic("unknown aggregate compare method: " + verifier.docCompareMethod)
			}

			for i, el := range aggOptions {
				if el.Key != "pipeline" {
					continue
				}

				aggOptions[i].Value = transformPipelineForToHashedIndexKey(
					aggOptions[i].Value.(mongo.Pipeline),
					task,
				)

				break
			}

		default:
			panic("bad doc compare query func: " + verifier.docCompareMethod.QueryFunction())
		}
	}

	var cmd bson.D

	switch verifier.docCompareMethod.QueryFunction() {
	case DocQueryFunctionFind:
		cmd = append(
			bson.D{{"find", collection.Name()}},
			findOptions...,
		)
	case DocQueryFunctionAggregate:
		cmd = append(
			bson.D{
				{"aggregate", collection.Name()},
				{"cursor", bson.D{}},
			},
			aggOptions...,
		)
	}

	if verifier.readPreference.Mode() != readpref.PrimaryMode {
		runCommandOptions = runCommandOptions.SetReadPreference(verifier.readPreference)
		if startAtTs != nil {
			readConcern := bson.D{
				{"afterClusterTime", *startAtTs},
			}

			// We never want to read before the change stream start time,
			// or for the last generation, the change stream end time.
			cmd = append(
				cmd,
				bson.E{"readConcern", readConcern},
			)
		}
	}

	// Suppress this log for recheck tasks because the list of IDs can be
	// quite long.
	if !task.IsRecheck() {
		if verifier.logger.Trace().Enabled() {
			evt := verifier.logger.Trace().
				Any("task", task.PrimaryKey)

			cmdStr, err := bson.MarshalExtJSON(cmd, true, false)
			if err != nil {
				cmdStr = fmt.Appendf(nil, "%s", cmd)
			}

			evt.
				Str("cmd", string(cmdStr)).
				Str("options", fmt.Sprintf("%v", *runCommandOptions)).
				Msg("getDocuments command.")
		}
	}

	return collection.Database().RunCommandCursor(ctx, cmd, runCommandOptions)
}

func transformPipelineForToHashedIndexKey(
	in mongo.Pipeline,
	task *VerificationTask,
) mongo.Pipeline {
	return append(
		slices.Clone(in),
		bson.D{{"$replaceWith", bson.D{
			// Single-letter field names minimize the document size.
			{docKeyInHashedCompare, dockey.ExtractTrueDocKeyAgg(
				task.QueryFilter.GetDocKeyFields(),
				"$$ROOT",
			)},
			{"h", bson.D{
				{"$toHashedIndexKey", bson.D{
					{"$_internalKeyStringValue", bson.D{
						{"input", "$$ROOT"},
					}},
				}},
			}},
			{"s", bson.D{{"$bsonSize", "$$ROOT"}}},
		}}},
	)
}

func (verifier *Verifier) compareOneDocument(srcClientDoc, dstClientDoc bson.Raw, namespace string) ([]VerificationResult, error) {
	match := bytes.Equal(srcClientDoc, dstClientDoc)
	if match {
		return nil, nil
	}

	if verifier.docCompareMethod == DocCompareToHashedIndexKey {
		// With hash comparison, mismatches are opaque.
		return []VerificationResult{{
			ID:        getDocIdFromComparison(verifier.docCompareMethod, srcClientDoc),
			Details:   Mismatch,
			Cluster:   ClusterTarget,
			NameSpace: namespace,
		}}, nil
	}

	mismatch, err := BsonUnorderedCompareRawDocumentWithDetails(srcClientDoc, dstClientDoc)
	if err != nil {
		return nil, err
	}
	if mismatch == nil {
		if verifier.docCompareMethod.ShouldIgnoreFieldOrder() {
			return nil, nil
		}
		dataSize := max(len(srcClientDoc), len(dstClientDoc))

		// If we're respecting field order we have just done a binary compare so we have fields in different order.
		return []VerificationResult{{
			ID:        srcClientDoc.Lookup("_id"),
			Details:   Mismatch + fmt.Sprintf(" : Document %s has fields in different order", srcClientDoc.Lookup("_id")),
			Cluster:   ClusterTarget,
			NameSpace: namespace,
			dataSize:  int32(dataSize),
		}}, nil
	}
	results := mismatchResultsToVerificationResults(mismatch, srcClientDoc, dstClientDoc, namespace, srcClientDoc.Lookup("_id"), "" /* fieldPrefix */)
	return results, nil
}
