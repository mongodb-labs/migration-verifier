package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/keystring"
	"github.com/10gen/migration-verifier/internal/logger"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const fauxDocSizeForDeleteEvents = 1024

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
	metadataChangeStreamCollectionName = "changeStream"
)

type UnknownEventError struct {
	Event *ParsedEvent
}

func (uee UnknownEventError) Error() string {
	return fmt.Sprintf("received event with unknown optype: %+v", uee.Event)
}

type ChangeStreamReader struct {
	readerType clusterType

	lastChangeEventTime *primitive.Timestamp
	logger              *logger.Logger
	namespaces          []string
	metaDBName          string

	metaClient    *mongo.Client
	watcherClient *mongo.Client

	changeStreamRunning         bool
	ChangeEventBatchChan        chan []ParsedEvent
	ChangeStreamWritesOffTsChan chan primitive.Timestamp
	ChangeStreamErrChan         chan error
	ChangeStreamDoneChan        chan struct{}

	startAtTs *primitive.Timestamp
}

func (verifier *Verifier) initializeChangeStreamReaders() {
	verifier.srcChangeStreamReader = &ChangeStreamReader{
		readerType:                  srcReaderType,
		lastChangeEventTime:         nil,
		logger:                      verifier.logger,
		namespaces:                  verifier.srcNamespaces,
		metaDBName:                  verifier.metaDBName,
		metaClient:                  verifier.metaClient,
		watcherClient:               verifier.srcClient,
		changeStreamRunning:         false,
		ChangeEventBatchChan:        make(chan []ParsedEvent),
		ChangeStreamWritesOffTsChan: make(chan primitive.Timestamp),
		ChangeStreamErrChan:         make(chan error),
		ChangeStreamDoneChan:        make(chan struct{}),
		startAtTs:                   nil,
	}
	verifier.dstChangeStreamReader = &ChangeStreamReader{
		readerType:                  dstReaderType,
		lastChangeEventTime:         nil,
		logger:                      verifier.logger,
		namespaces:                  verifier.dstNamespaces,
		metaDBName:                  verifier.metaDBName,
		metaClient:                  verifier.metaClient,
		watcherClient:               verifier.dstClient,
		changeStreamRunning:         false,
		ChangeEventBatchChan:        make(chan []ParsedEvent),
		ChangeStreamWritesOffTsChan: make(chan primitive.Timestamp),
		ChangeStreamErrChan:         make(chan error),
		ChangeStreamDoneChan:        make(chan struct{}),
		startAtTs:                   nil,
	}
}

// StartChangeEventHandler starts a goroutine that handles change event batches from the reader.
func (verifier *Verifier) StartChangeEventHandler(ctx context.Context, reader *ChangeStreamReader) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case batch := <-reader.ChangeEventBatchChan:
				err := verifier.HandleChangeStreamEvents(ctx, batch, reader.readerType)
				if err != nil {
					reader.ChangeStreamErrChan <- err
				}
			}
		}
	}()
}

