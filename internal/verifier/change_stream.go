package verifier

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/10gen/migration-verifier/internal/keystring"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/internal/verifier/namespaces"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mmongo"
	"github.com/10gen/migration-verifier/option"
	mapset "github.com/deckarep/golang-set/v2"
	clone "github.com/huandu/go-clone/generic"
	"github.com/pkg/errors"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

var supportedEventOpTypes = mapset.NewSet(
	"insert",
	"update",
	"replace",
	"delete",
)

const (
	maxChangeStreamAwaitTime = time.Second

	ChangeReaderOptChangeStream = "changeStream"
)

type UnknownEventError struct {
	Event bson.Raw
}

func (uee UnknownEventError) Error() string {
	return fmt.Sprintf("received event with unknown optype: %+v", uee.Event)
}

type ChangeStreamReader struct {
	changeStream *mongo.ChangeStream

	// cosmosStreams is populated when reading from a CosmosDB source.
	// CosmosDB change streams are per-collection only, so we hold one stream
	// per namespace and round-robin through them on iteration.
	cosmosStreams    []*mongo.ChangeStream
	cosmosNamespaces []string

	ChangeReaderCommon
}

var _ changeReader = &ChangeStreamReader{}

func (csr *ChangeStreamReader) isCosmos() bool {
	return csr.clusterInfo.IsCosmosDB()
}

func (v *Verifier) newChangeStreamReader(
	namespaces []string,
	cluster whichCluster,
	client *mongo.Client,
	clusterInfo util.ClusterInfo,
) *ChangeStreamReader {
	common := newChangeReaderCommon(cluster)
	common.namespaces = namespaces
	common.readerType = cluster
	common.watcherClient = client
	common.clusterInfo = clusterInfo

	common.logger = v.logger
	common.metaDB = v.metaClient.Database(v.metaDBName)

	if clusterInfo.IsCosmosDB() {
		// CosmosDB resume tokens are opaque base64-JSON, not MongoDB keystrings.
		// Synthesize a wall-clock timestamp instead; all timestamp comparisons
		// in the writes-off path then operate consistently in wall-clock space.
		common.resumeTokenTSExtractor = synthesizeWallClockTimestamp
	} else {
		common.resumeTokenTSExtractor = extractTSFromChangeStreamResumeToken
	}

	csr := &ChangeStreamReader{ChangeReaderCommon: common}

	csr.createIteratorCb = csr.createChangeStream
	csr.iterateCb = csr.iterateChangeStream

	return csr
}

// synthesizeWallClockTimestamp produces a bson.Timestamp from current wall
// time. Used as the resume-token timestamp extractor for CosmosDB sources,
// where genuine cluster times aren’t available in events or resume tokens.
func synthesizeWallClockTimestamp(_ bson.Raw) (bson.Timestamp, error) {
	return bson.Timestamp{T: uint32(time.Now().Unix()), I: 0}, nil
}

// GetChangeStreamFilter returns an aggregation pipeline that filters
// namespaces as per configuration.
//
// Note that this omits verifier.globalFilter because we still need to
// recheck any out-filter documents that may have changed in order to
// account for filter traversals (i.e., updates that change whether a
// document matches the filter).
//
// NB: Ideally we could make the change stream give $bsonSize(fullDocument)
// and omit fullDocument, but $bsonSize was new in MongoDB 4.4, and we still
// want to verify migrations from 4.2. fullDocument is unlikely to be a
// bottleneck anyway.
func (csr *ChangeStreamReader) GetChangeStreamFilter() (pipeline mongo.Pipeline) {
	if len(csr.namespaces) == 0 {
		pipeline = mongo.Pipeline{
			{{"$match", util.ExcludePrefixesQuery(
				"ns.db",
				append(
					slices.Clone(namespaces.ExcludedDBPrefixes),
					csr.metaDB.Name(),
				),
			)}},
		}
	} else {
		filter := []bson.D{}
		for _, ns := range csr.namespaces {
			db, coll := mmongo.SplitNamespace(ns)
			filter = append(filter, bson.D{
				{"ns", bson.D{
					{"db", db},
					{"coll", coll},
				}},
			})
		}
		pipeline = mongo.Pipeline{
			{{"$match", bson.D{{"$or", filter}}}},
		}
	}

	pipeline = append(
		pipeline,
		bson.D{
			{"$addFields", bson.D{
				{"_docID", "$documentKey._id"},

				{"updateDescription", "$$REMOVE"},
				{"wallTime", "$$REMOVE"},
				{"documentKey", "$$REMOVE"},
			}},
		},
	)

	if util.ClusterHasBSONSize([2]int(csr.clusterInfo.VersionArray)) {
		pipeline = append(
			pipeline,
			bson.D{
				{"$addFields", bson.D{
					{"_fullDocLen", bson.D{{"$bsonSize", "$fullDocument"}}},
					{"fullDocument", "$$REMOVE"},
				}},
			},
		)
	}

	return pipeline
}

