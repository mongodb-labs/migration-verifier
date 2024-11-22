package verifier

import (
	"context"
	"fmt"
	"time"

	"github.com/10gen/migration-verifier/internal/keystring"
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

// HandleChangeStreamEvents performs the necessary work for change stream events after receiving a batch.
func (verifier *Verifier) HandleChangeStreamEvents(ctx context.Context, batch []ParsedEvent) error {
	if len(batch) == 0 {
		return nil
	}

	dbNames := make([]string, len(batch))
	collNames := make([]string, len(batch))
	docIDs := make([]interface{}, len(batch))
	dataSizes := make([]int, len(batch))

	for i, changeEvent := range batch {
		if changeEvent.ClusterTime != nil &&
			(verifier.lastChangeEventTime == nil ||
				verifier.lastChangeEventTime.Compare(*changeEvent.ClusterTime) < 0) {
			verifier.lastChangeEventTime = changeEvent.ClusterTime
		}
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
			dbNames[i] = changeEvent.Ns.DB
			collNames[i] = changeEvent.Ns.Coll
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
func (verifier *Verifier) GetChangeStreamFilter() []bson.D {
	if len(verifier.srcNamespaces) == 0 {
		return []bson.D{{bson.E{"$match", bson.D{{"ns.db", bson.D{{"$ne", verifier.metaDBName}}}}}}}
	}
	filter := bson.A{}
	for _, ns := range verifier.srcNamespaces {
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
func (verifier *Verifier) readAndHandleOneChangeEventBatch(
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

		eventsRead++
	}

	if eventsRead == 0 {
		return nil
	}

	err := verifier.HandleChangeStreamEvents(ctx, changeEventBatch)
	if err != nil {
		return errors.Wrap(err, "failed to handle change events")
	}

	return nil
}

func (verifier *Verifier) iterateChangeStream(ctx context.Context, cs *mongo.ChangeStream) {
	defer cs.Close(ctx)

	var lastPersistedTime time.Time

	persistResumeTokenIfNeeded := func() error {
		if time.Since(lastPersistedTime) <= minChangeStreamPersistInterval {
			return nil
		}

		err := verifier.persistChangeStreamResumeToken(ctx, cs)
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
			verifier.logger.Debug().
				Err(ctx.Err()).
				Msg("Change stream quitting.")
			return

		// If the changeStreamEnderChan has a message, the user has indicated that
		// source writes are ended. This means we should exit rather than continue
		// reading the change stream since there should be no more events.
		case writesOffTs := <-verifier.changeStreamWritesOffTsChan:
			verifier.logger.Debug().
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

				if curTs == writesOffTs || curTs.After(writesOffTs) {
					verifier.logger.Debug().
						Interface("currentTimestamp", curTs).
						Interface("writesOffTimestamp", writesOffTs).
						Msg("Change stream has reached the writesOff timestamp. Shutting down.")

					break
				}

				err = verifier.readAndHandleOneChangeEventBatch(ctx, cs)

				if err != nil {
					break
				}
			}

		default:
			err = verifier.readAndHandleOneChangeEventBatch(ctx, cs)

			if err == nil {
				err = persistResumeTokenIfNeeded()
			}
		}

		if err != nil && !errors.Is(err, context.Canceled) {
			verifier.logger.Debug().
				Err(err).
				Msg("Sending change stream error.")

			verifier.changeStreamErrChan <- err

			if !gotwritesOffTimestamp {
				break
			}
		}

		if gotwritesOffTimestamp {
			verifier.mux.Lock()
			verifier.changeStreamRunning = false
			if verifier.lastChangeEventTime != nil {
				verifier.srcStartAtTs = verifier.lastChangeEventTime
			}
			verifier.mux.Unlock()
			// since we have started Recheck, we must signal that we have
			// finished the change stream changes so that Recheck can continue.
			verifier.changeStreamDoneChan <- struct{}{}
			break
		}
	}

	infoLog := verifier.logger.Info()
	if verifier.lastChangeEventTime == nil {
		infoLog = infoLog.Str("lastEventTime", "none")
	} else {
		infoLog = infoLog.Interface("lastEventTime", *verifier.lastChangeEventTime)
	}

	infoLog.Msg("Change stream is done.")
}

// StartChangeStream starts the change stream.
func (verifier *Verifier) StartChangeStream(ctx context.Context) error {
	pipeline := verifier.GetChangeStreamFilter()
	opts := options.ChangeStream().
		SetMaxAwaitTime(1 * time.Second).
		SetFullDocument(options.UpdateLookup)

	savedResumeToken, err := verifier.loadChangeStreamResumeToken(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to load persisted change stream resume token")
	}

	csStartLogEvent := verifier.logger.Info()

	if savedResumeToken != nil {
		logEvent := csStartLogEvent.
			Stringer("resumeToken", savedResumeToken)

		ts, err := extractTimestampFromResumeToken(savedResumeToken)
		if err == nil {
			logEvent = addTimestampToLogEvent(ts, logEvent)
		} else {
			verifier.logger.Warn().
				Err(err).
				Msg("Failed to extract timestamp from persisted resume token.")
		}

		logEvent.Msg("Starting change stream from persisted resume token.")

		opts = opts.SetStartAfter(savedResumeToken)
	} else {
		csStartLogEvent.Msg("Starting change stream from current source cluster time.")
	}

	sess, err := verifier.srcClient.StartSession()
	if err != nil {
		return errors.Wrap(err, "failed to start session")
	}
	sctx := mongo.NewSessionContext(ctx, sess)
	srcChangeStream, err := verifier.srcClient.Watch(sctx, pipeline, opts)
	if err != nil {
		return errors.Wrap(err, "failed to open change stream")
	}

	err = verifier.persistChangeStreamResumeToken(ctx, srcChangeStream)
	if err != nil {
		return err
	}

	csTimestamp, err := extractTimestampFromResumeToken(srcChangeStream.ResumeToken())
	if err != nil {
		return errors.Wrap(err, "failed to extract timestamp from change stream's resume token")
	}

	clusterTime, err := getClusterTimeFromSession(sess)
	if err != nil {
		return errors.Wrap(err, "failed to read cluster time from session")
	}

	verifier.srcStartAtTs = &csTimestamp
	if csTimestamp.After(clusterTime) {
		verifier.srcStartAtTs = &clusterTime
	}

	verifier.mux.Lock()
	verifier.changeStreamRunning = true
	verifier.mux.Unlock()

	go verifier.iterateChangeStream(ctx, srcChangeStream)

	return nil
}

func addTimestampToLogEvent(ts primitive.Timestamp, event *zerolog.Event) *zerolog.Event {
	return event.
		Interface("timestamp", ts).
		Time("time", time.Unix(int64(ts.T), int64(0)))
}

func (v *Verifier) getChangeStreamMetadataCollection() *mongo.Collection {
	return v.metaClient.Database(v.metaDBName).Collection(metadataChangeStreamCollectionName)
}

func (verifier *Verifier) loadChangeStreamResumeToken(ctx context.Context) (bson.Raw, error) {
	coll := verifier.getChangeStreamMetadataCollection()

	token, err := coll.FindOne(
		ctx,
		bson.D{{"_id", "resumeToken"}},
	).Raw()

	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}

	return token, err
}

func (verifier *Verifier) persistChangeStreamResumeToken(ctx context.Context, cs *mongo.ChangeStream) error {
	token := cs.ResumeToken()

	coll := verifier.getChangeStreamMetadataCollection()
	_, err := coll.ReplaceOne(
		ctx,
		bson.D{{"_id", "resumeToken"}},
		token,
		options.Replace().SetUpsert(true),
	)

	if err == nil {
		ts, err := extractTimestampFromResumeToken(token)

		logEvent := verifier.logger.Debug()

		if err == nil {
			logEvent = addTimestampToLogEvent(ts, logEvent)
		} else {
			verifier.logger.Warn().Err(err).
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
