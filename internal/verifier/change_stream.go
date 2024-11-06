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
	"golang.org/x/exp/constraints"
)

// ParsedEvent contains the fields of an event that we have parsed from 'bson.Raw'.
type ParsedEvent struct {
	ID          interface{}          `bson:"_id"`
	OpType      string               `bson:"operationType"`
	Ns          *Namespace           `bson:"ns,omitempty"`
	DocKey      DocKey               `bson:"documentKey,omitempty"`
	ClusterTime *primitive.Timestamp `bson:"clusterTime,omitEmpty"`
}

func (pe *ParsedEvent) String() string {
	return fmt.Sprintf("{ OpType: %s, namespace: %s, docID: %v}", pe.OpType, pe.Ns, pe.DocKey.ID)
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
	return fmt.Sprintf("Unknown event type: %#q", uee.Event.OpType)
}

// HandleChangeStreamEvent performs the necessary work for change stream events that occur during
// operation.
func (verifier *Verifier) HandleChangeStreamEvent(ctx context.Context, changeEvent *ParsedEvent) error {
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
		return verifier.InsertChangeEventRecheckDoc(ctx, changeEvent)
	default:
		return UnknownEventError{Event: changeEvent}
	}
}

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

func (verifier *Verifier) iterateChangeStream(ctx context.Context, cs *mongo.ChangeStream) {
	var changeEvent ParsedEvent

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

	readOneChangeEvent := func() (bool, error) {
		gotEvent := cs.TryNext(ctx)
		if gotEvent {
			if err := cs.Decode(&changeEvent); err != nil {
				return false, errors.Wrap(err, "failed to decode change event")
			}
			err := verifier.HandleChangeStreamEvent(ctx, &changeEvent)
			if err != nil {
				return false, errors.Wrap(err, "failed to handle change event")
			}
		}

		return gotEvent, errors.Wrap(cs.Err(), "change stream iteration failed")
	}

	for {
		var err error
		var changeStreamEnded bool

		select {
		// if the context is cancelled return immmediately
		case <-ctx.Done():
			return

		// If the changeStreamEnderChan has a message, the user has indicated that
		// source writes are ended. This means we should exit rather than continue
		// reading the change stream since there should be no more events.
		case <-verifier.changeStreamEnderChan:
			var gotEvent bool

			changeStreamEnded = true

			// Read all change events until the source reports no events.
			// (i.e., the `getMore` call returns empty)
			for {
				gotEvent, err = readOneChangeEvent()

				if !gotEvent || err != nil {
					break
				}
			}

			if err != nil {
				break
			}

		default:
			_, err = readOneChangeEvent()
		}

		if err == nil {
			err = persistResumeTokenIfNeeded()
		}

		if err != nil && !errors.Is(err, context.Canceled) {
			verifier.changeStreamErrChan <- err
		}

		if changeStreamEnded {
			verifier.mux.Lock()
			verifier.changeStreamRunning = false
			if verifier.lastChangeEventTime != nil {
				verifier.srcStartAtTs = verifier.lastChangeEventTime
			}
			verifier.mux.Unlock()
			// since we have started Recheck, we must signal that we have
			// finished the change stream changes so that Recheck can continue.
			verifier.changeStreamDoneChan <- struct{}{}
			// since the changeStream is exhausted, we now return
			verifier.logger.Debug().Msg("Change stream is done")
			return
		}
	}
}

// StartChangeStream starts the change stream.
func (verifier *Verifier) StartChangeStream(ctx context.Context) error {
	pipeline := verifier.GetChangeStreamFilter()
	opts := options.ChangeStream().SetMaxAwaitTime(1 * time.Second)

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
			logEvent = addUnixTimeToLogEvent(ts.T, logEvent)
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

func addUnixTimeToLogEvent[T constraints.Integer](unixTime T, event *zerolog.Event) *zerolog.Event {
	return event.Time("clockTime", time.Unix(int64(unixTime), int64(0)))
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
			logEvent = addUnixTimeToLogEvent(ts.T, logEvent)
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