// This function reads a single `getMore` response into a slice.
//
// Note that this doesn’t care about the writesOff timestamp. Thus,
// if writesOff has happened and a `getMore` response’s events straddle
// the writesOff timestamp (i.e., some events precede it & others follow it),
// the verifier will enqueue rechecks from those post-writesOff events. This
// is unideal but shouldn’t impede correctness since post-writesOff events
// shouldn’t really happen anyway by definition.
func (csr *ChangeStreamReader) readAndHandleOneChangeEventBatch(
	sctx context.Context,
	ri retry.SuccessNotifier,
	cs *mongo.ChangeStream,
) error {
	eventsRead := 0
	var changeEvents []ParsedEvent

	latestEvent := option.None[ParsedEvent]()

	var batchTotalBytes int
	for hasEventInBatch := true; hasEventInBatch; hasEventInBatch = cs.RemainingBatchLength() > 0 {
		gotEvent := cs.TryNext(sctx)

		if cs.Err() != nil {
			return errors.Wrap(cs.Err(), "change stream iteration failed")
		}

		if !gotEvent {
			break
		}

		if changeEvents == nil {
			batchSize := cs.RemainingBatchLength() + 1

			ri.NoteSuccess("received a batch of %d change event(s)", batchSize)

			changeEvents = make([]ParsedEvent, batchSize)
		}

		batchTotalBytes += len(cs.Current)

		if err := (&changeEvents[eventsRead]).UnmarshalFromBSON(cs.Current); err != nil {
			return errors.Wrapf(err, "failed to decode change event to %T", changeEvents[eventsRead])
		}

		// This only logs in tests.
		csr.logger.Trace().
			Stringer("changeStream", csr).
			Any("event", changeEvents[eventsRead]).
			Int("eventsPreviouslyReadInBatch", eventsRead).
			Int("batchEvents", len(changeEvents)).
			Int("batchBytes", batchTotalBytes).
			Msg("Received a change event.")

		opType := changeEvents[eventsRead].OpType
		if !supportedEventOpTypes.Contains(opType) {
			// We expect certain DDL events on the destination as part of
			// a migration. For example, mongosync enables indexes’ uniqueness
			// constraints and sets capped collection sizes, and sometimes
			// indexes are created after initial sync.

			if csr.onDDLEvent == onDDLEventAllow {
				csr.logIgnoredDDL(cs.Current)

				// Discard this event, then keep reading.
				changeEvents = changeEvents[:len(changeEvents)-1]

				continue
			} else {
				return UnknownEventError{Event: clone.Clone(cs.Current)}
			}
		}

		// This shouldn’t happen, but just in case:
		if changeEvents[eventsRead].Ns == nil {
			return errors.Errorf("Change event lacks a namespace: %+v", changeEvents[eventsRead])
		}

		eventTime := changeEvents[eventsRead].ClusterTime
		if eventTime != nil && csr.lastChangeEventTime.Load().OrZero().Before(*eventTime) {
			csr.lastChangeEventTime.Store(option.Some(*eventTime))
			latestEvent = option.Some(changeEvents[eventsRead])
		}

		eventsRead++
	}

	sess := mongo.SessionFromContext(sctx)
	csr.updateLastSeenClusterTime(sess)
	csr.updateTimestamps(sess, cs.ResumeToken())

	if event, has := latestEvent.Get(); has {
		csr.logger.Trace().
			Stringer("changeStreamReader", csr).
			Any("event", event).
			Msg("Updated lastChangeEventTime.")
	}

	ri.NoteSuccess("parsed %d-event batch", len(changeEvents))

	// NB: We send even “empty” batches to the persistor thread because
	// even with 0 events there is still a new resume token.
	select {
	case <-sctx.Done():
		return util.WrapCtxErrWithCause(sctx)
	case csr.eventBatchChan <- eventBatch{
		events:      changeEvents,
		resumeToken: cs.ResumeToken(),
	}:
	}

	ri.NoteSuccess("sent %d-event batch to handler", len(changeEvents))

	return nil
}