// HandleChangeStreamEvents performs the necessary work for change stream events after receiving a batch.
func (verifier *Verifier) HandleChangeStreamEvents(ctx context.Context, batch []ParsedEvent, eventOrigin clusterType) error {
	if len(batch) == 0 {
		return nil
	}

	dbNames := make([]string, len(batch))
	collNames := make([]string, len(batch))
	docIDs := make([]interface{}, len(batch))
	dataSizes := make([]int, len(batch))

	for i, changeEvent := range batch {
		switch changeEvent.OpType {
		case "delete":
			fallthrough
		case "insert":
			fallthrough
		case "replace":
			fallthrough
		case "update":
			if err := verifier.eventRecorder.AddEvent(&changeEvent); err != nil {
				return errors.Wrapf(err, "failed to augment stats with change event (%+v)", changeEvent)
			}

			var srcDBName, srcCollName string

			// Recheck Docs are keyed by source namespaces.
			// We need to retrieve the source namespaces if change events are from the destination.
			switch eventOrigin {
			case dstReaderType:
				if len(verifier.dstSrcNsMap) == 0 {
					srcDBName = changeEvent.Ns.DB
					srcCollName = changeEvent.Ns.Coll
				} else {
					dstNs := fmt.Sprintf("%s.%s", changeEvent.Ns.DB, changeEvent.Ns.Coll)
					srcNs, exist := verifier.dstSrcNsMap[dstNs]
					if !exist {
						return errors.Errorf("no source namespace corresponding to the destination namepsace %s", dstNs)
					}
					srcDBName, srcCollName = SplitNamespace(srcNs)
				}
			case srcReaderType:
				srcDBName = changeEvent.Ns.DB
				srcCollName = changeEvent.Ns.Coll
			default:
				return errors.Errorf("unknown event origin: %s", eventOrigin)
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
		default:
			return UnknownEventError{Event: &changeEvent}
		}
	}

	verifier.logger.Debug().
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
func (csr *ChangeStreamReader) GetChangeStreamFilter() []bson.D {
	if len(csr.namespaces) == 0 {
		return []bson.D{{bson.E{"$match", bson.D{{"ns.db", bson.D{{"$ne", csr.metaDBName}}}}}}}
	}
	filter := bson.A{}
	for _, ns := range csr.namespaces {
		db, coll := SplitNamespace(ns)
		filter = append(filter, bson.D{{"ns", bson.D{{"db", db}, {"coll", coll}}}})
	}
	stage := bson.D{{"$match", bson.D{{"$or", filter}}}}
	return []bson.D{stage}
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
	cs *mongo.ChangeStream,
) error {
	eventsRead := 0
	var changeEventBatch []ParsedEvent

	for hasEventInBatch := true; hasEventInBatch; hasEventInBatch = cs.RemainingBatchLength() > 0 {
		gotEvent := cs.TryNext(ctx)

		if cs.Err() != nil {
			return errors.Wrap(cs.Err(), "change stream iteration failed")
		}

		if !gotEvent {
			break
		}

		if changeEventBatch == nil {
			changeEventBatch = make([]ParsedEvent, cs.RemainingBatchLength()+1)
		}

		if err := cs.Decode(&changeEventBatch[eventsRead]); err != nil {
			return errors.Wrapf(err, "failed to decode change event to %T", changeEventBatch[eventsRead])
		}

		if changeEventBatch[eventsRead].ClusterTime != nil &&
			(csr.lastChangeEventTime == nil ||
				csr.lastChangeEventTime.Compare(*changeEventBatch[eventsRead].ClusterTime) < 0) {
			csr.lastChangeEventTime = changeEventBatch[eventsRead].ClusterTime
		}

		eventsRead++
	}

	if eventsRead == 0 {
		return nil
	}

	csr.ChangeEventBatchChan <- changeEventBatch
	return nil
}

func (csr *ChangeStreamReader) iterateChangeStream(ctx context.Context, cs *mongo.ChangeStream) {
	defer cs.Close(ctx)

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
			csr.logger.Debug().
				Err(ctx.Err()).
				Msg("Change stream quitting.")
			return

		// If the ChangeStreamEnderChan has a message, the user has indicated that
		// source writes are ended. This means we should exit rather than continue
		// reading the change stream since there should be no more events.
		case writesOffTs := <-csr.ChangeStreamWritesOffTsChan:
			csr.logger.Debug().
				Interface("writesOffTimestamp", writesOffTs).
				Msg("Change stream thread received writesOff timestamp. Finalizing change stream.")

			gotwritesOffTimestamp = true

			// Read all change events until the source reports no events.
			// (i.e., the `getMore` call returns empty)
			for {
				var curTs primitive.Timestamp
				curTs, err = extractTimestampFromResumeToken(cs.ResumeToken())
				if err != nil {
					err = errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
					break
				}

				if curTs.After(writesOffTs) {
					csr.logger.Debug().
						Interface("currentTimestamp", curTs).
						Interface("writesOffTimestamp", writesOffTs).
						Msg("Change stream has reached the writesOff timestamp. Shutting down.")

					break
				}

				err = csr.readAndHandleOneChangeEventBatch(ctx, cs)

				if err != nil {
					break
				}
			}

		default:
			err = csr.readAndHandleOneChangeEventBatch(ctx, cs)

			if err == nil {
				err = persistResumeTokenIfNeeded()
			}
		}

		if err != nil && !errors.Is(err, context.Canceled) {
			csr.logger.Debug().
				Err(err).
				Msg("Sending change stream error.")

			csr.ChangeStreamErrChan <- err

			if !gotwritesOffTimestamp {
				break
			}
		}

		if gotwritesOffTimestamp {
			csr.changeStreamRunning = false
			if csr.lastChangeEventTime != nil {
				csr.startAtTs = csr.lastChangeEventTime
			}
			// since we have started Recheck, we must signal that we have
			// finished the change stream changes so that Recheck can continue.
			csr.ChangeStreamDoneChan <- struct{}{}
			break
		}
	}

	infoLog := csr.logger.Info()
	if csr.lastChangeEventTime == nil {
		infoLog = infoLog.Str("lastEventTime", "none")
	} else {
		infoLog = infoLog.Interface("lastEventTime", *csr.lastChangeEventTime)
	}

	infoLog.Msg("Change stream is done.")
}

