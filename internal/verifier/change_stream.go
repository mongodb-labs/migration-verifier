package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/history"
	"github.com/10gen/migration-verifier/internal/keystring"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/mbson"
	"github.com/10gen/migration-verifier/mslices"
	"github.com/10gen/migration-verifier/msync"
	"github.com/10gen/migration-verifier/option"
	mapset "github.com/deckarep/golang-set/v2"
	clone "github.com/huandu/go-clone/generic"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/samber/lo"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"golang.org/x/exp/slices"
)

type ddlEventHandling string

const (
	fauxDocSizeForDeleteEvents = 1024

	// The number of batches we’ll hold in memory at once.
	batchChanBufferSize = 100

	onDDLEventAllow ddlEventHandling = "allow"
)

var supportedEventOpTypes = mapset.NewSet(
	"insert",
	"update",
	"replace",
	"delete",
)

const (
	minChangeStreamPersistInterval     = time.Second * 10
	maxChangeStreamAwaitTime           = time.Second
	metadataChangeStreamCollectionName = "changeStream"
)

type UnknownEventError struct {
	Event bson.Raw
}

func (uee UnknownEventError) Error() string {
	return fmt.Sprintf("received event with unknown optype: %+v", uee.Event)
}

type changeEventBatch struct {
	events      []ParsedEvent
	clusterTime bson.Timestamp
}

type ChangeStreamReader struct {
	readerType whichCluster

	lastChangeEventTime *bson.Timestamp
	logger              *logger.Logger
	namespaces          []string

	metaDB        *mongo.Database
	watcherClient *mongo.Client
	clusterInfo   util.ClusterInfo

	changeStreamRunning  bool
	changeEventBatchChan chan changeEventBatch
	writesOffTs          *util.Eventual[bson.Timestamp]
	readerError          *util.Eventual[error]
	handlerError         *util.Eventual[error]
	doneChan             chan struct{}

	startAtTs *bson.Timestamp

	lag              *msync.TypedAtomic[option.Option[time.Duration]]
	batchSizeHistory *history.History[int]

	onDDLEvent ddlEventHandling
}

func (verifier *Verifier) initializeChangeStreamReaders() {
	srcReader := &ChangeStreamReader{
		readerType:    src,
		namespaces:    verifier.srcNamespaces,
		watcherClient: verifier.srcClient,
		clusterInfo:   *verifier.srcClusterInfo,
	}
	verifier.srcChangeStreamReader = srcReader

	dstReader := &ChangeStreamReader{
		readerType:    dst,
		namespaces:    verifier.dstNamespaces,
		watcherClient: verifier.dstClient,
		clusterInfo:   *verifier.dstClusterInfo,
		onDDLEvent:    onDDLEventAllow,
	}
	verifier.dstChangeStreamReader = dstReader

	// Common elements in both readers:
	for _, csr := range mslices.Of(srcReader, dstReader) {
		csr.logger = verifier.logger
		csr.metaDB = verifier.metaClient.Database(verifier.metaDBName)
		csr.changeEventBatchChan = make(chan changeEventBatch, batchChanBufferSize)
		csr.writesOffTs = util.NewEventual[bson.Timestamp]()
		csr.readerError = util.NewEventual[error]()
		csr.handlerError = util.NewEventual[error]()
		csr.doneChan = make(chan struct{})
		csr.lag = msync.NewTypedAtomic(option.None[time.Duration]())
		csr.batchSizeHistory = history.New[int](time.Minute)
	}
}

// RunChangeEventHandler handles change event batches from the reader.
// It needs to be started after the reader starts and should run in its own
// goroutine.
func (verifier *Verifier) RunChangeEventHandler(ctx context.Context, reader *ChangeStreamReader) error {
	var err error

HandlerLoop:
	for err == nil {
		select {
		case <-ctx.Done():
			err = util.WrapCtxErrWithCause(ctx)

			verifier.logger.Debug().
				Err(err).
				Stringer("changeStreamReader", reader).
				Msg("Change event handler failed.")
		case batch, more := <-reader.changeEventBatchChan:
			if !more {
				verifier.logger.Debug().
					Stringer("changeStreamReader", reader).
					Msg("Change event batch channel has been closed.")

				break HandlerLoop
			}

			verifier.logger.Trace().
				Stringer("changeStreamReader", reader).
				Int("batchSize", len(batch.events)).
				Any("batch", batch).
				Msg("Handling change event batch.")

			err = errors.Wrap(
				verifier.HandleChangeStreamEvents(ctx, batch, reader.readerType),
				"failed to handle change stream events",
			)
		}
	}

	// This will prevent the reader from hanging because the reader checks
	// this along with checks for context expiry.
	if err != nil {
		reader.handlerError.Set(err)
	}

	return err
}