func (csr *ChangeStreamReader) iterateChangeStream(
	ctx context.Context,
	ri retry.SuccessNotifier,
	sess *mongo.Session,
) error {
	if csr.isCosmos() {
		return csr.iterateCosmosChangeStreams(ctx, ri)
	}

	sctx := mongo.NewSessionContext(ctx, sess)

	cs := csr.changeStream

	defer cs.Close(sctx)

changeStreamLoop:
	for {
		var err error

		select {

		// If the context is canceled, return immmediately.
		case <-ctx.Done():
			err := util.WrapCtxErrWithCause(ctx)

			csr.logger.Debug().
				Err(err).
				Stringer("changeStreamReader", csr).
				Msg("Stopping iteration.")

			return err

		// If the ChangeStreamEnderChan has a message, the user has indicated that
		// source writes are ended and the migration tool is finished / committed.
		// This means we should exit rather than continue reading the change stream
		// since there should be no more events.
		case <-csr.writesOffTS.Ready():
			writesOffTs := csr.writesOffTS.Get()

			csr.logger.Debug().
				Any("writesOffTimestamp", writesOffTs).
				Msgf("%s received writesOff timestamp. Finalizing change stream.", csr)

			// Read change events until the stream reaches the writesOffTs.
			// (i.e., the `getMore` call returns empty)
			for {
				var curTs bson.Timestamp
				curTs, err = csr.resumeTokenTSExtractor(cs.ResumeToken())
				if err != nil {
					return errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
				}

				// writesOffTs never refers to a real event,
				// so we can stop once curTs >= writesOffTs.
				if !curTs.Before(writesOffTs) {
					csr.logger.Debug().
						Any("currentTimestamp", curTs).
						Any("writesOffTimestamp", writesOffTs).
						Msgf("%s has reached the writesOff timestamp. Shutting down.", csr)

					csr.running = false
					csr.startAtTS = &curTs

					break changeStreamLoop
				}

				err = csr.readAndHandleOneChangeEventBatch(sctx, ri, cs)
				if err != nil {
					return errors.Wrap(err, "finishing change stream after writes-off")
				}
			}

		default:
			err = csr.readAndHandleOneChangeEventBatch(sctx, ri, cs)
			if err != nil {
				return err
			}
		}
	}

	infoLog := csr.logger.Info()
	if ts, has := csr.lastChangeEventTime.Load().Get(); has {
		infoLog = infoLog.Any("lastEventTime", ts)
	} else {
		infoLog = infoLog.Str("lastEventTime", "none")
	}

	infoLog.
		Stringer("reader", csr).
		Msg("Change stream reader is done.")

	return nil
}

