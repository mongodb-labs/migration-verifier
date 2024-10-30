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

// HandleChangeStreamEvent performs the necessary work for change stream events that occur during
// operation.
func (verifier *Verifier) HandleChangeStreamEvent(ctx context.Context, changeEvent *ParsedEvent) error {
	if changeEvent.ClusterTime != nil &&
		(verifier.lastChangeEventTime == nil ||
			primitive.CompareTimestamp(*verifier.lastChangeEventTime, *changeEvent.ClusterTime) < 0) {
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
		return errors.New(`Not supporting: "` + changeEvent.OpType + `" events`)
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

		return verifier.persistChangeStreamResumeToken(ctx, cs)
	}

	for {
		select {
		// if the context is cancelled return immmediately
		case <-ctx.Done():
			return
		// if the changeStreamEnderChan has a message, we have moved to the Recheck phase, obtain
		// the remaining changes, but when TryNext returns false, we will exit, since there should
		// be no message until the user has guaranteed writes to the source have ended.
		case <-verifier.changeStreamEnderChan:
			for cs.TryNext(ctx) {
				if err := cs.Decode(&changeEvent); err != nil {
					verifier.logger.Fatal().Err(err).Msg("Failed to decode change event")
				}
				err := verifier.HandleChangeStreamEvent(ctx, &changeEvent)
				if err != nil {
					verifier.changeStreamErrChan <- err
					verifier.logger.Fatal().Err(err).Msg("Error handling change event")
				}
			}
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
		// the default case is that we are still in the Check phase, in the check phase we still
		// use TryNext, but we do not exit if TryNext returns false.
		default:
			if next := cs.TryNext(ctx); !next {
				continue
			}
			if err := cs.Decode(&changeEvent); err != nil {
				verifier.logger.Fatal().Err(err).Msg("")
			}
			err := verifier.HandleChangeStreamEvent(ctx, &changeEvent)

			if err == nil {
				err = persistResumeTokenIfNeeded()
			}

			if err != nil {
				verifier.changeStreamErrChan <- err
				return
			}
		}
	}
}

// StartChangeStream starts the change stream.
func (verifier *Verifier) StartChangeStream(ctx context.Context, srcClusterTime primitive.Timestamp) error {
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

		logEvent.Msg("Starting change stream with persisted resume token.")

		opts = opts.SetStartAfter(savedResumeToken)
	} else {
		addUnixTimeToLogEvent(
			srcClusterTime.T,
			csStartLogEvent.Interface("clusterTime", srcClusterTime),
		).Msg("Starting change stream from last-observed source cluster time.")

		opts = opts.SetStartAtOperationTime(&srcClusterTime)
	}

	srcChangeStream, err := verifier.srcClient.Watch(ctx, pipeline, opts)
	if err != nil {
		return errors.Wrap(err, "failed to open change stream")
	}

	err = verifier.persistChangeStreamResumeToken(ctx, srcChangeStream)
	if err != nil {
		return errors.Wrap(err, "failed to persist change stream resume token")
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
	).DecodeBytes()

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

	verifier.logger.Debug().
		Stringer("resumeToken", token).
		Msg("Persisted change stream resume token.")

	return errors.Wrapf(err, "failed to persist change stream resume token (%v)", cs.ResumeToken())
}

func extractTimestampFromResumeToken(resumeToken bson.Raw) (primitive.Timestamp, error) {
	// Change stream token is always a V1 keystring in the _data field
	resumeTokenDataValue := resumeToken.Lookup("_data")
	resumeTokenData, ok := resumeTokenDataValue.StringValueOK()
	if !ok {
		return primitive.Timestamp{}, fmt.Errorf("Resume token _data is missing or the wrong type: %v",
			resumeTokenDataValue.Type)
	}
	resumeTokenBson, err := keystring.KeystringToBson(keystring.V1, resumeTokenData)
	if err != nil {
		return primitive.Timestamp{}, err
	}
	// First element is the cluster time we want
	resumeTokenTime, ok := resumeTokenBson[0].Value.(primitive.Timestamp)
	if !ok {
		return primitive.Timestamp{}, errors.New("Resume token lacks a cluster time")
	}

	return resumeTokenTime, nil
}