// HandleChangeStreamEvents performs the necessary work for change stream events after receiving a batch.
func (verifier *Verifier) HandleChangeStreamEvents(ctx context.Context, batch changeEventBatch, eventOrigin whichCluster) error {
	if len(batch.events) == 0 {
		return nil
	}

	dbNames := make([]string, len(batch.events))
	collNames := make([]string, len(batch.events))
	docIDs := make([]bson.RawValue, len(batch.events))
	dataSizes := make([]int, len(batch.events))

	latestTimestamp := bson.Timestamp{}

	for i, changeEvent := range batch.events {
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

		var eventRecorder EventRecorder

		// Recheck Docs are keyed by source namespaces.
		// We need to retrieve the source namespaces if change events are from the destination.
		switch eventOrigin {
		case dst:
			eventRecorder = *verifier.dstEventRecorder

			if verifier.nsMap.Len() == 0 {
				// Namespace is not remapped. Source namespace is the same as the destination.
				srcDBName = changeEvent.Ns.DB
				srcCollName = changeEvent.Ns.Coll
			} else {
				dstNs := fmt.Sprintf("%s.%s", changeEvent.Ns.DB, changeEvent.Ns.Coll)
				srcNs, exist := verifier.nsMap.GetSrcNamespace(dstNs)
				if !exist {
					return errors.Errorf("no source namespace corresponding to the destination namepsace %s", dstNs)
				}
				srcDBName, srcCollName = SplitNamespace(srcNs)
			}
		case src:
			eventRecorder = *verifier.srcEventRecorder

			srcDBName = changeEvent.Ns.DB
			srcCollName = changeEvent.Ns.Coll
		default:
			panic(fmt.Sprintf("unknown event origin: %s", eventOrigin))
		}

		dbNames[i] = srcDBName
		collNames[i] = srcCollName
		docIDs[i] = changeEvent.DocID

		if changeEvent.FullDocLen.OrZero() > 0 {
			dataSizes[i] = int(changeEvent.FullDocLen.OrZero())
		} else if changeEvent.FullDocument == nil {
			// This happens for deletes and for some updates.
			// The document is probably, but not necessarily, deleted.
			dataSizes[i] = fauxDocSizeForDeleteEvents
		} else {
			// This happens for inserts, replaces, and most updates.
			dataSizes[i] = len(changeEvent.FullDocument)
		}

		if err := eventRecorder.AddEvent(&changeEvent); err != nil {
			return errors.Wrapf(
				err,
				"failed to augment stats with %s change event (%+v)",
				eventOrigin,
				changeEvent,
			)
		}
	}

	latestTimestampTime := time.Unix(int64(latestTimestamp.T), 0)
	lag := time.Unix(int64(batch.clusterTime.T), 0).Sub(latestTimestampTime)

	verifier.logger.Trace().
		Str("origin", string(eventOrigin)).
		Int("count", len(docIDs)).
		Any("latestTimestamp", latestTimestamp).
		Time("latestTimestampTime", latestTimestampTime).
		Stringer("lag", lag).
		Msg("Persisting rechecks for change events.")

	return verifier.insertRecheckDocs(
		ctx,
		lo.Ternary(eventOrigin == src, recheckCauseSource, recheckCauseDestination),
		dbNames, collNames, docIDs, dataSizes,
	)
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
					slices.Clone(MongosyncMetaDBPrefixes),
					csr.metaDB.Name(),
				),
			)}},
		}
	} else {
		filter := []bson.D{}
		for _, ns := range csr.namespaces {
			db, coll := SplitNamespace(ns)
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

	if csr.hasBsonSize() {
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

func (csr *ChangeStreamReader) hasBsonSize() bool {
	major := csr.clusterInfo.VersionArray[0]

	if major == 4 {
		return csr.clusterInfo.VersionArray[1] >= 4
	}

	return major > 4
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
	ctx context.Context,
	ri *retry.FuncInfo,
	cs *mongo.ChangeStream,
	sess *mongo.Session,
) error {
	eventsRead := 0
	var changeEvents []ParsedEvent

	latestEvent := option.None[ParsedEvent]()

	var batchTotalBytes int
	for hasEventInBatch := true; hasEventInBatch; hasEventInBatch = cs.RemainingBatchLength() > 0 {
		gotEvent := cs.TryNext(ctx)

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

		if err := cs.Decode(&changeEvents[eventsRead]); err != nil {
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
				csr.logger.Info().
					Stringer("changeStream", csr).
					Stringer("event", cs.Current).
					Msg("Ignoring event with unrecognized type on destination. (It’s assumedly internal to the migration.)")

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

		if changeEvents[eventsRead].ClusterTime != nil &&
			(csr.lastChangeEventTime == nil ||
				csr.lastChangeEventTime.Before(*changeEvents[eventsRead].ClusterTime)) {

			csr.lastChangeEventTime = changeEvents[eventsRead].ClusterTime
			latestEvent = option.Some(changeEvents[eventsRead])
		}

		eventsRead++
	}

	var tokenTs bson.Timestamp
	tokenTs, err := extractTimestampFromResumeToken(cs.ResumeToken())
	if err == nil {
		lagSecs := int64(sess.OperationTime().T) - int64(tokenTs.T)
		csr.lag.Store(option.Some(time.Second * time.Duration(lagSecs)))
	} else {
		csr.logger.Warn().
			Err(err).
			Msgf("Failed to extract timestamp from %s's resume token to compute change stream lag.", csr)
	}

	if eventsRead == 0 {
		ri.NoteSuccess("received an empty change stream response")

		return nil
	}

	csr.batchSizeHistory.Add(eventsRead)

	if event, has := latestEvent.Get(); has {
		csr.logger.Trace().
			Stringer("changeStreamReader", csr).
			Any("event", event).
			Msg("Updated lastChangeEventTime.")
	}

	ri.NoteSuccess("parsed %d-event batch", len(changeEvents))

	select {
	case <-ctx.Done():
		return util.WrapCtxErrWithCause(ctx)
	case <-csr.handlerError.Ready():
		return csr.wrapHandlerErrorForReader()
	case csr.changeEventBatchChan <- changeEventBatch{
		events: changeEvents,

		// NB: We know by now that OperationTime is non-nil.
		clusterTime: *sess.OperationTime(),
	}:
	}

	ri.NoteSuccess("sent %d-event batch to handler", len(changeEvents))

	return nil
}

func (csr *ChangeStreamReader) wrapHandlerErrorForReader() error {
	return errors.Wrap(
		csr.handlerError.Get(),
		"event handler failed, so no more events can be processed",
	)
}

func (csr *ChangeStreamReader) iterateChangeStream(
	ctx context.Context,
	ri *retry.FuncInfo,
	cs *mongo.ChangeStream,
	sess *mongo.Session,
) error {
	var lastPersistedTime time.Time

	persistResumeTokenIfNeeded := func() error {
		if time.Since(lastPersistedTime) <= minChangeStreamPersistInterval {
			return nil
		}

		err := csr.persistChangeStreamResumeToken(ctx, cs)
		if err == nil {
			lastPersistedTime = time.Now()
		}

		return err
	}

	for {
		var err error
		var gotwritesOffTimestamp bool

		select {

		// If the context is canceled, return immmediately.
		case <-ctx.Done():
			err := util.WrapCtxErrWithCause(ctx)

			csr.logger.Debug().
				Err(err).
				Stringer("changeStreamReader", csr).
				Msg("Stopping iteration.")

			return err

		case <-csr.handlerError.Ready():
			return csr.wrapHandlerErrorForReader()

		// If the ChangeStreamEnderChan has a message, the user has indicated that
		// source writes are ended and the migration tool is finished / committed.
		// This means we should exit rather than continue reading the change stream
		// since there should be no more events.
		case <-csr.writesOffTs.Ready():
			writesOffTs := csr.writesOffTs.Get()

			csr.logger.Debug().
				Any("writesOffTimestamp", writesOffTs).
				Msgf("%s received writesOff timestamp. Finalizing change stream.", csr)

			gotwritesOffTimestamp = true

			// Read change events until the stream reaches the writesOffTs.
			// (i.e., the `getMore` call returns empty)
			for {
				var curTs bson.Timestamp
				curTs, err = extractTimestampFromResumeToken(cs.ResumeToken())
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

					break
				}

				err = csr.readAndHandleOneChangeEventBatch(ctx, ri, cs, sess)

				if err != nil {
					return err
				}
			}

		default:
			err = csr.readAndHandleOneChangeEventBatch(ctx, ri, cs, sess)

			if err == nil {
				err = persistResumeTokenIfNeeded()
			}

			if err != nil {
				return err
			}
		}

		if gotwritesOffTimestamp {
			csr.changeStreamRunning = false
			if csr.lastChangeEventTime != nil {
				csr.startAtTs = csr.lastChangeEventTime
			}
			// since we have started Recheck, we must signal that we have
			// finished the change stream changes so that Recheck can continue.
			close(csr.doneChan)
			break
		}
	}

	infoLog := csr.logger.Info()
	if csr.lastChangeEventTime == nil {
		infoLog = infoLog.Str("lastEventTime", "none")
	} else {
		infoLog = infoLog.Any("lastEventTime", *csr.lastChangeEventTime)
	}

	infoLog.
		Stringer("reader", csr).
		Msg("Change stream reader is done.")

	return nil
}

func (csr *ChangeStreamReader) createChangeStream(
	ctx context.Context,
) (*mongo.ChangeStream, *mongo.Session, bson.Timestamp, error) {
	pipeline := csr.GetChangeStreamFilter()
	opts := options.ChangeStream().
		SetMaxAwaitTime(maxChangeStreamAwaitTime)

	if csr.clusterInfo.VersionArray[0] >= 6 {
		opts = opts.SetCustomPipeline(
			bson.M{
				"showSystemEvents":   true,
				"showExpandedEvents": true,
			},
		)
	}

	savedResumeToken, err := csr.loadChangeStreamResumeToken(ctx)
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to load persisted change stream resume token")
	}

	csStartLogEvent := csr.logger.Info()

	if savedResumeToken != nil {
		logEvent := csStartLogEvent.
			Stringer(csr.resumeTokenDocID(), savedResumeToken)

		ts, err := extractTimestampFromResumeToken(savedResumeToken)
		if err == nil {
			logEvent = addTimestampToLogEvent(ts, logEvent)
		} else {
			csr.logger.Warn().
				Err(err).
				Msg("Failed to extract timestamp from persisted resume token.")
		}

		logEvent.Msg("Starting change stream from persisted resume token.")

		opts = opts.SetStartAfter(savedResumeToken)
	} else {
		csStartLogEvent.Msgf("Starting change stream from current %s cluster time.", csr.readerType)
	}

	sess, err := csr.watcherClient.StartSession()
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to start session")
	}
	sctx := mongo.NewSessionContext(ctx, sess)
	changeStream, err := csr.watcherClient.Watch(sctx, pipeline, opts)
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to open change stream")
	}

	err = csr.persistChangeStreamResumeToken(ctx, changeStream)
	if err != nil {
		return nil, nil, bson.Timestamp{}, err
	}

	startTs, err := extractTimestampFromResumeToken(changeStream.ResumeToken())
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
	}

	// With sharded clusters the resume token might lead the cluster time
	// by 1 increment. In that case we need the actual cluster time;
	// otherwise we will get errors.
	clusterTime, err := util.GetClusterTimeFromSession(sess)
	if err != nil {
		return nil, nil, bson.Timestamp{}, errors.Wrap(err, "failed to read cluster time from session")
	}

	csr.logger.Debug().
		Any("resumeTokenTimestamp", startTs).
		Any("clusterTime", clusterTime).
		Stringer("changeStreamReader", csr).
		Msg("Using earlier time as start timestamp.")

	if startTs.After(clusterTime) {
		startTs = clusterTime
	}

	return changeStream, sess, startTs, nil
}