func (csr *ChangeStreamReader) createChangeStream(
	ctx context.Context,
	sess *mongo.Session,
) (bson.Timestamp, error) {
	if csr.isCosmos() {
		return csr.createCosmosChangeStreams(ctx)
	}

	pipeline := csr.GetChangeStreamFilter()
	opts := options.ChangeStream().
		SetMaxAwaitTime(maxChangeStreamAwaitTime)

	// showSystemEvents/showExpandedEvents are MongoDB 6.0+ features the
	// CosmosDB Mongo API does not implement; setting them causes the change
	// stream to error.
	if csr.clusterInfo.VersionArray[0] >= 6 && !csr.clusterInfo.IsCosmosDB() {
		opts = opts.SetCustomPipeline(
			bson.M{
				"showSystemEvents":   true,
				"showExpandedEvents": true,
			},
		)
	}

	savedResumeToken, err := csr.loadResumeToken(ctx)
	if err != nil {
		return bson.Timestamp{}, errors.Wrap(err, "failed to load persisted change stream resume token")
	}

	csStartLogEvent := csr.logger.Info()

	resumetoken, hasSavedToken := savedResumeToken.Get()

	if hasSavedToken {
		logEvent := csStartLogEvent.
			Stringer(resumeTokenDocID(csr.readerType), resumetoken)

		ts, err := csr.resumeTokenTSExtractor(resumetoken)
		if err == nil {
			logEvent = addTimestampToLogEvent(ts, logEvent)
		} else {
			csr.logger.Warn().
				Err(err).
				Msg("Failed to extract timestamp from persisted resume token.")
		}

		logEvent.Msg("Starting change stream from persisted resume token.")

		if util.ClusterHasChangeStreamStartAfter([2]int(csr.clusterInfo.VersionArray)) {
			opts = opts.SetStartAfter(resumetoken)
		} else {
			opts = opts.SetResumeAfter(resumetoken)
		}
	} else {
		csStartLogEvent.Msgf("Starting change stream from current %s cluster time.", csr.readerType)
	}

	sctx := mongo.NewSessionContext(ctx, sess)

	changeStream, err := csr.watcherClient.Watch(sctx, pipeline, opts)
	if err != nil {
		return bson.Timestamp{}, errors.Wrap(err, "opening change stream")
	}

	if !hasSavedToken {
		// Usually the change stream’s initial response is empty, but sometimes
		// there are events right away. We can discard those events because
		// they’ve already happened, and our initial scan is yet to come.
		if len(changeStream.ResumeToken()) == 0 {
			_, _, err := mmongo.GetBatch(ctx, changeStream, nil, nil)
			if err != nil {
				return bson.Timestamp{}, errors.Wrap(err, "discarding change stream’s initial events")
			}
		}

		err = csr.persistResumeToken(ctx, changeStream.ResumeToken())
		if err != nil {
			return bson.Timestamp{}, errors.Wrapf(err, "persisting initial resume token")
		}
	}

	startTs, err := csr.resumeTokenTSExtractor(changeStream.ResumeToken())
	if err != nil {
		changeStream.Close(sctx)
		return bson.Timestamp{}, errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
	}

	// With sharded clusters the resume token might lead the cluster time
	// by 1 increment. In that case we need the actual cluster time;
	// otherwise we will get errors.
	clusterTime, err := util.GetClusterTimeFromSession(sess)
	if err != nil {
		changeStream.Close(sctx)
		return bson.Timestamp{}, errors.Wrap(err, "failed to read cluster time from session")
	}

	if startTs.After(clusterTime) {
		csr.logger.Debug().
			Any("resumeTokenTimestamp", startTs).
			Any("clusterTime", clusterTime).
			Stringer("changeStreamReader", csr).
			Msg("Cluster time predates resume token; using it as start timestamp.")

		startTs = clusterTime
	} else {
		csr.logger.Debug().
			Any("resumeTokenTimestamp", startTs).
			Stringer("changeStreamReader", csr).
			Msg("Got start timestamp from change stream.")
	}

	csr.changeStream = changeStream

	return startTs, nil
}

func (csr *ChangeStreamReader) String() string {
	return fmt.Sprintf("%s change stream reader", csr.readerType)
}

// CosmosDB change stream support.
//
// Constraints discovered empirically against Azure CosmosDB's Mongo API (≥ v4.2):
//   - Change streams are per-collection only — cluster-wide and DB-wide
//     Watch() calls fail with `Expected type string but found int`.
//   - `fullDocument: "updateLookup"` is required.
//   - The pipeline must be exactly:
//       1. $match (with an operationType filter that constrains to the set
//          {insert, update, replace} — deletes aren't emitted anyway)
//       2. $project (whitelist of _id, fullDocument, ns, documentKey)
//       3. (optional) further stages, including $addFields
//   - $bsonSize is NOT a supported aggregation operator.
//   - The change event omits operationType (filtered by $project) and
//     clusterTime. We synthesize both client-side after decoding so the
//     downstream persistor doesn't have to know about CosmosDB.
//   - Resume tokens are opaque base64-JSON (not MongoDB keystrings), so the
//     timestamp extractor is swapped for a wall-clock synthesizer at
//     reader-construction time (see newChangeStreamReader).

const cosmosResumeTokenDocPrefix = "cosmosResumeToken:"

func cosmosResumeTokenDocID(clusterType whichCluster, namespace string) string {
	return cosmosResumeTokenDocPrefix + string(clusterType) + ":" + namespace
}

