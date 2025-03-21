package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/keystring"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/10gen/migration-verifier/internal/retry"
	"github.com/10gen/migration-verifier/internal/util"
	"github.com/10gen/migration-verifier/msync"
	"github.com/10gen/migration-verifier/option"
	mapset "github.com/deckarep/golang-set/v2"
	clone "github.com/huandu/go-clone/generic"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/samber/mo"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type modifyEventHandling string

const (
	fauxDocSizeForDeleteEvents = 1024

	onModifyEventIgnore modifyEventHandling = "ignore"
	modifyEventType                         = "modify"
)

var supportedEventOpTypes = mapset.NewSet(
	"insert",
	"update",
	"replace",
	"delete",
)

// ParsedEvent contains the fields of an event that we have parsed from 'bson.Raw'.
type ParsedEvent struct {
	ID           interface{}          `bson:"_id"`
	OpType       string               `bson:"operationType"`
	Ns           *Namespace           `bson:"ns,omitempty"`
	DocKey       DocKey               `bson:"documentKey,omitempty"`
	FullDocument bson.Raw             `bson:"fullDocument,omitempty"`
	ClusterTime  *primitive.Timestamp `bson:"clusterTime,omitEmpty"`
}

func (pe *ParsedEvent) String() string {
	return fmt.Sprintf("{OpType: %s, namespace: %s, docID: %v, clusterTime: %v}", pe.OpType, pe.Ns, pe.DocKey.ID, pe.ClusterTime)
}

// DocKey is a deserialized form for the ChangeEvent documentKey field. We currently only care about
// the _id.
type DocKey struct {
	ID interface{} `bson:"_id"`
}

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

type ChangeStreamReader struct {
	readerType whichCluster

	lastChangeEventTime *primitive.Timestamp
	logger              *logger.Logger
	namespaces          []string

	metaDB        *mongo.Database
	watcherClient *mongo.Client
	clusterInfo   util.ClusterInfo

	changeStreamRunning  bool
	changeEventBatchChan chan []ParsedEvent
	writesOffTs          *util.Eventual[primitive.Timestamp]
	readerError          *util.Eventual[error]
	handlerError         *util.Eventual[error]
	doneChan             chan struct{}

	startAtTs *primitive.Timestamp

	lag *msync.TypedAtomic[option.Option[time.Duration]]

	onModifyEvent modifyEventHandling
}