// StartChangeStream starts the change stream.
func (csr *ChangeStreamReader) StartChangeStream(ctx context.Context) error {
	// This channel holds the first change stream creation's result, whether
	// success or failure. Rather than using a Result we could make separate
	// Timestamp and error channels, but the single channel is cleaner since
	// there's no chance of "nonsense" like both channels returning a payload.
	initialCreateResultChan := make(chan mo.Result[bson.Timestamp])

	go func() {
		// Closing changeEventBatchChan at the end of change stream goroutine
		// notifies the verifier's change event handler to exit.
		defer func() {
			csr.logger.Debug().
				Stringer("changeStreamReader", csr).
				Msg("Closing change event batch channel.")

			close(csr.changeEventBatchChan)
		}()

		retryer := retry.New().WithErrorCodes(util.CursorKilledErrCode)

		parentThreadWaiting := true

		err := retryer.WithCallback(
			func(ctx context.Context, ri *retry.FuncInfo) error {
				changeStream, sess, startTs, err := csr.createChangeStream(ctx)
				if err != nil {
					logEvent := csr.logger.Debug().
						Err(err).
						Stringer("changeStreamReader", csr)

					if parentThreadWaiting {
						logEvent.Msg("First change stream open failed.")

						initialCreateResultChan <- mo.Err[bson.Timestamp](err)
						return nil
					}

					logEvent.Msg("Retried change stream open failed.")

					return err
				}

				defer changeStream.Close(ctx)

				logEvent := csr.logger.Debug().
					Stringer("changeStreamReader", csr).
					Any("startTimestamp", startTs)

				if parentThreadWaiting {
					logEvent.Msg("First change stream open succeeded.")

					initialCreateResultChan <- mo.Ok(startTs)
					close(initialCreateResultChan)
					parentThreadWaiting = false
				} else {
					logEvent.Msg("Retried change stream open succeeded.")
				}

				return csr.iterateChangeStream(ctx, ri, changeStream, sess)
			},
			"running %s", csr,
		).Run(ctx, csr.logger)

		if err != nil {
			csr.readerError.Set(err)
		}
	}()

	result := <-initialCreateResultChan

	startTs, err := result.Get()
	if err != nil {
		return err
	}

	csr.startAtTs = &startTs

	csr.changeStreamRunning = true

	return nil
}