// getCosmosChangeStreamPipeline returns the pipeline CosmosDB requires.
// CosmosDB rejects every other shape we've tested.
func getCosmosChangeStreamPipeline() mongo.Pipeline {
	return mongo.Pipeline{
		{{"$match", bson.D{
			{"operationType", bson.D{{"$in", bson.A{"insert", "update", "replace"}}}},
		}}},
		{{"$project", bson.D{
			{"_id", 1},
			{"fullDocument", 1},
			{"ns", 1},
			{"documentKey", 1},
		}}},
		// operationType is dropped by the $project (CosmosDB only allows the
		// four whitelisted fields). Re-inject a placeholder so the verifier's
		// existing event handler doesn't reject events as DDL. We use
		// "insert" because for recheck purposes all three op types are
		// treated identically (re-fetch the _id from both clusters).
		{{"$addFields", bson.D{
			{"_docID", "$documentKey._id"},
			{"operationType", "insert"},
		}}},
	}
}

func (csr *ChangeStreamReader) createCosmosChangeStreams(ctx context.Context) (bson.Timestamp, error) {
	if len(csr.namespaces) == 0 {
		return bson.Timestamp{}, errors.New("CosmosDB source requires explicit namespaces — cluster-wide change streams aren't supported. Set --srcNamespace/--dstNamespace, or ensure --verifyAll has populated the list.")
	}

	pipeline := getCosmosChangeStreamPipeline()
	csr.cosmosStreams = make([]*mongo.ChangeStream, 0, len(csr.namespaces))
	csr.cosmosNamespaces = make([]string, 0, len(csr.namespaces))

	for _, ns := range csr.namespaces {
		dbName, collName := mmongo.SplitNamespace(ns)
		coll := csr.watcherClient.Database(dbName).Collection(collName)

		opts := options.ChangeStream().
			SetMaxAwaitTime(maxChangeStreamAwaitTime).
			SetFullDocument(options.UpdateLookup)

		// Resume from a previously persisted per-namespace token if available.
		// CosmosDB only supports resumeAfter (not startAfter).
		if savedTokenOpt, err := csr.loadCosmosResumeToken(ctx, ns); err != nil {
			return bson.Timestamp{}, errors.Wrapf(err, "loading persisted resume token for %#q", ns)
		} else if savedToken, has := savedTokenOpt.Get(); has {
			csr.logger.Info().
				Str("namespace", ns).
				Msg("Resuming CosmosDB change stream from persisted token.")
			opts = opts.SetResumeAfter(savedToken)
		} else {
			csr.logger.Info().
				Str("namespace", ns).
				Msg("Starting CosmosDB change stream from current cluster state.")
		}

		stream, err := coll.Watch(ctx, pipeline, opts)
		if err != nil {
			// Common case for src/dst-asymmetric namespaces (e.g. migration
			// metadata collections that exist on dst only). Skip and continue.
			if util.IsNamespaceNotFoundError(err) {
				csr.logger.Warn().
					Str("namespace", ns).
					Msg("Skipping CosmosDB change stream: collection does not exist on source.")
				continue
			}
			return bson.Timestamp{}, errors.Wrapf(err, "opening CosmosDB change stream for %#q", ns)
		}

		// Persist the initial token immediately so we have something to resume
		// from if the verifier restarts before we read any events.
		if token := stream.ResumeToken(); len(token) > 0 {
			if err := csr.persistCosmosResumeToken(ctx, ns, token); err != nil {
				stream.Close(ctx)
				return bson.Timestamp{}, errors.Wrapf(err, "persisting initial resume token for %#q", ns)
			}
		}

		csr.cosmosStreams = append(csr.cosmosStreams, stream)
		csr.cosmosNamespaces = append(csr.cosmosNamespaces, ns)
	}

	// Synthetic start TS in wall-clock space (consistent with the synthetic
	// writes-off TS produced by getSourceWritesOffTimestamp).
	return bson.Timestamp{T: uint32(time.Now().Unix()), I: 0}, nil
}