func (verifier *Verifier) initializeChangeStreamReaders() {
	verifier.srcChangeStreamReader = &ChangeStreamReader{
		readerType:           src,
		logger:               verifier.logger,
		namespaces:           verifier.srcNamespaces,
		metaDB:               verifier.metaClient.Database(verifier.metaDBName),
		watcherClient:        verifier.srcClient,
		clusterInfo:          *verifier.srcClusterInfo,
		changeStreamRunning:  false,
		changeEventBatchChan: make(chan []ParsedEvent),
		writesOffTs:          util.NewEventual[primitive.Timestamp](),
		readerError:          util.NewEventual[error](),
		handlerError:         util.NewEventual[error](),
		doneChan:             make(chan struct{}),
		lag:                  msync.NewTypedAtomic(option.None[time.Duration]()),
	}
	verifier.dstChangeStreamReader = &ChangeStreamReader{
		readerType:           dst,
		logger:               verifier.logger,
		namespaces:           verifier.dstNamespaces,
		metaDB:               verifier.metaClient.Database(verifier.metaDBName),
		watcherClient:        verifier.dstClient,
		clusterInfo:          *verifier.dstClusterInfo,
		changeStreamRunning:  false,
		changeEventBatchChan: make(chan []ParsedEvent),
		writesOffTs:          util.NewEventual[primitive.Timestamp](),
		readerError:          util.NewEventual[error](),
		handlerError:         util.NewEventual[error](),
		doneChan:             make(chan struct{}),
		lag:                  msync.NewTypedAtomic(option.None[time.Duration]()),
		onModifyEvent:        onModifyEventIgnore,
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
				Int("batchSize", len(batch)).
				Interface("batch", batch).
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
func (verifier *Verifier) HandleChangeStreamEvents(ctx context.Context, batch []ParsedEvent, eventOrigin whichCluster) error {
	if len(batch) == 0 {
		return nil
	}

	dbNames := make([]string, len(batch))
	collNames := make([]string, len(batch))
	docIDs := make([]interface{}, len(batch))
	dataSizes := make([]int, len(batch))

	for i, changeEvent := range batch {
		if !supportedEventOpTypes.Contains(changeEvent.OpType) {
			panic(fmt.Sprintf("Unsupported optype in event; should have failed already! event=%+v", changeEvent))
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
		docIDs[i] = changeEvent.DocKey.ID

		if changeEvent.FullDocument == nil {
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

	verifier.logger.Debug().
		Str("origin", string(eventOrigin)).
		Int("count", len(docIDs)).
		Msg("Persisting rechecks for change events.")

	return verifier.insertRecheckDocs(ctx, dbNames, collNames, docIDs, dataSizes)
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
			{{"$match", bson.D{
				{"ns.db", bson.D{{"$nin", bson.A{
					primitive.Regex{Pattern: MongosyncMetaDBsPattern},
					csr.metaDB.Name(),
				}}}},
			}}},
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

	return append(
		pipeline,
		bson.D{
			{"$unset", []string{
				"updateDescription",
			}},
		},
	)
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
	sess mongo.Session,
) error {
	eventsRead := 0
	var changeEventBatch []ParsedEvent

	latestEvent := option.None[ParsedEvent]()

	for hasEventInBatch := true; hasEventInBatch; hasEventInBatch = cs.RemainingBatchLength() > 0 {
		gotEvent := cs.TryNext(ctx)

		if cs.Err() != nil {
			return errors.Wrap(cs.Err(), "change stream iteration failed")
		}

		if !gotEvent {
			break
		}

		if changeEventBatch == nil {
			batchSize := cs.RemainingBatchLength() + 1

			ri.NoteSuccess("received a batch of %d change event(s)", batchSize)

			changeEventBatch = make([]ParsedEvent, batchSize)
		}

		if err := cs.Decode(&changeEventBatch[eventsRead]); err != nil {
			return errors.Wrapf(err, "failed to decode change event to %T", changeEventBatch[eventsRead])
		}

		// This only logs in tests.
		csr.logger.Trace().
			Stringer("changeStream", csr).
			Interface("event", changeEventBatch[eventsRead]).
			Int("eventsPreviouslyReadInBatch", eventsRead).
			Int("batchSize", len(changeEventBatch)).
			Msg("Received a change event.")

		opType := changeEventBatch[eventsRead].OpType
		if !supportedEventOpTypes.Contains(opType) {

			// We expect modify events on the destination as part of finalizing
			// a migration. For example, mongosync enables indexes’ uniqueness
			// constraints and sets capped collection sizes.
			if opType == modifyEventType && csr.onModifyEvent == onModifyEventIgnore {
				csr.logger.Info().
					Stringer("changeStream", csr).
					Interface("event", cs.Current).
					Msg("This event is probably internal to the migration. Ignoring.")

				// Discard this event, then keep reading.
				changeEventBatch = changeEventBatch[:len(changeEventBatch)-1]

				continue
			} else {
				return UnknownEventError{Event: clone.Clone(cs.Current)}
			}
		}

		// This shouldn’t happen, but just in case:
		if changeEventBatch[eventsRead].Ns == nil {
			return errors.Errorf("Change event lacks a namespace: %+v", changeEventBatch[eventsRead])
		}

		if changeEventBatch[eventsRead].ClusterTime != nil &&
			(csr.lastChangeEventTime == nil ||
				csr.lastChangeEventTime.Before(*changeEventBatch[eventsRead].ClusterTime)) {

			csr.lastChangeEventTime = changeEventBatch[eventsRead].ClusterTime
			latestEvent = option.Some(changeEventBatch[eventsRead])
		}

		eventsRead++
	}

	var tokenTs primitive.Timestamp
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

	if event, has := latestEvent.Get(); has {
		csr.logger.Trace().
			Stringer("changeStreamReader", csr).
			Interface("event", event).
			Msg("Updated lastChangeEventTime.")
	}

	ri.NoteSuccess("parsed %d-event batch", len(changeEventBatch))

	select {
	case <-ctx.Done():
		return util.WrapCtxErrWithCause(ctx)
	case <-csr.handlerError.Ready():
		return csr.wrapHandlerErrorForReader()
	case csr.changeEventBatchChan <- changeEventBatch:
	}

	ri.NoteSuccess("sent %d-event batch to handler", len(changeEventBatch))

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
	sess mongo.Session,
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
				Interface("writesOffTimestamp", writesOffTs).
				Msgf("%s thread received writesOff timestamp. Finalizing change stream.", csr)

			gotwritesOffTimestamp = true

			// Read change events until the stream reaches the writesOffTs.
			// (i.e., the `getMore` call returns empty)
			for {
				var curTs primitive.Timestamp
				curTs, err = extractTimestampFromResumeToken(cs.ResumeToken())
				if err != nil {
					return errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
				}

				// writesOffTs never refers to a real event,
				// so we can stop once curTs >= writesOffTs.
				if !curTs.Before(writesOffTs) {
					csr.logger.Debug().
						Interface("currentTimestamp", curTs).
						Interface("writesOffTimestamp", writesOffTs).
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
		infoLog = infoLog.Interface("lastEventTime", *csr.lastChangeEventTime)
	}

	infoLog.
		Stringer("reader", csr).
		Msg("Change stream reader is done.")

	return nil
}

func (csr *ChangeStreamReader) createChangeStream(
	ctx context.Context,
) (*mongo.ChangeStream, mongo.Session, primitive.Timestamp, error) {
	pipeline := csr.GetChangeStreamFilter()
	opts := options.ChangeStream().
		SetMaxAwaitTime(maxChangeStreamAwaitTime).
		SetFullDocument(options.UpdateLookup)

	if csr.clusterInfo.VersionArray[0] >= 6 {
		opts = opts.SetCustomPipeline(bson.M{"showExpandedEvents": true})
	}

	savedResumeToken, err := csr.loadChangeStreamResumeToken(ctx)
	if err != nil {
		return nil, nil, primitive.Timestamp{}, errors.Wrap(err, "failed to load persisted change stream resume token")
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
		return nil, nil, primitive.Timestamp{}, errors.Wrap(err, "failed to start session")
	}
	sctx := mongo.NewSessionContext(ctx, sess)
	changeStream, err := csr.watcherClient.Watch(sctx, pipeline, opts)
	if err != nil {
		return nil, nil, primitive.Timestamp{}, errors.Wrap(err, "failed to open change stream")
	}

	err = csr.persistChangeStreamResumeToken(ctx, changeStream)
	if err != nil {
		return nil, nil, primitive.Timestamp{}, err
	}

	startTs, err := extractTimestampFromResumeToken(changeStream.ResumeToken())
	if err != nil {
		return nil, nil, primitive.Timestamp{}, errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
	}

	// With sharded clusters the resume token might lead the cluster time
	// by 1 increment. In that case we need the actual cluster time;
	// otherwise we will get errors.
	clusterTime, err := util.GetClusterTimeFromSession(sess)
	if err != nil {
		return nil, nil, primitive.Timestamp{}, errors.Wrap(err, "failed to read cluster time from session")
	}

	csr.logger.Debug().
		Interface("resumeTokenTimestamp", startTs).
		Interface("clusterTime", clusterTime).
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
	initialCreateResultChan := make(chan mo.Result[primitive.Timestamp])

	go func() {
		// Closing changeEventBatchChan at the end of change stream goroutine
		// notifies the verifier's change event handler to exit.
		defer func() {
			csr.logger.Debug().
				Stringer("changeStreamReader", csr).
				Msg("Closing change event batch channel.")

			close(csr.changeEventBatchChan)
		}()

		retryer := retry.New().WithErrorCodes(util.CursorKilled)

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

						initialCreateResultChan <- mo.Err[primitive.Timestamp](err)
						return nil
					}

					logEvent.Msg("Retried change stream open failed.")

					return err
				}

				defer changeStream.Close(ctx)

				logEvent := csr.logger.Debug().
					Stringer("changeStreamReader", csr).
					Interface("startTimestamp", startTs)

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

func (csr *ChangeStreamReader) GetLag() option.Option[time.Duration] {
	return csr.lag.Load()
}

func addTimestampToLogEvent(ts primitive.Timestamp, event *zerolog.Event) *zerolog.Event {
	return event.
		Interface("timestamp", ts).
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

func extractTimestampFromResumeToken(resumeToken bson.Raw) (primitive.Timestamp, error) {
	tokenStruct := struct {
		Data string `bson:"_data"`
	}{}

	// Change stream token is always a V1 keystring in the _data field
	err := bson.Unmarshal(resumeToken, &tokenStruct)
	if err != nil {
		return primitive.Timestamp{}, errors.Wrapf(err, "failed to extract %#q from resume token (%v)", "_data", resumeToken)
	}

	resumeTokenBson, err := keystring.KeystringToBson(keystring.V1, tokenStruct.Data)
	if err != nil {
		return primitive.Timestamp{}, err
	}
	// First element is the cluster time we want
	resumeTokenTime, ok := resumeTokenBson[0].Value.(primitive.Timestamp)
	if !ok {
		return primitive.Timestamp{}, errors.Errorf("resume token data's (%+v) first element is of type %T, not a timestamp", resumeTokenBson, resumeTokenBson[0].Value)
	}

	return resumeTokenTime, nil
}