// GetLag returns the observed change stream lag (i.e., the delta between
// cluster time and the most-recently-seen change event).
func (csr *ChangeStreamReader) GetLag() option.Option[time.Duration] {
	return csr.lag.Load()
}

// GetSaturation returns the reader’s internal buffer’s saturation level as
// a fraction. If saturation rises, that means we’re reading events faster than
// we can persist them.
func (csr *ChangeStreamReader) GetSaturation() float64 {
	return util.DivideToF64(len(csr.changeEventBatchChan), cap(csr.changeEventBatchChan))
}

// GetEventsPerSecond returns the number of change events per second we’ve been
// seeing “recently”. (See implementation for the actual period over which we
// compile this metric.)
func (csr *ChangeStreamReader) GetEventsPerSecond() option.Option[float64] {
	logs := csr.batchSizeHistory.Get()
	lastLog, hasLogs := lo.Last(logs)

	if hasLogs && lastLog.At != logs[0].At {
		span := lastLog.At.Sub(logs[0].At)

		// Each log contains a time and a # of events that happened since
		// the prior log. Thus, each log’s Datum is a count of events that
		// happened before the timestamp. Since we want the # of events that
		// happened between the first & last times, we only want events *after*
		// the first time. Thus, we skip the first log entry here.
		totalEvents := 0
		for _, log := range logs[1:] {
			totalEvents += log.Datum
		}

		return option.Some(util.DivideToF64(totalEvents, span.Seconds()))
	}

	return option.None[float64]()
}