func (csr *ChangeStreamReader) iterateCosmosChangeStreams(
	ctx context.Context,
	ri retry.SuccessNotifier,
) error {
	defer func() {
		for _, cs := range csr.cosmosStreams {
			cs.Close(ctx)
		}
	}()

	lastTokenPersist := time.Now()

cosmosLoop:
	for {
		select {
		case <-ctx.Done():
			return util.WrapCtxErrWithCause(ctx)

		case <-csr.writesOffTS.Ready():
			// We have no event-level timestamps on CosmosDB; once writesOff
			// is signaled, drain any remaining buffered events on every
			// stream, then declare done. The user is expected to stop writes
			// before calling writesOff.
			writesOffTs := csr.writesOffTS.Get()
			csr.logger.Debug().
				Any("writesOffTimestamp", writesOffTs).
				Msgf("%s received writesOff; draining CosmosDB streams.", csr)

			drainDeadline := time.Now().Add(2 * maxChangeStreamAwaitTime)
			for time.Now().Before(drainDeadline) {
				gotAny := false
				for i, cs := range csr.cosmosStreams {
					hadEvents, err := csr.readCosmosBatch(ctx, ri, cs, csr.cosmosNamespaces[i])
					if err != nil {
						return errors.Wrap(err, "draining CosmosDB stream after writes-off")
					}
					if hadEvents {
						gotAny = true
					}
				}
				if !gotAny {
					break
				}
			}

			csr.running = false
			now := bson.Timestamp{T: uint32(time.Now().Unix()), I: 0}
			csr.startAtTS = &now
			break cosmosLoop

		default:
			gotAny := false
			for i, cs := range csr.cosmosStreams {
				select {
				case <-ctx.Done():
					return util.WrapCtxErrWithCause(ctx)
				default:
				}
				hadEvents, err := csr.readCosmosBatch(ctx, ri, cs, csr.cosmosNamespaces[i])
				if err != nil {
					return err
				}
				if hadEvents {
					gotAny = true
				}
			}

			// Persist resume tokens at most once per resumeTokenPersistInterval.
			// We do this synchronously here (rather than going through the
			// eventBatchChan) because each CosmosDB stream has its own token.
			if time.Since(lastTokenPersist) >= minResumeTokenPersistInterval {
				for i, cs := range csr.cosmosStreams {
					if token := cs.ResumeToken(); len(token) > 0 {
						if err := csr.persistCosmosResumeToken(ctx, csr.cosmosNamespaces[i], token); err != nil {
							csr.logger.Warn().
								Err(err).
								Str("namespace", csr.cosmosNamespaces[i]).
								Msg("Failed to persist CosmosDB resume token.")
						}
					}
				}
				lastTokenPersist = time.Now()
			}

			// If nothing arrived this round, briefly back off so we don't
			// spin-wait. The change-stream TryNext already has a short
			// maxAwaitTime, but with N streams that's N * maxAwait per round.
			if !gotAny {
				select {
				case <-ctx.Done():
					return util.WrapCtxErrWithCause(ctx)
				case <-time.After(100 * time.Millisecond):
				}
			}
		}
	}

	infoLog := csr.logger.Info()
	if ts, has := csr.lastChangeEventTime.Load().Get(); has {
		infoLog = infoLog.Any("lastEventTime", ts)
	} else {
		infoLog = infoLog.Str("lastEventTime", "none")
	}
	infoLog.Stringer("reader", csr).Msg("CosmosDB change stream reader is done.")
	return nil
}

// readCosmosBatch reads a single getMore-style batch from one CosmosDB stream
// and forwards events through the standard event batch channel. Returns true
// if any events were read.
func (csr *ChangeStreamReader) readCosmosBatch(
	ctx context.Context,
	ri retry.SuccessNotifier,
	cs *mongo.ChangeStream,
	namespace string,
) (bool, error) {
	eventsRead := 0
	var changeEvents []ParsedEvent
	var batchTotalBytes int

	for hasEventInBatch := true; hasEventInBatch; hasEventInBatch = cs.RemainingBatchLength() > 0 {
		gotEvent := cs.TryNext(ctx)
		if cs.Err() != nil {
			return false, errors.Wrapf(cs.Err(), "CosmosDB change stream iteration failed (%s)", namespace)
		}
		if !gotEvent {
			break
		}

		if changeEvents == nil {
			batchSize := cs.RemainingBatchLength() + 1
			ri.NoteSuccess("received a batch of %d CosmosDB change event(s) on %s", batchSize, namespace)
			changeEvents = make([]ParsedEvent, batchSize)
		}

		batchTotalBytes += len(cs.Current)

		if err := (&changeEvents[eventsRead]).UnmarshalFromBSON(cs.Current); err != nil {
			return false, errors.Wrapf(err, "decoding CosmosDB change event")
		}

		// CosmosDB events lack operationType (dropped by required $project)
		// and clusterTime. Re-inject synthetic values so the downstream
		// persistor doesn't reject them.
		if changeEvents[eventsRead].OpType == "" {
			changeEvents[eventsRead].OpType = "insert"
		}
		if changeEvents[eventsRead].ClusterTime == nil {
			ts := bson.Timestamp{T: uint32(time.Now().Unix()), I: 0}
			changeEvents[eventsRead].ClusterTime = &ts
		}

		if changeEvents[eventsRead].Ns == nil {
			return false, errors.Errorf("CosmosDB change event lacks ns: %s", cs.Current)
		}

		// Track wall-clock as the "last seen" so writes-off draining can
		// compare timestamps consistently.
		eventTime := changeEvents[eventsRead].ClusterTime
		if csr.lastChangeEventTime.Load().OrZero().Before(*eventTime) {
			csr.lastChangeEventTime.Store(option.Some(*eventTime))
		}

		eventsRead++
	}

	if changeEvents == nil {
		return false, nil
	}
	// Trim in case some events were dropped (none currently are for cosmos).
	changeEvents = changeEvents[:eventsRead]

	select {
	case <-ctx.Done():
		return false, util.WrapCtxErrWithCause(ctx)
	case csr.eventBatchChan <- eventBatch{
		events: changeEvents,
		// For CosmosDB, resume tokens are per-stream and we persist them
		// directly out-of-band. The persistor's resume-token persistence path
		// would only ever see the last stream's token, so we explicitly omit
		// it here.
		resumeToken: nil,
	}:
	}

	ri.NoteSuccess("sent %d-event CosmosDB batch (%s) to handler", len(changeEvents), namespace)
	return true, nil
}