// StartChangeStream starts the change stream.
func (csr *ChangeStreamReader) StartChangeStream(ctx context.Context) error {
	pipeline := csr.GetChangeStreamFilter()

	opts := options.ChangeStream().
		SetMaxAwaitTime(1 * time.Second).
		SetFullDocument(options.UpdateLookup)

	savedResumeToken, err := csr.loadChangeStreamResumeToken(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to load persisted change stream resume token")
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
		return errors.Wrap(err, "failed to start session")
	}
	sctx := mongo.NewSessionContext(ctx, sess)
	changeStream, err := csr.watcherClient.Watch(sctx, pipeline, opts)
	if err != nil {
		return errors.Wrap(err, "failed to open change stream")
	}

	err = csr.persistChangeStreamResumeToken(ctx, changeStream)
	if err != nil {
		return err
	}

	csTimestamp, err := extractTimestampFromResumeToken(changeStream.ResumeToken())
	if err != nil {
		return errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
	}

	clusterTime, err := getClusterTimeFromSession(sess)
	if err != nil {
		return errors.Wrap(err, "failed to read cluster time from session")
	}

	csr.startAtTs = &csTimestamp
	if csTimestamp.After(clusterTime) {
		csr.startAtTs = &clusterTime
	}

	csr.changeStreamRunning = true

	go csr.iterateChangeStream(ctx, changeStream)

	return nil
}

func addTimestampToLogEvent(ts primitive.Timestamp, event *zerolog.Event) *zerolog.Event {
	return event.
		Interface("timestamp", ts).
		Time("time", time.Unix(int64(ts.T), int64(0)))
}

func (csr *ChangeStreamReader) getChangeStreamMetadataCollection() *mongo.Collection {
	return csr.metaClient.Database(csr.metaDBName).Collection(metadataChangeStreamCollectionName)
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
	case srcReaderType:
		return "srcResumeToken"
	case dstReaderType:
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

		logEvent.Msg("Persisted change stream resume token.")

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

func getClusterTimeFromSession(sess mongo.Session) (primitive.Timestamp, error) {
	ctStruct := struct {
		ClusterTime struct {
			ClusterTime primitive.Timestamp `bson:"clusterTime"`
		} `bson:"$clusterTime"`
	}{}

	clusterTimeRaw := sess.ClusterTime()
	err := bson.Unmarshal(sess.ClusterTime(), &ctStruct)
	if err != nil {
		return primitive.Timestamp{}, errors.Wrapf(err, "failed to find clusterTime in session cluster time document (%v)", clusterTimeRaw)
	}

	return ctStruct.ClusterTime.ClusterTime, nil
}