func addTimestampToLogEvent(ts bson.Timestamp, event *zerolog.Event) *zerolog.Event {
	return event.
		Any("timestamp", ts).
		Time("time", time.Unix(int64(ts.T), int64(0)))
}

func (csr *ChangeStreamReader) getChangeStreamMetadataCollection() *mongo.Collection {
	return csr.metaDB.Collection(metadataChangeStreamCollectionName)
}

func (csr *ChangeStreamReader) loadChangeStreamResumeToken(ctx context.Context) (bson.Raw, error) {
	coll := csr.getChangeStreamMetadataCollection()

	token, err := coll.FindOne(
		ctx,
		bson.D{{"_id", csr.resumeTokenDocID()}},
	).Raw()

	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}

	return token, err
}

func (csr *ChangeStreamReader) String() string {
	return fmt.Sprintf("%s change stream reader", csr.readerType)
}

func (csr *ChangeStreamReader) resumeTokenDocID() string {
	switch csr.readerType {
	case src:
		return "srcResumeToken"
	case dst:
		return "dstResumeToken"
	default:
		panic("unknown readerType: " + csr.readerType)
	}
}

func (csr *ChangeStreamReader) persistChangeStreamResumeToken(ctx context.Context, cs *mongo.ChangeStream) error {
	token := cs.ResumeToken()

	coll := csr.getChangeStreamMetadataCollection()
	_, err := coll.ReplaceOne(
		ctx,
		bson.D{{"_id", csr.resumeTokenDocID()}},
		token,
		options.Replace().SetUpsert(true),
	)

	if err == nil {
		ts, err := extractTimestampFromResumeToken(token)

		logEvent := csr.logger.Debug()

		if err == nil {
			logEvent = addTimestampToLogEvent(ts, logEvent)
		} else {
			csr.logger.Warn().Err(err).
				Msg("failed to extract resume token timestamp")
		}

		logEvent.Msgf("Persisted %s's resume token.", csr)

		return nil
	}

	return errors.Wrapf(err, "failed to persist change stream resume token (%v)", token)
}

func extractTimestampFromResumeToken(resumeToken bson.Raw) (bson.Timestamp, error) {
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