func (csr *ChangeStreamReader) persistCosmosResumeToken(
	ctx context.Context,
	namespace string,
	token bson.Raw,
) error {
	if len(token) == 0 {
		return nil
	}
	docID := cosmosResumeTokenDocID(csr.getWhichCluster(), namespace)
	coll := csr.metaDB.Collection(changeReaderCollectionName)
	_, err := coll.ReplaceOne(
		ctx,
		bson.D{{"_id", docID}},
		bson.D{
			{"_id", docID},
			{"token", token},
			{"namespace", namespace},
			{"cluster", string(csr.getWhichCluster())},
		},
		options.Replace().SetUpsert(true),
	)
	return err
}

func (csr *ChangeStreamReader) loadCosmosResumeToken(
	ctx context.Context,
	namespace string,
) (option.Option[bson.Raw], error) {
	docID := cosmosResumeTokenDocID(csr.getWhichCluster(), namespace)
	raw, err := csr.metaDB.Collection(changeReaderCollectionName).
		FindOne(ctx, bson.D{{"_id", docID}}).Raw()
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return option.None[bson.Raw](), nil
		}
		return option.None[bson.Raw](), err
	}
	tokenRV, lookupErr := raw.LookupErr("token")
	if lookupErr != nil {
		return option.None[bson.Raw](), errors.Wrapf(lookupErr, "decoding stored CosmosDB resume token for %#q", namespace)
	}
	tokenDoc, ok := tokenRV.DocumentOK()
	if !ok {
		return option.None[bson.Raw](), errors.Errorf("stored CosmosDB resume token for %#q is not a document", namespace)
	}
	return option.Some(bson.Raw(tokenDoc)), nil
}

func extractTSFromChangeStreamResumeToken(resumeToken bson.Raw) (bson.Timestamp, error) {
	if len(resumeToken) == 0 {
		panic("internal error: resume token is empty but should never be")
	}

	// Change stream token is always a V1 keystring in the _data field
	tokenDataRV, err := resumeToken.LookupErr("_data")
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "extracting %#q from resume token (%v)", "_data", resumeToken)
	}

	tokenData, err := mbson.CastRawValue[string](tokenDataRV)
	if err != nil {
		return bson.Timestamp{}, errors.Wrapf(err, "parsing resume token (%v)", resumeToken)
	}

	resumeTokenBson, err := keystring.KeystringToBson(keystring.V1, tokenData)
	if err != nil {
		return bson.Timestamp{}, err
	}
	// First element is the cluster time we want
	resumeTokenTime, ok := resumeTokenBson[0].Value.(bson.Timestamp)
	if !ok {
		return bson.Timestamp{}, errors.Errorf("resume token data's (%+v) first element is of type %T, not a timestamp", resumeTokenBson, resumeTokenBson[0].Value)
	}

	return resumeTokenTime, nil
}
